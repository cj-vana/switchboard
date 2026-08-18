package mcpnative

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// ErrInvalidCodexSnapshot means a purported Codex config/read result was not
// a complete, bounded, internally consistent effective configuration view.
var ErrInvalidCodexSnapshot = errors.New("native Codex effective configuration snapshot is invalid")

// CodexSnapshot is an opaque, redacting snapshot of Codex's authoritative
// config/read result for one cwd. Its effective mcp_servers map incorporates
// package, MDM, system, enterprise, user/profile, project, session/thread, and
// legacy-managed layers. The individual layers are retained only for exact
// provenance, project-trust classification, and activation binding.
//
// Obtain the payload by calling the installed Codex app-server config/read
// method with includeLayers=true and the same cwd passed here. Pass the result
// object itself (the object containing config, origins, and layers), not the
// outer JSON-RPC envelope. The caller remains responsible for a bounded
// process deadline and output cap; this constructor independently enforces
// structural and byte bounds.
type CodexSnapshot struct {
	cwd     string
	entries map[string]codexSnapshotEntry
}

type codexSnapshotEntry struct {
	definition   any
	contributors []codexSnapshotLayer
}

type codexSnapshotLayer struct {
	rank       int
	order      int
	scope      Scope
	source     Source
	path       string
	realPath   string
	baseDir    string
	version    string
	projectDir string
	servers    map[string]any
}

func (CodexSnapshot) String() string   { return "<native Codex snapshot redacted>" }
func (CodexSnapshot) GoString() string { return "<native Codex snapshot redacted>" }
func (s CodexSnapshot) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(s.String()))
}
func (CodexSnapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal("<native Codex snapshot redacted>")
}
func (CodexSnapshot) MarshalText() ([]byte, error) {
	return []byte("<native Codex snapshot redacted>"), nil
}

