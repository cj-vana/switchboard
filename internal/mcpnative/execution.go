package mcpnative

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Feature names one semantic the eventual MCP runtime must implement before a
// normalized server using it may run. Materialize makes every non-baseline
// behavior explicit so integration code cannot silently discard native
// configuration.
type Feature string

const (
	FeatureCWD                 Feature = "stdio-cwd"
	FeatureForwardedEnv        Feature = "stdio-forwarded-env"
	FeatureHTTPHeaders         Feature = "http-static-headers"
	FeatureHTTPHeaderEnv       Feature = "http-env-headers"
	FeatureBearerEnv           Feature = "http-bearer-env"
	FeatureStartupTimeout      Feature = "codex-startup-timeout"
	FeatureToolTimeout         Feature = "codex-tool-timeout"
	FeatureClaudeTimeout       Feature = "claude-tool-timeout-ms"
	FeatureRequired            Feature = "required-startup"
	FeatureToolFilters         Feature = "tool-filters"
	FeatureApprovalModes       Feature = "tool-approval-modes"
	FeatureCodexOAuth          Feature = "codex-oauth-authentication"
	FeatureCodexChatGPT        Feature = "codex-chatgpt-authentication"
	FeatureClaudeOAuth         Feature = "claude-oauth-authentication"
	FeatureRemoteExecution     Feature = "remote-stdio-execution"
	FeatureSSE                 Feature = "legacy-sse"
	FeatureWebSocket           Feature = "websocket"
	FeatureClaudeExpansion     Feature = "claude-environment-expansion"
	FeatureCodexHeadersHelper  Feature = "codex-http-headers-helper"
	FeatureClaudeHeadersHelper Feature = "claude-headers-helper"
	FeatureAlwaysLoad          Feature = "always-load"
	FeatureToolExposure        Feature = "tool-exposure-surfaces"
	FeatureParallelTools       Feature = "parallel-tool-calls"
	FeaturePluginExpansion     Feature = "plugin-environment-expansion"
	FeatureImmediateTimeout    Feature = "explicit-zero-immediate-timeout"
)

var (
	ErrServerNotFound       = errors.New("native MCP server was not discovered")
	ErrInvalidServer        = errors.New("native MCP server has invalid or unsupported fields")
	ErrDisabled             = errors.New("native MCP server is disabled by its native configuration")
	ErrNotConfigured        = errors.New("native MCP remote server is a not-configured placeholder")
	ErrActivationRequired   = errors.New("native MCP server requires explicit Switchboard activation")
	ErrPolicyRequired       = errors.New("native MCP server requires an authoritative managed-policy decision")
	ErrPolicyDenied         = errors.New("native MCP server is denied by managed policy")
	ErrPolicyUnavailable    = errors.New("native MCP managed policy could not be evaluated")
	ErrInvalidPolicyMatcher = errors.New("native MCP managed-policy matcher is invalid")
	ErrPolicyExpansion      = errors.New("native MCP policy comparison requires controlled environment expansion")
	ErrInvalidDiscovery     = errors.New("native MCP discovery result has no authoritative definition")
	ErrInvalidActivationKey = errors.New("native MCP activation key must contain at least 32 bytes")
)

// ActivationRequest identifies one exact winning native entry without
// exposing its contents. An activation ledger should persist the identity
// returned by Identity using a stable random key stored in its own 0600 file.
// Changing any entry semantic, native layer, canonical config path, or trust
// root changes the keyed digest and invalidates the decision.
type ActivationRequest struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Dialect   Dialect `json:"dialect"`
	Scope     Scope   `json:"scope"`
	PluginID  string  `json:"plugin_id,omitempty"`
	RealPath  string  `json:"real_path"`
	TrustRoot string  `json:"trust_root,omitempty"`

	definition *definitionMaterial
}

func (r ActivationRequest) String() string {
	return redactedJSON(r, "<native MCP activation request redacted>")
}
func (r ActivationRequest) GoString() string { return r.String() }
func (r ActivationRequest) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(r.String()))
}

