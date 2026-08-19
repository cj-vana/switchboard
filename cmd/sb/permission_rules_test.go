package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/permission"
)

func rulesConfig(rules ...permission.Rule) *config.Config {
	return &config.Config{Path: "/home/u/.switchboard/config.toml", Permissions: rules}
}

// The point of the dry-run is settling what a rule says before a turn depends
// on it, and that means naming the rule, not just the verdict.
func TestPermissionsCheckNamesTheRuleThatAnswered(t *testing.T) {
	var out bytes.Buffer
	cfg := rulesConfig(permission.Rule{
		Decision: permission.Allow, Tool: "exec", ArgvPrefix: []string{"go", "test"},
	})
	if err := runPermissionsCLI(&out, cfg, []string{"--", "go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"go test ./...", "decision  allow", "argv go test", cfg.Path} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

// A dry-run reading as full coverage while covering one of three rule sources
// is the confident wrong answer this program exists to refuse.
func TestPermissionsCheckStatesWhatItCannotSee(t *testing.T) {
	var out bytes.Buffer
	if err := runPermissionsCLI(&out, rulesConfig(), []string{"--", "ls"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"MCP server", "remembered answers", "No sandbox is assumed"} {
		if !strings.Contains(text, want) {
			t.Errorf("scope statement missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "none matched") {
		t.Errorf("an unmatched command did not say so:\n%s", text)
	}
}

// A deny answers wherever it sits, so the dry-run has to report the deny and
// not the allow that happens to be listed first.
func TestPermissionsCheckReportsTheDenyOverAnEarlierAllow(t *testing.T) {
	var out bytes.Buffer
	cfg := rulesConfig(
		permission.Rule{Decision: permission.Allow, Tool: "exec"},
		permission.Rule{Decision: permission.Deny, Tool: "exec", ArgvPrefix: []string{"rm"}},
	)
	if err := runPermissionsCLI(&out, cfg, []string{"--", "rm", "-rf", "/"}); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "decision  deny") {
		t.Errorf("the deny did not win:\n%s", text)
	}
}

// The mode is half the answer, so it is both selectable and stated.
func TestPermissionsCheckHonorsAndReportsTheMode(t *testing.T) {
	var out bytes.Buffer
	if err := runPermissionsCLI(&out, rulesConfig(), []string{"-mode", "plan", "--", "ls"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "mode      plan") {
		t.Errorf("the mode was not reported:\n%s", text)
	}
	if !strings.Contains(text, "decision  deny") {
		t.Errorf("plan mode did not refuse a command:\n%s", text)
	}
}

// A command has its own flags, and this one must not try to read them.
func TestPermissionsCheckRequiresTheSeparator(t *testing.T) {
	var out bytes.Buffer
	err := runPermissionsCLI(&out, rulesConfig(), []string{"go", "test"})
	if err == nil {
		t.Fatal("a command without -- was accepted")
	}
	if !strings.Contains(err.Error(), "--") {
		t.Errorf("error = %q, which does not say what to type", err)
	}
}

// Bare lists, and an empty list says how to start rather than printing nothing.
func TestPermissionsBareListsAndAnEmptyOneTeaches(t *testing.T) {
	var out bytes.Buffer
	if err := runPermissionsCLI(&out, rulesConfig(), nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "[[permissions]]") {
		t.Errorf("an empty list did not say how to write one:\n%s", text)
	}

	out.Reset()
	cfg := rulesConfig(permission.Rule{Decision: permission.Allow, Tool: "exec", ArgvPrefix: []string{"go", "vet"}})
	if err := runPermissionsCLI(&out, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "argv go vet") {
		t.Errorf("the list did not render the rule:\n%s", text)
	}
}

// The reach a rule grants is the reach a typed yes grants, and the list is
// where a reader finds that out rather than by running something.
func TestThePermissionsListStatesTheReachARuleGrants(t *testing.T) {
	cfg := rulesConfig(permission.Rule{Decision: permission.Allow, Tool: "exec", ArgvPrefix: []string{"go"}})
	text := renderPermissionRules(cfg, permission.ModeDefault)
	if !strings.Contains(text, "runs on the host") {
		t.Errorf("the list does not say what an allow grants without a sandbox:\n%s", text)
	}
	if !strings.Contains(text, "before any allow list an MCP server declared") {
		t.Errorf("the list does not state its precedence:\n%s", text)
	}
}
