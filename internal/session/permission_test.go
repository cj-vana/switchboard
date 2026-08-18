package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestResolvedPermissionAuditRoundTrips(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test", "rev")
	if err != nil {
		t.Fatal(err)
	}
	want := Permission{
		Tool: "exec", Mode: "auto", Decision: "allow", Reason: "reviewer allowed",
		Approved: true, ResolvedBy: "model", Reviewer: "t1", ReviewDecision: "allow",
		ReviewReason: "scoped test", ReviewError: "",
	}
	if err := sess.AppendPermission(want); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("permissions = %+v, want %+v", got, want)
	}
}

func TestLegacyPermissionAuditRemainsReadable(t *testing.T) {
	var legacy Permission
	if err := json.Unmarshal([]byte(`{"tool":"exec","mode":"default","decision":"ask","reason":"runs a command"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Approved || legacy.ResolvedBy != "" {
		t.Fatalf("legacy fields invented a resolution: %+v", legacy)
	}

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendPermission(Permission{Tool: "exec", Mode: "default", Decision: "ask", Reason: "runs a command"}); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// New writes carry approved=false. Removing that field recreates the JSON
	// shape emitted by older binaries without rewriting the frame checksum, so
	// use ReadPermissions' JSON compatibility through a direct legacy decode in
	// the record codec tests instead of corrupting the log here.
	if !strings.Contains(string(data), `"approved":false`) {
		t.Fatal("new denial did not make its final approved state explicit")
	}
	got, err := ReadPermissions(path)
	if err != nil || len(got) != 1 || got[0].ResolvedBy != "" || got[0].Approved {
		t.Fatalf("legacy-shaped permission = %+v err=%v", got, err)
	}
}
