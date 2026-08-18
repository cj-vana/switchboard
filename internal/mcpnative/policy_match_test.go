package mcpnative

import (
	"errors"
	"testing"
)

func TestPolicyRequestCodexMatchersStayOpaque(t *testing.T) {
	request := PolicyRequest{
		command: sensitivePointer("runner"),
		args: []SensitiveValue{
			sensitive("--exact"),
			sensitive("prefix-secret-value"),
			sensitive("token-123"),
		},
		url: sensitivePointer("https://example.test/mcp/tenant-42"),
	}
	tokenMatcher, err := NewRegexPolicyMatcher(`token-[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := request.ArgsMatchPolicy([]PolicyValueMatcher{
		NewExactPolicyMatcher("--exact"),
		NewPrefixPolicyMatcher("prefix-"),
		tokenMatcher,
	})
	if err != nil || !matched {
		t.Fatalf("ordered matchers = %v, %v", matched, err)
	}
	urlMatcher, err := NewRegexPolicyMatcher(`https://example\.test/mcp/tenant-[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	matched, err = request.URLMatchesPolicy(urlMatcher)
	if err != nil || !matched {
		t.Fatalf("full URL matcher = %v, %v", matched, err)
	}
	partialMatcher, err := NewRegexPolicyMatcher(`example\.test`)
	if err != nil {
		t.Fatal(err)
	}
	matched, err = request.URLMatchesPolicy(partialMatcher)
	if err != nil || matched {
		t.Fatalf("regex was not full-value matched: %v, %v", matched, err)
	}
	if _, err := request.URLMatchesPolicy(PolicyValueMatcher{}); !errors.Is(err, ErrInvalidPolicyMatcher) {
		t.Fatalf("uninitialized matcher = %v", err)
	}
	if _, err := NewRegexPolicyMatcher("["); !errors.Is(err, ErrInvalidPolicyMatcher) {
		t.Fatalf("bad regex = %v", err)
	}
	for _, expression := range []string{`(?i)é`, `\p{Greek}+`} {
		if _, err := NewRegexPolicyMatcher(expression); !errors.Is(err, ErrInvalidPolicyMatcher) {
			t.Fatalf("regex outside the conservative regex-lite subset %q = %v", expression, err)
		}
	}
	if matched, err := request.URLMatchesPolicy(NewPrefixPolicyMatcher("")); err != nil || !matched {
		t.Fatalf("explicit empty prefix should remain a deliberate native wildcard: %v, %v", matched, err)
	}
}

func TestPolicyRequestClaudeURLPatterns(t *testing.T) {
	tests := []struct {
		value   string
		pattern string
		want    bool
	}{
		{"https://mcp.example.com/api", "https://mcp.example.com/*", true},
		{"https://mcp.example.com/api", "https://mcp.example.com", true},
		{"https://api.internal.example.com/mcp", "https://*.internal.example.com/*", true},
		{"http://localhost:4312/mcp", "http://localhost:*/*", true},
		{"http://mcp.example.com/mcp", "*://mcp.example.com/*", true},
		{"https://MCP.Example.com./Case", "https://mcp.example.com/Case", true},
		{"https://mcp.example.com/Case", "https://mcp.example.com/case", false},
		{"https://example.com/mcp", "https://*.example.com/*", false},
		{"https://staging.example.com/mcp", "https://*.example.com/*", true},
	}
	for _, test := range tests {
		request := PolicyRequest{url: sensitivePointer(test.value)}
		got, err := request.URLMatchesClaudePattern(test.pattern)
		if err != nil || got != test.want {
			t.Errorf("%q against %q = %v, %v; want %v", test.value, test.pattern, got, err, test.want)
		}
	}
	request := PolicyRequest{url: sensitivePointer("https://example.test/mcp")}
	if _, err := request.URLMatchesClaudePattern("not-a-url"); !errors.Is(err, ErrInvalidPolicyMatcher) {
		t.Fatalf("malformed Claude pattern = %v", err)
	}
}

func TestPolicyRequestClaudeExpansionUsesSeparateEnvironments(t *testing.T) {
	request := PolicyRequest{
		command: sensitivePointer("${SERVER_HOME}/bin/server"),
		args:    []SensitiveValue{sensitive("--tenant=${TENANT:-default}")},
		url:     sensitivePointer("${SERVER_SCHEME}://${SERVER_HOST}/mcp/${TENANT}"),
	}
	serverEnvironment := func(name string) (string, bool) {
		values := map[string]string{
			"SERVER_HOME": "/opt", "TENANT": "blue",
			"SERVER_SCHEME": "https", "SERVER_HOST": "mcp.example.test",
		}
		value, ok := values[name]
		return value, ok
	}
	policyEnvironment := func(name string) (string, bool) {
		values := map[string]string{"POLICY_HOME": "/opt", "POLICY_TENANT": "blue"}
		value, ok := values[name]
		return value, ok
	}
	expansion := ClaudePolicyExpansion{
		List: ClaudePolicyDeny, ServerEnvironment: serverEnvironment, PolicyEnvironment: policyEnvironment,
	}
	matched, err := request.CommandMatchesClaude(
		[]string{"${POLICY_HOME}/bin/server", "--tenant=${POLICY_TENANT}"}, expansion,
	)
	if err != nil || !matched {
		t.Fatalf("expanded command = %v, %v", matched, err)
	}
	matched, err = request.URLMatchesClaudePatternExpanded(
		"https://mcp.example.test/mcp/${POLICY_TENANT}", expansion,
	)
	if err != nil || !matched {
		t.Fatalf("expanded deny URL = %v, %v", matched, err)
	}
	if _, err := request.URLMatchesClaudePattern("https://mcp.example.test/*"); !errors.Is(err, ErrPolicyExpansion) {
		t.Fatalf("raw comparison did not require expansion: %v", err)
	}

	// Claude ignores allowlist entries when expansion changes their URL scope.
	expansion.List = ClaudePolicyAllow
	matched, err = request.URLMatchesClaudePatternExpanded(
		"${POLICY_SCHEME:-https}://mcp.example.test/*", expansion,
	)
	if err != nil || matched {
		t.Fatalf("scope-changing allow expansion = %v, %v", matched, err)
	}

	// Query-only policy expansion does not change the URL's scope.
	request.url = sensitivePointer("https://mcp.example.test/mcp?tenant=blue")
	matched, err = request.URLMatchesClaudePatternExpanded(
		"https://mcp.example.test/mcp?tenant=${POLICY_TENANT}", expansion,
	)
	if err != nil || !matched {
		t.Fatalf("query-only allow expansion = %v, %v", matched, err)
	}
}

func sensitivePointer(value string) *SensitiveValue {
	result := sensitive(value)
	return &result
}
