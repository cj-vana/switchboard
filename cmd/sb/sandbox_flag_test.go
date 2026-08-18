package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

func TestSandboxFlagAcceptsBareBoolAndNamedModes(t *testing.T) {
	for input, want := range map[string]string{
		"true": "on", "false": "off", "on": "on", "off": "off", "auto": "auto",
	} {
		var got string
		flag := sandboxFlag{target: &got}
		if err := flag.Set(input); err != nil || got != want {
			t.Errorf("Set(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	var got string
	if err := (sandboxFlag{target: &got}).Set("maybe"); err == nil {
		t.Error("invalid sandbox mode accepted")
	}
}

func TestYOLOLaunchRejectsConfiguredOrExplicitSandbox(t *testing.T) {
	for _, sandbox := range []execution.SandboxMode{execution.SandboxOn, execution.SandboxAuto} {
		err := validateExecutionPosture(permission.ModeYOLO, sandbox)
		if err == nil || !strings.Contains(err.Error(), "requires sandbox off") {
			t.Fatalf("yolo + %s = %v, want clear conflict", sandbox, err)
		}
	}
	if err := validateExecutionPosture(permission.ModeYOLO, execution.SandboxOff); err != nil {
		t.Fatalf("yolo + off: %v", err)
	}
	if err := validateExecutionPosture(permission.ModeAuto, execution.SandboxOn); err != nil {
		t.Fatalf("auto + on: %v", err)
	}
}
