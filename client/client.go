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
	"strings"
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

// newRequest builds a request against the kagi.com origin (sign-in flow).
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	return c.newRequestURL(ctx, method, BaseURL+path, body)
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

	req, err := c.newRequestURL(ctx, method, APIBase+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "kagi_session="+c.Session)
	req.Header.Set("Origin", APIBase)
	req.Header.Set("Referer", APIBase+"/")

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

// --- Chat flow (v2) ---------------------------------------------------------

// messageBody is the POST /api/branches/{branch}/messages payload.
type messageBody struct {
	Message         string  `json:"message"`
	ThinkingPreset  *string `json:"thinking_preset"`
	ModelName       string  `json:"model_name,omitempty"`
	ProfileUUID     string  `json:"profile_uuid,omitempty"`
	EnableSearch    *bool   `json:"enable_search,omitempty"`
	Personalization *bool   `json:"personalization,omitempty"`
	LensID          *string `json:"lens_id,omitempty"`
}

// startChat runs the first three steps of the v2 flow — resolve/create the
// conversation + branch and post the user message — returning the branch uuid
// (which the stream lives under), the conversation uuid, and the SSE stream
// path. It does NOT read the stream; Send and Stream do that differently.
func (c *Client) startChat(ctx context.Context, req PromptRequest) (branchUUID, convUUID, streamURL string, err error) {
	model := req.Profile.Model
	profileUUID := req.Profile.ID

	convUUID = ""
	if req.Focus.ThreadID != nil {
		convUUID = *req.Focus.ThreadID
	}

	if convUUID == "" {
		// New conversation. POST /api/conversations needs a model_name; when a
		// custom assistant is selected without an explicit model, fall back to
		// the assistant's own base model.
		createModel := model
		if createModel == "" && profileUUID != "" {
			ca, lerr := c.findAssistant(ctx, profileUUID)
			if lerr != nil {
				return "", "", "", fmt.Errorf("resolve assistant model: %w", lerr)
			}
			createModel = ca.LLMID
		}
		if createModel == "" {
			return "", "", "", errors.New("chat: model or profile required")
		}
		var created struct {
			Conversation  Conversation `json:"conversation"`
			DefaultBranch Branch       `json:"default_branch"`
		}
		if err = c.apiDo(ctx, http.MethodPost, "/api/conversations",
			map[string]string{"model_name": createModel}, &created); err != nil {
			return "", "", "", fmt.Errorf("create conversation: %w", err)
		}
		convUUID = created.Conversation.UUID
		branchUUID = created.DefaultBranch.UUID
	} else {
		// Existing conversation: post onto its active branch.
		var init conversationInit
		if err = c.apiDo(ctx, http.MethodGet, "/api/conversations/"+convUUID+"/init", nil, &init); err != nil {
			return "", "", "", fmt.Errorf("load conversation: %w", err)
		}
		branchUUID = init.ActiveBranch.UUID
		if branchUUID == "" && len(init.Branches) > 0 {
			branchUUID = init.Branches[0].UUID
		}
		if branchUUID == "" {
			return "", "", "", fmt.Errorf("conversation %s: no branch to post to", convUUID)
		}
	}

	body := messageBody{Message: req.Focus.Prompt}
	internet := req.Profile.InternetAccess
	personalization := req.Profile.Personalizations
	if profileUUID != "" {
		body.ProfileUUID = profileUUID
		if model != "" {
			body.ModelName = model
		}
	} else {
		body.ModelName = model
	}
	body.EnableSearch = &internet
	body.Personalization = &personalization
	body.LensID = req.Profile.LensID

	var posted struct {
		StreamURL string `json:"stream_url"`
	}
	if err = c.apiDo(ctx, http.MethodPost, "/api/branches/"+branchUUID+"/messages", body, &posted); err != nil {
		if errors.Is(err, ErrNotFound) && profileUUID != "" {
			return "", "", "", fmt.Errorf("post message: profile %q not found — it may be a stale v1 profile id; pick one from `kagi profiles` or run `kagi config set profile <uuid>`", profileUUID)
		}
		return "", "", "", fmt.Errorf("post message: %w", err)
	}
	streamURL = posted.StreamURL
	if streamURL == "" {
		streamURL = "/api/branches/" + branchUUID + "/stream"
	}
	return branchUUID, convUUID, streamURL, nil
}

