package mcpnative

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

var aliases = map[string]string{
	"type":                         "type",
	"name":                         "display_name",
	"command":                      "command",
	"args":                         "args",
	"cwd":                          "cwd",
	"env":                          "env",
	"env_vars":                     "env_vars",
	"envVars":                      "env_vars",
	"environment_id":               "environment_id",
	"url":                          "url",
	"headers":                      "headers",
	"http_headers":                 "headers",
	"httpHeaders":                  "headers",
	"env_http_headers":             "header_env",
	"envHttpHeaders":               "header_env",
	"bearer_token_env_var":         "bearer_env",
	"bearerTokenEnvVar":            "bearer_env",
	"auth":                         "auth",
	"oauth_resource":               "oauth_resource",
	"scopes":                       "oauth_scopes",
	"startup_timeout_ms":           "startup_timeout_ms",
	"startup_timeout_sec":          "startup_timeout",
	"startupTimeoutSec":            "startup_timeout",
	"tool_timeout_sec":             "tool_timeout",
	"toolTimeoutSec":               "tool_timeout",
	"enabled":                      "enabled",
	"disabled":                     "disabled",
	"required":                     "required",
	"enabled_tools":                "enabled_tools",
	"enabledTools":                 "enabled_tools",
	"disabled_tools":               "disabled_tools",
	"disabledTools":                "disabled_tools",
	"default_tools_approval_mode":  "default_approval",
	"defaultToolsApprovalMode":     "default_approval",
	"tools":                        "tool_approvals",
	"timeout":                      "claude_timeout",
	"alwaysLoad":                   "always_load",
	"oauth":                        "oauth",
	"headersHelper":                "headers_helper",
	"http_headers_helper":          "headers_helper",
	"omit_tools_from":              "omit_tools_from",
	"supports_parallel_tool_calls": "supports_parallel_tool_calls",
}

var codexFields = stringSet(
	"name", "command", "args", "cwd", "env", "env_vars", "environment_id",
	"url", "auth", "bearer_token_env_var", "http_headers", "env_http_headers",
	"oauth", "oauth_resource", "scopes", "http_headers_helper", "omit_tools_from",
	"supports_parallel_tool_calls", "startup_timeout_ms", "startup_timeout_sec",
	"tool_timeout_sec", "enabled", "required",
	"enabled_tools", "disabled_tools", "default_tools_approval_mode", "tools",
)

var claudeFields = stringSet(
	"type", "command", "args", "env", "url", "headers",
	"timeout", "alwaysLoad", "oauth", "headersHelper",
)

type entryParser struct {
	server      Server
	diagnostics []Diagnostic
	seen        map[string]string
	disabled    *bool
}

func parseEntry(name string, raw map[string]any, provenance Provenance, trustRoot string) (Server, []Diagnostic) {
	p := entryParser{
		server: Server{
			ID:                     normalizedServerID(provenance, name),
			Name:                   name,
			Provenance:             provenance,
			ExecutionTrustRequired: requiresWorkspaceTrust(provenance.Scope),
			TrustRoot:              trustRoot,
			Supported:              true,
			Enabled:                true,
		},
		seen: make(map[string]string),
	}
	if len(raw) > MaxEntryValues {
		p.problem("entry-value-budget-exceeded", "", "server entry exceeds the bounded field limit")
		return p.server, p.diagnostics
	}
	definition, definitionErr := canonicalDefinition(provenance, trustRoot, raw)
	p.server.definition = definition
	if definitionErr != nil {
		p.problem("unhashable-entry", "", "server entry cannot be canonicalized for activation")
	}
	if provenance.Scope == ScopeUser {
		p.server.TrustRoot = ""
	}
	if err := validDisplayName(name); err != nil {
		p.problem("invalid-name", "", "server name is empty or contains a control character")
	}
	if provenance.Dialect == DialectClaude && reservedClaudeName(name) {
		p.problem("reserved-name", "", "server name is reserved by Claude Code")
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		canonical, ok := aliases[key]
		if !ok || !supportedNativeField(provenance.Dialect, key) {
			p.unsupported(key)
			continue
		}
		if first, duplicate := p.seen[canonical]; duplicate {
			p.problem("conflicting-fields", key, fmt.Sprintf("fields %s and %s express the same setting", safeToken(first), safeToken(key)))
			continue
		}
		p.seen[canonical] = key
		p.field(canonical, key, raw[key])
	}
	p.finish()
	return p.server, p.diagnostics
}

