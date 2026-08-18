// Package extensions discovers native Codex and Claude plugin layouts and
// records Switchboard's independent activation decisions. Discovery never
// enables or executes anything a plugin declares.
//
// Discovery is deliberately rooted: callers provide exact local directories
// and their scopes. The package does not search home directories, repositories,
// marketplaces, or network locations, and it does not resolve which duplicate
// should win. Executable describes capability, not permission.
package extensions

// Dialect identifies the native plugin layout that produced a record.
type Dialect string

const (
	DialectCodex  Dialect = "codex"
	DialectClaude Dialect = "claude"
)

// Kind is the extension container type. This slice only recognizes plugins;
// individual skills remain components of their owning plugin.
type Kind string

const KindPlugin Kind = "plugin"

// Scope records where a caller found a plugin. Scope is intentionally opaque:
// discovery preserves it but does not invent cross-product precedence rules.
type Scope string

const (
	ScopeLocal     Scope = "local"
	ScopeWorkspace Scope = "workspace"
	ScopeUser      Scope = "user"
	ScopeManaged   Scope = "managed"
)

// ComponentKind is a component shape normalized by this bounded discovery
// layer. Commands, agents, apps, monitors, workflows, and LSP servers are
// reported as unsupported warnings rather than silently treated as one of
// these kinds.
type ComponentKind string

const (
	ComponentSkill ComponentKind = "skill"
	ComponentMCP   ComponentKind = "mcp"
	ComponentHook  ComponentKind = "hook"
)

// ComponentSource says whether a path was declared by a manifest or found at
// the dialect's conventional location.
type ComponentSource string

const (
	SourceManifest ComponentSource = "manifest"
	SourceDefault  ComponentSource = "default"
)

// Candidate is an exact local plugin root supplied by a caller. Scope must be
// non-empty. Dialect may constrain discovery when a marketplace or native
// registry already knows the format; an empty Dialect inspects both formats.
// Root may itself be reached through a symlink; Root preserves that absolute
// spelling while Plugin.RealPath records its physical identity.
type Candidate struct {
	Root    string
	Scope   Scope
	Dialect Dialect
}

// Warning records a valid but lossy or unsupported part of a plugin. Paths are
// local display paths; no warning grants permission to read outside Root or to
// execute a component.
type Warning struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Component is one manifest declaration or conventional component path.
// DeclaredPath is the plugin-relative spelling (for example "./skills"). Path
// is absolute through the supplied root and RealPath is its resolved physical
// path. Inline declarations have Inline true and no paths; they are retained so
// executable capability cannot disappear during normalization.
type Component struct {
	Kind         ComponentKind   `json:"kind"`
	Source       ComponentSource `json:"source"`
	DeclaredPath string          `json:"declared_path,omitempty"`
	Path         string          `json:"path,omitempty"`
	RealPath     string          `json:"real_path,omitempty"`
	Inline       bool            `json:"inline,omitempty"`
	Executable   bool            `json:"executable"`
}

// Plugin is a normalized, read-only plugin record. ID is dialect-qualified
// (for example "claude:review-tools") so identical native namespaces do not
// collide across products. Digest is a SHA-256 digest of the bounded plugin
// tree (excluding .git metadata), not an authenticity or trust assertion.
type Plugin struct {
	Dialect    Dialect     `json:"dialect"`
	Kind       Kind        `json:"kind"`
	Scope      Scope       `json:"scope"`
	Root       string      `json:"root"`
	RealPath   string      `json:"real_path"`
	Manifest   string      `json:"manifest,omitempty"`
	Namespace  string      `json:"namespace"`
	ID         string      `json:"id"`
	Components []Component `json:"components"`
	Executable bool        `json:"executable"`
	Digest     string      `json:"digest"`
	Warnings   []Warning   `json:"warnings,omitempty"`
}

// Severity is the disposition of a discovery diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic describes a rejected candidate/dialect or an ambiguity spanning
// multiple otherwise valid plugins. Discovery continues after each diagnostic.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

// Result is deterministically ordered regardless of Candidate input order.
type Result struct {
	Plugins     []Plugin     `json:"plugins"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
