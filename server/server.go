package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/marshallku/kagi/client"
)

type Server struct {
	mu               sync.RWMutex
	session          string
	email            string
	password         string
	defaultProfileID string
	defaultModel     string
}

func New(session string) *Server {
	cfg, _ := client.LoadConfig()
	return &Server{
		session:          session,
		defaultProfileID: cfg.Profile,
		defaultModel:     cfg.Model,
	}
}

// SetCredentials enables auto-relogin on auth failure. The server persists
// any refreshed session value to the keyring transparently.
func (s *Server) SetCredentials(email, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.email = email
	s.password = password
}

// newClient builds a client snapshot for one request. OnRefresh writes any
// new session back to the server's master state and the OS keyring, so
// subsequent requests automatically pick up the refreshed cookie.
func (s *Server) newClient() *client.Client {
	s.mu.RLock()
	sess, email, pw := s.session, s.email, s.password
	s.mu.RUnlock()

	c := client.New(sess)
	if email != "" && pw != "" {
		c.SetCredentials(email, pw)
	}
	c.OnRefresh = func(ns string) {
		s.mu.Lock()
		s.session = ns
		s.mu.Unlock()
		if err := client.SaveSession(ns); err != nil {
			fmt.Fprintln(os.Stderr, "kagi: warn: keyring save failed:", err)
		}
	}
	return c
}

type chatRequest struct {
	Prompt         string `json:"prompt"`
	ThreadID       string `json:"thread_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Model          string `json:"model,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	InternetAccess *bool  `json:"internet_access,omitempty"`
	// Resume continues the most recent thread for this host (the same state
	// file `kagi chat --resume` reads/writes — $XDG_STATE_HOME/kagi/
	// last-session.json). Mutually exclusive with thread_id; intended for
	// single-user / loopback deployments where the server and the CLI share
	// the same host state.
	Resume bool `json:"resume,omitempty"`
}

// buildPrompt assembles a client.PromptRequest. Validation only — no I/O.
// Callers handle auto-resolution of message_id (see resolveAutoParent) so
// that lookup failures stay distinguishable from validation failures (400)
// at the HTTP layer.
func (s *Server) buildPrompt(req chatRequest) (client.PromptRequest, error) {
	if req.Prompt == "" {
		return client.PromptRequest{}, fmt.Errorf("prompt required")
	}
	if req.ThreadID != "" && req.MessageID == "" {
		return client.PromptRequest{}, fmt.Errorf("message_id required when thread_id is set (or call resolveAutoParent first)")
	}
	profileID := req.ProfileID
	if profileID == "" {
		profileID = s.defaultProfileID
	}
	if profileID == "" {
		return client.PromptRequest{}, fmt.Errorf("profile_id required (per-request or run: kagi config set profile <uuid>)")
	}
	model := req.Model
	if model == "" {
		model = s.defaultModel
	}
	if model == "" {
		return client.PromptRequest{}, fmt.Errorf("model required (per-request or run: kagi config set model <id>)")
	}
	internet := true
	if req.InternetAccess != nil {
		internet = *req.InternetAccess
	}
	return client.NewPrompt(req.Prompt, req.ThreadID, req.MessageID, profileID, model, internet), nil
}

// errEmptyThread is the sentinel for "thread exists but has no messages
// yet". Distinct from ErrNotFound (the thread doesn't exist) and from
// upstream errors — it's a client-input issue (you can't continue a thread
// that has nothing to continue), so the chat handlers map it to 400.
var errEmptyThread = errors.New("thread has no messages — cannot determine parent")

// resolveAutoParent fills in req.MessageID from the thread's last turn when
// the caller passed thread_id without one. Also handles the "resume" flag:
// reads the last-session state file and pre-populates thread_id +
// message_id. Returns errResumeUnavailable if resume is requested but no
// state exists, errResumeConflict if combined with explicit thread_id.
// Other errors come from the underlying ShowThread lookup.
func (s *Server) resolveAutoParent(ctx context.Context, req chatRequest) (chatRequest, error) {
	if req.Resume {
		// Match the CLI's --resume preconditions: cannot combine with
		// thread_id OR message_id. Silently overwriting either would
		// surprise the caller.
		if req.ThreadID != "" || req.MessageID != "" {
			return req, errResumeConflict
		}
		last, err := client.LoadLastSession()
		if err != nil {
			return req, fmt.Errorf("load last session: %w", err)
		}
		if last.ThreadID == "" {
			return req, errResumeUnavailable
		}
		req.ThreadID = last.ThreadID
		req.MessageID = last.MessageID
	}
	if req.ThreadID == "" || req.MessageID != "" {
		return req, nil
	}
	d, err := s.newClient().ShowThread(ctx, req.ThreadID)
	if err != nil {
		return req, err
	}
	req.MessageID = d.LastMessageID()
	if req.MessageID == "" {
		return req, fmt.Errorf("thread %s: %w", req.ThreadID, errEmptyThread)
	}
	return req, nil
}

// errResumeUnavailable / errResumeConflict cover the two precondition
// failures of {"resume":true}. Both are 400-class — caller-input issues,
// not upstream or auth problems.
var (
	errResumeUnavailable = errors.New("no saved session at " + client.StatePath() + " — run a chat first")
	errResumeConflict    = errors.New("resume cannot be combined with thread_id or message_id")
)

