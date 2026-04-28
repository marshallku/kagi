package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"

	"github.com/marshallku/kagi/client"
	"github.com/marshallku/kagi/server"
)

const usage = `kagi - Kagi Assistant CLI/HTTP client

Usage:
  kagi chat                [-t thread] [--parent msg] [-m model] [-p profile] [--stream|--json] <prompt...>
  kagi serve               [-addr 127.0.0.1:8921]
  kagi login               sign in (env or stdin) and cache session in OS keyring
  kagi logout              delete cached session
  kagi models              list available models (id, name, provider)
  kagi profiles            list profiles & custom assistants (id, name, model)
  kagi config get [key]    print one config value, or all when key omitted
  kagi config set <k> <v>  set a config value (keys: model, profile)

Env:
  KAGI_SESSION   value of the kagi_session cookie (overrides keyring)
  KAGI_EMAIL     account email (enables auto-login when session is missing/expired)
  KAGI_PASSWORD  account password

Defaults (model + profile) come from $XDG_CONFIG_HOME/kagi/config.json.
Discover and set them with:
  kagi models                      # see all model ids (e.g. grok-4-20)
  kagi profiles                    # see all profile uuids
  kagi config set model <id>
  kagi config set profile <uuid>
Session order: KAGI_SESSION → keyring → auto-login (if KAGI_EMAIL+KAGI_PASSWORD).
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
	case "config":
		configCmd(os.Args[2:])
	case "models":
		modelsCmd(os.Args[2:])
	case "profiles":
		profilesCmd(os.Args[2:])
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
	cfg, err := client.LoadConfig()
	if err != nil {
		die("config load: " + err.Error())
	}

	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	threadID := fs.String("t", "", "existing thread id (omit to create new)")
	parentID := fs.String("parent", "", "parent message id (required with -t)")
	model := fs.String("m", cfg.Model, "model id (default from config)")
	profile := fs.String("p", cfg.Profile, "profile id (default from config)")
	asJSON := fs.Bool("json", false, "emit final JSON result instead of text")
	stream := fs.Bool("stream", false, "stream raw tokens (HTML) as they arrive")
	noInternet := fs.Bool("no-internet", false, "disable internet access")
	_ = fs.Parse(args)

	if *profile == "" {
		die("profile not set; pass -p or run: kagi config set profile <uuid> (see: kagi profiles)")
	}
	if *model == "" {
		die("model not set; pass -m or run: kagi config set model <id>")
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

	email := os.Getenv("KAGI_EMAIL")
	pw := os.Getenv("KAGI_PASSWORD")
	if email == "" || pw == "" {
		var err error
		email, pw, err = readMissingCreds(email, pw)
		if err != nil {
			die("read credentials: " + err.Error())
		}
	}

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

// readMissingCreds fills in email/password from stdin. On a TTY it prompts to
// stderr and reads the password silently; when piped it expects two lines
// (email then password) on stdin in order — only for the values that are
// missing from the environment.
func readMissingCreds(email, pw string) (string, string, error) {
	fd := int(os.Stdin.Fd())
	isTTY := term.IsTerminal(fd)
	rd := bufio.NewReader(os.Stdin)

	readLine := func(prompt string) (string, error) {
		if isTTY {
			fmt.Fprint(os.Stderr, prompt)
		}
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	if email == "" {
		v, err := readLine("Email: ")
		if err != nil {
			return "", "", err
		}
		email = v
	}
	if pw == "" {
		if isTTY {
			fmt.Fprint(os.Stderr, "Password: ")
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", "", err
			}
			pw = string(b)
		} else {
			v, err := readLine("")
			if err != nil {
				return "", "", err
			}
			pw = v
		}
	}
	if email == "" {
		return "", "", errors.New("email is empty")
	}
	if pw == "" {
		return "", "", errors.New("password is empty")
	}
	return email, pw, nil
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

func configCmd(args []string) {
	if len(args) < 1 {
		die("usage: kagi config <get|set> [key] [value]")
	}
	switch args[0] {
	case "get":
		configGet(args[1:])
	case "set":
		configSet(args[1:])
	default:
		die("unknown config action: " + args[0] + " (expected get|set)")
	}
}

func configGet(args []string) {
	cfg, err := client.LoadConfig()
	if err != nil {
		die("config load: " + err.Error())
	}
	if len(args) == 0 {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return
	}
	switch args[0] {
	case "model":
		fmt.Println(cfg.Model)
	case "profile":
		fmt.Println(cfg.Profile)
	default:
		die("unknown config key: " + args[0] + " (expected model|profile)")
	}
}

func configSet(args []string) {
	if len(args) != 2 {
		die("usage: kagi config set <key> <value>")
	}
	cfg, err := client.LoadConfig()
	if err != nil {
		die("config load: " + err.Error())
	}
	switch args[0] {
	case "model":
		cfg.Model = args[1]
	case "profile":
		cfg.Profile = args[1]
	default:
		die("unknown config key: " + args[0] + " (expected model|profile)")
	}
	if err := client.SaveConfig(cfg); err != nil {
		die("config save: " + err.Error())
	}
	fmt.Fprintf(os.Stderr, "kagi: %s = %s (saved to %s)\n", args[0], args[1], client.ConfigPath())
}

func modelsCmd(args []string) {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	profiles, err := newAuthedClient(resolveSession()).FetchProfiles(ctx)
	if err != nil {
		die(err.Error())
	}

	type modelEntry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Provider    string `json:"provider"`
		Recommended bool   `json:"recommended"`
	}
	seen := map[string]modelEntry{}
	for _, p := range profiles {
		if !p.Accessible || p.Deprecate || p.Retired || p.Model == "" {
			continue
		}
		if _, ok := seen[p.Model]; ok {
			continue
		}
		seen[p.Model] = modelEntry{
			ID: p.Model, Name: p.ModelName, Provider: p.ModelProvider, Recommended: p.Recommended,
		}
	}

	out := make([]modelEntry, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  MODEL\tNAME\tPROVIDER")
	for _, m := range out {
		marker := " "
		if m.Recommended {
			marker = "★"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\n", marker, m.ID, m.Name, m.Provider)
	}
	_ = w.Flush()
	fmt.Fprintln(os.Stderr, "\n★ = recommended.  use: kagi config set model <id>")
}

func profilesCmd(args []string) {
	fs := flag.NewFlagSet("profiles", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	profiles, err := newAuthedClient(resolveSession()).FetchProfiles(ctx)
	if err != nil {
		die(err.Error())
	}

	visible := profiles[:0]
	for _, p := range profiles {
		if p.ID == "" || !p.Accessible || p.Deprecate || p.Retired {
			continue
		}
		visible = append(visible, p)
	}
	sort.Slice(visible, func(i, j int) bool {
		return visible[i].Name < visible[j].Name
	})

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(visible)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  PROFILE_ID\tNAME\tMODEL")
	for _, p := range visible {
		marker := " "
		if p.IsDefaultProfile {
			marker = "★"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\n", marker, p.ID, p.Name, p.Model)
	}
	_ = w.Flush()
	fmt.Fprintln(os.Stderr, "\n★ = system default.  use: kagi config set profile <uuid>")
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "kagi:", msg)
	os.Exit(1)
}
