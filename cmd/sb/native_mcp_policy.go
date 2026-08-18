package main

import (
	"errors"
	"os"
	"strings"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
	"github.com/switchboard-code/switchboard/internal/mcppolicy"
)

// nativeMCPAssemblyPolicy binds authorization and Claude expansion to one
// immutable managed-policy snapshot. Keeping them together prevents policy
// from authorizing one expanded command or URL while the runtime executes a
// value expanded from the later, mutable process environment.
type nativeMCPAssemblyPolicy struct {
	checker                mcpnative.PolicyChecker
	claudeExpansion        mcppolicy.RuntimeExpansion
	claudeManagedExclusive bool
	codexPluginRestricted  bool
}

func (policy nativeMCPAssemblyPolicy) NativeMCPAllowed(request mcpnative.PolicyRequest) (bool, error) {
	if policy.checker == nil {
		return false, mcpnative.ErrPolicyRequired
	}
	return policy.checker.NativeMCPAllowed(request)
}

func (policy nativeMCPAssemblyPolicy) expandClaudeValue(value string) (string, error) {
	return policy.claudeExpansion.Expand(value)
}

// loadNativeMCPPolicy snapshots the local Codex and Claude policy surfaces at
// session assembly. It never assumes that an uninspected cloud or managed
// source is empty: mcppolicy quarantines only the affected dialect and returns
// redacted diagnostics for the UI.
func loadNativeMCPPolicy(workspace string, codexRequirementsChecked bool) (nativeMCPAssemblyPolicy, []mcpNote) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nativeMCPAssemblyPolicy{}, []mcpNote{{"error", "native MCP managed policy: home directory unavailable; native servers stay off"}}
	}
	options := mcppolicy.Options{
		HomeDir:                  home,
		Workspace:                workspace,
		StartupEnv:               append([]string(nil), os.Environ()...),
		ProgramData:              os.Getenv("ProgramData"),
		ProgramFiles:             os.Getenv("ProgramFiles"),
		CloudRequirementsChecked: codexRequirementsChecked,
	}
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		options.CodexConfigDir = configured
	}
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		options.ClaudeConfigDir = configured
	}

	checker, diagnostics, loadErr := mcppolicy.Load(options)
	notes := nativeMCPPolicyNotes(diagnostics)
	if loadErr != nil {
		notes = append(notes, mcpNote{"error", "native MCP managed policy is unavailable; native servers stay off: " + loadErr.Error()})
		return nativeMCPAssemblyPolicy{}, notes
	}
	binding := nativeMCPAssemblyPolicy{
		checker:                checker,
		claudeManagedExclusive: checker.ClaudeManagedExclusive(),
		codexPluginRestricted:  checker.CodexPluginMCPRestricted(),
	}
	expansion, expansionErr := checker.ClaudeRuntimeExpansion()
	if expansionErr == nil {
		binding.claudeExpansion = expansion
	} else {
		// A quarantined Claude policy must not disable otherwise valid Codex
		// policy. The absent expansion capability keeps every Claude server off.
		notes = append(notes, mcpNote{"error", "native Claude MCP runtime expansion is unavailable; Claude servers stay off"})
		if !errors.Is(expansionErr, mcppolicy.ErrClaudePolicyUnavailable) {
			notes[len(notes)-1].text += ": " + expansionErr.Error()
		}
	}
	return binding, notes
}

func nativeMCPPolicyNotes(diagnostics []mcppolicy.Diagnostic) []mcpNote {
	notes := make([]mcpNote, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		level := "warn"
		if diagnostic.Severity == mcppolicy.SeverityError {
			level = "error"
		}
		message := "native MCP policy " + diagnostic.Code + ": " + diagnostic.Message
		if diagnostic.Dialect != "" {
			message = "native MCP " + string(diagnostic.Dialect) + " policy " + diagnostic.Code + ": " + diagnostic.Message
		}
		if diagnostic.Field != "" {
			message += " [field " + diagnostic.Field + "]"
		}
		if diagnostic.Path != "" {
			message += " (" + diagnostic.Path + ")"
		}
		notes = append(notes, mcpNote{level, message})
	}
	return notes
}
