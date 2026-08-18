package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/switchboard-code/switchboard/internal/extensions"
)

type codexConfig struct {
	Plugins      map[string]codexPluginSetting `toml:"plugins"`
	Marketplaces map[string]codexMarketplace   `toml:"marketplaces"`
}

type codexPluginSetting struct {
	Enabled    *bool                     `toml:"enabled"`
	MCPServers map[string]toml.Primitive `toml:"mcp_servers"`
}

func (setting codexPluginSetting) isEnabled() bool {
	return setting.Enabled == nil || *setting.Enabled
}

type codexMarketplace struct {
	SourceType   string   `toml:"source_type"`
	Source       string   `toml:"source"`
	Ref          string   `toml:"ref"`
	SparsePaths  []string `toml:"sparse_paths"`
	LastUpdated  string   `toml:"last_updated"`
	LastRevision string   `toml:"last_revision"`
}

type codexMarketplaceIndex struct {
	Name    string                   `json:"name"`
	Plugins []codexMarketplacePlugin `json:"plugins"`
}

type codexMarketplacePlugin struct {
	Name     string          `json:"name"`
	Source   json.RawMessage `json:"source"`
	Policy   json.RawMessage `json:"policy"`
	Category string          `json:"category,omitempty"`
}

type validatedCodexMarketplacePlugin struct {
	plugin   codexMarketplacePlugin
	policy   CodexMarketplacePolicy
	eligible bool
}

const (
	maxCodexCatalogs       = 64
	maxCodexCatalogEntries = 256
)

type codexLocalSource struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

const (
	codexInstallNotAvailable       = "NOT_AVAILABLE"
	codexInstallAvailable          = "AVAILABLE"
	codexInstallInstalledByDefault = "INSTALLED_BY_DEFAULT"
	codexAuthOnInstall             = "ON_INSTALL"
	codexAuthOnUse                 = "ON_USE"
	codexProductCodex              = "CODEX"
	codexProductChatGPT            = "CHATGPT"
	codexProductAtlas              = "ATLAS"
)

type codexCatalogOrigin struct {
	root            string
	indexPath       string
	expectedName    string
	scope           extensions.Scope
	projectPath     string
	declarationPath string
	optional        bool
}

func resolveCodex(options CodexOptions, result *Result) {
	config, configPath, configOK := readCodexUserConfig(options.UserConfigPath, result)
	origins := explicitCodexCatalogs(options, result)
	if configOK {
		origins = append(origins, configuredCodexCatalogs(configPath, config.Marketplaces, result)...)
	}
	candidates := discoverCodexCatalogs(origins, result)
	if configOK {
		applyCodexEnablementProvenance(candidates, configPath, config.Plugins)
	}
	result.Candidates = append(result.Candidates, candidates...)
	if configOK {
		reportUnresolvedCodexEnablement(configPath, config.Plugins, result)
	}
}

func applyCodexEnablementProvenance(candidates []ResolvedCandidate, configPath string, plugins map[string]codexPluginSetting) {
	for index := range candidates {
		setting, ok := plugins[candidates[index].NativeID]
		if !ok || !setting.isEnabled() {
			continue
		}
		candidates[index].NativeEnabled = true
		candidates[index].Provenance.EnablementPath = configPath
		candidates[index].Provenance.EnablementScope = extensions.ScopeUser
	}
}

func readCodexUserConfig(configPath string, result *Result) (codexConfig, string, bool) {
	if configPath == "" {
		return codexConfig{}, "", false
	}
	exactPath, err := exactAbsolutePath(configPath)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "codex-config", configPath,
			"Codex user config path is not exact: "+err.Error())
		return codexConfig{}, "", false
	}
	raw, err := readBoundedFile(exactPath, maxConfigBytes)
	if err != nil {
		if !os.IsNotExist(err) {
			addDiagnostic(result, extensions.SeverityError, "codex-config", exactPath,
				"cannot read Codex user plugin config: "+err.Error())
		}
		return codexConfig{}, exactPath, false
	}
	config, err := decodeCodexConfig(raw)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "codex-config", exactPath,
			"invalid Codex user plugin config; native enablement is unresolved: "+err.Error())
		return codexConfig{}, exactPath, false
	}
	return config, exactPath, true
}

