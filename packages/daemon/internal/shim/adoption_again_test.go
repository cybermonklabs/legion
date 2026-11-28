package shim_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// laterWorkingCopyReads is the case of a recording jj in which every `jj debug working-copy`
// after the first, the first being the freshness check's before the adoption's change, runs answer
// in place of the real jj, and the first runs the real jj. marker is a file of the test's own.
func laterWorkingCopyReads(marker, answer string) string {
	return "*' debug working-copy '*)\nif [ -e " + shellQuoted(marker) + " ]; then\n" + answer + "\nfi\n: > " + shellQuoted(marker) + " ;;\n"
}

// An adoption takes no edit off the disk of a working copy that its freshness check does not read
// as stale (R4 of dispatch://INFRA-3916). After an adoption's change, and until the role's next jj
// command, @ was rewritten after the operation the working copy recorded, with its tree kept,
// which jj finds fresh: there `jj workspace update-stale` would take the edits that no jj command
// recorded off the disk, into a divergent commit. In a workspace an issue pod provisions, a role
// changes a tracked file and writes a new one, the adoption runs as tester (the metaedit branch),
// and another adoption follows as reviewer with no jj command between. That adoption runs no
// update. It reads the working copy fresh by its tree, with no read of the operation log, and
// answers success naming no commit. Or a command of its check before or after its change fails or
// prints what does not parse: it then refuses, naming that check, writes one warning line, and
// runs nothing after that command. Either way both edits are still on disk, the role's next `jj
// status` finds the working copy fresh, and both edits are then in @.
//
// The jj is the pinned one (fixturegit.PinnedJJ), and the second adoption runs it through a
// recording wrapper that also fails or answers in its place where a row says.
func TestAnAdoptionTakesNoEditOffTheDiskOfAWorkingCopyItDoesNotReadStale(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	edits := map[string]string{"README.md": "smoke, edited by a role\n", "new.txt": "a file no jj command recorded\n"}
	const injected = "echo 'Error: an injected failure of the check' >&2; exit 1"
	for _, tc := range []struct {
		name string
		// cases answers the cases of the second adoption's recording jj; marker is a file of the
		// test's own a case may use.
		cases func(marker string) string
		ok    bool
		// check is the freshness check whose refusal ends the adoption, "" for none, and errorHas
		// what that refusal says of the command besides.
		check    string
		errorHas []string
		// reads is how many times the adoption ran `jj debug working-copy`.
		reads int
	}{
		// The jj fails a read of the head's last 1,000 operations, which the check never runs.
		{name: "adopted again, with no jj command between", ok: true, reads: 2,
			cases: func(string) string {
				return "*' op log -n 1000 '*) echo 'Error: an injected failure of the operation log' >&2; exit 1 ;;\n"
			}},
		{name: "the check before the change prints a record that does not parse", reads: 1,
			cases: func(marker string) string {
				return firstWorkingCopyRead(marker, "echo 'Current operation: of another shape'; exit 0")
			},
			check: "before the adoption's change", errorHas: []string{"jj debug working-copy printed what does not parse", "Current operation: of another shape"}},
		{name: "a command of the check before the change fails", reads: 1,
			cases: func(marker string) string { return firstWorkingCopyRead(marker, injected) },
			check: "before the adoption's change", errorHas: []string{"Command failed (exit 1)", "debug working-copy", "an injected failure of the check"}},
		{name: "a command of the check after the change fails", reads: 2,
			cases: func(marker string) string { return laterWorkingCopyReads(marker, injected) },
			check: "after the adoption's change", errorHas: []string{"Command failed (exit 1)", "debug working-copy", "an injected failure of the check"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWorkspace(t, jj)
			w.write(edits)
			w.adopt("tester")
			bin, log := recordingJJ(t, jj, tc.cases(filepath.Join(t.TempDir(), "answered")))

			got, shim := w.adoptWith("reviewer", bin, 60_000)
			if got.OK != tc.ok {
				t.Fatalf("the shim answered %#v, want ok=%v; log:\n%s", got, tc.ok, shim.log)
			}
			if len(got.EditsCommits) != 0 {
				t.Errorf("the answer names %q, want no commit: no edit left the disk", got.EditsCommits)
			}
			refusal := "the freshness check " + tc.check + " could not read the working copy, so the adoption runs no jj workspace update-stale and refuses"
			if !tc.ok {
				for _, want := range append([]string{refusal}, tc.errorHas...) {
					if !strings.Contains(got.Error, want) {
						t.Errorf("the refusal %q does not say %q", got.Error, want)
					}
				}
			}
			ran := commandsRan(t, log)
			if n := countLines(ran, "workspace update-stale"); n != 0 {
				t.Errorf("jj workspace update-stale ran %d time(s); jj ran:\n%s", n, strings.Join(ran, "\n"))
			}
			if n := countLines(ran, " debug working-copy "); n != tc.reads || !tc.ok && !strings.Contains(ran[len(ran)-1], " debug working-copy ") {
				t.Errorf("the adoption ran jj debug working-copy %d time(s), want %d, and nothing after a check it could not read; jj ran:\n%s",
					n, tc.reads, strings.Join(ran, "\n"))
			}
			warned := 0
			if !tc.ok {
				warned = 1
			}
			if warnings := shim.log.count("warning: the freshness check"); warnings != warned || !tc.ok && shim.log.count("warning: "+refusal) != 1 {
				t.Errorf("the shim wrote %d warning line(s) of the freshness check, want %d naming the check %q; log:\n%s", warnings, warned, tc.check, shim.log)
			}
			for name, body := range edits {
				if onDisk, err := os.ReadFile(filepath.Join(w.dir, name)); err != nil || string(onDisk) != body {
					t.Errorf("%s on disk after the adoption is %q (%v), want the edit %q that no jj command recorded", name, onDisk, err, body)
				}
			}

			status, err := w.try("reviewer", "jj", "status")
			if err != nil || strings.Contains(strings.ToLower(status), "stale") {
				t.Fatalf("jj status after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", err, status)
			}
			for name, body := range edits {
				if inAt, _ := w.fileIn("@", name); inAt != strings.TrimSpace(body) {
					t.Errorf("after jj status, @ holds %s as %q, want the edit %q that no jj command recorded before the adoption", name, inAt, strings.TrimSpace(body))
				}
			}
			if !tc.ok {
				return
			}
			if at := w.as("reviewer", "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", `description ++ "|" ++ author.name()`); at != "|reviewer" {
				t.Errorf("@ after the adoption is %q (description|author), want an undescribed commit authored by reviewer", at)
			}
		})
	}
}
