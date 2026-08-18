// Package mcpnative discovers MCP server declarations written for Codex and
// Claude Code and normalizes them without starting a process, dialing a
// server, expanding environment variables, or accepting another client's
// workspace trust decision.
//
// Discovery is intentionally explicit: callers provide the user's home
// directory and the resolved workspace root. Project declarations are marked
// as requiring a Switchboard trust grant; this package never grants one.
package mcpnative

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MaxConfigBytes is the largest native configuration file discovery reads.
// The limit is applied independently to every candidate file.
const MaxConfigBytes int64 = 1 << 20

// Discovery also caps aggregate work across nested Codex project layers.
const (
	MaxConfigFiles      = 64
	MaxConfigCandidates = 132
	MaxTotalConfigBytes = 4 << 20
	MaxServerEntries    = 2048
	MaxProjectEntries   = 4096
	MaxEntryValues      = 1024
	MaxConfigDepth      = 64
	MaxConfigValues     = 65536
)

// Dialect identifies the client whose configuration format was read.
type Dialect string

const (
	DialectCodex  Dialect = "codex"
	DialectClaude Dialect = "claude"
)

// Scope is the native configuration scope. Local is Claude Code's
// project-specific state stored in ~/.claude.json; it still requires a
// Switchboard workspace trust grant because it selects executable behavior for
// one project. Managed is Claude's system-wide exclusive managed-mcp.json
// source; it still requires explicit Switchboard activation but not workspace
// trust. Plugin is an explicit component of a separately discovered plugin and
// requires the caller's digest-bound executable-trust checker.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
	ScopeManaged Scope = "managed"
	ScopePlugin  Scope = "plugin"
)

// Source identifies one exact native declaration surface.
type Source string

const (
	SourceCodexUser     Source = "codex-user-config"
	SourceCodexProject  Source = "codex-project-config"
	SourceCodexPackage  Source = "codex-packaged-defaults"
	SourceCodexMDM      Source = "codex-managed-preferences"
	SourceCodexSystem   Source = "codex-system-config"
	SourceCodexCloud    Source = "codex-enterprise-config"
	SourceCodexProfile  Source = "codex-user-profile"
	SourceCodexSession  Source = "codex-session-config"
	SourceCodexLegacy   Source = "codex-legacy-managed-config"
	SourceClaudeUser    Source = "claude-user-config"
	SourceClaudeProject Source = "claude-project-config"
	SourceClaudeLocal   Source = "claude-local-config"
	SourceClaudeManaged Source = "claude-managed-config"
	SourceCodexPlugin   Source = "codex-plugin-mcp"
	SourceClaudePlugin  Source = "claude-plugin-mcp"
)

// Transport preserves the native connection mechanism. Non-baseline
// transports such as SSE and WebSocket remain feature-gated at Materialize so
// they cannot be silently treated as Streamable HTTP.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
	TransportWS    Transport = "ws"
)

// EnvSource preserves which executor environment Codex should read an
// env_vars name from. A remote source remains parsed but Materialize requires
// an explicit remote-execution feature claim.
type EnvSource string

const (
	EnvSourceLocal  EnvSource = "local"
	EnvSourceRemote EnvSource = "remote"
)

// EnvVar is one Codex env_vars forwarding declaration.
type EnvVar struct {
	Name   string    `json:"name"`
	Source EnvSource `json:"source"`
}

// ExecutionEnvironment is retained in the normalized handoff for older
// callers. Current Codex declarations use EnvironmentID; the obsolete
// experimental_environment input is rejected as unsupported.
type ExecutionEnvironment string

const (
	ExecutionEnvironmentLocal  ExecutionEnvironment = "local"
	ExecutionEnvironmentRemote ExecutionEnvironment = "remote"
)

// HTTPAuth preserves Codex HTTP authentication requests. Credential flows are
// separate runtime features and must be claimed explicitly at Materialize.
type HTTPAuth string

const (
	HTTPAuthOAuth   HTTPAuth = "oauth"
	HTTPAuthChatGPT HTTPAuth = "chatgpt"
)

// ApprovalMode is Codex's native per-server/per-tool MCP approval behavior.
type ApprovalMode string

const (
	ApprovalAuto    ApprovalMode = "auto"
	ApprovalPrompt  ApprovalMode = "prompt"
	ApprovalWrites  ApprovalMode = "writes"
	ApprovalApprove ApprovalMode = "approve"
)

