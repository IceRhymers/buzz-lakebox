// Package redact scrubs secrets out of strings before they are logged or
// returned in error messages. It must be applied on every code path that
// renders payload-derived text (PLAN.md §5).
package redact

import (
	"regexp"
	"sort"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// Placeholder is substituted for every redacted secret occurrence.
const Placeholder = "[REDACTED]"

// minSecretLen is the shortest secret value we bother scrubbing; shorter
// strings are too likely to appear incidentally (and too short to be a real
// credential) and would over-redact ordinary text.
const minSecretLen = 4

// nsecPrefixPattern scrubs any "nsec1..." token from its prefix up to the
// next whitespace or quote character, even if the exact value wasn't in the
// known-secrets list passed to Redact (defense in depth for partial/derived
// nsec strings that leak into logs).
var nsecPrefixPattern = regexp.MustCompile(`nsec1[^\s"']*`)

// dapiPrefixPattern scrubs a bare Databricks PAT ("dapi" + hex) from its
// prefix up to the next whitespace or quote character, mirroring
// nsecPrefixPattern. It exists for the sandbox inference-auth mode: the
// token there is derived in-sandbox from the baked ~/.databrickscfg
// (provider_config.inference_auth: "sandbox") and never rides the
// payload, so payload-keyed Redact() has no known-secret value to catch
// it with — this is the only defense for a bare dapi token that leaks
// into acp.log or other remote output. The >=16 hex-char requirement
// after the prefix avoids scrubbing ordinary prose that merely contains
// the word "dapi"; consuming to the next whitespace/quote catches
// suffixed token forms the same way nsecPrefixPattern does.
var dapiPrefixPattern = regexp.MustCompile(`(?i)\bdapi[0-9a-f]{16,}[^\s"']*`)

// antPrefixPattern scrubs a bare Anthropic API key ("sk-ant-" + token)
// from its prefix up to the next whitespace or quote character, mirroring
// dapiPrefixPattern and for the same structural reason.
//
// It exists for the Claude runtime. secretAssignmentPattern already covers
// the `ANTHROPIC_API_KEY=…` / `ANTHROPIC_AUTH_TOKEN=…` assignment shapes,
// but not a bare key appearing in prose — an HTTP debug line, an
// `x-api-key: sk-ant-…` header dump, a stack trace. That reaches an
// operator's terminal for real: buzz-acp spawns the agent with inherited
// stderr and launch.sh folds buzz-acp's own stderr into acp.log, which the
// operator `logs`/`status` subcommands tail. Those paths have no payload to
// build a known-secret list from, so redact.Log is their only scrub.
var antPrefixPattern = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`)

// secretAssignmentPattern scrubs credential-shaped `NAME=value` /
// `NAME: value` pairs out of remote output. It exists for the paths
// where there is no payload to derive known secrets from — the operator
// lifecycle commands (status/logs/start) render an in-sandbox acp.log
// tail, and buzz-acp's own output or a crashing agent's traceback can
// echo the env file's exports. Deliberately narrow: bare "KEY" is NOT a
// trigger, so buzz-acp's diagnostic `pubkey=…` startup line survives.
//
// The value alternation handles single-quoted, double-quoted, and bare
// forms symmetrically: whichever quote (if any) opens the value, the
// matching close must be present, and the whole token — quotes included —
// is consumed by the match. Go's regexp is RE2, which has no
// backreferences, so the two quote styles are spelled out as separate
// alternatives rather than matched with a `("?)...\3`-style capture; the
// replacement never re-emits a quote, so the output is uniformly
// `NAME=[REDACTED]` regardless of how the original value was quoted —
// no stray unbalanced quote survives.
var secretAssignmentPattern = regexp.MustCompile(
	`(?i)\b([A-Za-z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|PRIVATE_KEY|CREDENTIALS?|AUTH_TAG)[A-Za-z0-9_]*)(\s*[=:]\s*)(?:'[^']*'|"[^"]*"|[^\s"']+)`)

// Log scrubs remote output (an acp.log tail, a command's stdout) that is
// about to be rendered into an error, a status payload, or the
// operator's terminal. Unlike Redact it needs no known-secret list: it
// removes any bare nsec token and any credential-shaped assignment.
// Callers that DO have the payload should still call Redact as well —
// Log is the floor, not a replacement.
func Log(s string) string {
	if s == "" {
		return s
	}
	s = secretAssignmentPattern.ReplaceAllString(s, "${1}${2}"+Placeholder)
	s = nsecPrefixPattern.ReplaceAllString(s, Placeholder)
	s = antPrefixPattern.ReplaceAllString(s, Placeholder)
	return dapiPrefixPattern.ReplaceAllString(s, Placeholder)
}

// Redact replaces every occurrence of each secret in secrets (deduplicated,
// filtered to length >= minSecretLen, longest-first so that longer secrets
// win over any shorter secret that happens to be a substring of it) with
// Placeholder in a single pass over s. It additionally scrubs any bare
// "nsec1..." token regardless of whether it was supplied in secrets.
func Redact(s string, secrets []string) string {
	if s == "" {
		return s
	}

	cleaned := prepareSecrets(secrets)
	if len(cleaned) > 0 {
		pairs := make([]string, 0, len(cleaned)*2)
		for _, secret := range cleaned {
			pairs = append(pairs, secret, Placeholder)
		}
		s = strings.NewReplacer(pairs...).Replace(s)
	}

	s = nsecPrefixPattern.ReplaceAllString(s, Placeholder)
	// Same defense-in-depth role the neighboring prefix patterns play, for
	// the Claude runtime's credential. Redact is the OUTER scrub on the
	// deploy path, and not every error string reaching it has been through
	// redact.Log first — so omitting it here would leave a bare sk-ant-
	// token dependent on a single upstream call site.
	s = antPrefixPattern.ReplaceAllString(s, Placeholder)
	s = dapiPrefixPattern.ReplaceAllString(s, Placeholder)

	return s
}

// SecretsFromPayload collects every string in an agent payload that must
// never survive into a rendered log or error message: the nsec private
// key, the auth tag, and every env_vars value (this indiscriminately
// includes anything that looks like a credential, e.g. a DATABRICKS_TOKEN
// entry added to the env for AI Gateway inference — PLAN.md §4.4 step 7 —
// without needing to special-case the key name).
func SecretsFromPayload(agent payload.Agent) []string {
	secrets := make([]string, 0, 2+len(agent.EnvVars))
	if agent.PrivateKeyNsec != "" {
		secrets = append(secrets, agent.PrivateKeyNsec)
	}
	if agent.AuthTag != "" {
		secrets = append(secrets, agent.AuthTag)
	}
	for _, v := range agent.EnvVars {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// prepareSecrets dedupes, drops short/empty values, and sorts the remaining
// secrets longest-first so that strings.Replacer (which resolves ties at the
// same start position by argument order) prefers the longest match.
func prepareSecrets(secrets []string) []string {
	seen := make(map[string]struct{}, len(secrets))
	out := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if len(secret) < minSecretLen {
			continue
		}
		if _, ok := seen[secret]; ok {
			continue
		}
		seen[secret] = struct{}{}
		out = append(out, secret)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}