func decodeCodexConfig(raw []byte) (codexConfig, error) {
	var config codexConfig
	metadata, err := toml.Decode(string(raw), &config)
	if err != nil {
		return codexConfig{}, err
	}
	for _, key := range metadata.Undecoded() {
		parts := []string(key)
		if len(parts) == 0 || parts[0] != "plugins" && parts[0] != "marketplaces" {
			continue
		}
		if parts[0] == "plugins" && len(parts) >= 3 && parts[2] == "mcp_servers" {
			continue
		}
		return codexConfig{}, fmt.Errorf("unsupported native plugin field %q", key.String())
	}
	return config, nil
}

func explicitCodexCatalogs(options CodexOptions, result *Result) []codexCatalogOrigin {
	specs := append([]CodexCatalog(nil), options.Catalogs...)
	if len(specs) > maxCodexCatalogs {
		addDiagnostic(result, extensions.SeverityError, "marketplace-catalog-limit", "",
			fmt.Sprintf("Codex options name %d marketplace catalogs; limit is %d and none are inspected", len(specs), maxCodexCatalogs))
		return nil
	}
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].Scope != specs[j].Scope {
			return specs[i].Scope < specs[j].Scope
		}
		if specs[i].Path != specs[j].Path {
			return specs[i].Path < specs[j].Path
		}
		return specs[i].ProjectPath < specs[j].ProjectPath
	})
	workspace, workspaceErr := optionalCanonicalWorkspace(options.Workspace)
	origins := make([]codexCatalogOrigin, 0, len(specs))
	seen := make(map[string]bool)
	for _, spec := range specs {
		if spec.Scope != extensions.ScopeUser && spec.Scope != extensions.ScopeWorkspace {
			addDiagnostic(result, extensions.SeverityError, "unsupported-catalog-scope", spec.Path,
				fmt.Sprintf("Codex catalog scope %q is unsupported", spec.Scope))
			continue
		}
		indexPath, root, err := conventionalCodexCatalogPath(spec.Path)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "codex-catalog", spec.Path,
				"Codex catalog path is not exact: "+err.Error())
			continue
		}
		projectPath := ""
		if spec.Scope == extensions.ScopeWorkspace {
			if workspaceErr != nil || workspace == "" {
				message := "workspace catalog requires an exact selected workspace"
				if workspaceErr != nil {
					message = "cannot canonicalize the selected Codex workspace: " + workspaceErr.Error()
				}
				addDiagnostic(result, extensions.SeverityError, "invalid-workspace", spec.Path, message)
				continue
			}
			projectRoot, err := canonicalExactDirectory(spec.ProjectPath)
			if err != nil || projectRoot != workspace {
				if err != nil {
					addDiagnostic(result, extensions.SeverityWarning, "ignored-catalog-project", spec.Path,
						"Codex catalog has no resolvable matching project and is not applicable: "+err.Error())
				}
				continue
			}
			catalogRoot, err := canonicalExactDirectory(root)
			if err != nil {
				if !spec.Optional || !os.IsNotExist(err) {
					addDiagnostic(result, extensions.SeverityError, "unsafe-catalog-path", spec.Path,
						"workspace Codex catalog root cannot be resolved: "+err.Error())
				}
				continue
			}
			if !pathWithin(workspace, catalogRoot) {
				addDiagnostic(result, extensions.SeverityError, "unsafe-catalog-path", spec.Path,
					"workspace Codex catalog root escapes the selected project")
				continue
			}
			projectPath = workspace
		}
		key := string(spec.Scope) + "\x00" + indexPath
		if seen[key] {
			continue
		}
		seen[key] = true
		origins = append(origins, codexCatalogOrigin{
			root:            root,
			indexPath:       indexPath,
			scope:           spec.Scope,
			projectPath:     projectPath,
			declarationPath: indexPath,
			optional:        spec.Optional,
		})
	}
	return origins
}

func conventionalCodexCatalogPath(indexPath string) (exactPath, root string, err error) {
	exactPath, err = exactAbsolutePath(indexPath)
	if err != nil {
		return "", "", err
	}
	pluginsDir := filepath.Dir(exactPath)
	agentsDir := filepath.Dir(pluginsDir)
	if filepath.Base(exactPath) != "marketplace.json" || filepath.Base(pluginsDir) != "plugins" || filepath.Base(agentsDir) != ".agents" {
		return "", "", errors.New("path must end in .agents/plugins/marketplace.json")
	}
	root = filepath.Dir(agentsDir)
	return exactPath, root, nil
}