// Options names the exact local roots to inspect. HomeDir and Workspace are
// not inferred from process state, which keeps discovery testable and lets the
// caller decide what it considers the active workspace. CurrentDir optionally
// enables Codex's project-root-to-cwd config layering and must be within
// Workspace; it defaults to Workspace. CodexConfigDir and ClaudeStatePath let
// the caller supply its already-resolved CODEX_HOME and CLAUDE_CONFIG_DIR
// locations without this package reading process environment variables.
// ClaudeManagedMCPPath is the caller-resolved platform system path for
// managed-mcp.json. When the file exists it is Claude's exclusive server
// source, including when its mcpServers object is empty. CodexSnapshot must be
// produced from Codex's authoritative effective config loader for direct Codex
// entries to become executable. Without it, the user/project scan remains
// inventory-only because omitted package, MDM, system, cloud, profile,
// session/thread, or legacy-managed layers could carry restrictive fields.
type Options struct {
	HomeDir              string
	Workspace            string
	CurrentDir           string
	CodexConfigDir       string
	ClaudeStatePath      string
	ClaudeManagedMCPPath string
	CodexSnapshot        *CodexSnapshot
}

// Provenance records where a winning entry came from. Codex direct entries may
// contain recursively merged fields recorded in ContributingLayers. ConfigKey
// is the native table/object path, not a value from the entry.
type Provenance struct {
	Dialect            Dialect           `json:"dialect"`
	Scope              Scope             `json:"scope"`
	Source             Source            `json:"source"`
	Path               string            `json:"path"`
	RealPath           string            `json:"real_path"`
	ConfigKey          string            `json:"config_key"`
	PluginID           string            `json:"plugin_id,omitempty"`
	PluginRoot         string            `json:"plugin_root,omitempty"`
	ContributingLayers []LayerProvenance `json:"contributing_layers,omitempty"`
}

// LayerProvenance records each low-to-high Codex layer that contributed fields
// to a recursively merged server definition. Claude entries use whole-entry
// precedence and therefore do not need this list.
type LayerProvenance struct {
	Scope    Scope  `json:"scope"`
	Source   Source `json:"source"`
	Path     string `json:"path"`
	RealPath string `json:"real_path"`
}

// SensitiveValue holds configuration bytes that commonly contain credentials
// (commands, arguments, environment values, header values, and URLs). Every
// ordinary rendering is redacted. Discovery deliberately provides no public
// raw-value accessor; values become accessible only through Result.Materialize
// after Switchboard activation, the applicable workspace/plugin trust check,
// and runtime compatibility checks succeed.
type SensitiveValue struct {
	value string
}

func sensitive(value string) SensitiveValue { return SensitiveValue{value: value} }

func (v SensitiveValue) raw() string { return v.value }

// Empty reports whether the exact configuration value is empty.
func (v SensitiveValue) Empty() bool { return v.value == "" }

const redacted = "<native MCP value redacted>"

func (SensitiveValue) String() string   { return redacted }
func (SensitiveValue) GoString() string { return redacted }
func (SensitiveValue) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redacted))
}
func (SensitiveValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}
func (SensitiveValue) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// EnvReferences returns sorted environment variable names referenced by the
// native ${NAME} or ${NAME:-fallback} syntax. It never reads the environment
// and never returns fallback bytes.
func (v SensitiveValue) EnvReferences() []string {
	return envReferences(v.value)
}

// Timeouts preserves the two timeout boundaries native Codex exposes.
type Timeouts struct {
	StartupSeconds      float64 `json:"startup_seconds,omitempty"`
	StartupMilliseconds uint64  `json:"startup_milliseconds,omitempty"`
	ToolSeconds         float64 `json:"tool_seconds,omitempty"`
	ClaudeToolMillis    float64 `json:"claude_tool_millis,omitempty"`
	StartupSet          bool    `json:"startup_set,omitempty"`
	StartupMillisSet    bool    `json:"startup_millis_set,omitempty"`
	ToolSet             bool    `json:"tool_set,omitempty"`
	ClaudeToolSet       bool    `json:"claude_tool_set,omitempty"`
}

