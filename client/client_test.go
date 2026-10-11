package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testTurn is the chatTurn the stream tests read.
var testTurn = chatTurn{
	convUUID:   "conv1",
	branchUUID: "b1",
	turnUUID:   "t1",
	streamURL:  "/api/v2/turns/t1/stream",
}

// frame renders one SSE frame. An empty id omits the id line.
func frame(id, data string) string {
	if id == "" {
		return "data: " + data + "\n\n"
	}
	return "id: " + id + "\ndata: " + data + "\n\n"
}

// ev renders one turn-event frame of turn t1.
func ev(id string, seq int, typ, node, parent, payload string) string {
	return evOf(id, "t1", seq, typ, node, parent, payload)
}

// evOf is ev for an arbitrary turn id.
func evOf(id, turnID string, seq int, typ, node, parent, payload string) string {
	e := fmt.Sprintf(`{"seq":%d,"turn_id":%q,"type":%q`, seq, turnID, typ)
	if node != "" {
		e += fmt.Sprintf(`,"node_id":%q`, node)
	}
	if parent != "" {
		e += fmt.Sprintf(`,"parent_node_id":%q`, parent)
	}
	if payload == "" {
		payload = "{}"
	}
	e += `,"payload":` + payload + `}`
	return frame(id, `{"event":`+e+`}`)
}

// answerStream is a complete turn whose answer is "Four", streamed as deltas.
func answerStream() string {
	return ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		ev("2-0", 2, "node.created", "r1", "", `{"kind":"assistant_message","status":"running"}`) +
		frame("3-0", `{"title":"My Title"}`) +
		ev("4-0", 3, "node.created", "n1", "r1", `{"kind":"text","format":"markdown","status":"running"}`) +
		ev("5-0", 4, "node.delta", "n1", "r1", `{"delta":"Fo"}`) +
		ev("6-0", 5, "node.delta", "n1", "r1", `{"delta":"ur"}`) +
		ev("7-0", 6, "node.completed", "n1", "r1", `{"status":"completed","content":"Four"}`) +
		ev("8-0", 7, "turn.completed", "", "", `{"root_node_id":"r1"}`) +
		frame("9-0", `{"is_final":true}`)
}

// collectSSE runs relaySSE over raw with a fresh state.
func collectSSE(t *testing.T, raw string) ([]Event, error, *sseState) {
	t.Helper()
	return collectSSEState(t, raw, newSSEState())
}

// collectSSEState is collectSSE with an explicit carry-over state, so tests can
// drive the reconnect path across two partial streams.
func collectSSEState(t *testing.T, raw string, st *sseState) ([]Event, error, *sseState) {
	t.Helper()
	out := make(chan Event, 256)
	var err error
	done := make(chan struct{})
	go func() {
		err = relaySSE(context.Background(), strings.NewReader(raw), testTurn, out, st)
		close(out)
		close(done)
	}()
	var evs []Event
	for e := range out {
		evs = append(evs, e)
	}
	<-done
	return evs, err, st
}

// tokenTexts returns the text of every tokens.json event.
func tokenTexts(evs []Event) []string {
	var texts []string
	for _, e := range evs {
		if e.Type == "tokens.json" {
			var m struct{ Text string }
			_ = json.Unmarshal(e.Data, &m)
			texts = append(texts, m.Text)
		}
	}
	return texts
}

// doneEvent is the decoded terminal new_message.json.
type doneEvent struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	State    string `json:"state"`
	Reply    string `json:"reply"`
	MD       string `json:"md"`
}

// finals returns every terminal event emitted.
func finals(evs []Event) []doneEvent {
	var out []doneEvent
	for _, e := range evs {
		if e.Type == "new_message.json" {
			var m doneEvent
			_ = json.Unmarshal(e.Data, &m)
			out = append(out, m)
		}
	}
	return out
}

