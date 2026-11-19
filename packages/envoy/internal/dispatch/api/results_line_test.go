package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// lineIssueKeys are the issue keys the tests below write into a reading or a check's words, each
// of a project the test's Dispatch holds: a body line that carries any of them, in any case,
// carries a private reference into a pull request.
var lineIssueKeys = []string{"resk-", "acme-", "rescq-"}

// linePrivateHosts are the hosts the tests below write into a reading or a check's words: a
// Tailscale MagicDNS host, a host below `.internal.`, and the test server's own host.
var linePrivateHosts = []string{".ts.net", ".internal.", "dispatch.example"}

// requireNoPrivateReference fails when line carries an issue key of lineIssueKeys, a host of
// linePrivateHosts or a link.
func requireNoPrivateReference(t *testing.T, line string) {
	t.Helper()
	lowered := strings.ToLower(line)
	for _, reference := range append(slices.Clone(lineIssueKeys), linePrivateHosts...) {
		if strings.Contains(lowered, reference) {
			t.Fatalf("the pull-request line carries a private reference (%s): %q", reference, line)
		}
	}
	if strings.Contains(line, "://") {
		t.Fatalf("the pull-request line carries a link: %q", line)
	}
}

// The pull-request body line goes into a pull request, which can be public, so it drops every
// word of the reading that holds an issue key of a project this Dispatch holds, in any case, or a
// link, and cuts what is left to the budget. A hyphenated word that is no such key stays: one
// whose first part names no project, and a project's name followed by a version.
func TestThePullRequestLineDropsIssueKeysAndLinksFromTheReading(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createProjects(t, handler, "ACME", "KERNEL")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	long := "The rig drove the command against a local Dispatch and the refusal named the flag it wanted, which is the whole of the case."
	for _, row := range []struct {
		name     string
		observed string
		want     string
		// control is a row whose reading holds nothing to drop: it reads the same before the
		// change as after it.
		control bool
	}{
		{
			name:     "an issue key opening the reading",
			observed: "RESK-1's open-items v14 lists every row.",
			want:     "open-items v14 lists every row.",
		},
		{
			name:     "an issue key in the middle of the reading",
			observed: "All 40 cases passed (ACME-12) on the second run.",
			want:     "All 40 cases passed on the second run.",
		},
		{
			name:     "a dispatch:// reference",
			observed: "Matched dispatch://RESK-1/spec#b-r1 line by line.",
			want:     "Matched line by line.",
		},
		{
			name:     "a dashboard URL",
			observed: "Read https://dispatch.example/issues/RESK-1/spec twice.",
			want:     "Read twice.",
		},
		{
			name:     "an issue key in lower case inside a branch name",
			observed: "The branch fix/resk-1-lines builds clean.",
			want:     "The branch builds clean.",
		},
		{
			name:     "the cut comes after the drop",
			observed: "RESK-1 ACME-2 ACME-3 RESK-4 " + long,
			want:     "The rig drove the command against a local Dispatch and the refusal named the…",
		},
		{
			name:     "a reading of nothing but issue keys and links",
			observed: "RESK-1 dispatch://RESK-1/spec ACME-2",
			want:     "The reading holds only links, hosts and issue keys, which this line leaves out.",
		},
		{
			name:     "a Tailscale MagicDNS host written without a scheme",
			observed: "Read dispatch.example-tailnet.ts.net/issues and it rendered.",
			want:     "Read and it rendered.",
		},
		{
			name:     "a host below .internal. written without a scheme",
			observed: "curl dispatch.internal.example/api/v1 answered 200.",
			want:     "curl answered 200.",
		},
		{
			name:     "this Dispatch's own host written without a scheme",
			observed: "Opened dispatch.example/projects/RESK/documents/notes again.",
			want:     "Opened again.",
		},
		{
			name:     "a public host written without a scheme stays",
			observed: "github.com/acme/widgets/pull/7 merged cleanly.",
			want:     "github.com/acme/widgets/pull/7 merged cleanly.",
			control:  true,
		},
		{
			name:     "hyphenated words whose first part names no project stay",
			observed: "UTF-8 output, SHA-256 digests, x86-64 builds and TEST-1 all match.",
			want:     "UTF-8 output, SHA-256 digests, x86-64 builds and TEST-1 all match.",
			control:  true,
		},
		{
			name:     "a project's name followed by a version stays",
			observed: "kernel-6.8 and KERNEL-6.8 both boot.",
			want:     "kernel-6.8 and KERNEL-6.8 both boot.",
			control:  true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"observed": row.observed}))
			if recorded.Code != http.StatusCreated {
				t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			line := decodeBody[recordedResult](t, recorded).Line
			want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. " + row.want
			if line != want {
				t.Fatalf("line = %q, want %q", line, want)
			}
			if !row.control {
				requireNoPrivateReference(t, line)
			}
		})
	}
}

