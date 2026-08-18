// Package native resolves exact native installations and explicit marketplace
// inventory into read-only extension discovery candidates.
//
// Native enablement is provenance, not Switchboard permission. This package
// never starts components, grants trust, expands environment variables, walks
// plugin caches, or chooses between ambiguous installed records.
package native

import (
	"path/filepath"
	"sort"

	"github.com/switchboard-code/switchboard/internal/extensions"
)

// CodexCatalog identifies one exact conventional marketplace index. Path must
// end in .agents/plugins/marketplace.json. ProjectPath is required for a
// workspace catalog and confines it to the matching canonical workspace.
type CodexCatalog struct {
	Path        string
	Scope       extensions.Scope
	ProjectPath string
	// Optional suppresses a not-found diagnostic for conventional defaults.
	// Every present file is still parsed and validated identically.
	Optional bool
}

// CodexOptions identifies the effective user config and exact marketplace
// catalogs to inspect. Codex plugin selection is intentionally user-only;
// repository catalogs are available inventory, not native enablement.
type CodexOptions struct {
	UserConfigPath string
	Workspace      string
	Catalogs       []CodexCatalog
}

// ClaudeSettings identifies one native settings layer. ProjectPath is required
// for workspace and local layers and limits that layer to the matching
// canonical workspace.
type ClaudeSettings struct {
	Path        string
	Scope       extensions.Scope
	ProjectPath string
}

// ClaudeOptions identifies the installed-plugin index and settings layers to
// join. Settings precedence is native and fixed: managed, local, workspace,
// then user. Input order does not affect the result.
type ClaudeOptions struct {
	Workspace            string
	InstalledPluginsPath string
	Settings             []ClaudeSettings
}

// Options selects either or both native resolvers. Nil products are skipped.
type Options struct {
	Codex  *CodexOptions
	Claude *ClaudeOptions
	// ManagedPluginConstraints and ClaudeManagedPolicyUnavailable let an
	// embedding runtime supply the frozen result of its authoritative platform
	// policy loader (including managed drop-ins, remote, MDM, or registry
	// surfaces) without teaching discovery how to retrieve those sources.
	ManagedPluginConstraints       []ManagedPluginConstraint
	ClaudeManagedPolicyUnavailable bool
}

// Provenance records why a candidate exists. Native enablement, when present,
// does not authorize Switchboard execution.
type Provenance struct {
	Dialect          extensions.Dialect `json:"dialect"`
	NativeID         string             `json:"native_id"`
	EnablementPath   string             `json:"enablement_path,omitempty"`
	RegistryPath     string             `json:"registry_path,omitempty"`
	Marketplace      string             `json:"marketplace,omitempty"`
	MarketplacePath  string             `json:"marketplace_path,omitempty"`
	MarketplaceScope extensions.Scope   `json:"marketplace_scope,omitempty"`
	NativeScope      string             `json:"native_scope"`
	ProjectPath      string             `json:"project_path,omitempty"`
	EnablementScope  extensions.Scope   `json:"enablement_scope,omitempty"`
	// MarketplacePolicy preserves the exact validated Codex catalog policy.
	// It is descriptive provenance only and never grants Switchboard permission.
	MarketplacePolicy *CodexMarketplacePolicy `json:"marketplace_policy,omitempty"`
}

// CodexMarketplacePolicy is the validated policy envelope from one Codex
// marketplace entry. Products is nil when the entry applies to every product.
// Switchboard currently recognizes CODEX as its compatibility target.
type CodexMarketplacePolicy struct {
	Installation   string   `json:"installation"`
	Authentication string   `json:"authentication"`
	Products       []string `json:"products,omitempty"`
}

// CandidateState distinguishes an exact installed root from marketplace
// inventory. Available source trees are inspectable but are not eligible for
// activation until Switchboard records its own explicit installation/enable
// decision.
type CandidateState string

const (
	CandidateAvailable CandidateState = "available"
	CandidateInstalled CandidateState = "installed"
)

// ResolvedCandidate pairs a native record with the neutral discovery input.
// ActivationEligible means one exact native-installed root matched bounded
// discovery at the same dialect, scope, and physical root and is eligible as
// an InstallActivation source. It is not an activation capability: callers
// must copy and rediscover the bytes in Switchboard's cache before State can
// enable them.
type ResolvedCandidate struct {
	NativeID string         `json:"native_id"`
	State    CandidateState `json:"state"`
	// NativeEnabled preserves native selection independently of installation.
	// A catalog-only candidate can be native-enabled but never activation-eligible.
	NativeEnabled bool `json:"native_enabled"`
	// ActivationEligible requires an exact installed root and matching discovered
	// identity. Switchboard cache installation, trust, and enable state remain
	// separate gates.
	ActivationEligible bool `json:"activation_eligible"`
	// ManagedDenied is an authoritative native policy constraint. It never
	// changes Switchboard's activation ledger, but callers must keep the
	// candidate (and any byte-identical cached copy) from contributing runtime
	// behavior while the constraint applies.
	ManagedDenied bool                 `json:"managed_denied,omitempty"`
	Candidate     extensions.Candidate `json:"candidate"`
	Provenance    Provenance           `json:"provenance"`
}

