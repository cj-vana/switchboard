package native

import (
	"fmt"
	"os"
	"sort"

	"github.com/switchboard-code/switchboard/internal/extensions"
)

const (
	claudeInstalledPluginsVersion = 2
	maxClaudeInstalledRecords     = 64
	maxClaudeSettingsLayers       = 32
)

type claudeSettingsFile struct {
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
}

type claudeInstalledPlugins struct {
	Version int                                `json:"version"`
	Plugins map[string][]claudeInstalledRecord `json:"plugins"`
}

type claudeInstalledRecord struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	ProjectPath string `json:"projectPath,omitempty"`
}

type claudeEnableState struct {
	enabled      bool
	ambiguous    bool
	conflictPath string
	rank         int
	scope        extensions.Scope
	path         string
}

type applicableClaudeSetting struct {
	path        string
	scope       extensions.Scope
	projectRoot string
	relative    string
	rank        int
}

type normalizedClaudeRecord struct {
	nativeScope string
	scope       extensions.Scope
	root        string
	realRoot    string
	projectPath string
}

func resolveClaude(options ClaudeOptions, result *Result) {
	workspace, workspaceErr := optionalCanonicalWorkspace(options.Workspace)
	settings, fatal := applicableClaudeSettings(options.Settings, workspace, workspaceErr, result)

	states := make(map[string]claudeEnableState)
	for _, setting := range settings {
		var document claudeSettingsFile
		var err error
		if setting.projectRoot == "" {
			err = readJSONFile(setting.path, &document)
		} else {
			err = readJSONWithin(setting.projectRoot, setting.relative, &document)
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if setting.scope == extensions.ScopeManaged {
				result.ClaudeManagedPolicyUnavailable = true
			}
			addDiagnostic(result, extensions.SeverityError, "claude-settings", setting.path,
				"cannot read an applicable Claude settings layer; enablement is unresolved: "+err.Error())
			fatal = true
			continue
		}
		applyClaudeSettings(states, setting, document.EnabledPlugins, result)
	}
	appendClaudeManagedConstraints(states, result)
	if fatal {
		return
	}

	configuredIDs := make([]string, 0, len(states))
	for id := range states {
		configuredIDs = append(configuredIDs, id)
	}
	sort.Strings(configuredIDs)
	invalidIDs := make(map[string]bool)
	registryRequired := false
	for _, id := range configuredIDs {
		state := states[id]
		if state.ambiguous {
			addDiagnostic(result, extensions.SeverityError, "ambiguous-enable", state.conflictPath,
				fmt.Sprintf("Claude settings at equal precedence disagree about %q; no enablement is inferred", id))
		}
		if _, _, err := splitNativeID(id); err != nil {
			addDiagnostic(result, extensions.SeverityError, "invalid-native-id", state.path,
				fmt.Sprintf("configured Claude plugin ID %q: %v", id, err))
			invalidIDs[id] = true
			continue
		}
		registryRequired = registryRequired || state.enabled && !state.ambiguous
	}
	if options.InstalledPluginsPath == "" {
		if registryRequired {
			addDiagnostic(result, extensions.SeverityError, "claude-installed-registry", "",
				"Claude plugins are enabled, but no exact installed_plugins.json path was provided; caches are not searched")
		}
		return
	}
	registryPath, err := exactAbsolutePath(options.InstalledPluginsPath)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "claude-installed-registry", options.InstalledPluginsPath,
			"Claude installed-plugin registry path is not exact: "+err.Error())
		return
	}
	options.InstalledPluginsPath = registryPath

	var installed claudeInstalledPlugins
	if err := readJSONFile(options.InstalledPluginsPath, &installed); err != nil {
		if os.IsNotExist(err) && !registryRequired {
			return
		}
		addDiagnostic(result, extensions.SeverityError, "claude-installed-registry", options.InstalledPluginsPath,
			"cannot read the exact Claude installed-plugin registry: "+err.Error())
		return
	}
	if installed.Version != claudeInstalledPluginsVersion {
		addDiagnostic(result, extensions.SeverityError, "unsupported-installed-registry", options.InstalledPluginsPath,
			fmt.Sprintf("Claude installed-plugin registry version is %d; only version %d is parsed", installed.Version, claudeInstalledPluginsVersion))
		return
	}
	installedRecordCount := 0
	for _, records := range installed.Plugins {
		if len(records) > maxClaudeInstalledRecords-installedRecordCount {
			addDiagnostic(result, extensions.SeverityError, "installed-record-limit", options.InstalledPluginsPath,
				fmt.Sprintf("Claude installed-plugin registry has more than %d records; no installed roots are inspected", maxClaudeInstalledRecords))
			return
		}
		installedRecordCount += len(records)
	}

	allIDs := make(map[string]bool, len(installed.Plugins)+len(configuredIDs))
	for id := range installed.Plugins {
		allIDs[id] = true
	}
	for _, id := range configuredIDs {
		allIDs[id] = true
	}
	ids := make([]string, 0, len(allIDs))
	for id := range allIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		state, configured := states[id]
		if invalidIDs[id] {
			continue
		}
		if _, _, err := splitNativeID(id); err != nil {
			addDiagnostic(result, extensions.SeverityError, "invalid-native-id", options.InstalledPluginsPath,
				fmt.Sprintf("installed Claude plugin ID %q: %v", id, err))
			continue
		}
		nativeEnabled := configured && !state.ambiguous && state.enabled
		records, ok := installed.Plugins[id]
		if !ok || len(records) == 0 {
			if nativeEnabled {
				addDiagnostic(result, extensions.SeverityError, "enabled-plugin-not-installed", options.InstalledPluginsPath,
					fmt.Sprintf("enabled Claude plugin %q has no exact installed record; caches are not searched", id))
			}
			continue
		}
		resolved := normalizeClaudeRecords(id, records, workspace, workspaceErr, options.InstalledPluginsPath, result)
		if len(resolved) == 0 {
			if nativeEnabled {
				addDiagnostic(result, extensions.SeverityError, "enabled-plugin-not-applicable", options.InstalledPluginsPath,
					fmt.Sprintf("enabled Claude plugin %q has no valid installed record applicable to this workspace", id))
			}
			continue
		}
		if len(resolved) > 1 {
			addDiagnostic(result, extensions.SeverityWarning, "ambiguous-installed-record", options.InstalledPluginsPath,
				fmt.Sprintf("Claude plugin %q has %d distinct applicable records; all are returned and none is preferred", id, len(resolved)))
		}
		managedDenied := configured && state.scope == extensions.ScopeManaged && (state.ambiguous || !state.enabled)
		if managedDenied && !state.ambiguous {
			addDiagnostic(result, extensions.SeverityError, "managed-plugin-disabled", state.path,
				fmt.Sprintf("Claude managed settings disable plugin %q; installed inventory is visible but Switchboard activation is denied", id))
		}
		for _, record := range resolved {
			enablementPath := ""
			enablementScope := extensions.Scope("")
			if configured && !state.ambiguous {
				enablementPath = state.path
				enablementScope = state.scope
			}
			candidate := extensions.Candidate{
				Root:    record.root,
				Scope:   record.scope,
				Dialect: extensions.DialectClaude,
			}
			_, identityMatched := discoverNativeIdentity(id, candidate, result)
			activationEligible := len(resolved) == 1 && identityMatched && !managedDenied
			result.Candidates = append(result.Candidates, ResolvedCandidate{
				NativeID:           id,
				State:              CandidateInstalled,
				NativeEnabled:      nativeEnabled,
				ActivationEligible: activationEligible,
				ManagedDenied:      managedDenied,
				Candidate:          candidate,
				Provenance: Provenance{
					Dialect:         extensions.DialectClaude,
					NativeID:        id,
					EnablementPath:  enablementPath,
					RegistryPath:    options.InstalledPluginsPath,
					NativeScope:     record.nativeScope,
					ProjectPath:     record.projectPath,
					EnablementScope: enablementScope,
				},
			})
		}
	}
}

