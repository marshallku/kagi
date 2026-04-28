package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/marshallku/kagi-cli/client"
)

type Server struct {
	session          string
	defaultProfileID string
	defaultModel     string
}

func New(session string) *Server {
	return &Server{
		session:          session,
		defaultProfileID: os.Getenv("KAGI_PROFILE_ID"),
		defaultModel:     getenvOr("KAGI_MODEL", "ki_quick"),
	}
}

type chatRequest struct {
	Prompt         string `json:"prompt"`
	ThreadID       string `json:"thread_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Model          string `json:"model,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	InternetAccess *bool  `json:"internet_access,omitempty"`
}

func (s *Server) buildPrompt(req chatRequest) (client.PromptRequest, error) {
	if req.Prompt == "" {
		return client.PromptRequest{}, fmt.Errorf("prompt required")
	}
	if req.ThreadID != "" && req.MessageID == "" {
		return client.PromptRequest{}, fmt.Errorf("message_id required when thread_id is set")
	}
	profileID := req.ProfileID
	if profileID == "" {
		profileID = s.defaultProfileID
	}
	if profileID == "" {
		return client.PromptRequest{}, fmt.Errorf("profile_id required (or set KAGI_PROFILE_ID)")
	}
	model := req.Model
	if model == "" {
		model = s.defaultModel
	}
	internet := true
	if req.InternetAccess != nil {
		internet = *req.InternetAccess
	}
	return client.NewPrompt(req.Prompt, req.ThreadID, req.MessageID, profileID, model, internet), nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pr, err := s.buildPrompt(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := client.New(s.session).Send(r.Context(), pr, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

	events, errs, streamErr := client.New(s.session).Stream(r.Context(), pr)
	if streamErr != nil {
		http.Error(w, streamErr.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	for ev := range events {
		writeSSE(w, ev.Type, ev.Data)
		flusher.Flush()
	}
	if err := <-errs; err != nil {
		writeSSE(w, "error", []byte(err.Error()))
		flusher.Flush()
	}
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Routes())
}

func getenvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
