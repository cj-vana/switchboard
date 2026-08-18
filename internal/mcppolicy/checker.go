package mcppolicy

import "github.com/switchboard-code/switchboard/internal/mcpnative"

// NativeMCPAllowed applies the policy belonging to the native declaration's
// dialect. Unknown dialects and a nil checker fail closed.
func (checker *Checker) NativeMCPAllowed(request mcpnative.PolicyRequest) (bool, error) {
	if checker == nil {
		return false, ErrUnsupportedDialect
	}
	switch request.Dialect {
	case mcpnative.DialectCodex:
		return checker.codex.allowed(request)
	case mcpnative.DialectClaude:
		return checker.claude.allowed(request)
	default:
		return false, ErrUnsupportedDialect
	}
}
