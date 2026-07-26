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
