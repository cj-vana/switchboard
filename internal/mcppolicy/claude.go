package mcppolicy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

type claudeEntryKind uint8

const (
	claudeEntryName claudeEntryKind = iota + 1
	claudeEntryCommand
	claudeEntryURL
)

type claudePolicyEntry struct {
	kind    claudeEntryKind
	name    string
	command []string
	url     string
}

type claudeSettings struct {
	sourceNonempty bool
	allowPresent   bool
	allowLockdown  bool
	allowed        []claudePolicyEntry
	denied         []claudePolicyEntry

	managedOnlySet bool
	managedOnly    bool
	helperPresent  bool

	disabledProject []string
	disabledAll     []string
	environment     map[string]string
	enabledPlugins  map[string]bool

	strippedAllowed int
}

type claudePolicy struct {
	unavailable      bool
	managedExclusive bool

	allowPresent            bool
	allowEntries            []claudePolicyEntry
	denyEntries             []claudePolicyEntry
	disabledAll             map[string]struct{}
	disabledProject         map[string]struct{}
	stateDisabledNonProject map[string]struct{}
	stateDisabledProject    map[string]struct{}

	serverEnvironment  mcpnative.EnvironmentLookup
	allowEnvironment   mcpnative.EnvironmentLookup
	denyEnvironment    mcpnative.EnvironmentLookup
	runtimeEnvironment environment
	pluginDenials      map[string]struct{}
}

var allowedClaudeName = regexp.MustCompile(`\A[A-Za-z0-9_-]+\z`)

// parseClaudeSettings parses only MCP enforcement fields and env, which is
// security-relevant to Claude's policy expansion. Unknown settings are left
// to Claude's broader settings implementation. managed selects Claude's
// documented tolerant managed-policy behavior for allowedMcpServers: an
// invalid whole field becomes an empty allowlist and invalid entries are
// stripped. Ambiguous deny and environment fields still return an error so
// Switchboard fails closed instead of weakening a native restriction.
func parseClaudeSettings(data []byte, managed bool) (claudeSettings, error) {
	root, err := decodeUniqueJSONObject(data)
	if err != nil {
		return claudeSettings{}, fmt.Errorf("invalid JSON")
	}
	result := claudeSettings{}
	result.sourceNonempty = len(root) != 0
	if raw, exists := root["allowedMcpServers"]; exists {
		result.allowPresent = true
		entries, invalidWhole, entryErr := parseClaudeEntries(raw, true, managed)
		if entryErr != nil {
			return claudeSettings{}, entryErr
		}
		if invalidWhole {
			if !managed {
				return claudeSettings{}, fmt.Errorf("invalid allowedMcpServers")
			}
			result.strippedAllowed++
			result.allowLockdown = true
		} else {
			result.allowed = entries.valid
			result.strippedAllowed += entries.invalid
		}
	}
	if raw, exists := root["deniedMcpServers"]; exists {
		entries, invalidWhole, entryErr := parseClaudeEntries(raw, false, false)
		if entryErr != nil || invalidWhole || entries.invalid != 0 {
			return claudeSettings{}, fmt.Errorf("invalid deniedMcpServers")
		}
		result.denied = entries.valid
	}
	if raw, exists := root["allowManagedMcpServersOnly"]; exists && managed {
		result.managedOnlySet = true
		value, ok := rawBool(raw)
		if !ok {
			// This enforcement field is explicitly fail-closed in Claude Code.
			result.managedOnly = true
		} else {
			result.managedOnly = value
		}
	}
	if _, exists := root["policyHelper"]; exists && managed {
		result.helperPresent = true
	}
	var fieldErr error
	result.disabledProject, fieldErr = optionalStringList(root, "disabledMcpjsonServers")
	if fieldErr != nil {
		return claudeSettings{}, fieldErr
	}
	result.disabledAll, fieldErr = optionalStringList(root, "disabledMcpServers")
	if fieldErr != nil {
		return claudeSettings{}, fieldErr
	}
	if raw, exists := root["env"]; exists {
		environment, ok := rawStringMap(raw)
		if !ok || len(environment) > MaxPolicyEntries {
			return claudeSettings{}, fmt.Errorf("invalid env")
		}
		for name := range environment {
			if !validEnvironmentName(name) {
				return claudeSettings{}, fmt.Errorf("invalid env")
			}
		}
		result.environment = environment
	}
	if raw, exists := root["enabledPlugins"]; exists {
		plugins, ok := rawBoolMap(raw)
		if !ok || len(plugins) > MaxPolicyEntries {
			return claudeSettings{}, fmt.Errorf("invalid enabledPlugins")
		}
		for id := range plugins {
			if !validClaudePluginID(id) {
				return claudeSettings{}, fmt.Errorf("invalid enabledPlugins")
			}
		}
		result.enabledPlugins = plugins
	}
	return result, nil
}

