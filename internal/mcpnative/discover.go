package mcpnative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type configFile struct {
	path     string
	realPath string
	data     []byte
}

type readError struct {
	code    string
	message string
}

func (e *readError) Error() string { return e.message }

type candidate struct {
	server     Server
	precedence int
}

type codexConfigLayer struct {
	servers    map[string]any
	provenance Provenance
	trustRoot  string
	precedence int
}

type collector struct {
	winners                map[string]candidate
	diagnostics            []Diagnostic
	quarantines            []Quarantine
	filesRead              int
	bytesRead              int64
	attempts               int
	claudeDisabled         map[string]struct{}
	claudeProjectDisabled  map[string]struct{}
	claudeManagedExclusive bool
	codexLayers            []codexConfigLayer
}

const (
	claudeUserPrecedence        = 10
	claudeProjectPrecedenceBase = 20
	claudeLocalPrecedence       = 1000
	claudeManagedPrecedence     = 2000
)

// Discover reads the native declaration surfaces under opts. It performs
// no network or process operation. Codex tables are recursively merged from
// low to high precedence, matching Codex's native TOML layer semantics. Claude
// project/local entries use its documented whole-entry precedence. Codex and
// Claude IDs stay dialect-qualified so an equal native name is reported rather
// than resolved by an invented cross-client precedence.
func Discover(opts Options) Result {
	c := collector{
		winners:               make(map[string]candidate),
		claudeDisabled:        make(map[string]struct{}),
		claudeProjectDisabled: make(map[string]struct{}),
	}
	// Authoritative system configuration is read before user-controlled files
	// and even before user roots are normalized, so lower layers cannot exhaust
	// discovery budgets or make an irrelevant home failure suppress it.
	if strings.TrimSpace(opts.ClaudeManagedMCPPath) != "" {
		c.loadClaudeManaged(opts.ClaudeManagedMCPPath)
	}
	home, ok := normalizeRoot(opts.HomeDir, "home", &c.diagnostics)
	workspace, workspaceOK := normalizeRoot(opts.Workspace, "workspace", &c.diagnostics)
	if strings.TrimSpace(opts.HomeDir) != "" && !ok {
		if opts.CodexSnapshot == nil && strings.TrimSpace(opts.CodexConfigDir) == "" {
			c.quarantine(DialectCodex, 1<<30, opts.HomeDir, "unresolved-home")
		}
		if strings.TrimSpace(opts.ClaudeStatePath) == "" && !c.claudeManagedExclusive {
			c.quarantine(DialectClaude, 1<<30, opts.HomeDir, "unresolved-home")
		}
	}
	if strings.TrimSpace(opts.Workspace) != "" && !workspaceOK {
		c.quarantine(DialectCodex, 1<<30, opts.Workspace, "unresolved-workspace")
		if !c.claudeManagedExclusive {
			c.quarantine(DialectClaude, 1<<30, opts.Workspace, "unresolved-workspace")
		}
	}
	current := workspace
	if workspaceOK && strings.TrimSpace(opts.CurrentDir) != "" {
		if resolved, currentOK := normalizeRoot(opts.CurrentDir, "current-directory", &c.diagnostics); currentOK {
			if withinRoot(workspace, resolved) {
				current = resolved
			} else {
				c.diagnostics = append(c.diagnostics, Diagnostic{
					Severity: SeverityError, Code: "current-directory-outside-workspace", Path: resolved,
					Message: "current directory is outside the declared workspace; nested project config is ignored",
				})
				c.quarantine(DialectCodex, 1<<30, resolved, "current-directory-outside-workspace")
			}
		} else {
			c.quarantine(DialectCodex, 1<<30, opts.CurrentDir, "unresolved-current-directory")
		}
	} else if !workspaceOK && strings.TrimSpace(opts.CurrentDir) != "" {
		c.diagnostics = append(c.diagnostics, Diagnostic{
			Severity: SeverityError, Code: "current-directory-without-workspace", Path: opts.CurrentDir,
			Message: "current directory cannot be evaluated without a valid workspace",
		})
		c.quarantine(DialectCodex, 1<<30, opts.CurrentDir, "current-directory-without-workspace")
	}

	codexUserPath := ""
	if strings.TrimSpace(opts.CodexConfigDir) != "" {
		codexUserPath = filepath.Join(opts.CodexConfigDir, "config.toml")
	} else if ok {
		codexUserPath = filepath.Join(home, ".codex", "config.toml")
	}
	claudeStatePath := ""
	if strings.TrimSpace(opts.ClaudeStatePath) != "" {
		claudeStatePath = opts.ClaudeStatePath
	} else if ok {
		claudeStatePath = filepath.Join(home, ".claude.json")
	}
	if codexUserPath != "" && opts.CodexSnapshot == nil {
		c.loadCodex(codexUserPath, "", Provenance{
			Dialect: DialectCodex, Scope: ScopeUser, Source: SourceCodexUser,
		}, "", 10)
	}
	if claudeStatePath != "" && !c.claudeManagedExclusive {
		c.loadClaudeHome(claudeStatePath, workspace, workspaceOK)
	}
	if workspaceOK {
		directories := configDirectories(workspace, current)
		maxProjectDirectories := (MaxConfigCandidates - 3) / 2
		if len(directories) > maxProjectDirectories {
			c.diagnostics = append(c.diagnostics, Diagnostic{
				Severity: SeverityError, Code: "discovery-budget-exceeded", Path: current,
				Message: "nested native project configuration discovery exceeds its candidate budget",
			})
			c.quarantine(DialectCodex, 1<<30, current, "discovery-budget-exceeded")
			if !c.claudeManagedExclusive {
				c.quarantine(DialectClaude, 1<<30, current, "discovery-budget-exceeded")
			}
			directories = directories[:maxProjectDirectories]
		}
		for index, directory := range directories {
			if opts.CodexSnapshot == nil {
				c.loadCodex(filepath.Join(directory, ".codex", "config.toml"), workspace, Provenance{
					Dialect: DialectCodex, Scope: ScopeProject, Source: SourceCodexProject,
				}, workspace, 20+index)
			}
			if !c.claudeManagedExclusive {
				c.loadClaudeProject(filepath.Join(directory, ".mcp.json"), workspace, Provenance{
					Dialect: DialectClaude, Scope: ScopeProject, Source: SourceClaudeProject,
				}, workspace, claudeProjectPrecedenceBase+index)
			}
		}
	}
	if opts.CodexSnapshot != nil {
		c.loadCodexSnapshot(opts.CodexSnapshot, workspace, current)
	} else {
		c.finishCodex()
		if c.hasDialectWinner(DialectCodex) {
			c.codexSnapshotDiagnostic("codex-layer-stack-unavailable", "direct Codex entries are inventory-only until the authoritative effective layer stack is available")
		}
	}
	c.applyClaudeDenials()

	servers := make([]Server, 0, len(c.winners))
	authoritative := make(map[string]Server, len(c.winners))
	precedence := make(map[string]int, len(c.winners))
	for id, winner := range c.winners {
		authoritative[id] = deepCloneServer(winner.server)
		precedence[id] = winner.precedence
		servers = append(servers, deepCloneServer(winner.server))
	}
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].ID != servers[j].ID {
			return servers[i].ID < servers[j].ID
		}
		return servers[i].Provenance.Path < servers[j].Provenance.Path
	})
	c.crossDialectCollisions(servers)
	sortDiagnostics(c.diagnostics)
	sort.Slice(c.quarantines, func(i, j int) bool {
		if c.quarantines[i].Dialect != c.quarantines[j].Dialect {
			return c.quarantines[i].Dialect < c.quarantines[j].Dialect
		}
		if c.quarantines[i].Precedence != c.quarantines[j].Precedence {
			return c.quarantines[i].Precedence < c.quarantines[j].Precedence
		}
		if c.quarantines[i].Path != c.quarantines[j].Path {
			return c.quarantines[i].Path < c.quarantines[j].Path
		}
		return c.quarantines[i].Code < c.quarantines[j].Code
	})
	if servers == nil {
		servers = []Server{}
	}
	return Result{
		Servers: servers, Diagnostics: c.diagnostics,
		Quarantines:   append([]Quarantine(nil), c.quarantines...),
		authoritative: authoritative, precedence: precedence,
		quarantine: append([]Quarantine(nil), c.quarantines...),
	}
}

