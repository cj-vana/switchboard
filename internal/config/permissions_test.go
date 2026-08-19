package config

import (
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
)

// Order decides which non-deny rule answers, so the list has to survive as a
// list rather than as a set.
func TestPermissionRulesLoadInFileOrder(t *testing.T) {
	path := write(t, `
[[permissions]]
decision = "allow"
tool = "exec"
argv_prefix = ["go", "test"]

[[permissions]]
decision = "deny"
tool = "exec"
argv_prefix = ["rm"]

[[permissions]]
decision = "ask"
tool = "write"
path = "/etc/*"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Permissions) != 3 {
		t.Fatalf("Permissions = %+v, want three rules", c.Permissions)
	}
	if c.Permissions[0].Decision != permission.Allow || c.Permissions[0].ArgvPrefix[1] != "test" {
		t.Errorf("first rule = %+v", c.Permissions[0])
	}
	if c.Permissions[1].Decision != permission.Deny {
		t.Errorf("second rule = %+v, want the deny", c.Permissions[1])
	}
	if c.Permissions[2].PathGlob != "/etc/*" {
		t.Errorf("third rule = %+v, want the path glob", c.Permissions[2])
	}
}

// An allow this wide is a permission mode, and a mode is typed on purpose,
// shown in the status bar, and revocable by typing another one. A line in a
// config file is none of those.
func TestAnAllowAsWideAsAModeIsRefused(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"nothing named", "[[permissions]]\ndecision = \"allow\"\n", "yolo"},
		{"every command", "[[permissions]]\ndecision = \"allow\"\neffect = \"execute\"\n", "every one of them"},
		{"every write", "[[permissions]]\ndecision = \"allow\"\neffect = \"write\"\n", "every one of them"},
		{"every external tool", "[[permissions]]\ndecision = \"allow\"\neffect = \"external\"\n", "name the tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFile(write(t, test.body))
			if err == nil {
				t.Fatal("a mode-wide allow loaded")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to name %q", err, test.want)
			}
		})
	}
}

// Tightening is always allowed to be broad: a deny that names nothing is a
// user turning the tool off, which needs no ceremony.
func TestABroadDenyIsAllowed(t *testing.T) {
	c, err := LoadFile(write(t, "[[permissions]]\ndecision = \"deny\"\neffect = \"execute\"\n"))
	if err != nil {
		t.Fatalf("a broad deny was refused: %v", err)
	}
	if len(c.Permissions) != 1 || c.Permissions[0].Decision != permission.Deny {
		t.Errorf("Permissions = %+v", c.Permissions)
	}
}

// A word this program does not know is a typo, and a typo in a permission file
// must not load as "matches nothing".
func TestUnknownDecisionAndEffectFailClosed(t *testing.T) {
	if _, err := LoadFile(write(t, "[[permissions]]\ndecision = \"maybe\"\ntool = \"exec\"\n")); err == nil {
		t.Error("an unknown decision loaded")
	}
	if _, err := LoadFile(write(t, "[[permissions]]\ndecision = \"allow\"\ntool = \"exec\"\neffect = \"sideways\"\n")); err == nil {
		t.Error("an unknown effect loaded")
	}
	if _, err := LoadFile(write(t, "[[permissions]]\ntool = \"exec\"\n")); err == nil {
		t.Error("a rule with no decision loaded")
	}
}

// The error has to name which block, because a permission file is read by a
// person counting blocks and not indexing an array.
func TestARejectedRuleNamesItsPosition(t *testing.T) {
	_, err := LoadFile(write(t, `
[[permissions]]
decision = "allow"
tool = "exec"

[[permissions]]
decision = "nonsense"
`))
	if err == nil {
		t.Fatal("the bad rule loaded")
	}
	if !strings.Contains(err.Error(), "permission rule 2") {
		t.Errorf("error = %q, which does not say which rule", err)
	}
}

// A config this program wrote has to load back into the rules it was written
// from, or Save quietly drops what a user set.
func TestPermissionRulesSurviveASaveAndLoad(t *testing.T) {
	shellOnly := true
	path := write(t, "[tiers.t1]\nmodel = \"ollama/small\"\n")
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Permissions = []permission.Rule{
		{Decision: permission.Allow, Tool: "exec", ArgvPrefix: []string{"go", "build"}},
		{Decision: permission.Deny, Effect: permission.EffectExecute, Shell: &shellOnly},
		{Decision: permission.Ask, Tool: "write", PathGlob: "/tmp/*"},
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := LoadFile(path)
	if err != nil {
		rendered, _ := os.ReadFile(path)
		t.Fatalf("Save wrote a config that LoadFile rejects: %v\n%s", err, rendered)
	}
	if len(again.Permissions) != 3 {
		t.Fatalf("Permissions = %+v, want all three back", again.Permissions)
	}
	if again.Permissions[1].Shell == nil || !*again.Permissions[1].Shell {
		t.Errorf("shell-only did not survive: %+v", again.Permissions[1])
	}
	if again.Permissions[2].PathGlob != "/tmp/*" {
		t.Errorf("path glob did not survive: %+v", again.Permissions[2])
	}
}

// A rule reads back in the vocabulary it was written in, because the same
// sentence appears in /permissions and in the dry-run.
func TestARuleRendersWhatItCovers(t *testing.T) {
	got := RenderPermissionRule(permission.Rule{
		Decision: permission.Allow, Tool: "exec", ArgvPrefix: []string{"go", "test"},
	})
	for _, want := range []string{"allow", "tool exec", "argv go test"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %q, missing %q", got, want)
		}
	}
}