func appendClaudeManagedConstraints(states map[string]claudeEnableState, result *Result) {
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		state := states[id]
		if state.scope != extensions.ScopeManaged || state.enabled && !state.ambiguous {
			continue
		}
		if _, _, err := splitNativeID(id); err != nil {
			continue
		}
		result.ManagedPluginConstraints = append(result.ManagedPluginConstraints, ManagedPluginConstraint{
			Dialect: extensions.DialectClaude, NativeID: id, Denied: true, Path: state.path,
		})
	}
}

func optionalCanonicalWorkspace(workspace string) (string, error) {
	if workspace == "" {
		return "", nil
	}
	return canonicalExactDirectory(workspace)
}

func applicableClaudeSettings(specs []ClaudeSettings, workspace string, workspaceErr error, result *Result) ([]applicableClaudeSetting, bool) {
	if len(specs) > maxClaudeSettingsLayers {
		addDiagnostic(result, extensions.SeverityError, "settings-layer-limit", "",
			fmt.Sprintf("Claude options name %d settings layers; limit is %d and none are inspected", len(specs), maxClaudeSettingsLayers))
		return nil, true
	}
	ordered := append([]ClaudeSettings(nil), specs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		leftRank, _ := claudeSettingsRank(ordered[i].Scope)
		rightRank, _ := claudeSettingsRank(ordered[j].Scope)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if ordered[i].Path != ordered[j].Path {
			return ordered[i].Path < ordered[j].Path
		}
		return ordered[i].ProjectPath < ordered[j].ProjectPath
	})

	seen := make(map[string]bool)
	settings := make([]applicableClaudeSetting, 0, len(ordered))
	fatal := false
	for _, spec := range ordered {
		rank, ok := claudeSettingsRank(spec.Scope)
		if !ok {
			if spec.Scope == extensions.ScopeManaged {
				result.ClaudeManagedPolicyUnavailable = true
			}
			addDiagnostic(result, extensions.SeverityError, "unsupported-settings-scope", spec.Path,
				fmt.Sprintf("Claude settings scope %q is unsupported", spec.Scope))
			fatal = true
			continue
		}
		if spec.Path == "" {
			if spec.Scope == extensions.ScopeManaged {
				result.ClaudeManagedPolicyUnavailable = true
			}
			addDiagnostic(result, extensions.SeverityError, "claude-settings", "", "Claude settings path is empty")
			fatal = true
			continue
		}
		exactPath, err := exactAbsolutePath(spec.Path)
		if err != nil {
			if spec.Scope == extensions.ScopeManaged {
				result.ClaudeManagedPolicyUnavailable = true
			}
			addDiagnostic(result, extensions.SeverityError, "claude-settings", spec.Path,
				"Claude settings path is not exact: "+err.Error())
			fatal = true
			continue
		}

		setting := applicableClaudeSetting{path: exactPath, scope: spec.Scope, rank: rank}
		if spec.Scope == extensions.ScopeWorkspace || spec.Scope == extensions.ScopeLocal {
			if workspaceErr != nil {
				addDiagnostic(result, extensions.SeverityError, "invalid-workspace", spec.Path,
					"cannot canonicalize the selected Claude workspace: "+workspaceErr.Error())
				fatal = true
				continue
			}
			if workspace == "" {
				addDiagnostic(result, extensions.SeverityError, "missing-workspace", spec.Path,
					"workspace and local Claude settings require an exact selected workspace")
				fatal = true
				continue
			}
			projectRoot, err := canonicalExactDirectory(spec.ProjectPath)
			if err != nil {
				addDiagnostic(result, extensions.SeverityWarning, "ignored-settings-project", spec.Path,
					"Claude settings layer has no resolvable project path and is not applicable: "+err.Error())
				continue
			}
			if projectRoot != workspace {
				continue
			}
			relative, err := containedRelative(projectRoot, spec.Path)
			if err != nil {
				addDiagnostic(result, extensions.SeverityError, "unsafe-settings-path", spec.Path,
					"applicable Claude project settings are not safely contained: "+err.Error())
				fatal = true
				continue
			}
			setting.projectRoot = projectRoot
			setting.relative = relative
		}

		key := fmt.Sprintf("%d\x00%s\x00%s\x00%s", setting.rank, setting.path, setting.projectRoot, setting.relative)
		if seen[key] {
			continue
		}
		seen[key] = true
		settings = append(settings, setting)
	}
	return settings, fatal
}

