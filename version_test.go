package quarry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the PROVENANCE guarantees of the build stamp (#13, P8), not the format
// of a version string. The format is cosmetic; what a record promises about the build that
// wrote it is not.
//
// The package-level `version`/`commit` vars are stamped at link time, so a test cannot set
// them via ldflags. Where a stamped build is needed, the test sets and restores them —
// which is safe because these tests do not run in parallel with anything reading them.

// A development build must assert NOTHING about its provenance.
//
// THE FAILURE THIS FORBIDS is a record stamped "dev" or "unknown", which reads as a
// provenance fact while being the absence of one. Absence is not zero: an unstamped record
// says "nothing was claimed", and only a release build's stamp is worth trusting precisely
// because the unstamped case is empty rather than filled in with a placeholder.
func TestAnUnstampedBuildClaimsNoProducer(t *testing.T) {
	defer stampFor(t, "", "")()

	if got := Producer(); got != "" {
		t.Fatalf("an unstamped build must claim no producer, got %q — a placeholder is a "+
			"provenance claim nothing backs (P8)", got)
	}
}

// A commit alone is not a release.
//
// A commit identifies SOURCE; it cannot tell a reader whether the artifact was published,
// signed or verifiable, which is the question #13 exists to answer. So a build with a VCS
// stamp but no version is still unstamped as far as a record is concerned.
func TestACommitWithoutAVersionIsNotAProducer(t *testing.T) {
	defer stampFor(t, "", "abc1234")()

	if got := Commit(); got != "abc1234" {
		t.Fatalf("an explicit commit stamp must be reported: %q", got)
	}
	if got := Producer(); got != "" {
		t.Fatalf("a commit without a released version is not a producer, got %q: it says what "+
			"source was built, not that anything was released or signed (#13)", got)
	}
}

func TestAStampedBuildNamesItselfAndItsImplementation(t *testing.T) {
	defer stampFor(t, "v0.1.0", "abc1234")()

	got := Producer()
	// The IMPLEMENTATION is named, not just the version. There is a parallel Python quarry
	// that agrees on behaviour and is not the same code, so a record naming only "v0.1.0"
	// would not say which one wrote it — the same argument that put "quarry-go" in the
	// stream frame.
	if !strings.HasPrefix(got, "quarry-go/") {
		t.Fatalf("the producer must name the implementation, not only the version: %q", got)
	}
	if !strings.Contains(got, "v0.1.0") || !strings.Contains(got, "abc1234") {
		t.Fatalf("the producer must carry both the version and the commit: %q", got)
	}
}

// The record's producer is INSIDE its identity, like PlanID (#13, P8 / #15 D3).
//
// Two runs of the same tree under the same caps from different builds are different
// records. Without the re-hash the field would be settable after the fact on a record that
// still hashed to its own RunID — a provenance claim that could be edited in.
func TestStampingAProducerChangesTheRunID(t *testing.T) {
	rec := NewRunRecord(Result{}, problem("root"), planCaps(FromFloat(1)), ModeFresh)
	before := rec.RunID

	stamped := rec.WithProducer("quarry-go/v0.1.0 (abc1234)")
	if stamped.RunID == before {
		t.Fatal("Producer is a hashed field: stamping it must re-derive the RunID, or a " +
			"provenance claim could be added to a record without disturbing its identity")
	}
	if got := RecordHash(stamped); got != stamped.RunID {
		t.Fatalf("a stamped record must hash to its own RunID: stored %s, recomputed %s",
			stamped.RunID, got)
	}
	// And a DIFFERENT build gives a different record, which is the point of hashing it.
	other := rec.WithProducer("quarry-go/v0.2.0 (def5678)")
	if other.RunID == stamped.RunID {
		t.Fatal("two builds producing the same tree must yield different records")
	}
}

// An empty producer must leave the record BYTE-IDENTICAL, not merely equal-looking.
//
// This is what keeps every development-build record — the whole suite, testdata, and
// anything a user already has — unchanged by this field's existence. Asserting the RunID
// alone would miss a canonical-bytes change that happened to collide.
func TestAnUnstampedRecordIsUntouchedByTheProducerField(t *testing.T) {
	rec := NewRunRecord(Result{}, problem("root"), planCaps(FromFloat(1)), ModeFresh)
	beforeBytes, err := rec.Canonical()
	if err != nil {
		t.Fatal(err)
	}

	after := rec.WithProducer("")
	afterBytes, err := after.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("stamping an empty producer must be a no-op on the canonical bytes:\n  before %s\n  after  %s",
			beforeBytes, afterBytes)
	}
	if after.RunID != rec.RunID {
		t.Fatalf("an unstamped record's identity must not change: %s vs %s", rec.RunID, after.RunID)
	}
	if strings.Contains(string(afterBytes), "Producer") {
		t.Fatal("omitempty is load-bearing: an unstamped record must not mention the field at " +
			"all, or every record written before it existed stops hashing to its own RunID")
	}

	// AND THE CASE ONLY THE GUARD COVERS. Everything above passes with WithProducer's
	// `if producer == ""` return DELETED, because omitempty already drops an empty field from
	// the canonical bytes — assigning "" over "" changes nothing. Found by reintroducing that
	// defect behind this test's back and watching it stay green.
	//
	// What the early return actually prevents is UNSTAMPING an already-stamped record: without
	// it, WithProducer("") assigns "", omitempty drops the field, the re-hash produces the
	// unstamped record's identity, and a release build's provenance is erased by a caller
	// passing an empty string. That is a P8 fact silently discarded, not a no-op.
	stamped := rec.WithProducer("quarry-go/v0.1.0 (abc1234)")
	if again := stamped.WithProducer(""); again.Producer != stamped.Producer || again.RunID != stamped.RunID {
		t.Fatalf("an empty producer must not ERASE one already recorded: %q/%s became %q/%s",
			stamped.Producer, stamped.RunID, again.Producer, again.RunID)
	}
}

