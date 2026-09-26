package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// collectSSE runs relaySSE over the given raw stream and returns the events it
// emitted plus the terminal error.
func collectSSE(t *testing.T, raw, convUUID string) ([]Event, error) {
	t.Helper()
	evs, err, _ := collectSSEState(t, raw, convUUID, &sseState{cursor: "0-0"})
	return evs, err
}

// collectSSEState is collectSSE with an explicit carry-over state, so tests can
// drive the reconnect path across two partial streams.
func collectSSEState(t *testing.T, raw, convUUID string, st *sseState) ([]Event, error, *sseState) {
	t.Helper()
	out := make(chan Event, 256)
	var err error
	done := make(chan struct{})
	go func() {
		err = relaySSE(context.Background(), strings.NewReader(raw), convUUID, out, st)
		close(out)
		close(done)
	}()
	var evs []Event
	for ev := range out {
		evs = append(evs, ev)
	}
	<-done
	return evs, err, st
}

func TestRelaySSE_NormalStream(t *testing.T) {
	// A representative v2 stream: title frame, an incremental frame, a final
	// frame with is_final, then the [DONE] sentinel.
	raw := strings.Join([]string{
		`id: 1-0`,
		`data: {"text":"","conversation_uuid":"conv1","branch_uuid":"b1","is_final":false,"conversation_title":"My Title"}`,
		``,
		`id: 2-0`,
		`data: {"text":"Fo","conversation_uuid":"conv1","is_final":false,"html_content":"<p>Fo</p>"}`,
		``,
		`id: 3-0`,
		`data: {"text":"Four","conversation_uuid":"conv1","is_final":true,"html_content":"<p>Four</p>","assistant_message_uuid":"asst1"}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	evs, err := collectSSE(t, raw, "conv1")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF terminal, got %v", err)
	}

	var gotTitle, gotDone bool
	var lastText, finalMD, finalReply string
	var tokenCount int
	for _, ev := range evs {
		switch ev.Type {
		case "thread.json":
			var m struct{ ID, Title string }
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				t.Fatalf("thread.json decode: %v", err)
			}
			if m.ID != "conv1" || m.Title != "My Title" {
				t.Errorf("thread.json = %+v, want id=conv1 title=My Title", m)
			}
			gotTitle = true
		case "tokens.json":
			var m struct{ Text, ID string }
			_ = json.Unmarshal(ev.Data, &m)
			lastText = m.Text
			tokenCount++
		case "new_message.json":
			var m struct {
				ID       string `json:"id"`
				ThreadID string `json:"thread_id"`
				State    string `json:"state"`
				Reply    string `json:"reply"`
				MD       string `json:"md"`
			}
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				t.Fatalf("new_message.json decode: %v", err)
			}
			if m.State != "done" {
				t.Errorf("new_message state = %q, want done", m.State)
			}
			if m.ID != "asst1" || m.ThreadID != "conv1" {
				t.Errorf("new_message ids = %q/%q, want asst1/conv1", m.ID, m.ThreadID)
			}
			finalMD, finalReply = m.MD, m.Reply
			gotDone = true
		}
	}

	if !gotTitle {
		t.Error("no thread.json title event emitted")
	}
	if !gotDone {
		t.Error("no terminal new_message.json emitted")
	}
	if tokenCount == 0 {
		t.Error("no tokens.json events emitted")
	}
	if lastText != "Four" {
		t.Errorf("last cumulative text = %q, want Four", lastText)
	}
	if finalMD != "Four" {
		t.Errorf("final md = %q, want Four", finalMD)
	}
	if finalReply != "<p>Four</p>" {
		t.Errorf("final reply = %q, want <p>Four</p>", finalReply)
	}
}

func TestRelaySSE_TitleEmittedOnce(t *testing.T) {
	// The same title appearing on multiple frames must only emit one
	// thread.json event.
	raw := strings.Join([]string{
		`data: {"text":"","is_final":false,"conversation_title":"T"}`,
		``,
		`data: {"text":"a","is_final":false,"conversation_title":"T"}`,
		``,
		`data: {"text":"ab","is_final":true,"assistant_message_uuid":"m"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	evs, _ := collectSSE(t, raw, "conv1")
	titles := 0
	for _, ev := range evs {
		if ev.Type == "thread.json" {
			titles++
		}
	}
	if titles != 1 {
		t.Errorf("thread.json emitted %d times, want 1", titles)
	}
}

func TestRelaySSE_ErrorFrame(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"text":"","is_final":false}`,
		``,
		`data: {"error":"upstream boom"}`,
		``,
	}, "\n")

	_, err := collectSSE(t, raw, "conv1")
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("expected error containing 'upstream boom', got %v", err)
	}
}

func TestRelaySSE_FallbackConvUUID(t *testing.T) {
	// When a frame omits conversation_uuid, relaySSE substitutes the one
	// resolved at chat start.
	raw := strings.Join([]string{
		`data: {"text":"hi","is_final":true,"assistant_message_uuid":"m","conversation_title":"X"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	evs, _ := collectSSE(t, raw, "fallback-conv")
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				ThreadID string `json:"thread_id"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			if m.ThreadID != "fallback-conv" {
				t.Errorf("thread_id = %q, want fallback-conv", m.ThreadID)
			}
		}
	}
}

func TestMessageBody_BaseVsProfile(t *testing.T) {
	// Base model: model_name set, no profile_uuid.
	base := NewPrompt("hello", "", "", "", "ki_quick", true)
	if base.Profile.ID != "" || base.Profile.Model != "ki_quick" {
		t.Errorf("base NewPrompt profile = %+v", base.Profile)
	}
	if base.Focus.ThreadID != nil {
		t.Error("new-conversation prompt should have nil ThreadID")
	}

	// Follow-up: thread id set.
	follow := NewPrompt("hi", "conv1", "parent1", "prof1", "model1", false)
	if follow.Focus.ThreadID == nil || *follow.Focus.ThreadID != "conv1" {
		t.Error("follow-up should carry the conversation id")
	}
	if follow.Profile.ID != "prof1" {
		t.Errorf("profile id = %q, want prof1", follow.Profile.ID)
	}
	if follow.Profile.InternetAccess {
		t.Error("internet should be false when passed false")
	}
}

func TestRelaySSE_TracksCursorAndResumes(t *testing.T) {
	// First connection dies mid-turn: no is_final, no [DONE]. The state must
	// carry the last event id so a reconnect can pick up from there.
	first := strings.Join([]string{
		`id: 1700000000001-0`,
		`data: {"text":"Fo","conversation_uuid":"conv1","is_final":false,"html_content":"<p>Fo</p>","conversation_title":"T"}`,
		``,
		`id: 1700000000002-0`,
		`data: {"text":"Four","conversation_uuid":"conv1","is_final":false,"html_content":"<p>Four</p>"}`,
		``,
	}, "\n")

	evs, err, st := collectSSEState(t, first, "conv1", &sseState{cursor: "0-0"})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("truncated stream should end in io.EOF, got %v", err)
	}
	if st.final || st.sawDone {
		t.Error("state marked complete without a terminal frame")
	}
	if st.cursor != "1700000000002-0" {
		t.Errorf("cursor = %q, want 1700000000002-0", st.cursor)
	}
	if st.frames != 2 {
		t.Errorf("frames = %d, want 2", st.frames)
	}
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			t.Error("terminal event emitted for a truncated stream")
		}
	}

	// Reconnect: the terminal frame carries only `text`, so the html must come
	// from the cumulative value captured before the drop.
	second := strings.Join([]string{
		`id: 1700000000003-0`,
		`data: {"text":"Four!","conversation_uuid":"conv1","is_final":true,"assistant_message_uuid":"asst1"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	evs2, _, st2 := collectSSEState(t, second, "conv1", st)
	if !st2.final {
		t.Error("state not marked final after the terminal frame")
	}
	var titles, finals int
	var md, reply string
	for _, ev := range evs2 {
		switch ev.Type {
		case "thread.json":
			titles++
		case "new_message.json":
			var m struct {
				MD    string `json:"md"`
				Reply string `json:"reply"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md, reply = m.MD, m.Reply
			finals++
		}
	}
	if titles != 0 {
		t.Errorf("title re-emitted %d times after reconnect, want 0", titles)
	}
	if finals != 1 {
		t.Fatalf("terminal events = %d, want 1", finals)
	}
	if md != "Four!" {
		t.Errorf("final md = %q, want Four!", md)
	}
	if reply != "<p>Four</p>" {
		t.Errorf("final reply = %q, want the html carried across the reconnect", reply)
	}
}

func TestRelaySSE_ErrorFrameIsNotRetryable(t *testing.T) {
	// An error reported inside a frame is upstream's verdict on the turn;
	// pumpStream must be able to tell it apart from a transport drop.
	raw := "data: {\"error\":\"upstream boom\"}\n\n"

	_, err := collectSSE(t, raw, "conv1")
	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *upstreamError, got %T (%v)", err, err)
	}
}

// --- pumpStream reconnect behaviour -----------------------------------------

// testTurn is the chatTurn the stream tests post as.
var testTurn = chatTurn{
	branchUUID:  "b1",
	convUUID:    "conv1",
	streamURL:   "/api/branches/b1/stream",
	userMsgUUID: "u1",
}

// newStreamTestClient points a Client at a local test server.
func newStreamTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-session")
	c.apiBase = srv.URL
	return c
}

// drain runs pumpStream over an already-open first connection and collects the
// events it emits.
func drain(t *testing.T, c *Client, turn chatTurn) ([]Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.openStream(ctx, turn.streamURL, "0-0")
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	out := make(chan Event, 256)
	var perr error
	done := make(chan struct{})
	go func() {
		perr = c.pumpStream(ctx, resp, turn, out)
		close(out)
		close(done)
	}()
	var evs []Event
	for ev := range out {
		evs = append(evs, ev)
	}
	<-done
	return evs, perr
}

func TestPumpStream_ReconnectsAfterDrop(t *testing.T) {
	// Mirrors the real failure: the first connection is killed mid-turn (the
	// edge proxy resets it once the cumulative frames get large), and the
	// answer only arrives on the reconnect.
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		cursor := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			if cursor != "0-0" {
				t.Errorf("first connect cursor = %q, want 0-0", cursor)
			}
			io.WriteString(w, "id: 10-0\ndata: {\"text\":\"partial\",\"conversation_uuid\":\"conv1\",\"is_final\":false,\"html_content\":\"<p>partial</p>\"}\n\n")
			// Close without a terminal frame.
			return
		}
		if cursor != "10-0" {
			t.Errorf("reconnect cursor = %q, want 10-0", cursor)
		}
		io.WriteString(w, "id: 11-0\ndata: {\"text\":\"complete\",\"conversation_uuid\":\"conv1\",\"is_final\":true,\"assistant_message_uuid\":\"asst1\"}\n\ndata: [DONE]\n\n")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":true,"branch_uuid":"b1"}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}

	var finals int
	var md, reply string
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				MD    string `json:"md"`
				Reply string `json:"reply"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md, reply = m.MD, m.Reply
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("terminal events = %d, want 1", finals)
	}
	if md != "complete" {
		t.Errorf("final md = %q, want complete", md)
	}
	if reply != "<p>partial</p>" {
		t.Errorf("final reply = %q, want the html carried across the reconnect", reply)
	}
}

func TestPumpStream_RecoversPersistedAnswer(t *testing.T) {
	// The stream is exhausted and the turn is over, but no terminal frame was
	// ever seen. The answer landed in the conversation anyway, so it must be
	// recovered rather than reported as lost.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":"b1"}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1","title":"T"},
			"active_branch":{"uuid":"b1","head_message_uuid":"asst1"},
			"messages":{"items":[
				{"uuid":"u1","role":"user","content":"q"},
				{"uuid":"asst1","role":"assistant","parent_message_uuid":"u1","content":"the answer","html_content":"<p>the answer</p>"}
			]}}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	var md, reply string
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				MD    string `json:"md"`
				Reply string `json:"reply"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md, reply = m.MD, m.Reply
		}
	}
	if md != "the answer" || reply != "<p>the answer</p>" {
		t.Errorf("recovered md/reply = %q/%q, want the persisted assistant message", md, reply)
	}
}

func TestPumpStream_ErrorFrameDoesNotReconnect(t *testing.T) {
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"error\":\"upstream boom\"}\n\n")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":true,"branch_uuid":"b1"}`))
	})

	c := newStreamTestClient(t, mux)
	_, err := drain(t, c, testTurn)
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("err = %v, want the upstream error", err)
	}
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Errorf("connections = %d, want 1 (no retry on an upstream error)", got)
	}
}