func (c *collector) loadClaudeManaged(path string) {
	const precedence = claudeManagedPrecedence
	if _, err := os.Lstat(path); err != nil && os.IsNotExist(err) {
		return
	}
	// Any object at the managed path activates exclusivity. Read failures are
	// quarantined rather than permitting fallback to a user-controlled source.
	c.claudeManagedExclusive = true
	file, found, err := c.readConfig(path, "")
	if err != nil {
		c.readDiagnostic(path, DialectClaude, precedence, err)
		return
	}
	if !found {
		return
	}
	base := Provenance{
		Dialect: DialectClaude, Scope: ScopeManaged, Source: SourceClaudeManaged,
		Path: file.path, RealPath: file.realPath,
	}
	servers, diagnostics := parseClaudeServers(file.data, base, "", true)
	c.diagnostics = append(c.diagnostics, diagnostics...)
	// managed-mcp.json is exclusive even when it is empty or malformed. A
	// malformed file additionally quarantines Claude materialization rather
	// than falling back to a user-controlled lower layer.
	c.removeDialect(DialectClaude)
	if hasStructuralFailure(diagnostics) {
		c.quarantine(DialectClaude, precedence, file.path, firstStructuralCode(diagnostics))
	}
	for _, server := range servers {
		c.add(server, precedence)
	}
	c.diagnostics = append(c.diagnostics, Diagnostic{
		Severity: SeverityWarning, Code: "managed-mcp-exclusive", Path: file.path,
		Message: "Claude managed-mcp.json is present; user, project, and local server declarations are suppressed",
	})
}

func (c *collector) removeDialect(dialect Dialect) {
	for id, candidate := range c.winners {
		if candidate.server.Provenance.Dialect == dialect {
			delete(c.winners, id)
		}
	}
}

func configDirectories(workspace, current string) []string {
	if workspace == "" || current == "" || !withinRoot(workspace, current) {
		return nil
	}
	rel, err := filepath.Rel(workspace, current)
	if err != nil {
		return []string{workspace}
	}
	directories := []string{workspace}
	if rel == "." {
		return directories
	}
	directory := workspace
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		directory = filepath.Join(directory, component)
		directories = append(directories, directory)
	}
	return directories
}

func normalizeRoot(root, label string, diagnostics *[]Diagnostic) (string, bool) {
	if strings.TrimSpace(root) == "" {
		return "", false
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		*diagnostics = append(*diagnostics, Diagnostic{
			Severity: SeverityError, Code: "invalid-" + label, Path: root,
			Message: label + " path cannot be made absolute",
		})
		return "", false
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		*diagnostics = append(*diagnostics, Diagnostic{
			Severity: SeverityError, Code: "unreadable-" + label, Path: filepath.Clean(abs),
			Message: label + " path does not resolve to a readable directory",
		})
		return "", false
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		*diagnostics = append(*diagnostics, Diagnostic{
			Severity: SeverityError, Code: "invalid-" + label, Path: filepath.Clean(abs),
			Message: label + " path is not a directory",
		})
		return "", false
	}
	return filepath.Clean(real), true
}