// NewCodexSnapshot validates and seals one Codex app-server config/read result.
// The returned snapshot has no exported configuration bytes and is safe to put
// in Options without making ordinary formatting a credential disclosure path.
func NewCodexSnapshot(configReadResult []byte, cwd string) (*CodexSnapshot, error) {
	if len(configReadResult) == 0 || int64(len(configReadResult)) > MaxTotalConfigBytes {
		return nil, snapshotError("response exceeds the bounded byte limit")
	}
	if err := validateUniqueJSONLimits(configReadResult, MaxConfigDepth, MaxConfigValues); err != nil {
		return nil, snapshotError("response is not bounded duplicate-free JSON")
	}
	resolvedCWD, err := snapshotDirectory(cwd)
	if err != nil {
		return nil, snapshotError("cwd is not a canonical directory")
	}
	root, err := decodeUniqueObject(configReadResult)
	if err != nil {
		return nil, snapshotError("response is not an object")
	}
	configRaw, configOK := root["config"]
	originsRaw, originsOK := root["origins"]
	layersRaw, layersOK := root["layers"]
	if !configOK || !originsOK || !layersOK || string(layersRaw) == "null" {
		return nil, snapshotError("config/read must include config, origins, and layers")
	}
	config, err := rawAnyMap(configRaw)
	if err != nil {
		return nil, snapshotError("effective config is not an object")
	}
	effectiveServers := map[string]any{}
	if rawServers, exists := config["mcp_servers"]; exists {
		var ok bool
		effectiveServers, ok = asMap(rawServers)
		if !ok {
			return nil, snapshotError("effective mcp_servers is not an object")
		}
	}
	if len(effectiveServers) > MaxServerEntries {
		return nil, snapshotError("effective mcp_servers exceeds the entry limit")
	}

	var layerMessages []json.RawMessage
	if err := json.Unmarshal(layersRaw, &layerMessages); err != nil || layerMessages == nil {
		return nil, snapshotError("layers is not an array")
	}
	if len(layerMessages) > MaxConfigFiles {
		return nil, snapshotError("layer count exceeds the bounded limit")
	}
	highToLow := make([]codexSnapshotLayer, 0, len(layerMessages))
	previousRank := int(^uint(0) >> 1)
	for index, raw := range layerMessages {
		layer, disabled, parseErr := parseCodexSnapshotLayer(raw)
		if parseErr != nil {
			return nil, parseErr
		}
		if layer.rank > previousRank {
			return nil, snapshotError("layers are not ordered from high to low precedence")
		}
		previousRank = layer.rank
		if disabled {
			continue
		}
		layer.order = len(layerMessages) - 1 - index
		highToLow = append(highToLow, layer)
	}
	layers := make([]codexSnapshotLayer, len(highToLow))
	for index := range highToLow {
		layers[len(highToLow)-1-index] = highToLow[index]
	}
	if err := validateCodexSnapshotProjects(layers, resolvedCWD); err != nil {
		return nil, err
	}
	origins, err := rawObject(originsRaw)
	if err != nil {
		return nil, snapshotError("origins is not an object")
	}
	type mergedSnapshotEntry struct {
		definition   any
		contributors []codexSnapshotLayer
	}
	merged := make(map[string]mergedSnapshotEntry)
	for _, layer := range layers {
		for _, name := range sortedKeys(layer.servers) {
			higher := cloneTOMLValue(layer.servers[name])
			entry, exists := merged[name]
			if !exists {
				merged[name] = mergedSnapshotEntry{
					definition: higher, contributors: []codexSnapshotLayer{cloneCodexSnapshotLayer(layer)},
				}
				continue
			}
			lowerMap, lowerOK := asMap(entry.definition)
			higherMap, higherOK := asMap(higher)
			if lowerOK && higherOK {
				entry.definition = mergeTOMLMaps(lowerMap, higherMap)
			} else {
				entry.definition = higher
			}
			entry.contributors = append(entry.contributors, cloneCodexSnapshotLayer(layer))
			merged[name] = entry
		}
	}
	for name := range merged {
		if _, exists := effectiveServers[name]; !exists {
			return nil, snapshotError("returned layers contain an MCP server absent from effective config")
		}
	}

	entries := make(map[string]codexSnapshotEntry, len(effectiveServers))
	for _, name := range sortedKeys(effectiveServers) {
		if !snapshotEntryWithinBounds(effectiveServers[name]) {
			return nil, snapshotError("effective MCP server exceeds the entry depth or value limit")
		}
		entry, exists := merged[name]
		definition := cloneTOMLValue(effectiveServers[name])
		var contributors []codexSnapshotLayer
		if exists {
			definition = cloneTOMLValue(entry.definition)
			contributors = append([]codexSnapshotLayer(nil), entry.contributors...)
			if !snapshotExplicitEnablementAgrees(definition, effectiveServers[name]) {
				return nil, snapshotError("effective MCP enablement disagrees with its returned layers")
			}
		}
		origin, originOK, originErr := effectiveCodexServerOrigin(origins, name)
		if originErr != nil {
			return nil, originErr
		}
		if raw, ok := asMap(definition); ok {
			_, commandSet := raw["command"]
			_, urlSet := raw["url"]
			if (commandSet || urlSet) && !originOK {
				return nil, snapshotError("an effective MCP server has no winning transport origin")
			}
		}
		if originOK && !snapshotLayerPresent(contributors, origin) {
			if origin.source != SourceCodexPackage {
				return nil, snapshotError("an effective MCP origin is absent from the returned layer stack")
			}
			if snapshotLayerIdentityPresent(contributors, origin) {
				return nil, snapshotError("an effective MCP origin version disagrees with its returned layer")
			}
			contributors = append([]codexSnapshotLayer{origin}, contributors...)
			if !exists {
				definition = pruneSnapshotNulls(definition)
				definition, err = normalizeHiddenPackageDefinition(definition, origin.baseDir)
				if err != nil {
					return nil, err
				}
			}
		}
		if len(contributors) == 0 {
			return nil, snapshotError("an effective MCP server has no authoritative layer origin")
		}
		entries[name] = codexSnapshotEntry{definition: definition, contributors: contributors}
	}
	return &CodexSnapshot{cwd: resolvedCWD, entries: entries}, nil
}

func snapshotError(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidCodexSnapshot, message)
}

func snapshotDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || hasControl(path) {
		return "", ErrInvalidCodexSnapshot
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidCodexSnapshot
	}
	return filepath.Clean(real), nil
}