func configuredCodexCatalogs(configPath string, marketplaces map[string]codexMarketplace, result *Result) []codexCatalogOrigin {
	if len(marketplaces) > maxCodexCatalogs {
		addDiagnostic(result, extensions.SeverityError, "marketplace-catalog-limit", configPath,
			fmt.Sprintf("Codex config names %d marketplaces; limit is %d and none are inspected", len(marketplaces), maxCodexCatalogs))
		return nil
	}
	names := make([]string, 0, len(marketplaces))
	for name := range marketplaces {
		names = append(names, name)
	}
	sort.Strings(names)
	origins := make([]codexCatalogOrigin, 0, len(names))
	for _, name := range names {
		marketplace := marketplaces[name]
		if marketplace.SourceType != "local" {
			addDiagnostic(result, extensions.SeverityWarning, "unsupported-marketplace-source", configPath,
				fmt.Sprintf("Codex marketplace %q uses source type %q; offline inventory reads only exact local catalogs", name, marketplace.SourceType))
			continue
		}
		root, err := codexUserMarketplaceRoot(configPath, marketplace.Source)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "invalid-marketplace", configPath,
				fmt.Sprintf("Codex marketplace %q source is rejected: %v", name, err))
			continue
		}
		origins = append(origins, codexCatalogOrigin{
			root:            root,
			indexPath:       filepath.Join(root, filepath.FromSlash(marketplaceIndex)),
			expectedName:    name,
			scope:           extensions.ScopeUser,
			declarationPath: configPath,
		})
	}
	return origins
}

func codexUserMarketplaceRoot(configPath, source string) (string, error) {
	if source == "" {
		return "", errors.New("local source path is empty")
	}
	if filepath.IsAbs(source) {
		return exactAbsoluteDirectory(source)
	}
	return resolveLocalDirectory(filepath.Dir(configPath), source)
}

func discoverCodexCatalogs(origins []codexCatalogOrigin, result *Result) []ResolvedCandidate {
	if len(origins) > maxCodexCatalogs {
		addDiagnostic(result, extensions.SeverityError, "marketplace-catalog-limit", "",
			fmt.Sprintf("Codex resolution selected %d marketplace catalogs; limit is %d and none are inspected", len(origins), maxCodexCatalogs))
		return nil
	}
	sort.SliceStable(origins, func(i, j int) bool {
		if origins[i].scope != origins[j].scope {
			return origins[i].scope < origins[j].scope
		}
		if origins[i].root != origins[j].root {
			return origins[i].root < origins[j].root
		}
		if origins[i].expectedName != origins[j].expectedName {
			return origins[i].expectedName < origins[j].expectedName
		}
		return origins[i].declarationPath < origins[j].declarationPath
	})
	candidates := make([]ResolvedCandidate, 0)
	for _, origin := range origins {
		var index codexMarketplaceIndex
		if err := readJSONWithin(origin.root, marketplaceIndex, &index); err != nil {
			if origin.optional && os.IsNotExist(err) {
				continue
			}
			addDiagnostic(result, extensions.SeverityError, "invalid-marketplace", origin.indexPath,
				"cannot read exact Codex marketplace inventory: "+err.Error())
			continue
		}
		if index.Name == "" {
			addDiagnostic(result, extensions.SeverityError, "invalid-marketplace", origin.indexPath, "Codex marketplace index has no name")
			continue
		}
		if origin.expectedName != "" && index.Name != origin.expectedName {
			addDiagnostic(result, extensions.SeverityError, "invalid-marketplace", origin.indexPath,
				fmt.Sprintf("Codex marketplace index name %q does not match config key %q", index.Name, origin.expectedName))
			continue
		}
		plugins, ok := validateCodexCatalogPolicies(origin, index, result)
		if !ok {
			continue
		}
		candidates = append(candidates, catalogCandidates(origin, index, plugins, result)...)
	}
	return deduplicateCatalogCandidates(candidates, result)
}

