package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjvana/switchboard/internal/provider"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return store, workspace
}

func TestReplayReconstructsState(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()

	messages := []provider.Message{
		provider.UserText("add a test"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Thinking{Text: "look at the file first"},
			provider.ToolUse{ID: "call_1", Name: "read", Input: []byte(`{"path":"main.go"}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
		}},
	}
	for _, m := range messages {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.AppendUsage(Usage{
		Target:   "ollama/local/qwen3.5:9b-mlx",
		Usage:    provider.Usage{InputTokens: 283, OutputTokens: 57},
		Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got := reopened.State()
	if len(got.Messages) != len(messages) {
		t.Fatalf("replayed %d messages, want %d", len(got.Messages), len(messages))
	}
	if got.Messages[1].ToolUses()[0].Name != "read" {
		t.Errorf("tool call did not survive replay: %+v", got.Messages[1])
	}
	if got.Usage.InputTokens != 283 || got.Usage.OutputTokens != 57 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Calls != 1 {
		t.Errorf("calls = %d, want 1", got.Calls)
	}
	if got.Target != "ollama/local/qwen3.5:9b-mlx" {
		t.Errorf("target = %q", got.Target)
	}
	if got.Workspace != workspace {
		t.Errorf("workspace = %q, want %q", got.Workspace, workspace)
	}
	if reopened.TruncatedBytes() != 0 {
		t.Errorf("clean log reported %d truncated bytes", reopened.TruncatedBytes())
	}
}

// A process killed mid-write leaves a frame with no terminator. Replay must
// recover everything before it rather than refusing to load the session.
func TestTornFinalWriteRecovers(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	for _, text := range []string{"first", "second", "third"} {
		if err := sess.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the kill: chop the last record in half.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastNewline := strings.LastIndexByte(string(data[:len(data)-1]), '\n')
	torn := append([]byte{}, data[:lastNewline+1]...)
	torn = append(torn, data[lastNewline+1:len(data)-10]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("a torn tail must not make the session unloadable: %v", err)
	}
	defer reopened.Close()

	got := reopened.State()
	if len(got.Messages) != 2 {
		t.Fatalf("recovered %d messages, want the 2 that were fully written", len(got.Messages))
	}
	if got.Messages[1].Text() != "second" {
		t.Errorf("last recovered message = %q, want second", got.Messages[1].Text())
	}
	if reopened.TruncatedBytes() == 0 {
		t.Error("lost bytes must be reported, not swallowed")
	}

	// The truncation is durable, so appending after recovery produces a log that
	// replays cleanly the next time.
	if err := reopened.AppendMessage(provider.UserText("fourth")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.TruncatedBytes() != 0 {
		t.Error("reopening a repaired log truncated again")
	}
	if n := len(again.State().Messages); n != 3 {
		t.Errorf("got %d messages after appending post-recovery, want 3", n)
	}
}

func TestAlteredPayloadIsDetected(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	for _, text := range []string{"keep", "tamper"} {
		if err := sess.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	sess.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the final record's payload, leaving the framing intact.
	// Only the checksum can catch this.
	tampered := strings.Replace(string(data), "tamper", "TAMPER", 1)
	if tampered == string(data) {
		t.Fatal("test fixture did not contain the expected payload")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if n := len(reopened.State().Messages); n != 1 {
		t.Errorf("got %d messages, want only the 1 record whose checksum verified", n)
	}
}

func TestSchemaFromNewerBinaryIsRefused(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	sess.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(data), magic+" 1", magic+" 99", 1)
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = store.Open(id)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew; a best-effort parse would drop records silently", err)
	}
}

func TestSecondWriterIsRefused(t *testing.T) {
	store, workspace := newStore(t)

	first, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	_, err = store.Open(first.ID())
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("err = %v, want ErrSessionLocked; interleaved frames would corrupt the log", err)
	}

	first.Close()
	reopened, err := store.Open(first.ID())
	if err != nil {
		t.Fatalf("the lock must release on close: %v", err)
	}
	reopened.Close()
}

func TestLatestAndListOrdering(t *testing.T) {
	store, workspace := newStore(t)

	var ids []string
	for range 3 {
		s, err := store.Create(workspace, "t")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(provider.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID())
		s.Close()
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("listed %d sessions, want 3", len(infos))
	}
	if infos[0].ID != ids[2] {
		t.Errorf("most recent = %s, want %s", infos[0].ID, ids[2])
	}

	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.ID() != ids[2] {
		t.Errorf("Latest = %s, want %s", latest.ID(), ids[2])
	}
}

func TestLatestWithNoSessions(t *testing.T) {
	store, workspace := newStore(t)
	if _, err := store.Latest(workspace); !errors.Is(err, ErrNoSessions) {
		t.Errorf("err = %v, want ErrNoSessions", err)
	}
}

func TestIncompleteMessageSurvivesReplay(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	partial := provider.Message{
		Role:       provider.RoleAssistant,
		Incomplete: true,
		Content:    []provider.Block{provider.Text{Text: "cut off mid"}},
	}
	if err := sess.AppendMessage(partial); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got := reopened.State().Messages
	if len(got) != 1 || !got[0].Incomplete {
		t.Fatalf("incomplete flag did not survive replay: %+v", got)
	}
}

func TestSessionsAreNotWorldReadable(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	fi, err := os.Stat(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("session mode = %o; logs hold prompts and code and must stay owner-only", perm)
	}
}