func TestRelaySSE_NormalStream(t *testing.T) {
	evs, err, st := collectSSE(t, answerStream())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF terminal, got %v", err)
	}
	if !st.ended {
		t.Error("state not marked ended after turn.completed + is_final")
	}

	var titles int
	for _, e := range evs {
		if e.Type == "thread.json" {
			var m struct{ ID, Title string }
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatalf("thread.json decode: %v", err)
			}
			if m.ID != "conv1" || m.Title != "My Title" {
				t.Errorf("thread.json = %+v, want id=conv1 title=My Title", m)
			}
			titles++
		}
	}
	if titles != 1 {
		t.Errorf("thread.json emitted %d times, want 1", titles)
	}
	got := tokenTexts(evs)
	want := []string{"Fo", "Four"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("tokens = %q, want %q (cumulative, only on change)", got, want)
	}
	// The terminal event needs the persisted HTML, so relaySSE leaves it to
	// the caller.
	if n := len(finals(evs)); n != 0 {
		t.Errorf("relaySSE emitted %d terminal events, want 0", n)
	}
}

func TestRelaySSE_OnlyRootTextIsTheAnswer(t *testing.T) {
	// Thinking deltas and a sub-agent's text (nested under a tool node) are not
	// part of the reply; two root-level text steps are joined in order.
	raw := ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		ev("", 2, "node.created", "r1", "", `{"kind":"assistant_message"}`) +
		ev("", 3, "node.created", "a1", "r1", `{"kind":"text"}`) +
		ev("", 4, "node.delta", "a1", "r1", `{"delta":"Let me look."}`) +
		ev("", 5, "node.created", "think", "r1", `{"kind":"activity","type":"thinking"}`) +
		ev("", 6, "node.delta", "think", "r1", `{"delta":"pondering"}`) +
		ev("", 7, "node.completed", "think", "r1", `{"content":"pondering"}`) +
		ev("", 8, "node.created", "tool", "r1", `{"kind":"activity","type":"tool_call"}`) +
		ev("", 9, "node.created", "sub", "tool", `{"kind":"text"}`) +
		ev("", 10, "node.delta", "sub", "tool", `{"delta":"sub-agent notes"}`) +
		ev("", 11, "node.completed", "tool", "r1", `{"result":{"type":"tool_return","content":"raw results"}}`) +
		ev("", 12, "node.created", "a2", "r1", `{"kind":"text"}`) +
		ev("", 13, "node.delta", "a2", "r1", `{"delta":"Paris."}`)

	evs, _, st := collectSSE(t, raw)
	if got := st.replyText(); got != "Let me look.\n\nParis." {
		t.Errorf("reply = %q, want both root text steps and nothing else", got)
	}
	for _, text := range tokenTexts(evs) {
		if strings.Contains(text, "pondering") || strings.Contains(text, "sub-agent") || strings.Contains(text, "raw results") {
			t.Errorf("token leaked non-answer text: %q", text)
		}
	}
}

func TestRelaySSE_ReplayIsNotAppliedTwice(t *testing.T) {
	// A reconnect can replay the turn from the start under fresh SSE ids. The
	// deltas must not be appended a second time.
	first := ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		ev("2-0", 2, "node.created", "n1", "r1", `{"kind":"text"}`) +
		ev("3-0", 3, "node.delta", "n1", "r1", `{"delta":"Fo"}`)

	_, err, st := collectSSE(t, first)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("truncated stream should end in io.EOF, got %v", err)
	}
	if st.ended {
		t.Error("state marked ended without the turn ending")
	}
	if st.cursor != "3-0" {
		t.Errorf("cursor = %q, want 3-0", st.cursor)
	}

	replay := ev("90-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		ev("91-0", 2, "node.created", "n1", "r1", `{"kind":"text"}`) +
		ev("92-0", 3, "node.delta", "n1", "r1", `{"delta":"Fo"}`) +
		ev("93-0", 4, "node.delta", "n1", "r1", `{"delta":"ur"}`) +
		ev("94-0", 5, "turn.completed", "", "", `{}`)

	evs, _, st := collectSSEState(t, replay, st)
	if got := st.replyText(); got != "Four" {
		t.Errorf("reply after replay = %q, want Four", got)
	}
	if got := tokenTexts(evs); len(got) != 1 || got[0] != "Four" {
		t.Errorf("tokens after replay = %q, want only the new text", got)
	}
	if !st.ended {
		t.Error("turn.completed on the replay did not end the turn")
	}
}