// ActivationIdentity is safe to persist. Digest is an HMAC rather than a raw
// hash so a logged identity is not an offline oracle for low-entropy secrets.
type ActivationIdentity struct {
	ID        string `json:"id"`
	RealPath  string `json:"real_path"`
	TrustRoot string `json:"trust_root,omitempty"`
	Digest    string `json:"digest"`
}

// Identity derives a stable, domain-separated activation identity. The key
// must be stable for the lifetime of the activation ledger and kept private.
func (r ActivationRequest) Identity(key []byte) (ActivationIdentity, error) {
	if len(key) < 32 {
		return ActivationIdentity{}, ErrInvalidActivationKey
	}
	if r.definition == nil || len(r.definition.canonical) == 0 {
		return ActivationIdentity{}, ErrInvalidDiscovery
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("switchboard/native-mcp-activation/v1\x00"))
	_, _ = mac.Write(r.definition.canonical)
	return ActivationIdentity{
		ID: r.ID, RealPath: r.RealPath, TrustRoot: r.TrustRoot,
		Digest: hex.EncodeToString(mac.Sum(nil)),
	}, nil
}

// ActivationChecker is implemented by Switchboard's separate, per-user
// activation ledger. It must compare the request's keyed identity to an
// explicit decision for this exact definition. Native enabled/trust state is
// never an activation decision.
type ActivationChecker interface {
	NativeMCPActivated(ActivationRequest) bool
}

// TrustChecker is the narrow trust capability Materialize needs. Direct
// config callers can pass *trust.Store. Plugin integration can instead pass an
// ephemeral checker backed by the exact digest-bound executable-trust decision
// from extensions.State, avoiding a second unrelated workspace trust grant.
type TrustChecker interface {
	Trusted(root string) bool
}

// PolicyRequest lets an authoritative managed-policy loader compare native
// identities without receiving or rendering their raw values.
type PolicyRequest struct {
	ID         string
	Name       string
	Dialect    Dialect
	Scope      Scope
	Source     Source
	RealPath   string
	PluginID   string
	PluginRoot string
	Transport  Transport

	command *SensitiveValue
	args    []SensitiveValue
	url     *SensitiveValue
}

func (r PolicyRequest) String() string {
	return redactedJSON(r, "<native MCP policy request redacted>")
}
func (r PolicyRequest) GoString() string { return r.String() }
func (r PolicyRequest) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(r.String()))
}

// CommandMatches, ArgsMatch, and URLMatches perform exact comparisons against
// managed allow/deny identities while keeping discovered values opaque. Use
// ArgsMatchPolicy and URLMatchesPolicy for Codex's exact/prefix/regex matcher
// tables, and URLMatchesClaudePattern for Claude's URL wildcard rules.
func (r PolicyRequest) CommandMatches(want string) bool {
	return r.command != nil && hmac.Equal([]byte(r.command.raw()), []byte(want))
}

func (r PolicyRequest) ArgsMatch(want []string) bool {
	if len(r.args) != len(want) {
		return false
	}
	for index := range want {
		if !hmac.Equal([]byte(r.args[index].raw()), []byte(want[index])) {
			return false
		}
	}
	return true
}

func (r PolicyRequest) URLMatches(want string) bool {
	return r.url != nil && hmac.Equal([]byte(r.url.raw()), []byte(want))
}

// PolicyChecker must merge and enforce every native policy source not parsed
// as a server declaration here: Codex requirements identities; Claude managed
// allow/deny rules; and Claude disabledMcpjsonServers/disabledMcpServers from
// all applicable settings layers. A true result means the entry is permitted
// and no native restriction denies it; nil/error/false all fail closed.
type PolicyChecker interface {
	NativeMCPAllowed(PolicyRequest) (bool, error)
}

