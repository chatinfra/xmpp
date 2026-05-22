package opencode

import (
	"strings"
	"testing"
)

func TestResponseStateExcludesReasoningParts(t *testing.T) {
	response := observeResponse(t, "ses-1", []string{
		`{"type":"message.part.updated","properties":{"sessionID":"ses-1","part":{"id":"reason-1","sessionID":"ses-1","messageID":"msg-1","type":"reasoning","text":"internal plan "}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses-1","part":{"id":"text-1","sessionID":"ses-1","messageID":"msg-1","type":"text"}}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"reason-1","field":"text","delta":"hidden chain "}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"text-1","field":"text","delta":"Visible "}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"reason-1","field":"text","delta":"secret"}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"text-1","field":"text","delta":"answer"}}`,
		`{"type":"message.updated","properties":{"sessionID":"ses-1","info":{"id":"msg-1","sessionID":"ses-1","role":"assistant","time":{"created":1,"completed":2}}}}`,
	})

	if response.Text != "Visible answer" {
		t.Fatalf("Text=%q, want %q", response.Text, "Visible answer")
	}
	for _, forbidden := range []string{"internal plan", "hidden chain", "secret"} {
		if strings.Contains(response.Text, forbidden) {
			t.Fatalf("Text=%q contains reasoning content %q", response.Text, forbidden)
		}
	}
}

func TestResponseStateExcludesReasoningDeltasBeforePartUpdate(t *testing.T) {
	response := observeResponse(t, "ses-1", []string{
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"reason-early","field":"text","delta":"early hidden "}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses-1","part":{"id":"text-1","sessionID":"ses-1","messageID":"msg-1","type":"text"}}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"text-1","field":"text","delta":"Final "}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"reason-early","field":"text","delta":"still hidden"}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses-1","part":{"id":"reason-early","sessionID":"ses-1","messageID":"msg-1","type":"reasoning","text":"early hidden still hidden"}}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses-1","messageID":"msg-1","partID":"text-1","field":"text","delta":"answer"}}`,
		`{"type":"message.updated","properties":{"sessionID":"ses-1","info":{"id":"msg-1","sessionID":"ses-1","role":"assistant","time":{"created":1,"completed":2}}}}`,
	})

	if response.Text != "Final answer" {
		t.Fatalf("Text=%q, want %q", response.Text, "Final answer")
	}
	for _, forbidden := range []string{"early hidden", "still hidden"} {
		if strings.Contains(response.Text, forbidden) {
			t.Fatalf("Text=%q contains reasoning content %q", response.Text, forbidden)
		}
	}
}

func observeResponse(t *testing.T, sessionID string, events []string) AssistantResponse {
	t.Helper()
	state := newResponseState(sessionID)
	var response AssistantResponse
	for index, event := range events {
		got, done, err := state.Observe([]byte(event))
		if err != nil {
			t.Fatalf("Observe event %d: %v", index, err)
		}
		if done {
			response = got
			if index != len(events)-1 {
				t.Fatalf("Observe event %d completed before final event", index)
			}
		}
	}
	if response.Text == "" {
		t.Fatal("response did not complete")
	}
	return response
}
