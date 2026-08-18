package main

import (
	"strings"
	"testing"
)

func TestSanitizedCommandWithholdsCredentialsFromEditors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "provider-secret")
	t.Setenv("SESSION_ID", "generic-secret")
	t.Setenv("DATABASE_URL", "postgres://database-secret")
	t.Setenv("SB_EDITOR_VISIBLE", "ordinary-value")

	command := sanitizedCommand("fixture-editor", "file.go")
	environment := strings.Join(command.Env, "\n")
	for _, secret := range []string{"provider-secret", "generic-secret", "database-secret"} {
		if strings.Contains(environment, secret) {
			t.Errorf("editor command environment retained %q", secret)
		}
	}
	if !strings.Contains(environment, "SB_EDITOR_VISIBLE=ordinary-value") {
		t.Errorf("editor command dropped ordinary environment: %q", environment)
	}
}
