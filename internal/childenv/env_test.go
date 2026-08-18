package childenv

import (
	"slices"
	"testing"
)

func TestSensitiveCoversProviderAndGenericCredentialNames(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "openai_api_key", "OPENAI_ORG_ID", "SB_KIMI_WORK_API_KEY",
		"AUTH", "SESSION_ID", "COOKIE", "ClientSecret", "SSH_AUTH_SOCK",
		"DATABASE_URL", "SERVICE_DSN", "REDIS_URL", "CONNECTION_STRING",
	} {
		if !Sensitive(name) {
			t.Errorf("Sensitive(%q) = false", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "TERM", "SAFE_VISIBLE", "X_MONKEY", "HOCKEY_TEAM"} {
		if Sensitive(name) {
			t.Errorf("Sensitive(%q) = true", name)
		}
	}
}

func TestFilterPreservesSafeOrderAndDropsMixedCaseSecrets(t *testing.T) {
	input := []string{
		"PATH=/bin", "github_token=secret", "SAFE_VISIBLE=yes", "Db_PaSsWoRd=secret",
		"TERM=xterm", "DATABASE_URL=postgres://secret", "malformed",
	}
	want := []string{"PATH=/bin", "SAFE_VISIBLE=yes", "TERM=xterm"}
	if got := Filter(input); !slices.Equal(got, want) {
		t.Fatalf("Filter() = %q, want %q", got, want)
	}
}