// ActivationRequest returns the immutable request for one authoritative
// winner. The public Result.Servers slice is intentionally not trusted.
func (r Result) ActivationRequest(id string) (ActivationRequest, error) {
	server, err := r.activatableServer(id)
	if err != nil {
		return ActivationRequest{}, err
	}
	return activationRequest(server), nil
}

func activationRequest(server Server) ActivationRequest {
	return ActivationRequest{
		ID: server.ID, Name: server.Name, Dialect: server.Provenance.Dialect,
		Scope: server.Provenance.Scope, PluginID: server.Provenance.PluginID,
		RealPath:  server.Provenance.RealPath,
		TrustRoot: server.TrustRoot, definition: server.definition,
	}
}

// DiscoveryQuarantinedError reports a higher-precedence layer whose contents
// could not be determined. Running a surviving lower entry would fail open.
type DiscoveryQuarantinedError struct {
	Quarantine Quarantine
}

func (e *DiscoveryQuarantinedError) Error() string {
	return fmt.Sprintf("native MCP %s configuration is quarantined at precedence %d", e.Quarantine.Dialect, e.Quarantine.Precedence)
}

// TrustRequiredError says a project/local/plugin entry lacks Switchboard's own
// applicable trust decision. Codex and Claude trust state is never consulted.
type TrustRequiredError struct {
	Root string
}

func (e *TrustRequiredError) Error() string {
	return fmt.Sprintf("native MCP entry requires Switchboard execution trust for %s", e.Root)
}

// CompatibilityError lists semantics the selected runtime did not explicitly
// claim. The list is sorted and contains no configuration values.
type CompatibilityError struct {
	Missing []Feature
}

func (e *CompatibilityError) Error() string {
	return fmt.Sprintf("native MCP runtime does not implement required semantics: %v", e.Missing)
}

// MaterializedValue is an execution-time value released only by Materialize.
// Rendering remains redacted; Expose should be called only at the final spawn,
// request construction, or controlled Claude-expansion boundary.
type MaterializedValue struct {
	value string
}

// Expose returns the exact, still-unexpanded native value.
func (v MaterializedValue) Expose() string { return v.value }

func (v MaterializedValue) Empty() bool { return v.value == "" }

func (MaterializedValue) String() string   { return redacted }
func (MaterializedValue) GoString() string { return redacted }
func (MaterializedValue) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redacted))
}
func (MaterializedValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}
func (MaterializedValue) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MaterializedClaudeOAuth is Claude's public-client metadata after the gate.
// Client secrets remain in Claude's credential store and are never discovered.
type MaterializedClaudeOAuth struct {
	ClientID              *MaterializedValue
	CallbackPort          int
	CallbackPortSet       bool
	AuthServerMetadataURL *MaterializedValue
	Scopes                *MaterializedValue
}

type MaterializedCodexOAuth struct {
	ClientID        *MaterializedValue
	CallbackPort    int
	CallbackPortSet bool
}

// MaterializedServer is the complete normalized execution handoff. Sensitive
// fields still redact ordinary formatting and JSON. No instance can be made by
// this package without native validity, explicit activation, workspace trust
// where required, and runtime feature checks.
type MaterializedServer struct {
	ID                           string
	Name                         string
	Provenance                   Provenance
	Activation                   ActivationRequest
	Transport                    Transport
	ExecutionEnvironment         ExecutionEnvironment
	EnvironmentID                string
	EnvironmentIDSet             bool
	Command                      *MaterializedValue
	Args                         []MaterializedValue
	ArgsSet                      bool
	CWD                          *MaterializedValue
	Env                          map[string]MaterializedValue
	EnvSet                       bool
	ForwardedEnv                 []EnvVar
	ForwardedEnvSet              bool
	URL                          *MaterializedValue
	Headers                      map[string]MaterializedValue
	HeadersSet                   bool
	HeaderEnv                    map[string]string
	HeaderEnvSet                 bool
	BearerTokenEnvVar            string
	BearerTokenEnvVarSet         bool
	Auth                         HTTPAuth
	AuthSet                      bool
	AuthDefaulted                bool
	OAuthResource                *MaterializedValue
	OAuthScopes                  []string
	OAuthScopesSet               bool
	CodexOAuth                   *MaterializedCodexOAuth
	ClaudeOAuth                  *MaterializedClaudeOAuth
	HeadersHelper                *MaterializedValue
	AlwaysLoad                   bool
	AlwaysLoadSet                bool
	OmitToolsFrom                []ToolExposureSurface
	OmitToolsFromSet             bool
	SupportsParallelToolCalls    bool
	SupportsParallelToolCallsSet bool
	Timeouts                     Timeouts
	Tools                        ToolFilter
	Approvals                    Approvals
	Required                     bool
	RequiredSet                  bool
}