func requiresWorkspaceTrust(scope Scope) bool {
	return scope == ScopeProject || scope == ScopeLocal || scope == ScopePlugin
}

func normalizedServerID(provenance Provenance, name string) string {
	if provenance.Scope == ScopePlugin {
		id, _ := PluginServerID(provenance.Dialect, provenance.PluginID, name)
		return id
	}
	return string(provenance.Dialect) + ":" + encodeServerIDComponent(name)
}

// PluginServerID returns the collision-free public ID ParsePluginMCP assigns
// to a native plugin server. PluginID must be the exact native plugin identity,
// not a cache directory name or display label. Callers should prefer the
// structured fields on ActivationRequest for authorization checks rather than
// parsing or prefix-matching the returned string.
func PluginServerID(dialect Dialect, pluginID, name string) (string, error) {
	if dialect != DialectCodex && dialect != DialectClaude {
		return "", fmt.Errorf("unsupported native MCP dialect")
	}
	if validDisplayName(pluginID) != nil || validDisplayName(name) != nil {
		return "", fmt.Errorf("native plugin and server IDs must be non-empty and contain no control characters")
	}
	return string(dialect) + ":plugin:" + encodeServerIDComponent(pluginID) + ":" + encodeServerIDComponent(name), nil
}

func encodeServerIDComponent(value string) string {
	value = strings.ReplaceAll(value, "%", "%25")
	return strings.ReplaceAll(value, ":", "%3A")
}

