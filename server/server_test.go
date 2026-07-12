package server

import "testing"

func boolPtr(b bool) *bool { return &b }

// buildPrompt threads an explicit per-request personalization override, while
// defaulting to on (Kagi's own default) when the field is omitted.
func TestBuildPrompt_PersonalizationOverride(t *testing.T) {
	s := &Server{}

	cases := []struct {
		name string
		req  chatRequest
		want bool
	}{
		{"omitted defaults on", chatRequest{Prompt: "hi", Model: "m"}, true},
		{"explicit false disables", chatRequest{Prompt: "hi", Model: "m", Personalization: boolPtr(false)}, false},
		{"explicit true enables", chatRequest{Prompt: "hi", Model: "m", Personalization: boolPtr(true)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := s.buildPrompt(tc.req)
			if err != nil {
				t.Fatalf("buildPrompt: %v", err)
			}
			if pr.Profile.Personalizations != tc.want {
				t.Errorf("Personalizations = %v, want %v", pr.Profile.Personalizations, tc.want)
			}
		})
	}
}

// The internet toggle stays independent of the personalization change.
func TestBuildPrompt_InternetStillIndependent(t *testing.T) {
	s := &Server{}
	pr, err := s.buildPrompt(chatRequest{Prompt: "hi", Model: "m", InternetAccess: boolPtr(false), Personalization: boolPtr(false)})
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if pr.Profile.InternetAccess {
		t.Error("InternetAccess should be false")
	}
	if pr.Profile.Personalizations {
		t.Error("Personalizations should be false")
	}
}