func TestRelaySSE_OtherTurnsDoNotEndOurs(t *testing.T) {
	// A nested sub-turn finishing is not our turn finishing.
	raw := ev("", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		evOf("", "sub1", 1, "turn.started", "", "", `{"root_node_id":"s1"}`) +
		evOf("", "sub1", 2, "turn.completed", "", "", `{}`)

	_, _, st := collectSSE(t, raw)
	if st.ended {
		t.Error("a sub-turn's completion ended our turn")
	}
	if st.rootNode != "r1" {
		t.Errorf("rootNode = %q, want r1 (not the sub-turn's root)", st.rootNode)
	}
}

func TestRelaySSE_TurnFailedIsUpstreamError(t *testing.T) {
	raw := ev("", 1, "turn.started", "", "", `{"root_node_id":"r1"}`) +
		ev("", 2, "turn.failed", "", "", `{"error_code":"provider_error"}`)

	_, err, _ := collectSSE(t, raw)
	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *upstreamError, got %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "provider_error") {
		t.Errorf("err = %q, want the error code", err)
	}
}

func TestRelaySSE_ErrorFrameIsUpstreamError(t *testing.T) {
	_, err, _ := collectSSE(t, frame("", `{"error":"upstream boom"}`))
	var ue *upstreamError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("expected *upstreamError with 'upstream boom', got %T (%v)", err, err)
	}
}

func TestRelaySSE_StopsAtTurnEnd(t *testing.T) {
	// A connection held open past the end must not keep folding events in.
	raw := answerStream() + ev("10-0", 8, "node.delta", "n1", "r1", `{"delta":" more"}`)

	_, err, st := collectSSE(t, raw)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF right after the end", err)
	}
	if got := st.replyText(); got != "Four" {
		t.Errorf("reply = %q, want Four", got)
	}
}

func TestNewPrompt_BaseVsProfile(t *testing.T) {
	base := NewPrompt("hello", "", "", "", "ki_quick", true)
	if base.Profile.ID != "" || base.Profile.Model != "ki_quick" {
		t.Errorf("base NewPrompt profile = %+v", base.Profile)
	}
	if base.Focus.ThreadID != nil {
		t.Error("new-conversation prompt should have nil ThreadID")
	}

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

// --- startChat --------------------------------------------------------------

// newStreamTestClient points a Client at a local test server.
func newStreamTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-session")
	c.apiBase = srv.URL
	return c
}

func TestStartChat_NewConversationDispatchesInOneCall(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/conversations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"conversation_uuid":"conv9","branch_uuid":"b9","assistant_turn_uuid":"t9","user_turn_uuid":"u9"}`))
	})

	c := newStreamTestClient(t, mux)
	turn, err := c.startChat(context.Background(), NewPrompt("hi", "", "", "", "m1", false))
	if err != nil {
		t.Fatalf("startChat: %v", err)
	}
	if got["content"] != "hi" || got["model"] != "m1" || got["enable_search"] != false {
		t.Errorf("body = %v, want content=hi model=m1 enable_search=false", got)
	}
	if _, ok := got["profile_uuid"]; ok {
		t.Errorf("body = %v, want no profile_uuid for a base model", got)
	}
	want := chatTurn{convUUID: "conv9", branchUUID: "b9", turnUUID: "t9", streamURL: "/api/v2/turns/t9/stream"}
	if turn != want {
		t.Errorf("turn = %+v, want %+v", turn, want)
	}
}

func TestStartChat_FollowUpUsesReportedBranch(t *testing.T) {
	// The server may answer on a different branch than the one we named; the
	// persisted row is then read from that one.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/conv1/init", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversation":{"uuid":"conv1"},"active_branch":{"uuid":"old-branch"},"messages":{"items":[]}}`))
	})
	mux.HandleFunc("POST /api/v2/branches/old-branch/respond", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"assistant_turn_uuid":"t9","branch_uuid":"forked-branch","conversation_uuid":"conv1","user_turn_uuid":"u9"}`))
	})

	c := newStreamTestClient(t, mux)
	turn, err := c.startChat(context.Background(), NewPrompt("hi", "conv1", "", "", "m", false))
	if err != nil {
		t.Fatalf("startChat: %v", err)
	}
	if turn.branchUUID != "forked-branch" || turn.turnUUID != "t9" {
		t.Errorf("turn = %+v, want branch forked-branch, turn t9", turn)
	}
}

func TestStartChat_RequiresModelOrProfile(t *testing.T) {
	c := New("s")
	if _, err := c.startChat(context.Background(), NewPrompt("hi", "", "", "", "", false)); err == nil {
		t.Fatal("expected an error without model or profile")
	}
}

// --- pumpStream -------------------------------------------------------------

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
	for e := range out {
		evs = append(evs, e)
	}
	<-done
	return evs, perr
}

// turnsHandler serves a branch's persisted turns.
func turnsHandler(rows string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"turns":[` + rows + `],"has_more":false}`))
	}
}

