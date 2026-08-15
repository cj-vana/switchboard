package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/config"
)

func TestShouldAutoCompactTriggersAtTheThreshold(t *testing.T) {
	m := testModel(t)
	m.app.config.CompactAuto = true
	m.app.config.CompactAtPercent = 85
	m.ctxWindow = 100_000

	cases := []struct {
		callTokens int
		want       bool
		why        string
	}{
		{84_000, false, "below the threshold"},
		{85_000, true, "at the threshold"},
		{99_000, true, "nearly full"},
		{0, false, "no usage observed yet"},
	}
	for _, c := range cases {
		m.callTokens = c.callTokens
		if got := m.shouldAutoCompact(); got != c.want {
			t.Errorf("%s (%d of %d): got %v, want %v", c.why, c.callTokens, m.ctxWindow, got, c.want)
		}
	}

	m.callTokens = 99_000
	m.app.config.CompactAuto = false
	if m.shouldAutoCompact() {
		t.Error("auto off must mean off, however full the window")
	}

	m.app.config.CompactAuto = true
	m.ctxWindow = 0
	if m.shouldAutoCompact() {
		t.Error("a target with no known window has no threshold to cross")
	}
}

func TestCompactSettingsPersist(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.config.CompactAuto = true

	if cmd, handled := compactSettings(m, "auto off"); !handled {
		t.Fatal("auto off was not handled as a setting")
	} else if n := cmd().(noticeMsg); n.level == "error" {
		t.Fatalf("auto off failed: %s", n.text)
	}
	if cmd, handled := compactSettings(m, "at 70"); !handled {
		t.Fatal("at 70 was not handled as a setting")
	} else if n := cmd().(noticeMsg); n.level == "error" {
		t.Fatalf("at 70 failed: %s", n.text)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CompactAuto {
		t.Error("auto off did not survive the rewrite")
	}
	if saved.CompactAtPercent != 70 {
		t.Errorf("threshold %d did not survive, want 70", saved.CompactAtPercent)
	}

	if cmd, handled := compactSettings(m, "at 30"); !handled || !strings.Contains(cmd().(noticeMsg).text, "usage") {
		t.Error("a threshold outside 50–95 must be refused with usage")
	}

	// Guidance is not a setting: it flows through to the summarizer.
	if _, handled := compactSettings(m, "focus on the migration"); handled {
		t.Error("guidance text was swallowed by the settings parser")
	}
}
