package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// k38 of dispatch://INFRA-4730 (R25 of dispatch://INFRA-3320): the daemon reads the events of
// Dispatch's one-time fix of parents' statuses as any reading, and decides each by R19 and R20.
// The scratch Dispatch first runs with DISPATCH_PARENT_STATUS_RULE off, which is the state the
// fix finds: open issues under done parents, and issues in work under parents nobody started. The
// daemon boots over that state with the records an older daemon left for some of those issues,
// and then Dispatch boots again with the rule on, which runs the fix before it serves: it unlinks
// each open issue from its done parent, keeping its status, and moves each not-started ancestor of
// an issue in work to in_progress, as its system actor. The daemon reads those events off the bus:
//
//   - an unlinked issue in implementing keeps its run and its claims as a root;
//   - an unlinked root that stands admitted and in_progress waiting on its child keeps its
//     generation and its place in admission.blocked, and nothing of it starts;
//   - an unlinked issue recorded at phase done, whose status the fix leaves as it is, stays out of
//     Legion's work with its phase and generation unchanged;
//   - a labelled todo issue a person handed to Legion, made a root, waits in the admission line;
//   - an unlabelled todo issue made a root waits for a person's handover;
//   - an issue recorded at phase done and moved to in_progress stays out of Legion's work until a
//     person's todo, which opens a new run;
//   - an unrecorded issue moved to in_progress starts nothing.
func TestEachKindOfIssueReadsTheFixAsR25Says(t *testing.T) {
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it on its first connection.
	board := scratchDispatchOptions{natsURL: testnats.URL(t), env: []string{parentStatusRule + "=off"}}
	// One slot: the issue in implementing holds it, so the labelled root waits in the line.
	r := newPersonHandoverRigOn(t, board, 1)
	// A first boot migrates the daemon's database, which the records below are written to; the
	// daemon is stopped while they are written, so its next boot reads them as an older daemon
	// left them.
	r.boot()
	settled(t, r.d, "answer its state on its first boot", func(api.State) bool { return true })
	r.d.stop()

	// The pre-fix state in Dispatch, with the rule off.
	under := func(title, parent string, fields map[string]any) scratchIssue {
		t.Helper()
		if fields == nil {
			fields = map[string]any{}
		}
		fields["parent"] = parent
		return r.create(asPerson, title, fields)
	}
	doneParent := func(title string) scratchIssue {
		t.Helper()
		return r.create(asPerson, title, nil)
	}
	working := doneParent("Kestrel umbrella")
	implementing := under("Heron implementation", working.Key, nil)
	r.setStatus(implementing.Key, "in_progress")
	r.setStatus(working.Key, "done")

	// The waiting root has an open child and waits on an unfinished issue outside its chain (a link
	// to its own child would be a cycle Dispatch refuses), which is what puts it in
	// admission.blocked.
	waitingParent := doneParent("Osprey canopy")
	waiting := under("Falcon admitted waiter", waitingParent.Key, map[string]any{"labels": []string{"legion"}})
	r.setStatus(waiting.Key, "in_progress")
	pending := under("Plover pending", waiting.Key, nil)
	r.setStatus(pending.Key, "todo")
	outside := r.create(asPerson, "Gannet outside blocker", nil)
	r.setStatus(outside.Key, "todo")
	dispatchCall(t, r.baseURL, "PATCH", "/api/v1/issues/"+waiting.Key, map[string]any{"blocked_by": []string{outside.Key}}, nil)
	r.setStatus(waitingParent.Key, "done")

	closedParent := doneParent("Egret shelter")
	closed := under("Ibis closed record", closedParent.Key, nil)
	r.setStatus(closed.Key, "in_progress")
	r.setStatus(closedParent.Key, "done")

	labelledParent := doneParent("Puffin awning")
	labelled := under("Tern labelled handover", labelledParent.Key, map[string]any{"labels": []string{"legion"}})
	r.setStatus(labelled.Key, "todo")
	r.setStatus(labelledParent.Key, "done")

	unlabelledParent := doneParent("Curlew roof")
	unlabelled := under("Dunlin bare todo", unlabelledParent.Key, nil)
	r.setStatus(unlabelled.Key, "todo")
	r.setStatus(unlabelledParent.Key, "done")

	recordedAncestor := r.create(asPerson, "Avocet backlogged closed", nil)
	r.setStatus(recordedAncestor.Key, "backlog")
	recordedWorker := under("Lapwing working child", recordedAncestor.Key, nil)
	r.setStatus(recordedWorker.Key, "in_progress")

	unrecordedAncestor := r.create(asPerson, "Bittern backlogged unrecorded", nil)
	r.setStatus(unrecordedAncestor.Key, "backlog")
	unrecordedWorker := under("Sanderling working grandchild", unrecordedAncestor.Key, nil)
	r.setStatus(unrecordedWorker.Key, "in_progress")

	// The records an older daemon left: the child in implementing holding the slot, the root at
	// admitted waiting on its child, and two records at phase done.
	recordOf := func(issue scratchIssue, at phase.Phase, generation uint64, status string, parent string) record.Issue {
		seen := r.issue(issue.Key)
		recorded := record.Issue{Key: issue.Key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: r.cfg.Project, Tree: issue.Key, Title: issue.Key, Phase: at, Generation: generation, Status: status,
			DispatchStatus: seen.Status, Rank: "A", LastDispatchSeq: seen.LastSeq}
		if parent != "" {
			recorded.Parent, recorded.Tree = &parent, parent
		}
		return recorded
	}
	if err := pgx.BeginFunc(t.Context(), r.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		for _, issue := range []record.Issue{
			recordOf(implementing, phase.Implementing, 1, "in_progress", working.Key),
			recordOf(waiting, phase.Admitted, 1, "in_progress", waitingParent.Key),
			recordOf(closed, phase.Done, 2, "done", closedParent.Key),
			recordOf(recordedAncestor, phase.Done, 1, "done", ""),
		} {
			if err := records.PutIssue(t.Context(), tx, issue); err != nil {
				return err
			}
		}
		// The waiting root's design gate is approved, so what holds its first phase is the issue it
		// waits on (admission.blocked) rather than its gate.
		approved := 1
		if err := records.PutGate(t.Context(), tx, record.DesignGate{Issue: waiting.Key, ArtifactID: waiting.PrimaryArtifactID,
			LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			return err
		}
		return records.PutSlot(t.Context(), tx, record.Slot{Issue: implementing.Key, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed the older daemon's records: %v", err)
	}
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	r.quiet(implementing.Key, waiting.Key, pending.Key, outside.Key, closed.Key, labelled.Key, unlabelled.Key, recordedAncestor.Key,
		recordedWorker.Key, unrecordedAncestor.Key, unrecordedWorker.Key)
	_, body := settled(t, r.d, "hold the pre-fix state", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, implementing.Key) && slices.Contains(s.Admission.Waiting, labelled.Key) &&
			blockedNames(s, waiting.Key)
	})
	t.Logf("before the fix:\n%s", body)

	// R25's fix: Dispatch boots again with the rule on and changes the pairs before it serves.
	seqs, statuses := map[string]int64{}, map[string]string{}
	changed := []scratchIssue{implementing, waiting, closed, labelled, unlabelled, recordedAncestor, unrecordedAncestor}
	for _, issue := range changed {
		shown := r.issue(issue.Key)
		seqs[issue.Key], statuses[issue.Key] = shown.LastSeq, shown.Status
	}
	r.dispatch.stop()
	r.dispatch.env = []string{parentStatusRule + "=on"}
	r.dispatch.start()
	for _, issue := range changed {
		var fixed *scratchEvent
		events := r.issueEvents(issue.Key)
		for i := range events {
			if events[i].Seq > seqs[issue.Key] && events[i].Actor.Kind == "system" && factEvent(events[i].Type) {
				fixed = &events[i]
			}
		}
		if fixed == nil {
			t.Fatalf("the event log of %s holds no event of R25's fix after sequence %d: %+v", issue.Key, seqs[issue.Key], events)
		}
		r.applied(*fixed)
	}
	// The unlinks keep each issue's status.
	for _, issue := range []scratchIssue{implementing, waiting, closed, labelled, unlabelled} {
		if shown := r.issue(issue.Key).Status; shown != statuses[issue.Key] {
			t.Fatalf("Dispatch shows %s %s after the fix, want the %s it had: an unlink keeps the status", issue.Key, shown, statuses[issue.Key])
		}
	}
	for _, issue := range []scratchIssue{recordedAncestor, unrecordedAncestor} {
		if shown := r.issue(issue.Key).Status; shown != "in_progress" {
			t.Fatalf("Dispatch shows %s %s after the fix, want in_progress: an ancestor of an issue in work", issue.Key, shown)
		}
	}
	after, body := settled(t, r.d, "decide the fix's events", func(api.State) bool { return true })
	t.Logf("after the fix:\n%s", body)

	// The unlinked issue in implementing keeps its run, its slot and its claims.
	if got := r.record(implementing.Key); got.Phase != phase.Implementing || got.Generation != 1 || !record.InLegionsWork(got) {
		t.Fatalf("the unlinked issue in implementing = %+v, want it in implementing at generation 1, in Legion's work", got)
	}
	if !slices.Contains(after.Admission.Active, implementing.Key) {
		t.Fatalf("the unlinked issue in implementing holds no slot after the fix: %s", body)
	}
	if stopped := r.stoppedClaims(implementing.Key); len(stopped) != 0 {
		t.Fatalf("the unlinked issue in implementing has claims stopped %v, want every claim kept", stopped)
	}

	// The unlinked root waiting on its child keeps its generation and its place among the blocked,
	// and nothing of it starts.
	if got := r.record(waiting.Key); got.Phase != phase.Admitted || got.Generation != 1 {
		t.Fatalf("the unlinked root waiting on its child = %+v, want it admitted at generation 1", got)
	}
	if !blockedNames(after, waiting.Key) || slices.Contains(after.Admission.Active, waiting.Key) {
		t.Fatalf("the unlinked root waiting on its child is not in admission.blocked, or holds a slot: %s", body)
	}
	if claims := r.claimsOf(waiting.Key); len(claims) != 0 {
		t.Fatalf("the unlinked root waiting on its child has claims %v, want nothing of it started", claims)
	}

	// The unlinked record at phase done stays out of Legion's work.
	if got := r.record(closed.Key); got.Phase != phase.Done || got.Generation != 2 || record.InLegionsWork(got) {
		t.Fatalf("the unlinked record at phase done = %+v, want it at phase done, generation 2, out of Legion's work", got)
	}

	// The labelled todo a person handed over waits in the line as a root.
	if !slices.Contains(after.Admission.Waiting, labelled.Key) {
		t.Fatalf("the labelled todo made a root is not in the admission line: %s", body)
	}

	// The unlabelled todo waits for a person's handover: nothing of it is in work or in the line.
	if inLine(after, unlabelled.Key) || r.recorded(unlabelled.Key) != nil && record.InLegionsWork(*r.recorded(unlabelled.Key)) {
		t.Fatalf("the unlabelled todo made a root is in Legion's work or its line: %s", body)
	}
	if shown := r.issue(unlabelled.Key).Status; shown != "todo" {
		t.Fatalf("Dispatch shows the unlabelled todo %s, want todo with no write of the daemon's", shown)
	}

	// The record at phase done moved to in_progress stays out of Legion's work.
	if got := r.record(recordedAncestor.Key); got.Phase != phase.Done || got.Generation != 1 || record.InLegionsWork(got) ||
		inLine(after, recordedAncestor.Key) {
		t.Fatalf("the record at phase done moved to in_progress = %+v, want it out of Legion's work at generation 1", got)
	}

	// The unrecorded issue moved to in_progress starts nothing.
	if inLine(after, unrecordedAncestor.Key) || r.recorded(unrecordedAncestor.Key) != nil && record.InLegionsWork(*r.recorded(unrecordedAncestor.Key)) {
		t.Fatalf("the unrecorded issue moved to in_progress is in Legion's work or its line: %s", body)
	}
	if claims := r.claimsOf(unrecordedAncestor.Key); len(claims) != 0 {
		t.Fatalf("the unrecorded issue moved to in_progress has claims %v, want nothing started", claims)
	}

	// A person's todo on the record at phase done opens a new run: its child at work finishes
	// first, since the rule refuses a not-started parent over a child in work.
	r.setStatus(recordedWorker.Key, "done")
	back := r.setStatus(recordedAncestor.Key, "todo")
	r.applied(r.newestEvent(recordedAncestor.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == back.LastSeq }))
	if got := r.record(recordedAncestor.Key); got.Generation != 2 || got.Phase != phase.Admitted {
		t.Fatalf("the record at phase done after a person's todo = %+v, want a new run at generation 2, admitted", got)
	}
}