const (
	turnsPath    = "/api/v2/branches/b1/turns"
	streamPath   = "/api/v2/turns/t1/stream"
	completedRow = `{"uuid":"t1","role":"assistant","status":"completed","content":"Four","html_content":"<p>Four</p>"}`
)

// onlyFinal returns the single terminal event, failing otherwise.
func onlyFinal(t *testing.T, evs []Event) doneEvent {
	t.Helper()
	f := finals(evs)
	if len(f) != 1 {
		t.Fatalf("terminal events = %d, want 1", len(f))
	}
	return f[0]
}

func TestPumpStream_FinishesWithPersistedHTML(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("cursor") {
			t.Errorf("first connect sent cursor %q, want none", r.URL.Query().Get("cursor"))
		}
		io.WriteString(w, answerStream())
	})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"u1","role":"user","content":"q"},`+completedRow))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	want := doneEvent{ID: "t1", ThreadID: "conv1", State: "done", Reply: "<p>Four</p>", MD: "Four"}
	if got := onlyFinal(t, evs); got != want {
		t.Errorf("final = %+v, want %+v", got, want)
	}
}

func TestPumpStream_FinishKeepsStreamedTextWhenLookupFails(t *testing.T) {
	// The answer arrived on the wire; failing to read the persisted row must
	// cost only the HTML, not the answer.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answerStream())
	})
	mux.HandleFunc(turnsPath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"unavailable","message":"later"}}`, http.StatusServiceUnavailable)
	})

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := onlyFinal(t, evs); got.MD != "Four" || got.Reply != "" {
		t.Errorf("final md/reply = %q/%q, want the streamed text and no html", got.MD, got.Reply)
	}
}

func TestPumpStream_FinishWaitsForPersistedRow(t *testing.T) {
	// The stream ended but the row still says running; the HTML must not be
	// dropped just because persistence trails the wire by a beat.
	var reads int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answerStream())
	})
	mux.HandleFunc(turnsPath, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&reads, 1) == 1 {
			turnsHandler(`{"uuid":"t1","role":"assistant","status":"running","content":"Fo"}`)(w, r)
			return
		}
		turnsHandler(completedRow)(w, r)
	})

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := onlyFinal(t, evs); got.Reply != "<p>Four</p>" {
		t.Errorf("final reply = %q, want the html once the row completed", got.Reply)
	}
}

func TestPumpStream_ReconnectsAfterDrop(t *testing.T) {
	// The first connection is killed mid-turn and the rest of the answer only
	// arrives on the reconnect, which must resume from the last id.
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if atomic.AddInt32(&connects, 1) == 1 {
			io.WriteString(w, ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`)+
				ev("2-0", 2, "node.created", "n1", "r1", `{"kind":"text"}`)+
				ev("3-0", 3, "node.delta", "n1", "r1", `{"delta":"Fo"}`))
			return
		}
		if cursor != "3-0" {
			t.Errorf("reconnect cursor = %q, want 3-0", cursor)
		}
		io.WriteString(w, ev("4-0", 4, "node.delta", "n1", "r1", `{"delta":"ur"}`)+
			ev("5-0", 5, "turn.completed", "", "", `{}`)+
			frame("6-0", `{"is_final":true}`))
	})
	mux.HandleFunc(turnsPath, turnsHandler(completedRow))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}
	if got := onlyFinal(t, evs); got.MD != "Four" {
		t.Errorf("final md = %q, want Four", got.MD)
	}
}

func TestPumpStream_RecoversPersistedAnswer(t *testing.T) {
	// The stream is exhausted and the turn is over, but its end was never
	// seen. The answer landed on the branch anyway, so it must be recovered.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc(turnsPath, turnsHandler(completedRow))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := onlyFinal(t, evs); got.MD != "Four" || got.Reply != "<p>Four</p>" {
		t.Errorf("recovered md/reply = %q/%q, want the persisted turn", got.MD, got.Reply)
	}
}

func TestPumpStream_PersistedFailureIsReported(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"t1","role":"assistant","status":"failed","error_code":"quota","content":""}`))

	_, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v, want the persisted failure with its code", err)
	}
}