func (p *entryParser) field(canonical, original string, value any) {
	switch canonical {
	case "display_name":
		name := p.stringField(original, value)
		if validDisplayName(name) != nil {
			p.invalid(original, "must be a non-empty display name without control characters")
			return
		}
		p.server.DisplayName = name
	case "type":
		value, ok := asString(value)
		if !ok {
			p.invalid(original, "must be a string")
			return
		}
		switch value {
		case "stdio":
			p.server.Transport = TransportStdio
		case "http", "streamable-http":
			p.server.Transport = TransportHTTP
		case "sse":
			p.server.Transport = TransportSSE
		case "ws":
			p.server.Transport = TransportWS
		default:
			p.invalid(original, "names an unsupported transport")
		}

	case "command":
		p.server.Command = p.sensitiveStringField(original, value)
	case "args":
		p.server.Args = p.sensitiveListField(original, value)
		p.server.ArgsSet = true
	case "cwd":
		p.server.CWD = p.sensitiveStringField(original, value)
	case "env":
		p.server.Env = p.envValueMapField(original, value)
		p.server.EnvSet = true
	case "env_vars":
		p.server.ForwardedEnv = p.envVarsField(original, value)
		p.server.ForwardedEnvSet = true
	case "execution_environment":
		v, ok := asString(value)
		if !ok {
			p.invalid(original, "must be local or remote")
			return
		}
		switch ExecutionEnvironment(v) {
		case ExecutionEnvironmentLocal, ExecutionEnvironmentRemote:
			p.server.ExecutionEnvironment = ExecutionEnvironment(v)
		default:
			p.invalid(original, "must be local or remote")
		}
	case "environment_id":
		environmentID, ok := asString(value)
		if !ok || strings.TrimSpace(environmentID) == "" || hasControl(environmentID) {
			p.invalid(original, "must be a non-empty string")
			return
		}
		p.server.EnvironmentID = environmentID
		p.server.EnvironmentIDSet = true
	case "url":
		p.server.URL = p.sensitiveStringField(original, value)
	case "headers":
		p.server.Headers = p.headerValueMapField(original, value)
		p.server.HeadersSet = true
	case "header_env":
		p.server.HeaderEnv = p.headerEnvMapField(original, value)
		p.server.HeaderEnvSet = true
	case "bearer_env":
		p.server.BearerTokenEnvVar = p.envNameField(original, value)
		p.server.BearerTokenEnvVarSet = true
	case "auth":
		v, ok := asString(value)
		if !ok {
			p.invalid(original, "must be oauth or chatgpt")
			return
		}
		switch HTTPAuth(v) {
		case HTTPAuthOAuth, HTTPAuthChatGPT:
			p.server.Auth = HTTPAuth(v)
			p.server.AuthSet = true
		default:
			p.invalid(original, "must be oauth or chatgpt")
		}
	case "oauth_resource":
		p.server.OAuthResource = p.sensitiveStringField(original, value)
	case "oauth_scopes":
		p.server.OAuthScopes = p.listField(original, value)
		p.server.OAuthScopesSet = true
	case "oauth":
		if p.server.Provenance.Dialect == DialectClaude {
			p.server.ClaudeOAuth = p.claudeOAuthField(original, value)
		} else {
			p.server.CodexOAuth = p.codexOAuthField(original, value)
		}
	case "startup_timeout":
		seconds, ok := asCodexDurationSeconds(value)
		if !ok {
			p.invalid(original, "must be a non-negative finite duration representable by the runtime")
			return
		}
		p.server.Timeouts.StartupSeconds = seconds
		p.server.Timeouts.StartupSet = true
	case "startup_timeout_ms":
		milliseconds, ok := asNonnegativeRuntimeMilliseconds(value)
		if !ok {
			p.invalid(original, "must be a non-negative integer number of milliseconds representable by the runtime")
			return
		}
		p.server.Timeouts.StartupMilliseconds = milliseconds
		p.server.Timeouts.StartupMillisSet = true
	case "tool_timeout":
		seconds, ok := asCodexDurationSeconds(value)
		if !ok {
			p.invalid(original, "must be a non-negative finite duration representable by the runtime")
			return
		}
		p.server.Timeouts.ToolSeconds = seconds
		p.server.Timeouts.ToolSet = true
	case "enabled":
		v, ok := value.(bool)
		if !ok {
			p.invalid(original, "must be a boolean")
			return
		}
		p.server.Enabled = v
		p.server.EnabledSet = true
	case "disabled":
		v, ok := value.(bool)
		if !ok {
			p.invalid(original, "must be a boolean")
			return
		}
		p.disabled = &v
	case "required":
		v, ok := value.(bool)
		if !ok {
			p.invalid(original, "must be a boolean")
			return
		}
		p.server.Required = v
		p.server.RequiredSet = true
	case "enabled_tools":
		p.server.Tools.Enabled = p.listField(original, value)
		p.server.Tools.EnabledSet = true
	case "disabled_tools":
		p.server.Tools.Disabled = p.listField(original, value)
		p.server.Tools.DisabledSet = true
	case "default_approval":
		mode, ok := approvalMode(value)
		if !ok {
			p.invalid(original, "must be auto, prompt, writes, or approve")
			return
		}
		p.server.Approvals.Default = mode
		p.server.Approvals.DefaultSet = true
	case "tool_approvals":
		p.server.Approvals.Tools = p.toolApprovalsField(original, value)
	case "claude_timeout":
		milliseconds, ok := asPositiveRuntimeMilliseconds(value)
		if !ok {
			p.invalid(original, "must be a positive integer number of milliseconds representable by the runtime")
			return
		}
		p.server.Timeouts.ClaudeToolMillis = milliseconds
		p.server.Timeouts.ClaudeToolSet = true
	case "always_load":
		v, ok := value.(bool)
		if !ok {
			p.invalid(original, "must be a boolean")
			return
		}
		p.server.AlwaysLoad = v
		p.server.AlwaysLoadSet = true
	case "headers_helper":
		p.server.HeadersHelper = p.nonemptySensitiveStringField(original, value)
	case "omit_tools_from":
		p.server.OmitToolsFrom = p.toolExposureField(original, value)
		p.server.OmitToolsFromSet = true
	case "supports_parallel_tool_calls":
		v, ok := value.(bool)
		if !ok {
			p.invalid(original, "must be a boolean")
			return
		}
		p.server.SupportsParallelToolCalls = v
		p.server.SupportsParallelToolCallsSet = true
	}
}