func (c *collector) loadCodex(path, allowedRoot string, base Provenance, trustRoot string, precedence int) {
	file, found, err := c.readConfig(path, allowedRoot)
	if err != nil {
		c.readDiagnostic(path, base.Dialect, precedence, err)
		return
	}
	if !found {
		return
	}
	base.Path, base.RealPath = file.path, file.realPath
	servers, diagnostics, valid := decodeCodexServers(file.data, base)
	if valid {
		var pathDiagnostics []Diagnostic
		servers, pathDiagnostics, valid = normalizeCodexLayerPaths(servers, filepath.Dir(file.realPath), base)
		diagnostics = append(diagnostics, pathDiagnostics...)
	}
	c.diagnostics = append(c.diagnostics, diagnostics...)
	if hasStructuralFailure(diagnostics) {
		c.quarantine(base.Dialect, precedence, file.path, firstStructuralCode(diagnostics))
	}
	if valid {
		c.codexLayers = append(c.codexLayers, codexConfigLayer{
			servers: servers, provenance: base, trustRoot: trustRoot, precedence: precedence,
		})
	}
}

// normalizeCodexLayerPaths runs before recursive merge because Codex resolves
// path-typed values relative to the directory of the layer that declared
// them. Deferring this until after merge would let an inherited user cwd be
// reinterpreted relative to an untrusted workspace.
func normalizeCodexLayerPaths(servers map[string]any, layerDir string, base Provenance) (map[string]any, []Diagnostic, bool) {
	result := make(map[string]any, len(servers))
	for name, raw := range servers {
		entry, ok := asMap(raw)
		if !ok {
			result[name] = raw
			continue
		}
		rawCWD, exists := entry["cwd"]
		if !exists {
			result[name] = entry
			continue
		}
		cwd, isString := asString(rawCWD)
		if !isString || filepath.IsAbs(cwd) {
			result[name] = entry
			continue
		}
		resolved, err := filepath.Abs(filepath.Join(layerDir, filepath.FromSlash(cwd)))
		if err != nil {
			return nil, []Diagnostic{{
				Severity: SeverityError, Code: "invalid-layer-relative-path", Path: base.Path,
				Entry: safeToken(name), Field: "cwd", Message: "Codex layer-relative cwd cannot be resolved",
			}}, false
		}
		entry["cwd"] = filepath.Clean(resolved)
		result[name] = entry
	}
	return result, nil, true
}

func (c *collector) finishCodex() {
	type mergedEntry struct {
		value        any
		contributors []codexConfigLayer
	}
	merged := make(map[string]mergedEntry)
	for _, layer := range c.codexLayers {
		for _, name := range sortedKeys(layer.servers) {
			higher := cloneTOMLValue(layer.servers[name])
			current, exists := merged[name]
			if !exists {
				merged[name] = mergedEntry{value: higher, contributors: []codexConfigLayer{layer}}
				continue
			}
			lowerMap, lowerOK := asMap(current.value)
			higherMap, higherOK := asMap(higher)
			if lowerOK && higherOK {
				current.value = mergeTOMLMaps(lowerMap, higherMap)
				current.contributors = append(current.contributors, layer)
				merged[name] = current
				continue
			}
			// Native merging replaces unlike values rather than attempting to
			// retain fields from a table that ceased to be a table.
			merged[name] = mergedEntry{value: higher, contributors: []codexConfigLayer{layer}}
		}
	}
	for _, name := range sortedKeysAny(merged) {
		entry := merged[name]
		highest := entry.contributors[len(entry.contributors)-1]
		provenance := highest.provenance
		provenance.ConfigKey = "mcp_servers." + name
		provenance.ContributingLayers = make([]LayerProvenance, 0, len(entry.contributors))
		trustRoot := ""
		requiresTrust := false
		for _, contributor := range entry.contributors {
			provenance.ContributingLayers = append(provenance.ContributingLayers, LayerProvenance{
				Scope: contributor.provenance.Scope, Source: contributor.provenance.Source,
				Path: contributor.provenance.Path, RealPath: contributor.provenance.RealPath,
			})
			if requiresWorkspaceTrust(contributor.provenance.Scope) {
				requiresTrust = true
				trustRoot = contributor.trustRoot
			}
		}
		raw, ok := asMap(entry.value)
		var server Server
		var diagnostics []Diagnostic
		if !ok {
			server = invalidShell(name, provenance, trustRoot, entry.value)
			diagnostics = []Diagnostic{{
				Severity: SeverityError, Code: "invalid-server-entry", Path: provenance.Path,
				Entry: safeToken(name), Message: "merged server entry must be an object/table",
			}}
		} else {
			server, diagnostics = parseEntry(name, raw, provenance, trustRoot)
		}
		server.ExecutionTrustRequired = requiresTrust
		if !requiresTrust {
			server.TrustRoot = ""
		}
		c.diagnostics = append(c.diagnostics, diagnostics...)
		c.add(server, highest.precedence)
	}
}

func mergeTOMLMaps(lower, higher map[string]any) map[string]any {
	result := make(map[string]any, len(lower)+len(higher))
	for key, value := range lower {
		result[key] = cloneTOMLValue(value)
	}
	for key, value := range higher {
		if lowerValue, exists := result[key]; exists {
			lowerMap, lowerOK := asMap(lowerValue)
			higherMap, higherOK := asMap(value)
			if lowerOK && higherOK {
				result[key] = mergeTOMLMaps(lowerMap, higherMap)
				continue
			}
		}
		result[key] = cloneTOMLValue(value)
	}
	return result
}

func cloneTOMLValue(value any) any {
	if object, ok := asMap(value); ok {
		return mergeTOMLMaps(nil, object)
	}
	if values, ok := asAnyList(value); ok {
		cloned := make([]any, len(values))
		for index := range values {
			cloned[index] = cloneTOMLValue(values[index])
		}
		return cloned
	}
	return value
}

