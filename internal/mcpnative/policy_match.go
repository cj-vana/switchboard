package mcpnative

import (
	"crypto/hmac"
	"net/url"
	"regexp"
	"strings"
)

// PolicyMatchKind is one matcher operation accepted by Codex requirements
// identities. Regular expressions are matched against the complete value,
// matching Codex's managed-policy semantics rather than regexp substring
// semantics.
type PolicyMatchKind string

const (
	PolicyMatchExact  PolicyMatchKind = "exact"
	PolicyMatchPrefix PolicyMatchKind = "prefix"
	PolicyMatchRegex  PolicyMatchKind = "regex"
)

// PolicyValueMatcher is the opaque normalized form of one Codex requirements
// matcher. Construct it through NewExactPolicyMatcher,
// NewPrefixPolicyMatcher, or NewRegexPolicyMatcher. Its fields are private so
// a missing required value cannot become an accidental empty-prefix wildcard.
type PolicyValueMatcher struct {
	match      PolicyMatchKind
	value      string
	expression string
	valid      bool
}

// NewExactPolicyMatcher matches one complete value. An explicitly empty value
// is valid and remains distinguishable from an uninitialized matcher.
func NewExactPolicyMatcher(value string) PolicyValueMatcher {
	return PolicyValueMatcher{match: PolicyMatchExact, value: value, valid: true}
}

// NewPrefixPolicyMatcher matches one prefix. An explicitly empty prefix is a
// valid native matcher; callers must intentionally construct it here.
func NewPrefixPolicyMatcher(value string) PolicyValueMatcher {
	return PolicyValueMatcher{match: PolicyMatchPrefix, value: value, valid: true}
}

// NewRegexPolicyMatcher validates and constructs a full-value regular
// expression matcher.
func NewRegexPolicyMatcher(expression string) (PolicyValueMatcher, error) {
	if _, err := compilePolicyRegex(expression); err != nil {
		return PolicyValueMatcher{}, ErrInvalidPolicyMatcher
	}
	return PolicyValueMatcher{match: PolicyMatchRegex, expression: expression, valid: true}, nil
}

// EnvironmentLookup is a controlled environment view used only while
// comparing Claude managed policy. Callers should supply the live launch
// environment for configured server values and Claude's pinned policy
// environment for policy values. The adapter never reads process environment
// variables itself and never returns values obtained through this callback.
type EnvironmentLookup func(name string) (value string, ok bool)

// ClaudePolicyList distinguishes the asymmetric expansion rules Claude uses
// for allowlists and denylists. In particular, an allowlist URL entry whose
// expansion changes its scheme, host, or path scope is ignored, while a
// denylist match remains a deny.
type ClaudePolicyList string

const (
	ClaudePolicyAllow ClaudePolicyList = "allow"
	ClaudePolicyDeny  ClaudePolicyList = "deny"
)

// ClaudePolicyExpansion supplies the two deliberately different environment
// views used by Claude policy comparison.
type ClaudePolicyExpansion struct {
	List              ClaudePolicyList
	ServerEnvironment EnvironmentLookup
	PolicyEnvironment EnvironmentLookup
}

// ArgsMatchPolicy evaluates Codex's ordered argument matcher form without
// releasing the configured argument values to the policy loader. The number
// of configured arguments must exactly equal the number of matchers.
func (r PolicyRequest) ArgsMatchPolicy(matchers []PolicyValueMatcher) (bool, error) {
	if len(r.args) != len(matchers) {
		return false, nil
	}
	for index, matcher := range matchers {
		matched, err := matchPolicyValue(r.args[index].raw(), matcher)
		if err != nil || !matched {
			return matched, err
		}
	}
	return true, nil
}

// URLMatchesPolicy evaluates Codex's exact, prefix, or full regular-expression
// URL identity without releasing the configured URL.
func (r PolicyRequest) URLMatchesPolicy(matcher PolicyValueMatcher) (bool, error) {
	if r.url == nil {
		return false, nil
	}
	return matchPolicyValue(r.url.raw(), matcher)
}

func matchPolicyValue(value string, matcher PolicyValueMatcher) (bool, error) {
	if !matcher.valid {
		return false, ErrInvalidPolicyMatcher
	}
	switch matcher.match {
	case PolicyMatchExact:
		return hmac.Equal([]byte(value), []byte(matcher.value)), nil
	case PolicyMatchPrefix:
		return strings.HasPrefix(value, matcher.value), nil
	case PolicyMatchRegex:
		expression, err := compilePolicyRegex(matcher.expression)
		if err != nil {
			return false, ErrInvalidPolicyMatcher
		}
		return expression.MatchString(value), nil
	default:
		return false, ErrInvalidPolicyMatcher
	}
}

