// Package childenv builds the ambient environment for untrusted child
// processes. It preserves ordinary host configuration while withholding
// provider and generic credential-bearing variables.
package childenv

import (
	"os"
	"strings"
)

// Current returns the current process environment without credential-bearing
// entries. This is hygiene, not containment: a child may still read secrets
// from files or credential services unless a separate boundary prevents it.
func Current() []string {
	return Filter(os.Environ())
}

// Filter removes credential-bearing and malformed entries from environ while
// preserving the order of ordinary variables.
func Filter(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || Sensitive(name) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// Sensitive reports whether an environment name conventionally carries a
// credential or a credential-bearing service URL. Matching is deliberately
// case-insensitive on every platform because mixed-case names should not turn
// into an escape hatch on a case-sensitive host.
func Sensitive(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, token := range tokens {
		switch token {
		case "auth", "authorization", "authentication",
			"cookie", "cookies", "credential", "credentials",
			"dsn", "key", "keys", "passwd", "password", "passwords", "pwd",
			"secret", "secrets", "session", "sessions", "token", "tokens":
			return true
		}
	}

	compact := strings.Join(tokens, "")
	switch compact {
	case "apikey", "xapikey", "accesstoken", "authtoken", "bearertoken",
		"clientsecret", "privatekey", "secretkey", "sessionid", "sessionkey",
		"signingkey", "encryptionkey", "refreshtoken", "sshauthsock", "sshagentpid",
		"openaiorgid":
		return true
	}

	has := func(candidates ...string) bool {
		for _, token := range tokens {
			for _, candidate := range candidates {
				if token == candidate {
					return true
				}
			}
		}
		return false
	}
	if has("url", "uri") && has("db", "database", "postgres", "postgresql", "mysql", "mariadb", "mongo", "mongodb", "redis", "amqp", "rabbitmq") {
		return true
	}
	return has("connection") && has("string")
}
