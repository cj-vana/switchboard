package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
	"github.com/switchboard-code/switchboard/internal/trust"
)

// nativeMCPRuntimeFeatures is an explicit compatibility claim, not a wish
// list. Every feature here is mapped below into internal/mcp with a regression
// test. Materialize rejects all other native semantics before releasing any
// sensitive value.
var nativeMCPRuntimeFeatures = []mcpnative.Feature{
	mcpnative.FeatureCWD,
	mcpnative.FeatureForwardedEnv,
	mcpnative.FeatureHTTPHeaders,
	mcpnative.FeatureHTTPHeaderEnv,
	mcpnative.FeatureBearerEnv,
	mcpnative.FeatureStartupTimeout,
	mcpnative.FeatureToolTimeout,
	mcpnative.FeatureClaudeTimeout,
	mcpnative.FeatureRequired,
	mcpnative.FeatureToolFilters,
	mcpnative.FeatureClaudeExpansion,
	mcpnative.FeatureAlwaysLoad,
}

func activatedNativeMCPSpecs(inv *nativeMCPInventory, trustStore *trust.Store, policy nativeMCPAssemblyPolicy) ([]mcp.Spec, []mcpNote, error) {
	if inv == nil {
		return nil, nil, nil
	}
	notes := append([]mcpNote(nil), inv.notes...)
	if inv.codexSnapshotErr != nil {
		return nil, notes, inv.codexSnapshotErr
	}
	var trustChecker mcpnative.TrustChecker
	if trustStore != nil {
		trustChecker = trustStore
	}
	var specs []mcp.Spec
	for _, server := range inv.result.Servers {
		request, err := inv.result.ActivationRequest(server.ID)
		if err != nil || inv.state == nil || !inv.state.NativeMCPActivated(request) {
			continue
		}
		materialized, err := inv.result.Materialize(server.ID, trustChecker, policy, inv.state, nativeMCPRuntimeFeatures...)
		if err != nil {
			message := fmt.Sprintf("native MCP server %s stays off: %v", server.ID, err)
			if server.Required {
				return nil, notes, errors.New(message)
			}
			notes = append(notes, mcpNote{"warn", message})
			continue
		}
		spec, err := nativeMCPRuntimeSpec(materialized, policy)
		if err != nil {
			message := fmt.Sprintf("native MCP server %s stays off: %v", server.ID, err)
			if server.Required {
				return nil, notes, errors.New(message)
			}
			notes = append(notes, mcpNote{"warn", message})
			continue
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, notes, nil
}

func nativeMCPRuntimeSpec(server mcpnative.MaterializedServer, policy nativeMCPAssemblyPolicy) (mcp.Spec, error) {
	var expand func(string) (string, error)
	if server.Provenance.Dialect == mcpnative.DialectClaude {
		expand = policy.expandClaudeValue
	}
	return nativeMCPRuntimeSpecWithExpansion(server, expand)
}

func nativeMCPRuntimeSpecWithExpansion(server mcpnative.MaterializedServer, expand func(string) (string, error)) (mcp.Spec, error) {
	if server.Provenance.Dialect == mcpnative.DialectClaude && expand == nil {
		return mcp.Spec{}, fmt.Errorf("native MCP server %s has no authorized Claude environment snapshot", server.ID)
	}
	spec := mcp.Spec{
		Name:              server.ID,
		RestrictedEnv:     server.Transport == mcpnative.TransportStdio,
		HeaderEnv:         cloneNativeStringMap(server.HeaderEnv),
		BearerTokenEnvVar: server.BearerTokenEnvVar,
		EnabledTools:      append([]string(nil), server.Tools.Enabled...),
		EnabledToolsSet:   server.Tools.EnabledSet,
		DisabledTools:     append([]string(nil), server.Tools.Disabled...),
		DisabledToolsSet:  server.Tools.DisabledSet,
		Required:          server.Required,
	}

	var err error
	if spec.StartupTimeout, err = nativeMCPStartupTimeout(server.Timeouts); err != nil {
		return mcp.Spec{}, fmt.Errorf("native MCP server %s has an invalid startup timeout", server.ID)
	}
	if spec.ToolTimeout, err = nativeMCPToolTimeout(server.Timeouts); err != nil {
		return mcp.Spec{}, fmt.Errorf("native MCP server %s has an invalid tool timeout", server.ID)
	}

	expose := func(field string, value *mcpnative.MaterializedValue) (string, error) {
		if value == nil {
			return "", nil
		}
		return expandNativeMCPValue(server, field, value.Expose(), expand)
	}
	switch server.Transport {
	case mcpnative.TransportStdio:
		if server.Command == nil {
			return mcp.Spec{}, fmt.Errorf("native MCP server %s has no command", server.ID)
		}
		if spec.Command, err = expose("command", server.Command); err != nil {
			return mcp.Spec{}, err
		}
		for i := range server.Args {
			value, expandErr := expandNativeMCPValue(server, fmt.Sprintf("args[%d]", i), server.Args[i].Expose(), expand)
			if expandErr != nil {
				return mcp.Spec{}, expandErr
			}
			spec.Args = append(spec.Args, value)
		}
		if spec.CWD, err = expose("cwd", server.CWD); err != nil {
			return mcp.Spec{}, err
		}
		spec.Env = make(map[string]string, len(server.Env))
		for _, name := range sortedNativeMCPMaterializedKeys(server.Env) {
			value, expandErr := expandNativeMCPValue(server, "env."+name, server.Env[name].Expose(), expand)
			if expandErr != nil {
				return mcp.Spec{}, expandErr
			}
			spec.Env[name] = value
		}
		if len(spec.Env) == 0 && !server.EnvSet {
			spec.Env = nil
		}
		for _, variable := range server.ForwardedEnv {
			if variable.Source != mcpnative.EnvSourceLocal {
				return mcp.Spec{}, fmt.Errorf("native MCP server %s requests unsupported remote environment forwarding", server.ID)
			}
			spec.InheritEnv = append(spec.InheritEnv, variable.Name)
		}
		sort.Strings(spec.InheritEnv)
	case mcpnative.TransportHTTP:
		if server.URL == nil {
			return mcp.Spec{}, fmt.Errorf("native MCP server %s has no URL", server.ID)
		}
		if spec.URL, err = expose("url", server.URL); err != nil {
			return mcp.Spec{}, err
		}
		spec.Headers = make(map[string]string, len(server.Headers))
		for _, name := range sortedNativeMCPMaterializedKeys(server.Headers) {
			value, expandErr := expandNativeMCPValue(server, "headers."+name, server.Headers[name].Expose(), expand)
			if expandErr != nil {
				return mcp.Spec{}, expandErr
			}
			spec.Headers[name] = value
		}
		if len(spec.Headers) == 0 && !server.HeadersSet {
			spec.Headers = nil
		}
	case mcpnative.TransportSSE, mcpnative.TransportWS:
		return mcp.Spec{}, fmt.Errorf("native MCP server %s uses unsupported %s transport", server.ID, server.Transport)
	default:
		return mcp.Spec{}, fmt.Errorf("native MCP server %s has no supported transport", server.ID)
	}
	return spec, nil
}

func nativeMCPStartupTimeout(value mcpnative.Timeouts) (time.Duration, error) {
	if value.StartupSet {
		return nativeMCPFloatDuration(value.StartupSeconds, time.Second)
	}
	if value.StartupMillisSet {
		return nativeMCPUnsignedDuration(value.StartupMilliseconds, time.Millisecond)
	}
	return 0, nil
}

func nativeMCPToolTimeout(value mcpnative.Timeouts) (time.Duration, error) {
	if value.ToolSet {
		return nativeMCPFloatDuration(value.ToolSeconds, time.Second)
	}
	if value.ClaudeToolSet {
		return nativeMCPFloatDuration(value.ClaudeToolMillis, time.Millisecond)
	}
	return 0, nil
}

func nativeMCPFloatDuration(value float64, unit time.Duration) (time.Duration, error) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > float64(math.MaxInt64)/float64(unit) {
		return 0, errors.New("duration is out of range")
	}
	return time.Duration(value * float64(unit)), nil
}

func nativeMCPUnsignedDuration(value uint64, unit time.Duration) (time.Duration, error) {
	if value > uint64(math.MaxInt64)/uint64(unit) {
		return 0, errors.New("duration is out of range")
	}
	return time.Duration(value) * unit, nil
}

func expandNativeMCPValue(server mcpnative.MaterializedServer, field, value string, expand func(string) (string, error)) (string, error) {
	if server.Provenance.Dialect != mcpnative.DialectClaude {
		return value, nil
	}
	if expand == nil {
		return "", fmt.Errorf("native MCP server %s %s has no authorized Claude environment snapshot", server.ID, field)
	}
	expanded, err := expand(value)
	if err != nil {
		return "", fmt.Errorf("native MCP server %s %s: %w", server.ID, field, err)
	}
	return expanded, nil
}

func expandClaudeEnvironment(value string, lookup func(string) (string, bool)) (string, error) {
	var out strings.Builder
	for cursor := 0; cursor < len(value); {
		start := strings.Index(value[cursor:], "${")
		if start < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start += cursor
		out.WriteString(value[cursor:start])
		endOffset := strings.IndexByte(value[start+2:], '}')
		if endOffset < 0 {
			return "", errors.New("contains an unterminated environment reference")
		}
		end := start + 2 + endOffset
		body := value[start+2 : end]
		name, fallback, hasFallback := body, "", false
		if cut := strings.Index(body, ":-"); cut >= 0 {
			name, fallback, hasFallback = body[:cut], body[cut+2:], true
		}
		if !nativeMCPEnvironmentName(name) {
			return "", fmt.Errorf("contains an invalid environment reference %q", name)
		}
		replacement, exists := lookup(name)
		if !exists || replacement == "" {
			if !hasFallback {
				return "", fmt.Errorf("requires environment variable %s", name)
			}
			replacement = fallback
		}
		out.WriteString(replacement)
		cursor = end + 1
	}
	return out.String(), nil
}

func nativeMCPEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || i > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func cloneNativeStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func sortedNativeMCPMaterializedKeys(values map[string]mcpnative.MaterializedValue) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