func compilePolicyRegex(expression string) (*regexp.Regexp, error) {
	// Codex evaluates requirements with Rust regex-lite. Go's regexp differs
	// for inline flags (notably Unicode case folding) and accepts Unicode
	// property classes that regex-lite rejects. Keep the public evaluator to a
	// conservative common subset so an allow identity can never match more
	// broadly here than it does in Codex.
	if strings.Contains(expression, "(?") || strings.Contains(expression, `\p{`) || strings.Contains(expression, `\P{`) {
		return nil, ErrInvalidPolicyMatcher
	}
	return regexp.Compile(`\A(?:` + expression + `)\z`)
}

// URLMatchesClaudePattern evaluates Claude's serverUrl policy syntax. A '*'
// matches zero or more characters anywhere, including the scheme, host, port,
// path, and query. Scheme and host matching is case-insensitive, a trailing
// fully-qualified-domain dot is ignored, and paths remain case-sensitive. As
// in Claude Code, a pattern with no path matches every path on that authority.
func (r PolicyRequest) URLMatchesClaudePattern(pattern string) (bool, error) {
	if r.url == nil {
		return false, nil
	}
	if len(r.url.EnvReferences()) > 0 || len(envReferences(pattern)) > 0 {
		return false, ErrPolicyExpansion
	}
	return matchClaudeURLPattern(r.url.raw(), pattern)
}

// CommandMatchesClaude compares Claude's serverCommand array after expanding
// both sides through their respective controlled environment views. The first
// configured element is command followed by the ordered args. Missing
// variables without defaults remain as their original ${...} text, matching
// Claude Code's current behavior.
func (r PolicyRequest) CommandMatchesClaude(policyCommand []string, expansion ClaudePolicyExpansion) (bool, error) {
	if expansion.List != ClaudePolicyAllow && expansion.List != ClaudePolicyDeny {
		return false, ErrInvalidPolicyMatcher
	}
	if r.command == nil || len(policyCommand) != len(r.args)+1 {
		return false, nil
	}
	configured := make([]string, 0, len(r.args)+1)
	configured = append(configured, r.command.raw())
	for _, argument := range r.args {
		configured = append(configured, argument.raw())
	}
	for index := range configured {
		got := expandClaudePolicyValue(configured[index], expansion.ServerEnvironment)
		want := expandClaudePolicyValue(policyCommand[index], expansion.PolicyEnvironment)
		if !hmac.Equal([]byte(got), []byte(want)) {
			return false, nil
		}
	}
	return true, nil
}

// URLMatchesClaudePatternExpanded applies Claude's pre-match expansion and URL
// wildcard rules without exposing the expanded configured URL. Policy loaders
// remain responsible for constructing the pinned allow/deny environment views
// described by Claude's managed-MCP policy.
func (r PolicyRequest) URLMatchesClaudePatternExpanded(pattern string, expansion ClaudePolicyExpansion) (bool, error) {
	if expansion.List != ClaudePolicyAllow && expansion.List != ClaudePolicyDeny {
		return false, ErrInvalidPolicyMatcher
	}
	if r.url == nil {
		return false, nil
	}
	expandedPattern := expandClaudePolicyValue(pattern, expansion.PolicyEnvironment)
	if expansion.List == ClaudePolicyAllow && claudeURLScopeExpanded(pattern, expandedPattern) {
		return false, nil
	}
	expandedURL := expandClaudePolicyValue(r.url.raw(), expansion.ServerEnvironment)
	return matchClaudeURLPattern(expandedURL, expandedPattern)
}

func expandClaudePolicyValue(value string, lookup EnvironmentLookup) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		if index+2 > len(value) || value[index:index+2] != "${" {
			result.WriteByte(value[index])
			index++
			continue
		}
		end := strings.IndexByte(value[index+2:], '}')
		if end < 0 {
			result.WriteString(value[index:])
			break
		}
		end += index + 2
		body := value[index+2 : end]
		name, fallback, hasFallback := body, "", false
		if split := strings.Index(body, ":-"); split >= 0 {
			name, fallback, hasFallback = body[:split], body[split+2:], true
		}
		if !validEnvName(name) {
			result.WriteString(value[index : end+1])
			index = end + 1
			continue
		}
		if lookup != nil {
			if replacement, ok := lookup(name); ok {
				result.WriteString(replacement)
				index = end + 1
				continue
			}
		}
		if hasFallback {
			result.WriteString(fallback)
		} else {
			result.WriteString(value[index : end+1])
		}
		index = end + 1
	}
	return result.String()
}

func claudeURLScopeExpanded(before, after string) bool {
	if before == after {
		return false
	}
	// Query and fragment values do not broaden the scheme, authority, or path.
	scopeEnd := len(before)
	if index := strings.IndexAny(before, "?#"); index >= 0 {
		scopeEnd = index
	}
	return len(envReferences(before[:scopeEnd])) > 0
}