// sseEvent is the JSON payload of each `data:` line in the response stream.
type sseEvent struct {
	Text               string `json:"text"`
	HTMLContent        string `json:"html_content"`
	ConversationUUID   string `json:"conversation_uuid"`
	BranchUUID         string `json:"branch_uuid"`
	ConversationTitle  string `json:"conversation_title"`
	ModelName          string `json:"model_name"`
	IsFinal            bool   `json:"is_final"`
	AssistantMessageID string `json:"assistant_message_uuid"`
	Error              string `json:"error"`
}

// Stream runs a chat turn and emits events. For compatibility with the v1
// consumers (client.Send, the HTTP server's snoop+relay), the events are
// emitted under the v1 type names — `thread.json`, `tokens.json`, and a
// terminal `new_message.json` carrying state="done" — synthesised from the v2
// SSE stream.
func (c *Client) Stream(ctx context.Context, req PromptRequest) (<-chan Event, <-chan error, error) {
	branchUUID, convUUID, streamURL, err := c.startChat(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	_ = branchUUID

	httpReq, err := c.newRequestURL(ctx, http.MethodGet, APIBase+streamURL+"?cursor=0-0", nil)
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cookie", "kagi_session="+c.Session)
	httpReq.Header.Set("Origin", APIBase)
	httpReq.Header.Set("Referer", APIBase+"/")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("open stream: %w", err)
	}
	if isAuthFail(resp) {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("auth failed (status %d) opening stream", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("stream %s", apiErrorMessage(raw, resp.StatusCode))
	}

	events := make(chan Event, 16)
	errs := make(chan error, 1)

	go func() {
		defer resp.Body.Close()
		defer close(events)
		defer close(errs)
		if err := relaySSE(resp.Body, convUUID, events); err != nil && !errors.Is(err, io.EOF) {
			errs <- err
		}
	}()

	return events, errs, nil
}

// relaySSE reads the v2 Server-Sent Events stream and translates each into the
// v1-style events the existing consumers expect. `text` is cumulative, so we
// forward it as the `tokens.json.text` field on every update.
func relaySSE(r io.Reader, convUUID string, out chan<- Event) error {
	br := bufio.NewReader(r)
	var dataBuf bytes.Buffer
	titleSent := ""

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		if strings.TrimSpace(payload) == "[DONE]" {
			return io.EOF
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil // ignore non-JSON keepalive frames
		}
		if ev.Error != "" {
			return errors.New(ev.Error)
		}
		cid := ev.ConversationUUID
		if cid == "" {
			cid = convUUID
		}
		// Title (emitted once, usually on the first frame).
		if ev.ConversationTitle != "" && ev.ConversationTitle != titleSent {
			titleSent = ev.ConversationTitle
			b, _ := json.Marshal(map[string]string{"id": cid, "title": ev.ConversationTitle})
			out <- Event{Type: "thread.json", Data: b}
		}
		// Incremental tokens (text is cumulative).
		tok, _ := json.Marshal(map[string]string{"text": ev.Text, "id": ev.AssistantMessageID})
		out <- Event{Type: "tokens.json", Data: tok}
		// Terminal frame.
		if ev.IsFinal {
			done, _ := json.Marshal(map[string]any{
				"id":        ev.AssistantMessageID,
				"thread_id": cid,
				"state":     "done",
				"reply":     ev.HTMLContent,
				"md":        ev.Text,
			})
			out <- Event{Type: "new_message.json", Data: done}
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
		case strings.HasPrefix(trimmed, "id:"), strings.HasPrefix(trimmed, ":"), strings.HasPrefix(trimmed, "event:"):
			// id / comment / event-name lines: ignored.
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