func sortedKeysAny[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (c *collector) loadClaudeProject(path, allowedRoot string, base Provenance, trustRoot string, precedence int) {
	file, found, err := c.readConfig(path, allowedRoot)
	if err != nil {
		c.readDiagnostic(path, base.Dialect, precedence, err)
		return
	}
	if !found {
		return
	}
	base.Path, base.RealPath = file.path, file.realPath
	servers, diagnostics := parseClaudeServers(file.data, base, trustRoot, true)
	c.diagnostics = append(c.diagnostics, diagnostics...)
	if hasStructuralFailure(diagnostics) {
		c.quarantine(base.Dialect, precedence, file.path, firstStructuralCode(diagnostics))
	}
	for _, server := range servers {
		c.add(server, precedence)
	}
}

func (c *collector) loadClaudeHome(path, workspace string, haveWorkspace bool) {
	precedence := claudeUserPrecedence
	if haveWorkspace {
		precedence = claudeLocalPrecedence
	}
	file, found, err := c.readConfig(path, "")
	if err != nil {
		c.readDiagnostic(path, DialectClaude, precedence, err)
		return
	}
	if !found {
		return
	}
	userBase := Provenance{
		Dialect: DialectClaude, Scope: ScopeUser, Source: SourceClaudeUser,
		Path: file.path, RealPath: file.realPath,
	}
	users, locals, denials, diagnostics := parseClaudeHome(file.data, userBase, workspace, haveWorkspace)
	c.diagnostics = append(c.diagnostics, diagnostics...)
	if hasStructuralFailure(diagnostics) {
		c.quarantine(DialectClaude, precedence, file.path, firstStructuralCode(diagnostics))
	}
	for _, server := range users {
		c.add(server, claudeUserPrecedence)
	}
	for _, server := range locals {
		c.add(server, claudeLocalPrecedence)
	}
	for _, name := range denials.All {
		c.claudeDisabled[name] = struct{}{}
	}
	for _, name := range denials.Project {
		c.claudeProjectDisabled[name] = struct{}{}
	}
}

func (c *collector) applyClaudeDenials() {
	for id, current := range c.winners {
		server := current.server
		if server.Provenance.Dialect != DialectClaude {
			continue
		}
		reason := ""
		if _, denied := c.claudeDisabled[server.Name]; denied && server.Provenance.Scope != ScopeProject {
			reason = "projects.<workspace>.disabledMcpServers"
		}
		if _, denied := c.claudeProjectDisabled[server.Name]; denied && server.Provenance.Scope == ScopeProject {
			reason = "projects.<workspace>.disabledMcpjsonServers"
		}
		if reason == "" {
			continue
		}
		server.Enabled = false
		server.EnabledSet = true
		server.EnablementSource = reason
		appendDefinitionContext(&server, "claude-enable-deny:"+reason)
		current.server = server
		c.winners[id] = current
		c.diagnostics = append(c.diagnostics, Diagnostic{
			Severity: SeverityWarning, Code: "native-server-disabled", Path: server.Provenance.Path,
			Entry: safeToken(server.Name), Field: reason,
			Message: "Claude project state disables this server; Switchboard activation cannot override the native deny",
		})
	}
}

func (c *collector) add(server Server, precedence int) {
	if c.claudeManagedExclusive && server.Provenance.Dialect == DialectClaude && server.Provenance.Scope != ScopeManaged {
		return
	}
	current, exists := c.winners[server.ID]
	if !exists {
		c.winners[server.ID] = candidate{server: server, precedence: precedence}
		return
	}
	if current.precedence == precedence {
		// Equal-precedence duplicates can arise when ~/.claude.json contains
		// multiple path spellings resolving to the same workspace. Picking one
		// as executable would make map/path ordering a permission decision.
		if server.Provenance.ConfigKey < current.server.Provenance.ConfigKey {
			current.server = server
		}
		current.server.Supported = false
		c.winners[server.ID] = current
		c.diagnostics = append(c.diagnostics, Diagnostic{
			Severity: SeverityError,
			Code:     "ambiguous-entry",
			Path:     current.server.Provenance.Path,
			Entry:    safeToken(server.Name),
			Message:  "multiple declarations have equal native precedence; the entry is disabled",
		})
		return
	}
	if precedence > current.precedence {
		c.diagnostics = append(c.diagnostics, shadowDiagnostic(current.server, server))
		c.winners[server.ID] = candidate{server: server, precedence: precedence}
		return
	}
	c.diagnostics = append(c.diagnostics, shadowDiagnostic(server, current.server))
}

func shadowDiagnostic(shadowed, winner Server) Diagnostic {
	return Diagnostic{
		Severity: SeverityWarning,
		Code:     "entry-shadowed",
		Path:     shadowed.Provenance.Path,
		Entry:    safeToken(shadowed.Name),
		Message:  fmt.Sprintf("whole entry is replaced by the higher-precedence %s scope declaration", winner.Provenance.Scope),
	}
}

func (c *collector) crossDialectCollisions(servers []Server) {
	byName := make(map[string][]Server)
	for _, server := range servers {
		byName[server.Name] = append(byName[server.Name], server)
	}
	for name, entries := range byName {
		if len(entries) < 2 {
			continue
		}
		dialects := make(map[Dialect]struct{})
		for _, entry := range entries {
			dialects[entry.Provenance.Dialect] = struct{}{}
		}
		if len(dialects) < 2 {
			continue
		}
		c.diagnostics = append(c.diagnostics, Diagnostic{
			Severity: SeverityWarning,
			Code:     "cross-dialect-name-collision",
			Entry:    safeToken(name),
			Message:  "Codex and Claude declarations share a name; IDs remain dialect-qualified",
		})
	}
}

func (c *collector) readDiagnostic(path string, dialect Dialect, precedence int, err error) {
	var readErr *readError
	if !errors.As(err, &readErr) {
		readErr = &readError{code: "unreadable-config", message: "configuration file could not be read"}
	}
	c.diagnostics = append(c.diagnostics, Diagnostic{
		Severity: SeverityError,
		Code:     readErr.code,
		Path:     filepath.Clean(path),
		Message:  readErr.message,
	})
	c.quarantine(dialect, precedence, path, readErr.code)
}

func (c *collector) quarantine(dialect Dialect, precedence int, path, code string) {
	item := Quarantine{Dialect: dialect, Precedence: precedence, Path: filepath.Clean(path), Code: code}
	for _, existing := range c.quarantines {
		if existing == item {
			return
		}
	}
	c.quarantines = append(c.quarantines, item)
}

func (c *collector) readConfig(path, allowedRoot string) (configFile, bool, error) {
	if c.attempts >= MaxConfigCandidates {
		return configFile{}, false, &readError{"discovery-budget-exceeded", "native configuration discovery exceeds its candidate budget"}
	}
	c.attempts++
	remaining := MaxTotalConfigBytes - c.bytesRead
	file, found, err := readConfig(path, allowedRoot, remaining, c.filesRead < MaxConfigFiles)
	if err != nil || !found {
		return file, found, err
	}
	c.filesRead++
	c.bytesRead += int64(len(file.data))
	return file, true, nil
}

func readConfig(path, allowedRoot string, remainingBytes int64, allowFile bool) (configFile, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return configFile{}, false, &readError{"invalid-config-path", "configuration path cannot be made absolute"}
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return configFile{}, false, nil
		}
		return configFile{}, false, &readError{"unreadable-config", "configuration file metadata could not be read"}
	}
	if !allowFile || remainingBytes < 0 {
		return configFile{}, false, &readError{"discovery-budget-exceeded", "native configuration discovery exceeds its aggregate file or byte budget"}
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return configFile{}, false, &readError{"unreadable-config", "configuration file symlinks could not be resolved"}
	}
	real = filepath.Clean(real)
	if allowedRoot != "" && !withinRoot(allowedRoot, real) {
		return configFile{}, false, &readError{"config-escapes-workspace", "project configuration resolves outside the workspace"}
	}
	before, err := os.Lstat(real)
	if err != nil {
		return configFile{}, false, &readError{"unreadable-config", "configuration file metadata could not be read"}
	}
	if !before.Mode().IsRegular() {
		return configFile{}, false, &readError{"non-regular-config", "configuration path is not a regular file"}
	}
	if before.Size() > remainingBytes {
		return configFile{}, false, &readError{"discovery-budget-exceeded", "native configuration discovery exceeds its aggregate file or byte budget"}
	}
	f, err := openConfigFile(real)
	if err != nil {
		return configFile{}, false, &readError{"unreadable-config", "configuration file could not be opened"}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return configFile{}, false, &readError{"unreadable-config", "configuration file metadata could not be read"}
	}
	if !info.Mode().IsRegular() {
		return configFile{}, false, &readError{"non-regular-config", "configuration path is not a regular file"}
	}
	if !os.SameFile(before, info) {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration file changed while it was opened"}
	}
	postReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration path changed while it was opened"}
	}
	postReal = filepath.Clean(postReal)
	postInfo, err := os.Stat(postReal)
	if err != nil || !os.SameFile(info, postInfo) {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration path changed while it was opened"}
	}
	if allowedRoot != "" && !withinRoot(allowedRoot, postReal) {
		return configFile{}, false, &readError{"config-escapes-workspace", "project configuration changed to resolve outside the workspace"}
	}
	if info.Size() > MaxConfigBytes {
		return configFile{}, false, &readError{"config-too-large", "configuration file exceeds the bounded read limit"}
	}
	readLimit := MaxConfigBytes
	if remainingBytes < readLimit {
		readLimit = remainingBytes
	}
	data, err := io.ReadAll(io.LimitReader(f, readLimit+1))
	if err != nil {
		return configFile{}, false, &readError{"unreadable-config", "configuration file could not be read"}
	}
	if int64(len(data)) > MaxConfigBytes {
		return configFile{}, false, &readError{"config-too-large", "configuration file exceeds the bounded read limit"}
	}
	if int64(len(data)) > remainingBytes {
		return configFile{}, false, &readError{"discovery-budget-exceeded", "native configuration discovery exceeds its aggregate file or byte budget"}
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration file changed while it was read"}
	}
	finalReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration path changed while it was read"}
	}
	finalReal = filepath.Clean(finalReal)
	finalInfo, err := os.Stat(finalReal)
	if err != nil || !os.SameFile(after, finalInfo) {
		return configFile{}, false, &readError{"config-changed-during-read", "configuration path changed while it was read"}
	}
	if allowedRoot != "" && !withinRoot(allowedRoot, finalReal) {
		return configFile{}, false, &readError{"config-escapes-workspace", "project configuration changed to resolve outside the workspace"}
	}
	return configFile{path: abs, realPath: finalReal, data: data}, true, nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func parseCodex(data []byte, base Provenance, trustRoot string) ([]Server, []Diagnostic) {
	serversMap, diagnostics, valid := decodeCodexServers(data, base)
	if !valid {
		return nil, diagnostics
	}
	servers, parsed := parseServerMap(serversMap, base, trustRoot, "mcp_servers")
	return servers, append(diagnostics, parsed...)
}

