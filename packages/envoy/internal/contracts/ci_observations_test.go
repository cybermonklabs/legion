package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGithubCIObservationsUsesCheckRunStartedAtBeforeCompletion(t *testing.T) {
	body := map[string]any{
		"repository": map[string]any{
			"name":  "example-repo",
			"owner": map[string]any{"login": "example-org"},
		},
		"check_run": map[string]any{
			"id":            float64(987654321),
			"name":          "unit-tests",
			"status":        "in_progress",
			"head_sha":      "abcdef1234567",
			"started_at":    "2026-09-07T02:00:00Z",
			"pull_requests": []any{map[string]any{"number": float64(42)}},
		},
	}

	observations := GithubCIObservations("check_run", body)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	if observations[0].ObservedAt != "2026-09-07T02:00:00Z" {
		t.Errorf("check run started timestamp = %q", observations[0].ObservedAt)
	}
}

func TestGithubCIObservationsExtractCheckSuite(t *testing.T) {
	body := map[string]any{
		"repository": map[string]any{
			"name":  "example-repo",
			"owner": map[string]any{"login": "example-org"},
		},
		"check_suite": map[string]any{
			"id":            float64(123456789),
			"status":        "in_progress",
			"conclusion":    nil,
			"head_sha":      "abcdef1234567890abcdef1234567890abcdef12",
			"app":           map[string]any{"id": float64(77)},
			"pull_requests": []any{map[string]any{"number": float64(42)}},
			"updated_at":    "2026-09-07T03:01:00Z",
		},
	}

	observations := GithubCIObservations("check_suite", body)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.SuiteID != "123456789" || observation.AppID != "77" {
		t.Fatalf("suite identity = %+v", observation)
	}
	if observation.Status != "in_progress" || observation.Conclusion != "" {
		t.Fatalf("suite state = %+v", observation)
	}
	if observation.ObservedAt != "2026-09-07T03:01:00Z" {
		t.Errorf("check suite observation timestamp = %q", observation.ObservedAt)
	}
}

