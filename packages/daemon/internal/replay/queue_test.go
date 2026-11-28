package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// loggedPullRequest is a line of the window's log at at that names the pull request number for the
// issue key, as the daemon logs a pull request it watches: with no repository.
func loggedPullRequest(at, key string, number int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",`+
		`"issue":%q,"pullRequest":%d,"head":"0123456789abcdef0123456789abcdef01234567"}`, at, key, number))
}

// timelineRequest is the request collect-queue sends for the first page of the timeline of
// cybermonklabs/legion#number, as timelineStub records it.
func timelineRequest(number int) stubRequest {
	return stubRequest{Method: http.MethodGet, Target: fmt.Sprintf("/repos/cybermonklabs/legion/issues/%d/timeline?per_page=100", number)}
}

// A pull request that only a line of the window's log names, for an issue of cybermonklabs/legion,
// and whose timeline GitHub answers 404 Not Found in that repository, is no pull request of it: the
// line names no repository, so the number can be one of another repository's. collect-queue reads
// it once, names it (NotFound), and reads the others and writes their events: inputsWindow with a
// line naming #11 for ACME-1, read first, is left as collect-queue leaves inputsWindow, with the
// queue events of #12 and #13. It fails where a 404 of such a pull request fails the read.
func TestCollectQueueSkipsAndNamesALogNamedPullRequestGitHubDoesNotFind(t *testing.T) {
	t.Parallel()
	w := inputsWindow(t)
	w.Log = append(w.Log, loggedPullRequest("2001-03-05T10:20:00Z", "ACME-1", 11))
	dir := writtenAged(t, w)
	want, err := ReadWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	lines := []json.RawMessage{}
	for _, number := range []int{12, 13} {
		queued, err := ParseQueueTimeline(number, inputsTimelines[number]...)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, queued...)
	}
	want.Queue["cybermonklabs/legion"] = lines
	want.Signals[SignalQueue] = []Span{{From: want.From.Add(-time.Duration(want.Before)), To: want.End()}}
	wantDir := filepath.Join(t.TempDir(), "want")
	if err := want.Write(wantDir); err != nil {
		t.Fatal(err)
	}

	server, seen := timelineStub(t, inputsTimelines)
	queued, err := CollectQueue(context.Background(), QueueConfig{Repo: "cybermonklabs/legion", Token: stubGitHubToken, API: server.URL, Window: dir, Wait: noWait})
	if err != nil {
		t.Fatalf("CollectQueue: %v", err)
	}
	if !slices.Equal(queued.PullRequests, []int{12, 13}) || queued.Lines != len(lines) || !slices.Equal(queued.NotFound, []int{11}) ||
		!slices.Equal(queued.NoRepo, []int{14}) {
		t.Errorf("collect-queue read the pull requests %v, wrote %d lines, names %v as not found and %v as of no repository; want [12 13], %d, [11] and [14]",
			queued.PullRequests, queued.Lines, queued.NotFound, queued.NoRepo, len(lines))
	}
	if got, want := seen.all(), []stubRequest{timelineRequest(11), timelineRequest(12), timelineRequest(13)}; !slices.Equal(got, want) {
		t.Errorf("collect-queue sent %v to GitHub, want %v: #11 once, then #12 and #13", got, want)
	}
	if got, want := windowFiles(t, dir), windowFiles(t, wantDir); !maps.EqualFunc(got, want, bytes.Equal) {
		for _, name := range fileNames(got, want) {
			if !bytes.Equal(got[name], want[name]) {
				t.Errorf("collect-queue left %s as\n  %q\nwant\n  %q", name, got[name], want[name])
			}
		}
	}
}

// A pull request that an attribution of an issue of the window's project moves in
// cybermonklabs/legion is that repository's, so a 404 Not Found of its timeline is a fault:
// collect-queue fails, naming it, and writes nothing, whether a line of the log names it too or
// not. #11, which only the log names and GitHub answers 404 for, is read first and skipped, so the
// read that fails is that of #15, the attributed one. It fails where collect-queue skips every
// 404, or fails on the first.
func TestCollectQueueFailsOnAnAttributedPullRequestGitHubDoesNotFind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// logged is whether a line of the log names #15 too.
		logged bool
	}{
		{"an attribution names it", false},
		{"an attribution and a line of the log name it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := inputsWindow(t)
			w.Log = append(w.Log, loggedPullRequest("2001-03-05T10:20:00Z", "ACME-1", 11))
			if tc.logged {
				w.Log = append(w.Log, loggedPullRequest("2001-03-05T10:25:00Z", "ACME-1", 15))
			}
			w.Events["ACME-2"] = append(w.Events["ACME-2"], inputsAttribution(5, 3, "ACME-2", "cybermonklabs/legion", 15))
			dir := writtenAged(t, w)
			before := fileStates(t, dir)

			server, seen := timelineStub(t, inputsTimelines)
			_, err := CollectQueue(context.Background(), QueueConfig{Repo: "cybermonklabs/legion", Token: stubGitHubToken, API: server.URL, Window: dir, Wait: noWait})
			if want := `replay: collect-queue: the timeline of cybermonklabs/legion#15: GitHub answered 404 Not Found: {"message":"Not Found"}`; err == nil || err.Error() != want {
				t.Errorf("collect-queue answered %v, want %q", err, want)
			}
			if got, want := seen.all(), []stubRequest{timelineRequest(11), timelineRequest(12), timelineRequest(13), timelineRequest(15)}; !slices.Equal(got, want) {
				t.Errorf("collect-queue sent %v to GitHub, want %v: #11 once, then #12, #13 and #15", got, want)
			}
			if changed := changedFiles(before, fileStates(t, dir)); len(changed) != 0 {
				t.Errorf("collect-queue changed %v, want nothing written", changed)
			}
		})
	}
}
