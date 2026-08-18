package mcpnative

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PluginMCPShape tells ParsePluginMCP whether the supplied JSON is the direct
// server-name map used by inline plugin manifests, or a component file wrapped
// in mcpServers/mcp_servers. Auto accepts either but rejects mixed wrappers.
type PluginMCPShape string

const (
	PluginMCPAuto    PluginMCPShape = "auto"
	PluginMCPDirect  PluginMCPShape = "direct"
	PluginMCPWrapped PluginMCPShape = "wrapped"
)

// PluginMCPOptions describes one MCP component from an already discovered
// plugin. Path is always required and must resolve inside PluginRoot. When
// InlineJSON is nil, Path is read with the normal bounded/no-follow config
// reader. A non-nil InlineJSON is parsed as the exact manifest value while
// Path identifies the containing manifest for provenance and activation.
//
// PluginID, the canonical plugin root, source path, trust root, and complete
// server definition are bound into every activation identity. Native plugin
// enablement is not activation. Plugin entries still require PolicyChecker,
// Switchboard activation, and trust for TrustRoot (default PluginRoot).
type PluginMCPOptions struct {
	Dialect    Dialect
	PluginID   string
	PluginRoot string
	TrustRoot  string
	Path       string
	InlineJSON []byte
	Shape      PluginMCPShape
	// ManifestField safely extracts an inline server map from the bounded
	// manifest at Path (normally "mcpServers" or "mcp_servers"). It is
	// mutually exclusive with InlineJSON.
	ManifestField string
}

// ParsePluginMCP parses one explicit plugin MCP component without searching
// plugin registries, starting processes, expanding arbitrary environment
// variables, or consulting another client's enablement/trust state.
func ParsePluginMCP(opts PluginMCPOptions) Result {
	var diagnostics []Diagnostic
	if opts.Dialect != DialectCodex && opts.Dialect != DialectClaude {
		return pluginErrorResult(opts.Dialect, opts.Path, "invalid-plugin-dialect", "plugin MCP dialect must be codex or claude")
	}
	if validDisplayName(opts.PluginID) != nil {
		return pluginErrorResult(opts.Dialect, opts.Path, "invalid-plugin-id", "plugin ID must be non-empty and contain no control characters")
	}
	root, ok := normalizeRoot(opts.PluginRoot, "plugin-root", &diagnostics)
	if !ok {
		return pluginResult(nil, diagnostics, []Quarantine{{Dialect: opts.Dialect, Precedence: 100, Path: filepath.Clean(opts.PluginRoot), Code: "invalid-plugin-root"}})
	}
	if hasControl(root) {
		return pluginErrorResult(opts.Dialect, opts.PluginRoot, "invalid-plugin-root", "plugin root contains a control character unsafe for native value substitution")
	}
	trustRoot := root
	if strings.TrimSpace(opts.TrustRoot) != "" {
		trustRoot, ok = normalizeRoot(opts.TrustRoot, "plugin-trust-root", &diagnostics)
		if !ok {
			return pluginResult(nil, diagnostics, []Quarantine{{Dialect: opts.Dialect, Precedence: 100, Path: filepath.Clean(opts.TrustRoot), Code: "invalid-plugin-trust-root"}})
		}
	}
	sourcePath, sourceReal, sourceOK := resolvePluginSource(opts.Path, root)
	if !sourceOK {
		return pluginErrorResult(opts.Dialect, opts.Path, "plugin-source-outside-root", "plugin MCP source must be a regular file contained by the plugin root")
	}
	if opts.ManifestField != "" && opts.InlineJSON != nil {
		return pluginErrorResult(opts.Dialect, sourcePath, "conflicting-plugin-input", "ManifestField and InlineJSON are mutually exclusive")
	}

	data := opts.InlineJSON
	if data == nil {
		file, found, err := readConfig(sourcePath, root, MaxConfigBytes, true)
		if err != nil || !found {
			code, message := "unreadable-plugin-mcp", "plugin MCP component could not be read"
			if readErr, isReadErr := err.(*readError); isReadErr {
				code, message = readErr.code, readErr.message
			}
			return pluginErrorResult(opts.Dialect, sourcePath, code, message)
		}
		data, sourcePath, sourceReal = file.data, file.path, file.realPath
	} else if int64(len(data)) > MaxConfigBytes {
		return pluginErrorResult(opts.Dialect, sourcePath, "config-too-large", "plugin MCP component exceeds the bounded read limit")
	}

	rootObject, err := decodeUniqueObject(data)
	if err != nil {
		return pluginErrorResult(opts.Dialect, sourcePath, "invalid-json", "plugin MCP component is not valid duplicate-free JSON")
	}
	var serverObject map[string]json.RawMessage
	var prefix string
	shapeOK := true
	if opts.ManifestField != "" {
		if opts.ManifestField != "mcpServers" && opts.ManifestField != "mcp_servers" {
			shapeOK = false
		} else if raw, exists := rootObject[opts.ManifestField]; !exists {
			shapeOK = false
		} else if serverObject, err = rawObject(raw); err != nil {
			shapeOK = false
		} else {
			prefix = opts.ManifestField
		}
	} else {
		serverObject, prefix, shapeOK = pluginServerObject(rootObject, opts.Shape)
	}
	if !shapeOK {
		return pluginErrorResult(opts.Dialect, sourcePath, "invalid-plugin-mcp-shape", "plugin MCP JSON must be an unambiguous direct server map or a single mcpServers/mcp_servers wrapper")
	}
	if opts.Dialect == DialectCodex {
		serverObject = normalizeCodexPluginServers(serverObject)
	}
	if len(serverObject) > MaxServerEntries {
		return pluginErrorResult(opts.Dialect, sourcePath, "server-entry-budget-exceeded", "plugin MCP server object exceeds the bounded entry limit")
	}
	source := SourceCodexPlugin
	if opts.Dialect == DialectClaude {
		source = SourceClaudePlugin
	}
	base := Provenance{
		Dialect: opts.Dialect, Scope: ScopePlugin, Source: source,
		Path: sourcePath, RealPath: sourceReal, PluginID: opts.PluginID, PluginRoot: root,
	}
	servers, parsed := parseServerRawMap(serverObject, base, trustRoot, prefix)
	diagnostics = append(diagnostics, parsed...)
	for index := range servers {
		pathDiagnostics := bindPluginPaths(&servers[index], root)
		diagnostics = append(diagnostics, pathDiagnostics...)
		effectiveDiagnostics := validateEffectivePluginServer(&servers[index])
		diagnostics = append(diagnostics, effectiveDiagnostics...)
		bindEffectivePluginPaths(&servers[index])
	}
	var quarantines []Quarantine
	if structural := firstStructuralCode(diagnostics); structural != "" {
		quarantines = append(quarantines, Quarantine{Dialect: opts.Dialect, Precedence: 100, Path: sourcePath, Code: structural})
	}
	return pluginResult(servers, diagnostics, quarantines)
}