func (p *entryParser) finish() {
	if p.disabled != nil {
		if _, hasEnabled := p.seen["enabled"]; hasEnabled {
			p.problem("conflicting-fields", "disabled", "enabled and disabled cannot both be declared")
		} else {
			p.server.Enabled = !*p.disabled
			p.server.EnabledSet = true
		}
	}

	if p.server.Transport == "" {
		hasCommand := p.server.Command != nil && strings.TrimSpace(p.server.Command.raw()) != ""
		hasURL := p.server.URL != nil && !p.server.URL.Empty()
		if p.server.Provenance.Dialect == DialectClaude && hasURL {
			p.problem("missing-type", "type", "Claude remote entries require an explicit type")
			if !hasCommand {
				p.server.Transport = TransportHTTP
			}
		}
		switch {
		case p.server.Transport != "":
		case hasCommand && !hasURL:
			p.server.Transport = TransportStdio
		case hasURL && !hasCommand:
			p.server.Transport = TransportHTTP
		case hasCommand && hasURL:
			p.problem("ambiguous-transport", "", "entry declares both command and url")
		default:
			p.problem("missing-transport", "", "entry declares neither a command nor a url")
		}
	}
	if p.server.Provenance.Dialect == DialectCodex && !p.server.AuthSet {
		p.server.Auth = HTTPAuthOAuth
		p.server.AuthDefaulted = true
	}
	if p.server.Provenance.Dialect == DialectCodex && !p.server.EnvironmentIDSet {
		// Codex's effective default is the local executor. Keep the presence bit
		// false while making the complete execution handoff self-contained.
		p.server.EnvironmentID = "local"
	}
	if p.server.HeadersHelper != nil {
		remoteLegacy := p.server.ExecutionEnvironment == ExecutionEnvironmentRemote
		remoteEnvironment := p.server.EnvironmentIDSet && p.server.EnvironmentID != "local"
		if remoteLegacy || remoteEnvironment {
			p.problem("invalid-field", "http_headers_helper", "HTTP headers helper is supported only in the local execution environment")
		}
	}
	if hasRemoteEnv(p.server.ForwardedEnv) &&
		p.server.ExecutionEnvironment != ExecutionEnvironmentRemote &&
		(!p.server.EnvironmentIDSet || p.server.EnvironmentID == "local") {
		p.problem("invalid-field", "env_vars", "remote-source environment variables require a remote MCP stdio execution environment")
	}

	switch p.server.Transport {
	case TransportStdio:
		if p.server.Command == nil || strings.TrimSpace(p.server.Command.raw()) == "" {
			p.problem("missing-command", "command", "stdio entry has no command")
		}
		if p.server.URL != nil || p.server.HeadersSet || p.server.HeaderEnvSet || p.server.BearerTokenEnvVarSet ||
			p.server.AuthSet || p.server.OAuthResource != nil ||
			p.server.CodexOAuth != nil || p.server.ClaudeOAuth != nil || p.server.HeadersHelper != nil {
			p.problem("mixed-transport-fields", "", "stdio entry also declares HTTP-only fields")
		}
	case TransportHTTP, TransportSSE:
		if p.server.URL == nil || p.server.URL.Empty() && p.server.Provenance.Dialect != DialectClaude {
			p.problem("missing-url", "url", "remote entry has no URL")
		} else if p.server.URL != nil && p.server.URL.Empty() {
			p.server.NotConfigured = true
		} else if p.server.URL != nil && !validTransportURL(p.server.URL.raw(), p.server.Transport, p.server.Provenance.Dialect == DialectClaude) {
			p.problem("invalid-url", "url", "remote URL must be an absolute HTTP(S) URL without control or whitespace characters")
		}
		if p.server.Command != nil || p.server.ArgsSet || p.server.CWD != nil || p.server.EnvSet || p.server.ForwardedEnvSet || p.server.ExecutionEnvironment != "" {
			p.problem("mixed-transport-fields", "", "remote entry also declares stdio-only fields")
		}
	case TransportWS:
		if p.server.URL == nil || p.server.URL.Empty() && p.server.Provenance.Dialect != DialectClaude {
			p.problem("missing-url", "url", "WebSocket entry has no URL")
		} else if p.server.URL != nil && p.server.URL.Empty() {
			p.server.NotConfigured = true
		} else if p.server.URL != nil && !validTransportURL(p.server.URL.raw(), p.server.Transport, p.server.Provenance.Dialect == DialectClaude) {
			p.problem("invalid-url", "url", "WebSocket URL must be an absolute WS(S) URL without control or whitespace characters")
		}
		if p.server.Command != nil || p.server.ArgsSet || p.server.CWD != nil || p.server.EnvSet || p.server.ForwardedEnvSet || p.server.ExecutionEnvironment != "" ||
			p.server.AuthSet || p.server.OAuthResource != nil || p.server.OAuthScopesSet || p.server.CodexOAuth != nil || p.server.ClaudeOAuth != nil {
			p.problem("mixed-transport-fields", "", "WebSocket entry declares a non-WebSocket field")
		}
	}

	p.server.ForwardedEnv = sortedUniqueEnvVars(p.server.ForwardedEnv)
	p.server.Tools.Enabled = sortedUnique(p.server.Tools.Enabled)
	p.server.Tools.Disabled = sortedUnique(p.server.Tools.Disabled)
	p.server.OAuthScopes = sortedUnique(p.server.OAuthScopes)
	p.server.UnsupportedFields = sortedUnique(p.server.UnsupportedFields)
}