func parseCodexSnapshotLayer(raw json.RawMessage) (codexSnapshotLayer, bool, error) {
	object, err := decodeUniqueObject(raw)
	if err != nil {
		return codexSnapshotLayer{}, false, snapshotError("layer is not an object")
	}
	for key := range object {
		if key != "name" && key != "version" && key != "config" && key != "disabledReason" {
			return codexSnapshotLayer{}, false, snapshotError("layer contains an unknown field")
		}
	}
	nameRaw, nameOK := object["name"]
	versionRaw, versionOK := object["version"]
	configRaw, configOK := object["config"]
	if !nameOK || !versionOK || !configOK {
		return codexSnapshotLayer{}, false, snapshotError("layer is missing required metadata")
	}
	version, ok := rawJSONString(versionRaw)
	if !ok || !safeSnapshotIdentity(version) {
		return codexSnapshotLayer{}, false, snapshotError("layer version is invalid")
	}
	layer, err := parseCodexSnapshotLayerName(nameRaw)
	if err != nil {
		return codexSnapshotLayer{}, false, err
	}
	layer.version = version
	if layer.source == SourceCodexSession {
		layer.path = "codex-session:" + version
		layer.realPath = layer.path
	}
	config, err := rawAnyMap(configRaw)
	if err != nil {
		return codexSnapshotLayer{}, false, snapshotError("layer config is not an object")
	}
	layer.servers = map[string]any{}
	if rawServers, exists := config["mcp_servers"]; exists {
		layer.servers, ok = asMap(rawServers)
		if !ok || len(layer.servers) > MaxServerEntries {
			return codexSnapshotLayer{}, false, snapshotError("layer mcp_servers is invalid or too large")
		}
		for _, definition := range layer.servers {
			if !snapshotEntryWithinBounds(definition) {
				return codexSnapshotLayer{}, false, snapshotError("layer MCP server exceeds the entry depth or value limit")
			}
		}
		if err := normalizeCodexSnapshotLayerPaths(&layer); err != nil {
			return codexSnapshotLayer{}, false, err
		}
	}
	_, disabled := object["disabledReason"]
	if disabled {
		if _, ok := rawJSONString(object["disabledReason"]); !ok {
			return codexSnapshotLayer{}, false, snapshotError("disabledReason is not a string")
		}
	}
	return layer, disabled, nil
}

