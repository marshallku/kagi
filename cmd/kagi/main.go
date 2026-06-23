package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"

	"github.com/marshallku/kagi/client"
	"github.com/marshallku/kagi/server"
)

const usage = `kagi - Kagi Assistant CLI/HTTP client

Usage:
  kagi chat                [-t thread] [--parent msg] [--resume] [-m model] [-p profile] [--stream|--json] <prompt...>
  kagi serve               [-addr 127.0.0.1:8921]
  kagi login               sign in (env or stdin) and cache session in OS keyring
  kagi logout              delete cached session
  kagi models              list available models (id, name, provider)
  kagi profiles            list profiles & custom assistants (id, name, model)
  kagi threads list        [--limit N] [--all] [--json] list saved threads
  kagi threads show <id>   [--json] print a thread (last user message id = follow-up parent)
  kagi threads search <q>  [--saved] [--shared] [--tag <id>] [--json] full-text search
  kagi threads rename <id> <new title...>      rename a thread
  kagi threads save   <id> | unsave <id>       toggle the temporary/forever flag
  kagi threads share  <id> | unshare <id>      toggle public sharing
  kagi threads delete <id>...                  delete one or more threads
  kagi assistants list                         alias of "kagi profiles"
  kagi assistants create -n <name> -m <model> [--prompt <file|->] [--bang <trigger>] [--no-internet]
  kagi assistants update <uuid> [-n <name>] [-m <model>] [--prompt <file|->] [--bang <trigger>] [--internet|--no-internet]
  kagi assistants show   <uuid> [--json]       print current spec (for review or piping)
  kagi assistants delete <uuid>                delete a custom assistant
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
	case "threads":
		threadsCmd(os.Args[2:])
	case "assistants":
		assistantsCmd(os.Args[2:])
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
	resume := fs.Bool("resume", false, "continue the most recent thread (auto-fills -t/--parent)")
	model := fs.String("m", cfg.Model, "model id (default from config)")
	profile := fs.String("p", cfg.Profile, "profile id (default from config)")
	asJSON := fs.Bool("json", false, "emit final JSON result instead of text")
	stream := fs.Bool("stream", false, "stream raw tokens (HTML) as they arrive")
	noInternet := fs.Bool("no-internet", false, "disable internet access")
	_ = fs.Parse(args)

	if *resume {
		if *threadID != "" || *parentID != "" {
			die("--resume cannot be combined with -t or --parent")
		}
		last, err := client.LoadLastSession()
		if err != nil {
			die("read last session: " + err.Error())
		}
		if last.ThreadID == "" {
			die("no saved session at " + client.StatePath() + " (run a chat first)")
		}
		*threadID = last.ThreadID
		*parentID = last.MessageID
		fmt.Fprintf(os.Stderr, "[resuming thread=%s title=%q]\n", last.ThreadID, last.Title)
	}

	// v2 needs at least one of model (base model) or profile (custom
	// assistant). A profile carries its own default model, so model is
	// optional when a profile is set, and vice versa.
	if *profile == "" && *model == "" {
		die("set a model or profile; pass -m/-p or run: kagi config set model <id> (see: kagi models / kagi profiles)")
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	c := newAuthedClient(resolveSession())

	// Auto-resolve --parent when -t is set without it. This used to be a hard
	// failure; now that we can fetch /assistant/<id>, we look up the last
	// user message ourselves.
	if *threadID != "" && *parentID == "" {
		detail, err := c.ShowThread(ctx, *threadID)
		if err != nil {
			die("resolve parent for -t " + *threadID + ": " + err.Error())
		}
		*parentID = detail.LastMessageID()
		if *parentID == "" {
			die("thread " + *threadID + " has no user messages — cannot determine parent")
		}
		fmt.Fprintf(os.Stderr, "[auto-parent=%s]\n", *parentID)
	}

	req := client.NewPrompt(prompt, *threadID, *parentID, *profile, *model, !*noInternet)

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

	if res.ThreadID != "" && res.MessageID != "" {
		if err := client.SaveLastSession(client.LastSession{
			ThreadID:  res.ThreadID,
			MessageID: res.MessageID,
			Title:     res.Title,
			Model:     *model,
			Profile:   *profile,
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			fmt.Fprintln(os.Stderr, "kagi: warn: state save failed:", err)
		}
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

func threadsCmd(args []string) {
	if len(args) < 1 {
		die("usage: kagi threads <list|show|search|rename|save|unsave|share|unshare|delete> ...")
	}
	switch args[0] {
	case "list":
		threadsListCmd(args[1:])
	case "show":
		threadsShowCmd(args[1:])
	case "search":
		threadsSearchCmd(args[1:])
	case "rename":
		threadsModifyCmd(args[1:], "rename")
	case "save":
		threadsModifyCmd(args[1:], "save")
	case "unsave":
		threadsModifyCmd(args[1:], "unsave")
	case "share":
		threadsModifyCmd(args[1:], "share")
	case "unshare":
		threadsModifyCmd(args[1:], "unshare")
	case "delete":
		threadsDeleteCmd(args[1:])
	default:
		die("unknown threads action: " + args[0])
	}
}

func threadsListCmd(args []string) {
	fs := flag.NewFlagSet("threads list", flag.ExitOnError)
	limit := fs.Int("limit", 100, "max threads per page")
	all := fs.Bool("all", false, "page through every thread (newest first)")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := newAuthedClient(resolveSession())

	var threads []client.ThreadSummary
	if *all {
		var err error
		threads, err = c.ListAllThreads(ctx, *limit, 0)
		if err != nil {
			die(err.Error())
		}
	} else {
		page, err := c.ListThreads(ctx, nil, *limit)
		if err != nil {
			die(err.Error())
		}
		threads = page.Threads
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(threads)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  THREAD_ID\tGROUP\tTITLE")
	for _, t := range threads {
		marker := " "
		if t.Shared {
			marker = "↗"
		}
		title := strings.ReplaceAll(t.Title, "\n", " ")
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\n", marker, t.ID, t.Group, title)
	}
	_ = w.Flush()
	fmt.Fprintf(os.Stderr, "\n%d thread(s).  ↗ = shared.  Use: kagi threads show <id>\n", len(threads))
}

func threadsShowCmd(args []string) {
	fs := flag.NewFlagSet("threads show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON (full conversation)")
	headOnly := fs.Bool("head", false, "print only the parent message id (for scripting)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		die("usage: kagi threads show [--json|--head] <thread-id>")
	}
	id := fs.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := newAuthedClient(resolveSession())

	d, err := c.ShowThread(ctx, id)
	if err != nil {
		die(err.Error())
	}
	if *headOnly {
		parent := d.LastMessageID()
		if parent == "" {
			die("thread has no messages")
		}
		fmt.Println(parent)
		return
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(d)
		return
	}
	fmt.Printf("# %s\nthread: %s  (saved=%v shared=%v)\n\n", d.Title, d.ID, d.Saved, d.Shared)
	for _, m := range d.Messages {
		fmt.Printf("--- you ---\n%s\n\n", strings.TrimSpace(m.Prompt))
		label := m.Profile.Name
		if label == "" {
			label = m.Profile.ModelName
		}
		body := m.Markdown
		if body == "" {
			body = htmlToText(m.Reply)
		}
		fmt.Printf("--- %s [%s, msg=%s] ---\n%s\n\n", label, m.Profile.Model, m.ID, strings.TrimSpace(body))
	}
	if parent := d.LastMessageID(); parent != "" {
		fmt.Fprintf(os.Stderr, "[follow-up: kagi chat -t %s --parent %s ...]\n", d.ID, parent)
	}
}

func threadsSearchCmd(args []string) {
	fs := flag.NewFlagSet("threads search", flag.ExitOnError)
	tagID := fs.String("tag", "", "restrict to a tag id")
	saved := fs.Bool("saved", false, "only saved threads")
	shared := fs.Bool("shared", false, "only shared threads")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		die("usage: kagi threads search [--saved] [--shared] [--tag <id>] <query...>")
	}
	q := strings.Join(fs.Args(), " ")

	opts := client.SearchOpts{TagID: *tagID}
	if *saved {
		t := true
		opts.Saved = &t
	}
	if *shared {
		t := true
		opts.Shared = &t
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	hits, err := newAuthedClient(resolveSession()).SearchThreads(ctx, q, opts)
	if err != nil {
		die(err.Error())
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(hits)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  RANK\tTHREAD_ID\tSNIPPET")
	for _, h := range hits {
		snippet := strings.ReplaceAll(htmlToText(h.Snippet), "\n", " ")
		if len(snippet) > 80 {
			snippet = snippet[:77] + "..."
		}
		fmt.Fprintf(w, "  %.2f\t%s\t%s\n", h.Rank, h.ThreadID, snippet)
	}
	_ = w.Flush()
	fmt.Fprintf(os.Stderr, "\n%d hit(s) for %q.\n", len(hits), q)
}

func threadsModifyCmd(args []string, action string) {
	usage := func() string { return "usage: kagi threads " + action + " <thread-id>" }
	if action == "rename" {
		usage = func() string { return "usage: kagi threads rename <thread-id> <new title...>" }
	}
	if len(args) < 1 {
		die(usage())
	}
	id := args[0]

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := newAuthedClient(resolveSession())

	// thread_modify expects the FULL state of the thread; we fetch the list
	// summary so we don't accidentally clobber tags/saved/shared with zero
	// values. We pull the current state from the thread show endpoint.
	d, err := c.ShowThread(ctx, id)
	if err != nil {
		die("fetch current state: " + err.Error())
	}
	mod := client.ThreadModification{
		ID:     id,
		Title:  d.Title,
		Saved:  d.Saved,
		Shared: d.Shared,
		TagIDs: d.TagIDs,
	}
	switch action {
	case "rename":
		if len(args) < 2 {
			die(usage())
		}
		mod.Title = strings.Join(args[1:], " ")
	case "save":
		mod.Saved = true
	case "unsave":
		mod.Saved = false
	case "share":
		mod.Shared = true
	case "unshare":
		mod.Shared = false
	}
	if mod.TagIDs == nil {
		mod.TagIDs = []string{}
	}
	if err := c.ModifyThreads(ctx, mod); err != nil {
		die(err.Error())
	}
	fmt.Fprintf(os.Stderr, "kagi: %s ok (thread %s)\n", action, id)
}

func threadsDeleteCmd(args []string) {
	if len(args) == 0 {
		die("usage: kagi threads delete <thread-id> [<thread-id>...]")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := newAuthedClient(resolveSession()).DeleteThreads(ctx, args...); err != nil {
		die(err.Error())
	}
	fmt.Fprintf(os.Stderr, "kagi: deleted %d thread(s)\n", len(args))
}

func assistantsCmd(args []string) {
	if len(args) < 1 {
		die("usage: kagi assistants <list|show|create|update|delete> ...")
	}
	switch args[0] {
	case "list":
		profilesCmd(args[1:])
	case "show":
		assistantsShowCmd(args[1:])
	case "create":
		assistantsCreateCmd(args[1:])
	case "update":
		assistantsUpdateCmd(args[1:])
	case "delete":
		assistantsDeleteCmd(args[1:])
	default:
		die("unknown assistants action: " + args[0])
	}
}

func assistantsShowCmd(args []string) {
	fs := flag.NewFlagSet("assistants show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of human form")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		die("usage: kagi assistants show [--json] <profile-uuid>")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	spec, err := newAuthedClient(resolveSession()).FetchCustomAssistant(ctx, fs.Arg(0))
	if err != nil {
		die(err.Error())
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(spec)
		return
	}
	fmt.Printf("id:                %s\n", spec.ID)
	fmt.Printf("name:              %s\n", spec.Name)
	fmt.Printf("base_model:        %s\n", spec.BaseModel)
	fmt.Printf("bang_trigger:      %s\n", spec.BangTrigger)
	fmt.Printf("internet_access:   %v\n", spec.InternetAccess)
	fmt.Printf("personalizations:  %v\n", spec.Personalizations)
	fmt.Printf("lens_id:           %s\n", spec.LensID)
	fmt.Printf("instructions:\n%s\n", spec.Instructions)
}

func assistantsCreateCmd(args []string) {
	fs := flag.NewFlagSet("assistants create", flag.ExitOnError)
	name := fs.String("n", "", "assistant name (required)")
	model := fs.String("m", "", "base model id (required, see: kagi models)")
	promptFile := fs.String("prompt", "", "path to system-prompt file (or - for stdin)")
	bang := fs.String("bang", "", "optional bang trigger (e.g. 'code' for !code)")
	noInternet := fs.Bool("no-internet", false, "disable internet access")
	noPersonalize := fs.Bool("no-personalize", false, "disable personalizations")
	lensID := fs.String("lens", "", "lens id (default: none)")
	_ = fs.Parse(args)

	if *name == "" || *model == "" {
		die("--n <name> and -m <model> are required")
	}
	instructions := ""
	if *promptFile != "" {
		var b []byte
		var err error
		if *promptFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*promptFile)
		}
		if err != nil {
			die("read prompt: " + err.Error())
		}
		instructions = string(b)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	id, err := newAuthedClient(resolveSession()).SaveCustomAssistant(ctx, client.CustomAssistantSpec{
		Name:             *name,
		BaseModel:        *model,
		Instructions:     instructions,
		BangTrigger:      *bang,
		InternetAccess:   !*noInternet,
		Personalizations: !*noPersonalize,
		LensID:           *lensID,
	})
	if err != nil {
		die(err.Error())
	}
	fmt.Println(id)
	fmt.Fprintf(os.Stderr, "kagi: created assistant %q (id=%s).  Use: kagi config set profile %s\n", *name, id, id)
}

func assistantsUpdateCmd(args []string) {
	if len(args) < 1 {
		die("usage: kagi assistants update <uuid> [-n <name>] [-m <model>] [--prompt <file|->] [--bang <trigger>] [--internet|--no-internet] [--personalize|--no-personalize] [--lens <id>]")
	}
	id := args[0]
	fs := flag.NewFlagSet("assistants update", flag.ExitOnError)
	name := fs.String("n", "", "new name (omit to keep)")
	model := fs.String("m", "", "new base model (omit to keep)")
	promptFile := fs.String("prompt", "", "new system-prompt file (or - for stdin; omit to keep)")
	bang := fs.String("bang", "", "new bang trigger (omit to keep, pass empty literal '\"\"' to clear)")
	internet := fs.Bool("internet", false, "force internet_access on (default: keep)")
	noInternet := fs.Bool("no-internet", false, "force internet_access off (default: keep)")
	personalize := fs.Bool("personalize", false, "force personalizations on (default: keep)")
	noPersonalize := fs.Bool("no-personalize", false, "force personalizations off (default: keep)")
	lensID := fs.String("lens", "", "new lens id (omit to keep, pass '0' to clear)")
	_ = fs.Parse(args[1:])

	if *internet && *noInternet {
		die("--internet and --no-internet are mutually exclusive")
	}
	if *personalize && *noPersonalize {
		die("--personalize and --no-personalize are mutually exclusive")
	}

	// Track which flags the user actually passed, so we can leave the rest
	// alone. flag.Visit only walks flags that were set on the command line.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := newAuthedClient(resolveSession())

	spec, err := c.FetchCustomAssistant(ctx, id)
	if err != nil {
		die("fetch current state: " + err.Error())
	}

	if set["n"] {
		spec.Name = *name
	}
	if set["m"] {
		spec.BaseModel = *model
	}
	if set["prompt"] {
		var b []byte
		if *promptFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*promptFile)
		}
		if err != nil {
			die("read prompt: " + err.Error())
		}
		spec.Instructions = string(b)
	}
	if set["bang"] {
		// `--bang ""` clears it; any other value sets it. The double-quote
		// hint in the help text is for shells that strip empty args.
		if *bang == `""` {
			spec.BangTrigger = ""
		} else {
			spec.BangTrigger = *bang
		}
	}
	if *internet {
		spec.InternetAccess = true
	}
	if *noInternet {
		spec.InternetAccess = false
	}
	if *personalize {
		spec.Personalizations = true
	}
	if *noPersonalize {
		spec.Personalizations = false
	}
	if set["lens"] {
		spec.LensID = *lensID
	}

	if _, err := c.SaveCustomAssistant(ctx, spec); err != nil {
		die(err.Error())
	}
	fmt.Fprintf(os.Stderr, "kagi: updated assistant %s\n", id)
}

func assistantsDeleteCmd(args []string) {
	if len(args) != 1 {
		die("usage: kagi assistants delete <profile-uuid>")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := newAuthedClient(resolveSession()).DeleteCustomAssistant(ctx, args[0]); err != nil {
		die(err.Error())
	}
	fmt.Fprintf(os.Stderr, "kagi: deleted assistant %s\n", args[0])
}

// htmlToText is a very lightweight HTML→text converter for the chat-bubble
// content rendered into the thread page. It strips tags and decodes entities;
// it does not try to preserve formatting beyond paragraph breaks.
func htmlToText(s string) string {
	// Replace block-level tags with newlines first so the stripped result
	// reads roughly like the original layout.
	for _, br := range []string{"</p>", "<br>", "<br/>", "<br />", "</li>", "</h1>", "</h2>", "</h3>"} {
		s = strings.ReplaceAll(s, br, "\n")
	}
	// Strip everything between angle brackets.
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(htmlUnescape(b.String()))
}

// htmlUnescape decodes the handful of HTML entities (&amp; &lt; etc.) that
// show up in chat-bubble content. Aliasing keeps the call sites short.
func htmlUnescape(s string) string { return html.UnescapeString(s) }

func die(msg string) {
	fmt.Fprintln(os.Stderr, "kagi:", msg)
	os.Exit(1)
}
