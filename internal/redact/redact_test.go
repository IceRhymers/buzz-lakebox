package redact

import (
	"fmt"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

func TestRedact_Basic(t *testing.T) {
	got := Redact("token is sk-marker-abcdef and stays elsewhere", []string{"sk-marker-abcdef"})
	want := "token is [REDACTED] and stays elsewhere"
	if got != want {
		t.Fatalf("Redact = %q, want %q", got, want)
	}
}

func TestRedact_LongestFirst(t *testing.T) {
	// "abc" is a substring of "abcdef"; redacting must consume the full
	// longer secret rather than leaving "def" behind as a fragment.
	got := Redact("value=abcdef", []string{"abc", "abcdef"})
	if strings.Contains(got, "def") {
		t.Fatalf("Redact left a fragment of the longer secret: %q", got)
	}
	if got != "value=[REDACTED]" {
		t.Fatalf("Redact = %q, want value=[REDACTED]", got)
	}
}

func TestRedact_ShortSecretsIgnored(t *testing.T) {
	// Secrets shorter than 4 chars are not scrubbed (too likely to be
	// incidental substrings of ordinary text).
	got := Redact("the cat sat", []string{"cat"})
	if got != "the cat sat" {
		t.Fatalf("Redact should not touch <4-char secrets, got %q", got)
	}
}

func TestRedact_NsecPrefixScrub(t *testing.T) {
	got := Redact(`saw token nsec1abcdefghijklmnopqrstuvwxyz0123456789 in the log`, nil)
	if strings.Contains(got, "nsec1abc") {
		t.Fatalf("nsec1 token survived redaction: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("expected placeholder in output: %q", got)
	}
}

func TestRedact_NsecPrefixScrub_StopsAtWhitespaceOrQuote(t *testing.T) {
	got := Redact(`"nsec1abcdefgh" and nsec1zyxwvu, done`, nil)
	if strings.Contains(got, "nsec1") {
		t.Fatalf("nsec1 fragments survived redaction: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("expected surrounding text preserved: %q", got)
	}
}

func TestRedact_EmptyInput(t *testing.T) {
	if got := Redact("", []string{"whatever"}); got != "" {
		t.Fatalf("Redact(\"\") = %q, want empty", got)
	}
}

// TestSecretsFromPayload_MarkerNeverSurvives plants a unique marker secret
// in every agent payload field that can carry one, renders an error string
// that concatenates every field, and asserts the marker never survives
// SecretsFromPayload + Redact — the property PLAN.md §7 calls for.
func TestSecretsFromPayload_MarkerNeverSurvives(t *testing.T) {
	markers := map[string]string{
		"nsec":      "nsec1MARKERPRIVATEKEY0000000000000000000000000000000000000",
		"auth_tag":  "MARKER-AUTH-TAG-VALUE-abcdefgh",
		"env_one":   "MARKER-ENV-VALUE-ONE-abcdefgh",
		"env_two":   "MARKER-ENV-VALUE-TWO-abcdefgh",
		"env_token": "MARKER-DATABRICKS-TOKEN-VALUE-abcdefgh",
	}

	agent := payload.Agent{
		Name:           "agent name unaffected",
		RelayURL:       "wss://relay.example.com",
		PrivateKeyNsec: markers["nsec"],
		AuthTag:        markers["auth_tag"],
		AgentCommand:   "buzz-agent",
		EnvVars: map[string]string{
			"FOO":              markers["env_one"],
			"BAR":              markers["env_two"],
			"DATABRICKS_TOKEN": markers["env_token"],
		},
	}

	// Simulate an error/log line that carelessly interpolates every field.
	rendered := fmt.Sprintf(
		"deploy failed for %s relay=%s nsec=%s auth=%s env=%v",
		agent.Name, agent.RelayURL, agent.PrivateKeyNsec, agent.AuthTag, agent.EnvVars,
	)

	secrets := SecretsFromPayload(agent)
	scrubbed := Redact(rendered, secrets)

	for field, marker := range markers {
		if strings.Contains(scrubbed, marker) {
			t.Fatalf("marker secret for %q survived redaction: %q", field, scrubbed)
		}
	}
	// Non-secret fields must still be present — redaction shouldn't be a
	// blunt instrument.
	if !strings.Contains(scrubbed, "agent name unaffected") {
		t.Fatalf("non-secret field was unexpectedly redacted: %q", scrubbed)
	}
	if !strings.Contains(scrubbed, "wss://relay.example.com") {
		t.Fatalf("non-secret relay_url was unexpectedly redacted: %q", scrubbed)
	}
}

func TestSecretsFromPayload_EmptyFieldsSkipped(t *testing.T) {
	agent := payload.Agent{}
	secrets := SecretsFromPayload(agent)
	if len(secrets) != 0 {
		t.Fatalf("expected no secrets from an empty agent, got %v", secrets)
	}
}

func TestLog_ScrubsCredentialShapedAssignments(t *testing.T) {
	in := strings.Join([]string{
		"export DATABRICKS_TOKEN=dapi123456789abcdef",
		"BUZZ_AUTH_TAG: tag-value-here",
		`OPENAI_API_KEY="sk-proj-abcdefghijklmnop"`,
		"password=hunter2",
		"BUZZ_PRIVATE_KEY=nsec1qqqqqqqqqqqqqqqqqqqq",
	}, "\n")

	got := Log(in)
	for _, leaked := range []string{"dapi123456789abcdef", "tag-value-here", "sk-proj-abcdefghijklmnop", "hunter2", "nsec1qqqqqqqqqqqqqqqqqqqq"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Log() left %q in the output: %s", leaked, got)
		}
	}
	// The variable NAMES must survive — an operator debugging a missing
	// env var needs to see which one was set.
	for _, keep := range []string{"DATABRICKS_TOKEN", "BUZZ_AUTH_TAG", "OPENAI_API_KEY", "BUZZ_PRIVATE_KEY"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("Log() removed the variable name %q: %s", keep, got)
		}
	}
}

// buzz-acp's startup line carries the agent's PUBLIC key and the relay
// URL — the two most useful diagnostics in the log. Neither is a secret
// and both must survive scrubbing.
func TestLog_KeepsDiagnosticNonSecrets(t *testing.T) {
	in := "buzz-acp starting: relay=wss://relay.example.com pubkey=abc123def456\nagent_pool_ready agents=2\n"
	if got := Log(in); got != in {
		t.Fatalf("Log() must not touch diagnostic output:\n got: %q\nwant: %q", got, in)
	}
}

func TestLog_ScrubsBareNsecWithoutAnAssignment(t *testing.T) {
	got := Log("panic: key nsec1abcdefghijklmnop was rejected")
	if strings.Contains(got, "nsec1abcdefghijklmnop") {
		t.Fatalf("Log() left a bare nsec in the output: %s", got)
	}
}

func TestLog_EmptyInput(t *testing.T) {
	if got := Log(""); got != "" {
		t.Fatalf("Log(\"\") = %q", got)
	}
}
