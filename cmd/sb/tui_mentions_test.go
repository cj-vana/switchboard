package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestMentionTokenFindsOnlyTheActiveToken(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"look at @cmd/sb", "cmd/sb"},
		{"@a", "a"},
		{"@", ""},                         // nothing typed yet
		{"mail me user@example.com ", ""}, // cursor past a space
		{"plain text", ""},
		{"two @first then @sec", "sec"},
	}
	for _, c := range cases {
		if got := mentionToken(c.input); got != c.want {
			t.Errorf("mentionToken(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestExpandMentionsAttachesOnlyRealFiles(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("the contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("summarize @notes.txt and email bob@example.com")
	if !strings.Contains(got, "the contents") {
		t.Fatalf("mentioned file was not attached:\n%s", got)
	}
	if !strings.Contains(got, "summarize @notes.txt") {
		t.Fatal("the prompt text itself was altered")
	}
	if strings.Contains(got, "Contents of example.com") || strings.Count(got, "Contents of") != 1 {
		t.Fatalf("something that is not a file was attached:\n%s", got)
	}
	if len(images) != 0 {
		t.Fatalf("a text mention produced image blocks: %d", len(images))
	}

	if got, _ := m.expandMentions("no mentions here"); got != "no mentions here" {
		t.Fatalf("a mention-free prompt should pass through untouched, got %q", got)
	}
}

func TestMentionedImageAttachesAsABlock(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	// The bytes only have to be bytes: the block carries them as they are,
	// and no image parser runs on this side of the wire.
	if err := os.WriteFile(filepath.Join(ws, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("what is wrong in @shot.png here")
	if len(images) != 1 || images[0].MediaType != "image/png" || len(images[0].Data) != 7 {
		t.Fatalf("image did not attach as a block: %+v", images)
	}
	if !strings.Contains(got, "Image shot.png (mentioned above) is attached.") {
		t.Fatalf("the prompt must tie the attachment to the mention:\n%s", got)
	}
	if strings.Contains(got, "Contents of shot.png") {
		t.Fatalf("an image must not also attach as text:\n%s", got)
	}
}

func TestOversizedImageIsRefusedWithItsReason(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	big := make([]byte, mentionImageCap+1)
	if err := os.WriteFile(filepath.Join(ws, "huge.png"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("look at @huge.png please")
	if len(images) != 0 {
		t.Fatal("an oversized image must not attach")
	}
	if !strings.Contains(got, "was not attached") {
		t.Fatalf("the refusal must be said in the prompt, not silent:\n%s", got)
	}
}

func TestShellContextDrainsIntoThePrompt(t *testing.T) {
	m := testModel(t)
	m.onShellDone(shellDoneMsg{command: "git status", output: "clean tree"})

	got := m.shellContext("what changed?")
	for _, want := range []string{"$ git status", "clean tree", "what changed?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, got)
		}
	}
	if again := m.shellContext("next"); again != "next" {
		t.Fatal("shell context should drain once, not repeat on every turn")
	}
}

func TestTierOverrideUsesExpandedPromptAndImageBlocks(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"vision": true})
	tier := ollamaTier("t1", "vision")
	m.app.config.Tiers = []config.Tier{tier}
	m.app.tier = tier
	m.app.providers = newProviders(server.URL, m.app.config)
	m.onShellDone(shellDoneMsg{command: "git status", output: "clean tree"})

	cmd := m.startTurn("inspect @shot.png", "t1")
	if cmd == nil || !m.turnPlanning {
		t.Fatal("override did not enter planning")
	}
	msg := cmd().(overrideProbeMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(msg.images) != 1 || !strings.Contains(msg.prompt, "Image shot.png") ||
		!strings.Contains(msg.prompt, "$ git status") || !strings.Contains(msg.prompt, "clean tree") {
		t.Fatalf("override opening lost ordinary prompt construction: prompt=%q images=%d", msg.prompt, len(msg.images))
	}
	m.finishPlanning()
}

func TestOffLadderVisionCheckDoesNotBlockValidOverride(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"vision": true})
	tier := ollamaTier("t2", "vision")
	m.app.config.Tiers = []config.Tier{tier}
	m.app.providers = newProviders(server.URL, m.app.config)
	m.app.tier = config.Tier{ID: "-resumed", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "text-only",
	}}

	cmd := m.startTurn("inspect @shot.png", "t2")
	if cmd == nil {
		t.Fatal("valid override was refused against the unrelated current target")
	}
	msg := cmd().(overrideProbeMsg)
	if msg.err != nil || len(msg.images) != 1 {
		t.Fatalf("override result: err=%v images=%d", msg.err, len(msg.images))
	}
	m.finishPlanning()
}

func TestOffLadderImageIsRefusedEvenWhenLadderExists(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	m.app.config.Tiers = []config.Tier{ollamaTier("t1", "vision")}
	server := capabilityOllama(t, map[string]bool{"text-only": false})
	m.app.providers = newProviders(server.URL, m.app.config)
	m.app.tier = config.Tier{ID: "-resumed", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "text-only",
	}}

	cmd := m.startTurn("inspect @shot.png", "")
	if cmd == nil || !m.turnPlanning {
		t.Fatal("off-ladder turn did not enter owned asynchronous planning")
	}
	msg := cmd().(turnPlanMsg)
	if msg.err == nil || !strings.Contains(strings.ToLower(msg.err.Error()), "image") {
		t.Fatalf("off-ladder non-vision target was not refused before launch: %v", msg.err)
	}
	m.finishPlanning()
}

func TestOffLadderParameterizedTargetRefreshesLiveVisionEvidence(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"unlisted-vision": true})
	m.app.providers = newProviders(server.URL, m.app.config)
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "unlisted-vision"}
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	m.app.tier = config.Tier{ID: "-model", Target: target}

	cmd := m.startTurn("inspect @shot.png", "")
	if cmd == nil {
		t.Fatal("off-ladder image turn did not plan")
	}
	msg := cmd().(turnPlanMsg)
	if msg.err != nil || len(msg.images) != 1 {
		t.Fatalf("fresh positive live vision evidence was not honored: err=%v images=%d", msg.err, len(msg.images))
	}
	if vision, known := m.app.providers.probedVision(target); !known || !vision {
		t.Fatalf("parameterized target probe evidence = vision %v known %v", vision, known)
	}
	m.finishPlanning()
}