func claudeSettingsRank(scope extensions.Scope) (int, bool) {
	switch scope {
	case extensions.ScopeUser:
		return 1, true
	case extensions.ScopeWorkspace:
		return 2, true
	case extensions.ScopeLocal:
		return 3, true
	case extensions.ScopeManaged:
		return 4, true
	default:
		return 0, false
	}
}

func applyClaudeSettings(states map[string]claudeEnableState, setting applicableClaudeSetting, enabled map[string]bool, result *Result) {
	ids := make([]string, 0, len(enabled))
	for id := range enabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		value := enabled[id]
		state, exists := states[id]
		if !exists || setting.rank > state.rank {
			states[id] = claudeEnableState{
				enabled: value,
				rank:    setting.rank,
				scope:   setting.scope,
				path:    setting.path,
			}
			continue
		}
		if setting.rank < state.rank {
			continue
		}
		if state.ambiguous || state.enabled != value {
			if state.conflictPath == "" {
				state.conflictPath = setting.path
			}
			state.ambiguous = true
			states[id] = state
			continue
		}
		addDiagnostic(result, extensions.SeverityWarning, "duplicate-enable", setting.path,
			fmt.Sprintf("Claude settings at equal precedence repeat %q=%t; provenance uses %s", id, value, state.path))
	}
}

