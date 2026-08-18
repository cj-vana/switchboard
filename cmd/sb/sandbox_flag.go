package main

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/execution"
)

// sandboxFlag is bool-like so `-sandbox` means on, while explicit
// `-sandbox=off|on|auto` remains available for scripts and overrides.
type sandboxFlag struct{ target *string }

func (f sandboxFlag) String() string {
	if f.target == nil {
		return ""
	}
	return *f.target
}

func (f sandboxFlag) Set(value string) error {
	if f.target == nil {
		return fmt.Errorf("sandbox flag has no target")
	}
	mode, err := execution.ParseSandboxMode(value)
	if err != nil {
		return err
	}
	*f.target = string(mode)
	return nil
}

func (sandboxFlag) IsBoolFlag() bool { return true }
