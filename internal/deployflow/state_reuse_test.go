package deployflow

// State-file reuse coverage: Lakebox does not persist caller-set sandbox
// names (live-verified 2026-07-26 — list/status echo the id as "name"),
// and the desktop never sends backend_agent_id, so the provider's own
// state file is the ONLY thing standing between a redeploy and an
// orphaned --no-autostop sandbox. These tests pin that behavior.

import (
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/identity"
	"github.com/IceRhymers/buzz-lakebox/internal/state"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

func testStateKey(t *testing.T) string {
	t.Helper()
	npub, err := identity.NsecToNpub(testNsec)
	if err != nil {
		t.Fatalf("NsecToNpub: %v", err)
	}
	return state.Key(version.DefaultProfile, npub)
}

// TestDeploy_ReusesSandboxFromState: with a recorded mapping and an empty
// sandbox list (the post-naming-regression reality: name matching finds
// nothing), deploy must reuse the mapped sandbox — starting it when
// stopped — and never create a new one.
func TestDeploy_ReusesSandboxFromState(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_STATUS_STATUS", "Stopped")
	t.Setenv("FAKE_STATUS_FLIP_FILE", h.logPath+".flip")

	if err := h.dep.State.Record(testStateKey(t), state.Entry{SandboxID: "sandbox-from-state"}); err != nil {
		t.Fatalf("pre-record mapping: %v", err)
	}

	agentID, err := h.dep.Deploy(buildReq(reqOpts{}))
	if err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}
	if agentID != "sandbox-from-state" {
		t.Fatalf("agent_id = %q, want the state-mapped sandbox-from-state", agentID)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:status", "CLI:start", "SSH:pat-reset"})
	assertNotContains(t, seq, "CLI:create")
	assertNotContains(t, seq, "CLI:list") // state hit skips the legacy match entirely
}

// TestDeploy_RecordsMappingAfterCreate: a fresh create must persist the
// npub→sandbox mapping so the NEXT redeploy can find it.
func TestDeploy_RecordsMappingAfterCreate(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-recorded-1")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	entry, ok, err := h.dep.State.Lookup(testStateKey(t))
	if err != nil || !ok {
		t.Fatalf("mapping not recorded after deploy: ok=%v err=%v", ok, err)
	}
	if entry.SandboxID != "sandbox-recorded-1" {
		t.Fatalf("recorded sandbox id = %q, want sandbox-recorded-1", entry.SandboxID)
	}
}

// TestDeploy_StaleStateFallsThroughToCreate: a mapping whose sandbox no
// longer exists (status probe fails) must not fail the deploy — it falls
// through to create, and the mapping is refreshed to the new sandbox.
func TestDeploy_StaleStateFallsThroughToCreate(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_STATUS_EXIT", "1") // mapped sandbox is gone
	t.Setenv("FAKE_CREATE_ID", "sandbox-replacement-1")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if err := h.dep.State.Record(testStateKey(t), state.Entry{SandboxID: "sandbox-deleted-0"}); err != nil {
		t.Fatalf("pre-record mapping: %v", err)
	}

	agentID, err := h.dep.Deploy(buildReq(reqOpts{}))
	if err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}
	if agentID != "sandbox-replacement-1" {
		t.Fatalf("agent_id = %q, want the replacement sandbox", agentID)
	}

	entry, ok, _ := h.dep.State.Lookup(testStateKey(t))
	if !ok || entry.SandboxID != "sandbox-replacement-1" {
		t.Fatalf("mapping not refreshed after stale fall-through: %+v ok=%v", entry, ok)
	}
}

// TestDeploy_NilStateStillDeploys: persistence unavailable (nil store —
// e.g. no resolvable home dir) must not break deploys; it just loses
// reuse.
func TestDeploy_NilStateStillDeploys(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")
	h.dep.State = nil

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("Deploy() with nil state store: %v", err)
	}
}