func parseCodexSnapshotLayerName(raw json.RawMessage) (codexSnapshotLayer, error) {
	object, err := decodeUniqueObject(raw)
	if err != nil {
		return codexSnapshotLayer{}, snapshotError("layer source is not an object")
	}
	typeRaw, ok := object["type"]
	if !ok {
		return codexSnapshotLayer{}, snapshotError("layer source has no type")
	}
	kind, ok := rawJSONString(typeRaw)
	if !ok {
		return codexSnapshotLayer{}, snapshotError("layer source type is invalid")
	}
	allowed := func(keys ...string) bool {
		set := map[string]struct{}{"type": {}}
		for _, key := range keys {
			set[key] = struct{}{}
		}
		for key := range object {
			if _, exists := set[key]; !exists {
				return false
			}
		}
		return true
	}
	fileLayer := func(rank int, scope Scope, source Source, field string) (codexSnapshotLayer, error) {
		if !allowed(field) {
			return codexSnapshotLayer{}, snapshotError("layer source contains an unknown field")
		}
		path, exists := rawJSONString(object[field])
		if !exists || !safeSnapshotPath(path) {
			return codexSnapshotLayer{}, snapshotError("layer source path is invalid")
		}
		path = canonicalSnapshotPath(path)
		return codexSnapshotLayer{
			rank: rank, scope: scope, source: source, path: path, realPath: path,
			baseDir: filepath.Dir(path),
		}, nil
	}
	switch kind {
	case "packagedDefaults":
		return fileLayer(-10, ScopeManaged, SourceCodexPackage, "file")
	case "mdm":
		if !allowed("domain", "key") {
			return codexSnapshotLayer{}, snapshotError("MDM layer source contains an unknown field")
		}
		domain, domainOK := rawJSONString(object["domain"])
		key, keyOK := rawJSONString(object["key"])
		if !domainOK || !keyOK || !safeSnapshotIdentity(domain) || !safeSnapshotIdentity(key) {
			return codexSnapshotLayer{}, snapshotError("MDM layer identity is invalid")
		}
		path := fmt.Sprintf("codex-mdm:%d:%s:%d:%s", len(domain), domain, len(key), key)
		return codexSnapshotLayer{rank: 0, scope: ScopeManaged, source: SourceCodexMDM, path: path, realPath: path}, nil
	case "system":
		return fileLayer(10, ScopeManaged, SourceCodexSystem, "file")
	case "enterpriseManaged":
		if !allowed("id", "name") {
			return codexSnapshotLayer{}, snapshotError("enterprise layer source contains an unknown field")
		}
		id, idOK := rawJSONString(object["id"])
		name, nameOK := rawJSONString(object["name"])
		if !idOK || !nameOK || !safeSnapshotIdentity(id) || !safeSnapshotIdentity(name) {
			return codexSnapshotLayer{}, snapshotError("enterprise layer identity is invalid")
		}
		path := "codex-enterprise:" + id
		return codexSnapshotLayer{rank: 15, scope: ScopeManaged, source: SourceCodexCloud, path: path, realPath: path}, nil
	case "user":
		if !allowed("file", "profile") {
			return codexSnapshotLayer{}, snapshotError("user layer source contains an unknown field")
		}
		path, pathOK := rawJSONString(object["file"])
		if !pathOK || !safeSnapshotPath(path) {
			return codexSnapshotLayer{}, snapshotError("user layer path is invalid")
		}
		rank, source := 20, SourceCodexUser
		if profileRaw, exists := object["profile"]; exists && string(profileRaw) != "null" {
			profile, profileOK := rawJSONString(profileRaw)
			if !profileOK || !safeSnapshotIdentity(profile) {
				return codexSnapshotLayer{}, snapshotError("profile layer identity is invalid")
			}
			rank, source = 21, SourceCodexProfile
		}
		path = canonicalSnapshotPath(path)
		return codexSnapshotLayer{
			rank: rank, scope: ScopeUser, source: source, path: path, realPath: path,
			baseDir: filepath.Dir(path),
		}, nil
	case "project":
		if !allowed("dotCodexFolder") {
			return codexSnapshotLayer{}, snapshotError("project layer source contains an unknown field")
		}
		folder, folderOK := rawJSONString(object["dotCodexFolder"])
		if !folderOK || !safeSnapshotPath(folder) {
			return codexSnapshotLayer{}, snapshotError("project layer path is invalid")
		}
		folder = canonicalSnapshotPath(folder)
		path := filepath.Join(folder, "config.toml")
		return codexSnapshotLayer{
			rank: 25, scope: ScopeProject, source: SourceCodexProject,
			path: path, realPath: path, baseDir: folder, projectDir: filepath.Dir(folder),
		}, nil
	case "sessionFlags":
		if !allowed() {
			return codexSnapshotLayer{}, snapshotError("session layer source contains an unknown field")
		}
		return codexSnapshotLayer{rank: 30, scope: ScopeUser, source: SourceCodexSession}, nil
	case "legacyManagedConfigTomlFromFile":
		return fileLayer(40, ScopeManaged, SourceCodexLegacy, "file")
	case "legacyManagedConfigTomlFromMdm":
		if !allowed() {
			return codexSnapshotLayer{}, snapshotError("legacy MDM source contains an unknown field")
		}
		const path = "codex-mdm:legacy-managed-config"
		return codexSnapshotLayer{rank: 50, scope: ScopeManaged, source: SourceCodexLegacy, path: path, realPath: path}, nil
	default:
		return codexSnapshotLayer{}, snapshotError("layer source type is unsupported")
	}
}

func validateCodexSnapshotProjects(layers []codexSnapshotLayer, cwd string) error {
	previous := ""
	for _, layer := range layers {
		if layer.scope != ScopeProject {
			continue
		}
		if layer.projectDir == "" || !withinRoot(layer.projectDir, cwd) {
			return snapshotError("project layer is not an ancestor of the snapshot cwd")
		}
		if previous != "" && (previous == layer.projectDir || !withinRoot(previous, layer.projectDir)) {
			return snapshotError("project layers are not ordered from root to cwd")
		}
		previous = layer.projectDir
	}
	return nil
}

func effectiveCodexServerOrigin(origins map[string]json.RawMessage, name string) (codexSnapshotLayer, bool, error) {
	for _, field := range []string{"command", "url"} {
		raw, exists := origins["mcp_servers."+name+"."+field]
		if !exists {
			continue
		}
		object, err := decodeUniqueObject(raw)
		if err != nil {
			return codexSnapshotLayer{}, false, snapshotError("MCP origin metadata is invalid")
		}
		nameRaw, nameOK := object["name"]
		versionRaw, versionOK := object["version"]
		if !nameOK || !versionOK || len(object) != 2 {
			return codexSnapshotLayer{}, false, snapshotError("MCP origin metadata is incomplete")
		}
		layer, err := parseCodexSnapshotLayerName(nameRaw)
		if err != nil {
			return codexSnapshotLayer{}, false, err
		}
		version, ok := rawJSONString(versionRaw)
		if !ok || !safeSnapshotIdentity(version) {
			return codexSnapshotLayer{}, false, snapshotError("MCP origin version is invalid")
		}
		layer.version = version
		if layer.source == SourceCodexSession {
			layer.path = "codex-session:" + version
			layer.realPath = layer.path
		}
		return layer, true, nil
	}
	return codexSnapshotLayer{}, false, nil
}

