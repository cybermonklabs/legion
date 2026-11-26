package signals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// issueEvent appends issue.created or issue.updated on issue as person, its payload holding the
// issue's key, labels and links as the issue routes write them (model.IssueEventPayload) and the
// fields an update changed.
func (r *rig) issueEvent(issue, kind string, labels, links, changed []string) {
	r.t.Helper()
	external := make([]map[string]string, len(links))
	for i, link := range links {
		external[i] = map[string]string{"url": link, "kind": "url"}
	}
	payload := map[string]any{"key": issue, "project": alpha, "labels": labels, "external_links": external}
	if changed != nil {
		payload["changed"] = changed
	}
	r.event(issue, kind, person, payload)
}

// link adds url to issue's own links.
func (r *rig) link(issue, url string) {
	r.t.Helper()
	r.exec("link "+url, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`, issue, url)
}

// TestAnIncidentIssueMakesOneRowAndABugIssueNone: an issue created with the incident label and a
// link to a pull request is one counted incident_issue row of the issue's project, blaming the
// deploy that shipped the pull request and naming it; a repeat writes no second row. An issue
// labelled bug whose link names a deploy run makes none. An incident issue with no link makes none
// until a link to a deploy run is added, which makes its row then, blaming that run's deploy row.
func TestAnIncidentIssueMakesOneRowAndABugIssueNone(t *testing.T) {
	r := newRig(t)
	r.pullRequest(11, at(30), sha("b"), "")
	r.deployRun(300, at(60), sha("a"), prodJob, success, success)
	r.deployRun(301, at(120), sha("c"), prodJob, success, success)
	r.passDeploys()
	shipped, later := r.deployOf(300), r.deployOf(301)

	incident := r.issue(10, "incident")
	r.link(incident, "https://github.com/acme/widgets/pull/11")
	r.issueEvent(incident, "issue.created", []string{"incident"}, []string{"https://github.com/acme/widgets/pull/11"}, nil)
	bug := r.issue(11, "bug")
	r.link(bug, "https://github.com/acme/widgets/actions/runs/301")
	r.issueEvent(bug, "issue.created", []string{"bug"}, []string{"https://github.com/acme/widgets/actions/runs/301"}, nil)
	unlinked := r.issue(12, "incident")
	r.issueEvent(unlinked, "issue.created", []string{"incident"}, nil, nil)
	r.passEvents()

	rows := r.failures()
	if len(rows) != 1 {
		t.Fatalf("failure rows = %+v, want the incident's one", rows)
	}
	row := rows[0]
	if row.ProjectKey != alpha || row.Source != prodevents.SourceIncidentLabel || *row.Signal != prodevents.SignalIncidentIssue ||
		row.SourceRef != "incident:"+incident || *row.DeployID != shipped.ID || row.Actor != person ||
		!slices.Equal(row.PullRequests, []string{"acme/widgets#11"}) {
		t.Fatalf("incident row = %+v", row)
	}
	if again, err := recordIncident(r.ctx, r.pool, incident, *row.DetectedAt, person); err != nil || again {
		t.Fatalf("a repeat of the incident = %v, %v; want no second row", again, err)
	}

	r.link(unlinked, "https://github.com/acme/widgets/actions/runs/301/job/9")
	r.issueEvent(unlinked, "issue.updated", []string{"incident"}, []string{"https://github.com/acme/widgets/actions/runs/301/job/9"},
		[]string{"external_links"})
	r.passEvents()
	rows = r.failures()
	if len(rows) != 2 {
		t.Fatalf("after the link was added, failure rows = %+v, want two", rows)
	}
	var added prodevents.Event
	for _, candidate := range rows {
		if candidate.SourceRef == "incident:"+unlinked {
			added = candidate
		}
	}
	if added.DeployID == nil || *added.DeployID != later.ID || !slices.Equal(added.PullRequests, later.PullRequests) {
		t.Fatalf("the row the added link made = %+v, want it blaming run 301's deploy %s", added, later.ID)
	}
}

// TestTheIncidentLinkParseReadsGitHubsOwnURLs: the links an incident names are read from GitHub's
// own answers, captured into delivery/testdata/github: the html_url of the revert-button pull
// request, of the pull request it reverts, and of a workflow run; and the job page the revert's
// body links under that run's own path.
func TestTheIncidentLinkParseReadsGitHubsOwnURLs(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "delivery", "testdata", "github", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var answer struct {
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return answer.HTMLURL
	}
	for _, c := range []struct {
		link string
		want linkTarget
	}{
		{read("revert-pull-cli-14462.json"), linkTarget{repo: "cli/cli", pull: 14462}},
		{read("reverted-pull-cli-14437.json"), linkTarget{repo: "cli/cli", pull: 14437}},
		{read("run-legion-45089342987.json"), linkTarget{repo: "cybermonklabs/legion", run: 45089342987}},
		{"https://github.com/cli/cli/actions/runs/34978056515/job/104410855926", linkTarget{repo: "cli/cli", run: 34978056515}},
	} {
		got, ok := parseLink(c.link)
		if !ok || got != c.want {
			t.Errorf("parseLink(%q) = %+v, %v; want %+v", c.link, got, ok, c.want)
		}
	}
	for _, link := range []string{"https://github.com/cli/cli", "https://github.com/cli/cli/issues/14437",
		"https://example.com/cli/cli/pull/1", "https://github.com/cli/cli/actions/runs/x", "not a link"} {
		if got, ok := parseLink(link); ok {
			t.Errorf("parseLink(%q) = %+v, want no target", link, got)
		}
	}
}
