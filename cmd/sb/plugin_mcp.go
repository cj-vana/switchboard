package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/extensions"
	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

// enabledPluginMCPSpecs adapts only Switchboard-enabled, digest-trusted plugin
// components. Native plugin enablement is not consulted here. The plugin is
// rediscovered through the content-addressed cache before and after parsing so
// changed bytes cannot inherit an earlier executable grant.
func enabledPluginMCPSpecs(inv *pluginInventory, workspace string, policy nativeMCPAssemblyPolicy) ([]mcp.Spec, []mcpNote, error) {
	if inv == nil || inv.state == nil {
		return nil, nil, nil
	}
	enabledCounts := make(map[string]int)
	for _, record := range inv.records {
		if record.behaviorEnabled() {
			enabledCounts[record.Plugin.ID]++
		}
	}
	var specs []mcp.Spec
	var notes []mcpNote
	for _, record := range inv.records {
		if !record.behaviorEnabled() || !pluginHasMCP(record.Plugin) {
			continue
		}
		// Claude's managed-mcp.json is an exclusive server source. Plugin MCP
		// is absent in that mode, so even a plugin-local required declaration
		// must not be allowed to abort the host session.
		if record.Plugin.Dialect == extensions.DialectClaude && policy.claudeManagedExclusive {
			continue
		}
		if enabledCounts[record.Plugin.ID] != 1 {
			notes = append(notes, mcpNote{"error", fmt.Sprintf(
				"plugin MCP: %s is enabled from more than one root; all of its servers stay off", record.Plugin.ID)})
			continue
		}
		if !record.Activation.ExecutableTrusted || record.Activation.Changed {
			notes = append(notes, mcpNote{"warn", fmt.Sprintf(
				"plugin MCP: %s stays off until its current executable bytes are trusted with /plugins trust %s",
				record.Plugin.ID, record.Plugin.ID)})
			continue
		}

		candidate, err := cachePluginActivation(record.Plugin)
		if err != nil {
			notes = append(notes, mcpNote{"error", fmt.Sprintf(
				"plugin MCP: %s could not be revalidated; its servers stay off: %v", record.Plugin.ID, err)})
			continue
		}
		plugin := candidate.Plugin()
		status := inv.state.Status(plugin, workspace)
		if !status.Enabled || !status.ExecutableTrusted || status.Changed {
			notes = append(notes, mcpNote{"warn", fmt.Sprintf(
				"plugin MCP: %s changed during activation validation; its servers stay off", record.Plugin.ID)})
			continue
		}
		materializedRecord := record
		materializedRecord.Plugin = plugin
		pluginSpecs, pluginNotes, err := materializePluginMCP(materializedRecord, policy)
		notes = append(notes, pluginNotes...)
		if err != nil {
			return nil, notes, err
		}
		// Recheck the digest after every component was read. This also catches
		// replacing an inline manifest between discovery and extraction.
		after, err := cachePluginActivation(plugin)
		if err != nil || after.Plugin().Digest != plugin.Digest ||
			!inv.state.Status(after.Plugin(), workspace).ExecutableTrusted {
			notes = append(notes, mcpNote{"error", fmt.Sprintf(
				"plugin MCP: %s changed while its MCP declarations were read; its servers stay off", plugin.ID)})
			continue
		}
		specs = append(specs, pluginSpecs...)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, notes, nil
}

func materializePluginMCP(record pluginRecord, policy nativeMCPAssemblyPolicy) ([]mcp.Spec, []mcpNote, error) {
	plugin := record.Plugin
	dialect, ok := pluginMCPDialect(plugin.Dialect)
	if !ok {
		return nil, []mcpNote{{"error", "plugin MCP: unsupported plugin dialect for " + plugin.ID}}, nil
	}
	pluginID := plugin.Namespace
	if dialect == mcpnative.DialectCodex && policy.codexPluginRestricted {
		if len(record.NativeIDs) != 1 {
			return nil, []mcpNote{{"error", fmt.Sprintf(
				"plugin MCP: %s has no unique canonical Codex marketplace identity; policy-restricted servers stay off", plugin.ID)}}, nil
		}
		pluginID = record.NativeIDs[0]
	}
	var specs []mcp.Spec
	var notes []mcpNote
	for _, component := range plugin.Components {
		if component.Kind != extensions.ComponentMCP {
			continue
		}
		opts := mcpnative.PluginMCPOptions{
			Dialect: dialect, PluginID: pluginID, PluginRoot: plugin.RealPath,
			TrustRoot: plugin.RealPath, Shape: mcpnative.PluginMCPAuto,
		}
		if component.Inline {
			if plugin.Manifest == "" {
				notes = append(notes, mcpNote{"error", fmt.Sprintf(
					"plugin MCP: %s declares inline MCP without a readable manifest", plugin.ID)})
				continue
			}
			opts.Path = plugin.Manifest
			opts.ManifestField = "mcpServers"
		} else {
			opts.Path = component.RealPath
		}
		result := mcpnative.ParsePluginMCP(opts)
		activation := exactPluginMCPAuthority{
			root: plugin.RealPath, dialect: dialect, pluginID: pluginID,
			names: make(map[string]struct{}, len(result.Servers)),
		}
		for _, server := range result.Servers {
			activation.names[server.Name] = struct{}{}
		}
		for _, diagnostic := range result.Diagnostics {
			level := "warn"
			if diagnostic.Severity == mcpnative.SeverityError {
				level = "error"
			}
			text := fmt.Sprintf("plugin MCP %s %s: %s", plugin.ID, diagnostic.Code, diagnostic.Message)
			if diagnostic.Entry != "" {
				text += " [" + diagnostic.Entry + "]"
			}
			if diagnostic.Field != "" {
				text += " field " + diagnostic.Field
			}
			notes = append(notes, mcpNote{level, text})
		}
		for _, server := range result.Servers {
			materialized, err := result.Materialize(server.ID, activation, policy, activation, nativeMCPRuntimeFeatures...)
			if err != nil {
				message := fmt.Sprintf("plugin MCP server %s stays off: %v", server.ID, err)
				if server.Required {
					return nil, notes, errors.New(message)
				}
				notes = append(notes, mcpNote{"warn", message})
				continue
			}
			spec, err := nativeMCPRuntimeSpec(materialized, policy)
			if err != nil {
				message := fmt.Sprintf("plugin MCP server %s stays off: %v", server.ID, err)
				if server.Required {
					return nil, notes, errors.New(message)
				}
				notes = append(notes, mcpNote{"warn", message})
				continue
			}
			specs = append(specs, spec)
		}
	}
	return specs, notes, nil
}

func pluginHasMCP(plugin extensions.Plugin) bool {
	for _, component := range plugin.Components {
		if component.Kind == extensions.ComponentMCP {
			return true
		}
	}
	return false
}

func pluginMCPDialect(dialect extensions.Dialect) (mcpnative.Dialect, bool) {
	switch dialect {
	case extensions.DialectCodex:
		return mcpnative.DialectCodex, true
	case extensions.DialectClaude:
		return mcpnative.DialectClaude, true
	default:
		return "", false
	}
}

type exactPluginMCPAuthority struct {
	root     string
	dialect  mcpnative.Dialect
	pluginID string
	names    map[string]struct{}
}

func (a exactPluginMCPAuthority) Trusted(path string) bool {
	return sameResolvedPluginPath(a.root, path)
}

func (a exactPluginMCPAuthority) NativeMCPActivated(request mcpnative.ActivationRequest) bool {
	if !sameResolvedPluginPath(a.root, request.TrustRoot) || !pathWithinPluginRoot(a.root, request.RealPath) {
		return false
	}
	if request.Dialect != a.dialect || request.Scope != mcpnative.ScopePlugin || request.PluginID != a.pluginID {
		return false
	}
	_, allowed := a.names[request.Name]
	return allowed
}

func sameResolvedPluginPath(left, right string) bool {
	leftReal, leftErr := filepath.EvalSymlinks(left)
	rightReal, rightErr := filepath.EvalSymlinks(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftInfo, leftErr := os.Stat(leftReal)
	rightInfo, rightErr := os.Stat(rightReal)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func pathWithinPluginRoot(root, path string) bool {
	rootReal, rootErr := filepath.EvalSymlinks(root)
	pathReal, pathErr := filepath.EvalSymlinks(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	rel, err := filepath.Rel(rootReal, pathReal)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