// decodeCodexServers intentionally stops before parsing individual entries.
// Native Codex recursively merges raw TOML tables across its configuration
// stack, so discovery must merge first and normalize the effective entry once.
func decodeCodexServers(data []byte, base Provenance) (map[string]any, []Diagnostic, bool) {
	var root map[string]any
	if _, err := toml.Decode(string(data), &root); err != nil {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "invalid-toml", Path: base.Path,
			Message: "native Codex configuration is not valid TOML",
		}}, false
	}
	values := 0
	if !configTreeWithinBounds(reflect.ValueOf(root), 0, &values, MaxConfigDepth, MaxConfigValues) {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "config-structure-budget-exceeded", Path: base.Path,
			Message: "native Codex configuration exceeds the bounded depth or value limit",
		}}, false
	}
	var diagnostics []Diagnostic
	if hasPluginMCPPolicy(root) {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityWarning, Code: "plugin-mcp-policy-not-imported", Path: base.Path, Field: "plugins.*.mcp_servers",
			Message: "plugin-contributed MCP policy is outside native server discovery and is not used to authorize any server",
		})
	}
	rawServers, exists := root["mcp_servers"]
	if !exists {
		return map[string]any{}, diagnostics, true
	}
	serversMap, ok := asMap(rawServers)
	if !ok {
		return nil, append(diagnostics, Diagnostic{
			Severity: SeverityError, Code: "invalid-server-table", Path: base.Path, Field: "mcp_servers",
			Message: "mcp_servers must be a table of server entries",
		}), false
	}
	if len(serversMap) > MaxServerEntries {
		return nil, append(diagnostics, Diagnostic{
			Severity: SeverityError, Code: "server-entry-budget-exceeded", Path: base.Path, Field: "mcp_servers",
			Message: "native MCP server table exceeds the bounded entry limit",
		}), false
	}
	for name, server := range serversMap {
		values := 0
		if !configTreeWithinBounds(reflect.ValueOf(server), 0, &values, MaxConfigDepth, MaxEntryValues) {
			return nil, append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "entry-value-budget-exceeded", Path: base.Path,
				Entry: safeToken(name), Message: "native MCP server entry exceeds the bounded depth or value limit",
			}), false
		}
	}
	return serversMap, diagnostics, true
}