func rawBoolMap(raw json.RawMessage) (map[string]bool, bool) {
	object, err := decodeUniqueJSONObject(raw)
	if err != nil {
		return nil, false
	}
	result := make(map[string]bool, len(object))
	for key, value := range object {
		parsed, ok := rawBool(value)
		if !ok {
			return nil, false
		}
		result[key] = parsed
	}
	return result, true
}

func validClaudePluginID(id string) bool {
	if id == "" || !utf8.ValidString(id) || strings.Count(id, "@") != 1 || strings.HasPrefix(id, "@") || strings.HasSuffix(id, "@") {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

type parsedClaudeEntries struct {
	valid   []claudePolicyEntry
	invalid int
}

func parseClaudeEntries(raw json.RawMessage, allow, tolerateEntries bool) (parsedClaudeEntries, bool, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return parsedClaudeEntries{}, true, nil
	}
	if len(values) > MaxPolicyEntries {
		return parsedClaudeEntries{}, false, fmt.Errorf("too many MCP policy entries")
	}
	result := parsedClaudeEntries{valid: make([]claudePolicyEntry, 0, len(values))}
	for _, value := range values {
		entry, err := parseClaudeEntry(value, allow)
		if err != nil {
			if tolerateEntries {
				result.invalid++
				continue
			}
			return parsedClaudeEntries{}, false, err
		}
		result.valid = appendUniqueClaudeEntry(result.valid, entry)
	}
	return result, false, nil
}

func parseClaudeEntry(raw json.RawMessage, allow bool) (claudePolicyEntry, error) {
	object, err := decodeUniqueJSONObject(raw)
	if err != nil || len(object) != 1 {
		return claudePolicyEntry{}, fmt.Errorf("invalid MCP policy entry")
	}
	if value, exists := object["serverName"]; exists {
		name, ok := rawString(value)
		if !ok || name == "" || allow && !allowedClaudeName.MatchString(name) {
			return claudePolicyEntry{}, fmt.Errorf("invalid MCP server name entry")
		}
		return claudePolicyEntry{kind: claudeEntryName, name: name}, nil
	}
	if value, exists := object["serverCommand"]; exists {
		command, ok := rawStringSlice(value)
		if !ok || len(command) == 0 || len(command) > MaxPolicyValues || command[0] == "" {
			return claudePolicyEntry{}, fmt.Errorf("invalid MCP server command entry")
		}
		return claudePolicyEntry{kind: claudeEntryCommand, command: command}, nil
	}
	if value, exists := object["serverUrl"]; exists {
		pattern, ok := rawString(value)
		if !ok || !validClaudeURLPattern(pattern) {
			return claudePolicyEntry{}, fmt.Errorf("invalid MCP server URL entry")
		}
		return claudePolicyEntry{kind: claudeEntryURL, url: pattern}, nil
	}
	return claudePolicyEntry{}, fmt.Errorf("invalid MCP policy entry")
}

func validClaudeURLPattern(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	separator := strings.Index(value, "://")
	if separator <= 0 {
		return false
	}
	rest := value[separator+3:]
	if rest == "" {
		return false
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	return rest != ""
}

func optionalStringList(root map[string]json.RawMessage, field string) ([]string, error) {
	raw, exists := root[field]
	if !exists {
		return nil, nil
	}
	values, ok := rawStringSlice(raw)
	if !ok || len(values) > MaxPolicyEntries {
		return nil, fmt.Errorf("invalid %s", field)
	}
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("invalid %s", field)
		}
	}
	return uniqueStrings(values), nil
}

func mergeManagedClaudeSettings(base *claudeSettings, higher claudeSettings) {
	base.sourceNonempty = base.sourceNonempty || higher.sourceNonempty
	if higher.allowLockdown {
		base.allowPresent = true
		base.allowLockdown = true
		base.allowed = nil
	}
	if higher.allowPresent {
		base.allowPresent = true
		if !base.allowLockdown {
			base.allowed = appendUniqueClaudeEntries(base.allowed, higher.allowed...)
		}
	}
	base.denied = appendUniqueClaudeEntries(base.denied, higher.denied...)
	base.disabledProject = appendUniqueStrings(base.disabledProject, higher.disabledProject...)
	base.disabledAll = appendUniqueStrings(base.disabledAll, higher.disabledAll...)
	if higher.managedOnlySet {
		base.managedOnlySet = true
		base.managedOnly = higher.managedOnly
	}
	if higher.helperPresent {
		base.helperPresent = true
	}
	if len(higher.environment) != 0 {
		if base.environment == nil {
			base.environment = make(map[string]string)
		}
		for name, value := range higher.environment {
			base.environment[name] = value
		}
	}
	if higher.enabledPlugins != nil {
		if base.enabledPlugins == nil {
			base.enabledPlugins = make(map[string]bool)
		}
		for id, enabled := range higher.enabledPlugins {
			base.enabledPlugins[id] = enabled
		}
	}
	base.strippedAllowed += higher.strippedAllowed
}

