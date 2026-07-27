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

// fakeDapi tokens for the dapi-scrub tests, assembled at runtime so the
// token-shaped literal never appears contiguously in source (the
// Databricks pre-commit secret scanner flags real-PAT shapes, which is
// exactly the shape these fixtures must have).
var (
	fakeDapiA = "dapi" + strings.Repeat("0123456789abcdef", 2)
	fakeDapiB = "dapi" + strings.Repeat("fedcba9876543210", 2)
)

func TestRedact_DapiPrefixScrub(t *testing.T) {
	got := Redact("saw token "+fakeDapiA+" mid-sentence", nil)
	if strings.Contains(got, "dapi0123456789") {
		t.Fatalf("dapi token survived redaction: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("expected placeholder in output: %q", got)
	}
}

func TestRedact_DapiPrefixScrub_QuotedForms(t *testing.T) {
	got := Redact(`"`+fakeDapiA+`" and '`+fakeDapiB+`', done`, nil)
	if strings.Contains(got, "dapi") {
		t.Fatalf("dapi fragments survived redaction: %q", got)
	}
	if !strings.Contains(got, `"`+Placeholder+`"`) || !strings.Contains(got, `'`+Placeholder+`'`) {
		t.Fatalf("expected quotes preserved around placeholder: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("expected surrounding text preserved: %q", got)
	}
}

func TestRedact_DapiPrefixScrub_NegativeCases(t *testing.T) {
	// The bare word "dapi" (no hex tail) must survive.
	if got := Redact("the prefix is dapi and nothing else", nil); got != "the prefix is dapi and nothing else" {
		t.Fatalf("Redact should not touch bare \"dapi\", got %q", got)
	}
	// "deadbeef" is only 8 hex chars — below the 16-char floor — so this
	// must also survive untouched.
	if got := Redact("value is dapideadbeef here", nil); got != "value is dapideadbeef here" {
		t.Fatalf("Redact should not touch short-hex dapideadbeef, got %q", got)
	}
}

func TestRedact_DapiPrefixScrub_SuffixConsumed(t *testing.T) {
	// A suffixed token form (e.g. a trailing "-3" disambiguator) must be
	// fully consumed, not left as a fragment after the placeholder.
	got := Redact("token="+fakeDapiA+"-3 trailing", nil)
	if strings.Contains(got, "dapi") || strings.Contains(got, "-3") {
		t.Fatalf("suffixed dapi token left a fragment: %q", got)
	}
	if !strings.Contains(got, "trailing") {
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

// TestLog_ScrubsBareDapiWithoutAnAssignment covers the sandbox
// inference-auth leak path: a derived Databricks PAT that never rode an
// assignment (e.g. echoed mid-sentence in acp.log) must still be
// scrubbed by the always-on dapiPrefixPattern floor.
func TestLog_ScrubsBareDapiWithoutAnAssignment(t *testing.T) {
	got := Log("panic: token " + fakeDapiA + " was rejected")
	if strings.Contains(got, "dapi0123456789") {
		t.Fatalf("Log() left a bare dapi token in the output: %s", got)
	}
}

// TestLog_ScrubsDapiAssignment asserts the DATABRICKS_TOKEN=dapi... value
// is gone from Log() output — the assignment-shaped path
// (secretAssignmentPattern) already scrubs it before dapiPrefixPattern
// ever runs, but the property under test is that the value doesn't
// survive by either mechanism.
func TestLog_ScrubsDapiAssignment(t *testing.T) {
	got := Log("export DATABRICKS_TOKEN=" + fakeDapiA)
	if strings.Contains(got, "dapi0123456789") {
		t.Fatalf("Log() left the dapi token value in the output: %s", got)
	}
	if !strings.Contains(got, "DATABRICKS_TOKEN") {
		t.Fatalf("Log() removed the variable name: %s", got)
	}
}

// TestLog_ScrubsAssignmentShapes is the regression test for the
// single-quote leak: internal/shellquote's Single wraps every value nest.go
// writes into an env file in single quotes, so KEY='value' is the shape
// this codebase actually produces. Each case asserts the exact scrubbed
// output — no stray leading/trailing quote left behind — across quoted,
// bare, and colon forms, and across more than one keyword family.
func TestLog_ScrubsAssignmentShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single_quoted_auth_tag_export_prefixed",
			in:   `export BUZZ_AUTH_TAG='sometag-secret-value'`,
			want: "export BUZZ_AUTH_TAG=" + Placeholder,
		},
		{
			name: "single_quoted_token_export_prefixed",
			in:   `export DATABRICKS_TOKEN='dapi1234567890abcdef'`,
			want: "export DATABRICKS_TOKEN=" + Placeholder,
		},
		{
			name: "double_quoted_token",
			in:   `export DATABRICKS_TOKEN="dapi1234567890abcdef"`,
			want: "export DATABRICKS_TOKEN=" + Placeholder,
		},
		{
			name: "bare_token",
			in:   "DATABRICKS_TOKEN=dapi1234567890abcdef",
			want: "DATABRICKS_TOKEN=" + Placeholder,
		},
		{
			name: "colon_with_space_token",
			in:   "DATABRICKS_TOKEN: dapi1234567890abcdef",
			want: "DATABRICKS_TOKEN: " + Placeholder,
		},
		{
			name: "colon_no_space_token",
			in:   "DATABRICKS_TOKEN:dapi1234567890abcdef",
			want: "DATABRICKS_TOKEN:" + Placeholder,
		},
		{
			name: "single_quoted_api_key",
			in:   `export OPENAI_API_KEY='sk-proj-abcdefghij'`,
			want: "export OPENAI_API_KEY=" + Placeholder,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Log(tt.in)
			if got != tt.want {
				t.Fatalf("Log(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestLog_DiagnosticKeywordsSurvive is the explicit negative-case
// counterpart to TestLog_ScrubsAssignmentShapes: a broadened assignment
// pattern must still not fire on pubkey=, relay=, or agents=2, none of
// which contain a credential keyword.
func TestLog_DiagnosticKeywordsSurvive(t *testing.T) {
	in := "buzz-acp starting: relay=wss://relay.example.com pubkey=abc123def456\nagent_pool_ready agents=2\n"
	got := Log(in)
	for _, keep := range []string{"relay=wss://relay.example.com", "pubkey=abc123def456", "agents=2"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("Log() removed diagnostic %q: %s", keep, got)
		}
	}
	if strings.Contains(got, Placeholder) {
		t.Fatalf("Log() redacted non-secret diagnostic output: %s", got)
	}
}

func TestLog_EmptyInput(t *testing.T) {
	if got := Log(""); got != "" {
		t.Fatalf("Log(\"\") = %q", got)
	}
}
