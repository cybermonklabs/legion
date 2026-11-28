package intake

import (
	"reflect"
	"testing"
	"time"
)

// A merge carries when GitHub merged the pull request, the merged_at the listener sends from
// GitHub's webhook, so a later merge of the issue can be told from an earlier one. An observation
// that carries none, or one that does not parse, leaves it zero: no reader invents a time.
func TestAMergeCarriesWhenGitHubMergedIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  time.Time
	}{
		{name: "GitHub's merged_at", extra: map[string]any{"merged_at": "2026-11-25T08:00:00Z"}, want: time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)},
		{name: "none"},
		{name: "one that does not parse", extra: map[string]any{"merged_at": "yesterday"}},
	} {
		fields := map[string]any{"kind": "pr", "action": "closed", "number": "42", "head_ref": "legion/LEGION-208", "head_sha": "head-2",
			"base_ref": "main", "merged": "true", "merge_commit_sha": "merge-1"}
		for key, value := range tc.extra {
			fields[key] = value
		}
		decoded, err := decodeMessage("notifications.github.sjawhar.legion.pr.42", "CAPTURE", capturedRepositories,
			githubEventEnvelope(t, "merged-"+tc.name, fields))
		want := PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-1", HeadSHA: "head-2", MergedAt: tc.want}
		if err != nil || !reflect.DeepEqual(decoded.Fact, want) {
			t.Errorf("a merge with %s decoded to %#v (%v), want %#v", tc.name, decoded.Fact, err, want)
		}
	}
}
