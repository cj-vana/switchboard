package mcppolicy

import (
	"fmt"

	"github.com/BurntSushi/toml"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

type codexPolicy struct {
	unavailable      bool
	restricted       bool
	rules            map[string]codexRule
	pluginRestricted bool
	pluginRules      map[string]map[string]codexRule
}

type codexRule struct {
	command *codexCommandRule
	url     *mcpnative.PolicyValueMatcher
}

type codexCommandRule struct {
	// simple distinguishes identity.command = "..." from the structured
	// executable/args form. The simple form intentionally ignores arguments.
	simple     bool
	executable string
	args       []mcpnative.PolicyValueMatcher
}

type codexRequirements struct {
	present        bool
	servers        map[string]any
	pluginsPresent bool
	plugins        map[string]any
}

func parseCodexRequirements(data []byte) (codexRequirements, error) {
	var root map[string]any
	_, err := toml.Decode(string(data), &root)
	if err != nil {
		return codexRequirements{}, fmt.Errorf("invalid TOML")
	}
	result := codexRequirements{}
	if value, present := root["mcp_servers"]; present {
		servers, ok := asStringMap(value)
		if !ok {
			return codexRequirements{}, fmt.Errorf("mcp_servers is not a table")
		}
		result.present = true
		result.servers = cloneStringMap(servers)
	}
	if value, present := root["plugins"]; present {
		plugins, ok := asStringMap(value)
		if !ok {
			return codexRequirements{}, fmt.Errorf("plugins is not a table")
		}
		result.pluginsPresent = true
		result.plugins = cloneStringMap(plugins)
	}
	return result, nil
}

func mergeCodexRequirements(base *codexRequirements, next codexRequirements) {
	if !next.present {
		// Plugin MCP requirements are independent of the top-level allowlist.
	} else {
		base.present = true
		if base.servers == nil {
			base.servers = make(map[string]any)
		}
		deepMergeStringMap(base.servers, next.servers)
	}
	if next.pluginsPresent {
		base.pluginsPresent = true
		if base.plugins == nil {
			base.plugins = make(map[string]any)
		}
		deepMergeStringMap(base.plugins, next.plugins)
	}
}

func compileCodexPolicy(requirements codexRequirements) (codexPolicy, error) {
	rules, err := compileCodexRules(requirements.servers)
	if err != nil {
		return codexPolicy{}, err
	}
	result := codexPolicy{
		restricted: requirements.present, rules: rules,
		pluginRules: make(map[string]map[string]codexRule),
	}
	if !requirements.pluginsPresent {
		return result, nil
	}
	if len(requirements.plugins) > MaxPolicyEntries {
		return codexPolicy{}, fmt.Errorf("too many plugin MCP requirements")
	}
	pluginServerCount := 0
	for pluginID, rawPlugin := range requirements.plugins {
		plugin, ok := asStringMap(rawPlugin)
		if !ok {
			return codexPolicy{}, fmt.Errorf("invalid plugin MCP requirements")
		}
		rawServers, exists := plugin["mcp_servers"]
		if !exists {
			// Other plugin requirements do not activate MCP filtering. Codex
			// starts filtering plugin MCP only when at least one plugin has an
			// explicit mcp_servers requirement.
			continue
		}
		result.pluginRestricted = true
		servers, ok := asStringMap(rawServers)
		if !ok {
			return codexPolicy{}, fmt.Errorf("invalid plugin MCP requirements")
		}
		pluginServerCount += len(servers)
		if pluginServerCount > MaxPolicyEntries {
			return codexPolicy{}, fmt.Errorf("too many plugin MCP requirements")
		}
		compiled, compileErr := compileCodexRules(servers)
		if compileErr != nil {
			return codexPolicy{}, compileErr
		}
		result.pluginRules[pluginID] = compiled
	}
	return result, nil
}

func compileCodexRules(servers map[string]any) (map[string]codexRule, error) {
	if len(servers) > MaxPolicyEntries {
		return nil, fmt.Errorf("too many mcp_servers entries")
	}
	result := make(map[string]codexRule, len(servers))
	for name, rawServer := range servers {
		server, ok := asStringMap(rawServer)
		if !ok || !onlyKeys(server, "identity", "description") {
			return nil, fmt.Errorf("invalid mcp_servers entry")
		}
		if description, present := server["description"]; present {
			if _, ok := description.(string); !ok {
				return nil, fmt.Errorf("invalid mcp_servers description")
			}
		}
		rawIdentity, exists := server["identity"]
		identity, ok := asStringMap(rawIdentity)
		if !exists || !ok || len(identity) != 1 {
			return nil, fmt.Errorf("invalid MCP identity")
		}
		if rawCommand, exists := identity["command"]; exists {
			command, commandErr := parseCodexCommand(rawCommand)
			if commandErr != nil {
				return nil, commandErr
			}
			result[name] = codexRule{command: &command}
			continue
		}
		if rawURL, exists := identity["url"]; exists {
			matcher, matcherErr := parseCodexMatcher(rawURL)
			if matcherErr != nil {
				return nil, matcherErr
			}
			result[name] = codexRule{url: &matcher}
			continue
		}
		return nil, fmt.Errorf("unsupported MCP identity")
	}
	return result, nil
}

func parseCodexCommand(raw any) (codexCommandRule, error) {
	if value, ok := raw.(string); ok {
		return codexCommandRule{simple: true, executable: value}, nil
	}
	table, ok := asStringMap(raw)
	if !ok || !onlyKeys(table, "executable", "args") {
		return codexCommandRule{}, fmt.Errorf("invalid command identity")
	}
	executable, executableOK := table["executable"].(string)
	rawArgs, argsExist := table["args"]
	if !executableOK || !argsExist {
		return codexCommandRule{}, fmt.Errorf("invalid command identity")
	}
	arguments, ok := asAnySlice(rawArgs)
	if !ok || len(arguments) > MaxPolicyValues {
		return codexCommandRule{}, fmt.Errorf("invalid command arguments")
	}
	matchers := make([]mcpnative.PolicyValueMatcher, 0, len(arguments))
	for _, rawMatcher := range arguments {
		matcher, err := parseCodexMatcher(rawMatcher)
		if err != nil {
			return codexCommandRule{}, err
		}
		matchers = append(matchers, matcher)
	}
	return codexCommandRule{executable: executable, args: matchers}, nil
}

func parseCodexMatcher(raw any) (mcpnative.PolicyValueMatcher, error) {
	if value, ok := raw.(string); ok {
		return mcpnative.NewExactPolicyMatcher(value), nil
	}
	table, ok := asStringMap(raw)
	if !ok || !onlyKeys(table, "match", "value", "expression") {
		return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
	}
	kindString, ok := table["match"].(string)
	if !ok {
		return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
	}
	switch mcpnative.PolicyMatchKind(kindString) {
	case mcpnative.PolicyMatchExact, mcpnative.PolicyMatchPrefix:
		value, valueOK := table["value"].(string)
		_, hasExpression := table["expression"]
		if !valueOK || hasExpression {
			return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
		}
		if kindString == string(mcpnative.PolicyMatchExact) {
			return mcpnative.NewExactPolicyMatcher(value), nil
		}
		return mcpnative.NewPrefixPolicyMatcher(value), nil
	case mcpnative.PolicyMatchRegex:
		expression, expressionOK := table["expression"].(string)
		_, hasValue := table["value"]
		if !expressionOK || hasValue {
			return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
		}
		matcher, err := mcpnative.NewRegexPolicyMatcher(expression)
		if err != nil {
			return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
		}
		return matcher, nil
	default:
		return mcpnative.PolicyValueMatcher{}, fmt.Errorf("invalid identity matcher")
	}
}

func (policy codexPolicy) allowed(request mcpnative.PolicyRequest) (bool, error) {
	if policy.unavailable {
		return false, ErrCodexPolicyUnavailable
	}
	rules := policy.rules
	restricted := policy.restricted
	if request.Scope == mcpnative.ScopePlugin || request.Source == mcpnative.SourceCodexPlugin {
		restricted = policy.pluginRestricted
		if !restricted {
			return true, nil
		}
		var exists bool
		rules, exists = policy.pluginRules[request.PluginID]
		if !exists {
			return false, nil
		}
	}
	if !restricted {
		return true, nil
	}
	rule, exists := rules[request.Name]
	if !exists {
		return false, nil
	}
	if rule.command != nil {
		if request.Transport != mcpnative.TransportStdio || !request.CommandMatches(rule.command.executable) {
			return false, nil
		}
		if rule.command.simple {
			return true, nil
		}
		return request.ArgsMatchPolicy(rule.command.args)
	}
	if rule.url != nil {
		if request.Transport != mcpnative.TransportHTTP {
			return false, nil
		}
		return request.URLMatchesPolicy(*rule.url)
	}
	return false, ErrCodexPolicyUnavailable
}

func asStringMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok && result != nil
}

func asAnySlice(value any) ([]any, bool) {
	if values, ok := value.([]any); ok {
		return values, true
	}
	if values, ok := value.([]map[string]any); ok {
		result := make([]any, len(values))
		for index := range values {
			result[index] = values[index]
		}
		return result, true
	}
	return nil, false
}

func cloneStringMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		if child, ok := asStringMap(value); ok {
			result[key] = cloneStringMap(child)
		} else {
			result[key] = value
		}
	}
	return result
}

func deepMergeStringMap(base, higher map[string]any) {
	for key, highValue := range higher {
		if highMap, ok := asStringMap(highValue); ok {
			if lowMap, lowOK := asStringMap(base[key]); lowOK {
				deepMergeStringMap(lowMap, highMap)
				continue
			}
			base[key] = cloneStringMap(highMap)
			continue
		}
		base[key] = highValue
	}
}

func onlyKeys(table map[string]any, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range table {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}