func TestPumpStream_IgnoresPreviousTurnsAnswer(t *testing.T) {
	// The turn died with nothing persisted. The conversation still holds the
	// *previous* turn's reply, which must not be passed off as this one's.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":"b1"}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},
			"active_branch":{"uuid":"b1"},
			"messages":{"items":[
				{"uuid":"u0","role":"user","content":"earlier q"},
				{"uuid":"asst0","role":"assistant","parent_message_uuid":"u0","content":"earlier answer"},
				{"uuid":"u1","role":"user","parent_message_uuid":"asst0","content":"q"}
			]}}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err == nil {
		t.Fatal("expected a premature-end error, got nil")
	}
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			t.Fatalf("emitted a terminal event from a stale message: %s", ev.Data)
		}
	}
}

func TestPumpStream_RecoversWhenDoneArrivesWithoutTerminalFrame(t *testing.T) {
	// The stream reaches [DONE] but the is_final frame never made it. The
	// answer is in the conversation, so it must be recovered rather than
	// reported as a completed-but-empty turn.
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 10-0\ndata: {\"text\":\"partial\",\"conversation_uuid\":\"conv1\",\"is_final\":false}\n\ndata: [DONE]\n\n")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":"b1"}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"b1"},
			"messages":{"items":[
				{"uuid":"u1","role":"user","content":"q"},
				{"uuid":"asst1","role":"assistant","parent_message_uuid":"u1","content":"the answer","html_content":"<p>the answer</p>"}
			]}}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Errorf("connections = %d, want 1 ([DONE] means no point reconnecting)", got)
	}
	var md string
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				MD string `json:"md"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md = m.MD
		}
	}
	if md != "the answer" {
		t.Errorf("recovered md = %q, want the persisted answer", md)
	}
}

func TestPumpStream_RetriesWhenReopenFails(t *testing.T) {
	// A transient failure while reopening the stream must not cost the answer:
	// it counts as a stall, and the next attempt succeeds.
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&connects, 1) {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "id: 10-0\ndata: {\"text\":\"partial\",\"conversation_uuid\":\"conv1\",\"is_final\":false}\n\n")
		case 2:
			http.Error(w, `{"error":{"code":"unavailable","message":"try later"}}`, http.StatusServiceUnavailable)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "id: 11-0\ndata: {\"text\":\"complete\",\"conversation_uuid\":\"conv1\",\"is_final\":true,\"assistant_message_uuid\":\"asst1\"}\n\ndata: [DONE]\n\n")
		}
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":true,"branch_uuid":"b1"}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 3 {
		t.Errorf("connections = %d, want 3 (initial, failed reopen, success)", got)
	}
	var md string
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				MD string `json:"md"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md = m.MD
		}
	}
	if md != "complete" {
		t.Errorf("final md = %q, want complete", md)
	}
}

