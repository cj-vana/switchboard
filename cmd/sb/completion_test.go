package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The completion lists are asserted against the real dispatch and the real
// flag set, read from main.go's own source: a completion that offers what
// the binary refuses, or hides what it takes, is worse than none.
func TestCompletionListsMatchTheBinary(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	subRe := regexp.MustCompile(`os\.Args\[1\] == "([a-z]+)"`)
	var dispatched []string
	for _, m := range subRe.FindAllStringSubmatch(string(src), -1) {
		dispatched = append(dispatched, m[1])
	}
	sort.Strings(dispatched)
	declared := append([]string(nil), completionSubcommands...)
	sort.Strings(declared)
	if strings.Join(dispatched, ",") != strings.Join(declared, ",") {
		t.Errorf("subcommands drifted:\n binary   %v\n complete %v", dispatched, declared)
	}

	// Most flags use the typed helpers; bool-like enum flags (such as
	// -sandbox[=on|off|auto]) use flag.Var directly.
	flagRe := regexp.MustCompile(`flag\.(?:[A-Za-z]+Var|Var)\([^,]+, "([a-zA-Z-]+)"`)
	var flags []string
	for _, m := range flagRe.FindAllStringSubmatch(string(src), -1) {
		flags = append(flags, "-"+m[1])
	}
	sort.Strings(flags)
	declaredFlags := append([]string(nil), completionFlags...)
	sort.Strings(declaredFlags)
	if strings.Join(flags, ",") != strings.Join(declaredFlags, ",") {
		t.Errorf("flags drifted:\n binary   %v\n complete %v", flags, declaredFlags)
	}
}

func TestCompletionEmitsForBothShellsAndRefusesOthers(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "fish"} {
		var b strings.Builder
		if err := runCompletionCLI(&b, shell); err != nil {
			t.Fatalf("%s: %v", shell, err)
		}
		for _, want := range []string{"doctor", "resume", "sb completion", "inspect", "untrust", "disable"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("%s script missing %q", shell, want)
			}
		}
	}
	var b strings.Builder
	if err := runCompletionCLI(&b, "powershell"); err == nil {
		t.Error("an unsupported shell should refuse with the supported ones named")
	}
}

func TestCompletionDoesNotOfferGlobalFlagsAfterSubcommands(t *testing.T) {
	var zsh strings.Builder
	if err := runCompletionCLI(&zsh, "zsh"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zsh.String(), "*) return 1 ;;") || !strings.Contains(zsh.String(), "else\n    return 1") {
		t.Fatal("zsh completion must stop offering global flags after a subcommand")
	}

	var bash strings.Builder
	if err := runCompletionCLI(&bash, "bash"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bash.String(), "else\n    COMPREPLY=()") {
		t.Fatal("bash completion must stop offering global flags after a subcommand")
	}
}