// ClaudeOAuth preserves preconfigured public-client metadata. Claude stores
// client secrets outside the config; this adapter never reads that store.
type ClaudeOAuth struct {
	ClientID              *SensitiveValue `json:"client_id,omitempty"`
	CallbackPort          int             `json:"callback_port,omitempty"`
	CallbackPortSet       bool            `json:"callback_port_set,omitempty"`
	AuthServerMetadataURL *SensitiveValue `json:"auth_server_metadata_url,omitempty"`
	Scopes                *SensitiveValue `json:"scopes,omitempty"`
}

// CodexOAuth preserves public OAuth client settings. Credentials themselves
// live in Codex's credential store and are never read by discovery.
type CodexOAuth struct {
	ClientID        *SensitiveValue `json:"client_id,omitempty"`
	CallbackPort    int             `json:"callback_port,omitempty"`
	CallbackPortSet bool            `json:"callback_port_set,omitempty"`
}

// ToolExposureSurface is a Codex model-facing surface from which configured
// MCP tools are omitted.
type ToolExposureSurface string

const (
	ToolExposureCodeMode ToolExposureSurface = "code_mode"
	ToolExposureDeferred ToolExposureSurface = "deferred"
	ToolExposureDirect   ToolExposureSurface = "direct"
)

// ToolFilter is an allow/deny name filter. Lists are sorted and deduplicated;
// the native rule that a disabled tool wins remains representable because both
// lists are retained.
type ToolFilter struct {
	Enabled     []string `json:"enabled,omitempty"`
	Disabled    []string `json:"disabled,omitempty"`
	EnabledSet  bool     `json:"enabled_set,omitempty"`
	DisabledSet bool     `json:"disabled_set,omitempty"`
}

// Approvals preserves directly representable per-tool allow/deny declarations.
// It does not import another client's remembered approvals or workspace trust.
type Approvals struct {
	Default    ApprovalMode            `json:"default,omitempty"`
	DefaultSet bool                    `json:"default_set,omitempty"`
	Tools      map[string]ApprovalMode `json:"tools,omitempty"`
}