// A check_suite webhook gives its suite's latest_check_runs_count, which the webhook's mapping
// keeps as CheckRunCount: a count of 0 as 0, not as none, and an event that gives no whole number
// as none. The two suites are this repository's, each captured with
//
//	gh api repos/cybermonklabs/legion/check-suites/<id> --jq '{id, status, conclusion, latest_check_runs_count, head_sha, head_branch, updated_at, app: {id: .app.id}, pull_requests: [.pull_requests[] | {number}]}'
//
// for 120620537894 (testdata/check_suite_queued_no_runs.json: an App's suite queued with no check
// run) and 120620605808 (testdata/check_suite_completed.json: GitHub Actions' suite completed with
// 48 runs). The API answers each with no pull request and the branch's name as head_branch, so the
// test wraps each in a check_suite webhook body whose suite names pull request 4464, as GitHub's
// event names an open pull request at that head. GithubCIObservation, which Dispatch's CI
// reconcile shares, reads no count.
func TestGithubCIObservationsReadsASuitesRunCount(t *testing.T) {
	const sha = "4198cda5fd0ae76e055089846f40f7be0d10dcf4"
	captured := func(t *testing.T, file string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		suite := map[string]any{}
		if err := json.Unmarshal(raw, &suite); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		return suite
	}
	webhook := func(action string, suite map[string]any) map[string]any {
		suite["pull_requests"] = []any{map[string]any{"number": float64(4464)}}
		return map[string]any{
			"action":      action,
			"check_suite": suite,
			"repository": map[string]any{
				"full_name": "cybermonklabs/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "cybermonklabs"},
			},
		}
	}
	// countText names a count in a failure message.
	countText := func(n *int) string {
		if n == nil {
			return "none"
		}
		return strconv.Itoa(*n)
	}
	// mapsTo fails unless the webhook body maps to the one observation want, the count compared by
	// value.
	mapsTo := func(t *testing.T, body map[string]any, want CIObservation) {
		t.Helper()
		observations := GithubCIObservations("check_suite", body)
		if len(observations) != 1 {
			t.Fatalf("observations = %+v, want one", observations)
		}
		got := observations[0]
		gotCount, wantCount := got.CheckRunCount, want.CheckRunCount
		got.CheckRunCount, want.CheckRunCount = nil, nil
		if got != want {
			t.Fatalf("observation = %+v, want %+v", got, want)
		}
		if (gotCount == nil) != (wantCount == nil) || gotCount != nil && *gotCount != *wantCount {
			t.Fatalf("CheckRunCount = %s, want %s", countText(gotCount), countText(wantCount))
		}
	}

	t.Run("a queued suite with no check run counts 0", func(t *testing.T) {
		mapsTo(t, webhook("requested", captured(t, "check_suite_queued_no_runs.json")), CIObservation{
			Owner: "cybermonklabs", Repo: "legion", Number: "4464", SHA: sha,
			AppID: "5341243", SuiteID: "120620537894", Status: "queued",
			ObservedAt: "2026-11-19T15:56:07Z", CheckRunCount: new(0),
		})
	})
	t.Run("a completed suite counts its 48 runs", func(t *testing.T) {
		mapsTo(t, webhook("completed", captured(t, "check_suite_completed.json")), CIObservation{
			Owner: "cybermonklabs", Repo: "legion", Number: "4464", SHA: sha,
			AppID: "15368", SuiteID: "120620605808", Status: "completed", Conclusion: "success",
			ObservedAt: "2026-11-19T16:20:04Z", CheckRunCount: new(48),
		})
	})
	for _, tc := range []struct {
		name   string
		absent bool
		value  any
	}{
		{name: "absent", absent: true},
		{name: "null", value: nil},
		{name: "negative", value: float64(-1)},
		{name: "fractional", value: 2.5},
		{name: "a string", value: "3"},
		{name: "past any count GitHub gives", value: float64(1 << 40)},
	} {
		t.Run("a count that is "+tc.name+" is none", func(t *testing.T) {
			suite := captured(t, "check_suite_queued_no_runs.json")
			if tc.absent {
				delete(suite, "latest_check_runs_count")
			} else {
				suite["latest_check_runs_count"] = tc.value
			}
			mapsTo(t, webhook("requested", suite), CIObservation{
				Owner: "cybermonklabs", Repo: "legion", Number: "4464", SHA: sha,
				AppID: "5341243", SuiteID: "120620537894", Status: "queued",
				ObservedAt: "2026-11-19T15:56:07Z",
			})
		})
	}
	t.Run("the shared mapping of a bare suite reads no count", func(t *testing.T) {
		obs, ok := GithubCIObservation("check_suite", captured(t, "check_suite_completed.json"), "cybermonklabs", "legion", "4464")
		if !ok || obs.SuiteID != "120620605808" || obs.CheckRunCount != nil {
			t.Fatalf("GithubCIObservation = %+v (ok=%v), want suite 120620605808 with no count", obs, ok)
		}
	})
}

// A commit status maps from the shape GitHub's REST answer lists it in, and the reconcile maps a
// status its GraphQL query read into the same shape with GraphQL's upper-case state, so both map to
// one observation. The sha and the number are the caller's; a null link or description is empty.
func TestGithubStatusObservationMapsAListedStatus(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	rest := map[string]any{
		"id":          float64(4021),
		"context":     "ci/preview",
		"state":       "failure",
		"created_at":  "2026-11-17T05:00:00Z",
		"updated_at":  "2026-11-17T05:00:00Z",
		"target_url":  "https://ci.example.com/acme/widgets/4021",
		"description": "The preview build failed",
	}
	got, ok := GithubStatusObservation(rest, "acme", "widgets", "7", sha)
	want := StatusObservation{
		Owner: "acme", Repo: "widgets", Number: "7", SHA: sha,
		Context: "ci/preview", State: "failure", CreatedAt: "2026-11-17T05:00:00Z",
		URL: "https://ci.example.com/acme/widgets/4021", Description: "The preview build failed",
	}
	if !ok || got != want {
		t.Fatalf("GithubStatusObservation(REST) = %+v, %v; want %+v", got, ok, want)
	}
	graphQL := map[string]any{
		"context":     "ci/preview",
		"state":       "FAILURE",
		"created_at":  "2026-11-17T05:00:00Z",
		"target_url":  "https://ci.example.com/acme/widgets/4021",
		"description": "The preview build failed",
	}
	if got, ok := GithubStatusObservation(graphQL, "acme", "widgets", "7", sha); !ok || got != want {
		t.Fatalf("GithubStatusObservation(GraphQL's state) = %+v, %v; want %+v", got, ok, want)
	}
	bare := map[string]any{"context": "ci/preview", "state": "success", "created_at": "2026-11-17T05:00:00Z", "target_url": nil, "description": nil}
	if got, ok := GithubStatusObservation(bare, "acme", "widgets", "7", sha); !ok || got.URL != "" || got.Description != "" || got.State != "success" {
		t.Fatalf("GithubStatusObservation(no link, no description) = %+v, %v; want a success with both empty", got, ok)
	}
}