func hasPluginMCPPolicy(root map[string]any) bool {
	plugins, ok := asMap(root["plugins"])
	if !ok {
		return false
	}
	for _, plugin := range plugins {
		entry, ok := asMap(plugin)
		if !ok {
			continue
		}
		if _, exists := entry["mcp_servers"]; exists {
			return true
		}
	}
	return false
}

func parseClaudeServers(data []byte, base Provenance, trustRoot string, requireField bool) ([]Server, []Diagnostic) {
	root, err := decodeUniqueObject(data)
	if err != nil {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "invalid-json", Path: base.Path,
			Message: "native Claude configuration is not valid duplicate-free JSON",
		}}
	}
	raw, exists := root["mcpServers"]
	if !exists {
		if !requireField {
			return nil, nil
		}
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "missing-server-object", Path: base.Path, Field: "mcpServers",
			Message: "Claude project configuration has no mcpServers object",
		}}
	}
	serversMap, err := rawObject(raw)
	if err != nil {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "invalid-server-object", Path: base.Path, Field: "mcpServers",
			Message: "mcpServers must be an object of server entries",
		}}
	}
	return parseServerRawMap(serversMap, base, trustRoot, "mcpServers")
}

type claudeDenials struct {
	All     []string
	Project []string
}

func parseClaudeHome(data []byte, userBase Provenance, workspace string, haveWorkspace bool) ([]Server, []Server, claudeDenials, []Diagnostic) {
	root, err := decodeUniqueObject(data)
	if err != nil {
		return nil, nil, claudeDenials{}, []Diagnostic{{
			Severity: SeverityError, Code: "invalid-json", Path: userBase.Path,
			Message: "native Claude configuration is not valid duplicate-free JSON",
		}}
	}
	var diagnostics []Diagnostic
	var users []Server
	if raw, ok := root["mcpServers"]; ok {
		serversMap, err := rawObject(raw)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "invalid-server-object", Path: userBase.Path, Field: "mcpServers",
				Message: "mcpServers must be an object of server entries",
			})
		} else {
			var parsed []Diagnostic
			users, parsed = parseServerRawMap(serversMap, userBase, "", "mcpServers")
			diagnostics = append(diagnostics, parsed...)
		}
	}
	if !haveWorkspace {
		return users, nil, claudeDenials{}, diagnostics
	}
	rawProjects, ok := root["projects"]
	if !ok {
		return users, nil, claudeDenials{}, diagnostics
	}
	projects, err := rawObject(rawProjects)
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError, Code: "invalid-projects-object", Path: userBase.Path, Field: "projects",
			Message: "projects must be an object",
		})
		return users, nil, claudeDenials{}, diagnostics
	}
	if len(projects) > MaxProjectEntries {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError, Code: "project-entry-budget-exceeded", Path: userBase.Path, Field: "projects",
			Message: "Claude project state exceeds the bounded entry limit",
		})
		return users, nil, claudeDenials{}, diagnostics
	}
	var locals []Server
	var denials claudeDenials
	matchingProjects, matchErr := matchingWorkspaceKeys(projects, workspace)
	if matchErr != nil {
		matchErr.Path = userBase.Path
		diagnostics = append(diagnostics, *matchErr)
		return users, nil, claudeDenials{}, diagnostics
	}
	for _, projectKey := range matchingProjects {
		project, err := rawObject(projects[projectKey])
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "invalid-project-object", Path: userBase.Path, Field: "projects",
				Message: "matching Claude project state must be an object",
			})
			continue
		}
		if hasForeignState(project) {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityWarning, Code: "foreign-trust-state-ignored", Path: userBase.Path,
				Message: "Claude project trust and remembered server enablement are not imported",
			})
		}
		for _, item := range []struct {
			field  string
			target *[]string
		}{
			{field: "disabledMcpServers", target: &denials.All},
			{field: "disabledMcpjsonServers", target: &denials.Project},
		} {
			rawList, exists := project[item.field]
			if !exists {
				continue
			}
			values, err := rawStringList(rawList)
			if err != nil || !validServerNames(values) {
				diagnostics = append(diagnostics, Diagnostic{
					Severity: SeverityError, Code: "invalid-disabled-server-list", Path: userBase.Path,
					Field:   "projects.<workspace>." + item.field,
					Message: "Claude disabled-server state must be an array of valid server names",
				})
				continue
			}
			*item.target = append(*item.target, values...)
		}
		rawServers, ok := project["mcpServers"]
		if !ok {
			continue
		}
		serversMap, err := rawObject(rawServers)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "invalid-server-object", Path: userBase.Path, Field: "projects.mcpServers",
				Message: "matching project mcpServers must be an object",
			})
			continue
		}
		base := Provenance{
			Dialect: DialectClaude, Scope: ScopeLocal, Source: SourceClaudeLocal,
			Path: userBase.Path, RealPath: userBase.RealPath,
		}
		parsedServers, parsedDiagnostics := parseServerRawMap(serversMap, base, workspace, "projects."+projectKey+".mcpServers")
		locals = append(locals, parsedServers...)
		diagnostics = append(diagnostics, parsedDiagnostics...)
	}
	denials.All = sortedUnique(denials.All)
	denials.Project = sortedUnique(denials.Project)
	return users, locals, denials, diagnostics
}