// isAutoParentClientErr distinguishes auto-parent failures that are
// caller-input issues (400) from auth/upstream/not-found ones routed
// through httpClientError. Centralised so handleChat and handleChatStream
// stay in sync.
func isAutoParentClientErr(err error) bool {
	return errors.Is(err, errEmptyThread) ||
		errors.Is(err, errResumeUnavailable) ||
		errors.Is(err, errResumeConflict)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := s.resolveAutoParent(r.Context(), req)
	if err != nil {
		if isAutoParentClientErr(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpClientError(w, err)
		return
	}
	pr, err := s.buildPrompt(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.newClient().Send(r.Context(), pr, nil)
	if err != nil {
		// Use the same classifier as the other handlers so auth failures
		// surface as 401 instead of 502.
		httpClientError(w, err)
		return
	}
	s.persistLastSession(res, pr)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := s.resolveAutoParent(r.Context(), req)
	if err != nil {
		if isAutoParentClientErr(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpClientError(w, err)
		return
	}
	pr, err := s.buildPrompt(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	events, errs, streamErr := s.newClient().Stream(r.Context(), pr)
	if streamErr != nil {
		// Stream-startup error happens before we've sent any SSE bytes, so
		// it's safe to send a normal HTTP error response.
		httpClientError(w, streamErr)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Snoop the stream for thread/message ids and the terminal state="done"
	// flag so we can persist last-session.json — but ONLY if the stream
	// actually completed. The unary client.Send enforces the same invariant
	// (errors with "stream ended before completion" otherwise); we replicate
	// it here so a truncated SSE doesn't corrupt the resume state.
	res := &client.ChatResult{}
	var done bool
	for ev := range events {
		writeSSE(w, ev.Type, ev.Data)
		flusher.Flush()
		if snoopChatEvent(res, ev) {
			done = true
		}
	}
	if err := <-errs; err != nil {
		writeSSE(w, "error", []byte(err.Error()))
		flusher.Flush()
		return
	}
	if done {
		s.persistLastSession(res, pr)
	}
}

// snoopChatEvent picks thread.json + new_message.json fields out of the
// streamed events to populate a ChatResult — mirrors what client.Send does
// internally for the unary path. Returns true when the terminal
// `new_message.json` with state="done" arrives, signalling completion.
func snoopChatEvent(res *client.ChatResult, ev client.Event) (done bool) {
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
	case "new_message.json":
		var m struct {
			ID       string `json:"id"`
			ThreadID string `json:"thread_id"`
			State    string `json:"state"`
		}
		if json.Unmarshal(ev.Data, &m) == nil {
			if m.ID != "" {
				res.MessageID = m.ID
			}
			if m.ThreadID != "" {
				res.ThreadID = m.ThreadID
			}
			if m.State == "done" {
				return true
			}
		}
	}
	return false
}

// persistLastSession writes the result of a successful /chat or
// /chat/stream call to $XDG_STATE_HOME/kagi/last-session.json — the same
// file the CLI's `kagi chat --resume` reads. Failures are logged, not
// fatal: persisting state is best-effort.
func (s *Server) persistLastSession(res *client.ChatResult, pr client.PromptRequest) {
	if res == nil || res.ThreadID == "" || res.MessageID == "" {
		return
	}
	if err := client.SaveLastSession(client.LastSession{
		ThreadID:  res.ThreadID,
		MessageID: res.MessageID,
		Title:     res.Title,
		Model:     pr.Profile.Model,
		Profile:   pr.Profile.ID,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "kagi: warn: state save failed:", err)
	}
}

// GET /threads/last — read $XDG_STATE_HOME/kagi/last-session.json. 404 if
// no thread has been completed yet on this host. Useful for clients that
// want to inspect the resume target before acting on it.
func (s *Server) handleThreadsLast(w http.ResponseWriter, r *http.Request) {
	last, err := client.LoadLastSession()
	if err != nil {
		httpClientError(w, err)
		return
	}
	if last.ThreadID == "" {
		http.Error(w, "no saved session", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, last)
}

// writeSSE emits a properly formatted Server-Sent Event. SSE forbids raw
// newlines in a data field, so multiline payloads are split into multiple
// `data:` lines per the EventStream spec.
func writeSSE(w http.ResponseWriter, eventType string, data []byte) {
	fmt.Fprintf(w, "event: %s\n", eventType)
	for _, line := range strings.Split(string(data), "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("POST /chat/stream", s.handleChatStream)

	// Threads
	mux.HandleFunc("GET /threads", s.handleThreadsList)
	mux.HandleFunc("GET /threads/last", s.handleThreadsLast)
	mux.HandleFunc("POST /threads/search", s.handleThreadsSearch)
	mux.HandleFunc("POST /threads/delete", s.handleThreadsBulkDelete)
	mux.HandleFunc("GET /threads/{id}", s.handleThreadShow)
	mux.HandleFunc("PATCH /threads/{id}", s.handleThreadModify)
	mux.HandleFunc("DELETE /threads/{id}", s.handleThreadDelete)

	// Custom Assistants
	mux.HandleFunc("POST /assistants", s.handleAssistantCreate)
	mux.HandleFunc("GET /assistants/{id}", s.handleAssistantShow)
	mux.HandleFunc("PATCH /assistants/{id}", s.handleAssistantUpdate)
	mux.HandleFunc("DELETE /assistants/{id}", s.handleAssistantDelete)

	// Discovery (read-only)
	mux.HandleFunc("GET /models", s.handleModelsList)
	mux.HandleFunc("GET /profiles", s.handleProfilesList)
	mux.HandleFunc("GET /assistants", s.handleProfilesList) // alias of /profiles

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Routes())
}
