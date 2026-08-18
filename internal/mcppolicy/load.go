package mcppolicy

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

// Load snapshots every local native MCP policy surface and composes already
// retrieved managed documents in official precedence order. Per-dialect
// source failures are returned as diagnostics and an unavailable checker so a
// Claude failure does not suppress a valid Codex policy (or vice versa).
// Non-nil errors are reserved for invalid loader options/path inventory.
func Load(options Options) (*Checker, []Diagnostic, error) {
	goos := platform(options)
	if goos != "darwin" && goos != "linux" && goos != "windows" {
		return nil, nil, fmt.Errorf("native MCP managed policy is unsupported on %s", goos)
	}
	if _, err := absoluteCleanPlatform(goos, options.HomeDir); err != nil {
		return nil, nil, fmt.Errorf("native MCP policy home: %w", err)
	}
	if _, err := absoluteCleanPlatform(goos, options.Workspace); err != nil {
		return nil, nil, fmt.Errorf("native MCP policy workspace: %w", err)
	}
	paths, err := resolvedPolicyPaths(options)
	if err != nil {
		return nil, nil, err
	}
	loader := policyLoader{options: options, paths: paths, goos: goos}
	checker := &Checker{}
	checker.codex = loader.loadCodex()
	checker.claude = loader.loadClaude()
	sort.Slice(loader.diagnostics, func(i, j int) bool {
		left, right := loader.diagnostics[i], loader.diagnostics[j]
		if left.Dialect != right.Dialect {
			return left.Dialect < right.Dialect
		}
		if left.Severity != right.Severity {
			return left.Severity > right.Severity
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Field < right.Field
	})
	if loader.diagnostics == nil {
		loader.diagnostics = []Diagnostic{}
	}
	return checker, append([]Diagnostic(nil), loader.diagnostics...), nil
}

type policyLoader struct {
	options     Options
	paths       Paths
	goos        string
	budget      readBudget
	diagnostics []Diagnostic
}

func resolvedPolicyPaths(options Options) (Paths, error) {
	var paths Paths
	if options.Paths == nil {
		resolved, err := ResolvePaths(options)
		if err != nil {
			return Paths{}, err
		}
		paths = resolved
	} else {
		paths = *options.Paths
		paths.CodexMDM = append([]string(nil), options.Paths.CodexMDM...)
		paths.ClaudeMDM = append([]string(nil), options.Paths.ClaudeMDM...)
	}
	required := map[string]string{
		"CodexRequirements":     paths.CodexRequirements,
		"CodexAuth":             paths.CodexAuth,
		"ClaudeManagedSettings": paths.ClaudeManagedSettings,
		"ClaudeManagedDropIns":  paths.ClaudeManagedDropIns,
		"ClaudeManagedMCP":      paths.ClaudeManagedMCP,
		"ClaudeRemoteSettings":  paths.ClaudeRemoteSettings,
		"ClaudeState":           paths.ClaudeState,
		"ClaudeUserSettings":    paths.ClaudeUserSettings,
		"ClaudeProjectSettings": paths.ClaudeProjectSettings,
		"ClaudeLocalSettings":   paths.ClaudeLocalSettings,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return Paths{}, fmt.Errorf("native MCP managed policy path %s is empty", field)
		}
		if !absolutePlatformPath(platform(options), value) {
			return Paths{}, fmt.Errorf("native MCP managed policy path %s is not absolute", field)
		}
	}
	for _, path := range append(append([]string(nil), paths.CodexMDM...), paths.ClaudeMDM...) {
		if strings.TrimSpace(path) == "" || !absolutePlatformPath(platform(options), path) {
			return Paths{}, fmt.Errorf("native MCP managed preference path is not absolute")
		}
	}
	return paths, nil
}

func absolutePlatformPath(goos, value string) bool {
	if goos == "windows" {
		_, err := absoluteCleanPlatform(goos, value)
		return err == nil
	}
	return filepath.IsAbs(value)
}