func TestStartChat_FollowsForkedBranch(t *testing.T) {
	// Posting can fork a branch; the reply then streams from the branch in the
	// response, which is also the one whose status we must poll.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"old-branch"},"messages":{"items":[]}}`))
	})
	mux.HandleFunc("/api/branches/old-branch/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"branch":{"uuid":"forked-branch"},
			"user_message":{"uuid":"u9"},
			"stream_url":"/api/branches/forked-branch/stream"}`))
	})

	c := newStreamTestClient(t, mux)
	turn, err := c.startChat(context.Background(), NewPrompt("hi", "conv1", "", "", "m", false))
	if err != nil {
		t.Fatalf("startChat: %v", err)
	}
	if turn.branchUUID != "forked-branch" {
		t.Errorf("branchUUID = %q, want forked-branch", turn.branchUUID)
	}
	if turn.userMsgUUID != "u9" {
		t.Errorf("userMsgUUID = %q, want u9", turn.userMsgUUID)
	}
	if turn.streamURL != "/api/branches/forked-branch/stream" {
		t.Errorf("streamURL = %q", turn.streamURL)
	}
}

func TestRelaySSE_StopsAtTerminalFrame(t *testing.T) {
	// A second is_final frame (or a connection held open past the first) must
	// not produce a second terminal event.
	raw := strings.Join([]string{
		`id: 1-0`,
		`data: {"text":"done","conversation_uuid":"conv1","is_final":true,"assistant_message_uuid":"asst1"}`,
		``,
		`id: 2-0`,
		`data: {"text":"done again","conversation_uuid":"conv1","is_final":true,"assistant_message_uuid":"asst2"}`,
		``,
	}, "\n")

	evs, err := collectSSE(t, raw, "conv1")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF right after the terminal frame", err)
	}
	finals := 0
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			finals++
		}
	}
	if finals != 1 {
		t.Errorf("terminal events = %d, want 1", finals)
	}
}

func TestPumpStream_UnblocksOnCancel(t *testing.T) {
	// A consumer that cancels and walks away must not leave the stream
	// goroutine wedged on a full channel.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			io.WriteString(w, "id: 1-0\ndata: {\"text\":\"x\",\"conversation_uuid\":\"conv1\",\"is_final\":false}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	})

	c := newStreamTestClient(t, mux)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := c.openStream(ctx, testTurn.streamURL, "0-0")
	if err != nil {
		cancel()
		t.Fatalf("openStream: %v", err)
	}

	// Never read from out, so the buffer fills and the sender blocks.
	out := make(chan Event, 1)
	done := make(chan error, 1)
	go func() { done <- c.pumpStream(ctx, resp, testTurn, out) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("pumpStream err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pumpStream did not return after cancellation")
	}
}

func TestPumpStream_RecoveryEmitsTokensForStreamMode(t *testing.T) {
	// `kagi chat --stream` prints only tokens.json, so a recovered answer has
	// to arrive as a token frame too, not just in the terminal event.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 10-0\ndata: {\"text\":\"partial\",\"conversation_uuid\":\"conv1\",\"is_final\":false}\n\ndata: [DONE]\n\n")
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":"b1"}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"b1"},
			"messages":{"items":[
				{"uuid":"u1","role":"user","content":"q"},
				{"uuid":"asst1","role":"assistant","parent_message_uuid":"u1","content":"partial then the rest","html_content":"<p>x</p>"}
			]}}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	var lastToken string
	for _, ev := range evs {
		if ev.Type == "tokens.json" {
			var m struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			lastToken = m.Text
		}
	}
	if lastToken != "partial then the rest" {
		t.Errorf("last token text = %q, want the full recovered answer", lastToken)
	}
}

func TestOpenStream_AcceptsNoContent(t *testing.T) {
	// 204 means the turn's stream is gone. That has to read as an empty stream
	// so recovery runs, not as a hard error that drops the answer.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":null}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"b1"},
			"messages":{"items":[
				{"uuid":"u1","role":"user","content":"q"},
				{"uuid":"asst1","role":"assistant","parent_message_uuid":"u1","content":"late answer","html_content":"<p>late</p>"}
			]}}`))
	})

	c := newStreamTestClient(t, mux)
	evs, err := drain(t, c, testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	var md string
	for _, ev := range evs {
		if ev.Type == "new_message.json" {
			var m struct {
				MD string `json:"md"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			md = m.MD
		}
	}
	if md != "late answer" {
		t.Errorf("md = %q, want the recovered answer after a 204", md)
	}
}

func TestPrematureEnd_PointsAtTheConversation(t *testing.T) {
	err := prematureEnd("conv1", "", nil)
	if !strings.Contains(err.Error(), "kagi threads show conv1") {
		t.Errorf("err = %q, want a pointer to re-check the conversation", err)
	}
}

func TestPumpStream_NamesTheUsageCap(t *testing.T) {
	// Hitting the usage cap kills a turn mid-generation with no error frame and
	// nothing persisted. The failure must say so rather than read as a network
	// problem.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/branches/b1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/branches/b1/stream/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active":false,"branch_uuid":null}`))
	})
	mux.HandleFunc("/api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"b1"},
			"messages":{"items":[{"uuid":"u1","role":"user","content":"q"}]}}`))
	})
	mux.HandleFunc("/api/billing/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limit":"hard","reason":"cost","can_proceed":false}`))
	})

	c := newStreamTestClient(t, mux)
	_, err := drain(t, c, testTurn)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage cap") {
		t.Errorf("err = %q, want it to name the usage cap", err)
	}
}