// The pre-change fixture must STILL hash to its own RunID (P8).
//
// The same guarantee TestAnUngatedRecordHashesAsItDidBeforeThePlanFieldExisted makes for
// PlanID, restated for Producer against the same captured file — and it has to be the
// captured file, because a record built by today's code carries today's field set by
// construction and cannot witness the addition.
func TestAPreProducerRecordStillHashesToItsOwnRunID(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "record-pre-planid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Producer") {
		t.Fatal("fixture is not a pre-change record — it must predate the Producer field, " +
			"or this test compares today's code with itself")
	}

	var old RunRecord
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	if old.Producer != "" {
		t.Fatal("a record written before the field existed cannot name a producer")
	}
	if got := RecordHash(old); got != old.RunID {
		t.Fatalf("adding Producer must not change what an existing record hashes to: "+
			"stored %s, recomputed %s — remove omitempty and every record on disk is "+
			"reported as edited (P8)", old.RunID, got)
	}
}

// A REPLAY INHERITS THE ORIGINAL'S PRODUCER, and this is the guarantee most likely to be
// got wrong in the plausible direction.
//
// The tempting reading is that a replay is a new execution by THIS binary, so it should
// stamp its own version. Then a v0.2 binary replaying a v0.1 record produces a record
// differing in the producer field, and `quarry replay` reports a divergence caused by
// nothing but the passage of time — exactly the failure BoundBy taught. The build that ran
// it is a fact of the original execution.
func TestAReplayInheritsTheProducerRatherThanStampingItsOwn(t *testing.T) {
	orig := NewRunRecord(Result{}, problem("root"), planCaps(FromFloat(1)), ModeFresh).
		WithProducer("quarry-go/v0.1.0 (abc1234)")

	// The replaying binary is a DIFFERENT, stamped build — the case that breaks if the field
	// is re-derived. Without this stamp the test would pass against a re-deriving
	// implementation too, since Producer() would return "" and inheriting "" from an
	// unstamped original is indistinguishable.
	defer stampFor(t, "v0.2.0", "def5678")()

	replayed := ReplayRecord(Result{Outcomes: orig.Outcomes}, orig)
	if replayed.Producer != "quarry-go/v0.1.0 (abc1234)" {
		t.Fatalf("a replay must inherit the ORIGINAL's producer, got %q: re-deriving it makes "+
			"a v0.2 replay of a v0.1 record diverge on a field the replay never observed (P8)",
			replayed.Producer)
	}
	if replayed.RunID != orig.RunID {
		t.Fatalf("a faithful replay of an unchanged tree must reproduce the RunID: %s vs %s",
			orig.RunID, replayed.RunID)
	}
}

// stampFor sets the link-time vars for one test and returns a restore func.
//
// A HELPER RATHER THAN t.Setenv-STYLE MAGIC because these are package vars, not
// environment: the release workflow sets them with -ldflags, which no test can do, so the
// only way to exercise a stamped build is to assign them. Restoring is what keeps the
// tests order-independent.
func stampFor(t *testing.T, v, c string) func() {
	t.Helper()
	oldV, oldC := version, commit
	version, commit = v, c
	return func() { version, commit = oldV, oldC }
}