func snapshotLayerPresent(layers []codexSnapshotLayer, wanted codexSnapshotLayer) bool {
	for _, layer := range layers {
		if layer.rank == wanted.rank && layer.source == wanted.source && layer.realPath == wanted.realPath && layer.version == wanted.version {
			return true
		}
	}
	return false
}

func snapshotLayerIdentityPresent(layers []codexSnapshotLayer, wanted codexSnapshotLayer) bool {
	for _, layer := range layers {
		if layer.rank == wanted.rank && layer.source == wanted.source && layer.realPath == wanted.realPath {
			return true
		}
	}
	return false
}

func normalizeCodexSnapshotLayerPaths(layer *codexSnapshotLayer) error {
	if layer == nil {
		return snapshotError("layer metadata is missing")
	}
	for name, raw := range layer.servers {
		entry, ok := asMap(raw)
		if !ok {
			continue
		}
		rawCWD, exists := entry["cwd"]
		if !exists {
			layer.servers[name] = entry
			continue
		}
		cwd, isString := asString(rawCWD)
		if !isString || filepath.IsAbs(cwd) {
			layer.servers[name] = entry
			continue
		}
		if layer.baseDir == "" {
			return snapshotError("relative MCP cwd has no authoritative layer base directory")
		}
		entry["cwd"] = filepath.Clean(filepath.Join(layer.baseDir, filepath.FromSlash(cwd)))
		layer.servers[name] = entry
	}
	return nil
}

func snapshotExplicitEnablementAgrees(definition, effective any) bool {
	definitionMap, definitionOK := asMap(definition)
	effectiveMap, effectiveOK := asMap(effective)
	if !definitionOK || !effectiveOK {
		return true
	}
	want, explicitlySet := definitionMap["enabled"]
	if !explicitlySet {
		return true
	}
	got, reported := effectiveMap["enabled"]
	return reported && reflect.DeepEqual(got, want)
}

func pruneSnapshotNulls(value any) any {
	if value == nil {
		return nil
	}
	if object, ok := asMap(value); ok {
		result := make(map[string]any, len(object))
		for key, child := range object {
			if child == nil {
				continue
			}
			result[key] = pruneSnapshotNulls(child)
		}
		return result
	}
	if values, ok := asAnyList(value); ok {
		result := make([]any, len(values))
		for index, child := range values {
			result[index] = pruneSnapshotNulls(child)
		}
		return result
	}
	return value
}

func normalizeHiddenPackageDefinition(definition any, baseDir string) (any, error) {
	entry, ok := asMap(definition)
	if !ok {
		return definition, nil
	}
	rawCWD, exists := entry["cwd"]
	if !exists {
		return entry, nil
	}
	cwd, isString := asString(rawCWD)
	if !isString || filepath.IsAbs(cwd) {
		return entry, nil
	}
	if baseDir == "" {
		return nil, snapshotError("packaged MCP cwd has no authoritative base directory")
	}
	entry["cwd"] = filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(cwd)))
	return entry, nil
}

func cloneCodexSnapshotLayer(layer codexSnapshotLayer) codexSnapshotLayer {
	if layer.servers != nil {
		layer.servers = cloneTOMLValue(layer.servers).(map[string]any)
	}
	return layer
}

func safeSnapshotIdentity(value string) bool {
	return value != "" && len(value) <= 4096 && !hasControl(value)
}

func safeSnapshotPath(value string) bool {
	return safeSnapshotIdentity(value) && filepath.IsAbs(value)
}

func snapshotEntryWithinBounds(value any) bool {
	values := 0
	return configTreeWithinBounds(reflect.ValueOf(value), 0, &values, MaxConfigDepth, MaxEntryValues)
}

func canonicalSnapshotPath(value string) string {
	value = filepath.Clean(value)
	if real, err := filepath.EvalSymlinks(value); err == nil {
		return filepath.Clean(real)
	}
	return value
}

