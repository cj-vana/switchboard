package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
	}{
		{"text", UserText("hello")},
		{
			"assistant with thinking and tool use",
			Message{Role: RoleAssistant, Content: []Block{
				Thinking{Text: "consider the options", Signature: "sig-1"},
				Text{Text: "reading the file"},
				ToolUse{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
		},
		{
			"tool results share one message",
			Message{Role: RoleTool, Content: []Block{
				ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
				ToolResult{ToolUseID: "call_2", Name: "exec", Content: "exit status 1", IsError: true},
			}},
		},
		{
			"incomplete assistant message",
			Message{Role: RoleAssistant, Incomplete: true, Content: []Block{Text{Text: "partial"}}},
		},
		{
			"binary blocks",
			Message{Role: RoleUser, Content: []Block{
				Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
				Document{MediaType: "application/pdf", Name: "spec.pdf", Data: []byte{0x25, 0x50}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Message
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.msg) {
				t.Errorf("round trip changed the message\n got: %#v\nwant: %#v", got, tc.msg)
			}
		})
	}
}

// A block kind from a newer binary must fail loudly. Dropping it would hand the
// next request a conversation that silently lost a tool call.
func TestUnknownBlockKindIsRejected(t *testing.T) {
	var m Message
	err := json.Unmarshal([]byte(`{"role":"assistant","content":[{"kind":"hologram","data":{}}]}`), &m)
	if err == nil {
		t.Fatal("expected an error for an unknown block kind")
	}
	if !strings.Contains(err.Error(), "hologram") {
		t.Errorf("error should name the unknown kind, got: %v", err)
	}
}

func TestRouteTargetIDIncludesReasoning(t *testing.T) {
	base := RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}
	high := base
	high.Params.Reasoning = &Reasoning{Enabled: true, Effort: "high"}
	low := base
	low.Params.Reasoning = &Reasoning{Enabled: true, Effort: "low"}

	if base.ID() == high.ID() {
		t.Error("enabling reasoning must produce a different target: it changes cache identity")
	}
	if high.ID() == low.ID() {
		t.Error("effort levels must produce different targets")
	}
	if want := RouteTargetID("ollama/local/qwen3.5:9b-mlx"); base.ID() != want {
		t.Errorf("base ID = %q, want %q", base.ID(), want)
	}
}

func TestAPIErrorRetryClassification(t *testing.T) {
	for status, want := range map[int]bool{
		400: false, 401: false, 403: false, 404: false,
		408: true, 429: true, 500: true, 503: true,
	} {
		got := (&APIError{StatusCode: status}).Retryable()
		if got != want {
			t.Errorf("status %d: Retryable() = %v, want %v", status, got, want)
		}
	}
}
