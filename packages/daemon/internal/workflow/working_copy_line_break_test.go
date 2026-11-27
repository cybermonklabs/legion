package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// This release's task opens with the working copy line and ends that line with a line break,
// before the continue line (taskOpening). A send writes the line again from byte 0 up to that line
// break (workingCopyLine), whatever the directory holds: a full stop followed by a space, the words
// of a continue line, or a full stop of its own right before the line's. A send whose runtime
// places the workspace elsewhere than the composing's runtime did, or names a directory where the
// composing named the branch alone, writes its own line whole. Every other byte is sent as
// composed, a line break after the line included, and a second send of what the first sent changes
// nothing. A task that opens with the line but holds no line break is sent as it was kept.
//
// A task an earlier release composed holds the line after its continue line and a space, and
// nothing but the directory marks where such a line ends. A send writes it again whole where the
// directories hold no full stop followed by a space, and sends it as it was kept where the
// directory the send would write holds one.
func TestASendWritesTheWorkingCopyLineUpToItsLineBreak(t *testing.T) {
	t.Parallel()
	const (
		key          = "LEGION-208"
		plainRoot    = "/work/"
		fullStopRoot = "/srv/legion v2. state/"
		continueRoot = "/srv/x. Continue root. Issue: LEGION-208. Phase: reviewing. y/"
		resume       = " Resume the existing phase work."
		// thisRelease is what follows the working copy line in a task this release composed.
		thisRelease = "\nContinue root. Issue: LEGION-208. Phase: reviewing."
	)
	type dir = func(key string, repo ghrepo.Repository) string
	under := func(root string) dir {
		return func(key string, repo ghrepo.Repository) string { return root + repo.String() + "/" + key }
	}
	// endsWithAFullStop places a workspace in a directory that holds full stops, none followed by a
	// space, and ends with one, so the line's own full stop follows it right before the line break.
	endsWithAFullStop := func(key string, repo ghrepo.Repository) string {
		return "/srv/legion.v2/" + repo.String() + "/" + key + "."
	}
	// line is the working copy line of key naming the directory where gives on repo, and naming the
	// branch alone where where is nil.
	line := func(where dir, repo ghrepo.Repository) string {
		if where == nil {
			return "Working copy: branch legion/" + key + "."
		}
		return "Working copy: branch legion/" + key + ", directory " + where(key, repo) + "."
	}
	composed := record.Issue{Key: key, Repo: "acme/widgets", Title: "root", Phase: phase.Reviewing}
	left, moved := ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("zeta/tools")
	standing := func(where dir) Standing {
		at, err := standingText(composed, where, todaysWalk(), nil, planTold{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	// numbered is the standing of where for a task whose pull request is #42 (pullRequestPart).
	numbered := func(where dir) Standing {
		at := standing(where)
		at.pull = pullRequestPart(composed, &record.PullRequest{Number: 42})
		return at
	}
	for _, tc := range []struct {
		name string
		kept string
		// composedLine is the daemon's working copy line the kept task holds, at is where it stands,
		// and follows is what the kept task holds right after it.
		composedLine string
		at           int
		follows      string
		// send is the directory the engine names at each send (Config.WorkspaceDir).
		send dir
		// asKept is whether every send leaves the task as it was kept.
		asKept bool
	}{
		{name: "a directory that holds a full stop, a space and the words of a continue line",
			kept:         task(composed, numbered(under(continueRoot)), record.PhaseRow{}, ""),
			composedLine: line(under(continueRoot), left), follows: thisRelease, send: under(continueRoot)},
		{name: "a directory that holds a full stop and a space, in a resume that carries news after it",
			kept:         resumeText(composed, standing(under(fullStopRoot))) + "\n\nThe approved spec moved from version 4 to 5.",
			composedLine: line(under(fullStopRoot), left), follows: thisRelease, send: under(fullStopRoot)},
		{name: "a directory that ends with a full stop, so the line's own stands right before the line break",
			kept:         task(composed, standing(endsWithAFullStop), record.PhaseRow{}, "the reviewer asked again"),
			composedLine: line(endsWithAFullStop, left), follows: thisRelease, send: endsWithAFullStop},
		{name: "a runtime whose directories changed between the composing and the send",
			kept:         resumeText(composed, standing(under(fullStopRoot))),
			composedLine: line(under(fullStopRoot), left), follows: thisRelease, send: under(plainRoot)},
		{name: "a line composed with the branch alone, sent where the runtime names a directory",
			kept:         task(composed, numbered(nil), record.PhaseRow{}, ""),
			composedLine: line(nil, left), follows: thisRelease, send: under(fullStopRoot)},
		{name: "a task that opens with the line and holds no line break",
			kept:         line(under(plainRoot), left) + " Continue root. Issue: LEGION-208. Phase: reviewing." + resume,
			composedLine: line(under(plainRoot), left), follows: " Continue root.", send: under(plainRoot), asKept: true},
		{name: "a task an earlier release composed, whose new directory holds a full stop and a space",
			kept:         earlierOpening + line(under(plainRoot), left) + resume,
			composedLine: line(under(plainRoot), left), at: len(earlierOpening), follows: resume, send: under(fullStopRoot), asKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasPrefix(tc.kept[tc.at:], tc.composedLine+tc.follows) {
				t.Fatalf("the kept task %q, want it to name %q at byte %d, followed by %q: the case's own premise", tc.kept, tc.composedLine, tc.at, tc.follows)
			}
			engine := stepEngine(nil)
			engine.cfg.WorkspaceDir = tc.send
			// want is the task a send to a process of repo sends: as it was kept, or with the line of
			// the send's runtime for repo in the composed line's place and every other byte as kept.
			want := func(repo ghrepo.Repository) string {
				if tc.asKept {
					return tc.kept
				}
				return tc.kept[:tc.at] + line(tc.send, repo) + tc.kept[tc.at+len(tc.composedLine):]
			}
			if got := engine.WorkingCopyAtSend(tc.kept, key, left); got != want(left) {
				t.Errorf("the task sent with no correction = %q,\nwant %q: the line the send's runtime gives acme/widgets, whole, and every other byte as composed", got, want(left))
			}
			sent := engine.WorkingCopyAtSend(tc.kept, key, moved)
			if sent != want(moved) {
				t.Errorf("the task sent after the correction = %q,\nwant %q: the line the send's runtime gives zeta/tools, whole, and every other byte as composed", sent, want(moved))
			}
			if again := engine.WorkingCopyAtSend(sent, key, moved); again != sent {
				t.Errorf("the same task sent a second time = %q,\nwant it as the first send left it, %q: a send of a line that already names the process's directory changes nothing", again, sent)
			}
		})
	}
}
