package api

import (
	"context"
	"maps"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// evals_narrowed.go is how an eval list read is narrowed by the delivery timeline's facets and its
// search (dispatch://INFRA-2918, D7a; R21). The list read, the cross-project read and the evals
// view take them beside `repo`; with none set, or `repo` alone, every number is folded from the
// stored day rows, as before. With a facet but the repository, or the search, set, the evals that
// count pull requests (evals.CountsPullRequests: merge-to-production, first-commit-to-production,
// open-to-production, pr-open-to-merge and rework-share, the numbers the timeline's facets narrow
// on the base, measuresPRs) are built when the list is read, from the pull requests the timeline
// keeps under them (evals.ComputeAtRead, through the refresher's own day builders and the stored
// path's fold), each series naming the filters it was narrowed by (`narrowed`); every other eval
// keeps its stored answer and names none. The timeline's rule of what a facet or the search
// selects has one statement, deliveryRow.matches over the rows deliveryPopulation.derive makes,
// which the narrowing selects through.

// evalNarrowing is what an eval list read narrows the evals that count pull requests by: the
// delivery timeline's facets but the repository, which the read's own `repo` answers through each
// repository's rows (evals.AtRead.Repos), and its search, as the timeline reads them from its
// query (parseDeliverySelection); and applied, the keys of those its query sets, in
// deliveryFacets' order and then q, which each series it narrows names. Once a read has narrowed
// by it, issues and sessions are what the pull requests it selected from name, each session with
// the title it shows: what a write of one of those issues, or a session's title, can move the read's
// answer by, which marks a live view that reads it (readEvalsView, as nameShown marks a
// timeline's).
type evalNarrowing struct {
	query    url.Values
	applied  []string
	issues   map[string]struct{}
	sessions map[string]string
}

// parseEvalNarrowing is the narrowing an eval list read's query names, nil where it sets no facet
// but the repository and no search: a facet is set where the query names it with any value, as the
// timeline's selection holds it, and the search where it holds more than blanks.
func parseEvalNarrowing(query url.Values) *evalNarrowing {
	narrowing := &evalNarrowing{query: url.Values{}}
	for _, facet := range deliveryFacets {
		if facet == "repo" || len(query[facet]) == 0 {
			continue
		}
		narrowing.query[facet] = query[facet]
		narrowing.applied = append(narrowing.applied, facet)
	}
	if search := query.Get("q"); strings.TrimSpace(search) != "" {
		narrowing.query.Set("q", search)
		narrowing.applied = append(narrowing.applied, "q")
	}
	if len(narrowing.applied) == 0 {
		return nil
	}
	return narrowing
}

// keep is the read's evals.Narrowing: of input's pull requests, those the delivery timeline shows
// under n (deliveryRow.matches, over the rows deliveryPopulation.derive makes of them), derived
// with the rules and the apply runs of the deploy source repositories the rows are built for
// (input.Sources, delivery.SourceRulesOf), each pull request shipped by its own repository's
// deploys as the eval rows ship it, so its deployed status is the one its rows count, and one of
// no source's repository by none, as no deploy tracks it; reading in q, the read's snapshot, what
// a facet or the search selects by: the labels of the pull requests (readDeliveryLabels) and the
// session groups a Parent agent is read from, once for every pull request. It records the issues
// and the sessions those pull requests name (issues, sessions).
func (n *evalNarrowing) keep(ctx context.Context, q delivery.Querier, input evals.NarrowInput) ([]bool, error) {
	pop := deliveryPopulation{query: n.query, prs: input.PullRequests, jobsByRun: map[int64][]delivery.DeliveryRunJob{}}
	var err error
	if pop.groups, err = delivery.ListSessionGroups(ctx, q); err != nil {
		return nil, err
	}
	if err := readDeliveryLabels(ctx, q, &pop); err != nil {
		return nil, err
	}
	rules := make([]delivery.DeliverySettings, len(input.Sources))
	for i, source := range input.Sources {
		rules[i] = source.Settings
		pop.applyRuns = append(pop.applyRuns, source.ApplyRuns...)
		maps.Copy(pop.jobsByRun, source.ApplyJobs)
	}
	derived := pop.derive(delivery.SourceRulesOf(rules))
	keep := make([]bool, len(pop.prs))
	for i := range pop.prs {
		keep[i] = derived.rows[i].matches(derived.selection, "")
	}
	n.issues, n.sessions = map[string]struct{}{}, map[string]string{}
	for _, pr := range pop.prs {
		if pr.IssueKey != nil {
			n.issues[*pr.IssueKey] = struct{}{}
		}
		for _, session := range pr.Sessions {
			n.sessions[session] = delivery.SessionTitle(pop.titles, session)
		}
	}
	return keep, nil
}