func rawStringList(raw json.RawMessage) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("not a string array")
	}
	return values, nil
}

func validServerNames(values []string) bool {
	for _, value := range values {
		if validDisplayName(value) != nil {
			return false
		}
	}
	return true
}

func parseServerMap(raw map[string]any, base Provenance, trustRoot, prefix string) ([]Server, []Diagnostic) {
	if len(raw) > MaxServerEntries {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "server-entry-budget-exceeded", Path: base.Path, Field: prefix,
			Message: "native MCP server table exceeds the bounded entry limit",
		}}
	}
	var servers []Server
	var diagnostics []Diagnostic
	for _, name := range sortedKeys(raw) {
		entry, ok := asMap(raw[name])
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "invalid-server-entry", Path: base.Path,
				Entry: safeToken(name), Message: "server entry must be an object/table",
			})
			server := invalidShell(name, withKey(base, prefix, name), trustRoot, raw[name])
			servers = append(servers, server)
			continue
		}
		server, parsed := parseEntry(name, entry, withKey(base, prefix, name), trustRoot)
		servers = append(servers, server)
		diagnostics = append(diagnostics, parsed...)
	}
	return servers, diagnostics
}

func parseServerRawMap(raw map[string]json.RawMessage, base Provenance, trustRoot, prefix string) ([]Server, []Diagnostic) {
	if len(raw) > MaxServerEntries {
		return nil, []Diagnostic{{
			Severity: SeverityError, Code: "server-entry-budget-exceeded", Path: base.Path, Field: prefix,
			Message: "native MCP server object exceeds the bounded entry limit",
		}}
	}
	var servers []Server
	var diagnostics []Diagnostic
	for _, name := range sortedRawKeys(raw) {
		if err := validateUniqueJSONLimits(raw[name], MaxConfigDepth, MaxEntryValues); err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "entry-value-budget-exceeded", Path: base.Path,
				Entry: safeToken(name), Message: "server entry exceeds the bounded depth or value limit",
			})
			servers = append(servers, invalidShell(name, withKey(base, prefix, name), trustRoot, json.RawMessage(raw[name])))
			continue
		}
		entry, err := rawAnyMap(raw[name])
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Code: "invalid-server-entry", Path: base.Path,
				Entry: safeToken(name), Message: "server entry must be an object",
			})
			servers = append(servers, invalidShell(name, withKey(base, prefix, name), trustRoot, json.RawMessage(raw[name])))
			continue
		}
		server, parsed := parseEntry(name, entry, withKey(base, prefix, name), trustRoot)
		servers = append(servers, server)
		diagnostics = append(diagnostics, parsed...)
	}
	return servers, diagnostics
}

func invalidShell(name string, provenance Provenance, trustRoot string, raw any) Server {
	definition, _ := canonicalDefinition(provenance, trustRoot, raw)
	server := Server{
		ID: normalizedServerID(provenance, name), Name: name, Provenance: provenance,
		ExecutionTrustRequired: requiresWorkspaceTrust(provenance.Scope),
		TrustRoot:              trustRoot, Supported: false, Enabled: true, definition: definition,
	}
	if provenance.Scope == ScopeUser {
		server.TrustRoot = ""
	}
	return server
}

func withKey(base Provenance, prefix, name string) Provenance {
	base.ConfigKey = prefix + "." + name
	return base
}

func decodeUniqueObject(data []byte) (map[string]json.RawMessage, error) {
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("not an object")
	}
	return object, nil
}

func validateUniqueJSON(data []byte) error {
	return validateUniqueJSONLimits(data, MaxConfigDepth, MaxConfigValues)
}

func validateUniqueJSONLimits(data []byte, maxDepth, maxValues int) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	values := 0
	if err := consumeJSONValue(decoder, 0, &values, maxDepth, maxValues); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing JSON value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, depth int, values *int, maxDepth, maxValues int) error {
	if depth > maxDepth || *values >= maxValues {
		return errors.New("JSON structure exceeds bounded depth or value count")
	}
	*values++
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder, depth+1, values, maxDepth, maxValues); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1, values, maxDepth, maxValues); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected closing delimiter")
	}
	return nil
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("not an object")
	}
	return object, nil
}

func rawAnyMap(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("not an object")
	}
	return object, nil
}

func sortedRawKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sameWorkspace(projectKey, workspace string) bool {
	if !filepath.IsAbs(projectKey) {
		return false
	}
	projectInfo, projectErr := os.Stat(projectKey)
	workspaceInfo, workspaceErr := os.Stat(workspace)
	if projectErr == nil && workspaceErr == nil {
		return os.SameFile(projectInfo, workspaceInfo)
	}
	project := filepath.Clean(projectKey)
	if real, err := filepath.EvalSymlinks(project); err == nil {
		project = filepath.Clean(real)
	}
	return project == filepath.Clean(workspace)
}

