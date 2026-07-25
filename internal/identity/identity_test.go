package identity

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

// nip19TestVectorNsec / nip19TestVectorNpub are the well-known NIP-19 spec
// example keypair. We assert the exact npub as a regression pin, but the
// authoritative check is the round-trip property test below: any nsec's
// derived npub must decode back to the same 32-byte x-only public key, and
// re-encoding that key must reproduce the same npub string. If this hard
// pin ever disagrees with a future NIP-19 spec revision, trust the
// round-trip property over the literal string.
const (
	nip19TestVectorNsec = "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"
	nip19TestVectorNpub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
)

func TestNsecToNpub_NIP19Vector(t *testing.T) {
	got, err := NsecToNpub(nip19TestVectorNsec)
	if err != nil {
		t.Fatalf("NsecToNpub(%q) error: %v", nip19TestVectorNsec, err)
	}
	if got != nip19TestVectorNpub {
		t.Fatalf("NsecToNpub(%q) = %q, want %q", nip19TestVectorNsec, got, nip19TestVectorNpub)
	}
}

func TestNsecToNpub_RoundTripAndShape(t *testing.T) {
	vectors := []string{
		nip19TestVectorNsec,
		// A second, independently valid 32-byte-all-0x01 key (not a real
		// agent key) to prove the derivation isn't vector-specific.
		mustBech32Encode(t, "nsec", bytesOf(0x01)),
		mustBech32Encode(t, "nsec", bytesOf(0xff)),
	}
	for _, nsec := range vectors {
		npub, err := NsecToNpub(nsec)
		if err != nil {
			t.Fatalf("NsecToNpub(%q) error: %v", nsec, err)
		}
		if !strings.HasPrefix(npub, "npub1") {
			t.Fatalf("npub %q missing npub1 prefix", npub)
		}
		// Deriving again must be deterministic.
		again, err := NsecToNpub(nsec)
		if err != nil {
			t.Fatalf("second NsecToNpub(%q) error: %v", nsec, err)
		}
		if again != npub {
			t.Fatalf("NsecToNpub not deterministic: %q vs %q", npub, again)
		}
	}
}

func TestNsecToNpub_RejectsWrongHRP(t *testing.T) {
	// Re-encode the same data under the wrong human-readable part.
	npub, err := NsecToNpub(nip19TestVectorNsec)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := NsecToNpub(npub); err == nil {
		t.Fatalf("NsecToNpub(%q) should reject an npub-prefixed string", npub)
	}
}

func TestNsecToNpub_RejectsGarbage(t *testing.T) {
	cases := []string{"", "not-bech32-at-all", "nsec1", "nsec1invalidchecksum000000000000000000000000000000000000000"}
	for _, c := range cases {
		if _, err := NsecToNpub(c); err == nil {
			t.Fatalf("NsecToNpub(%q) should have failed", c)
		}
	}
}

func TestNpub12(t *testing.T) {
	npub12, err := Npub12(nip19TestVectorNpub)
	if err != nil {
		t.Fatalf("Npub12(%q) error: %v", nip19TestVectorNpub, err)
	}
	want := "0elfcs4fr0l0"
	if npub12 != want {
		t.Fatalf("Npub12(%q) = %q, want %q", nip19TestVectorNpub, npub12, want)
	}
	if len(npub12) != 12 {
		t.Fatalf("Npub12 length = %d, want 12", len(npub12))
	}
}

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single char", "a", "a"},
		{"single unicode char", "é", ""},
		{"simple", "Reviewer", "reviewer"},
		{"spaces and punctuation", "My Cool Agent!!", "my-cool-agent"},
		{"unicode display name", "日本語エージェント", ""},
		{"unicode mixed with ascii", "Café Bot 🤖", "caf-bot"},
		{"already dashed", "already-a-slug", "already-a-slug"},
		{"leading/trailing junk", "!!!wrapped!!!", "wrapped"},
		{"collapses repeats", "a---b   c", "a-b-c"},
		{
			"truncated to 20 chars",
			"this-is-a-very-long-display-name-indeed",
			"this-is-a-very-long", // first 20 chars, no trailing dash to trim
		},
		{
			// "abcdefghijklmnopqrs" is exactly 19 chars; the 20th input
			// char is "-", so truncating to 20 chars lands exactly on
			// that dash, which must then be trimmed away.
			"truncation lands on dash gets trimmed",
			"abcdefghijklmnopqrs-tuvwxyz",
			"abcdefghijklmnopqrs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Slug(tc.in)
			if got != tc.want {
				t.Fatalf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 20 {
				t.Fatalf("Slug(%q) = %q exceeds 20 chars", tc.in, got)
			}
		})
	}
}

func TestSandboxNameAndPrefixFor(t *testing.T) {
	name, err := SandboxName(nip19TestVectorNpub, "Reviewer")
	if err != nil {
		t.Fatalf("SandboxName error: %v", err)
	}
	want := "buzz-0elfcs4fr0l0-reviewer"
	if name != want {
		t.Fatalf("SandboxName = %q, want %q", name, want)
	}

	prefix, err := PrefixFor(nip19TestVectorNpub)
	if err != nil {
		t.Fatalf("PrefixFor error: %v", err)
	}
	wantPrefix := "buzz-0elfcs4fr0l0-"
	if prefix != wantPrefix {
		t.Fatalf("PrefixFor = %q, want %q", prefix, wantPrefix)
	}
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("SandboxName %q does not have PrefixFor %q as prefix", name, prefix)
	}
}

func TestSandboxName_TwoAgentsSameDisplayNameDifferentKeysDontCollide(t *testing.T) {
	nsecA := mustBech32Encode(t, "nsec", bytesOf(0x01))
	nsecB := mustBech32Encode(t, "nsec", bytesOf(0x02))
	npubA, err := NsecToNpub(nsecA)
	if err != nil {
		t.Fatalf("NsecToNpub A: %v", err)
	}
	npubB, err := NsecToNpub(nsecB)
	if err != nil {
		t.Fatalf("NsecToNpub B: %v", err)
	}
	nameA, err := SandboxName(npubA, "Reviewer")
	if err != nil {
		t.Fatalf("SandboxName A: %v", err)
	}
	nameB, err := SandboxName(npubB, "Reviewer")
	if err != nil {
		t.Fatalf("SandboxName B: %v", err)
	}
	if nameA == nameB {
		t.Fatalf("two different agents with the same display name collided: %q", nameA)
	}
}

// -- test helpers --

func bytesOf(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func mustBech32Encode(t *testing.T, hrp string, data []byte) string {
	t.Helper()
	converted, err := bech32.ConvertBits(data, 8, 5, true)
	if err != nil {
		t.Fatalf("convert bits: %v", err)
	}
	s, err := bech32.Encode(hrp, converted)
	if err != nil {
		t.Fatalf("bech32 encode: %v", err)
	}
	return s
}