type urlAuthority struct {
	userinfo string
	host     string
	port     string
}

func matchClaudeURLPattern(value, pattern string) (bool, error) {
	if invalidPolicyURLText(value) || invalidPolicyURLText(pattern) {
		return false, ErrInvalidPolicyMatcher
	}
	actual, err := url.Parse(value)
	if err != nil || actual.Scheme == "" || actual.Host == "" {
		return false, ErrInvalidPolicyMatcher
	}
	patternScheme, patternAuthority, patternSuffix, hasSuffix, ok := splitPolicyURL(pattern)
	if !ok {
		return false, ErrInvalidPolicyMatcher
	}
	wantAuthority, ok := parseURLAuthority(patternAuthority, true)
	if !ok {
		return false, ErrInvalidPolicyMatcher
	}
	gotAuthority, ok := parseURLAuthority(actualAuthority(actual), false)
	if !ok {
		return false, ErrInvalidPolicyMatcher
	}
	if !wildcardMatch(strings.ToLower(patternScheme), strings.ToLower(actual.Scheme)) ||
		!wildcardMatch(wantAuthority.userinfo, gotAuthority.userinfo) ||
		!wildcardMatch(strings.ToLower(strings.TrimSuffix(wantAuthority.host, ".")), strings.ToLower(strings.TrimSuffix(gotAuthority.host, "."))) ||
		!wildcardMatch(wantAuthority.port, gotAuthority.port) {
		return false, nil
	}
	if !hasSuffix {
		return true, nil
	}
	return wildcardMatch(patternSuffix, actualURLSuffix(actual)), nil
}

func invalidPolicyURLText(value string) bool {
	return value == "" || hasControl(value) || strings.ContainsAny(value, " \t\r\n")
}

func splitPolicyURL(pattern string) (scheme, authority, suffix string, hasSuffix, ok bool) {
	separator := strings.Index(pattern, "://")
	if separator <= 0 {
		return "", "", "", false, false
	}
	scheme = pattern[:separator]
	rest := pattern[separator+3:]
	if rest == "" {
		return "", "", "", false, false
	}
	if index := strings.IndexAny(rest, "/?#"); index >= 0 {
		authority, suffix, hasSuffix = rest[:index], rest[index:], true
	} else {
		authority = rest
	}
	return scheme, authority, suffix, hasSuffix, authority != ""
}

func actualAuthority(value *url.URL) string {
	if value.User == nil {
		return value.Host
	}
	return value.User.String() + "@" + value.Host
}

func parseURLAuthority(value string, allowWildcard bool) (urlAuthority, bool) {
	result := urlAuthority{}
	if index := strings.LastIndexByte(value, '@'); index >= 0 {
		result.userinfo = value[:index]
		value = value[index+1:]
	}
	if value == "" {
		return urlAuthority{}, false
	}
	if strings.HasPrefix(value, "[") {
		end := strings.IndexByte(value, ']')
		if end < 0 {
			return urlAuthority{}, false
		}
		result.host = value[:end+1]
		rest := value[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return urlAuthority{}, false
			}
			result.port = rest[1:]
		}
	} else {
		if strings.Count(value, ":") > 1 {
			return urlAuthority{}, false
		}
		if index := strings.LastIndexByte(value, ':'); index >= 0 {
			result.host, result.port = value[:index], value[index+1:]
		} else {
			result.host = value
		}
	}
	if result.host == "" || result.port == "" && strings.HasSuffix(value, ":") {
		return urlAuthority{}, false
	}
	if !allowWildcard && strings.ContainsAny(result.userinfo+result.host+result.port, "*") {
		return urlAuthority{}, false
	}
	return result, true
}

func actualURLSuffix(value *url.URL) string {
	result := value.EscapedPath()
	if value.ForceQuery || value.RawQuery != "" {
		result += "?" + value.RawQuery
	}
	if value.Fragment != "" {
		result += "#" + value.EscapedFragment()
	}
	return result
}

// wildcardMatch treats only '*' specially. The linear backtracking algorithm
// is bounded by the input sizes and avoids compiling policy text as a regex.
func wildcardMatch(pattern, value string) bool {
	patternIndex, valueIndex := 0, 0
	starIndex, retryValue := -1, 0
	for valueIndex < len(value) {
		if patternIndex < len(pattern) && pattern[patternIndex] == value[valueIndex] {
			patternIndex++
			valueIndex++
			continue
		}
		if patternIndex < len(pattern) && pattern[patternIndex] == '*' {
			starIndex = patternIndex
			patternIndex++
			retryValue = valueIndex
			continue
		}
		if starIndex >= 0 {
			patternIndex = starIndex + 1
			retryValue++
			valueIndex = retryValue
			continue
		}
		return false
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}