func (s MaterializedServer) String() string {
	return redactedJSON(s, "<native MCP materialized server redacted>")
}
func (s MaterializedServer) GoString() string { return s.String() }
func (s MaterializedServer) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(s.String()))
}

// RequiredFeatures reports the exact non-baseline semantics this entry needs.
// Baseline means stdio command/ordered args/static env or Streamable HTTP URL.
func (s Server) RequiredFeatures() []Feature {
	return requiredFeatures(s)
}

func requiredFeatures(s Server) []Feature {
	needed := make(map[Feature]struct{})
	if s.CWD != nil {
		needed[FeatureCWD] = struct{}{}
	}
	if len(s.ForwardedEnv) > 0 {
		needed[FeatureForwardedEnv] = struct{}{}
	}
	if len(s.Headers) > 0 {
		needed[FeatureHTTPHeaders] = struct{}{}
	}
	if len(s.HeaderEnv) > 0 {
		needed[FeatureHTTPHeaderEnv] = struct{}{}
	}
	if s.BearerTokenEnvVar != "" {
		needed[FeatureBearerEnv] = struct{}{}
	}
	if s.Timeouts.StartupSet || s.Timeouts.StartupMillisSet {
		needed[FeatureStartupTimeout] = struct{}{}
	}
	if s.Timeouts.ToolSet {
		needed[FeatureToolTimeout] = struct{}{}
	}
	if s.Timeouts.ClaudeToolSet {
		needed[FeatureClaudeTimeout] = struct{}{}
	}
	if s.Timeouts.StartupSet && s.Timeouts.StartupSeconds == 0 ||
		!s.Timeouts.StartupSet && s.Timeouts.StartupMillisSet && s.Timeouts.StartupMilliseconds == 0 ||
		s.Timeouts.ToolSet && s.Timeouts.ToolSeconds == 0 {
		// Codex treats an explicit zero as an immediate deadline. A runtime
		// whose zero duration means "unset" must not claim this feature.
		needed[FeatureImmediateTimeout] = struct{}{}
	}
	if s.Required {
		needed[FeatureRequired] = struct{}{}
	}
	if s.Tools.EnabledSet || s.Tools.DisabledSet {
		needed[FeatureToolFilters] = struct{}{}
	}
	if s.Approvals.DefaultSet || len(s.Approvals.Tools) > 0 {
		needed[FeatureApprovalModes] = struct{}{}
	}
	if s.Provenance.Dialect == DialectCodex && isRemoteTransport(s.Transport) ||
		s.AuthSet || s.OAuthResource != nil || s.OAuthScopesSet || s.CodexOAuth != nil {
		if s.Auth == HTTPAuthChatGPT {
			// Native ChatGPT auth falls back through stored OAuth before an
			// unauthenticated request. Claiming only the primary flow would
			// silently change connection behavior.
			needed[FeatureCodexChatGPT] = struct{}{}
			needed[FeatureCodexOAuth] = struct{}{}
		} else {
			needed[FeatureCodexOAuth] = struct{}{}
		}
	}
	if s.ClaudeOAuth != nil {
		needed[FeatureClaudeOAuth] = struct{}{}
	}
	if s.ExecutionEnvironment == ExecutionEnvironmentRemote || hasRemoteEnv(s.ForwardedEnv) ||
		s.EnvironmentIDSet && s.EnvironmentID != "local" {
		needed[FeatureRemoteExecution] = struct{}{}
	}
	if s.Transport == TransportSSE {
		needed[FeatureSSE] = struct{}{}
	}
	if s.Transport == TransportWS {
		needed[FeatureWebSocket] = struct{}{}
	}
	if s.Provenance.Dialect == DialectClaude && serverHasEnvReferences(s) {
		needed[FeatureClaudeExpansion] = struct{}{}
	}
	if s.Provenance.Scope == ScopePlugin && s.Provenance.Dialect == DialectClaude && serverHasPluginContextReferences(s) {
		needed[FeaturePluginExpansion] = struct{}{}
	}
	if s.HeadersHelper != nil {
		if s.Provenance.Dialect == DialectCodex {
			needed[FeatureCodexHeadersHelper] = struct{}{}
		} else {
			needed[FeatureClaudeHeadersHelper] = struct{}{}
		}
	}
	if s.OmitToolsFromSet {
		needed[FeatureToolExposure] = struct{}{}
	}
	if s.SupportsParallelToolCalls {
		needed[FeatureParallelTools] = struct{}{}
	}
	if s.AlwaysLoad {
		needed[FeatureAlwaysLoad] = struct{}{}
	}
	features := make([]Feature, 0, len(needed))
	for feature := range needed {
		features = append(features, feature)
	}
	sort.Slice(features, func(i, j int) bool { return features[i] < features[j] })
	return features
}

