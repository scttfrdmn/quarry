package quarry

import "runtime/debug"

// Build identity, for the one purpose a record needs it: naming what produced it (#13, P8).
//
// WHY THIS LIVES IN THE ROOT PACKAGE rather than in cmd/quarry. RunRecord.Producer is a
// hashed field of a contract other repos parse, so the string that lands in it must come
// from one place; assembling it independently at each site is how a record comes to
// disagree with the binary that wrote it.
//
// THE HOST EVENT STREAM'S FRAME DELIBERATELY DOES NOT CARRY THIS. StreamEvent.Producer is
// "quarry-go" and stays that way: #9 D2 froze the frame, docs/integration-requirements.md
// §6 documents the line verbatim, bucktooth and rustynail vendor it, and the corpus pins
// it byte-for-byte. Appending a version there would turn every release into a wire change
// for two other repos, to carry a fact the RECORD already carries — and the record is the
// citable artifact (§8). A host that wants the version reads it off the record.
//
// It obeys Go rule 4 — no clock, no network, no SDK. Everything here is either a linker
// constant or read out of the binary's own embedded build info.

// version and commit are STAMPED AT LINK TIME by the release workflow:
//
//	-ldflags "-X github.com/scttfrdmn/quarry.version=v0.1.0 -X github.com/scttfrdmn/quarry.commit=abc1234"
//
// EMPTY ON A DEVELOPMENT BUILD, and deliberately not defaulted to "dev". Absence is not
// zero: an unstamped build asserts nothing about its provenance, which is honest, whereas
// a record stamped "dev" would carry a provenance claim that no release process backed.
// Producer() returns "" in that case and RunRecord.WithProducer treats it as a no-op, so
// a `go run` record is byte-identical to what it was before this field existed.
//
// Unexported with an accessor, so nothing outside this package can set them at runtime:
// a mutable version is a provenance field anything could forge.
var (
	version string
	commit  string
)

// Version is the release version this binary was stamped with, or "" if unstamped.
func Version() string { return version }

// Commit is the revision this binary was built from.
//
// FALLS BACK TO THE EMBEDDED VCS STAMP, which the Go toolchain records automatically for
// a build made inside a git work tree. That fallback is what makes the commit useful on a
// development build — where it is the only one of the two values obtainable — while the
// release version stays empty, because a version is a claim a release process makes and
// the toolchain cannot make it for us.
//
// A DIRTY WORK TREE IS REPORTED AS SUCH. "abc1234-dirty" says the source did not match
// the commit, which is exactly what a reader chasing a divergence needs to know; silently
// printing the clean hash would name a commit that never produced this binary.
func Commit() string {
	if commit != "" {
		return commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		return rev + "-dirty"
	}
	return rev
}

// Producer identifies this build for RunRecord.Producer and the stream frame.
//
//	"quarry-go/v0.1.0 (abc1234)"   a stamped release build
//	""                             an unstamped development build
//
// THE IMPLEMENTATION NAME IS PART OF IT, not decoration: there is a parallel Python
// quarry, the two agree on behaviour but are not the same code, and a host reading a
// vendored record months later needs to know which one wrote it — the same argument that
// put "quarry-go" in StreamEvent.Producer.
//
// EMPTY UNLESS A VERSION WAS STAMPED, even when a commit is available. A commit alone
// identifies source, not a release: it cannot tell a reader whether the artifact was
// published, signed or verifiable, which is the question this field exists to answer
// (#13). A development build is therefore unstamped rather than half-stamped.
func Producer() string {
	if version == "" {
		return ""
	}
	if c := Commit(); c != "" {
		return "quarry-go/" + version + " (" + c + ")"
	}
	return "quarry-go/" + version
}