func (loader *policyLoader) loadCodex() codexPolicy {
	merged := codexRequirements{}
	unavailable := false
	file, found, err := loader.budget.read(loader.paths.CodexRequirements, canonicalRoot(filepath.Dir(loader.paths.CodexRequirements)))
	if err != nil {
		unavailable = true
		loader.readDiagnostic(mcpnative.DialectCodex, loader.paths.CodexRequirements, err)
	} else if found {
		fragment, parseErr := parseCodexRequirements(file.data)
		if parseErr != nil {
			unavailable = true
			loader.invalidDiagnostic(mcpnative.DialectCodex, file.path, "mcp_servers", "invalid-codex-requirements")
		} else {
			mergeCodexRequirements(&merged, fragment)
		}
	}

	if !loader.options.CloudRequirementsChecked {
		// The absence of auth.json cannot prove that Codex is signed out:
		// credentials may live in the OS keyring, encrypted storage, or an
		// ephemeral host. Without an authoritative caller check, treating the
		// cloud requirements bundle as empty would be a policy bypass.
		unavailable = true
		loader.diagnostic(mcpnative.DialectCodex, SeverityError, "codex-cloud-policy-unavailable", "codex-cloud-config", "mcp_servers", "Codex cloud requirements were not authoritatively checked")
	}
	for _, document := range loader.options.CloudRequirements {
		data, documentErr := loader.budget.document(document)
		if documentErr != nil {
			unavailable = true
			loader.readDiagnostic(mcpnative.DialectCodex, "codex-cloud-config", documentErr)
			continue
		}
		fragment, parseErr := parseCodexRequirements(data)
		if parseErr != nil {
			unavailable = true
			loader.invalidDiagnostic(mcpnative.DialectCodex, "codex-cloud-config", "mcp_servers", "invalid-codex-requirements")
			continue
		}
		mergeCodexRequirements(&merged, fragment)
	}

	mdmDetected, mdmErr := anySurfacePresent(loader.paths.CodexMDM)
	if mdmErr != nil {
		unavailable = true
		loader.diagnostic(mcpnative.DialectCodex, SeverityError, "codex-mdm-policy-undetermined", "codex-managed-preferences", "mcp_servers", "Codex managed preferences could not be authoritatively inspected")
	}
	if mdmDetected && loader.options.CodexMDMRequirements == nil {
		unavailable = true
		loader.diagnostic(mcpnative.DialectCodex, SeverityError, "codex-mdm-policy-unavailable", "codex-managed-preferences", "mcp_servers", "Codex managed preferences are present but no decoded requirements document was supplied")
	}
	if loader.options.CodexMDMRequirements != nil {
		data, documentErr := loader.budget.document(*loader.options.CodexMDMRequirements)
		if documentErr != nil {
			unavailable = true
			loader.readDiagnostic(mcpnative.DialectCodex, "codex-managed-preferences", documentErr)
		} else if fragment, parseErr := parseCodexRequirements(data); parseErr != nil {
			unavailable = true
			loader.invalidDiagnostic(mcpnative.DialectCodex, "codex-managed-preferences", "mcp_servers", "invalid-codex-requirements")
		} else {
			mergeCodexRequirements(&merged, fragment)
		}
	}

	compiled, compileErr := compileCodexPolicy(merged)
	if compileErr != nil {
		unavailable = true
		loader.invalidDiagnostic(mcpnative.DialectCodex, "effective-codex-requirements", "mcp_servers", "invalid-codex-mcp-policy")
		compiled = codexPolicy{}
	}
	compiled.unavailable = unavailable
	return compiled
}

func (loader *policyLoader) codexChatGPTSignedIn() (bool, error) {
	file, found, err := loader.budget.read(loader.paths.CodexAuth, canonicalRoot(filepath.Dir(loader.paths.CodexAuth)))
	if err != nil || !found {
		return false, err
	}
	root, err := decodeUniqueJSONObject(file.data)
	if err != nil {
		return false, errors.New("Codex authentication state cannot be parsed")
	}
	tokens, exists := root["tokens"]
	if !exists {
		return false, nil
	}
	return !bytes.Equal(bytes.TrimSpace(tokens), []byte("null")), nil
}