// Materialize is the fail-closed integration seam. Native Enabled is only a
// constraint: activations must separately approve every native entry,
// including managed entries. Project/local/plugin entries also need a concrete
// Switchboard trust capability for their bound trust root. Runtime support is
// explicit.
func (r Result) Materialize(id string, trustStore TrustChecker, policy PolicyChecker, activations ActivationChecker, implemented ...Feature) (MaterializedServer, error) {
	server, err := r.activatableServer(id)
	if err != nil {
		return MaterializedServer{}, err
	}
	if policy == nil {
		return MaterializedServer{}, ErrPolicyRequired
	}
	allowed, policyErr := policy.NativeMCPAllowed(policyRequest(server))
	if policyErr != nil {
		return MaterializedServer{}, ErrPolicyUnavailable
	}
	if !allowed {
		return MaterializedServer{}, ErrPolicyDenied
	}
	request := activationRequest(server)
	if activations == nil || !activations.NativeMCPActivated(request) {
		return MaterializedServer{}, ErrActivationRequired
	}
	if server.ExecutionTrustRequired && (trustStore == nil || !trustStore.Trusted(server.TrustRoot)) {
		return MaterializedServer{}, &TrustRequiredError{Root: server.TrustRoot}
	}
	have := make(map[Feature]struct{}, len(implemented))
	for _, feature := range implemented {
		have[feature] = struct{}{}
	}
	var missing []Feature
	for _, feature := range requiredFeatures(server) {
		if _, ok := have[feature]; !ok {
			missing = append(missing, feature)
		}
	}
	if len(missing) > 0 {
		return MaterializedServer{}, &CompatibilityError{Missing: missing}
	}
	return materializeServer(server, request), nil
}

func policyRequest(server Server) PolicyRequest {
	return PolicyRequest{
		ID: server.ID, Name: server.Name, Dialect: server.Provenance.Dialect,
		Scope: server.Provenance.Scope, Source: server.Provenance.Source,
		RealPath: server.Provenance.RealPath, PluginID: server.Provenance.PluginID,
		PluginRoot: server.Provenance.PluginRoot, Transport: server.Transport,
		command: cloneSensitivePointer(server.Command),
		args:    append([]SensitiveValue(nil), server.Args...),
		url:     cloneSensitivePointer(server.URL),
	}
}

