package workflow

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mainCommit is the base commit GitHub's answers in these tests name for main, spelled as GitHub
// spells a commit.
const mainCommit = "4444444444444444444444444444444444444444"

// mergeStateReads is every line of logged that records a read of a pull request's merge state
// (MergeStateReadLine), in the order written.
func mergeStateReads(logged *bytes.Buffer) []map[string]any {
	var reads []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == MergeStateReadLine {
			reads = append(reads, entry)
		}
	}
	return reads
}

// The daemon logs each read of a pull request's merge state the record takes, a clean read
// included, with the pull request, the head GitHub answered for, the base and the base commit
// GitHub's answer names, so a window collected later gives git the exact head and base of each
// read (dispatch://INFRA-3199/spec R9). Taken in turn on one pull request, a clean read and a read
// that finds a conflict each write exactly one line. So does each read the engine drops - one of a
// head the record has left and one older than the read the record holds - since GitHub was read
// all the same; and a read that names no base branch and no base commit logs the record's base and
// no commit.
func TestEachReadOfAMergeStateIsLoggedWithItsHeadAndBase(t *testing.T) {
	t.Parallel()
	pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
	var logged bytes.Buffer
	engine := readsAfter(0)
	engine.log = slog.New(slog.NewJSONHandler(&logged, nil))
	read := func(head string, mergeable record.Mergeability, state record.MergeState, after time.Duration) intake.PullRequestMergeability {
		return intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", BaseSHA: mainCommit, HeadSHA: head,
			Mergeable: mergeable, MergeState: state, ReadAt: firstConflictRead.Add(after)}
	}
	unnamed := read(conflictHead, record.MergeabilityMergeable, record.MergeStateClean, 3*time.Minute)
	unnamed.Base, unnamed.BaseSHA = "", ""
	for _, step := range []struct {
		name      string
		fact      intake.PullRequestMergeability
		base, sha string
	}{
		{"a clean read", read(conflictHead, record.MergeabilityMergeable, record.MergeStateClean, 0), "main", mainCommit},
		{"a read that finds a conflict", read(conflictHead, record.MergeabilityConflicting, record.MergeStateDirty, time.Minute), "main", mainCommit},
		{"a read of a head the record has left", read(headThree, record.MergeabilityMergeable, record.MergeStateClean, 2*time.Minute), "main", mainCommit},
		{"a read older than the one the record holds", read(conflictHead, record.MergeabilityMergeable, record.MergeStateClean, 30*time.Second), "main", mainCommit},
		{"a read that names no base branch and no base commit", unnamed, "main", ""},
	} {
		logged.Reset()
		applyRefusingNothing(t, pool, engine, step.fact)
		lines := mergeStateReads(&logged)
		if len(lines) != 1 {
			t.Fatalf("%s logged %d lines %q (%v), want exactly one", step.name, len(lines), MergeStateReadLine, lines)
		}
		line := lines[0]
		text, _ := line["readAt"].(string)
		readAt, err := time.Parse(time.RFC3339Nano, text)
		if line["issue"] != "LEGION-208" || line["repo"] != "sjawhar/legion" || line["pullRequest"] != float64(42) ||
			line["head"] != step.fact.HeadSHA || line["recordedHead"] != conflictHead || line["base"] != step.base || line["baseSha"] != step.sha ||
			line["mergeable"] != string(step.fact.Mergeable) || line["mergeState"] != string(step.fact.MergeState) ||
			err != nil || !readAt.Equal(step.fact.ReadAt) {
			t.Fatalf("%s logged %v, want LEGION-208's pull request #42 of sjawhar/legion read at head %s, the record at %s, on base %s at commit %q, %s and %s, read at %s",
				step.name, line, step.fact.HeadSHA, conflictHead, step.base, step.sha, step.fact.Mergeable, step.fact.MergeState, step.fact.ReadAt)
		}
	}
}