func (loader *policyLoader) loadClaude() claudePolicy {
	unavailable := false
	managedFile, managedFileOK := loader.loadClaudeManagedFiles()
	if !managedFileOK {
		unavailable = true
	}

	var remote claudeSettings
	if loader.options.ClaudeRemoteSettings != nil {
		parsed, ok := loader.parseClaudeDocument(*loader.options.ClaudeRemoteSettings, "claude-server-managed-settings", true)
		if !ok {
			unavailable = true
		} else {
			// policyHelper is ignored in server-managed settings.
			parsed.helperPresent = false
			remote = parsed
		}
	} else if present, surfaceErr := surfacePresent(loader.paths.ClaudeRemoteSettings); surfaceErr != nil || present {
		unavailable = true
		code := "claude-remote-policy-unavailable"
		if surfaceErr != nil {
			code = "claude-remote-policy-undetermined"
		}
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, code, loader.paths.ClaudeRemoteSettings, "allowedMcpServers", "Claude server-managed settings are present or indeterminate but no authoritative document was supplied")
	}

	var mdm claudeSettings
	mdmDetected, mdmErr := anySurfacePresent(loader.paths.ClaudeMDM)
	registry, registryErr := detectClaudeRegistrySurfaces()
	if mdmErr != nil || registryErr != nil {
		unavailable = true
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, "claude-os-policy-undetermined", "claude-os-managed-settings", "allowedMcpServers", "Claude OS-managed settings could not be authoritatively inspected")
	}
	if loader.options.ClaudeMDMSettings != nil {
		parsed, ok := loader.parseClaudeDocument(*loader.options.ClaudeMDMSettings, "claude-os-managed-settings", true)
		if !ok {
			unavailable = true
		} else {
			mdm = parsed
		}
	} else if mdmDetected || registry.system {
		unavailable = true
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, "claude-os-policy-unavailable", "claude-os-managed-settings", "allowedMcpServers", "Claude OS-managed settings are present but no decoded document was supplied")
	}

	managed := claudeSettings{}
	switch {
	case remote.sourceNonempty:
		managed = remote
	case mdm.sourceNonempty:
		managed = mdm
	case managedFile.sourceNonempty:
		managed = managedFile
	case registry.user:
		unavailable = true
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, "claude-user-registry-policy-unavailable", "claude-user-managed-settings", "allowedMcpServers", "Claude HKCU managed settings are active but no authoritative document was supplied")
	}
	// Current Claude Code merges env per variable across admin-controlled
	// managed sources even though all other settings come from the single
	// highest-precedence non-empty source. Apply low-to-high precedence so an
	// MDM or remote value overrides the file fallback for the same variable.
	managed.environment = mergeClaudeManagedEnvironments(managedFile.environment, mdm.environment, remote.environment)
	// A system/MDM policy helper can replace all other managed settings at
	// startup. Never execute it here; quarantine until the caller supplies its
	// result through a future authoritative integration.
	if managedFile.helperPresent || mdm.helperPresent {
		unavailable = true
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, "claude-policy-helper-unavailable", "claude-managed-settings", "policyHelper", "Claude policyHelper is configured and cannot be evaluated by the offline policy loader")
	}

	user, userOK := loader.loadClaudeSettingsFile(loader.paths.ClaudeUserSettings, canonicalRoot(filepath.Dir(loader.paths.ClaudeUserSettings)), false)
	projectRoot := canonicalRoot(loader.options.Workspace)
	project, projectOK := loader.loadClaudeSettingsFile(loader.paths.ClaudeProjectSettings, projectRoot, false)
	local, localOK := loader.loadClaudeSettingsFile(loader.paths.ClaudeLocalSettings, projectRoot, false)
	if !userOK || !projectOK || !localOK {
		unavailable = true
	}

	managedMCP, managedMCPErr := surfacePresent(loader.paths.ClaudeManagedMCP)
	state := claudeProjectState{}
	stateOK := true
	if !managedMCP && managedMCPErr == nil {
		state, stateOK = loader.loadClaudeProjectState()
		if !stateOK {
			unavailable = true
		}
	}
	compiled := compileClaudePolicy(loader.goos, loader.options.StartupEnv, managed, user, project, local, state)
	compiled.managedExclusive = managedMCP
	if managedMCPErr != nil {
		unavailable = true
		compiled.managedExclusive = true
		loader.diagnostic(mcpnative.DialectClaude, SeverityError, "claude-managed-mcp-undetermined", loader.paths.ClaudeManagedMCP, "mcpServers", "Claude managed-mcp.json presence could not be authoritatively inspected")
	}
	compiled.unavailable = unavailable
	return compiled
}

func mergeClaudeManagedEnvironments(sources ...map[string]string) map[string]string {
	var result map[string]string
	for _, source := range sources {
		if len(source) == 0 {
			continue
		}
		if result == nil {
			result = make(map[string]string)
		}
		for name, value := range source {
			result[name] = value
		}
	}
	return result
}

func (loader *policyLoader) loadClaudeProjectState() (claudeProjectState, bool) {
	file, found, err := loader.budget.read(loader.paths.ClaudeState, canonicalRoot(filepath.Dir(loader.paths.ClaudeState)))
	if err != nil {
		loader.readDiagnostic(mcpnative.DialectClaude, loader.paths.ClaudeState, err)
		return claudeProjectState{}, false
	}
	if !found {
		return claudeProjectState{}, true
	}
	state, parseErr := parseClaudeProjectState(file.data, loader.options.Workspace)
	if parseErr != nil {
		loader.invalidDiagnostic(mcpnative.DialectClaude, file.path, "projects.<workspace>", "invalid-claude-project-state")
		return claudeProjectState{}, false
	}
	return state, true
}