func TestPumpStream_UpstreamErrorDoesNotReconnect(t *testing.T) {
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		io.WriteString(w, ev("1-0", 1, "turn.failed", "", "", `{"error_code":"boom"}`))
	})

	_, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the upstream error", err)
	}
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Errorf("connections = %d, want 1 (no retry on an upstream error)", got)
	}
}

func TestPumpStream_IgnoresPreviousTurnsAnswer(t *testing.T) {
	// The turn died with nothing persisted. The branch still holds the
	// *previous* turn's reply, which must not be passed off as this one's.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"t0","role":"assistant","status":"completed","content":"earlier answer"}`))
	mux.HandleFunc("/api/billing/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"can_proceed":true}`))
	})

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err == nil {
		t.Fatal("expected a premature-end error, got nil")
	}
	if f := finals(evs); len(f) != 0 {
		t.Fatalf("emitted a terminal event from a stale turn: %+v", f)
	}
}

func TestPumpStream_RetriesWhenReopenFails(t *testing.T) {
	// A transient failure while reopening the stream must not cost the answer:
	// it counts as a stall, and the next attempt succeeds.
	var connects int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&connects, 1) {
		case 1:
			io.WriteString(w, ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`))
		case 2:
			http.Error(w, `{"error":{"code":"unavailable","message":"try later"}}`, http.StatusServiceUnavailable)
		default:
			io.WriteString(w, answerStream())
		}
	})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"t1","role":"assistant","status":"running","content":""}`))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 3 {
		t.Errorf("connections = %d, want 3 (initial, failed reopen, success)", got)
	}
	if got := onlyFinal(t, evs); got.MD != "Four" {
		t.Errorf("final md = %q, want Four", got.MD)
	}
}

func TestPumpStream_UnblocksOnCancel(t *testing.T) {
	// A consumer that cancels and walks away must not leave the stream
	// goroutine wedged on a full channel.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		io.WriteString(w, ev("", 1, "turn.started", "", "", `{"root_node_id":"r1"}`)+
			ev("", 2, "node.created", "n1", "r1", `{"kind":"text"}`))
		for i := 0; i < 1000; i++ {
			io.WriteString(w, ev("", 3+i, "node.delta", "n1", "r1", `{"delta":"x"}`))
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
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, ev("1-0", 1, "turn.started", "", "", `{"root_node_id":"r1"}`)+
			ev("2-0", 2, "node.delta", "n1", "r1", `{"delta":"partial"}`))
	})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"t1","role":"assistant","status":"completed","content":"partial then the rest"}`))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	texts := tokenTexts(evs)
	if len(texts) == 0 || texts[len(texts)-1] != "partial then the rest" {
		t.Errorf("token texts = %q, want the full recovered answer last", texts)
	}
}

func TestOpenStream_AcceptsNoContent(t *testing.T) {
	// 204 means the turn's stream is gone. That has to read as an empty stream
	// so recovery runs, not as a hard error that drops the answer.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"t1","role":"assistant","status":"completed","content":"late answer","html_content":"<p>late</p>"}`))

	evs, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if got := onlyFinal(t, evs); got.MD != "late answer" {
		t.Errorf("md = %q, want the recovered answer after a 204", got.MD)
	}
}

func TestPrematureEnd_PointsAtTheConversation(t *testing.T) {
	err := prematureEnd("conv1", "", nil)
	if !strings.Contains(err.Error(), "kagi threads show conv1") {
		t.Errorf("err = %q, want a pointer to re-check the conversation", err)
	}
}

func TestPumpStream_NamesTheUsageCap(t *testing.T) {
	// Hitting the usage cap kills a turn mid-generation with no error event
	// and nothing persisted. The failure must say so rather than read as a
	// network problem.
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(turnsPath, turnsHandler(`{"uuid":"u1","role":"user","content":"q"}`))
	mux.HandleFunc("/api/billing/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limit":"hard","reason":"cost","can_proceed":false}`))
	})

	_, err := drain(t, newStreamTestClient(t, mux), testTurn)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage cap") {
		t.Errorf("err = %q, want it to name the usage cap", err)
	}
}
