package credential

import (
	"fmt"
	"strings"
	"testing"
)

func TestScanPromptFindsKnownTokenShapes(t *testing.T) {
	// Every fixture token is split after its prefix so no contiguous
	// key-shaped literal exists in this file: repository secret scanners
	// read source, and a pattern-valid dummy raises the same alarm a real
	// key would. The runtime strings are unchanged. Do not rejoin them.
	cases := []struct {
		text string
		kind string
	}{
		{"here is sk-ant-api03-" + "abcdefghijklmnopqrstuvwx my key", "an Anthropic API key"},
		{"OPENAI_API_KEY=sk-proj-" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", "an OpenAI API key"},
		{"token: ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "a GitHub token"},
		{"github_pat_" + "11ABCDEFG0abcdefghijklm", "a GitHub fine-grained token"},
		{"glpat-" + "abcdefghij0123456789", "a GitLab token"},
		{"xoxb-" + "1234567890-abcdef", "a Slack token"},
		{"aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "an AWS access key ID"},
		{"key=AIza" + "SyA-abcdefghijklmnopqrstuvwxyz01234", "a Google API key"},
		{"sk_live_" + "abcdefghij0123456789", "a Stripe live key"},
		{"npm_" + "abcdefghijklmnopqrstuvwxyz0123456789", "an npm token"},
		{"hf_" + "abcdefghijklmnopqrstuvwxyz01234", "a Hugging Face token"},
		{"-----BEGIN RSA PRIVATE KEY-----", "a private key block"},
	}
	for _, c := range cases {
		leaks := ScanPrompt(c.text)
		if len(leaks) == 0 {
			t.Errorf("scan missed %s in %q", c.kind, c.text)
			continue
		}
		if leaks[0].Kind != c.kind {
			t.Errorf("scan of %q called it %q, want %q", c.text, leaks[0].Kind, c.kind)
		}
	}
}

// The floors are the precision: a prefix mentioned in prose, without the
// token attached, is conversation about keys, not a key.
func TestScanPromptLeavesProseAlone(t *testing.T) {
	for _, text := range []string{
		"set sk-ant-... in the environment",
		"a GitHub token starts with ghp_",
		"the AKIA prefix marks an access key id",
		"rotate your sk_live key in the dashboard",
		"review this diff for anything key-shaped",
	} {
		if leaks := ScanPrompt(text); len(leaks) != 0 {
			t.Errorf("scan flagged prose %q as %v", text, leaks)
		}
	}
}

func TestScanPromptDeduplicatesRepeatedPastes(t *testing.T) {
	key := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	if leaks := ScanPrompt(key + " and again " + key); len(leaks) != 1 {
		t.Errorf("one key pasted twice reported %d findings", len(leaks))
	}
}

// The Secret rule applies to findings too: no rendering shows the match.
func TestLeakHasNoRenderingThatShowsTheSecret(t *testing.T) {
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	leaks := ScanPrompt("token " + secret)
	if len(leaks) != 1 {
		t.Fatalf("expected one finding, got %v", leaks)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", leaks[0]),
		fmt.Sprintf("%+v", leaks[0]),
		fmt.Sprintf("%#v", leaks[0]),
		fmt.Sprintf("%s", leaks[0]),
		leaks[0].Masked(),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("a rendering shows the secret: %q", rendered)
		}
	}
}

// The property the gate promises: after redact, no key material remains
// outbound. For a PEM that means the body and END line go with the header,
// and a block whose END was lost in the paste is stripped to the end of
// the text, because a truncated key is still a key.
func TestRedactStripsAWholePrivateKeyBlock(t *testing.T) {
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----"
	for name, text := range map[string]string{
		"complete":  "my key is\n" + pem + "\nplease review",
		"truncated": "my key is\n-----BEGIN RSA PRIVATE KEY-----\n" + body,
	} {
		out := Redact(text, ScanPrompt(text))
		if strings.Contains(out, body) {
			t.Errorf("%s: redaction left the key body outbound: %q", name, out)
		}
		if strings.Contains(out, "-----END") {
			t.Errorf("%s: redaction left the block's tail: %q", name, out)
		}
		if !strings.Contains(out, "[redacted: a private key block]") {
			t.Errorf("%s: redaction does not say what stood there: %q", name, out)
		}
	}
	if !strings.Contains(Redact("my key is\n"+pem+"\nplease review", ScanPrompt(pem)), "please review") {
		t.Error("redaction took the prose after the block with it")
	}
}

func TestRedactReplacesTheMatchAndNamesTheKind(t *testing.T) {
	secret := "sk-ant-api03-abcdefghijklmnopqrstuvwx"
	text := "use " + secret + " for auth"
	leaks := ScanPrompt(text)
	out := Redact(text, leaks)
	if strings.Contains(out, secret) {
		t.Errorf("redaction left the secret in place: %q", out)
	}
	if !strings.Contains(out, "[redacted: an Anthropic API key]") {
		t.Errorf("redaction does not say what stood there: %q", out)
	}
}