func (p *entryParser) stringField(field string, value any) string {
	v, ok := asString(value)
	if !ok {
		p.invalid(field, "must be a string")
		return ""
	}
	if strings.IndexByte(v, 0) >= 0 {
		p.invalid(field, "contains a NUL byte")
		return ""
	}
	return v
}

func (p *entryParser) sensitiveStringField(field string, value any) *SensitiveValue {
	v := p.stringField(field, value)
	if v == "" {
		if raw, ok := asString(value); !ok || raw != "" {
			return nil
		}
	}
	s := sensitive(v)
	return &s
}

func (p *entryParser) nonemptySensitiveStringField(field string, value any) *SensitiveValue {
	raw, ok := asString(value)
	if !ok || strings.TrimSpace(raw) == "" || strings.IndexByte(raw, 0) >= 0 {
		p.invalid(field, "must be a non-empty string")
		return nil
	}
	v := sensitive(raw)
	return &v
}

func (p *entryParser) sensitiveListField(field string, value any) []SensitiveValue {
	values := p.listField(field, value)
	if values == nil {
		return nil
	}
	result := make([]SensitiveValue, 0, len(values))
	for _, value := range values {
		result = append(result, sensitive(value))
	}
	return result
}

func (p *entryParser) listField(field string, value any) []string {
	values, ok := asStringList(value)
	if !ok {
		p.invalid(field, "must be an array of strings")
		return nil
	}
	if len(values) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	for _, value := range values {
		if strings.IndexByte(value, 0) >= 0 {
			p.invalid(field, "contains an item with a NUL byte")
			return nil
		}
	}
	return values
}

func (p *entryParser) envVarsField(field string, value any) []EnvVar {
	items, ok := asAnyList(value)
	if !ok {
		p.invalid(field, "must be an array of variable names or {name, source} objects")
		return nil
	}
	if len(items) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	result := make([]EnvVar, 0, len(items))
	for _, item := range items {
		if name, ok := asString(item); ok {
			if !validEnvName(name) {
				p.invalid(field, "contains an invalid environment variable name")
				return nil
			}
			result = append(result, EnvVar{Name: name, Source: EnvSourceLocal})
			continue
		}
		object, ok := asMap(item)
		if !ok {
			p.invalid(field, "must contain only variable names or {name, source} objects")
			return nil
		}
		for _, key := range sortedKeys(object) {
			if key != "name" && key != "source" {
				p.invalid(field, "contains an object with an unsupported field")
				return nil
			}
		}
		name, nameOK := asString(object["name"])
		if !nameOK || !validEnvName(name) {
			p.invalid(field, "contains an object without a valid name")
			return nil
		}
		source := EnvSourceLocal
		if rawSource, exists := object["source"]; exists {
			sourceName, sourceOK := asString(rawSource)
			if !sourceOK {
				p.invalid(field, "contains an object whose source is not a string")
				return nil
			}
			source = EnvSource(sourceName)
		}
		if source != EnvSourceLocal && source != EnvSourceRemote {
			p.invalid(field, "contains an object whose source is not local or remote")
			return nil
		}
		result = append(result, EnvVar{Name: name, Source: source})
	}
	return result
}

func (p *entryParser) toolApprovalsField(field string, value any) map[string]ApprovalMode {
	tools, ok := asMap(value)
	if !ok {
		p.invalid(field, "must be a table of tool approval settings")
		return nil
	}
	if len(tools) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	result := make(map[string]ApprovalMode, len(tools))
	for _, tool := range sortedKeys(tools) {
		if strings.TrimSpace(tool) == "" || hasControl(tool) {
			p.invalid(field, "contains an invalid tool name")
			return nil
		}
		settings, ok := asMap(tools[tool])
		if !ok || len(settings) != 1 {
			p.invalid(field, "tool settings must contain only approval_mode")
			return nil
		}
		rawMode, ok := settings["approval_mode"]
		if !ok {
			p.invalid(field, "tool settings must contain only approval_mode")
			return nil
		}
		mode, ok := approvalMode(rawMode)
		if !ok {
			p.invalid(field, "tool approval mode must be auto, prompt, writes, or approve")
			return nil
		}
		result[tool] = mode
	}
	return result
}

