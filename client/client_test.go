package client

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// collectSSE runs relaySSE over the given raw stream and returns the events it
// emitted plus the terminal error.
func collectSSE(t *testing.T, raw, convUUID string) ([]Event, error) {
	t.Helper()
	out := make(chan Event, 64)
	var err error
	done := make(chan struct{})
	go func() {
		err = relaySSE(strings.NewReader(raw), convUUID, out)
		close(out)
		close(done)
	}()
	var evs []Event
	for ev := range out {
		evs = append(evs, ev)
	}
	<-done
	return evs, err
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
