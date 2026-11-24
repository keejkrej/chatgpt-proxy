package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

const help = `Usage: chatgpt-proxy [command]

  login             Add or renew a ChatGPT account
  logout            Pick an account and remove its local login
  usage             Show usage limits for each account
  serve             Start the local Responses proxy (default)

Run chatgpt-proxy serve -h for server options.`

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, errCancelled) {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "help", "-h", "--help":
		fmt.Println(help)
		return nil
	case "login", "logout", "usage":
		if len(args) != 0 {
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				fmt.Println(help)
				return nil
			}
			return fmt.Errorf("usage: chatgpt-proxy %s", command)
		}
	case "serve":
	default:
		return fmt.Errorf("unknown command %q; run chatgpt-proxy help", command)
	}

	store, err := configStore()
	if err != nil {
		return err
	}
	if command == "login" {
		a, err := store.login(ctx, codexLogin)
		if err == nil {
			fmt.Printf("Signed in: %s\n", a.display())
		}
		return err
	}
	if command == "serve" {
		return serve(ctx, store, args)
	}

	accounts, err := store.list()
	if err != nil {
		return err
	}
	if command == "usage" {
		return showUsage(ctx, accounts, upstreamClient(), usageEndpoint, os.Stdout)
	}
	return logoutAccount(ctx, accounts, os.Stdin, os.Stdout)
}

func logoutAccount(ctx context.Context, accounts []account, in io.Reader, out io.Writer) error {
	if len(accounts) == 0 {
		fmt.Fprintln(out, "No accounts to log out.")
		return nil
	}
	a, err := chooseAccount(ctx, accounts, in, out, "Log out which account?")
	if err != nil {
		return err
	}
	if err := os.Remove(a.path); err != nil {
		return fmt.Errorf("remove local login: %w", err)
	}
	fmt.Fprintf(out, "Logged out: %s\n", a.display())
	return nil
}

func serve(ctx context.Context, store accountStore, args []string) error {
	flags := flag.NewFlagSet("chatgpt-proxy serve", flag.ContinueOnError)
	port := flags.Int("port", 8787, "TCP port on 127.0.0.1")
	id := flags.String("account", "", "pin to one account ID shown by chatgpt-proxy usage")
	strategy := flags.String("routing", roundRobin, "cold routing: fill-first, round-robin, or cache-aware")
	order := flags.String("order", "", "comma-separated account IDs to put first")
	cacheLog := flags.Bool("cache-log", false, "log per-response and rolling prompt cache rates")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("unexpected server argument; run chatgpt-proxy serve -h")
	}
	switch *strategy {
	case fillFirst, roundRobin, cacheAware:
	default:
		return errors.New("routing must be fill-first, round-robin, or cache-aware")
	}
	if *id != "" && *order != "" {
		return errors.New("-account and -order cannot be combined")
	}
	key := os.Getenv("CHATGPT_PROXY_KEY")
	if len(key) < 32 || strings.ContainsAny(key, " \t\r\n") {
		return errors.New("set CHATGPT_PROXY_KEY to a random bearer token of at least 32 characters")
	}
	if *port < 1 || *port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	accounts, err := store.list()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return errors.New("no accounts; run chatgpt-proxy login")
	}
	if *id != "" {
		a, err := accountByID(accounts, *id)
		if err != nil {
			return err
		}
		accounts = []account{a}
	}
	accounts, err = orderedAccounts(accounts, *order)
	if err != nil {
		return err
	}

	client := upstreamClient()
	server := &http.Server{
		Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)),
		Handler: proxy{
			endpoint:      "https://chatgpt.com/backend-api/codex/responses",
			imageEndpoint: "https://chatgpt.com/backend-api/codex/images/generations",
			editEndpoint:  "https://chatgpt.com/backend-api/codex/images/edits",
			client:        client,
			models:        newModelCatalog(client, codexModelsEndpoint),
			routes:        newRouter(accounts, *strategy, *cacheLog),
			key:           key,
		},
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp4", server.Addr)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	log.Printf("Listening at http://%s/v1 (%s, %d account(s))", listener.Addr(), *strategy, len(accounts))
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func upstreamClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Never send the subscription token through an environment-configured proxy.
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