func (p *entryParser) claudeOAuthField(field string, value any) *ClaudeOAuth {
	object, ok := asMap(value)
	if !ok {
		p.invalid(field, "must be an object")
		return nil
	}
	if len(object) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	oauth := &ClaudeOAuth{}
	for _, key := range sortedKeys(object) {
		switch key {
		case "clientId":
			oauth.ClientID = p.sensitiveStringField(field+".clientId", object[key])
		case "callbackPort":
			port, ok := asInteger(object[key])
			if !ok || port < 1 || port > 65535 {
				p.invalid(field+".callbackPort", "must be an integer from 1 through 65535")
				continue
			}
			oauth.CallbackPort = int(port)
			oauth.CallbackPortSet = true
		case "authServerMetadataUrl":
			oauth.AuthServerMetadataURL = p.nonemptySensitiveStringField(field+".authServerMetadataUrl", object[key])
			if oauth.AuthServerMetadataURL != nil && !validHTTPSURL(oauth.AuthServerMetadataURL.raw(), false) {
				p.invalid(field+".authServerMetadataUrl", "must be an absolute HTTPS URL")
			}
		case "scopes":
			oauth.Scopes = p.nonemptySensitiveStringField(field+".scopes", object[key])
		default:
			p.unsupported(field + "." + key)
		}
	}
	return oauth
}

func (p *entryParser) codexOAuthField(field string, value any) *CodexOAuth {
	object, ok := asMap(value)
	if !ok {
		p.invalid(field, "must be a table")
		return nil
	}
	if len(object) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	oauth := &CodexOAuth{}
	for _, key := range sortedKeys(object) {
		switch key {
		case "client_id":
			oauth.ClientID = p.nonemptySensitiveStringField(field+".client_id", object[key])
		case "callback_port":
			port, ok := asUint64(object[key])
			if !ok || port > 65535 {
				p.invalid(field+".callback_port", "must be an integer from 0 through 65535")
				continue
			}
			oauth.CallbackPort = int(port)
			oauth.CallbackPortSet = true
		default:
			p.unsupported(field + "." + key)
		}
	}
	return oauth
}

func (p *entryParser) toolExposureField(field string, value any) []ToolExposureSurface {
	values, ok := asStringList(value)
	if !ok {
		p.invalid(field, "must be an array of code_mode, deferred, or direct")
		return nil
	}
	if len(values) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	seen := make(map[ToolExposureSurface]struct{}, len(values))
	result := make([]ToolExposureSurface, 0, len(values))
	for _, raw := range values {
		surface := ToolExposureSurface(raw)
		switch surface {
		case ToolExposureCodeMode, ToolExposureDeferred, ToolExposureDirect:
		default:
			p.invalid(field, "must contain only code_mode, deferred, or direct")
			return nil
		}
		if _, duplicate := seen[surface]; !duplicate {
			seen[surface] = struct{}{}
			result = append(result, surface)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (p *entryParser) envValueMapField(field string, value any) map[string]SensitiveValue {
	raw, ok := asMap(value)
	if !ok {
		p.invalid(field, "must map environment variable names to string values")
		return nil
	}
	if len(raw) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	result := make(map[string]SensitiveValue, len(raw))
	for _, key := range sortedKeys(raw) {
		value, ok := asString(raw[key])
		if !ok || !validEnvName(key) || strings.IndexByte(value, 0) >= 0 {
			p.invalid(field, "must contain valid environment names and NUL-free string values")
			return nil
		}
		result[key] = sensitive(value)
	}
	return result
}

func (p *entryParser) headerValueMapField(field string, value any) map[string]SensitiveValue {
	raw, ok := asMap(value)
	if !ok {
		p.invalid(field, "must map HTTP header names to string values")
		return nil
	}
	if len(raw) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	result := make(map[string]SensitiveValue, len(raw))
	for _, key := range sortedKeys(raw) {
		value, ok := asString(raw[key])
		if !ok || !validHeaderName(key) || !validHeaderValue(value) {
			p.invalid(field, "must contain valid HTTP header names and values")
			return nil
		}
		result[key] = sensitive(value)
	}
	return result
}

func (p *entryParser) headerEnvMapField(field string, value any) map[string]string {
	raw, ok := asMap(value)
	if !ok {
		p.invalid(field, "must be an object mapping header names to environment variable names")
		return nil
	}
	if len(raw) > MaxEntryValues {
		p.invalid(field, "exceeds the bounded item limit")
		return nil
	}
	result := make(map[string]string, len(raw))
	for _, key := range sortedKeys(raw) {
		value, ok := asString(raw[key])
		if !ok || !validHeaderName(key) || !validEnvName(value) {
			p.invalid(field, "must map valid HTTP header names to valid environment variable names")
			return nil
		}
		result[key] = value
	}
	return result
}

func (p *entryParser) envNameField(field string, value any) string {
	name, ok := asString(value)
	if !ok || !validEnvName(name) {
		p.invalid(field, "must be a valid environment variable name")
		return ""
	}
	return name
}

func (p *entryParser) unsupported(field string) {
	p.server.UnsupportedFields = append(p.server.UnsupportedFields, field)
	p.problem("unsupported-field", field, "field is not representable and the whole entry is disabled")
}

func (p *entryParser) invalid(field, message string) {
	p.problem("invalid-field", field, message)
}

func (p *entryParser) problem(code, field, message string) {
	p.server.Supported = false
	p.diagnostics = append(p.diagnostics, Diagnostic{
		Severity: SeverityError,
		Code:     code,
		Path:     p.server.Provenance.Path,
		Entry:    safeToken(p.server.Name),
		Field:    safeToken(field),
		Message:  message,
	})
}

func asString(value any) (string, bool) {
	v, ok := value.(string)
	return v, ok
}

func asStringList(value any) ([]string, bool) {
	values, ok := asAnyList(value)
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, false
		}
		result = append(result, s)
	}
	return result, true
}

func asAnyList(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	result := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		result = append(result, rv.Index(i).Interface())
	}
	return result, true
}