func compileClaudePolicy(goos string, startup []string, managed, user, project, local claudeSettings, state claudeProjectState) claudePolicy {
	result := claudePolicy{
		disabledAll:             make(map[string]struct{}),
		disabledProject:         make(map[string]struct{}),
		stateDisabledNonProject: make(map[string]struct{}),
		stateDisabledProject:    make(map[string]struct{}),
		pluginDenials:           make(map[string]struct{}),
	}
	for id, enabled := range managed.enabledPlugins {
		if !enabled {
			result.pluginDenials[id] = struct{}{}
		}
	}
	if managed.allowLockdown {
		result.allowPresent = true
		result.allowEntries = nil
	} else if managed.managedOnly {
		result.allowPresent = managed.allowPresent
		result.allowEntries = appendUniqueClaudeEntries(nil, managed.allowed...)
	} else {
		result.allowPresent = managed.allowPresent || user.allowPresent || project.allowPresent || local.allowPresent
		result.allowEntries = appendUniqueClaudeEntries(nil, managed.allowed...)
		result.allowEntries = appendUniqueClaudeEntries(result.allowEntries, user.allowed...)
		result.allowEntries = appendUniqueClaudeEntries(result.allowEntries, project.allowed...)
		result.allowEntries = appendUniqueClaudeEntries(result.allowEntries, local.allowed...)
	}
	result.denyEntries = appendUniqueClaudeEntries(nil, managed.denied...)
	result.denyEntries = appendUniqueClaudeEntries(result.denyEntries, user.denied...)
	result.denyEntries = appendUniqueClaudeEntries(result.denyEntries, project.denied...)
	result.denyEntries = appendUniqueClaudeEntries(result.denyEntries, local.denied...)
	for _, settings := range []claudeSettings{managed, user, project, local} {
		for _, name := range settings.disabledAll {
			result.disabledAll[name] = struct{}{}
		}
		for _, name := range settings.disabledProject {
			result.disabledProject[name] = struct{}{}
		}
	}
	for _, name := range state.disabledNonProject {
		result.stateDisabledNonProject[name] = struct{}{}
	}
	for _, name := range state.disabledProject {
		result.stateDisabledProject[name] = struct{}{}
	}

	startupEnvironment := newEnvironment(goos, startup)
	serverEnvironment := startupEnvironment.clone()
	serverEnvironment.merge(user.environment)
	serverEnvironment.merge(project.environment)
	serverEnvironment.merge(local.environment)
	serverEnvironment.merge(managed.environment)
	allowEnvironment := startupEnvironment.clone()
	allowEnvironment.merge(managed.environment)
	denyEnvironment := allowEnvironment.clone()
	denyEnvironment.fillMissing(user.environment)
	result.serverEnvironment = serverEnvironment.lookup
	result.allowEnvironment = allowEnvironment.lookup
	result.denyEnvironment = denyEnvironment.lookup
	result.runtimeEnvironment = serverEnvironment.clone()
	return result
}

