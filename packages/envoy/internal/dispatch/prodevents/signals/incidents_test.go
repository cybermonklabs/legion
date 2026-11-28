package signals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	project, _, _ := strings.Cut(issue, "-")
	payload := map[string]any{"key": issue, "project": project, "labels": labels, "external_links": external}
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

// TestAnIncidentLinkNamesAPullRequestNumberOfAtMostNineDigits: a pull request's number is read
// against an integer column (delivery_pull_requests.number), so a link names one of at most nine
// digits, as a linked pull request does (delivery's linked_pull_requests.go): a nine-digit number
// is read, and a ten-digit one is no pull request's link, whose read would fail every pass and
// hold the incidents cursor where it stands.
func TestAnIncidentLinkNamesAPullRequestNumberOfAtMostNineDigits(t *testing.T) {
	t.Parallel()
	if got, ok := parseLink("https://github.com/acme/widgets/pull/999999999"); !ok || got != (linkTarget{repo: "acme/widgets", pull: 999999999}) {
		t.Errorf("parseLink of a nine-digit pull request = %+v, %v; want acme/widgets #999999999", got, ok)
	}
	for _, link := range []string{"https://github.com/acme/widgets/pull/1000000000", "https://github.com/acme/widgets/pull/99999999999999999999/files"} {
		if got, ok := parseLink(link); ok {
			t.Errorf("parseLink(%q) = %+v, want no target", link, got)
		}
	}
}

// TestAnIncidentBlamesNoDeployAnotherProjectPostedAndNeedsACountingDeploy: a deploy posted to
// BETA that names acme/widgets#11 and finished first is never blamed by ALPHA's incident linking
// #11, which blames ALPHA's GitHub deploy of #11 instead; BETA's own incident linking #11 blames
// BETA's post. And a BETA incident linking ALPHA's run 311, whose deploy counts for ALPHA alone,
// writes no row.
func TestAnIncidentBlamesNoDeployAnotherProjectPostedAndNeedsACountingDeploy(t *testing.T) {
	r := newRig(t)
	r.project("BETA", "acme/beta")
	r.pullRequest(11, at(30), sha("b"), "")
	r.deployRun(310, at(60), sha("a"), prodJob, success, success)
	r.deployRun(311, at(120), sha("c"), prodJob, success, success)
	r.passDeploys()
	shipped := r.deployOf(310)
	betaPost := r.postDeploy("BETA", "ci:beta-11", at(40), nil, []string{"acme/widgets#11"})

	const pullLink = "https://github.com/acme/widgets/pull/11"
	const betaPullLink = "https://github.com/acme/widgets/pull/11/files"
	const runLink = "https://github.com/acme/widgets/actions/runs/311"
	alphaIncident := r.issue(20, "incident")
	r.link(alphaIncident, pullLink)
	r.issueEvent(alphaIncident, "issue.created", []string{"incident"}, []string{pullLink}, nil)
	betaOwn := r.issueOf("BETA", 1, "incident")
	r.link(betaOwn, betaPullLink)
	r.issueEvent(betaOwn, "issue.created", []string{"incident"}, []string{betaPullLink}, nil)
	betaForeign := r.issueOf("BETA", 2, "incident")
	r.link(betaForeign, runLink)
	r.issueEvent(betaForeign, "issue.created", []string{"incident"}, []string{runLink}, nil)
	r.passEvents()

	if rows := r.failures(); len(rows) != 1 || rows[0].SourceRef != IncidentSourceRef(alphaIncident) || *rows[0].DeployID != shipped.ID {
		t.Fatalf("ALPHA's incident rows = %+v, want one blaming ALPHA's deploy %s rather than BETA's earlier post", rows, shipped.ID)
	}
	betaRows, _, err := prodevents.List(r.ctx, r.pool, prodevents.ListQuery{
		Project: "BETA", From: base.Add(-365 * 24 * time.Hour), To: time.Now().Add(24 * time.Hour), Kind: prodevents.KindFailure, Limit: 100,
	})
	if err != nil {
		t.Fatalf("read BETA's failures: %v", err)
	}
	if len(betaRows) != 1 || betaRows[0].SourceRef != IncidentSourceRef(betaOwn) || *betaRows[0].DeployID != betaPost.ID {
		t.Fatalf("BETA's incident rows = %+v, want only %s blaming BETA's post %s (and no row for %s linking ALPHA's run)",
			betaRows, betaOwn, betaPost.ID, betaForeign)
	}
}