func validateCodexCatalogPolicies(origin codexCatalogOrigin, index codexMarketplaceIndex, result *Result) ([]validatedCodexMarketplacePlugin, bool) {
	if len(index.Plugins) > maxCodexCatalogEntries {
		addDiagnostic(result, extensions.SeverityError, "marketplace-entry-limit", origin.indexPath,
			fmt.Sprintf("Codex marketplace %q has %d plugin entries; limit is %d and no sources are inspected", index.Name, len(index.Plugins), maxCodexCatalogEntries))
		return nil, false
	}
	plugins := make([]validatedCodexMarketplacePlugin, len(index.Plugins))
	valid := true
	for position, plugin := range index.Plugins {
		policy, eligible, err := decodeCodexMarketplacePolicy(plugin.Policy)
		if err != nil {
			nativeID := plugin.Name + "@" + index.Name
			addDiagnostic(result, extensions.SeverityError, "invalid-plugin-policy", origin.indexPath,
				fmt.Sprintf("Codex catalog plugin %q has an invalid policy: %v; native Codex rejects the entire marketplace", nativeID, err))
			valid = false
			continue
		}
		plugins[position] = validatedCodexMarketplacePlugin{plugin: plugin, policy: policy, eligible: eligible}
	}
	if !valid {
		return nil, false
	}
	return plugins, true
}

func catalogCandidates(origin codexCatalogOrigin, index codexMarketplaceIndex, plugins []validatedCodexMarketplacePlugin, result *Result) []ResolvedCandidate {
	plugins = append([]validatedCodexMarketplacePlugin(nil), plugins...)
	sort.SliceStable(plugins, func(i, j int) bool {
		if plugins[i].plugin.Name != plugins[j].plugin.Name {
			return plugins[i].plugin.Name < plugins[j].plugin.Name
		}
		return string(plugins[i].plugin.Source) < string(plugins[j].plugin.Source)
	})
	candidates := make([]ResolvedCandidate, 0, len(plugins))
	for indexPosition := 0; indexPosition < len(plugins); {
		end := indexPosition + 1
		for end < len(plugins) && plugins[end].plugin.Name == plugins[indexPosition].plugin.Name {
			end++
		}
		validated := plugins[indexPosition]
		plugin := validated.plugin
		if end-indexPosition > 1 {
			addDiagnostic(result, extensions.SeverityError, "duplicate-marketplace-plugin", origin.indexPath,
				fmt.Sprintf("Codex marketplace %q repeats plugin %q; no source was chosen", index.Name, plugin.Name))
			indexPosition = end
			continue
		}
		indexPosition = end

		nativeID := plugin.Name + "@" + index.Name
		if parsedPlugin, parsedMarketplace, err := splitNativeID(nativeID); err != nil || parsedPlugin != plugin.Name || parsedMarketplace != index.Name {
			addDiagnostic(result, extensions.SeverityError, "invalid-native-id", origin.indexPath,
				fmt.Sprintf("Codex catalog entry %q in marketplace %q does not form an unambiguous native ID", plugin.Name, index.Name))
			continue
		}
		policy := validated.policy
		if !validated.eligible {
			code := "plugin-product-ineligible"
			message := fmt.Sprintf("Codex catalog plugin %q does not include product %s; no available candidate is emitted", nativeID, codexProductCodex)
			if policy.Installation == codexInstallNotAvailable {
				code = "plugin-not-available"
				message = fmt.Sprintf("Codex catalog plugin %q has installation policy %s; no available candidate is emitted", nativeID, policy.Installation)
			}
			addDiagnostic(result, extensions.SeverityWarning, code, origin.indexPath, message)
			continue
		}
		source, err := decodeCodexLocalSource(plugin.Source)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "invalid-plugin-source", origin.indexPath,
				fmt.Sprintf("Codex catalog plugin %q has an invalid source: %v", nativeID, err))
			continue
		}
		if source.Source != "local" {
			addDiagnostic(result, extensions.SeverityWarning, "unsupported-plugin-source", origin.indexPath,
				fmt.Sprintf("Codex catalog plugin %q uses source %q; no local inventory candidate is emitted", nativeID, source.Source))
			continue
		}
		pluginRoot, err := resolveLocalDirectory(origin.root, source.Path)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "unsafe-plugin-source", origin.indexPath,
				fmt.Sprintf("Codex catalog plugin %q source %q is rejected: %v", nativeID, source.Path, err))
			continue
		}
		candidate := extensions.Candidate{
			Root:    pluginRoot,
			Scope:   origin.scope,
			Dialect: extensions.DialectCodex,
		}
		if !validateNativeManifestIdentity(nativeID, candidate, result) {
			continue
		}
		candidates = append(candidates, ResolvedCandidate{
			NativeID:           nativeID,
			State:              CandidateAvailable,
			NativeEnabled:      false,
			ActivationEligible: false,
			Candidate:          candidate,
			Provenance: Provenance{
				Dialect:           extensions.DialectCodex,
				NativeID:          nativeID,
				RegistryPath:      origin.indexPath,
				Marketplace:       index.Name,
				MarketplacePath:   origin.declarationPath,
				MarketplaceScope:  origin.scope,
				NativeScope:       "catalog",
				ProjectPath:       origin.projectPath,
				MarketplacePolicy: &policy,
			},
		})
	}
	return candidates
}