// The record keeps a status in success, failure or error alone. GitHub's GraphQL StatusState also
// has PENDING and EXPECTED, and REST has pending: none of them maps to an observation, and neither
// does a status with no context or a head with no sha.
func TestGithubStatusObservationMapsOnlyATerminalStatus(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	status := func(context, state string) map[string]any {
		return map[string]any{"context": context, "state": state, "created_at": "2026-11-17T05:00:00Z"}
	}
	for _, state := range []string{"success", "failure", "error", "SUCCESS", "FAILURE", "ERROR"} {
		if got, ok := GithubStatusObservation(status("ci/build", state), "acme", "widgets", "7", sha); !ok || got.State != strings.ToLower(state) {
			t.Errorf("a status in %s = %+v, %v; want it mapped in %s", state, got, ok, strings.ToLower(state))
		}
	}
	for _, state := range []string{"pending", "PENDING", "EXPECTED", "expected", "", "cancelled"} {
		if got, ok := GithubStatusObservation(status("ci/build", state), "acme", "widgets", "7", sha); ok {
			t.Errorf("a status in %q mapped to %+v; want none", state, got)
		}
	}
	if got, ok := GithubStatusObservation(status("", "success"), "acme", "widgets", "7", sha); ok {
		t.Errorf("a status with no context mapped to %+v; want none", got)
	}
	if got, ok := GithubStatusObservation(status("ci/build", "success"), "acme", "widgets", "7", ""); ok {
		t.Errorf("a status of no head mapped to %+v; want none", got)
	}
	for _, state := range []string{"success", "failure", "error"} {
		if !TerminalStatusState(state) {
			t.Errorf("TerminalStatusState(%q) = false, want true", state)
		}
	}
	for _, state := range []string{"pending", "expected", "SUCCESS", ""} {
		if TerminalStatusState(state) {
			t.Errorf("TerminalStatusState(%q) = true, want false", state)
		}
	}
}

// What a status observation copies goes to the CI record, which goes to NATS whole, so each text is
// capped as a check run's is. The record is keyed by context, so a context past the cap keeps a
// digest of the whole context after its ellipsis: two contexts that share the kept text stay two
// entries, and one context keys the same entry every time.
func TestAStatusIsCappedAtTheEnvelopeTextCap(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	observe := func(t *testing.T, fields map[string]any) StatusObservation {
		t.Helper()
		object := map[string]any{"context": "ci/build", "state": "success", "created_at": "2026-11-17T05:00:00Z"}
		for key, value := range fields {
			object[key] = value
		}
		got, ok := GithubStatusObservation(object, "acme", "widgets", "7", sha)
		if !ok {
			t.Fatalf("GithubStatusObservation(%.60v) mapped to nothing", object)
		}
		return got
	}
	shared := strings.Repeat("c", 3000)
	first := observe(t, map[string]any{"context": shared + "/lint"}).Context
	second := observe(t, map[string]any{"context": shared + "/test"}).Context
	if first == second {
		t.Fatalf("two contexts sharing their first %d runes capped to one: %.60q", maxEnvelopeTextRunes, first)
	}
	if again := observe(t, map[string]any{"context": shared + "/lint"}).Context; again != first {
		t.Fatalf("one context capped two ways: %.60q and %.60q", first, again)
	}
	if first != checkName(shared+"/lint") {
		t.Fatalf("a capped context is %.60q, want it keyed as a check name is (checkName)", first)
	}
	if !strings.HasPrefix(first, strings.Repeat("c", 2048)+"… ") || len([]rune(first)) != 2048+2+16 {
		t.Fatalf("capped context %.60q (%d runes) does not keep the first 2,048 runes, an ellipsis and a digest", first, len([]rune(first)))
	}
	long := strings.Repeat("u", 600_000)
	got := observe(t, map[string]any{"target_url": long, "description": long, "created_at": long})
	for field, value := range map[string]string{"URL": got.URL, "Description": got.Description, "CreatedAt": got.CreatedAt} {
		if value != strings.Repeat("u", 2048)+"…" {
			t.Fatalf("%s has %d runes, want the first 2,048 and an ellipsis", field, len([]rune(value)))
		}
	}
}