func normalizeCodexPluginServers(servers map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(servers))
	for name, raw := range servers {
		result[name] = append(json.RawMessage(nil), raw...)
		if validateUniqueJSONLimits(raw, MaxConfigDepth, MaxEntryValues) != nil {
			continue
		}
		entry, err := rawAnyMap(raw)
		if err != nil {
			continue
		}
		if rawType, exists := entry["type"]; exists {
			typeName, ok := asString(rawType)
			if ok && (typeName == "stdio" || typeName == "http" || typeName == "streamable_http" || typeName == "streamable-http") {
				delete(entry, "type")
			}
		}
		if rawOAuth, exists := entry["oauth"]; exists {
			if oauth, ok := asMap(rawOAuth); ok {
				for camel, snake := range map[string]string{"clientId": "client_id", "callbackPort": "callback_port"} {
					value, hasCamel := oauth[camel]
					_, hasSnake := oauth[snake]
					if hasCamel && !hasSnake {
						oauth[snake] = value
						delete(oauth, camel)
					}
				}
				entry["oauth"] = oauth
			}
		}
		if encoded, err := json.Marshal(entry); err == nil {
			result[name] = encoded
		}
	}
	return result
}

func pluginServerObject(root map[string]json.RawMessage, shape PluginMCPShape) (map[string]json.RawMessage, string, bool) {
	if shape == "" {
		shape = PluginMCPAuto
	}
	if shape != PluginMCPAuto && shape != PluginMCPDirect && shape != PluginMCPWrapped {
		return nil, "", false
	}
	var wrapper string
	for _, candidate := range []string{"mcpServers", "mcp_servers"} {
		if _, exists := root[candidate]; exists {
			if wrapper != "" {
				return nil, "", false
			}
			wrapper = candidate
		}
	}
	if shape == PluginMCPDirect || shape == PluginMCPAuto && wrapper == "" {
		return root, "plugin.mcpServers", true
	}
	if wrapper == "" || len(root) != 1 {
		return nil, "", false
	}
	object, err := rawObject(root[wrapper])
	if err != nil {
		return nil, "", false
	}
	return object, wrapper, true
}

func resolvePluginSource(path, root string) (string, string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", "", false
	}
	requested := path
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(root, requested)
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", "", false
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil || !withinRoot(root, real) {
		return "", "", false
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}
	return filepath.Clean(abs), filepath.Clean(real), true
}

func bindPluginPaths(server *Server, root string) []Diagnostic {
	if server == nil {
		return nil
	}
	if server.Provenance.Dialect == DialectClaude {
		replacePluginRoot(server, "${CLAUDE_PLUGIN_ROOT}", root)
	}
	var diagnostics []Diagnostic
	if server.CWD != nil && !server.CWD.Empty() && len(server.CWD.EnvReferences()) == 0 {
		resolved, ok := resolvePluginDirectory(server.CWD.raw(), root)
		if !ok {
			server.Supported = false
			diagnostics = append(diagnostics, pluginPathDiagnostic(*server, "cwd", "plugin cwd must resolve to a directory inside the plugin root"))
		} else {
			value := sensitive(resolved)
			server.CWD = &value
		}
	}
	if server.Provenance.Dialect == DialectClaude && server.Command != nil && filepath.IsAbs(server.Command.raw()) &&
		withinRoot(root, server.Command.raw()) && len(server.Command.EnvReferences()) == 0 {
		resolved, ok := resolvePluginCommand(server.Command.raw(), root)
		if !ok {
			server.Supported = false
			diagnostics = append(diagnostics, pluginPathDiagnostic(*server, "command", "Claude plugin-root command must resolve to a regular file inside the plugin root"))
		} else {
			value := sensitive(resolved)
			server.Command = &value
		}
	}
	return diagnostics
}

