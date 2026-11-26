package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// parentRuleDispatch is a Dispatch whose parent status rule is on (R24 of dispatch://INFRA-3320):
// a status write that would put an issue in work under a parent that is not in work is refused
// 409 PARENT_STATUS_CONFLICT, naming the parent with its status. Each issue reads as issues holds
// it, and its event log is empty.
type parentRuleDispatch struct {
	outboxDispatch
	issues  map[string]dispatch.Issue
	refused []string
}

func (d *parentRuleDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	return d.issues[key], nil
}

func (d *parentRuleDispatch) SetStatusAt(_ context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	current := d.issues[issue.Key]
	if record.InWorkStatus(status) && current.Parent != nil {
		if parent := d.issues[*current.Parent]; !record.InWorkStatus(parent.Status) {
			d.refused = append(d.refused, issue.Key+":"+status)
			return dispatch.Issue{}, &dispatch.Error{Status: http.StatusConflict, Code: "PARENT_STATUS_CONFLICT",
				Message: "the parent is not in work", Issues: []dispatch.ConflictIssue{{Key: parent.Key, Status: parent.Status}}}
		}
	}
	current.Status = status
	current.LastSeq++
	d.issues[issue.Key] = current
	return current, nil
}

// R24 of dispatch://INFRA-3320: "The daemon passes one of these with each status write of its own
// that would otherwise be refused". A phase worker of the root moves the root to backlog in
// Dispatch with include_children, which Dispatch takes from a token, writing the child first so
// the rule holds after each step: Dispatch then shows the root and its child, both at work,
// at backlog, each by an event of that session. The repair sets both back to in_progress
// (sessionStatusWrite's ReassertStatusWrite), one row on each issue's lane. The child's event comes
// first, so its row runs first, while Dispatch still shows the root at backlog: the daemon's
// write of the child into work under a parent out of work is one Dispatch's rule refuses unless
// it passes include_parent or follows the root's write. No write of the daemon's own is refused,
// and both issues end in work as the daemon set them back.
func TestTheSetBackOfAChildAndItsParentNeverBreaksTheRule(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, child = "LEGION-1", "LEGION-2"
	parent := root
	seedRecord(t, pool, record.Issue{Key: root, Status: "in_progress", DispatchStatus: "backlog", LastDispatchSeq: 3, SeenSeq: 3})
	seedRecord(t, pool, record.Issue{Key: child, Parent: &parent, Status: "in_progress", DispatchStatus: "backlog", LastDispatchSeq: 3, SeenSeq: 3})
	client := &parentRuleDispatch{issues: map[string]dispatch.Issue{
		root:  {Key: root, Status: "backlog", LastSeq: 3, Labels: []string{"legion"}, HandedOver: true},
		child: {Key: child, Status: "backlog", Parent: &parent, LastSeq: 3},
	}}
	for _, key := range []string{child, root} {
		enqueueOutbox(t, pool, records, mustOutboxRow(t, key, record.ReassertStatusWrite("in_progress", "backlog", 3), time.Now()))
	}
	logs := &syncBuffer{}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, now: time.Now,
		log: slog.New(slog.NewJSONHandler(logs, nil))}
	for range 3 {
		if _, err := pool.Exec(context.Background(), "update outbox set next_at = now() where kind = 'dispatch_status'"); err != nil {
			t.Fatalf("make the rows due: %v", err)
		}
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the rows: %v", err)
		}
	}
	if len(client.refused) != 0 {
		t.Errorf("the writes Dispatch's parent rule refused = %v, want none: a set-back is a status write of the daemon's own", client.refused)
	}
	if lines := loggedLines(t, logs.String(), "outbox status write refused by Dispatch's parent status rule; the row ends and its caller is told"); len(lines) != 0 {
		t.Errorf("the parent-rule refusal lines = %v, want none", lines)
	}
	for _, key := range []string{root, child} {
		if got := client.issues[key].Status; got != "in_progress" {
			t.Errorf("Dispatch shows %s %s after the set-backs, want in_progress: the session's backlog stands", key, got)
		}
	}
}