// matchingWorkspaceKeys avoids filesystem work for the normal exact-key case.
// Alias/case-variant compatibility is bounded so a large stale projects map
// cannot turn discovery into thousands of arbitrary Stat/automount requests.
func matchingWorkspaceKeys(projects map[string]json.RawMessage, workspace string) ([]string, *Diagnostic) {
	keys := sortedRawKeys(projects)
	var exact []string
	for _, key := range keys {
		if filepath.IsAbs(key) && filepath.Clean(key) == filepath.Clean(workspace) {
			exact = append(exact, key)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	const maxAliasChecks = 64
	var absolute []string
	for _, key := range keys {
		if filepath.IsAbs(key) {
			absolute = append(absolute, key)
		}
	}
	if len(absolute) > maxAliasChecks {
		return nil, &Diagnostic{
			Severity: SeverityError, Code: "project-alias-budget-exceeded", Field: "projects",
			Message: "matching Claude project aliases exceeds the bounded filesystem-check limit",
		}
	}
	var matched []string
	for _, key := range absolute {
		if sameWorkspace(key, workspace) {
			matched = append(matched, key)
		}
	}
	return matched, nil
}

func hasForeignState(project map[string]json.RawMessage) bool {
	for _, key := range []string{
		"hasTrustDialogAccepted",
		"enabledMcpjsonServers",
		"enableAllProjectMcpServers",
		"enabledMcpServers",
	} {
		if _, ok := project[key]; ok {
			return true
		}
	}
	return false
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		a, b := diagnostics[i], diagnostics[j]
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Entry != b.Entry {
			return a.Entry < b.Entry
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Message < b.Message
	})
}

func hasStructuralFailure(diagnostics []Diagnostic) bool {
	return firstStructuralCode(diagnostics) != ""
}

func firstStructuralCode(diagnostics []Diagnostic) string {
	for _, diagnostic := range diagnostics {
		switch diagnostic.Code {
		case "invalid-toml", "invalid-server-table", "invalid-json", "missing-server-object",
			"invalid-server-object", "invalid-projects-object", "invalid-project-object":
			return diagnostic.Code
		case "invalid-disabled-server-list", "server-entry-budget-exceeded", "project-entry-budget-exceeded",
			"project-alias-budget-exceeded", "entry-value-budget-exceeded":
			return diagnostic.Code
		case "config-structure-budget-exceeded":
			return diagnostic.Code
		case "invalid-layer-relative-path":
			return diagnostic.Code
		}
	}
	return ""
}

func configTreeWithinBounds(value reflect.Value, depth int, count *int, maxDepth, maxValues int) bool {
	if depth > maxDepth || *count >= maxValues {
		return false
	}
	*count++
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return true
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Len() > maxValues-*count {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if !configTreeWithinBounds(iterator.Value(), depth+1, count, maxDepth, maxValues) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Len() > maxValues-*count {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if !configTreeWithinBounds(value.Index(index), depth+1, count, maxDepth, maxValues) {
				return false
			}
		}
	}
	return true
}

func deepCloneServer(server Server) Server {
	server.Provenance.ContributingLayers = append([]LayerProvenance(nil), server.Provenance.ContributingLayers...)
	server.UnsupportedFields = append([]string(nil), server.UnsupportedFields...)
	server.Command = cloneSensitivePointer(server.Command)
	server.Args = append([]SensitiveValue(nil), server.Args...)
	server.CWD = cloneSensitivePointer(server.CWD)
	server.Env = cloneSensitiveMap(server.Env)
	server.ForwardedEnv = append([]EnvVar(nil), server.ForwardedEnv...)
	server.URL = cloneSensitivePointer(server.URL)
	server.Headers = cloneSensitiveMap(server.Headers)
	server.HeaderEnv = cloneStringMap(server.HeaderEnv)
	server.OAuthResource = cloneSensitivePointer(server.OAuthResource)
	server.OAuthScopes = append([]string(nil), server.OAuthScopes...)
	server.HeadersHelper = cloneSensitivePointer(server.HeadersHelper)
	server.OmitToolsFrom = append([]ToolExposureSurface(nil), server.OmitToolsFrom...)
	server.Tools = cloneToolFilter(server.Tools)
	server.Approvals = cloneApprovals(server.Approvals)
	if server.CodexOAuth != nil {
		value := *server.CodexOAuth
		value.ClientID = cloneSensitivePointer(value.ClientID)
		server.CodexOAuth = &value
	}
	if server.ClaudeOAuth != nil {
		value := *server.ClaudeOAuth
		value.ClientID = cloneSensitivePointer(value.ClientID)
		value.AuthServerMetadataURL = cloneSensitivePointer(value.AuthServerMetadataURL)
		value.Scopes = cloneSensitivePointer(value.Scopes)
		server.ClaudeOAuth = &value
	}
	if server.definition != nil {
		server.definition = &definitionMaterial{canonical: append([]byte(nil), server.definition.canonical...)}
	}
	return server
}

func cloneSensitivePointer(value *SensitiveValue) *SensitiveValue {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneSensitiveMap(values map[string]SensitiveValue) map[string]SensitiveValue {
	if values == nil {
		return nil
	}
	result := make(map[string]SensitiveValue, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func appendDefinitionContext(server *Server, context string) {
	if server.definition == nil {
		return
	}
	canonical := append([]byte(nil), server.definition.canonical...)
	canonical = append(canonical, 0)
	canonical = append(canonical, []byte(context)...)
	server.definition = &definitionMaterial{canonical: canonical}
}