func replacePluginRoot(server *Server, marker, root string) {
	replace := func(value *SensitiveValue) {
		if value != nil {
			value.value = strings.ReplaceAll(value.value, marker, root)
		}
	}
	replace(server.Command)
	for index := range server.Args {
		replace(&server.Args[index])
	}
	replace(server.CWD)
	for key, value := range server.Env {
		value.value = strings.ReplaceAll(value.value, marker, root)
		server.Env[key] = value
	}
	replace(server.URL)
	for key, value := range server.Headers {
		value.value = strings.ReplaceAll(value.value, marker, root)
		server.Headers[key] = value
	}
	replace(server.OAuthResource)
	replace(server.HeadersHelper)
}

func resolvePluginDirectory(value, root string) (string, bool) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !withinRoot(root, real) {
		return "", false
	}
	info, err := os.Stat(real)
	return filepath.Clean(real), err == nil && info.IsDir()
}

func resolvePluginCommand(value, root string) (string, bool) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !withinRoot(root, real) {
		return "", false
	}
	info, err := os.Stat(real)
	return filepath.Clean(real), err == nil && info.Mode().IsRegular()
}

func pluginPathDiagnostic(server Server, field, message string) Diagnostic {
	return Diagnostic{Severity: SeverityError, Code: "plugin-path-outside-root", Path: server.Provenance.Path, Entry: safeToken(server.Name), Field: field, Message: message}
}

func validateEffectivePluginServer(server *Server) []Diagnostic {
	if server == nil {
		return nil
	}
	var diagnostics []Diagnostic
	problem := func(field, message string) {
		server.Supported = false
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError, Code: "invalid-effective-plugin-value", Path: server.Provenance.Path,
			Entry: safeToken(server.Name), Field: field, Message: message,
		})
	}
	if server.URL != nil && !server.URL.Empty() && !validTransportURL(server.URL.raw(), server.Transport, server.Provenance.Dialect == DialectClaude) {
		problem("url", "plugin substitution produced an invalid transport URL")
	}
	for name, value := range server.Headers {
		if !validHeaderName(name) || !validHeaderValue(value.raw()) {
			problem("headers", "plugin substitution produced an invalid HTTP header")
			break
		}
	}
	return diagnostics
}

func bindEffectivePluginPaths(server *Server) {
	if server == nil || server.definition == nil {
		return
	}
	material := struct {
		Command string `json:"command,omitempty"`
		CWD     string `json:"cwd,omitempty"`
	}{}
	if server.Command != nil && filepath.IsAbs(server.Command.raw()) && withinRoot(server.Provenance.PluginRoot, server.Command.raw()) {
		material.Command = server.Command.raw()
	}
	if server.CWD != nil {
		material.CWD = server.CWD.raw()
	}
	encoded, err := json.Marshal(material)
	if err == nil {
		appendDefinitionContext(server, "plugin-effective-paths:"+string(encoded))
	}
}

func pluginErrorResult(dialect Dialect, path, code, message string) Result {
	diagnostic := Diagnostic{Severity: SeverityError, Code: code, Path: filepath.Clean(path), Message: message}
	return pluginResult(nil, []Diagnostic{diagnostic}, []Quarantine{{Dialect: dialect, Precedence: 100, Path: filepath.Clean(path), Code: code}})
}

func pluginResult(servers []Server, diagnostics []Diagnostic, quarantines []Quarantine) Result {
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	sortDiagnostics(diagnostics)
	sort.Slice(quarantines, func(i, j int) bool {
		if quarantines[i].Dialect != quarantines[j].Dialect {
			return quarantines[i].Dialect < quarantines[j].Dialect
		}
		if quarantines[i].Path != quarantines[j].Path {
			return quarantines[i].Path < quarantines[j].Path
		}
		return quarantines[i].Code < quarantines[j].Code
	})
	public := make([]Server, 0, len(servers))
	authoritative := make(map[string]Server, len(servers))
	precedence := make(map[string]int, len(servers))
	for _, server := range servers {
		authoritative[server.ID] = deepCloneServer(server)
		precedence[server.ID] = 100
		public = append(public, deepCloneServer(server))
	}
	return Result{
		Servers: public, Diagnostics: diagnostics, Quarantines: append([]Quarantine(nil), quarantines...),
		authoritative: authoritative, precedence: precedence, quarantine: append([]Quarantine(nil), quarantines...),
	}
}
