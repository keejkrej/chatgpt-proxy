package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeLogin(t *testing.T, email, user, token string) func(context.Context, string) error {
	t.Helper()
	return func(_ context.Context, home string) error {
		claims, _ := json.Marshal(map[string]string{"email": email, "sub": user})
		idToken := "fake." + base64.RawURLEncoding.EncodeToString(claims) + ".fake"
		body := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"account_id":"workspace","id_token":%q,"access_token":%q}}`, idToken, token)
		return os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0600)
	}
}

func addTestAccount(t *testing.T, store accountStore, email, user, token string) account {
	t.Helper()
	a, err := store.login(t.Context(), fakeLogin(t, email, user, token))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAccountLoginRenewAndLogout(t *testing.T) {
	store := accountStore(t.TempDir())
	a := addTestAccount(t, store, "a@example.test", "user-a", "token-a")
	b := addTestAccount(t, store, "b@example.test", "user-b", "token-b")
	renewed := addTestAccount(t, store, "a@example.test", "user-a", "renewed-a")
	if a.id != renewed.id || a.id == b.id {
		t.Fatalf("expected renew not to add a duplicate: %v %v %v", a, b, renewed)
	}
	auth, err := readCredentials(a.path)
	if err != nil || auth.AccessToken != "renewed-a" {
		t.Fatal("renewal did not replace credentials")
	}
	accounts, err := store.list()
	if err != nil || len(accounts) != 2 || accounts[0].id != a.id {
		t.Fatalf("unexpected account list: %v; %v", accounts, err)
	}
	var out bytes.Buffer
	if err := logoutAccount(t.Context(), accounts, strings.NewReader("\n"), &out); !errors.Is(err, errCancelled) {
		t.Fatalf("expected cancelled logout, got %v", err)
	}
	if _, err := readCredentials(b.path); err != nil {
		t.Fatalf("cancelled logout removed account: %v", err)
	}
	out.Reset()
	if err := logoutAccount(t.Context(), accounts, strings.NewReader("2\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Logged out: \"b@example.test\"") {
		t.Errorf("wrong logout output: %s", out.String())
	}
	accounts, err = store.list()
	if err != nil || len(accounts) != 1 || accounts[0].id != a.id {
		t.Errorf("expected only first account: %v; %v", accounts, err)
	}
}

func TestAccountChangesWhileReaderIsOpen(t *testing.T) {
	store := accountStore(t.TempDir())
	a := addTestAccount(t, store, "a@example.test", "user-a", "token-a")
	f, err := openAuth(a.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	closed := make(chan struct{})
	time.AfterFunc(40*time.Millisecond, func() { _ = f.Close(); close(closed) })
	renewed := addTestAccount(t, store, "a@example.test", "user-a", "renewed")
	<-closed
	current, err := openAuth(renewed.path)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := os.Remove(renewed.path); err != nil {
		t.Fatal(err)
	}
}

func TestFailedLoginCleansStagingArea(t *testing.T) {
	store := accountStore(t.TempDir())
	errAbort := errors.New("aborted")
	_, err := store.login(t.Context(), func(ctx context.Context, home string) error {
		if err := fakeLogin(t, "a@example.test", "user-a", "new-token")(ctx, home); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatal(err)
	}
	files, err := os.ReadDir(string(store))
	if err != nil || len(files) != 1 || files[0].Name() != "accounts" {
		t.Fatalf("expected only empty accounts dir, got %v; %v", files, err)
	}
}

func TestUsageShowsEveryAccount(t *testing.T) {
	store := accountStore(t.TempDir())
	a := addTestAccount(t, store, "expired@example.test", "user-a", "expired")
	b := addTestAccount(t, store, "valid@example.test", "user-b", "valid")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("ChatGPT-Account-Id") != "workspace" {
			t.Errorf("unexpected usage request: %s %s", r.Method, r.URL.Path)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer expired":
			w.WriteHeader(401)
		case "Bearer valid":
			fmt.Fprint(w, `{
				"plan_type":"pro",
				"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1700000000},"secondary_window":{"used_percent":23,"limit_window_seconds":604800}},
				"additional_rate_limits":[{"limit_name":"Extra","rate_limit":{"primary_window":{"used_percent":12.5}}}]
			}`)
		default:
			t.Error("usage request has wrong authorization")
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	err := showUsage(t.Context(), []account{a, b}, upstreamClient(), server.URL+"/backend-api/wham/usage", &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("expected partial failure, got %v", err)
	}
	for _, want := range []string{"expired@example.test", "run chatgpt-proxy login again", "valid@example.test", `Plan: "pro"`, "Codex (5h): 0% used; resets", "Codex (7d): 23% used", `"Extra" (primary): 12.5% used`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage output missing %q:\n%s", want, &out)
		}
	}
}