func (loader *policyLoader) loadClaudeManagedFiles() (claudeSettings, bool) {
	result := claudeSettings{}
	ok := true
	root := canonicalRoot(filepath.Dir(loader.paths.ClaudeManagedSettings))
	if file, found, err := loader.budget.read(loader.paths.ClaudeManagedSettings, root); err != nil {
		loader.readDiagnostic(mcpnative.DialectClaude, loader.paths.ClaudeManagedSettings, err)
		ok = false
	} else if found {
		settings, parseErr := parseClaudeSettings(file.data, true)
		if parseErr != nil {
			loader.invalidDiagnostic(mcpnative.DialectClaude, file.path, "", "invalid-claude-managed-settings")
			ok = false
		} else {
			mergeManagedClaudeSettings(&result, settings)
			loader.strippedClaudeAllowed(file.path, settings.strippedAllowed)
		}
	}
	files, _, err := loader.budget.readDropIns(loader.paths.ClaudeManagedDropIns, root)
	if err != nil {
		loader.readDiagnostic(mcpnative.DialectClaude, loader.paths.ClaudeManagedDropIns, err)
		ok = false
		return result, ok
	}
	for _, file := range files {
		settings, parseErr := parseClaudeSettings(file.data, true)
		if parseErr != nil {
			loader.invalidDiagnostic(mcpnative.DialectClaude, file.path, "", "invalid-claude-managed-settings")
			ok = false
			continue
		}
		mergeManagedClaudeSettings(&result, settings)
		loader.strippedClaudeAllowed(file.path, settings.strippedAllowed)
	}
	return result, ok
}

func (loader *policyLoader) loadClaudeSettingsFile(path, root string, managed bool) (claudeSettings, bool) {
	file, found, err := loader.budget.read(path, root)
	if err != nil {
		loader.readDiagnostic(mcpnative.DialectClaude, path, err)
		return claudeSettings{}, false
	}
	if !found {
		return claudeSettings{}, true
	}
	settings, parseErr := parseClaudeSettings(file.data, managed)
	if parseErr != nil {
		loader.invalidDiagnostic(mcpnative.DialectClaude, file.path, "", "invalid-claude-settings")
		return claudeSettings{}, false
	}
	return settings, true
}

func (loader *policyLoader) parseClaudeDocument(document Document, surface string, managed bool) (claudeSettings, bool) {
	data, err := loader.budget.document(document)
	if err != nil {
		loader.readDiagnostic(mcpnative.DialectClaude, surface, err)
		return claudeSettings{}, false
	}
	settings, parseErr := parseClaudeSettings(data, managed)
	if parseErr != nil {
		loader.invalidDiagnostic(mcpnative.DialectClaude, surface, "", "invalid-claude-managed-settings")
		return claudeSettings{}, false
	}
	loader.strippedClaudeAllowed(surface, settings.strippedAllowed)
	return settings, true
}

func (loader *policyLoader) strippedClaudeAllowed(path string, count int) {
	if count == 0 {
		return
	}
	loader.diagnostic(mcpnative.DialectClaude, SeverityWarning, "invalid-claude-allow-entry-stripped", path, "allowedMcpServers", "Claude stripped invalid managed allowlist entries; the remaining valid subset is enforced")
}

func (loader *policyLoader) readDiagnostic(dialect mcpnative.Dialect, path string, err error) {
	code := "unreadable-policy"
	message := "native MCP policy could not be read safely"
	var readErr *safeReadError
	if errors.As(err, &readErr) {
		code = readErr.code
		message = readErr.message
	}
	loader.diagnostic(dialect, SeverityError, code, filepath.Clean(path), "", message)
}

func (loader *policyLoader) invalidDiagnostic(dialect mcpnative.Dialect, path, field, code string) {
	loader.diagnostic(dialect, SeverityError, code, path, field, "native MCP policy is malformed or has an unsupported security-relevant shape")
}

func (loader *policyLoader) diagnostic(dialect mcpnative.Dialect, severity Severity, code, path, field, message string) {
	loader.diagnostics = append(loader.diagnostics, Diagnostic{
		Severity: severity, Dialect: dialect, Code: code, Path: path, Field: field, Message: message,
	})
}

func anySurfacePresent(paths []string) (bool, error) {
	present := false
	for _, path := range paths {
		found, err := surfacePresent(path)
		present = present || found
		if err != nil {
			return present, err
		}
	}
	return present, nil
}

func canonicalRoot(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if real, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}
