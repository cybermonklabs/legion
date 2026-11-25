package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The issue read and the issue listing at a cut. Both run their present statements under the
// cut, which the store binds to the shadows. These are the parts of the two answers that the
// shadows do not give, and that each answer leaves out (leaveOut), or reads another way, at a cut.

// refuseParameterAtCut answers 400 AS_OF_NOT_SERVED, naming parameter, for a request at a cut
// that sends parameter, which the records at the cut cannot evaluate (decision 5): its message is
// "<parameter>: <reason>; send the request with no <parameter>". It reports whether it answered.
func refuseParameterAtCut(w http.ResponseWriter, r *http.Request, parameter, reason string) bool {
	if _, atCut := asof.CutFrom(r.Context()); !atCut || !r.URL.Query().Has(parameter) {
		return false
	}
	writeError(w, contracts.DispatchAsOfNotServed, http.StatusBadRequest,
		parameter+": "+reason+"; send the request with no "+parameter)
	return true
}

// refuseListingFilterAtCut refuses a listing at a cut that sends a filter the records at the cut
// cannot evaluate (refuseParameterAtCut): route_status reads the route roster now, and pinned
// reads one person's pins now; neither keeps a history. It reports whether it answered.
func refuseListingFilterAtCut(w http.ResponseWriter, r *http.Request) bool {
	return refuseParameterAtCut(w, r, "route_status",
		"the route roster keeps no history, so a listing at a past time cannot filter by route_status") ||
		refuseParameterAtCut(w, r, "pinned",
			"the pins of a person keeps no history, so a listing at a past time cannot filter by pinned")
}

// issueAtTheCut completes the issue read at the cut ctx carries. The read does not read the route
// roster, which keeps no past state: an issue with a route leaves out the route's reach, and an
// issue a session holds leaves out the holder's liveness, where the present read reads the roster
// (getIssue); the fields are null without a route and without a session holder at any time. It
// leaves out design_gates (no event records them) and referenced_by_count (the reference graph
// keeps no history), which readIssue does not read at a cut, and the members of an explicit
// component attachment, which the present component model splits (an attachment of another mode
// holds no member). Progress follows decision 9 (tasksAtTheCut). The documents leave out the
// documents of an approval request whose newest event at the cut holds none (reason B,
// approvalAtTheCut), and the open asks what the records at the cut do not give of them
// (leaveOutPastAskFields). trees is whether the read carries display trees.
func (s *server) issueAtTheCut(ctx context.Context, read *issueDetail, trees bool) error {
	leaveOut(ctx, "/design_gates", "/"+model.ReferencedByCountKey)
	leaveOutRouteReach(ctx, read.Route, "")
	if issueClaimSession(read.Claim) != "" {
		leaveOut(ctx, "/holder_state", "/last_seen", "/takeable_at")
	}
	if read.Components.Mode == "explicit" {
		leaveOut(ctx, "/components/ids", "/components/unknown")
	}
	tasks, err := s.tasksAtTheCut(ctx, []string{read.Key})
	if err != nil {
		return err
	}
	if counted, ok := tasks[read.Key]; ok {
		read.Progress.Tasks = counted
	}
	asks := make([]model.Ask, len(read.OpenAsks))
	for index := range read.OpenAsks {
		asks[index] = read.OpenAsks[index].Ask
	}
	artifact := elementsOf("/artifacts")
	for index := range read.Artifacts {
		if approval := read.Artifacts[index].Approval; approval != nil {
			approvalAtTheCut(ctx, &approval.ArtifactApproval, artifact(index))
		}
	}
	return s.leaveOutPastAskFields(ctx, asks, elementsOf("/open_asks"), trees)
}

// issueSummariesAtTheCut completes the listing at the cut ctx carries, each issue at the place
// that prefix and its index name in the answer ("" for an unpaged answer, "/issues" for a paged
// one). The listing does not read the route roster: an issue with a route leaves out the route's
// reach, which is null at any time for an issue with none. An issue with an explicit component
// attachment leaves out its members, which the present component model splits. Progress follows
// decision 9 (tasksAtTheCut).
func (s *server) issueSummariesAtTheCut(ctx context.Context, issues []model.IssueSummary, prefix string) error {
	keys := make([]string, len(issues))
	for index := range issues {
		keys[index] = issues[index].Key
	}
	tasks, err := s.tasksAtTheCut(ctx, keys)
	if err != nil {
		return err
	}
	place := elementsOf(prefix)
	for index := range issues {
		at := place(index)
		if counted, ok := tasks[issues[index].Key]; ok {
			issues[index].Progress.Tasks = counted
		}
		leaveOutRouteReach(ctx, issues[index].Route, at)
		if issues[index].Components.Mode == "explicit" {
			leaveOut(ctx, at+"/components/ids", at+"/components/unknown")
		}
	}
	return nil
}