// TestARefusedIncidentStaysRefusedThroughASourceMoveAndBack (the architect's ruling,
// dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706): a BETA incident whose link
// names ALPHA's deploy run 312, which ships no pull request and so counts for ALPHA alone, is
// refused on that deploy, logged once, and it stays refused. When acme/widgets's one source moves
// from ALPHA to BETA, so run 312's row counts for BETA, a later event of the issue derives the
// incident again from its links, a label it gains and then a link to the run's job page, and writes
// no row; when the source moves back, the record holds no failure row of any project. The refusal
// is the deploy's alone: a link to a pull request whose deploy BETA posted, which finished first and
// counts for BETA, then writes the incident's row, blaming that deploy.
func TestARefusedIncidentStaysRefusedThroughASourceMoveAndBack(t *testing.T) {
	r := newRig(t)
	refusals := r.logRefusals()
	r.project("BETA", "acme/beta")
	r.deployRun(312, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(312)
	const runLink = "https://github.com/acme/widgets/actions/runs/312"
	const jobLink = "https://github.com/acme/widgets/actions/runs/312/job/9"
	incident := r.issueOf("BETA", 1, "incident")
	r.link(incident, runLink)
	r.issueEvent(incident, "issue.created", []string{"incident"}, []string{runLink}, nil)
	pass := func() {
		t.Helper()
		r.passDeploys()
		r.passEvents()
	}
	failureRows := func() int {
		t.Helper()
		var rows int
		if err := r.pool.QueryRow(r.ctx, `select count(*) from production_events where kind = 'failure'`).Scan(&rows); err != nil {
			t.Fatalf("count the failure rows: %v", err)
		}
		return rows
	}
	requireRefused := func(step, owner string) {
		t.Helper()
		if moved := r.deployOf(312); moved.ID != deploy.ID || moved.ProjectKey != owner {
			t.Fatalf("%s, run 312's row is %s of %s; want %s of %s", step, moved.ID, moved.ProjectKey, deploy.ID, owner)
		}
		var refused bool
		if err := r.pool.QueryRow(r.ctx, `select exists (select from incident_refusals where issue_key = $1 and deploy_id = $2::uuid)`,
			incident, deploy.ID).Scan(&refused); err != nil || !refused {
			t.Errorf("%s, %s holds a refusal of run 312's deploy %t (%v); want it held", step, incident, refused, err)
		}
		if rows := failureRows(); rows != 0 {
			t.Errorf("%s, the record holds %d failure rows across every project; want none", step, rows)
		}
	}

	pass()
	requireRefused("under ALPHA's source", alpha)
	r.deliveryOff(alpha)
	r.saveSource("BETA", new(widgets), revertsSince, prodJob)
	pass()
	requireRefused("once the source moved to BETA", "BETA")
	r.issueEvent(incident, "issue.updated", []string{"incident", "sev1"}, []string{runLink}, []string{"labels"})
	pass()
	requireRefused("once the incident gained a label under BETA's source", "BETA")
	r.link(incident, jobLink)
	r.issueEvent(incident, "issue.updated", []string{"incident", "sev1"}, []string{runLink, jobLink}, []string{"external_links"})
	pass()
	requireRefused("once the incident gained a link to the run's job under BETA's source", "BETA")
	r.deliveryOff("BETA")
	r.saveRule(prodJob)
	pass()
	requireRefused("once the source moved back to ALPHA", alpha)

	const pullLink = "https://github.com/acme/widgets/pull/13"
	betaPost := r.postDeploy("BETA", "ci:beta-13", at(30), nil, []string{"acme/widgets#13"})
	r.link(incident, pullLink)
	r.issueEvent(incident, "issue.updated", []string{"incident", "sev1"}, []string{runLink, jobLink, pullLink}, []string{"external_links"})
	pass()
	rows, _, err := prodevents.List(r.ctx, r.pool, prodevents.ListQuery{
		Project: "BETA", From: base.Add(-365 * 24 * time.Hour), To: time.Now().Add(24 * time.Hour), Kind: prodevents.KindFailure, Limit: 100,
	})
	if err != nil {
		t.Fatalf("read BETA's failures: %v", err)
	}
	if len(rows) != 1 || rows[0].SourceRef != IncidentSourceRef(incident) || *rows[0].DeployID != betaPost.ID || failureRows() != 1 {
		t.Fatalf("once the incident linked a pull request of BETA's post, BETA's failure rows = %+v; want the one incident row blaming %s",
			rows, betaPost.ID)
	}
	if logged, want := refusals(), []string{prodevents.SignalIncidentIssue + " " + incident + " " + deploy.ID}; !slices.Equal(logged, want) {
		t.Errorf("the refusals logged = %q; want %q, once", logged, want)
	}
}

// TestAnIncidentWhoseDeployCountsForNoProjectIsLoggedAndAskedAgain (the architect's ruling,
// dispatch://INFRA-978/comment/527e19cd-344b-4a20-9a4b-b7d515da91b0, for the incident signal): ALPHA
// gives up the source that found run 320's deploy, which ships no pull request, so that deploy, kept
// and not dropped, counts for no project. An incident linking the run then writes no row and no
// refusal, and logs the unmatched line naming the issue and the deploy. Once ALPHA's source is back,
// a later event of the incident writes its row, blaming that deploy.
func TestAnIncidentWhoseDeployCountsForNoProjectIsLoggedAndAskedAgain(t *testing.T) {
	r := newRig(t)
	lines := r.logLines()
	r.deployRun(320, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(320)
	r.deliveryOff(alpha)
	r.passDeploys()
	const runLink = "https://github.com/acme/widgets/actions/runs/320"
	incident := r.issue(20, "incident")
	r.link(incident, runLink)
	r.issueEvent(incident, "issue.created", []string{"incident"}, []string{runLink}, nil)
	r.passEvents()
	if rows := r.failures(); len(rows) != 0 {
		t.Fatalf("with no source reading acme/widgets, failure rows = %+v; want none", rows)
	}
	var refused int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from incident_refusals`).Scan(&refused); err != nil || refused != 0 {
		t.Errorf("the record holds %d incident refusals (%v); want none", refused, err)
	}
	if logged, want := lines("", "issue", "deploy"), []string{incident + " " + deploy.ID}; !slices.Equal(logged, want) {
		t.Errorf("the unmatched lines logged = %q; want %q", logged, want)
	}
	if logged := lines(refusedMessage, "key"); len(logged) != 0 {
		t.Errorf("the refusals logged = %q; want none", logged)
	}

	r.saveRule(prodJob)
	r.passDeploys()
	r.issueEvent(incident, "issue.updated", []string{"incident", "sev1"}, []string{runLink}, []string{"labels"})
	r.passEvents()
	if rows := r.failures(); len(rows) != 1 || rows[0].SourceRef != IncidentSourceRef(incident) || *rows[0].DeployID != deploy.ID {
		t.Fatalf("once ALPHA's source is back, a later event of the incident leaves failure rows %+v; want the incident's, blaming %s",
			rows, deploy.ID)
	}
}