func (policy claudePolicy) allowed(request mcpnative.PolicyRequest) (bool, error) {
	if policy.unavailable {
		return false, ErrClaudePolicyUnavailable
	}
	if policy.managedExclusive && request.Source != mcpnative.SourceClaudeManaged {
		return false, nil
	}
	if _, denied := policy.disabledAll[request.Name]; denied {
		return false, nil
	}
	if request.Scope == mcpnative.ScopeProject {
		if _, denied := policy.disabledProject[request.Name]; denied {
			return false, nil
		}
		if _, denied := policy.stateDisabledProject[request.Name]; denied {
			return false, nil
		}
	} else if _, denied := policy.stateDisabledNonProject[request.Name]; denied {
		return false, nil
	}
	for _, entry := range policy.denyEntries {
		matched, err := entry.matches(request, mcpnative.ClaudePolicyDeny, policy.serverEnvironment, policy.denyEnvironment)
		if err != nil {
			return false, ErrClaudePolicyUnavailable
		}
		if matched {
			return false, nil
		}
	}
	if !policy.allowPresent {
		return true, nil
	}
	strictKind := claudeEntryKind(0)
	switch request.Transport {
	case mcpnative.TransportStdio:
		for _, entry := range policy.allowEntries {
			if entry.kind == claudeEntryCommand {
				strictKind = claudeEntryCommand
				break
			}
		}
	case mcpnative.TransportHTTP, mcpnative.TransportSSE:
		for _, entry := range policy.allowEntries {
			if entry.kind == claudeEntryURL {
				strictKind = claudeEntryURL
				break
			}
		}
	default:
		return false, nil
	}
	for _, entry := range policy.allowEntries {
		if strictKind != 0 && entry.kind != strictKind {
			continue
		}
		if strictKind == 0 && entry.kind != claudeEntryName {
			continue
		}
		matched, err := entry.matches(request, mcpnative.ClaudePolicyAllow, policy.serverEnvironment, policy.allowEnvironment)
		if err != nil {
			return false, ErrClaudePolicyUnavailable
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func (entry claudePolicyEntry) matches(request mcpnative.PolicyRequest, list mcpnative.ClaudePolicyList, serverEnvironment, policyEnvironment mcpnative.EnvironmentLookup) (bool, error) {
	switch entry.kind {
	case claudeEntryName:
		return hmac.Equal([]byte(entry.name), []byte(request.Name)), nil
	case claudeEntryCommand:
		if request.Transport != mcpnative.TransportStdio {
			return false, nil
		}
		return request.CommandMatchesClaude(entry.command, mcpnative.ClaudePolicyExpansion{
			List: list, ServerEnvironment: serverEnvironment, PolicyEnvironment: policyEnvironment,
		})
	case claudeEntryURL:
		if request.Transport != mcpnative.TransportHTTP && request.Transport != mcpnative.TransportSSE {
			return false, nil
		}
		return request.URLMatchesClaudePatternExpanded(entry.url, mcpnative.ClaudePolicyExpansion{
			List: list, ServerEnvironment: serverEnvironment, PolicyEnvironment: policyEnvironment,
		})
	default:
		return false, fmt.Errorf("invalid MCP policy entry")
	}
}

type environment struct {
	goos   string
	values map[string]string
}

func newEnvironment(goos string, entries []string) environment {
	if entries == nil {
		entries = os.Environ()
	}
	result := environment{goos: goos, values: make(map[string]string)}
	for _, entry := range entries {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			continue
		}
		result.values[result.key(entry[:separator])] = entry[separator+1:]
	}
	return result
}

func (env environment) key(name string) string {
	if env.goos == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func (env environment) clone() environment {
	result := environment{goos: env.goos, values: make(map[string]string, len(env.values))}
	for name, value := range env.values {
		result.values[name] = value
	}
	return result
}

func (env environment) merge(values map[string]string) {
	for name, value := range values {
		env.values[env.key(name)] = value
	}
}

func (env environment) fillMissing(values map[string]string) {
	for name, value := range values {
		key := env.key(name)
		if _, exists := env.values[key]; !exists {
			env.values[key] = value
		}
	}
}

func (env environment) lookup(name string) (string, bool) {
	value, ok := env.values[env.key(name)]
	// Claude's ${VAR:-default} form treats an empty value like an unset value,
	// as does the runtime expansion capability below. Returning false keeps
	// policy comparison and eventual execution on the same interpretation.
	return value, ok && value != ""
}

func validEnvironmentName(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, character := range name {
		if character != '_' && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func appendUniqueClaudeEntries(base []claudePolicyEntry, values ...claudePolicyEntry) []claudePolicyEntry {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, entry := range base {
		seen[claudeEntryKey(entry)] = struct{}{}
	}
	for _, entry := range values {
		key := claudeEntryKey(entry)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		base = append(base, entry)
	}
	return base
}

func appendUniqueClaudeEntry(base []claudePolicyEntry, value claudePolicyEntry) []claudePolicyEntry {
	return appendUniqueClaudeEntries(base, value)
}

func claudeEntryKey(entry claudePolicyEntry) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte{byte(entry.kind)})
	writeHashString(hash, entry.name)
	writeHashString(hash, entry.url)
	for _, part := range entry.command {
		writeHashString(hash, part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeHashString(writer hashWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write([]byte(value))
}

func uniqueStrings(values []string) []string {
	return appendUniqueStrings(nil, values...)
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}
