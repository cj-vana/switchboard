// Package mcppolicy loads the native Codex and Claude Code policy surfaces
// that constrain MCP servers and plugin selection. It deliberately does not
// start either client, execute policy helpers, contact a management service,
// or expose configured commands, arguments, URLs, environment values, or
// policy contents.
package mcppolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

const (
	// MaxPolicyFileBytes is applied independently to every policy or settings
	// file. Aggregate reads and candidate counts have separate bounds.
	MaxPolicyFileBytes int64 = 1 << 20
	MaxPolicyFiles           = 64
	MaxPolicyBytes     int64 = 4 << 20
	MaxPolicyEntries         = 4096
	MaxPolicyValues          = 1024
)

var (
	ErrCodexPolicyUnavailable  = errors.New("native Codex MCP policy is unavailable")
	ErrClaudePolicyUnavailable = errors.New("native Claude MCP policy is unavailable")
	ErrUnsupportedDialect      = errors.New("native MCP policy dialect is unsupported")
	ErrClaudeRuntimeExpansion  = errors.New("Claude MCP value cannot be expanded from the authorized environment")
)

// Document is an already-retrieved managed policy fragment. Its contents are
// intentionally private and every ordinary rendering is redacted.
type Document struct {
	label    string
	contents []byte
}

// NewDocument copies a managed policy fragment. Label must identify only the
// delivery surface; it must not contain policy values or credentials.
func NewDocument(label string, contents []byte) Document {
	return Document{label: label, contents: append([]byte(nil), contents...)}
}

func (d Document) String() string   { return "<managed policy document redacted>" }
func (d Document) GoString() string { return d.String() }
func (d Document) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(d.String()))
}
func (d Document) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Paths is the exact native policy surface inventory. Call ResolvePaths for
// platform defaults. Tests and embedding runtimes may provide a complete
// override through Options.Paths.
type Paths struct {
	CodexRequirements string
	CodexAuth         string
	CodexMDM          []string

	ClaudeManagedSettings string
	ClaudeManagedDropIns  string
	ClaudeManagedMCP      string
	ClaudeRemoteSettings  string
	ClaudeMDM             []string
	ClaudeState           string
	ClaudeUserSettings    string
	ClaudeProjectSettings string
	ClaudeLocalSettings   string
}

// Options supplies roots and controlled inputs. StartupEnv is the environment
// captured before any native settings are applied. A nil slice uses the live
// process environment; a non-nil empty slice means an empty environment.
//
// CloudRequirementsChecked says the caller authoritatively checked Codex's
// signed-in cloud bundle. CloudRequirements may be empty when that check found
// no fragments. Without the check, a detected ChatGPT login quarantines Codex
// policy rather than assuming the cloud bundle is empty.
type Options struct {
	HomeDir         string
	Workspace       string
	CodexConfigDir  string
	ClaudeConfigDir string
	GOOS            string
	UserName        string
	ProgramData     string
	ProgramFiles    string
	StartupEnv      []string
	Paths           *Paths

	CloudRequirements        []Document
	CloudRequirementsChecked bool
	CodexMDMRequirements     *Document
	ClaudeRemoteSettings     *Document
	ClaudeMDMSettings        *Document
}

// Severity describes a safe-to-render policy diagnostic.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Diagnostic never contains policy values. Path and Field identify only the
// source and structural location that needs attention.
type Diagnostic struct {
	Severity Severity          `json:"severity"`
	Dialect  mcpnative.Dialect `json:"dialect"`
	Code     string            `json:"code"`
	Path     string            `json:"path,omitempty"`
	Field    string            `json:"field,omitempty"`
	Message  string            `json:"message"`
}

func (d Diagnostic) String() string {
	encoded, err := json.Marshal(d)
	if err != nil {
		return "<managed policy diagnostic>"
	}
	return string(encoded)
}

// Checker is immutable after Load and implements mcpnative.PolicyChecker.
// Its fields stay private so commands, URLs, and environment values cannot be
// accidentally serialized by a status or diagnostic path.
type Checker struct {
	codex  codexPolicy
	claude claudePolicy
}

// ClaudePluginConstraint is an authoritative managed selection bound to one
// exact marketplace-qualified Claude plugin identity. Native enablement still
// never grants Switchboard authority; only denials cross this boundary.
type ClaudePluginConstraint struct {
	NativeID string
	Denied   bool
}

var _ mcpnative.PolicyChecker = (*Checker)(nil)

func (c *Checker) String() string   { return "<native MCP managed policy redacted>" }
func (c *Checker) GoString() string { return c.String() }
func (c *Checker) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(c.String()))
}
func (c *Checker) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// ClaudeManagedExclusive reports whether a system managed-mcp.json surface
// was present at load time. It exposes no server definition or policy value;
// plugin loaders may use it to skip Claude plugin MCP before parsing. The
// checker independently enforces the same restriction on every request.
func (c *Checker) ClaudeManagedExclusive() bool {
	return c != nil && c.claude.managedExclusive
}

// CodexPluginMCPRestricted reports whether authoritative requirements contain
// at least one plugin MCP rule. Callers use it only to require canonical
// native plugin provenance before presenting PluginID to the checker; the
// checker still enforces every rule independently.
func (c *Checker) CodexPluginMCPRestricted() bool {
	return c != nil && !c.codex.unavailable && c.codex.pluginRestricted
}

// ClaudePluginConstraints returns managed plugin denies from the same frozen
// source composition used for Claude MCP policy. If any authoritative source
// was present but could not be decoded, callers must quarantine Claude plugin
// behavior rather than treating the missing constraint as an allow.
func (c *Checker) ClaudePluginConstraints() ([]ClaudePluginConstraint, error) {
	if c == nil || c.claude.unavailable {
		return nil, ErrClaudePolicyUnavailable
	}
	ids := make([]string, 0, len(c.claude.pluginDenials))
	for id := range c.claude.pluginDenials {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	constraints := make([]ClaudePluginConstraint, 0, len(ids))
	for _, id := range ids {
		constraints = append(constraints, ClaudePluginConstraint{NativeID: id, Denied: true})
	}
	return constraints, nil
}

func platform(options Options) string {
	if options.GOOS != "" {
		return options.GOOS
	}
	return runtime.GOOS
}
