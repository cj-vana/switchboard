package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
	"github.com/switchboard-code/switchboard/internal/mcppolicy"
)

func TestNativeMCPPolicyNotesAreRedactedAndActionable(t *testing.T) {
	notes := nativeMCPPolicyNotes([]mcppolicy.Diagnostic{{
		Severity: mcppolicy.SeverityError,
		Dialect:  mcpnative.DialectClaude,
		Code:     "invalid-policy",
		Path:     "/managed/settings.json",
		Field:    "allowedMcpServers",
		Message:  "managed policy could not be parsed",
	}})
	if len(notes) != 1 || notes[0].level != "error" {
		t.Fatalf("notes = %#v", notes)
	}
	for _, want := range []string{"claude", "invalid-policy", "allowedMcpServers", "/managed/settings.json"} {
		if !strings.Contains(notes[0].text, want) {
			t.Fatalf("note %q does not contain %q", notes[0].text, want)
		}
	}
}