// DefaultLocalOptions constructs the documented user and workspace paths
// without reading environment variables or the filesystem. Callers supply an
// already selected home and workspace. Platform-managed Claude settings must
// be appended explicitly because their location is deployment-specific.
func DefaultLocalOptions(userHome, workspace string) Options {
	options := Options{Codex: &CodexOptions{}, Claude: &ClaudeOptions{}}
	if userHome != "" {
		options.Codex.UserConfigPath = filepath.Join(userHome, ".codex", "config.toml")
		options.Codex.Catalogs = append(options.Codex.Catalogs, CodexCatalog{
			Path:     filepath.Join(userHome, ".agents", "plugins", "marketplace.json"),
			Scope:    extensions.ScopeUser,
			Optional: true,
		})
		options.Claude.InstalledPluginsPath = filepath.Join(userHome, ".claude", "plugins", "installed_plugins.json")
		options.Claude.Settings = append(options.Claude.Settings, ClaudeSettings{
			Path:  filepath.Join(userHome, ".claude", "settings.json"),
			Scope: extensions.ScopeUser,
		})
	}
	if workspace != "" {
		options.Codex.Workspace = workspace
		options.Codex.Catalogs = append(options.Codex.Catalogs, CodexCatalog{
			Path:        filepath.Join(workspace, ".agents", "plugins", "marketplace.json"),
			Scope:       extensions.ScopeWorkspace,
			ProjectPath: workspace,
			Optional:    true,
		})
		options.Claude.Workspace = workspace
		options.Claude.Settings = append(options.Claude.Settings,
			ClaudeSettings{
				Path:        filepath.Join(workspace, ".claude", "settings.json"),
				Scope:       extensions.ScopeWorkspace,
				ProjectPath: workspace,
			},
			ClaudeSettings{
				Path:        filepath.Join(workspace, ".claude", "settings.local.json"),
				Scope:       extensions.ScopeLocal,
				ProjectPath: workspace,
			},
		)
	}
	return options
}

// Result contains stable, deterministic candidate and diagnostic ordering.
type Result struct {
	Candidates               []ResolvedCandidate       `json:"candidates"`
	Diagnostics              []extensions.Diagnostic   `json:"diagnostics,omitempty"`
	ManagedPluginConstraints []ManagedPluginConstraint `json:"managed_plugin_constraints,omitempty"`
	// ClaudeManagedPolicyUnavailable means an applicable managed settings
	// source could not be interpreted. Consumers must fail closed for Claude
	// runtime behavior rather than treating the missing decision as an allow.
	ClaudeManagedPolicyUnavailable bool `json:"claude_managed_policy_unavailable,omitempty"`
}

// ManagedPluginConstraint is independent of installation and content digest.
// This lets callers constrain a previously cached copy even after the native
// source is updated, removed, or otherwise no longer discoverable.
type ManagedPluginConstraint struct {
	Dialect  extensions.Dialect `json:"dialect"`
	NativeID string             `json:"native_id"`
	Denied   bool               `json:"denied"`
	Path     string             `json:"path,omitempty"`
}

// Resolve reads only the exact files named by options. It returns exact
// installed records joined to explicit native state, plus non-activatable local
// catalog inventory, as distinct states. Native enabled/disabled state is
// provenance except when applicable managed policy is false or ambiguous at
// equal precedence. Resolve performs no network access and never enumerates a
// cache.
func Resolve(options Options) Result {
	result := Result{
		ManagedPluginConstraints:       append([]ManagedPluginConstraint(nil), options.ManagedPluginConstraints...),
		ClaudeManagedPolicyUnavailable: options.ClaudeManagedPolicyUnavailable,
	}
	if options.Codex != nil {
		resolveCodex(*options.Codex, &result)
	}
	if options.Claude != nil {
		resolveClaude(*options.Claude, &result)
	}
	sortResult(&result)
	if result.Candidates == nil {
		result.Candidates = []ResolvedCandidate{}
	}
	return result
}

func addDiagnostic(result *Result, severity extensions.Severity, code, path, message string) {
	result.Diagnostics = append(result.Diagnostics, extensions.Diagnostic{
		Severity: severity,
		Code:     code,
		Path:     path,
		Message:  message,
	})
}

func sortResult(result *Result) {
	sort.SliceStable(result.Candidates, func(i, j int) bool {
		left, right := result.Candidates[i], result.Candidates[j]
		if left.Candidate.Dialect != right.Candidate.Dialect {
			return left.Candidate.Dialect < right.Candidate.Dialect
		}
		if left.NativeID != right.NativeID {
			return left.NativeID < right.NativeID
		}
		if left.State != right.State {
			return left.State < right.State
		}
		if left.NativeEnabled != right.NativeEnabled {
			return !left.NativeEnabled
		}
		if left.ActivationEligible != right.ActivationEligible {
			return !left.ActivationEligible
		}
		if left.Candidate.Scope != right.Candidate.Scope {
			return left.Candidate.Scope < right.Candidate.Scope
		}
		if left.Candidate.Root != right.Candidate.Root {
			return left.Candidate.Root < right.Candidate.Root
		}
		if left.Provenance.EnablementPath != right.Provenance.EnablementPath {
			return left.Provenance.EnablementPath < right.Provenance.EnablementPath
		}
		return left.Provenance.RegistryPath < right.Provenance.RegistryPath
	})
	sort.SliceStable(result.Diagnostics, func(i, j int) bool {
		left, right := result.Diagnostics[i], result.Diagnostics[j]
		if left.Severity != right.Severity {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Message < right.Message
	})
	sort.SliceStable(result.ManagedPluginConstraints, func(i, j int) bool {
		left, right := result.ManagedPluginConstraints[i], result.ManagedPluginConstraints[j]
		if left.Dialect != right.Dialect {
			return left.Dialect < right.Dialect
		}
		if left.NativeID != right.NativeID {
			return left.NativeID < right.NativeID
		}
		return left.Path < right.Path
	})
}

func severityRank(severity extensions.Severity) int {
	if severity == extensions.SeverityError {
		return 0
	}
	return 1
}