// tasksAtTheCutStatement reads, of the issues whose keys $1 holds, each issue whose primary
// document's newest version at the cut is not the version its stored task count names
// (tasks_version, which the issues shadow gives as the row holds it now), with that version's
// markdown: the newest version that holds markdown, or the empty text. It is the
// reconciliation's own reading of a drifted count (docs.RecordTaskProgressMarkdown,
// docs.TaskProgressDrift), read through the shadows of artifacts and artifact_versions, so it
// reads the versions at the cut.
const tasksAtTheCutStatement = `
		select i.key,
		       coalesce((
		         select v.markdown from artifacts a join artifact_versions v on v.artifact_id = a.id
		         where a.issue_key = i.key and a.is_primary and v.markdown is not null
		         order by v.number desc limit 1
		       ), '')
		from issues i
		where i.key = any($1::text[]) and ` + docs.TaskProgressDrift

// tasksAtTheCut is progress.tasks at the cut ctx carries for each of keys whose primary
// document's newest version at the cut is not the version its stored count names (decision 9):
// the task items of that version's markdown, counted as a version write counts them
// (pmdoc.CountTasksMarkdown), nil for none. An issue it does not name keeps the stored count,
// which counts the version at the cut. Markdown the reader cannot read counts as no task, as the
// reconciliation counts it.
func (s *server) tasksAtTheCut(ctx context.Context, keys []string) (map[string]*model.ProgressCount, error) {
	tasks := map[string]*model.ProgressCount{}
	if len(keys) == 0 {
		return tasks, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, tasksAtTheCutStatement, keys)
	if err != nil {
		return nil, fmt.Errorf("read the primary documents' versions at the cut: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, markdown string
		if err := rows.Scan(&key, &markdown); err != nil {
			return nil, err
		}
		counted, err := pmdoc.CountTasksMarkdown(markdown)
		if err != nil {
			slog.Warn("dispatch: the issue read at a cut cannot read the spec's version at the cut; counting no task",
				"issue", key, "error", err)
			counted = pmdoc.TaskProgress{}
		}
		tasks[key] = nil
		if counted.Total > 0 {
			tasks[key] = &model.ProgressCount{Done: counted.Done, Total: counted.Total}
		}
	}
	return tasks, rows.Err()
}

// leaveOutPastAskFields names what a past-time answer leaves out of each of asks, the ask at the
// place at(index) names in the answer (decision 5):
//   - referenced_by_count, which the reference graph gives now (reason G);
//   - revision, each option's id and the answer's id, the ask's parts as the present tables hold
//     them, for an ask that changed after the cut (reason P, asof.AsksChangedAfterStatement);
//   - the question, the options, multiple and the urgency, and the question's tree where the
//     answer carries display trees (trees), for an ask whose wording at the cut no record gives:
//     a restore changed it with no event, at a time no record gives (reason E,
//     asof.AsksChangedAfterStatement). Such an ask counts as changed, so its parts are left out
//     too;
//   - waiting_on_person where the records at the cut give no person: no one handed the ask on,
//     its issue held nobody, and its project's default assignee is unknown at the cut, which the
//     projects shadow gives as null (reason D); the read then gives no person;
//   - approval.documents of an approval request whose state at the cut names no document, written
//     before requests held their documents (reason B).
func (s *server) leaveOutPastAskFields(ctx context.Context, asks []model.Ask, at func(index int) string, trees bool) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut || len(asks) == 0 {
		return nil
	}
	ids := make([]string, len(asks))
	for index := range asks {
		ids[index] = asks[index].ID
	}
	changed, wordingUnknown, err := s.asksChangedAfterTheCut(ctx, ids, cut)
	if err != nil {
		return fmt.Errorf("read which asks changed after the cut: %w", err)
	}
	for index, ask := range asks {
		place := at(index)
		leaveOut(ctx, place+"/"+model.ReferencedByCountKey)
		if changed[ask.ID] {
			leaveOutAskParts(ctx, place, len(ask.Options), ask.Answer != nil)
		}
		if wordingUnknown[ask.ID] {
			leaveOutAskWording(ctx, place, trees)
		}
		if ask.WaitingOnPerson == "" {
			leaveOut(ctx, place+"/waiting_on_person")
		}
		if ask.Approval != nil && len(ask.Approval.Documents) == 0 {
			leaveOut(ctx, place+"/approval/documents")
		}
	}
	return nil
}
