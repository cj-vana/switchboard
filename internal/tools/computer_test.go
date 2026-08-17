package tools

// The offline half of the computer tool's verification: every wire shape
// here was captured from a live osascript run (computer_live_test.go is
// the capturer), so what these tests parse is what the scripts actually
// say, not what they were expected to say.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/permission"
)

// fakeComputer builds the tool over canned script output: each key is a
// script constant, each value the one JSON line osascript would print.
func fakeComputer(t *testing.T, outputs map[string]string) *computerTool {
	t.Helper()
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	tool.runScript = func(_ context.Context, script string, _ []string, _ time.Duration) (string, error) {
		out, ok := outputs[script]
		if !ok {
			return "", errors.New("no canned output for this script")
		}
		return out, nil
	}
	tool.launch = func(context.Context, string) error {
		return errors.New("nothing should launch in this test")
	}
	return tool
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runComputer(t *testing.T, tool *computerTool, input string) Result {
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

func TestComputerStateFormatsTheCapturedWalk(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_state.json"),
	})
	res := runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	if res.IsError {
		t.Fatalf("state errored: %s", res.Content)
	}
	for _, want := range []string{
		`TextEdit — frontmost: yes; windows: "Untitled"`,
		"menus: Apple, TextEdit, File",
		`[0] AXColorWell (text color) = "rgb 1 1 1 1" at 404,142`,
		"low-signal elements hidden",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("state output missing %q:\n%s", want, res.Content)
		}
	}

	// The walk armed the element cache: a click plan may now name an index,
	// and carries the recorded role into its request detail.
	pln, err := tool.Plan(json.RawMessage(`{"action":"click","app":"TextEdit","element":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pln.Request.Detail, "AXColorWell") {
		t.Errorf("click detail does not name the element: %q", pln.Request.Detail)
	}
}

func TestComputerAppsListsWhatRuns(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerAppsScript: fixture(t, "computer_apps.json"),
	})
	res := runComputer(t, tool, `{"action":"apps"}`)
	if res.IsError {
		t.Fatalf("apps errored: %s", res.Content)
	}
	for _, want := range []string{
		"Finder — 0 window(s)",
		"Safari — 1 window(s), frontmost",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("apps output missing %q:\n%s", want, res.Content)
		}
	}
}

func TestComputerActionsCarryTheExternalEffect(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_state.json"),
	})
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`) // arms element 0

	for _, input := range []string{
		`{"action":"apps"}`,
		`{"action":"state","app":"TextEdit"}`,
		`{"action":"click","app":"TextEdit","element":0}`,
		`{"action":"click","app":"TextEdit","x":10,"y":20}`,
		`{"action":"type","app":"TextEdit","text":"hi"}`,
		`{"action":"key","app":"TextEdit","key":"cmd+s"}`,
		`{"action":"set","app":"TextEdit","element":0,"text":"v"}`,
		`{"action":"menu","app":"TextEdit","menu":"File > Save"}`,
	} {
		pln, err := tool.Plan(json.RawMessage(input))
		if err != nil {
			t.Fatalf("plan %s: %v", input, err)
		}
		if pln.Request.Effect != permission.EffectExternal {
			t.Errorf("%s carries %s, want external", input, pln.Request.Effect)
		}
		if pln.Request.Detail == "" {
			t.Errorf("%s has no display detail", input)
		}
		if strings.Contains(input, "TextEdit") && pln.Request.Path != "TextEdit" {
			t.Errorf("%s puts %q in Path; the app is the approval's grain", input, pln.Request.Path)
		}
	}
}

func TestComputerClickBeforeStateNamesTheFix(t *testing.T) {
	tool := fakeComputer(t, nil)
	_, err := tool.Plan(json.RawMessage(`{"action":"click","app":"Safari","element":3}`))
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("want an error naming state, got %v", err)
	}
}

func TestComputerStaleElementSaysStateAgain(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_state.json"),
		computerClickScript: `{"running":true,"stale":true,"role":"AXGroup"}`,
	})
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	res := runComputer(t, tool, `{"action":"click","app":"TextEdit","element":0}`)
	if !res.IsError || !strings.Contains(res.Content, "state again") {
		t.Fatalf("a stale path must say state again, got: %s", res.Content)
	}
}

