package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEveryCompletionSubcommandHasPureScopedHelp(t *testing.T) {
	for _, command := range completionSubcommands {
		command := command
		for _, flag := range []string{"-h", "--help"} {
			flag := flag
			t.Run(command+"/"+flag, func(t *testing.T) {
				var out strings.Builder
				handled, err := handleCLIHelp(&out, []string{command, flag})
				if !handled || err != nil {
					t.Fatalf("handled=%v err=%v", handled, err)
				}
				if !strings.Contains(out.String(), "usage:") {
					t.Fatalf("help has no usage: %q", out.String())
				}
				if command != "help" && !strings.Contains(out.String(), "sb "+command) {
					t.Fatalf("help is not scoped to %s: %q", command, out.String())
				}
			})
		}
	}
}

func TestHelpCommandSupportsEveryCompletionSubcommand(t *testing.T) {
	var root strings.Builder
	handled, err := handleCLIHelp(&root, []string{"help"})
	if !handled || err != nil {
		t.Fatalf("root help: handled=%v err=%v", handled, err)
	}
	for _, command := range completionSubcommands {
		if !strings.Contains(root.String(), command) {
			t.Errorf("root help missing %q", command)
		}

		var scoped strings.Builder
		handled, err := handleCLIHelp(&scoped, []string{"help", command})
		if !handled || err != nil {
			t.Errorf("sb help %s: handled=%v err=%v", command, handled, err)
			continue
		}
		if !strings.Contains(scoped.String(), "usage:") || !strings.Contains(scoped.String(), "sb "+command) {
			t.Errorf("sb help %s is not scoped: %q", command, scoped.String())
		}
	}
}

func TestResolvedHelpPathsAcceptTheirCompletedHelpFlags(t *testing.T) {
	for _, request := range [][]string{
		{"help", "doctor", "-h"},
		{"help", "doctor", "--help"},
		{"help", "plugins", "install", "-h"},
		{"help", "mcp", "inspect", "--help"},
	} {
		var out strings.Builder
		handled, err := handleCLIHelp(&out, request)
		if !handled || err != nil || !strings.Contains(out.String(), "usage:") {
			t.Errorf("%v: handled=%v err=%v output=%q", request, handled, err, out.String())
		}
	}
}

func TestHelpKeepsFlagsBeforeSubcommandGrammarPure(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"-workspace", "repo with spaces", "plugins", "--help"}, want: "sb plugins list"},
		{args: []string{"-mode=auto", "mcp", "inspect", "-h"}, want: "usage: sb mcp inspect"},
		{args: []string{"-sandbox", "doctor", "--help"}, want: "usage: sb doctor"},
		{args: []string{"-profile", "review", "help", "completion"}, want: "usage: sb completion"},
		{args: []string{"-workspace", "repo", "--help"}, want: "subcommands:"},
	}
	for _, test := range tests {
		var out strings.Builder
		handled, err := handleCLIHelp(&out, test.args)
		if !handled || err != nil || !strings.Contains(out.String(), test.want) {
			t.Errorf("%v: handled=%v err=%v output=%q; want %q", test.args, handled, err, out.String(), test.want)
		}
	}
}

func TestEveryNestedActionHasScopedHelp(t *testing.T) {
	for command, actions := range completionActions {
		for _, action := range actions {
			action := action
			t.Run(command+"/"+action, func(t *testing.T) {
				requests := [][]string{
					{command, action, "-h"},
					{command, action, "--help"},
					{command, "help", action},
					{command, "help", action, "-h"},
					{command, "help", action, "--help"},
					{"help", command, action},
				}
				for _, request := range requests {
					var out strings.Builder
					handled, err := handleCLIHelp(&out, request)
					if !handled || err != nil {
						t.Errorf("%v: handled=%v err=%v", request, handled, err)
						continue
					}
					want := "usage: sb " + command + " " + action
					if !strings.Contains(out.String(), want) {
						t.Errorf("%v: want %q in %q", request, want, out.String())
					}
				}
			})
		}
	}
}

func TestHelpRunReturnsBeforeAnyFilesystemOrRuntimeAssembly(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-must-not-be-opened"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude-must-not-be-opened"))

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	before := treeNames(t, home, workspace)
	requests := [][]string{
		{"help"},
		{"-h"},
		{"--help"},
		{"-help"},
		{"-h=false"},
		{"--help=false"},
		{"-help=false"},
	}
	for _, command := range completionSubcommands {
		for _, helpFlag := range []string{"-h", "--help"} {
			requests = append(requests, []string{command, helpFlag})
		}
	}
	for command, actions := range completionActions {
		for _, action := range actions {
			for _, helpFlag := range []string{"-h", "--help"} {
				requests = append(requests,
					[]string{command, action, helpFlag},
					[]string{command, "help", action, helpFlag},
				)
			}
		}
	}
	for _, request := range requests {
		oldArgs, oldStdout := os.Args, os.Stdout
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Args = append([]string{"sb"}, request...)
		os.Stdout = writeEnd
		runErr := run()
		_ = writeEnd.Close()
		os.Args, os.Stdout = oldArgs, oldStdout
		_, readErr := io.ReadAll(readEnd)
		_ = readEnd.Close()
		if runErr != nil || readErr != nil {
			t.Fatalf("sb %s: run=%v read=%v", strings.Join(request, " "), runErr, readErr)
		}
	}
	after := treeNames(t, home, workspace)
	if strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("help mutated files:\n before %v\n after  %v", before, after)
	}
}

func TestHelpErrorsStayOnPureHelpPath(t *testing.T) {
	for _, request := range [][]string{
		{"help", "not-a-command"},
		{"plugins", "help", "not-an-action"},
		{"mcp", "not-an-action", "--help"},
	} {
		var out strings.Builder
		handled, err := handleCLIHelp(&out, request)
		if !handled || err == nil {
			t.Errorf("%v: handled=%v err=%v", request, handled, err)
		}
	}
}

func treeNames(t *testing.T, roots ...string) []string {
	t.Helper()
	var names []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, root+"\x00"+rel+"\x00"+entry.Type().String())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	return names
}