// Server is one winning/effective native entry. Supported means the native entry
// parsed without unknown fields or invalid combinations; it is not a claim
// that a particular runtime implements every preserved semantic. Consumers
// must use Result.Materialize before constructing a transport.
type Server struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`

	// definition contains a canonical whole-entry representation used only to
	// derive a keyed activation identity. Its rendering is always redacted.
	definition *definitionMaterial

	Provenance Provenance `json:"provenance"`

	// ExecutionTrustRequired is true for every project/local/plugin declaration,
	// regardless of any trust flag stored by Codex or Claude Code.
	ExecutionTrustRequired bool   `json:"execution_trust_required"`
	TrustRoot              string `json:"trust_root,omitempty"`

	Supported         bool     `json:"supported"`
	UnsupportedFields []string `json:"unsupported_fields,omitempty"`

	Enabled     bool `json:"enabled"`
	EnabledSet  bool `json:"enabled_set,omitempty"`
	Required    bool `json:"required,omitempty"`
	RequiredSet bool `json:"required_set,omitempty"`

	Transport            Transport            `json:"transport,omitempty"`
	ExecutionEnvironment ExecutionEnvironment `json:"execution_environment,omitempty"`
	EnvironmentID        string               `json:"environment_id,omitempty"`
	EnvironmentIDSet     bool                 `json:"environment_id_set,omitempty"`
	Command              *SensitiveValue      `json:"command,omitempty"`
	Args                 []SensitiveValue     `json:"args,omitempty"`
	ArgsSet              bool                 `json:"args_set,omitempty"`
	CWD                  *SensitiveValue      `json:"cwd,omitempty"`

	// Static environment and header values stay redacted in every rendering.
	// InheritedEnv and header environment references are names only; discovery
	// never reads their values.
	Env                          map[string]SensitiveValue `json:"env,omitempty"`
	EnvSet                       bool                      `json:"env_set,omitempty"`
	ForwardedEnv                 []EnvVar                  `json:"forwarded_env,omitempty"`
	ForwardedEnvSet              bool                      `json:"forwarded_env_set,omitempty"`
	URL                          *SensitiveValue           `json:"url,omitempty"`
	Headers                      map[string]SensitiveValue `json:"headers,omitempty"`
	HeadersSet                   bool                      `json:"headers_set,omitempty"`
	HeaderEnv                    map[string]string         `json:"header_env,omitempty"`
	HeaderEnvSet                 bool                      `json:"header_env_set,omitempty"`
	BearerTokenEnvVar            string                    `json:"bearer_token_env_var,omitempty"`
	BearerTokenEnvVarSet         bool                      `json:"bearer_token_env_var_set,omitempty"`
	Auth                         HTTPAuth                  `json:"auth,omitempty"`
	AuthSet                      bool                      `json:"auth_set,omitempty"`
	AuthDefaulted                bool                      `json:"auth_defaulted,omitempty"`
	OAuthResource                *SensitiveValue           `json:"oauth_resource,omitempty"`
	OAuthScopes                  []string                  `json:"oauth_scopes,omitempty"`
	OAuthScopesSet               bool                      `json:"oauth_scopes_set,omitempty"`
	CodexOAuth                   *CodexOAuth               `json:"codex_oauth,omitempty"`
	ClaudeOAuth                  *ClaudeOAuth              `json:"claude_oauth,omitempty"`
	HeadersHelper                *SensitiveValue           `json:"headers_helper,omitempty"`
	OmitToolsFrom                []ToolExposureSurface     `json:"omit_tools_from,omitempty"`
	OmitToolsFromSet             bool                      `json:"omit_tools_from_set,omitempty"`
	SupportsParallelToolCalls    bool                      `json:"supports_parallel_tool_calls,omitempty"`
	SupportsParallelToolCallsSet bool                      `json:"supports_parallel_tool_calls_set,omitempty"`
	AlwaysLoad                   bool                      `json:"always_load,omitempty"`
	AlwaysLoadSet                bool                      `json:"always_load_set,omitempty"`
	EnablementSource             string                    `json:"enablement_source,omitempty"`
	NotConfigured                bool                      `json:"not_configured,omitempty"`

	Timeouts  Timeouts   `json:"timeouts,omitempty"`
	Tools     ToolFilter `json:"tools,omitempty"`
	Approvals Approvals  `json:"approvals,omitempty"`
}

func (s Server) String() string   { return redactedJSON(s, "<native MCP server redacted>") }
func (s Server) GoString() string { return s.String() }
func (s Server) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(s.String()))
}

type definitionMaterial struct {
	canonical []byte
}

const definitionRedacted = "<native MCP definition redacted>"

func (*definitionMaterial) String() string   { return definitionRedacted }
func (*definitionMaterial) GoString() string { return definitionRedacted }
func (*definitionMaterial) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(definitionRedacted))
}
func (*definitionMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(definitionRedacted)
}
func (*definitionMaterial) MarshalText() ([]byte, error) {
	return []byte(definitionRedacted), nil
}

// Severity is the disposition of a discovery diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic contains only source paths, entry names, and field names. Native
// configuration values are deliberately excluded so errors are safe to log.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Entry    string   `json:"entry,omitempty"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
}

// Result is deterministically ordered. A missing native file is not a
// diagnostic; most machines have only one or none of these files.
type Result struct {
	Servers     []Server     `json:"servers"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	Quarantines []Quarantine `json:"quarantines,omitempty"`

	authoritative map[string]Server
	precedence    map[string]int
	quarantine    []Quarantine
}

func (r Result) String() string   { return redactedJSON(r, "<native MCP discovery redacted>") }
func (r Result) GoString() string { return r.String() }
func (r Result) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(r.String()))
}

// Quarantine records a native layer that could not be safely interpreted.
// A server from the same dialect at this or a lower precedence cannot be
// materialized because the unreadable layer may have replaced it.
type Quarantine struct {
	Dialect    Dialect `json:"dialect"`
	Precedence int     `json:"precedence"`
	Path       string  `json:"path"`
	Code       string  `json:"code"`
}

func redactedJSON(value any, fallback string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fallback
	}
	return string(encoded)
}

func envReferences(value string) []string {
	seen := make(map[string]struct{})
	for i := 0; i+3 <= len(value); i++ {
		if value[i] != '$' || value[i+1] != '{' {
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			break
		}
		body := value[i+2 : i+2+end]
		if cut := strings.Index(body, ":-"); cut >= 0 {
			body = body[:cut]
		}
		if validEnvName(body) {
			seen[body] = struct{}{}
		}
		i += end + 2
	}
	refs := make([]string, 0, len(seen))
	for name := range seen {
		refs = append(refs, name)
	}
	sort.Strings(refs)
	return refs
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
