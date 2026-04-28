package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/marshallku/kagi-cli/client"
	"github.com/marshallku/kagi-cli/server"
)

const usage = `kagi - Kagi Assistant CLI/HTTP client

Usage:
  kagi chat    [-t thread] [--parent msg] [-m model] [-p profile] [--stream|--json] <prompt...>
  kagi serve   [-addr 127.0.0.1:8921]
  kagi login   - sign in with KAGI_EMAIL/KAGI_PASSWORD; cache session in OS keyring
  kagi logout  - delete cached session

Env:
  KAGI_SESSION     value of the kagi_session cookie (overrides keyring)
  KAGI_EMAIL       account email (enables auto-login when session is missing/expired)
  KAGI_PASSWORD    account password
  KAGI_PROFILE_ID  default profile UUID (custom assistant id)
  KAGI_MODEL       default model id (e.g. ki_quick, claude-4-sonnet, grok-4-20)

Session resolution: KAGI_SESSION → keyring → auto-login (if KAGI_EMAIL+KAGI_PASSWORD set).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "chat":
		chatCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "login":
		loginCmd(os.Args[2:])
	case "logout":
		logoutCmd(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// resolveSession returns the cookie value to use, prefering env over keyring.
// Empty result is OK: the client will auto-login if creds are set.
func resolveSession() string {
	if v := os.Getenv("KAGI_SESSION"); v != "" {
		return v
	}
	if v, err := client.LoadSession(); err == nil {
		return v
	}
	return ""
}

// newAuthedClient builds a Client wired with credentials (if present) and a
// keyring-persisting OnRefresh hook so any auto-login transparently updates
// the cached session.
func newAuthedClient(session string) *client.Client {
	c := client.New(session)
	if email, pw := os.Getenv("KAGI_EMAIL"), os.Getenv("KAGI_PASSWORD"); email != "" && pw != "" {
		c.SetCredentials(email, pw)
	}
	c.OnRefresh = func(s string) {
		if err := client.SaveSession(s); err != nil {
			fmt.Fprintln(os.Stderr, "kagi: warn: keyring save failed:", err)
		}
	}
	return c
}

func chatCmd(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	threadID := fs.String("t", "", "existing thread id (omit to create new)")
	parentID := fs.String("parent", "", "parent message id (required with -t)")
	model := fs.String("m", os.Getenv("KAGI_MODEL"), "model id")
	profile := fs.String("p", os.Getenv("KAGI_PROFILE_ID"), "profile id (custom assistant uuid)")
	asJSON := fs.Bool("json", false, "emit final JSON result instead of text")
	stream := fs.Bool("stream", false, "stream raw tokens (HTML) as they arrive")
	noInternet := fs.Bool("no-internet", false, "disable internet access")
	_ = fs.Parse(args)

	if *profile == "" {
		die("profile id not set; pass -p or KAGI_PROFILE_ID")
	}
	if *model == "" {
		die("model not set; pass -m or KAGI_MODEL")
	}
	if *threadID != "" && *parentID == "" {
		die("--parent <message-id> is required when -t is set")
	}
	if *asJSON && *stream {
		die("--json and --stream are mutually exclusive")
	}

	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			die(err.Error())
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		die("empty prompt")
	}

	req := client.NewPrompt(prompt, *threadID, *parentID, *profile, *model, !*noInternet)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	c := newAuthedClient(resolveSession())

	var lastText string
	onToken := func(t string) {
		if !*stream {
			return
		}
		if len(t) >= len(lastText) && strings.HasPrefix(t, lastText) {
			_, _ = io.WriteString(os.Stdout, t[len(lastText):])
		} else {
			_, _ = io.WriteString(os.Stdout, t)
		}
		lastText = t
	}

	res, err := c.Send(ctx, req, onToken)
	if err != nil {
		die(err.Error())
	}

	switch {
	case *asJSON:
		_ = json.NewEncoder(os.Stdout).Encode(res)
	case *stream:
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "[thread=%s msg=%s]\n", res.ThreadID, res.MessageID)
	default:
		fmt.Println(res.Markdown)
		fmt.Fprintf(os.Stderr, "[thread=%s msg=%s]\n", res.ThreadID, res.MessageID)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8921", "listen address")
	_ = fs.Parse(args)

	s := server.New(resolveSession())
	if email, pw := os.Getenv("KAGI_EMAIL"), os.Getenv("KAGI_PASSWORD"); email != "" && pw != "" {
		s.SetCredentials(email, pw)
	}
	fmt.Fprintf(os.Stderr, "kagi serve on http://%s\n", *addr)
	if err := s.ListenAndServe(*addr); err != nil {
		die(err.Error())
	}
}

func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	_ = fs.Parse(args)

	email := mustEnv("KAGI_EMAIL")
	pw := mustEnv("KAGI_PASSWORD")

	c := client.New("")
	c.SetCredentials(email, pw)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := c.Login(ctx); err != nil {
		die(err.Error())
	}
	if err := client.SaveSession(c.Session); err != nil {
		die("keyring save: " + err.Error())
	}
	fmt.Fprintln(os.Stderr, "kagi: signed in; session cached in keyring")
}

func logoutCmd(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	_ = fs.Parse(args)
	err := client.DeleteSession()
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		die("keyring delete: " + err.Error())
	}
	fmt.Fprintln(os.Stderr, "kagi: cached session deleted")
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		die(k + " not set")
	}
	return v
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "kagi:", msg)
	os.Exit(1)
}