func TestComputerRedactsWhatItReadsBack(t *testing.T) {
	// Another app's UI can hold anything — this fixture plants a key-shaped
	// value the way a password manager or a visible .env would.
	token := "sk-ant-" + strings.Repeat("a", 24)
	state := fmt.Sprintf(`{"running":true,"frontmost":true,"windows":["w"],"menus":[],`+
		`"els":[{"path":[0],"r":"AXTextArea","t":"","d":"text entry area","v":%q,"p":[0,0],"s":[10,10]}],`+
		`"walked":1,"timedOut":false}`, token)
	tool := fakeComputer(t, map[string]string{computerStateScript: state})
	res := runComputer(t, tool, `{"action":"state","app":"Vault"}`)
	if strings.Contains(res.Content, token) {
		t.Fatal("a key read off another app's UI reached the result")
	}
	if !strings.Contains(res.Content, "redacted") {
		t.Errorf("the redaction should name what it held back:\n%s", res.Content)
	}
}

func TestComputerRefusesToTypeASecret(t *testing.T) {
	token := "sk-ant-" + strings.Repeat("b", 24)
	tool := fakeComputer(t, nil)
	for _, input := range []string{
		fmt.Sprintf(`{"action":"type","app":"Safari","text":%q}`, token),
		fmt.Sprintf(`{"action":"set","app":"Safari","element":0,"text":%q}`, token),
	} {
		_, err := tool.Plan(json.RawMessage(input))
		if err == nil {
			t.Fatalf("%s should refuse a key-shaped string", input)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatal("the refusal quoted the key it exists to hold back")
		}
	}
}

func TestComputerKeySpecs(t *testing.T) {
	cases := []struct {
		in      string
		char    string
		code    int
		mods    []string
		display string
	}{
		{"return", "", 36, nil, "return"},
		{"cmd+s", "s", 0, []string{"command down"}, "cmd+s"},
		{"shift+tab", "", 48, []string{"shift down"}, "shift+tab"},
		{"cmd+shift+t", "t", 0, []string{"command down", "shift down"}, "cmd+shift+t"},
		{"Escape", "", 53, nil, "escape"},
	}
	for _, c := range cases {
		spec, display, err := parseKeySpec(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		var got struct {
			Char string   `json:"char"`
			Code *int     `json:"code"`
			Mods []string `json:"mods"`
		}
		if err := json.Unmarshal([]byte(spec), &got); err != nil {
			t.Errorf("%s: bad spec %s", c.in, spec)
			continue
		}
		if got.Char != c.char {
			t.Errorf("%s: char %q, want %q", c.in, got.Char, c.char)
		}
		if c.char == "" && (got.Code == nil || *got.Code != c.code) {
			t.Errorf("%s: code %v, want %d", c.in, got.Code, c.code)
		}
		if len(got.Mods) != len(c.mods) {
			t.Errorf("%s: mods %v, want %v", c.in, got.Mods, c.mods)
		}
		if display != c.display {
			t.Errorf("%s: display %q, want %q", c.in, display, c.display)
		}
	}

	if _, _, err := parseKeySpec("superkey"); err == nil || !strings.Contains(err.Error(), "return") {
		t.Errorf("an unknown key should list what would have worked, got %v", err)
	}
	if _, _, err := parseKeySpec("hyper+s"); err == nil || !strings.Contains(err.Error(), "cmd") {
		t.Errorf("an unknown modifier should list the real ones, got %v", err)
	}
}

func TestComputerLaunchesWhatIsNotRunning(t *testing.T) {
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	var mu sync.Mutex
	launched := 0
	stateCalls := 0
	tool.launch = func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		launched++
		return nil
	}
	running := fixture(t, "computer_state.json")
	tool.runScript = func(_ context.Context, script string, _ []string, _ time.Duration) (string, error) {
		if script != computerStateScript {
			return "", errors.New("only state should run here")
		}
		mu.Lock()
		defer mu.Unlock()
		stateCalls++
		if stateCalls == 1 {
			return `{"running":false}`, nil
		}
		return running, nil
	}
	res := runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	if res.IsError {
		t.Fatalf("state errored: %s", res.Content)
	}
	if launched != 1 {
		t.Errorf("launched %d times, want 1", launched)
	}
	if !strings.Contains(res.Content, "[0]") {
		t.Errorf("the post-launch walk went missing:\n%s", res.Content)
	}
}

func TestComputerMenuNeedsAPath(t *testing.T) {
	tool := fakeComputer(t, nil)
	for _, input := range []string{
		`{"action":"menu","app":"TextEdit","menu":"Save"}`,
		`{"action":"menu","app":"TextEdit","menu":""}`,
	} {
		if _, err := tool.Plan(json.RawMessage(input)); err == nil || !strings.Contains(err.Error(), "File > Save") {
			t.Errorf("%s should teach the path shape, got %v", input, err)
		}
	}
}
