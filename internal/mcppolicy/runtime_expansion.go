package mcppolicy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RuntimeExpansion is an immutable capability for expanding Claude MCP
// configuration from the exact environment snapshot used during policy
// evaluation. Its contents are deliberately opaque and redact under every
// ordinary formatting and serialization path.
//
// Callers must acquire it from the same Checker that authorized a server and
// use Expand for every Claude server field that supports ${...} expansion,
// including plugin-provided MCP. It is not used for Codex declarations.
type RuntimeExpansion struct {
	environment environment
	valid       bool
}

// ClaudeRuntimeExpansion returns the runtime expansion capability paired with
// this policy snapshot. An unavailable or nil checker fails closed.
func (checker *Checker) ClaudeRuntimeExpansion() (RuntimeExpansion, error) {
	if checker == nil || checker.claude.unavailable {
		return RuntimeExpansion{}, ErrClaudePolicyUnavailable
	}
	return RuntimeExpansion{environment: checker.claude.runtimeEnvironment.clone(), valid: true}, nil
}

// Expand resolves Claude's ${VAR} and ${VAR:-default} forms without consulting
// the live process environment. Missing variables without a default and
// malformed references return a stable redacted error.
func (expansion RuntimeExpansion) Expand(value string) (string, error) {
	if !expansion.valid {
		return "", ErrClaudeRuntimeExpansion
	}
	var result strings.Builder
	for cursor := 0; cursor < len(value); {
		start := strings.Index(value[cursor:], "${")
		if start < 0 {
			result.WriteString(value[cursor:])
			break
		}
		start += cursor
		result.WriteString(value[cursor:start])
		endOffset := strings.IndexByte(value[start+2:], '}')
		if endOffset < 0 {
			return "", ErrClaudeRuntimeExpansion
		}
		end := start + 2 + endOffset
		body := value[start+2 : end]
		name, fallback, hasFallback := body, "", false
		if split := strings.Index(body, ":-"); split >= 0 {
			name, fallback, hasFallback = body[:split], body[split+2:], true
		}
		if !validEnvironmentName(name) {
			return "", ErrClaudeRuntimeExpansion
		}
		replacement, exists := expansion.environment.lookup(name)
		if !exists {
			if !hasFallback {
				return "", ErrClaudeRuntimeExpansion
			}
			replacement = fallback
		}
		result.WriteString(replacement)
		cursor = end + 1
	}
	return result.String(), nil
}

func (RuntimeExpansion) String() string             { return "<Claude MCP runtime expansion redacted>" }
func (expansion RuntimeExpansion) GoString() string { return expansion.String() }
func (expansion RuntimeExpansion) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(expansion.String()))
}
func (expansion RuntimeExpansion) MarshalJSON() ([]byte, error) {
	return json.Marshal(expansion.String())
}
func (expansion RuntimeExpansion) MarshalText() ([]byte, error) {
	return []byte(expansion.String()), nil
}