// The line never carries the command a result ran or its negative control, so an issue key in
// either reaches no pull request through it.
func TestThePullRequestLineCarriesNoRunAndNoNegativeControl(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createProjects(t, handler, "ACME")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{
		"run":              "go test -run TestResultLine ./... # RESK-1",
		"negative_control": "Reverted ACME-12's guard and the run failed; see dispatch://RESK-1/spec.",
		"observed":         "ok",
	}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	line := decodeBody[recordedResult](t, recorded).Line
	if want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. ok"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	requireNoPrivateReference(t, line)
}

// resultDryRun is the body POST /api/v1/issues/{key}/results/dry-run answers.
type resultDryRun struct {
	Line string `json:"line"`
}

// A result's dry run answers the line the record would answer for the same body, after the
// refusals the record makes before it writes, and records nothing: no row, no event. The line
// it answers drops issue keys and links as the record's does.
func TestAResultsDryRunAnswersTheLineAndRecordsNothing(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	target := "/api/v1/issues/" + issue.Key + "/results/dry-run"
	body := resultBody("k1", "pass", headCommit, map[string]any{
		"run":              "go test ./... # RESK-1",
		"observed":         "RESK-1's open-items v14 lists every row.",
		"negative_control": "Broke RESK-1's guard and watched it fail.",
	})
	events := issueEventTypes(t, handler, issue.Key)

	dry := dispatchRequest(t, handler, http.MethodPost, target, body, "alice")
	if dry.Code != http.StatusOK {
		t.Fatalf("dry-run a result: status=%d body=%s, want 200", dry.Code, dry.Body.String())
	}
	line := decodeBody[resultDryRun](t, dry).Line
	want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. open-items v14 lists every row."
	if line != want {
		t.Fatalf("the dry run's line = %q, want %q", line, want)
	}
	requireNoPrivateReference(t, line)
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the dry run recorded %+v, want nothing", results)
	}
	if after := issueEventTypes(t, handler, issue.Key); !slices.Equal(after, events) {
		t.Fatalf("the dry run moved the issue's events from %v to %v, want them unmoved", events, after)
	}

	for _, refused := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"a check no Verification document holds", resultBody("k9", "pass", headCommit, nil), "UNKNOWN_CHECK"},
		{"a verdict that is neither pass nor fail", resultBody("k1", "maybe", headCommit, nil), "RESULT_INPUT"},
		{"a pass that counts a failed case", resultBody("k1", "pass", headCommit, map[string]any{"failed": 1}), "RESULT_CONTRADICTS"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, target, refused.body, "alice")
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != refused.code {
			t.Fatalf("the dry run of %s: status=%d body=%s, want 400 %s", refused.name, response.Code, response.Body.String(), refused.code)
		}
	}
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the refused dry runs recorded %+v, want nothing", results)
	}

	recorded := recordResult(t, handler, issue.Key, body)
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if got := decodeBody[recordedResult](t, recorded).Line; got != line {
		t.Fatalf("the record's line = %q, want the dry run's %q", got, line)
	}
}

// The line Dispatch writes for a check CI proves carries the check's words in place of a reading,
// and drops an issue key and a private host from them as a recorded line drops them from its
// reading. The check's own text, which the results read answers beside it, keeps both: only the
// line goes into a pull request. The line is written at the read, so a check whose words were
// written before this rule answers it the same way.
func TestTheCIPullRequestLineDropsIssueKeysFromTheChecksWords(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCQ", "acme/widgets", "CI line")
	words := "logs.example-tailnet.ts.net/runs shows " + issue.Key + "'s build passing at the head."
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#c1 "+ciCheckKept+"}\n"+words+"\n:::\n")
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(31, ciCheckRun, "success"))

	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	if check.Text != words {
		t.Fatalf("c1's text = %q, want its words whole, %q", check.Text, words)
	}
	if check.CI == nil {
		t.Fatalf("c1 carries no ci")
	}
	want := "- Check c1 (before_merge): pass at " + headCommit + ", from CI run " + ciCheckRun + ". shows build passing at the head."
	if check.CI.Line != want {
		t.Fatalf("c1's pull-request line = %q, want %q", check.CI.Line, want)
	}
	requireNoPrivateReference(t, check.CI.Line)
}
