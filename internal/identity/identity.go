// Package identity derives an agent's Nostr npub (public identity) from its
// nsec (private key) and computes the deterministic Databricks Sandbox name
// keyed on that npub (PLAN.md §4.1).
package identity

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

const (
	nsecHRP = "nsec"
	npubHRP = "npub"

	// npubDataPrefix is the bech32 human-readable-part + separator that
	// precedes the data portion of every npub string.
	npubDataPrefix = npubHRP + "1"

	// npub12Len is the number of characters of the npub data part used as
	// the sandbox-naming identity key (PLAN.md §4.1).
	npub12Len = 12

	// sandboxSlugMaxLen bounds the cosmetic display-name slug in a sandbox
	// name (PLAN.md §4.1).
	sandboxSlugMaxLen = 20

	// sandboxPrefix is the static prefix every sandbox name derived here
	// carries, ahead of the npub12 identity key.
	sandboxPrefix = "buzz-"
)

// nonSlugChars matches any run of characters that may not appear in a
// sandbox-name slug; each run collapses to a single "-".
var nonSlugChars = regexp.MustCompile(`[^a-z0-9-]+`)

// multiDash collapses repeated "-" (both from the substitution above and
// from dashes already present in the input) into one.
var multiDash = regexp.MustCompile(`-+`)

// NsecToNpub decodes a bech32 "nsec1..." private key, derives the
// secp256k1 x-only public key (BIP-340 / NIP-01 convention: the X
// coordinate of privKey*G, sign-independent), and re-encodes it as a
// bech32 "npub1..." string.
func NsecToNpub(nsec string) (string, error) {
	hrp, data, err := bech32.Decode(nsec)
	if err != nil {
		return "", fmt.Errorf("decode nsec: %w", err)
	}
	if hrp != nsecHRP {
		return "", fmt.Errorf("unexpected bech32 prefix %q; want %q", hrp, nsecHRP)
	}

	keyBytes, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return "", fmt.Errorf("convert nsec payload: %w", err)
	}
	if len(keyBytes) != 32 {
		return "", fmt.Errorf("nsec payload is %d bytes; want 32", len(keyBytes))
	}

	privKey, pubKey := btcec.PrivKeyFromBytes(keyBytes)
	defer privKey.Zero()

	// BIP-340/NIP-01 x-only public key: the 32-byte X coordinate. A
	// compressed SEC1 pubkey is a 1-byte parity prefix followed by X;
	// dropping the prefix yields the x-only form regardless of parity.
	compressed := pubKey.SerializeCompressed()
	xOnly := compressed[1:]

	converted, err := bech32.ConvertBits(xOnly, 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("convert npub payload: %w", err)
	}
	npub, err := bech32.Encode(npubHRP, converted)
	if err != nil {
		return "", fmt.Errorf("encode npub: %w", err)
	}
	return npub, nil
}

// Npub12 returns the first 12 characters of an npub's bech32 data part
// (everything after the "npub1" prefix) — the identity key used in sandbox
// names (PLAN.md §4.1).
func Npub12(npub string) (string, error) {
	if !strings.HasPrefix(npub, npubDataPrefix) {
		return "", fmt.Errorf("npub %q does not start with %q", npub, npubDataPrefix)
	}
	data := npub[len(npubDataPrefix):]
	if len(data) < npub12Len {
		return "", fmt.Errorf("npub %q data part shorter than %d chars", npub, npub12Len)
	}
	return data[:npub12Len], nil
}

// Slug sanitizes a cosmetic display name into the slug portion of a
// sandbox name: lowercased, any run of characters outside [a-z0-9-]
// collapsed to a single "-", repeated "-" collapsed, leading/trailing "-"
// trimmed, and truncated to sandboxSlugMaxLen characters (re-trimmed if
// truncation lands on a trailing "-"). May be empty for a display name with
// no ASCII alphanumeric content.
func Slug(displayName string) string {
	s := strings.ToLower(displayName)
	s = nonSlugChars.ReplaceAllString(s, "-")
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > sandboxSlugMaxLen {
		s = s[:sandboxSlugMaxLen]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// SandboxName computes the deterministic sandbox name for an agent:
// "buzz-<npub12>-<slug>" (PLAN.md §4.1). npub must be a valid "npub1..."
// string (e.g. from NsecToNpub); displayName is cosmetic only.
func SandboxName(npub, displayName string) (string, error) {
	npub12, err := Npub12(npub)
	if err != nil {
		return "", err
	}
	return sandboxPrefix + npub12 + "-" + Slug(displayName), nil
}

// PrefixFor returns the stable "buzz-<npub12>-" prefix used to find all
// sandboxes belonging to this npub via `sandbox list` (PLAN.md §4.1).
func PrefixFor(npub string) (string, error) {
	npub12, err := Npub12(npub)
	if err != nil {
		return "", err
	}
	return sandboxPrefix + npub12 + "-", nil
}
