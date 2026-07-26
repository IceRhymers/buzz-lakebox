package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "nested", "agents.json")}
}

func TestLookup_MissingFileIsNotAnError(t *testing.T) {
	s := tempStore(t)
	_, ok, err := s.Lookup(Key("DEFAULT", "npub1abc"))
	if err != nil {
		t.Fatalf("Lookup on missing file: %v", err)
	}
	if ok {
		t.Fatal("Lookup on missing file must report not-found")
	}
}

func TestRecordAndLookup_RoundTrip(t *testing.T) {
	s := tempStore(t)
	key := Key("fevm-west", "npub1abc")
	want := Entry{SandboxID: "viable-pika-4294", Profile: "fevm-west"}
	if err := s.Record(key, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, ok, err := s.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("Lookup after Record: ok=%v err=%v", ok, err)
	}
	if got.SandboxID != want.SandboxID || got.Profile != want.Profile {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("Record must stamp UpdatedAt when zero")
	}

	// Same npub on a different profile is a distinct key.
	if _, ok, _ := s.Lookup(Key("other", "npub1abc")); ok {
		t.Fatal("different profile must not share the mapping")
	}
}

func TestRecord_UpsertsWithoutClobberingOthers(t *testing.T) {
	s := tempStore(t)
	if err := s.Record("a", Entry{SandboxID: "one"}); err != nil {
		t.Fatalf("Record a: %v", err)
	}
	if err := s.Record("b", Entry{SandboxID: "two"}); err != nil {
		t.Fatalf("Record b: %v", err)
	}
	if err := s.Record("a", Entry{SandboxID: "one-v2"}); err != nil {
		t.Fatalf("Record a again: %v", err)
	}

	ea, _, _ := s.Lookup("a")
	eb, _, _ := s.Lookup("b")
	if ea.SandboxID != "one-v2" || eb.SandboxID != "two" {
		t.Fatalf("upsert broke entries: a=%+v b=%+v", ea, eb)
	}
}

func TestSave_FileModeIsPrivate(t *testing.T) {
	s := tempStore(t)
	if err := s.Record("a", Entry{SandboxID: "one", UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
}

func TestLookup_CorruptFileIsAnError(t *testing.T) {
	s := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup("a"); err == nil {
		t.Fatal("corrupt state file must surface as an error, not silent not-found")
	}
}