func asMap(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	result := make(map[string]any, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		result[iter.Key().String()] = iter.Value().Interface()
	}
	return result, true
}

func asNonnegativeNumber(value any) (float64, bool) {
	number, ok := asNumber(value)
	return number, ok && number >= 0
}

// asCodexDurationSeconds additionally enforces the bound of the Go runtime's
// time.Duration handoff. Rust's native Duration accepts a wider range, but
// retaining a value that the Switchboard executor cannot represent would turn
// a successful compatibility check into overflow at connection time.
func asCodexDurationSeconds(value any) (float64, bool) {
	number, ok := asNonnegativeNumber(value)
	const maxRuntimeDurationNanos = float64(uint64(1<<63 - 1))
	return number, ok && number*1e9 <= maxRuntimeDurationNanos
}

func asPositiveRuntimeMilliseconds(value any) (float64, bool) {
	milliseconds, ok := asNonnegativeRuntimeMilliseconds(value)
	return float64(milliseconds), ok && milliseconds > 0
}

func asNonnegativeRuntimeMilliseconds(value any) (uint64, bool) {
	milliseconds, ok := asUint64(value)
	const maxRuntimeDurationMillis = uint64(1<<63-1) / 1_000_000
	return milliseconds, ok && milliseconds <= maxRuntimeDurationMillis
}

func asNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case int64:
		number = float64(value)
	case int:
		number = float64(value)
	case json.Number:
		var err error
		number, err = value.Float64()
		if err != nil {
			return 0, false
		}
	case float64:
		number = value
	default:
		return 0, false
	}
	return number, !math.IsInf(number, 0) && !math.IsNaN(number)
}

func asInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case json.Number:
		result, err := value.Int64()
		return result, err == nil
	case float64:
		result := int64(value)
		return result, float64(result) == value
	default:
		return 0, false
	}
}

