package api

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedDeliveryIssueFacts seeds what the timeline joins a pull request's issue against: project
// ACME, its component tree from two sources (platform > api from acme/arch, and docs from
// acme/docs), ACME-1 (P0, attached to api), its child ACME-2 (P2, attaching nothing of its own,
// so it inherits api), ACME-3 (no priority, attached to none with a reason), and ACME-4, another
// child of ACME-1 with no priority and no attachment of its own, so it takes P0 from ACME-1 as it
// inherits api from it. ACME's default repository is acme/widgets (projects.default_repo), which an
// issue with no repository of its own resolves to. ACME-3's own repository is acme/other, so each
// issue resolves to the repository whose pull requests name it.
func seedDeliveryIssueFacts(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := t.Context()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee, default_repo) values ('ACME', 'Acme', 'alice', 'acme/widgets')
			on conflict (key) do update set default_repo = excluded.default_repo`,
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by) values
			('ACME', 1, 'acme/arch', 'main', 1, '{"kind":"system","id":"test"}'),
			('ACME', 2, 'acme/docs', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (900, 'ACME', 'c1', '{}'), (901, 'ACME', 'd1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('ACME', 'platform', 'The platform', '', null, 900, 1),
			('ACME', 'api', 'Public API', '', 'platform', 900, 1),
			('ACME', 'docs', 'Documentation', '', null, 901, 2)`,
		`insert into issues (key, project_key, number, title, status, priority, repo, created_by, rank) values
			('ACME-1', 'ACME', 1, 'Ship widgets', 'todo', 0, null, '{"kind":"system","id":"test"}', 'U'),
			('ACME-3', 'ACME', 3, 'Tidy docs', 'todo', null, 'acme/other', '{"kind":"system","id":"test"}', 'W')`,
		`insert into issues (key, project_key, number, title, status, priority, parent_key, created_by, rank) values
			('ACME-2', 'ACME', 2, 'Widget child', 'todo', 2, 'ACME-1', '{"kind":"system","id":"test"}', 'V'),
			('ACME-4', 'ACME', 4, 'Follow-up fix', 'todo', null, 'ACME-1', '{"kind":"system","id":"test"}', 'X')`,
		`insert into issue_components (issue_key, mode, reason) values ('ACME-1', 'explicit', null), ('ACME-3', 'none', 'chore')`,
		`insert into issue_component_members (issue_key, project_key, component_id) values ('ACME-1', 'ACME', 'api')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed issue facts: %v\n%s", err, statement)
		}
	}
}

// seedAttributedHead stores the results row and the texts of repo#number, so that the statement
// gives the pull request the issue from its branch, legion/<issue>. The texts hold the commit
// messages as read, and the messages name no key. The issue must exist and resolve to repo. The
// helper fails the test where the statement gives another answer. It writes no timeline row.
// attributor is the server's (Deps.Attributor), so the row is written and read under the rename
// pairs the server reads by.
func seedAttributedHead(t *testing.T, attributor *delivery.Attributor, pool *store.Pool, repo string, number int, issue string) {
	t.Helper()
	ctx := t.Context()
	at := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	title := fmt.Sprintf("pull request %d", number)
	if err := attributor.UpsertPullRequestHead(ctx, pool, delivery.PullRequestHead{
		Repo: repo, Number: number, HeadSHA: fmt.Sprintf("%040x", number), HeadRef: "legion/" + issue, BaseRef: "main",
		State: "merged", Title: title, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), EventAt: &at,
		Texts: &delivery.PullRequestTexts{HeadRepo: repo, BaseRef: "main", Title: title, CommitMessages: []string{}, MessagesRead: true},
	}); err != nil {
		t.Fatalf("store the results row of %s#%d with its texts: %v", repo, number, err)
	}
	key := delivery.PullRequestKey{Repo: repo, Number: number}
	answers, err := attributor.PullRequestIssues(ctx, pool, []delivery.PullRequestKey{key})
	if err != nil {
		t.Fatalf("PullRequestIssues(%s#%d): %v", repo, number, err)
	}
	answer, found := answers[key]
	if !found || answer.IssueKey == nil || *answer.IssueKey != issue || !answer.Known {
		t.Fatalf("seedAttributedHead(%s#%d, %s): the statement answers %q from %q (found %t, known %t), want %s: the issue must exist and resolve to %s",
			repo, number, issue, deref(answer.IssueKey), answer.Source, found, answer.Known, issue, repo)
	}
}

// TestGetDeliveryTimelineJoinsIssueFactsAndCountsFacets proves each PR carries its issue's title,
// P0-P3 priority and effective components (inherited from the nearest ancestor that chose, as the
// issue read resolves them), that the priority is the one the issue resolves to (ACME-4, with none
// of its own, takes P0 from ACME-1, which its row names in priority_from, and is selected and
// counted under P0), that the component facet's parent selection includes its children, and that
// facet_counts count each facet with every other facet and the search applied and its own
// selection ignored, while color_counts ignore every facet.
func TestGetDeliveryTimelineJoinsIssueFactsAndCountsFacets(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat", "hubot"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	seedDeliveryIssueFacts(t, database)

	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	key := func(k string) *string { return &k }
	for _, seed := range []struct {
		pr    delivery.DeliveryPullRequest
		issue string
	}{
		{delivery.DeliveryPullRequest{Repo: "acme/widgets", Number: 1, Title: "feat: ship the api", Author: "octocat"}, "ACME-1"},
		{delivery.DeliveryPullRequest{Repo: "acme/widgets", Number: 2, Title: "fix: child widget", Author: "hubot", Rework: true}, "ACME-2"},
		{delivery.DeliveryPullRequest{Repo: "acme/widgets", Number: 5, Title: "fix: follow up the launch", Author: "octocat"}, "ACME-4"},
		{delivery.DeliveryPullRequest{Repo: "acme/other", Number: 3, Title: "docs: tidy", Author: "octocat"}, "ACME-3"},
		{delivery.DeliveryPullRequest{Repo: "acme/other", Number: 4, Title: "chore: no issue", Author: "hubot"}, ""},
	} {
		pr := seed.pr
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
		at := merged.Add(time.Duration(pr.Number) * time.Minute)
		pr.MergedAt, pr.CreatedAt = &at, &at
		if err := delivery.UpsertPullRequest(ctx, database.Pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
		if seed.issue != "" {
			seedAttributedHead(t, deps.Attributor, database.Pool, pr.Repo, pr.Number, seed.issue)
		}
	}
	window := "from=" + merged.Add(-time.Hour).Format(time.RFC3339) + "&to=" + merged.Add(time.Hour).Format(time.RFC3339)

	body := getDeliveryTimeline(t, handler, window)
	byID := map[string]delivery.DeliveryPRView{}
	for _, pr := range body.PRs {
		byID[pr.ID] = pr
	}
	for id, want := range map[string]struct {
		title, priority, from *string
		components            []string
	}{
		"acme/widgets#1": {key("Ship widgets"), key("P0"), nil, []string{"ACME/api"}},
		"acme/widgets#2": {key("Widget child"), key("P2"), nil, []string{"ACME/api"}},
		"acme/widgets#5": {key("Follow-up fix"), key("P0"), key("ACME-1"), []string{"ACME/api"}},
		"acme/other#3":   {key("Tidy docs"), nil, nil, []string{}},
		"acme/other#4":   {nil, nil, nil, []string{}},
	} {
		got := byID[id]
		if !reflect.DeepEqual(got.IssueTitle, want.title) || !reflect.DeepEqual(got.Priority, want.priority) ||
			!reflect.DeepEqual(got.PriorityFrom, want.from) || !reflect.DeepEqual(got.Components, want.components) {
			t.Errorf("%s: issue_title=%v priority=%v priority_from=%v components=%v, want %v %v %v %v", id,
				deref(got.IssueTitle), deref(got.Priority), deref(got.PriorityFrom), got.Components,
				deref(want.title), deref(want.priority), deref(want.from), want.components)
		}
	}
	wantComponents := map[string]delivery.DeliveryComponentView{
		"ACME/platform": {Title: "The platform"},
		"ACME/api":      {Title: "Public API", Parent: key("ACME/platform")},
		"ACME/docs":     {Title: "Documentation"},
	}
	if !reflect.DeepEqual(body.Components, wantComponents) {
		t.Errorf("components = %+v, want %+v", body.Components, wantComponents)
	}
	if want := map[string]string{"ACME-1": "Ship widgets", "ACME-2": "Widget child", "ACME-3": "Tidy docs", "ACME-4": "Follow-up fix"}; !reflect.DeepEqual(body.IssueTitles, want) {
		t.Errorf("issue_titles = %v, want %v", body.IssueTitles, want)
	}

	// A parent component's selection includes its children; the placeholders select by cause.
	for query, want := range map[string][]string{
		"component=ACME/platform":                  {"acme/widgets#1", "acme/widgets#2", "acme/widgets#5"},
		"component=__no_component__":               {"acme/other#3"},
		"component=__no_issue__":                   {"acme/other#4"},
		"priority=__no_priority__":                 {"acme/other#3"},
		"priority=P0":                              {"acme/widgets#1", "acme/widgets#5"},
		"priority=p2&priority=__no_issue__":        {"acme/other#4", "acme/widgets#2"},
		"parent_agent=__no_session__&author=hubot": {"acme/other#4", "acme/widgets#2"},
		"q=CHILD":       {"acme/widgets#2"},
		"q=widgets%233": {},
		"q=other%233":   {"acme/other#3"},
	} {
		got := deliveryPRIDs(getDeliveryTimeline(t, handler, window+"&"+query).PRs)
		if !slices.Equal(got, want) {
			t.Errorf("%s: prs = %v, want %v", query, got, want)
		}
	}

	// repo is selected, so its own counts still name both repositories while author counts only
	// acme/widgets' PRs; the search applies to every count.
	counted := getDeliveryTimeline(t, handler, window+"&repo=acme/widgets")
	wantCounts := map[string]map[string]int{
		"repo":         {"acme/widgets": 3, "acme/other": 2},
		"parent_agent": {"__no_session__": 3},
		"session":      {},
		"issue":        {"ACME-1": 1, "ACME-2": 1, "ACME-4": 1},
		"priority":     {"P0": 2, "P2": 1},
		"component":    {"ACME/api": 3},
		"author":       {"octocat": 2, "hubot": 1},
		"rework":       {"value": 2, "rework": 1},
		"deployed":     {"waiting": 3},
	}
	if !reflect.DeepEqual(counted.FacetCounts, wantCounts) {
		t.Errorf("facet_counts under repo=acme/widgets = %v, want %v", counted.FacetCounts, wantCounts)
	}
	searched := getDeliveryTimeline(t, handler, window+"&repo=acme/widgets&q=api")
	if got := searched.FacetCounts["repo"]; !reflect.DeepEqual(got, map[string]int{"acme/widgets": 1}) {
		t.Errorf("facet_counts.repo under q=api = %v, want only acme/widgets#1's repository", got)
	}
	wantColors := map[string]map[string]int{
		"repo":         {"acme/widgets": 3, "acme/other": 2},
		"author":       {"octocat": 3, "hubot": 2},
		"priority":     {"P0": 2, "P2": 1, "__no_priority__": 1, "__no_issue__": 1},
		"component":    {"ACME/api": 3, "__no_component__": 1, "__no_issue__": 1},
		"parent_agent": {"__no_session__": 5},
	}
	if !reflect.DeepEqual(counted.ColorCounts, wantColors) {
		t.Errorf("color_counts = %v, want %v (no facet applied)", counted.ColorCounts, wantColors)
	}
}

// TestGetDeliveryTimelineFacetsComponentsOfEverySource proves the component facet names a
// project's components whichever of its sources defines them: a pull request on an issue attached
// to acme/arch's api and one on an issue attached to acme/docs' docs each carry their component,
// both are counted, and selecting the second source's component selects its pull request.
func TestGetDeliveryTimelineFacetsComponentsOfEverySource(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	seedDeliveryIssueFacts(t, database)
	for _, statement := range []string{
		`update issue_components set mode = 'explicit', reason = null where issue_key = 'ACME-3'`,
		`insert into issue_component_members (issue_key, project_key, component_id) values ('ACME-3', 'ACME', 'docs')`,
		// Here ACME-3 is the work of acme/widgets#3, so it resolves to acme/widgets.
		`update issues set repo = 'acme/widgets' where key = 'ACME-3'`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("attach ACME-3 to docs: %v\n%s", err, statement)
		}
	}
	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, seed := range []struct {
		pr    delivery.DeliveryPullRequest
		issue string
	}{
		{delivery.DeliveryPullRequest{Repo: "acme/widgets", Number: 1, Title: "feat: ship the api", Author: "octocat"}, "ACME-1"},
		{delivery.DeliveryPullRequest{Repo: "acme/widgets", Number: 3, Title: "docs: tidy", Author: "octocat"}, "ACME-3"},
	} {
		pr := seed.pr
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
		at := merged.Add(time.Duration(pr.Number) * time.Minute)
		pr.MergedAt, pr.CreatedAt = &at, &at
		if err := delivery.UpsertPullRequest(ctx, database.Pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
		seedAttributedHead(t, deps.Attributor, database.Pool, pr.Repo, pr.Number, seed.issue)
	}
	window := "from=" + merged.Add(-time.Hour).Format(time.RFC3339) + "&to=" + merged.Add(time.Hour).Format(time.RFC3339)

	body := getDeliveryTimeline(t, handler, window)
	components := map[string][]string{}
	for _, pr := range body.PRs {
		components[pr.ID] = pr.Components
	}
	if want := map[string][]string{"acme/widgets#1": {"ACME/api"}, "acme/widgets#3": {"ACME/docs"}}; !reflect.DeepEqual(components, want) {
		t.Fatalf("pull request components = %v, want %v", components, want)
	}
	if got, want := body.FacetCounts["component"], map[string]int{"ACME/api": 1, "ACME/docs": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("facet_counts.component = %v, want %v", got, want)
	}
	if _, ok := body.Components["ACME/docs"]; !ok {
		t.Fatalf("components = %+v, want acme/docs' docs among them", body.Components)
	}
	if got := deliveryPRIDs(getDeliveryTimeline(t, handler, window+"&component=ACME/docs").PRs); !slices.Equal(got, []string{"acme/widgets#3"}) {
		t.Fatalf("component=ACME/docs: prs = %v, want acme/widgets#3", got)
	}
}

// TestTheTimelineMarksAPullRequestWhoseIssueIsNotKnown: acme/widgets#40 has a results row whose
// texts are current and name no key, and its commit messages are not read. So its issue is not
// known. #41 names no issue: its messages are read and name no key. The timeline counts and
// selects #40 under __issue_not_known__ and #41 under __no_issue__, by priority and by component.
// Control in the same test: with the messages of #40 read and empty, both are under __no_issue__.
func TestTheTimelineMarksAPullRequestWhoseIssueIsNotKnown(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	seedDeliveryIssueFacts(t, database)
	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, number := range []int{40, 41} {
		at := merged.Add(time.Duration(number) * time.Minute)
		if err := delivery.UpsertPullRequest(ctx, database.Pool, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: number, Title: "feat: change " + strconv.Itoa(number),
			URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number), Author: "octocat", CreatedAt: &at, MergedAt: &at,
		}); err != nil {
			t.Fatalf("seed PR acme/widgets#%d: %v", number, err)
		}
	}
	storeAttributionRow(t, deps.Attributor, database, 40, "work/none", false)
	storeAttributionRow(t, deps.Attributor, database, 41, "work/none", true)
	const window = "from=2024-01-01T00:00:00Z&to=2024-01-02T00:00:00Z"

	body := getDeliveryTimeline(t, handler, window)
	apart := map[string]int{"__issue_not_known__": 1, "__no_issue__": 1}
	for _, facet := range []string{"priority", "component"} {
		if got := body.FacetCounts[facet]; !reflect.DeepEqual(got, apart) {
			t.Errorf("facet_counts.%s = %v, want %v", facet, got, apart)
		}
	}
	if got := body.ColorCounts["priority"]; !reflect.DeepEqual(got, apart) {
		t.Errorf("color_counts.priority = %v, want %v", got, apart)
	}
	notKnown := map[string]bool{}
	for _, pr := range body.PRs {
		notKnown[pr.ID] = pr.IssueNotKnown
	}
	if want := map[string]bool{"acme/widgets#40": true, "acme/widgets#41": false}; !reflect.DeepEqual(notKnown, want) {
		t.Errorf("issue_not_known by pull request = %v, want %v", notKnown, want)
	}
	for query, want := range map[string][]string{
		"priority=__issue_not_known__":  {"acme/widgets#40"},
		"priority=__no_issue__":         {"acme/widgets#41"},
		"component=__issue_not_known__": {"acme/widgets#40"},
		"component=__no_issue__":        {"acme/widgets#41"},
	} {
		if got := deliveryPRIDs(getDeliveryTimeline(t, handler, window+"&"+query).PRs); !slices.Equal(got, want) {
			t.Errorf("%s: prs = %v, want %v", query, got, want)
		}
	}

	// Control: with the messages of #40 read and empty, its issue is known, and it is none.
	storeAttributionRow(t, deps.Attributor, database, 40, "work/none", true)
	control := getDeliveryTimeline(t, handler, window)
	none := map[string]int{"__no_issue__": 2}
	for _, facet := range []string{"priority", "component"} {
		if got := control.FacetCounts[facet]; !reflect.DeepEqual(got, none) {
			t.Errorf("control: facet_counts.%s = %v, want %v", facet, got, none)
		}
	}
	if got := control.ColorCounts["priority"]; !reflect.DeepEqual(got, none) {
		t.Errorf("control: color_counts.priority = %v, want %v", got, none)
	}
}

// TestNewDeliveryRowGivesEveryColourFacetAValue: deliveryColorCounts colours a row by its first
// value under each colour-by facet, so newDeliveryRow must give each of them one, a placeholder
// where the pull request has none (no issue, no session).
func TestNewDeliveryRowGivesEveryColourFacetAValue(t *testing.T) {
	t.Parallel()
	bare := newDeliveryRow(delivery.DeliveryPRView{Repo: "acme/widgets", Author: "octocat", Components: []string{}})
	for _, facet := range deliveryColorFacets {
		if len(bare.values[facet]) == 0 {
			t.Fatalf("a pull request with no issue and no session has no %s value: %v", facet, bare.values)
		}
	}
	if got := bare.values["rework"]; len(got) != 1 || got[0] != deliveryReworkValue {
		t.Fatalf("rework = %v, want [%s] for a pull request that is not rework", got, deliveryReworkValue)
	}
}