func (r Result) activatableServer(id string) (Server, error) {
	server, ok := r.authoritative[id]
	if !ok {
		if r.authoritative == nil {
			return Server{}, ErrInvalidDiscovery
		}
		return Server{}, ErrServerNotFound
	}
	precedence := r.precedence[id]
	for _, quarantine := range r.quarantine {
		if quarantine.Dialect != server.Provenance.Dialect {
			continue
		}
		// Any unknown Codex layer can contain fields that recursively merge into
		// an otherwise valid effective entry, including enabled=false, filters,
		// and authentication policy. No Codex winner is executable until the
		// complete participating layer stack is readable and structurally valid.
		if server.Provenance.Dialect == DialectCodex || quarantine.Precedence >= precedence {
			return Server{}, &DiscoveryQuarantinedError{Quarantine: quarantine}
		}
	}
	if !server.Supported || server.definition == nil || len(server.definition.canonical) == 0 {
		return Server{}, ErrInvalidServer
	}
	if !server.Enabled {
		return Server{}, ErrDisabled
	}
	if server.NotConfigured {
		return Server{}, ErrNotConfigured
	}
	return server, nil
}

func materializeServer(s Server, request ActivationRequest) MaterializedServer {
	timeouts := s.Timeouts
	provenance := s.Provenance
	provenance.ContributingLayers = append([]LayerProvenance(nil), s.Provenance.ContributingLayers...)
	if timeouts.StartupSet {
		// Native Codex gives seconds precedence when both aliases are present.
		timeouts.StartupMilliseconds = 0
		timeouts.StartupMillisSet = false
	}
	result := MaterializedServer{
		ID: s.ID, Name: s.Name, Provenance: provenance, Activation: request,
		Transport: s.Transport, ExecutionEnvironment: s.ExecutionEnvironment,
		EnvironmentID: s.EnvironmentID, EnvironmentIDSet: s.EnvironmentIDSet,
		Command: materializePointer(s.Command), Args: materializeSlice(s.Args), ArgsSet: s.ArgsSet,
		CWD: materializePointer(s.CWD), Env: materializeMap(s.Env), EnvSet: s.EnvSet,
		ForwardedEnv: append([]EnvVar(nil), s.ForwardedEnv...), ForwardedEnvSet: s.ForwardedEnvSet,
		URL: materializePointer(s.URL), Headers: materializeMap(s.Headers),
		HeadersSet: s.HeadersSet, HeaderEnv: cloneStringMap(s.HeaderEnv), HeaderEnvSet: s.HeaderEnvSet,
		BearerTokenEnvVar: s.BearerTokenEnvVar, BearerTokenEnvVarSet: s.BearerTokenEnvVarSet,
		Auth: s.Auth, AuthSet: s.AuthSet, AuthDefaulted: s.AuthDefaulted,
		OAuthResource: materializePointer(s.OAuthResource),
		OAuthScopes:   append([]string(nil), s.OAuthScopes...), OAuthScopesSet: s.OAuthScopesSet,
		HeadersHelper: materializePointer(s.HeadersHelper), AlwaysLoad: s.AlwaysLoad,
		AlwaysLoadSet:                s.AlwaysLoadSet,
		OmitToolsFrom:                append([]ToolExposureSurface(nil), s.OmitToolsFrom...),
		OmitToolsFromSet:             s.OmitToolsFromSet,
		SupportsParallelToolCalls:    s.SupportsParallelToolCalls,
		SupportsParallelToolCallsSet: s.SupportsParallelToolCallsSet,
		Timeouts:                     timeouts,
		Tools:                        cloneToolFilter(s.Tools), Approvals: cloneApprovals(s.Approvals),
		Required: s.Required, RequiredSet: s.RequiredSet,
	}
	if s.CodexOAuth != nil {
		result.CodexOAuth = &MaterializedCodexOAuth{
			ClientID:        materializePointer(s.CodexOAuth.ClientID),
			CallbackPort:    s.CodexOAuth.CallbackPort,
			CallbackPortSet: s.CodexOAuth.CallbackPortSet,
		}
	}
	if s.ClaudeOAuth != nil {
		result.ClaudeOAuth = &MaterializedClaudeOAuth{
			ClientID:              materializePointer(s.ClaudeOAuth.ClientID),
			CallbackPort:          s.ClaudeOAuth.CallbackPort,
			CallbackPortSet:       s.ClaudeOAuth.CallbackPortSet,
			AuthServerMetadataURL: materializePointer(s.ClaudeOAuth.AuthServerMetadataURL),
			Scopes:                materializePointer(s.ClaudeOAuth.Scopes),
		}
	}
	return result
}

