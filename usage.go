package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const usageEndpoint = "https://chatgpt.com/backend-api/wham/usage"

type usageWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	Seconds     int64    `json:"limit_window_seconds"`
	ResetAt     int64    `json:"reset_at"`
}

type usageLimits struct {
	Primary      *usageWindow `json:"primary_window"`
	Secondary    *usageWindow `json:"secondary_window"`
	LimitReached bool         `json:"limit_reached"`
}

type accountUsage struct {
	Plan       string      `json:"plan_type"`
	Codex      usageLimits `json:"rate_limit"`
	CodeReview usageLimits `json:"code_review_rate_limit"`
	Additional []struct {
		Name   string      `json:"limit_name"`
		Limits usageLimits `json:"rate_limit"`
	} `json:"additional_rate_limits"`
}

func fetchUsage(ctx context.Context, client *http.Client, endpoint string, auth credentials) (accountUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return accountUsage{}, err
	}
	setCodexHeaders(req.Header, auth)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return accountUsage{}, errors.New("could not reach the Codex usage endpoint")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return accountUsage{}, errors.New("login expired or rejected; run chatgpt-proxy login again")
	case http.StatusOK:
	default:
		return accountUsage{}, fmt.Errorf("usage lookup returned HTTP %d", resp.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return accountUsage{}, errors.New("could not read the usage response")
	}
	var usage *accountUsage
	if json.Unmarshal(body, &usage) != nil || usage == nil {
		return accountUsage{}, errors.New("invalid usage response")
	}
	return *usage, nil
}

func showUsage(ctx context.Context, accounts []account, client *http.Client, endpoint string, out io.Writer) error {
	if len(accounts) == 0 {
		fmt.Fprintln(out, "No accounts. Run chatgpt-proxy login.")
		return nil
	}
	var failed int
	for _, a := range accounts {
		fmt.Fprintln(out, a.display())
		auth, err := readCredentials(a.path)
		if err != nil {
			failed++
			fmt.Fprintln(out, "  Could not read the login. Run chatgpt-proxy login again.")
			continue
		}
		usage, err := fetchUsage(ctx, client, endpoint, auth)
		if err != nil {
			failed++
			fmt.Fprintf(out, "  Unavailable: %s\n", err)
			continue
		}
		usage.print(out)
	}
	if failed > 0 {
		return fmt.Errorf("could not load usage for %d of %d accounts", failed, len(accounts))
	}
	return nil
}

func (u accountUsage) print(out io.Writer) {
	if u.Plan != "" {
		fmt.Fprintf(out, "  Plan: %q\n", u.Plan)
	}
	shown := u.Codex.print(out, "Codex")
	shown = u.CodeReview.print(out, "Code review") || shown
	for _, extra := range u.Additional {
		name := "Additional limit"
		if extra.Name != "" {
			name = fmt.Sprintf("%q", extra.Name)
		}
		shown = extra.Limits.print(out, name) || shown
	}
	if !shown {
		fmt.Fprintln(out, "  No usage limits reported.")
	}
}

func (l usageLimits) print(out io.Writer, name string) bool {
	if l.LimitReached {
		fmt.Fprintf(out, "  %s: limit reached\n", name)
	}
	for i, window := range []*usageWindow{l.Primary, l.Secondary} {
		if window == nil {
			continue
		}
		label := []string{"primary", "secondary"}[i]
		if n := window.Seconds; n > 0 && n <= 365*24*60*60 {
			switch {
			case n%86400 == 0:
				label = fmt.Sprintf("%dd", n/86400)
			case n%3600 == 0:
				label = fmt.Sprintf("%dh", n/3600)
			case n%60 == 0:
				label = fmt.Sprintf("%dm", n/60)
			default:
				label = (time.Duration(n) * time.Second).String()
			}
		}
		fmt.Fprintf(out, "  %s (%s):", name, label)
		if window.UsedPercent != nil {
			fmt.Fprintf(out, " %g%% used", *window.UsedPercent)
		} else {
			fmt.Fprint(out, " usage not reported")
		}
		if window.ResetAt > 0 {
			fmt.Fprintf(out, "; resets %s", time.Unix(window.ResetAt, 0).Local().Format("Jan 2 15:04 MST"))
		}
		fmt.Fprintln(out)
	}
	return l.LimitReached || l.Primary != nil || l.Secondary != nil
}