func decodeCodexMarketplacePolicy(raw json.RawMessage) (CodexMarketplacePolicy, bool, error) {
	if len(raw) == 0 {
		return CodexMarketplacePolicy{
			Installation:   codexInstallAvailable,
			Authentication: codexAuthOnInstall,
		}, true, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return CodexMarketplacePolicy{}, false, errors.New("policy must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document struct {
		Installation   json.RawMessage `json:"installation"`
		Authentication json.RawMessage `json:"authentication"`
		Products       json.RawMessage `json:"products"`
	}
	if err := decoder.Decode(&document); err != nil {
		return CodexMarketplacePolicy{}, false, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return CodexMarketplacePolicy{}, false, err
		}
		return CodexMarketplacePolicy{}, false, fmt.Errorf("unexpected JSON after policy: %v", token)
	}
	installation, err := decodeCodexPolicyString(document.Installation, codexInstallAvailable, "installation")
	if err != nil {
		return CodexMarketplacePolicy{}, false, err
	}
	authentication, err := decodeCodexPolicyString(document.Authentication, codexAuthOnInstall, "authentication")
	if err != nil {
		return CodexMarketplacePolicy{}, false, err
	}
	policy := CodexMarketplacePolicy{
		Installation:   installation,
		Authentication: authentication,
	}
	if len(document.Products) > 0 {
		if !bytes.Equal(bytes.TrimSpace(document.Products), []byte("null")) {
			if err := json.Unmarshal(document.Products, &policy.Products); err != nil {
				return CodexMarketplacePolicy{}, false, fmt.Errorf("invalid products: %w", err)
			}
		}
	}
	switch policy.Installation {
	case codexInstallNotAvailable, codexInstallAvailable, codexInstallInstalledByDefault:
	default:
		return CodexMarketplacePolicy{}, false, fmt.Errorf("unsupported installation value %q", policy.Installation)
	}
	switch policy.Authentication {
	case codexAuthOnInstall, codexAuthOnUse:
	default:
		return CodexMarketplacePolicy{}, false, fmt.Errorf("unsupported authentication value %q", policy.Authentication)
	}

	productEligible := policy.Products == nil
	seenProducts := make(map[string]bool, len(policy.Products))
	normalizedProducts := make([]string, 0, len(policy.Products))
	for _, rawProduct := range policy.Products {
		product, ok := normalizeCodexProduct(rawProduct)
		if !ok {
			return CodexMarketplacePolicy{}, false, fmt.Errorf("unsupported product %q", rawProduct)
		}
		if !seenProducts[product] {
			normalizedProducts = append(normalizedProducts, product)
			seenProducts[product] = true
		}
		productEligible = productEligible || product == codexProductCodex
	}
	sort.Strings(normalizedProducts)
	if policy.Products != nil {
		policy.Products = normalizedProducts
	}
	return policy, policy.Installation != codexInstallNotAvailable && productEligible, nil
}

func decodeCodexPolicyString(raw json.RawMessage, defaultValue, field string) (string, error) {
	if len(raw) == 0 {
		return defaultValue, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("%s must be a string", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid %s: %w", field, err)
	}
	return value, nil
}

func normalizeCodexProduct(product string) (string, bool) {
	switch product {
	case codexProductCodex, "codex":
		return codexProductCodex, true
	case codexProductChatGPT, "chatgpt":
		return codexProductChatGPT, true
	case codexProductAtlas, "atlas":
		return codexProductAtlas, true
	default:
		return "", false
	}
}

func decodeCodexLocalSource(raw json.RawMessage) (codexLocalSource, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var source codexLocalSource
	if err := decoder.Decode(&source); err != nil {
		return codexLocalSource{}, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return codexLocalSource{}, err
		}
		return codexLocalSource{}, fmt.Errorf("unexpected JSON after source: %v", token)
	}
	return source, nil
}

