package client

import (
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
	BaseURL         = "https://kagi.com"
	PromptPath      = "/assistant/prompt"
	DefaultBranchID = "00000000-0000-4000-0000-000000000000"
	StreamAccept    = "application/vnd.kagi.stream"
	DefaultUA       = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
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

// newRequest builds an HTTP request with the spoofed User-Agent always set,
// so every outbound call (login, prompt, page fetches) looks like a browser.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUA
	}
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, body)
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

// ErrNotFound is returned by lookup helpers (ShowThread, FetchCustomAssistant)
// when the upstream resource genuinely doesn't exist — distinct from a
// transient 404 that the auto-relogin path treats as auth failure. Callers
// can do `errors.Is(err, client.ErrNotFound)` to surface a 404 instead of
// reporting an auth or upstream failure.
var ErrNotFound = errors.New("not found")

// isAuthFail reports whether a response indicates the session is invalid.
// Kagi obscures the auth check by serving 404 for unauthenticated requests
// to /assistant/prompt rather than the conventional 401/403; we also treat
// 302 redirects to /signin or /signup as auth failure.
func isAuthFail(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
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

type PromptRequest struct {
	Focus   Focus    `json:"focus"`
	Profile Profile  `json:"profile"`
	Threads []Thread `json:"threads,omitempty"`
}

type Focus struct {
	ThreadID  *string `json:"thread_id"`
	BranchID  string  `json:"branch_id"`
	Prompt    string  `json:"prompt"`
	MessageID string  `json:"message_id,omitempty"`
}

type Profile struct {
	ID               string  `json:"id"`
	Personalizations bool    `json:"personalizations"`
	InternetAccess   bool    `json:"internet_access"`
	Model            string  `json:"model"`
	LensID           *string `json:"lens_id"`
}

type Thread struct {
	TagIDs []string `json:"tag_ids"`
	Saved  bool     `json:"saved"`
	Shared bool     `json:"shared"`
}

type Event struct {
	Type string
	Data []byte
}

type ChatResult struct {
	ThreadID  string `json:"thread_id"`
	MessageID string `json:"message_id"`
	Title     string `json:"title,omitempty"`
	Markdown  string `json:"md,omitempty"`
	HTML      string `json:"reply,omitempty"`
}

func (c *Client) Stream(ctx context.Context, req PromptRequest) (<-chan Event, <-chan error, error) {
	return c.stream(ctx, req, false)
}

func (c *Client) stream(ctx context.Context, req PromptRequest, retried bool) (<-chan Event, <-chan error, error) {
	if c.Session == "" {
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, nil, fmt.Errorf("auto-login: %w", err)
			}
		} else {
			return nil, nil, errors.New("client: empty session (set KAGI_SESSION or KAGI_EMAIL/KAGI_PASSWORD)")
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, PromptPath, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", StreamAccept)
	httpReq.Header.Set("Cookie", "kagi_session="+c.Session)
	httpReq.Header.Set("Referer", BaseURL+"/assistant")
	httpReq.Header.Set("Origin", BaseURL)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("send: %w", err)
	}
	if isAuthFail(resp) {
		resp.Body.Close()
		if !retried && c.hasCreds() {
			if err := c.Login(ctx); err != nil {
				return nil, nil, fmt.Errorf("relogin after auth fail: %w", err)
			}
			return c.stream(ctx, req, true)
		}
		return nil, nil, fmt.Errorf("auth failed (status %d); refresh KAGI_SESSION or set KAGI_EMAIL/KAGI_PASSWORD", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}

	events := make(chan Event, 16)
	errs := make(chan error, 1)

	go func() {
		defer resp.Body.Close()
		defer close(events)
		defer close(errs)
		if err := parseStream(resp.Body, events); err != nil && !errors.Is(err, io.EOF) {
			errs <- err
		}
	}()

	return events, errs, nil
}

func parseStream(r io.Reader, out chan<- Event) error {
	var buf bytes.Buffer
	chunk := make([]byte, 8192)
	for {
		n, readErr := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			for {
				idx := bytes.IndexByte(buf.Bytes(), 0)
				if idx < 0 {
					break
				}
				rec := bytes.TrimLeft(buf.Bytes()[:idx], "\r\n")
				buf.Next(idx + 1)
				if len(rec) == 0 {
					continue
				}
				colon := bytes.IndexByte(rec, ':')
				if colon < 0 {
					continue
				}
				out <- Event{
					Type: string(rec[:colon]),
					Data: append([]byte(nil), rec[colon+1:]...),
				}
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

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

// NewPrompt builds a PromptRequest. If threadID is empty it creates a new
// thread; otherwise it appends to the existing thread anchored at parentMsgID.
func NewPrompt(prompt, threadID, parentMsgID, profileID, model string, internet bool) PromptRequest {
	focus := Focus{BranchID: DefaultBranchID, Prompt: prompt}
	var threads []Thread
	if threadID == "" {
		threads = []Thread{{TagIDs: []string{}, Saved: true, Shared: false}}
	} else {
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
		Threads: threads,
	}
}
