package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const (
	// BaseURL is the kagi.com origin, used only for the sign-in flow — the
	// kagi_session cookie it sets is scoped to .kagi.com and shared with the
	// assistant subdomain.
	BaseURL = "https://kagi.com"
	// APIBase is the new Kagi Assistant app (a SvelteKit SPA backed by a JSON
	// REST API). All assistant operations live under APIBase + "/api/...".
	APIBase = "https://assistant.kagi.com"

	DefaultUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

type Client struct {
	Session   string
	Email     string
	Password  string
	UserAgent string
	HTTP      *http.Client
	// OnRefresh, if set, is called with the new session value after a
	// successful auto-login or explicit Login() call.
	OnRefresh func(session string)

	// OnReconnect, if set, is called before each SSE reconnect. A long turn
	// legitimately drops several connections, so callers want to say so rather
	// than look frozen.
	OnReconnect func(attempt int, cursor string)

	// apiBase overrides APIBase and authBase overrides BaseURL; only tests
	// set them.
	apiBase  string
	authBase string
}

// base is the assistant API origin every /api/* call is built against.
func (c *Client) base() string {
	if c.apiBase != "" {
		return c.apiBase
	}
	return APIBase
}

func New(session string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		Session:   session,
		UserAgent: DefaultUA,
		HTTP: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetCredentials enables auto-relogin: when a request fails with an auth
// error, the client will silently re-authenticate once and retry.
func (c *Client) SetCredentials(email, password string) {
	c.Email = email
	c.Password = password
}

// newRequestURL builds an HTTP request with the spoofed User-Agent always set,
// so every outbound call looks like a browser.
func (c *Client) newRequestURL(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUA
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return req, nil
}

func (c *Client) hasCreds() bool {
	return c.Email != "" && c.Password != ""
}

// ErrNotFound is returned by lookup helpers when the upstream resource
// genuinely doesn't exist (a clean 404 from the JSON API), as distinct from an
// auth failure. Callers can do `errors.Is(err, client.ErrNotFound)`.
var ErrNotFound = errors.New("not found")

// apiError is the typed error envelope every /api/* endpoint returns on
// failure: {"error":{"code","message","request_id","fields":[...]}}.
type apiError struct {
	Err struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// apiDo performs a JSON request against the assistant API with cookie auth and
// one transparent relogin on 401/403. The new API returns a clean 401 for
// unauthenticated requests (unlike the old 404-as-auth-fail quirk) and a real
// 404 for genuinely-missing resources, which we surface as ErrNotFound.
//
// out may be nil (response body discarded). reqBody may be nil (no body).
func (c *Client) apiDo(ctx context.Context, method, path string, reqBody, out any) error {
	return c.apiDoRetry(ctx, method, path, reqBody, out, false)
}

func (c *Client) apiDoRetry(ctx context.Context, method, path string, reqBody, out any, retried bool) error {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return errors.New("client: empty session (set KAGI_SESSION or KAGI_EMAIL/KAGI_PASSWORD)")
		}
	}

	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := c.newRequestURL(ctx, method, c.base()+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Origin", c.base())
	req.Header.Set("Referer", c.base()+"/")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	if isAuthFail(resp) {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.apiDoRetry(ctx, method, path, reqBody, out, true)
		}
		return fmt.Errorf("auth failed (status %d); refresh KAGI_SESSION or set KAGI_EMAIL/KAGI_PASSWORD", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("%s %s: %s", method, path, apiErrorMessage(raw, resp.StatusCode))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// apiErrorMessage extracts the typed error envelope's message, falling back to
// a raw-body excerpt and finally the status code.
func apiErrorMessage(raw []byte, status int) string {
	var e apiError
	if json.Unmarshal(raw, &e) == nil && e.Err.Message != "" {
		if e.Err.Code != "" {
			return fmt.Sprintf("http %d: %s (%s)", status, e.Err.Message, e.Err.Code)
		}
		return fmt.Sprintf("http %d: %s", status, e.Err.Message)
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 {
		return fmt.Sprintf("http %d: %s", status, truncate(t, 200))
	}
	return fmt.Sprintf("http %d", status)
}

// isAuthFail reports whether a response indicates the session is invalid. The
// assistant API uses a conventional 401/403; we also treat 3xx redirects to
// /signin or /signup as auth failure (the SPA bounces there when unauthed).
func isAuthFail(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "/signin") || strings.Contains(loc, "/signup") {
			return true
		}
	}
	return false
}

// --- Wire types (assistant API v2) -----------------------------------------

// Conversation is the v2 replacement for a "thread" — one chat session.
type Conversation struct {
	UUID         string `json:"uuid"`
	Title        string `json:"title"`
	IsSaved      bool   `json:"is_saved"`
	IsShared     bool   `json:"is_shared"`
	IsPinned     bool   `json:"is_pinned"`
	ModelName    string `json:"model_name"`
	FolderUUID   string `json:"folder_uuid"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int    `json:"message_count"`
	HasBranches  bool   `json:"has_branches"`
	IsOwner      bool   `json:"is_owner"`
}

// Branch is a linear message chain within a conversation. New conversations
// have one default branch; re-rolling a turn forks a new branch.
type Branch struct {
	UUID             string `json:"uuid"`
	ConversationUUID string `json:"conversation_uuid"`
	HeadMessageUUID  string `json:"head_message_uuid"`
	IsDefault        bool   `json:"is_default"`
	MessageCount     int    `json:"message_count"`
}

// Message is one row in a conversation (user and assistant are separate rows).
type Message struct {
	UUID              string `json:"uuid"`
	Role              string `json:"role"` // "user" | "assistant"
	Content           string `json:"content"`
	HTMLContent       string `json:"html_content"`
	ParentMessageUUID string `json:"parent_message_uuid"`
	CreatedAt         string `json:"created_at"`
	ModelName         string `json:"model_name"`
	ModelDisplayName  string `json:"model_display_name"`
	ModelProvider     string `json:"model_provider"`
	ProfileUUID       string `json:"profile_uuid"`
	ProfileName       string `json:"profile_name"`
}

// --- Prompt request (compatibility shape) ----------------------------------

// PromptRequest is the assembled chat request. Its shape is preserved from the
// v1 client so the CLI and HTTP server call sites stay unchanged; Send/Stream
// translate it into the v2 three-step flow internally.
type PromptRequest struct {
	Focus   Focus   `json:"focus"`
	Profile Profile `json:"profile"`
}

type Focus struct {
	ThreadID  *string `json:"thread_id"` // conversation uuid; nil = new conversation
	Prompt    string  `json:"prompt"`
	MessageID string  `json:"message_id,omitempty"` // accepted but unused in v2 (branch tracks the head)
}

type Profile struct {
	ID               string  `json:"id"`    // custom-assistant uuid; "" = use a base model
	Model            string  `json:"model"` // base model id / override
	Personalizations bool    `json:"personalizations"`
	InternetAccess   bool    `json:"internet_access"`
	LensID           *string `json:"lens_id"`
}

type Event struct {
	Type string
	Data []byte
}

type ChatResult struct {
	ThreadID  string `json:"thread_id"` // conversation uuid
	MessageID string `json:"message_id"`
	Title     string `json:"title,omitempty"`
	Markdown  string `json:"md,omitempty"`    // final assistant text
	HTML      string `json:"reply,omitempty"` // final rendered html
}

// NewPrompt builds a PromptRequest. If threadID is empty it creates a new
// conversation; otherwise it appends to the existing conversation. parentMsgID
// is accepted for v1 compatibility but ignored — v2 threads follow-ups onto
// the branch head automatically.
func NewPrompt(prompt, threadID, parentMsgID, profileID, model string, internet bool) PromptRequest {
	focus := Focus{Prompt: prompt}
	if threadID != "" {
		tid := threadID
		focus.ThreadID = &tid
		focus.MessageID = parentMsgID
	}
	return PromptRequest{
		Focus: focus,
		Profile: Profile{
			ID:               profileID,
			Personalizations: true,
			InternetAccess:   internet,
			Model:            model,
		},
	}
}

// --- Chat flow (v2 turns) ---------------------------------------------------

// respondBody is the payload of both POST /api/v2/conversations (new
// conversation + first turn in one call) and POST /api/v2/branches/{b}/respond
// (follow-up turn on an existing branch).
type respondBody struct {
	Content         string  `json:"content"`
	Model           string  `json:"model,omitempty"`
	ProfileUUID     string  `json:"profile_uuid,omitempty"`
	EnableSearch    *bool   `json:"enable_search,omitempty"`
	Personalization *bool   `json:"personalization,omitempty"`
	LensID          *string `json:"lens_id,omitempty"`
}

// dispatched is what both dispatch endpoints return: the turn that is now
// running and where it runs.
type dispatched struct {
	ConversationUUID  string `json:"conversation_uuid"`
	BranchUUID        string `json:"branch_uuid"`
	AssistantTurnUUID string `json:"assistant_turn_uuid"`
	UserTurnUUID      string `json:"user_turn_uuid"`
}

// startChat resolves the conversation/branch and dispatches the user turn,
// returning everything the reader needs to pick the reply up. It does NOT
// read the stream; Send and Stream do that differently.
func (c *Client) startChat(ctx context.Context, req PromptRequest) (turn chatTurn, err error) {
	model := req.Profile.Model
	profileUUID := req.Profile.ID
	if model == "" && profileUUID == "" {
		return turn, errors.New("chat: model or profile required")
	}

	body := respondBody{Content: req.Focus.Prompt, Model: model, ProfileUUID: profileUUID}
	internet := req.Profile.InternetAccess
	personalization := req.Profile.Personalizations
	body.EnableSearch = &internet
	body.Personalization = &personalization
	body.LensID = req.Profile.LensID

	convUUID := ""
	if req.Focus.ThreadID != nil {
		convUUID = *req.Focus.ThreadID
	}

	var d dispatched
	if convUUID == "" {
		if err = c.apiDo(ctx, http.MethodPost, "/api/v2/conversations", body, &d); err != nil {
			return turn, dispatchError("create conversation", profileUUID, err)
		}
	} else {
		// Existing conversation: respond on its active branch.
		var init conversationInit
		if err = c.apiDo(ctx, http.MethodGet, "/api/conversations/"+convUUID+"/init", nil, &init); err != nil {
			return turn, fmt.Errorf("load conversation: %w", err)
		}
		branchUUID := init.ActiveBranch.UUID
		if branchUUID == "" && len(init.Branches) > 0 {
			branchUUID = init.Branches[0].UUID
		}
		if branchUUID == "" {
			return turn, fmt.Errorf("conversation %s: no branch to post to", convUUID)
		}
		if err = c.apiDo(ctx, http.MethodPost, "/api/v2/branches/"+branchUUID+"/respond", body, &d); err != nil {
			return turn, dispatchError("post message", profileUUID, err)
		}
		// The server may answer on a different branch than the one we named;
		// the reply and its persisted row live on the one it reports.
		if d.BranchUUID == "" {
			d.BranchUUID = branchUUID
		}
		if d.ConversationUUID == "" {
			d.ConversationUUID = convUUID
		}
	}
	if d.AssistantTurnUUID == "" {
		return turn, errors.New("chat: server did not return an assistant turn to stream")
	}
	return chatTurn{
		convUUID:   d.ConversationUUID,
		branchUUID: d.BranchUUID,
		turnUUID:   d.AssistantTurnUUID,
		streamURL:  "/api/v2/turns/" + url.PathEscape(d.AssistantTurnUUID) + "/stream",
	}, nil
}

func dispatchError(step, profileUUID string, err error) error {
	if errors.Is(err, ErrNotFound) && profileUUID != "" {
		return fmt.Errorf("%s: profile %q not found — it may be a stale v1 profile id; pick one from `kagi profiles` or run `kagi config set profile <uuid>`", step, profileUUID)
	}
	return fmt.Errorf("%s: %w", step, err)
}

// chatTurn is what startChat resolved: the assistant turn being generated and
// where it lives.
type chatTurn struct {
	convUUID   string
	branchUUID string
	turnUUID   string // assistant turn uuid; also the persisted message uuid
	streamURL  string
}

// sseFrame is the JSON payload of one `data:` line on the turn stream. Most
// frames wrap a turn event; a few are bare (`title`, the closing `is_final`).
type sseFrame struct {
	Event   *turnEvent `json:"event"`
	Title   string     `json:"title"`
	IsFinal bool       `json:"is_final"`
	Error   string     `json:"error"`
}

// turnEvent is one event of the event-sourced turn log. Text arrives as
// `node.delta` appends; `node.completed`/`node.replaced` carry the full text.
type turnEvent struct {
	Seq          int          `json:"seq"`
	TurnID       string       `json:"turn_id"`
	Type         string       `json:"type"`
	NodeID       string       `json:"node_id"`
	ParentNodeID string       `json:"parent_node_id"`
	Payload      eventPayload `json:"payload"`
}

type eventPayload struct {
	RootNodeID string          `json:"root_node_id"`
	Kind       string          `json:"kind"`
	Delta      string          `json:"delta"`
	Content    json.RawMessage `json:"content"` // a string on text nodes; other shapes elsewhere
	ErrorCode  string          `json:"error_code"`
}

// contentString returns payload.content when it is a string.
func (p eventPayload) contentString() (string, bool) {
	if len(p.Content) == 0 || p.Content[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(p.Content, &s) != nil {
		return "", false
	}
	return s, true
}

// Stream constants. A single SSE connection does not survive every long turn
// (observed: the edge proxy resetting it at ~5 minutes), while the generation
// keeps running server-side and is persisted when it finishes. So a dropped
// connection is reconnected rather than given up on — which is what the
// official web client does too.
const (
	// streamMaxStalls bounds *consecutive* reconnects that yield no new
	// events. A healthy long turn always makes progress, so this only trips
	// when the stream is genuinely over or wedged.
	streamMaxStalls = 5
	// streamRetryDelay is the pause before each reconnect attempt.
	streamRetryDelay = 2 * time.Second
	// streamRecoverTries is how many times the persisted-answer fallback
	// re-reads the turn before giving up.
	streamRecoverTries = 3
)

// turnNode is one node of the turn's tree. Only kind "text" nodes carry the
// answer; the rest (thinking, tool calls) are tracked so their deltas are not
// mistaken for answer text.
type turnNode struct {
	kind   string
	parent string
	text   string
}

// sseState is the turn reconstruction carried across reconnects. Events are
// deltas, so a replayed event must not be applied twice: the server replays a
// finished turn under fresh SSE ids, which makes the cursor useless for
// dedupe, so applied events are tracked by their per-turn seq instead.
type sseState struct {
	cursor    string         // last SSE id seen; "0-0" means "from the start"
	ended     bool           // the turn is over on the wire (is_final or a terminal turn event)
	final     bool           // the terminal new_message.json was emitted
	events    int            // events applied, used to detect progress
	seen      map[string]int // highest applied seq per turn id
	rootNode  string         // root node of our turn; its text children form the reply
	nodes     map[string]*turnNode
	order     []string // node ids in creation order
	titleSent string
	lastText  string // reply text last emitted as tokens
}

func newSSEState() *sseState {
	return &sseState{cursor: "0-0", seen: map[string]int{}, nodes: map[string]*turnNode{}}
}

// replyText joins the text nodes directly under the turn's root, in order.
// Nested nodes belong to sub-agents and are not part of the answer.
func (st *sseState) replyText() string {
	var parts []string
	for _, id := range st.order {
		n := st.nodes[id]
		if n.kind == "text" && n.parent == st.rootNode && n.text != "" {
			parts = append(parts, n.text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// apply folds one turn event into the state. It reports whether the event was
// new and, for a failed or cancelled turn, the upstream error.
func (st *sseState) apply(ev *turnEvent, turnUUID string) (bool, error) {
	if ev.Seq > 0 {
		if ev.Seq <= st.seen[ev.TurnID] {
			return false, nil
		}
		st.seen[ev.TurnID] = ev.Seq
	}
	st.events++
	ours := ev.TurnID == "" || ev.TurnID == turnUUID
	p := ev.Payload
	switch ev.Type {
	case "turn.started":
		if ours && p.RootNodeID != "" {
			st.rootNode = p.RootNodeID
		}
	case "turn.completed", "turn.awaiting_input":
		if ours {
			st.ended = true
		}
	case "turn.failed":
		if ours {
			return true, &upstreamError{msg: "turn failed" + codeSuffix(p.ErrorCode)}
		}
	case "turn.cancelled":
		if ours {
			return true, &upstreamError{msg: "turn was cancelled"}
		}
	case "node.created":
		n := st.node(ev, p.Kind)
		if p.Kind != "" {
			n.kind = p.Kind
		}
		if s, ok := p.contentString(); ok {
			n.text = s
		}
	case "node.delta":
		// A delta for a node never announced is text, as in the web
		// client's reducer.
		st.node(ev, "text").text += p.Delta
	case "node.completed", "node.replaced":
		n := st.node(ev, "text")
		if s, ok := p.contentString(); ok {
			n.text = s
		}
	case "node.failed":
		if ours && ev.NodeID != "" && ev.NodeID == st.rootNode {
			return true, &upstreamError{msg: "reply failed" + codeSuffix(p.ErrorCode)}
		}
	}
	return true, nil
}

// node returns the node for ev, creating it with kind on first sight.
func (st *sseState) node(ev *turnEvent, kind string) *turnNode {
	if n, ok := st.nodes[ev.NodeID]; ok {
		return n
	}
	n := &turnNode{kind: kind, parent: ev.ParentNodeID}
	st.nodes[ev.NodeID] = n
	st.order = append(st.order, ev.NodeID)
	return n
}

func codeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " (" + code + ")"
}

// Stream runs a chat turn and emits events. For compatibility with the v1
// consumers (client.Send, the HTTP server's snoop+relay), the events are
// emitted under the v1 type names — `thread.json`, `tokens.json`, and a
// terminal `new_message.json` carrying state="done" — synthesised from the v2
// turn stream.
func (c *Client) Stream(ctx context.Context, req PromptRequest) (<-chan Event, <-chan error, error) {
	turn, err := c.startChat(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	// The first connection is opened synchronously so that a hard failure
	// (auth, 4xx) is reported to the caller instead of arriving late on the
	// error channel.
	resp, err := c.openStream(ctx, turn.streamURL, "0-0")
	if err != nil {
		return nil, nil, err
	}

	events := make(chan Event, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)
		if err := c.pumpStream(ctx, resp, turn, events); err != nil {
			errs <- err
		}
	}()

	return events, errs, nil
}

// openStream opens one SSE connection to the turn stream at the given cursor.
// The caller owns the returned body.
func (c *Client) openStream(ctx context.Context, streamURL, cursor string) (*http.Response, error) {
	u := c.base() + streamURL
	// The web client omits the cursor when reading from the start.
	if cursor != "" && cursor != "0-0" {
		sep := "?"
		if strings.Contains(streamURL, "?") {
			sep = "&"
		}
		u += sep + "cursor=" + url.QueryEscape(cursor)
	}
	req, err := c.newRequestURL(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Origin", c.base())
	req.Header.Set("Referer", c.base()+"/")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	if isAuthFail(resp) {
		resp.Body.Close()
		return nil, fmt.Errorf("auth failed (status %d) opening stream", resp.StatusCode)
	}
	// 204 means the turn's stream no longer exists (it ended, or its buffer
	// was reclaimed). That is an empty read, not a failure: the reader sees
	// EOF, the stall path checks the turn status, and recovery takes over.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("stream %s", apiErrorMessage(raw, resp.StatusCode))
	}
	return resp, nil
}

// pumpStream drains the turn stream to completion, reconnecting from the last
// event id whenever the connection drops before the turn ends. It takes
// ownership of resp.
func (c *Client) pumpStream(ctx context.Context, resp *http.Response, turn chatTurn, out chan<- Event) error {
	st := newSSEState()
	stalls, reconnects := 0, 0
	var lastErr error

	for {
		if resp != nil {
			before := st.events
			lastErr = relaySSE(ctx, resp.Body, turn, out, st)
			resp.Body.Close()
			resp = nil

			if st.ended {
				return c.finishTurn(ctx, turn, out, st)
			}
			// An event saying the turn failed is upstream's verdict, not a
			// transport hiccup — reconnecting would just replay it.
			var ue *upstreamError
			if errors.As(lastErr, &ue) {
				return lastErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if st.events > before {
				stalls = 0
			} else {
				stalls++
			}
		}

		// Two fruitless attempts in a row with the turn no longer running:
		// nothing more will arrive on the wire. One is not enough — generation
		// can go quiet between chunks and finish right after the status check,
		// leaving the terminal events still unread.
		if stalls >= 2 {
			if t, ok, serr := c.persistedTurn(ctx, turn); serr == nil && ok && t.Status != "running" {
				return c.recoverPersisted(ctx, turn, out, st, lastErr)
			}
		}
		if stalls >= streamMaxStalls {
			return c.recoverPersisted(ctx, turn, out, st, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(streamRetryDelay):
		}

		reconnects++
		if c.OnReconnect != nil {
			c.OnReconnect(reconnects, st.cursor)
		}
		next, err := c.openStream(ctx, turn.streamURL, st.cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failed reopen is just another stall. Spending the budget here
			// keeps a transient outage from costing the answer, since the
			// recovery path is still ahead.
			lastErr = err
			stalls++
			continue
		}
		resp = next
	}
}

// persistedTurnRow is one row of GET /api/v2/branches/{b}/turns. Unlike the
// stream, it carries the server-rendered html_content.
type persistedTurnRow struct {
	UUID        string `json:"uuid"`
	Role        string `json:"role"`
	Content     string `json:"content"`
	HTMLContent string `json:"html_content"`
	Status      string `json:"status"` // running | completed | failed | cancelled | awaiting_input
	ErrorCode   string `json:"error_code"`
}

// persistedTurn reads our assistant turn back from its branch. ok is false
// when the turn is not (yet) among the branch's latest turns.
func (c *Client) persistedTurn(ctx context.Context, turn chatTurn) (persistedTurnRow, bool, error) {
	var page struct {
		Turns []persistedTurnRow `json:"turns"`
	}
	if err := c.apiDo(ctx, http.MethodGet, "/api/v2/branches/"+turn.branchUUID+"/turns?limit=4", nil, &page); err != nil {
		return persistedTurnRow{}, false, err
	}
	for _, t := range page.Turns {
		if t.UUID == turn.turnUUID {
			return t, true, nil
		}
	}
	return persistedTurnRow{}, false, nil
}

// finishTurn emits the terminal event once the stream reported the turn over.
// The persisted row is preferred — it is the canonical markdown and the only
// source of HTML — but the answer already streamed is not thrown away when
// that read fails.
func (c *Client) finishTurn(ctx context.Context, turn chatTurn, out chan<- Event, st *sseState) error {
	md, html := st.replyText(), ""
	// The row can trail the stream's end by a beat, as in recoverPersisted.
	for attempt := 0; ; attempt++ {
		t, ok, err := c.persistedTurn(ctx, turn)
		if err == nil && ok && t.Status != "running" {
			if t.Status == "failed" || t.Status == "cancelled" {
				return &upstreamError{msg: "turn " + t.Status + codeSuffix(t.ErrorCode)}
			}
			if t.Content != "" {
				md = t.Content
			}
			html = t.HTMLContent
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= streamRecoverTries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(streamRetryDelay):
		}
	}
	return emitDone(ctx, turn, out, st, md, html)
}

// recoverPersisted is the last resort when the stream stopped without the
// turn ending on the wire. The generation outlives the connection and is
// written to the branch when it finishes, so a completed answer is often
// already readable even though we never saw it end.
func (c *Client) recoverPersisted(ctx context.Context, turn chatTurn, out chan<- Event, st *sseState, cause error) error {
	// The write can trail the stream going idle by a beat, so give it a few
	// tries before declaring the answer lost.
	for attempt := 0; ; attempt++ {
		t, ok, err := c.persistedTurn(ctx, turn)
		if err == nil && ok {
			switch t.Status {
			case "failed", "cancelled":
				return &upstreamError{msg: "turn " + t.Status + codeSuffix(t.ErrorCode)}
			case "running":
			default:
				if t.Content != "" {
					return emitDone(ctx, turn, out, st, t.Content, t.HTMLContent)
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= streamRecoverTries-1 {
			return c.giveUp(ctx, turn.convUUID, cause)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(streamRetryDelay):
		}
	}
}

// emitDone sends the terminal new_message.json. When the final text differs
// from what was streamed it is sent as a token frame first: `kagi chat
// --stream` prints only tokens, so without this the terminal event would carry
// an answer the user never sees.
func emitDone(ctx context.Context, turn chatTurn, out chan<- Event, st *sseState, md, html string) error {
	if md != st.lastText {
		tok, _ := json.Marshal(map[string]string{"text": md, "id": turn.turnUUID})
		if err := sendEvent(ctx, out, Event{Type: "tokens.json", Data: tok}); err != nil {
			return err
		}
		st.lastText = md
	}
	done, _ := json.Marshal(map[string]any{
		"id":        turn.turnUUID,
		"thread_id": turn.convUUID,
		"state":     "done",
		"reply":     html,
		"md":        md,
	})
	if err := sendEvent(ctx, out, Event{Type: "new_message.json", Data: done}); err != nil {
		return err
	}
	st.final = true
	return nil
}

// billingStatus is GET /api/billing/status.
type billingStatus struct {
	CanProceed bool   `json:"can_proceed"`
	Limit      string `json:"limit"`
	Reason     string `json:"reason"`
}

// billingBlock explains a usage cap when that is why a turn stopped producing
// output, and returns "" otherwise. Hitting the cap mid-generation kills the
// turn silently, which is indistinguishable from a network failure unless we
// go and ask.
func (c *Client) billingBlock(ctx context.Context) string {
	var b billingStatus
	if err := c.apiDo(ctx, http.MethodGet, "/api/billing/status", nil, &b); err != nil || b.CanProceed {
		return ""
	}
	return fmt.Sprintf("the account's usage cap is reached (limit=%q, reason=%q), which cuts a turn off mid-generation", b.Limit, b.Reason)
}

// giveUp builds the terminal error, asking why the turn stopped first. The
// lookup can itself be cancelled, which the caller should see as cancellation
// rather than as a lost answer.
func (c *Client) giveUp(ctx context.Context, convUUID string, cause error) error {
	note := c.billingBlock(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return prematureEnd(convUUID, note, cause)
}

func prematureEnd(convUUID, note string, cause error) error {
	msg := "the reply stream ended without an answer"
	if note != "" {
		msg += " — " + note
	}
	if convUUID != "" {
		msg += fmt.Sprintf("; the turn may still finish server-side — re-check with `kagi threads show %s`", convUUID)
	}
	if cause != nil && !errors.Is(cause, io.EOF) {
		return fmt.Errorf("%s (last error: %w)", msg, cause)
	}
	return errors.New(msg)
}

// sendEvent delivers one event, giving up if the consumer is gone. A plain
// channel send would block forever on a full buffer once the caller cancels
// and stops draining, leaking the stream goroutine and never closing either
// channel.
func sendEvent(ctx context.Context, out chan<- Event, ev Event) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// upstreamError marks an error the server reported for the turn, as opposed
// to a transport failure. Only the latter is worth reconnecting for.
type upstreamError struct{ msg string }

func (e *upstreamError) Error() string { return e.msg }

// relaySSE reads the v2 turn stream, folds each event into st and translates
// the result into the v1-style events the existing consumers expect: the reply
// is rebuilt from text-node deltas and forwarded cumulatively as
// `tokens.json.text` whenever it changes. It returns io.EOF once the turn has
// ended (st.ended) or the connection closed; the terminal event is emitted by
// the caller, which can read the persisted HTML.
func relaySSE(ctx context.Context, r io.Reader, turn chatTurn, out chan<- Event, st *sseState) error {
	br := bufio.NewReader(r)
	var dataBuf bytes.Buffer
	pendingID := ""

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		eventID := pendingID
		pendingID = ""
		if strings.TrimSpace(payload) == "[DONE]" {
			st.ended = true
			return io.EOF
		}
		var fr sseFrame
		if err := json.Unmarshal([]byte(payload), &fr); err != nil {
			return nil // ignore non-JSON keepalive frames
		}
		// Only advance the cursor once the frame has been understood, so a
		// reconnect never skips past a frame we failed to process.
		if eventID != "" {
			st.cursor = eventID
		}
		if fr.Error != "" {
			return &upstreamError{msg: fr.Error}
		}
		if fr.Title != "" && fr.Title != st.titleSent {
			st.titleSent = fr.Title
			b, _ := json.Marshal(map[string]string{"id": turn.convUUID, "title": fr.Title})
			if err := sendEvent(ctx, out, Event{Type: "thread.json", Data: b}); err != nil {
				return err
			}
		}
		if fr.Event != nil {
			fresh, uerr := st.apply(fr.Event, turn.turnUUID)
			if uerr != nil {
				return uerr
			}
			if fresh {
				if text := st.replyText(); text != st.lastText {
					st.lastText = text
					tok, _ := json.Marshal(map[string]string{"text": text, "id": turn.turnUUID})
					if err := sendEvent(ctx, out, Event{Type: "tokens.json", Data: tok}); err != nil {
						return err
					}
				}
			}
		}
		if fr.IsFinal {
			st.ended = true
		}
		// The turn is over. Stop reading rather than wait out a connection
		// the server may hold open past the end.
		if st.ended {
			return io.EOF
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// Blank line terminates an event.
			if ferr := flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(trimmed, "data:"):
			d := strings.TrimPrefix(trimmed, "data:")
			d = strings.TrimPrefix(d, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(d)
		case strings.HasPrefix(trimmed, "id:"):
			pendingID = strings.TrimSpace(strings.TrimPrefix(trimmed, "id:"))
		case strings.HasPrefix(trimmed, ":"), strings.HasPrefix(trimmed, "event:"):
			// comment / event-name lines: ignored.
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if ferr := flush(); ferr != nil && !errors.Is(ferr, io.EOF) {
					return ferr
				}
				return io.EOF
			}
			return err
		}
	}
}

// Send runs a chat turn to completion, returning the assembled result. onToken
// (if non-nil) receives the cumulative reply text as it streams.
func (c *Client) Send(ctx context.Context, req PromptRequest, onToken func(text string)) (*ChatResult, error) {
	events, errs, err := c.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &ChatResult{}
	var done bool
	for ev := range events {
		switch ev.Type {
		case "thread.json":
			var t struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}
			if json.Unmarshal(ev.Data, &t) == nil {
				if t.ID != "" {
					res.ThreadID = t.ID
				}
				if t.Title != "" {
					res.Title = t.Title
				}
			}
		case "tokens.json":
			var tok struct {
				Text string `json:"text"`
				ID   string `json:"id"`
			}
			if json.Unmarshal(ev.Data, &tok) == nil {
				if tok.ID != "" {
					res.MessageID = tok.ID
				}
				// Keep the cumulative text as we go: if the turn dies, the
				// caller should still get the part that did arrive.
				if tok.Text != "" {
					res.Markdown = tok.Text
				}
				if onToken != nil {
					onToken(tok.Text)
				}
			}
		case "new_message.json":
			var m struct {
				ID       string `json:"id"`
				ThreadID string `json:"thread_id"`
				State    string `json:"state"`
				Reply    string `json:"reply"`
				MD       string `json:"md"`
			}
			if json.Unmarshal(ev.Data, &m) == nil {
				if m.ID != "" {
					res.MessageID = m.ID
				}
				if m.ThreadID != "" {
					res.ThreadID = m.ThreadID
				}
				if m.State == "done" {
					res.HTML = m.Reply
					res.Markdown = m.MD
					done = true
				}
			}
		}
	}
	if err, ok := <-errs; ok && err != nil {
		return res, err
	}
	if !done {
		return res, errors.New("stream ended before completion")
	}
	return res, nil
}