func materializePointer(value *SensitiveValue) *MaterializedValue {
	if value == nil {
		return nil
	}
	result := MaterializedValue{value: value.raw()}
	return &result
}

func materializeSlice(values []SensitiveValue) []MaterializedValue {
	if values == nil {
		return nil
	}
	result := make([]MaterializedValue, len(values))
	for i, value := range values {
		result[i] = MaterializedValue{value: value.raw()}
	}
	return result
}

func materializeMap(values map[string]SensitiveValue) map[string]MaterializedValue {
	if values == nil {
		return nil
	}
	result := make(map[string]MaterializedValue, len(values))
	for key, value := range values {
		result[key] = MaterializedValue{value: value.raw()}
	}
	return result
}

func hasRemoteEnv(values []EnvVar) bool {
	for _, value := range values {
		if value.Source == EnvSourceRemote {
			return true
		}
	}
	return false
}

func isRemoteTransport(transport Transport) bool {
	return transport == TransportHTTP || transport == TransportSSE || transport == TransportWS
}

func serverHasEnvReferences(server Server) bool {
	for _, value := range serverSensitiveValues(server) {
		if len(value.EnvReferences()) > 0 {
			return true
		}
	}
	return false
}

func serverHasPluginContextReferences(server Server) bool {
	for _, value := range serverSensitiveValues(server) {
		if strings.Contains(value.raw(), "${CLAUDE_PLUGIN_DATA") ||
			strings.Contains(value.raw(), "${CLAUDE_PROJECT_DIR") ||
			strings.Contains(value.raw(), "${user_config.") {
			return true
		}
	}
	return false
}

func serverSensitiveValues(server Server) []SensitiveValue {
	values := make([]SensitiveValue, 0, 6+len(server.Args)+len(server.Env)+len(server.Headers))
	for _, value := range []*SensitiveValue{
		server.Command, server.CWD, server.URL, server.OAuthResource, server.HeadersHelper,
	} {
		if value != nil {
			values = append(values, *value)
		}
	}
	if server.ClaudeOAuth != nil {
		for _, value := range []*SensitiveValue{
			server.ClaudeOAuth.ClientID, server.ClaudeOAuth.AuthServerMetadataURL, server.ClaudeOAuth.Scopes,
		} {
			if value != nil {
				values = append(values, *value)
			}
		}
	}
	if server.CodexOAuth != nil && server.CodexOAuth.ClientID != nil {
		values = append(values, *server.CodexOAuth.ClientID)
	}
	values = append(values, server.Args...)
	for _, value := range server.Env {
		values = append(values, value)
	}
	for _, value := range server.Headers {
		values = append(values, value)
	}
	return values
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneToolFilter(value ToolFilter) ToolFilter {
	value.Enabled = append([]string(nil), value.Enabled...)
	value.Disabled = append([]string(nil), value.Disabled...)
	return value
}

func cloneApprovals(value Approvals) Approvals {
	if value.Tools == nil {
		return value
	}
	tools := make(map[string]ApprovalMode, len(value.Tools))
	for name, mode := range value.Tools {
		tools[name] = mode
	}
	value.Tools = tools
	return value
}
