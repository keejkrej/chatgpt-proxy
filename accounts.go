package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type accountStore string

type account struct {
	id, label, path string
}

func configStore() (accountStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return accountStore(filepath.Join(home, ".chatgpt-proxy")), nil
}

func (s accountStore) accountsDir() string { return filepath.Join(string(s), "accounts") }

func accountLabel(auth credentials) (label, user string) {
	// Unverified JWT claims are only used for display and local file naming.
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	parts := strings.Split(auth.IDToken, ".")
	if len(parts) == 3 {
		if data, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			_ = json.Unmarshal(data, &claims)
		}
	}
	if claims.Email != "" {
		return claims.Email, claims.Sub
	}
	return "ChatGPT account", claims.Sub
}

func (s accountStore) list() ([]account, error) {
	entries, err := os.ReadDir(s.accountsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accounts []account
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || len(id) != sha256.Size*2 || !entry.Type().IsRegular() {
			continue
		}
		if _, err := hex.DecodeString(id); err != nil {
			continue
		}
		path := filepath.Join(s.accountsDir(), entry.Name())
		label := "Unreadable ChatGPT account"
		if auth, err := readCredentials(path); err == nil {
			label, _ = accountLabel(auth)
		}
		accounts = append(accounts, account{id, label, path})
	}
	slices.SortFunc(accounts, func(a, b account) int {
		if cmp := strings.Compare(strings.ToLower(a.label), strings.ToLower(b.label)); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.id, b.id)
	})
	return accounts, nil
}

func (s accountStore) login(ctx context.Context, runLogin func(context.Context, string) error) (account, error) {
	if err := os.MkdirAll(s.accountsDir(), 0700); err != nil {
		return account{}, err
	}
	home, err := os.MkdirTemp(string(s), ".login-")
	if err != nil {
		return account{}, err
	}
	defer os.RemoveAll(home)
	home, err = filepath.Abs(home)
	if err != nil {
		return account{}, err
	}
	if err := runLogin(ctx, home); err != nil {
		return account{}, err
	}
	loginFile := filepath.Join(home, "auth.json")
	auth, err := readCredentials(loginFile)
	if err != nil {
		return account{}, errors.New("Codex did not save a file-based ChatGPT login")
	}
	label, user := accountLabel(auth)
	digest := sha256.Sum256([]byte(auth.AccountID + "\x00" + user))
	id := hex.EncodeToString(digest[:])
	path := filepath.Join(s.accountsDir(), id+".json")
	if err := os.Chmod(loginFile, 0600); err != nil {
		return account{}, err
	}
	if err := replaceAuth(ctx, loginFile, path); err != nil {
		return account{}, fmt.Errorf("save account: %w", err)
	}
	return account{id, label, path}, nil
}

func codexLogin(ctx context.Context, home string) error {
	if _, err := exec.LookPath("codex"); err != nil {
		return errors.New("install the Codex CLI and put it on PATH to sign in")
	}
	cmd := exec.CommandContext(ctx, "codex", "login", "-c", `cli_auth_credentials_store="file"`, "-c", `forced_login_method="chatgpt"`)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Codex login did not complete: %w", err)
	}
	return nil
}

func (a account) display() string {
	return fmt.Sprintf("%q [%s]", a.label, a.id[:12])
}

func accountByID(accounts []account, id string) (account, error) {
	var found account
	for _, a := range accounts {
		if len(id) >= 12 && strings.HasPrefix(a.id, id) {
			if found.id != "" {
				return account{}, errors.New("ambiguous account ID")
			}
			found = a
		}
	}
	if found.id == "" {
		return account{}, errors.New("account not found; run chatgpt-proxy usage for IDs")
	}
	return found, nil
}

var errCancelled = errors.New("selection cancelled")

func chooseAccount(ctx context.Context, accounts []account, in io.Reader, out io.Writer, prompt string) (account, error) {
	if len(accounts) == 0 {
		return account{}, errors.New("no accounts; run chatgpt-proxy login")
	}
	for i, a := range accounts {
		fmt.Fprintf(out, "%d) %s\n", i+1, a.display())
	}
	fmt.Fprint(out, prompt+" [Enter cancels]: ")
	type selection struct {
		line []byte
		err  error
	}
	input := make(chan selection, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadSlice('\n')
		input <- selection{line, err}
	}()
	var value string
	select {
	case <-ctx.Done():
		return account{}, errCancelled
	case result := <-input:
		if result.err != nil && !errors.Is(result.err, io.EOF) {
			return account{}, result.err
		}
		value = strings.TrimSpace(string(result.line))
	}
	if value == "" {
		return account{}, errCancelled
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > len(accounts) {
		return account{}, errors.New("enter a number from the list")
	}
	return accounts[n-1], nil
}