func rawJSONString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func (c *collector) loadCodexSnapshot(snapshot *CodexSnapshot, workspace, current string) {
	if snapshot == nil || snapshot.cwd == "" || snapshot.entries == nil {
		c.codexSnapshotDiagnostic("codex-layer-stack-unavailable", "authoritative Codex layer stack is unavailable")
		return
	}
	if current != "" && snapshot.cwd != current {
		c.codexSnapshotDiagnostic("codex-snapshot-cwd-mismatch", "Codex snapshot cwd does not match native discovery cwd")
	}
	for _, name := range sortedKeysAny(snapshot.entries) {
		entry := snapshot.entries[name]
		if len(entry.contributors) == 0 {
			c.codexSnapshotDiagnostic("codex-snapshot-origin-missing", "Codex snapshot entry has no authoritative origin")
			continue
		}
		highest := entry.contributors[len(entry.contributors)-1]
		provenance := Provenance{
			Dialect: DialectCodex, Scope: highest.scope, Source: highest.source,
			Path: highest.path, RealPath: highest.realPath, ConfigKey: "mcp_servers." + name,
			ContributingLayers: make([]LayerProvenance, 0, len(entry.contributors)),
		}
		requiresTrust := false
		trustRoot := ""
		for _, contributor := range entry.contributors {
			provenance.ContributingLayers = append(provenance.ContributingLayers, LayerProvenance{
				Scope: contributor.scope, Source: contributor.source,
				Path: contributor.path, RealPath: contributor.realPath,
			})
			if contributor.scope == ScopeProject {
				requiresTrust = true
				if workspace == "" || !withinRoot(workspace, contributor.projectDir) {
					c.codexSnapshotDiagnostic("codex-snapshot-project-mismatch", "Codex project layer is outside the declared Switchboard workspace")
				} else {
					trustRoot = workspace
				}
			}
		}
		raw, rawOK := asMap(entry.definition)
		var server Server
		var diagnostics []Diagnostic
		if !rawOK {
			server = invalidShell(name, provenance, trustRoot, entry.definition)
			diagnostics = []Diagnostic{{
				Severity: SeverityError, Code: "invalid-server-entry", Path: provenance.Path,
				Entry: safeToken(name), Message: "effective Codex server entry must be an object/table",
			}}
		} else {
			server, diagnostics = parseEntry(name, raw, provenance, trustRoot)
		}
		server.ExecutionTrustRequired = requiresTrust
		if requiresTrust {
			server.TrustRoot = trustRoot
		} else {
			server.TrustRoot = ""
		}
		bindCodexSnapshotDefinition(&server, snapshot.cwd, entry.contributors)
		c.diagnostics = append(c.diagnostics, diagnostics...)
		c.add(server, highest.rank*1000+highest.order)
	}
}

func bindCodexSnapshotDefinition(server *Server, cwd string, contributors []codexSnapshotLayer) {
	type boundContributor struct {
		Scope      Scope  `json:"scope"`
		Source     Source `json:"source"`
		RealPath   string `json:"real_path"`
		Definition any    `json:"definition,omitempty"`
	}
	material := struct {
		Version      int                `json:"version"`
		CWD          string             `json:"cwd"`
		Contributors []boundContributor `json:"contributors"`
	}{Version: 1, CWD: cwd, Contributors: make([]boundContributor, 0, len(contributors))}
	for _, contributor := range contributors {
		var definition any
		if raw, exists := contributor.servers[server.Name]; exists {
			definition = raw
		}
		material.Contributors = append(material.Contributors, boundContributor{
			Scope: contributor.scope, Source: contributor.source,
			RealPath: contributor.realPath, Definition: definition,
		})
	}
	if encoded, err := json.Marshal(material); err == nil {
		appendDefinitionContext(server, "codex-authoritative-snapshot:"+string(encoded))
	}
}

func (c *collector) codexSnapshotDiagnostic(code, message string) {
	c.diagnostics = append(c.diagnostics, Diagnostic{
		Severity: SeverityError, Code: code, Path: "codex-effective-config", Message: message,
	})
	c.quarantine(DialectCodex, 1<<30, "codex-effective-config", code)
}

func (c *collector) hasDialectWinner(dialect Dialect) bool {
	for _, winner := range c.winners {
		if winner.server.Provenance.Dialect == dialect {
			return true
		}
	}
	return false
}
