package intake

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestDecodeSignalInputs covers the GitHub pull_request notifications that feed the merge queue
// signal: enqueued, dequeued and merged. Each decodes an event on an issue branch of the daemon's
// own project, rejects another project's pull request in a shared repository and any branch that
// names no issue, and carries no synthetic time (queue times come from GitHub's timeline, never
// from webhook receipt time or updated_at).
func TestDecodeSignalInputs(t *testing.T) {
	t.Parallel()
	const subject = "notifications.github.sjawhar.legion.pr.42"
	readFixture := func(t *testing.T, name string) []byte {
		t.Helper()
		data, err := os.ReadFile("testdata/github/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return data
	}
	t.Run("github-enqueued", func(t *testing.T) {
		t.Parallel()
		data := readFixture(t, "pr-enqueued.json")
		got, err := decodeMessage(subject, "LEGION", capturedRepositories, data)
		if err != nil {
			t.Fatalf("decode pr-enqueued: %v", err)
		}
		want := PullRequestEnqueued{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-captured"}
		if !reflect.DeepEqual(got.Fact, want) {
			t.Fatalf("fact = %#v, want %#v", got.Fact, want)
		}
		// Another project's pull request in the same heard repository keeps nothing.
		other, err := decodeMessage(subject, "OTHER", capturedRepositories, data)
		if err != nil {
			t.Fatalf("decode pr-enqueued for another project: %v", err)
		}
		if other.Fact != nil {
			t.Fatalf("another project's pr-enqueued decoded to %#v, want nil", other.Fact)
		}
		// A branch that names no issue keeps nothing.
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatal(err)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(env["payload"].(string)), &payload); err != nil {
			t.Fatal(err)
		}
		payload["head_ref"] = "feature/no-issue"
		encodedPayload, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		env["payload"] = string(encodedPayload)
		noIssue, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		gotNoIssue, err := decodeMessage(subject, "LEGION", capturedRepositories, noIssue)
		if err != nil {
			t.Fatalf("decode pr-enqueued on non-issue branch: %v", err)
		}
		if gotNoIssue.Fact != nil {
			t.Fatalf("non-issue branch pr-enqueued decoded to %#v, want nil", gotNoIssue.Fact)
		}
	})
	t.Run("github-dequeued", func(t *testing.T) {
		t.Parallel()
		payload, err := json.Marshal(map[string]string{
			"kind": "pr", "action": "dequeued", "repo": "sjawhar/legion", "number": "42", "title": "Captured GitHub event",
			"author": "author", "url": "https://github.com/sjawhar/legion/pull/42", "head_sha": "head-captured", "before": "",
			"sender": "github-merge-queue[bot]", "head_ref": "legion/LEGION-208", "head_repo": "sjawhar/legion", "base_ref": "main",
			"merged": "false", "merge_commit_sha": "", "merged_by": "", "updated_at": "2026-09-23T00:00:00Z",
			"body": "Dispatch: LEGION-208",
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{
			"event_id": "dequeued-signal-event", "source": "github", "source_event_id": "dequeued-signal-delivery", "topic": subject,
			"dedupe_key": "github.dequeued-signal-delivery", "issued_at": int64(1_790_124_795_391),
			"payload_summary": "pr dequeued: sjawhar/legion#42 Captured GitHub event", "payload": string(payload),
			"trace_id": "dequeued-signal-trace",
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeMessage(subject, "LEGION", capturedRepositories, data)
		if err != nil {
			t.Fatalf("decode dequeued: %v", err)
		}
		want := PullRequestDequeued{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-captured"}
		if !reflect.DeepEqual(got.Fact, want) {
			t.Fatalf("fact = %#v, want %#v", got.Fact, want)
		}
	})
	t.Run("github-merged", func(t *testing.T) {
		t.Parallel()
		data := readFixture(t, "pr-merged.json")
		got, err := decodeMessage(subject, "LEGION", capturedRepositories, data)
		if err != nil {
			t.Fatalf("decode pr-merged: %v", err)
		}
		want := PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-captured", HeadSHA: "head-captured"}
		if !reflect.DeepEqual(got.Fact, want) {
			t.Fatalf("fact = %#v, want %#v", got.Fact, want)
		}
	})
}