func normalizeClaudeRecords(id string, records []claudeInstalledRecord, workspace string, workspaceErr error, registryPath string, result *Result) []normalizedClaudeRecord {
	normalized := make([]normalizedClaudeRecord, 0, len(records))
	for _, record := range records {
		scope, projectScoped, ok := claudeInstalledScope(record.Scope)
		if !ok {
			addDiagnostic(result, extensions.SeverityWarning, "unsupported-installed-scope", registryPath,
				fmt.Sprintf("Claude plugin %q has unsupported installed scope %q; the record is ignored", id, record.Scope))
			continue
		}
		projectPath := ""
		if projectScoped {
			if workspaceErr != nil || workspace == "" {
				continue
			}
			canonicalProject, err := canonicalExactDirectory(record.ProjectPath)
			if err != nil || canonicalProject != workspace {
				continue
			}
			projectPath = canonicalProject
		}
		root, err := exactAbsoluteDirectory(record.InstallPath)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "unsafe-installed-path", registryPath,
				fmt.Sprintf("Claude plugin %q installed path %q is rejected: %v", id, record.InstallPath, err))
			continue
		}
		realRoot, err := canonicalDirectory(root)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "unsafe-installed-path", registryPath,
				fmt.Sprintf("Claude plugin %q installed path %q cannot be canonicalized: %v", id, record.InstallPath, err))
			continue
		}
		normalized = append(normalized, normalizedClaudeRecord{
			nativeScope: record.Scope,
			scope:       scope,
			root:        root,
			realRoot:    realRoot,
			projectPath: projectPath,
		})
	}

	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].scope != normalized[j].scope {
			return normalized[i].scope < normalized[j].scope
		}
		if normalized[i].realRoot != normalized[j].realRoot {
			return normalized[i].realRoot < normalized[j].realRoot
		}
		if normalized[i].root != normalized[j].root {
			return normalized[i].root < normalized[j].root
		}
		return normalized[i].projectPath < normalized[j].projectPath
	})

	deduplicated := normalized[:0]
	seen := make(map[string]bool)
	for _, record := range normalized {
		key := string(record.scope) + "\x00" + record.realRoot
		if seen[key] {
			addDiagnostic(result, extensions.SeverityWarning, "duplicate-installed-record", registryPath,
				fmt.Sprintf("Claude plugin %q repeats installed scope %q at physical root %q; one candidate is returned", id, record.nativeScope, record.realRoot))
			continue
		}
		seen[key] = true
		deduplicated = append(deduplicated, record)
	}
	return deduplicated
}

func claudeInstalledScope(value string) (scope extensions.Scope, projectScoped bool, ok bool) {
	switch value {
	case "user":
		return extensions.ScopeUser, false, true
	case "project":
		return extensions.ScopeWorkspace, true, true
	case "local":
		return extensions.ScopeLocal, true, true
	case "managed":
		return extensions.ScopeManaged, false, true
	default:
		return "", false, false
	}
}
