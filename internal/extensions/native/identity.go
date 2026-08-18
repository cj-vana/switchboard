package native

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/switchboard-code/switchboard/internal/extensions"
)

const maxNativeManifestBytes = 1 << 20

// validateNativeManifestIdentity is the bounded identity join for catalog-only
// inventory. It deliberately reads only the manifest instead of digesting the
// full source tree: catalog presence is not installation or activation proof,
// and callers run ordinary extension discovery before presenting inventory.
func validateNativeManifestIdentity(nativeID string, candidate extensions.Candidate, result *Result) bool {
	namespace, _, err := splitNativeID(nativeID)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-mismatch", candidate.Root,
			fmt.Sprintf("native plugin ID %q cannot be joined to a manifest: %v", nativeID, err))
		return false
	}
	if candidate.Dialect != extensions.DialectCodex {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("manifest-only identity validation does not support %s", candidate.Dialect))
		return false
	}

	const manifestRelative = ".codex-plugin/plugin.json"
	raw, err := readWithin(candidate.Root, manifestRelative, maxNativeManifestBytes)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest cannot be read at its exact catalog root: %v", nativeID, err))
		return false
	}
	if err := validateUniqueJSON(raw); err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest is invalid: %v", nativeID, err))
		return false
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest == nil {
		if err == nil {
			err = fmt.Errorf("manifest must be a JSON object")
		}
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest is invalid: %v", nativeID, err))
		return false
	}
	nameRaw, ok := manifest["name"]
	if !ok {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest requires a name", nativeID))
		return false
	}
	var manifestNamespace string
	if err := json.Unmarshal(nameRaw, &manifestNamespace); err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest name must be a string: %v", nativeID, err))
		return false
	}
	manifestNamespace = strings.TrimSpace(manifestNamespace)
	if manifestNamespace == "" || manifestNamespace == "." || manifestNamespace == ".." ||
		strings.ContainsAny(manifestNamespace, "/\\") || strings.IndexFunc(manifestNamespace, unicode.IsControl) >= 0 {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q manifest name %q is not a valid namespace", nativeID, manifestNamespace))
		return false
	}
	if manifestNamespace != namespace {
		addDiagnostic(result, extensions.SeverityError, "native-identity-mismatch", filepath.Join(candidate.Root, filepath.FromSlash(manifestRelative)),
			fmt.Sprintf("native plugin %q names namespace %q, but the catalog manifest names %q; activation is denied", nativeID, namespace, manifestNamespace))
		return false
	}
	return true
}

// discoverNativeIdentity performs the same bounded, read-only discovery that
// downstream inventory uses, then binds a native registry/catalog ID to the
// manifest identity at the exact candidate root. Native IDs have the form
// namespace@marketplace; marketplace membership is provenance, not part of the
// plugin manifest namespace.
func discoverNativeIdentity(nativeID string, candidate extensions.Candidate, result *Result) (extensions.Plugin, bool) {
	namespace, _, err := splitNativeID(nativeID)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-mismatch", candidate.Root,
			fmt.Sprintf("native plugin ID %q cannot be joined to a manifest: %v", nativeID, err))
		return extensions.Plugin{}, false
	}

	discovered := extensions.Discover([]extensions.Candidate{candidate})
	result.Diagnostics = append(result.Diagnostics, discovered.Diagnostics...)
	if len(discovered.Plugins) != 1 {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q resolved to %d discovered %s plugins at its exact root; activation is denied", nativeID, len(discovered.Plugins), candidate.Dialect))
		return extensions.Plugin{}, false
	}

	plugin := discovered.Plugins[0]
	candidateRoot, err := filepath.Abs(candidate.Root)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q root cannot be made absolute after discovery: %v", nativeID, err))
		return extensions.Plugin{}, false
	}
	realRoot, err := canonicalDirectory(candidate.Root)
	if err != nil {
		addDiagnostic(result, extensions.SeverityError, "native-identity-unresolved", candidate.Root,
			fmt.Sprintf("native plugin %q root cannot be canonicalized after discovery: %v", nativeID, err))
		return extensions.Plugin{}, false
	}
	if plugin.Dialect != candidate.Dialect || plugin.Scope != candidate.Scope ||
		filepath.Clean(plugin.Root) != filepath.Clean(candidateRoot) || filepath.Clean(plugin.RealPath) != filepath.Clean(realRoot) {
		addDiagnostic(result, extensions.SeverityError, "native-identity-mismatch", candidate.Root,
			fmt.Sprintf("native plugin %q does not match discovered dialect, scope, and physical root; activation is denied", nativeID))
		return extensions.Plugin{}, false
	}
	if plugin.Namespace != namespace || plugin.ID != string(candidate.Dialect)+":"+namespace {
		addDiagnostic(result, extensions.SeverityError, "native-identity-mismatch", plugin.Manifest,
			fmt.Sprintf("native plugin %q names namespace %q, but the discovered manifest names %q; activation is denied", nativeID, namespace, plugin.Namespace))
		return extensions.Plugin{}, false
	}
	return plugin, true
}