func deduplicateCatalogCandidates(candidates []ResolvedCandidate, result *Result) []ResolvedCandidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].NativeID != candidates[j].NativeID {
			return candidates[i].NativeID < candidates[j].NativeID
		}
		if candidates[i].Candidate.Scope != candidates[j].Candidate.Scope {
			return candidates[i].Candidate.Scope < candidates[j].Candidate.Scope
		}
		if candidates[i].Candidate.Root != candidates[j].Candidate.Root {
			return candidates[i].Candidate.Root < candidates[j].Candidate.Root
		}
		return candidates[i].Provenance.MarketplacePath < candidates[j].Provenance.MarketplacePath
	})
	seen := make(map[string]bool)
	rootCounts := make(map[string]int)
	deduplicated := candidates[:0]
	for _, candidate := range candidates {
		realRoot, err := canonicalDirectory(candidate.Candidate.Root)
		if err != nil {
			addDiagnostic(result, extensions.SeverityError, "unsafe-plugin-source", candidate.Candidate.Root,
				"Codex catalog candidate cannot be canonicalized: "+err.Error())
			continue
		}
		key := candidate.NativeID + "\x00" + string(candidate.Candidate.Scope) + "\x00" + realRoot
		if seen[key] {
			addDiagnostic(result, extensions.SeverityWarning, "duplicate-available-candidate", candidate.Provenance.RegistryPath,
				fmt.Sprintf("Codex catalog repeats available plugin %q at physical root %q; one inventory record is returned", candidate.NativeID, realRoot))
			continue
		}
		seen[key] = true
		rootCounts[candidate.NativeID]++
		deduplicated = append(deduplicated, candidate)
	}
	for id, count := range rootCounts {
		if count > 1 {
			addDiagnostic(result, extensions.SeverityWarning, "ambiguous-available-candidate", "",
				fmt.Sprintf("Codex plugin %q has %d distinct available catalog roots; none is preferred", id, count))
		}
	}
	return deduplicated
}

func reportUnresolvedCodexEnablement(configPath string, plugins map[string]codexPluginSetting, result *Result) {
	ids := make([]string, 0, len(plugins))
	for id := range plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		setting := plugins[id]
		if !setting.isEnabled() {
			continue
		}
		if _, _, err := splitNativeID(id); err != nil {
			addDiagnostic(result, extensions.SeverityError, "invalid-native-id", configPath,
				fmt.Sprintf("enabled Codex plugin ID %q: %v", id, err))
			continue
		}
		if len(setting.MCPServers) > 0 {
			addDiagnostic(result, extensions.SeverityWarning, "unsupported-plugin-policy", configPath,
				fmt.Sprintf("enabled Codex plugin %q declares native MCP policy; no Switchboard permission is inferred", id))
		}
		// A warning rather than an error: nothing here is broken and nothing
		// in this build is degraded. Codex enabled a plugin in its own config
		// and this build declines to guess where it was installed, which is a
		// documented limit rather than a failure, and one the user cannot act
		// on from here. Reported as an error it filled the first screen with
		// red for a condition that is working as designed.
		addDiagnostic(result, extensions.SeverityWarning, "enabled-plugin-install-unresolved", configPath,
			fmt.Sprintf("Codex plugin %q is enabled for Codex and is not loaded here; its install location is not discoverable and this build does not guess at one", id))
	}
}

func splitNativeID(id string) (plugin, marketplace string, err error) {
	separator := strings.LastIndexByte(id, '@')
	if separator <= 0 || separator == len(id)-1 {
		return "", "", errors.New("ID must be plugin-name@marketplace-name")
	}
	plugin, marketplace = id[:separator], id[separator+1:]
	if strings.ContainsRune(plugin, '@') || strings.ContainsRune(marketplace, '@') {
		return "", "", errors.New("ID components cannot contain @")
	}
	for _, character := range plugin + marketplace {
		if character == '/' || character == '\\' || unicode.IsControl(character) {
			return "", "", errors.New("ID contains a path separator or control character")
		}
	}
	return plugin, marketplace, nil
}