// blockedNames says whether the state's admission.blocked names key.
func blockedNames(s api.State, key string) bool {
	return slices.ContainsFunc(s.Admission.Blocked, func(blocked api.BlockedIssue) bool { return blocked.Issue == key })
}

// inLine says whether the state holds key in a slot or in the admission line.
func inLine(s api.State, key string) bool {
	return slices.Contains(s.Admission.Active, key) || slices.Contains(s.Admission.Waiting, key)
}

// recorded is the daemon's record of key, nil for an issue it does not record.
func (r *personHandoverRig) recorded(key string) *record.Issue {
	r.t.Helper()
	var issue *record.Issue
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(r.t.Context(), tx, key)
		return err
	}); err != nil {
		r.t.Fatalf("read the record of %s: %v", key, err)
	}
	return issue
}

// claimsOf is every claim the daemon holds of key, as role and state.
func (r *personHandoverRig) claimsOf(key string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select role, state from claims where issue = $1 order by role", key)
	if err != nil {
		r.t.Fatalf("read the claims of %s: %v", key, err)
	}
	defer rows.Close()
	var claims []string
	for rows.Next() {
		var role, state string
		if err := rows.Scan(&role, &state); err != nil {
			r.t.Fatalf("scan a claim of %s: %v", key, err)
		}
		claims = append(claims, fmt.Sprintf("%s %s", role, state))
	}
	return claims
}

// stoppedClaims is every claim of key the daemon stopped or closed: suspended or retired.
func (r *personHandoverRig) stoppedClaims(key string) []string {
	r.t.Helper()
	var stopped []string
	rows, err := r.pool.Query(context.Background(), "select role, state from claims where issue = $1 and state in ('suspended', 'retired')", key)
	if err != nil {
		r.t.Fatalf("read the stopped claims of %s: %v", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var role, state string
		if err := rows.Scan(&role, &state); err != nil {
			r.t.Fatalf("scan a stopped claim of %s: %v", key, err)
		}
		stopped = append(stopped, role+" "+state)
	}
	return stopped
}
