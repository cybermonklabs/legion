package api

//ci:reads packages/contracts/fixtures/dispatch-results/* TestTheResultsReadOfEachState

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// updateResultsFixtures rewrites packages/contracts/fixtures/dispatch-results from this Dispatch's
// answers, which the Legion daemon's client test reads as Dispatch's results route answers them.
var updateResultsFixtures = flag.Bool("update-results-fixtures", false,
	"rewrite packages/contracts/fixtures/dispatch-results from the answers of TestTheResultsReadOfEachState")

// resultsFixtureDir is where the daemon's fixtures of the results route live, from this package.
var resultsFixtureDir = filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "dispatch-results")

// TestTheResultsReadOfEachState holds the fixtures the Legion daemon reads Dispatch's computed
// check states from (packages/contracts/fixtures/dispatch-results) to this Dispatch's own answers:
// each case builds the state on the after-merge rig, reads the route, checks that the answer is the
// state the fixture is named for, and compares it with the fixture, which -update-results-fixtures
// writes instead. An id and a time the server makes are written as a placeholder, by order of first
// appearance and by order of time, so the answer reads the same on every run. It is serial: its live
// cases raise the block schema's manifest, which parallel tests read.
func TestTheResultsReadOfEachState(t *testing.T) {
	const codeCheck = ":::check{#k5 when=\"after_merge\"}\nProduction answers at the merged commit.\n:::\n"
	liveCheck := func(program string) string {
		named := ""
		if program != "" {
			named = fmt.Sprintf(" program=%q", program)
		}
		return fmt.Sprintf(":::check{#live when=\"after_merge\" subject=\"production\"%s}\nThe running program answers.\n:::\n", named)
	}
	// merged is a code check's issue whose #70 merged into main as afterMergeM, main carrying C0
	// before it and C1 and C2 after it, whose histories hold it.
	merged := func(t *testing.T, rig *afterMergeRig) string {
		t.Helper()
		issue := rig.issue(t, "A merged change", codeCheck)
		mergedSeventy(t, rig, issue, "main", afterMergeM)
		rig.github.carry("main", afterMergeC0, afterMergeM, afterMergeC1, afterMergeC2)
		rig.github.hold(afterMergeM, afterMergeC1, afterMergeC2)
		return issue
	}
	// live is a live check's issue, merged as #101 into main as afterMergeM, with a result at C1.
	live := func(t *testing.T, rig *afterMergeRig, program string) string {
		t.Helper()
		restore := useTheNextReleasesManifest(t)
		issue := rig.issue(t, "A running program", liveCheck(program))
		restore()
		writeKnownMergeOnBase(t, rig.database, "acme/widgets", 101, headCommit, issue, "main", afterMergeM)
		rig.github.carry("main", afterMergeC0, afterMergeM, afterMergeC1, afterMergeC2)
		rig.github.hold(afterMergeM, afterMergeC1, afterMergeC2)
		rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
		return issue
	}
	for _, tc := range []struct {
		name, check, answer, reason string
		build                       func(t *testing.T, rig *afterMergeRig) string
	}{
		{name: "code-pass", check: "k5", answer: "yes", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := merged(t, rig)
			rig.record(t, issue, "k5", "tester", "pass", afterMergeC1)
			return issue
		}},
		{name: "code-fail", check: "k5", answer: "no", reason: "fail", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := merged(t, rig)
			rig.record(t, issue, "k5", "tester", "fail", afterMergeC1)
			return issue
		}},
		{name: "code-pass-then-newer-fail", check: "k5", answer: "no", reason: "fail", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := merged(t, rig)
			rig.record(t, issue, "k5", "tester", "pass", afterMergeC1)
			rig.record(t, issue, "k5", "tester", "fail", afterMergeC2)
			return issue
		}},
		{name: "code-pass-and-fail", check: "k5", answer: "unknown", reason: "pass_and_fail", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := merged(t, rig)
			rig.record(t, issue, "k5", "tester", "pass", afterMergeC1)
			rig.record(t, issue, "k5", "reviewer", "fail", afterMergeC1)
			return issue
		}},
		{name: "code-no-result", check: "k5", answer: "no", reason: "no_result", build: merged},
		{name: "code-no-merge", check: "k5", answer: "no", reason: "no_result", build: func(t *testing.T, rig *afterMergeRig) string {
			return rig.issue(t, "A change with no merge", codeCheck)
		}},
		{name: "live-no-deploy", check: "live", answer: "unknown", reason: "no_deploy", build: func(t *testing.T, rig *afterMergeRig) string {
			return live(t, rig, "renderer")
		}},
		{name: "live-no-result-from-deploy", check: "live", answer: "unknown", reason: "no_result_from_deploy", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := live(t, rig, "renderer")
			postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
			return issue
		}},
		{name: "live-program-not-named", check: "live", answer: "unknown", reason: "program_not_named", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := live(t, rig, "")
			postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
			postDeploy(t, rig.handler, deployBody("worker", afterMergeC1), http.StatusCreated)
			return issue
		}},
		{name: "live-deploy-not-merged", check: "live", answer: "unknown", reason: "deploy_not_merged", build: func(t *testing.T, rig *afterMergeRig) string {
			issue := live(t, rig, "renderer")
			postDeploy(t, rig.handler, deployBody("renderer", afterMergeC0), http.StatusCreated)
			return issue
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAfterMergeRig(t)
			issue := tc.build(t, rig)
			response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
			}
			var read struct {
				Checks []struct {
					BlockID    string `json:"block_id"`
					PassStands struct {
						Answer string `json:"answer"`
						Reason string `json:"reason"`
					} `json:"pass_stands"`
				} `json:"checks"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
				t.Fatalf("decode the results of %s: %v", issue, err)
			}
			index := slices.IndexFunc(read.Checks, func(check struct {
				BlockID    string `json:"block_id"`
				PassStands struct {
					Answer string `json:"answer"`
					Reason string `json:"reason"`
				} `json:"pass_stands"`
			}) bool {
				return check.BlockID == tc.check
			})
			if index < 0 || read.Checks[index].PassStands.Answer != tc.answer || read.Checks[index].PassStands.Reason != tc.reason {
				t.Fatalf("the answer of %s is not the state %s names, %s (reason %q): %s", tc.check, tc.name, tc.answer, tc.reason, response.Body.String())
			}
			holdResultsFixture(t, tc.name, response.Body.Bytes())
		})
	}
	t.Run("issue-with-a-backlog-child", func(t *testing.T) {
		rig := newAfterMergeRig(t)
		issue := rig.issue(t, "A parent", codeCheck)
		// A new issue starts in triage whatever its create names, so the child is moved to backlog
		// as a person files one there.
		child := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": afterMergeProject, "title": "A child", "spec": "A spec\n", "parent": issue, "force": true}, "alice")
		if child.Code != http.StatusCreated {
			t.Fatalf("create the child of %s: status=%d body=%s", issue, child.Code, child.Body.String())
		}
		childKey := decodeBody[struct {
			Key string `json:"key"`
		}](t, child).Key
		if moved := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+childKey, map[string]any{"status": "backlog"}, "alice"); moved.Code != http.StatusOK {
			t.Fatalf("move %s to backlog: status=%d body=%s", childKey, moved.Code, moved.Body.String())
		}
		response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", issue, response.Code, response.Body.String())
		}
		var read struct {
			Children []struct {
				Status string `json:"status"`
			} `json:"children"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil || len(read.Children) != 1 || read.Children[0].Status != "backlog" {
			t.Fatalf("the read of %s lists the children %+v (%v), want one in backlog: %s", issue, read.Children, err, response.Body.String())
		}
		holdResultsFixture(t, "issue-with-a-backlog-child", response.Body.Bytes())
	})
	// wantAnswer reads issue's results through handler (with query) and fails t unless check
	// answers answer for reason, and answers the body.
	wantAnswer := func(t *testing.T, handler http.Handler, issue, query, check, answer, reason string) []byte {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read the results of %s%s: status=%d body=%s", issue, query, response.Code, response.Body.String())
		}
		var read struct {
			Checks []struct {
				BlockID    string `json:"block_id"`
				PassStands struct {
					Answer string `json:"answer"`
					Reason string `json:"reason"`
				} `json:"pass_stands"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
			t.Fatalf("decode the results of %s: %v", issue, err)
		}
		for _, got := range read.Checks {
			if got.BlockID == check && got.PassStands.Answer == answer && got.PassStands.Reason == reason {
				return response.Body.Bytes()
			}
		}
		t.Fatalf("%s of %s%s does not answer %s (reason %q): %s", check, issue, query, answer, reason, response.Body.String())
		return nil
	}
	t.Run("code-linked-merge-only", func(t *testing.T) {
		const (
			toolsMerge = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
			toolsHead  = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
		)
		linked := &linkedGitHub{
			held:  map[string][]string{"acme/tools": {toolsMerge, toolsHead}, "acme/widgets": {}},
			pulls: map[string]string{"acme/tools#9": "merged"},
			merged: map[string]linkedMerge{"acme/tools#9": {title: "The work in a second repository", headRef: "work", headSHA: toolsHead,
				base: "main", mergeCommit: toolsMerge, mergedAt: "2026-11-20T12:00:00Z"}},
			branches: map[string][]string{"acme/tools:main": {toolsMerge}},
		}
		handler, database := linkedRepositoriesGitHub(t, linked)
		issue := issueInRepository(t, handler, afterMergeProject, "acme/widgets", "Work merged in the second repository").Key
		uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", afterMergeAndDefaultChecks)
		linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/9")
		writeHeadOnBase(t, database, "acme/tools", 9, toolsHead, "merged", "", "main")
		recordAfterMerge(t, handler, issue, "k4", "tester", "pass", toolsMerge)
		holdResultsFixture(t, "code-linked-merge-only", wantAnswer(t, handler, issue, "", "k4", "yes", ""))
	})
	// integration is #70 merged into the integration branch legion/ACME-0 as m1, which main
	// reaches through #5 as m2 where landed says so, with a result at onMain.
	integration := func(t *testing.T, landed bool) (*afterMergeRig, string) {
		t.Helper()
		const (
			branch = "legion/ACME-0"
			m1     = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
			m2     = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
			f5     = "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5"
			onMain = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
		)
		rig := newAfterMergeRig(t)
		issue := rig.issue(t, "Work on an integration branch", afterMergeChecks)
		rig.github.carry(branch, m1, f5)
		rig.github.carry("main", m2, onMain)
		rig.github.hold(m1, f5, m2, onMain)
		rig.github.hold(m2, onMain)
		mergedSeventy(t, rig, issue, branch, m1)
		if landed {
			mergedAt := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
			rig.github.setPull(5, afterMergePull("ACME-0", 5, f5, branch, "main", m2, mergedAt.Format(time.RFC3339)), 0)
			if err := delivery.UpsertPullRequestHead(context.Background(), rig.database.Pool, nil, delivery.PullRequestHead{
				Repo: "acme/widgets", Number: 5, HeadSHA: f5, HeadRef: branch, BaseRef: "main", State: "merged",
				Title: "Land the integration branch", URL: "https://github.com/acme/widgets/pull/5", MergedAt: &mergedAt,
			}); err != nil {
				t.Fatalf("store the merge of #5: %v", err)
			}
		}
		rig.record(t, issue, "k6", "tester", "pass", onMain)
		return rig, issue
	}
	t.Run("default-branch-merged-elsewhere-not-landed", func(t *testing.T) {
		rig, issue := integration(t, false)
		holdResultsFixture(t, "default-branch-merged-elsewhere-not-landed", wantAnswer(t, rig.handler, issue, "", "k6", "no", "no_result"))
	})
	t.Run("default-branch-merged-elsewhere-landed", func(t *testing.T) {
		rig, issue := integration(t, true)
		holdResultsFixture(t, "default-branch-merged-elsewhere-landed", wantAnswer(t, rig.handler, issue, "", "k6", "yes", ""))
	})
	t.Run("before-merge-second-pull-request-open", func(t *testing.T) {
		rig := newAfterMergeRig(t)
		issue := rig.issue(t, "A change with a second pull request", ":::check{#k1 when=\"before_merge\"}\nThe change answers at its head.\n:::\n")
		writeKnownMergeOnBase(t, rig.database, "acme/widgets", 101, headCommit, issue, "main", afterMergeM)
		writeHeadOnBase(t, rig.database, "acme/widgets", 102, newHeadCommit, "open", issue, "main")
		rig.record(t, issue, "k1", "tester", "pass", headCommit)
		holdResultsFixture(t, "before-merge-second-pull-request-open", wantAnswer(t, rig.handler, issue, "", "k1", "no", ""))
		holdResultsFixture(t, "before-merge-at-first-head", wantAnswer(t, rig.handler, issue, "?at="+headCommit, "k1", "yes", ""))
	})
}

// fixtureUUID and fixtureTime are the ids and the times a server makes, which a fixture writes as a
// placeholder.
var (
	fixtureUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	fixtureTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
)

// holdResultsFixture compares answer, with its ids and times written as placeholders, with the
// fixture name, or writes it there under -update-results-fixtures.
func holdResultsFixture(t *testing.T, name string, answer []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(answer, &decoded); err != nil {
		t.Fatalf("decode the answer of %s: %v", name, err)
	}
	ids, times := map[string]string{}, []time.Time{}
	collectFixtureTimes(decoded, &times)
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	times = slices.CompactFunc(times, func(a, b time.Time) bool { return a.Equal(b) })
	stable := placeFixtureValues(decoded, ids, times)
	var written bytes.Buffer
	encoder := json.NewEncoder(&written)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(stable); err != nil {
		t.Fatalf("encode the fixture %s: %v", name, err)
	}
	path := filepath.Join(resultsFixtureDir, name+".json")
	if *updateResultsFixtures {
		if err := os.MkdirAll(resultsFixtureDir, 0o755); err != nil {
			t.Fatalf("make %s: %v", resultsFixtureDir, err)
		}
		if err := os.WriteFile(path, written.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update-results-fixtures to write it)", path, err)
	}
	if !bytes.Equal(held, written.Bytes()) {
		t.Fatalf("%s is not this Dispatch's answer; run with -update-results-fixtures.\nheld:\n%s\nanswered:\n%s", path, held, written.Bytes())
	}
}

// collectFixtureTimes adds each time value holds to times.
func collectFixtureTimes(value any, times *[]time.Time) {
	switch value := value.(type) {
	case map[string]any:
		for _, member := range value {
			collectFixtureTimes(member, times)
		}
	case []any:
		for _, element := range value {
			collectFixtureTimes(element, times)
		}
	case string:
		if fixtureTime.MatchString(value) {
			if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
				*times = append(*times, at)
			}
		}
	}
}

// placeFixtureValues is value with each id a placeholder by its first appearance and each time a
// placeholder by its rank among times, so the order of two times is kept.
func placeFixtureValues(value any, ids map[string]string, times []time.Time) any {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		placed := make(map[string]any, len(value))
		for _, key := range keys {
			placed[key] = placeFixtureValues(value[key], ids, times)
		}
		return placed
	case []any:
		placed := make([]any, len(value))
		for index, element := range value {
			placed[index] = placeFixtureValues(element, ids, times)
		}
		return placed
	case string:
		if fixtureUUID.MatchString(value) {
			if _, held := ids[value]; !held {
				ids[value] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(ids)+1)
			}
			return ids[value]
		}
		if fixtureTime.MatchString(value) {
			if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
				rank := slices.IndexFunc(times, func(other time.Time) bool { return other.Equal(at) })
				return time.Date(2026, 1, 1, 0, 0, rank, 0, time.UTC).Format(time.RFC3339)
			}
		}
		return value
	default:
		return value
	}
}
