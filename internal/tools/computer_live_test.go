package tools

// The live half of the computer tool's verification: the offline tests run
// against captured wire fixtures, and this is where the captures come from
// — the SB_FRAMES posture, applied to accessibility instead of the TUI.
// SB_LIVE=1 on macOS drives the real tool end to end against a scratch
// TextEdit document: launch, menu, state, type, and the close-discard-quit
// key sequence, leaving the machine as it was found.
// SB_COMPUTER_CAPTURE=<dir> additionally writes the raw script output the
// offline tests parse, so the fixtures can never drift from what osascript
// actually says.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func liveComputer(t *testing.T) *computerTool {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("SB_LIVE not set")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("computer use is macOS-only")
	}
	binary, err := exec.LookPath("osascript")
	if err != nil {
		t.Skip("no osascript")
	}
	if err := ProbeComputer(context.Background(), binary); err != nil {
		t.Skipf("accessibility unavailable: %v", err)
	}
	return NewComputer(binary).(*computerTool)
}

func computerCall(t *testing.T, tool *computerTool, input string) Result {
	t.Helper()
	pln, err := tool.Plan(json.RawMessage(input))
	if err != nil {
		t.Fatalf("plan %s: %v", input, err)
	}
	res, err := pln.Run(context.Background())
	if err != nil {
		t.Fatalf("run %s: %v", input, err)
	}
	return res
}

func TestComputerDrivesTextEditLive(t *testing.T) {
	tool := liveComputer(t)
	ctx := context.Background()

	if capture := os.Getenv("SB_COMPUTER_CAPTURE"); capture != "" {
		raw, err := tool.runScript(ctx, computerAppsScript, nil, computerStateLimit)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(capture, "computer_apps.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := computerCall(t, tool, `{"action":"state","app":"TextEdit"}`)
	if res.IsError {
		t.Fatalf("state: %s", res.Content)
	}
	if strings.Contains(res.Content, "windows: none") {
		res = computerCall(t, tool, `{"action":"menu","app":"TextEdit","menu":"File > New"}`)
		if res.IsError {
			t.Fatalf("File > New: %s", res.Content)
		}
		res = computerCall(t, tool, `{"action":"state","app":"TextEdit"}`)
	}
	if !strings.Contains(res.Content, "[0]") {
		t.Fatalf("state found no elements:\n%s", res.Content)
	}

	if capture := os.Getenv("SB_COMPUTER_CAPTURE"); capture != "" {
		wire, err := tool.stateWire(ctx, "TextEdit")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(wire)
		if err := os.WriteFile(filepath.Join(capture, "computer_state.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The scratch document is discarded whatever the test decides: cmd+w
	// raises the save sheet, cmd+delete is Don't Save, cmd+q ends it —
	// the key sequence verified by hand before the schema froze.
	defer func() {
		computerCall(t, tool, `{"action":"key","app":"TextEdit","key":"cmd+w"}`)
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			res := computerCall(t, tool, `{"action":"state","app":"TextEdit"}`)
			if strings.Contains(res.Content, "windows: none") {
				break
			}
			if strings.Contains(res.Content, "AXSheet") {
				computerCall(t, tool, `{"action":"key","app":"TextEdit","key":"cmd+delete"}`)
			}
			time.Sleep(400 * time.Millisecond)
		}
		res := computerCall(t, tool, `{"action":"state","app":"TextEdit"}`)
		if !strings.Contains(res.Content, "windows: none") {
			t.Errorf("the scratch window did not close:\n%s", res.Content)
			return
		}
		computerCall(t, tool, `{"action":"key","app":"TextEdit","key":"cmd+q"}`)
	}()

	// TextEdit's auto-capitalization rewrites what lands, so the check is
	// case-blind: the keystrokes arriving at all is what is under test.
	marker := fmt.Sprintf("sb live %d", time.Now().Unix())
	res = computerCall(t, tool, `{"action":"type","app":"TextEdit","text":"`+marker+`"}`)
	if res.IsError {
		t.Fatalf("type: %s", res.Content)
	}
	res = computerCall(t, tool, `{"action":"state","app":"TextEdit"}`)
	if !strings.Contains(strings.ToLower(res.Content), marker) {
		t.Fatalf("the typed text did not land:\n%s", res.Content)
	}
}