func asUint64(value any) (uint64, bool) {
	switch value := value.(type) {
	case uint64:
		return value, true
	case uint:
		return uint64(value), true
	case int64:
		return uint64(value), value >= 0
	case int:
		return uint64(value), value >= 0
	case json.Number:
		result, err := strconv.ParseUint(value.String(), 10, 64)
		return result, err == nil
	default:
		return 0, false
	}
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	out := result[:0]
	for _, value := range result {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func sortedUniqueEnvVars(values []EnvVar) []EnvVar {
	if len(values) == 0 {
		return nil
	}
	result := append([]EnvVar(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Source < result[j].Source
	})
	out := result[:0]
	for _, value := range result {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func approvalMode(value any) (ApprovalMode, bool) {
	mode, ok := asString(value)
	if !ok {
		return "", false
	}
	switch ApprovalMode(mode) {
	case ApprovalAuto, ApprovalPrompt, ApprovalWrites, ApprovalApprove:
		return ApprovalMode(mode), true
	default:
		return "", false
	}
}

func stringSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func supportedNativeField(dialect Dialect, field string) bool {
	var fields map[string]struct{}
	switch dialect {
	case DialectCodex:
		fields = codexFields
	case DialectClaude:
		fields = claudeFields
	default:
		return false
	}
	_, ok := fields[field]
	return ok
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\t' || c >= 0x20 && c != 0x7f {
			continue
		}
		return false
	}
	return true
}

func validTransportURL(value string, transport Transport, allowExpansion bool) bool {
	if value == "" || hasControl(value) || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	toParse := value
	dynamicScheme := false
	if allowExpansion {
		var expanded bool
		var ok bool
		toParse, expanded, ok = expansionSkeleton(value)
		if !ok {
			return false
		}
		dynamicScheme = expanded && strings.HasPrefix(value, "${")
	}
	parsed, err := url.Parse(toParse)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return dynamicScheme
	}
	if dynamicScheme {
		return true
	}
	switch transport {
	case TransportHTTP, TransportSSE:
		return parsed.Scheme == "http" || parsed.Scheme == "https"
	case TransportWS:
		return parsed.Scheme == "ws" || parsed.Scheme == "wss"
	default:
		return false
	}
}

func validHTTPSURL(value string, allowExpansion bool) bool {
	if value == "" || hasControl(value) || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	toParse := value
	if allowExpansion {
		var ok bool
		toParse, _, ok = expansionSkeleton(value)
		if !ok {
			return false
		}
	}
	parsed, err := url.Parse(toParse)
	return err == nil && parsed.IsAbs() && parsed.Scheme == "https" && parsed.Host != ""
}

func expansionSkeleton(value string) (string, bool, bool) {
	var result strings.Builder
	expanded := false
	for index := 0; index < len(value); {
		if index+2 <= len(value) && value[index:index+2] == "${" {
			end := strings.IndexByte(value[index+2:], '}')
			if end < 0 {
				return "", false, false
			}
			body := value[index+2 : index+2+end]
			name := body
			if cut := strings.Index(body, ":-"); cut >= 0 {
				name = body[:cut]
			}
			if !validEnvName(name) {
				return "", false, false
			}
			result.WriteByte('x')
			expanded = true
			index += end + 3
			continue
		}
		result.WriteByte(value[index])
		index++
	}
	return result.String(), expanded, true
}

func validDisplayName(value string) error {
	if strings.TrimSpace(value) == "" || hasControl(value) {
		return fmt.Errorf("invalid name")
	}
	return nil
}

func reservedClaudeName(value string) bool {
	switch value {
	case "workspace", "claude-in-chrome", "computer-use", "__proto__",
		"Slack sign-in (Claude Code tag)":
		return true
	}
	var normalized strings.Builder
	for _, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			normalized.WriteRune(character)
		} else {
			normalized.WriteByte('_')
		}
	}
	return normalized.String() == "Claude_Preview" || normalized.String() == "Claude_Browser"
}

func canonicalDefinition(provenance Provenance, trustRoot string, raw any) (*definitionMaterial, error) {
	envelope := struct {
		Version      int               `json:"version"`
		Dialect      Dialect           `json:"dialect"`
		Scope        Scope             `json:"scope"`
		Source       Source            `json:"source"`
		RealPath     string            `json:"real_path"`
		ConfigKey    string            `json:"config_key"`
		PluginID     string            `json:"plugin_id,omitempty"`
		PluginRoot   string            `json:"plugin_root,omitempty"`
		Contributors []LayerProvenance `json:"contributors,omitempty"`
		TrustRoot    string            `json:"trust_root"`
		Definition   any               `json:"definition"`
	}{
		Version: 1, Dialect: provenance.Dialect, Scope: provenance.Scope,
		Source: provenance.Source, RealPath: provenance.RealPath,
		ConfigKey: provenance.ConfigKey, PluginID: provenance.PluginID, PluginRoot: provenance.PluginRoot,
		Contributors: provenance.ContributingLayers,
		TrustRoot:    trustRoot, Definition: raw,
	}
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return &definitionMaterial{canonical: canonical}, nil
}

func safeToken(value string) string {
	if !hasControl(value) {
		return value
	}
	return strconv.QuoteToASCII(value)
}
