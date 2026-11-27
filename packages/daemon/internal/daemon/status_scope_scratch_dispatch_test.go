package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// legionStatusScope is Dispatch's setting that turns R26's refusal on (`on`) or off (unset or
// `off`; DISPATCH_LEGION_STATUS_SCOPE in packages/envoy/cmd/dispatch/settings.go). With it on, a
// write of a Legion issue's status from a credential that holds neither the person scope nor
// legion-status is refused 403 LEGION_STATUS_SCOPE_REQUIRED, and a publication of an issue's role
// holders from any token but the daemon's own broker token 403 ROLE_HOLDER_FORBIDDEN. A scratch
// Dispatch leaves it unset, so off, unless its caller's env names it.
const legionStatusScope = "DISPATCH_LEGION_STATUS_SCOPE"

// R26 of dispatch://INFRA-3320 against a real Dispatch built from this tree, with
// DISPATCH_LEGION_STATUS_SCOPE on and again off. The daemon's outbox writes with the scratch
// server's shared agent token, a bearer that can hold no legion-status, as a daemon under
// dispatch_token_file does. It runs four rows, each on a Legion issue: an ordinary status row of a
// labelled root (PATCH), a status row with include_children of a labelled parent and one with
// include_parent of a child under a labelled parent (POST /status-change carrying children and
// parent), and the role-holder publication of a labelled root's tester, whose claim is ready (PUT
// role-holders). The two children carry no label of their own: each is a Legion issue by the
// labelled parent above it.
//
// With the setting on, Dispatch refuses each write at each of the outbox's two runs: 403
// LEGION_STATUS_SCOPE_REQUIRED for the three status writes and 403 ROLE_HOLDER_FORBIDDEN for the
// publication. The outbox keeps each row and writes it again at its next run, logs a line naming
// the code and the issue at each refusal, and queues exactly one status-scope notice to the
// controller over both runs, which are one daemon run. Dispatch shows every issue at the status
// and sequence it stood at, and lists no role holder.
//
// The negative control is the same rows against a Dispatch with the setting off: each write is
// taken at the first run, no row is left, nothing is logged or told, and Dispatch shows what each
// write wrote. So each refusal is the setting's.
func TestADaemonWriteWithoutTheStatusScopeIsKeptAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"on", "off"} {
		t.Run("scope "+scope, func(t *testing.T) {
			on := scope == "on"
			ctx := context.Background()
			d := startScratchDispatchOn(t, scratchDispatchOptions{env: []string{legionStatusScope + "=" + scope}})
			// The person sets every issue up: a person's credential holds the person scope, so
			// Dispatch takes these writes with the setting on.
			person := func(key string, fields map[string]any) {
				t.Helper()
				dispatchCallAs(t, d.base, http.MethodPatch, "/api/v1/issues/"+key, fields, nil, asPerson)
			}
			legion := map[string]any{"labels": []string{"legion"}}
			// A labelled root at todo, which the ordinary row moves into work.
			plain := createScratchDispatchIssue(t, d.base)
			person(plain, legion)
			person(plain, map[string]any{"status": "todo"})
			// A labelled parent at work with a child at work under it, which the row with
			// include_children parks with its child.
			parked := createScratchDispatchIssueUnder(t, d.base, "Ferry timetable: the parent", "")
			person(parked, legion)
			person(parked, map[string]any{"status": "in_progress"})
			parkedChild := createScratchDispatchIssueUnder(t, d.base, "Ferry timetable: the child", parked)
			person(parkedChild, map[string]any{"status": "in_progress"})
			// A child at triage under a labelled parent on the backlog, which the row with
			// include_parent moves into work with its parent.
			raised := createScratchDispatchIssueUnder(t, d.base, "Harbour lamp: the parent", "")
			person(raised, legion)
			person(raised, map[string]any{"status": "backlog"})
			raisedChild := createScratchDispatchIssueUnder(t, d.base, "Harbour lamp: the child", raised)
			// A labelled root whose tester's ready claim the board lane publishes.
			published := createScratchDispatchIssueUnder(t, d.base, "Tide gauge calibration", "")
			person(published, legion)

			shown := func(key string) scratchIssue {
				t.Helper()
				var read scratchIssue
				dispatchCall(t, d.base, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
				return read
			}
			holders := func() []dispatch.RoleHolder {
				t.Helper()
				var read struct {
					Holders []dispatch.RoleHolder `json:"holders"`
				}
				dispatchCall(t, d.base, http.MethodGet, "/api/v1/issues/"+published+"/role-holders", nil, &read)
				return read.Holders
			}
			before := map[string]scratchIssue{}
			for _, key := range []string{plain, parked, parkedChild, raised, raisedChild} {
				before[key] = shown(key)
			}

			r := newRoleHoldersRig(t)
			client := &answeredDispatch{Client: dispatch.New(d.base, dispatch.BearerTransport(d.token, nil))}
			logs := &syncBuffer{}
			r.runner.dispatch = client
			r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
			r.seedClaim(published, claim.RoleTester, "ses-tester", "ready", time.Minute)
			enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, plain, record.OrdinaryStatusWrite("in_progress", "todo", ""), time.Now()))
			children := record.OrdinaryStatusWrite("backlog", "in_progress", "")
			children.IncludeChildren = true
			enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, parked, children, time.Now()))
			parent := record.OrdinaryStatusWrite("todo", "triage", "")
			parent.IncludeParent = true
			enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, raisedChild, parent, time.Now()))
			r.enqueue(published)

			// Two runs of one outbox, as one daemon run makes them: each row the first run keeps
			// is made due again for the second.
			for range 2 {
				if _, err := r.pool.Exec(ctx, "update outbox set next_at = now() where kind = any($1)",
					[]string{string(record.OutboxKindDispatchStatus), string(record.OutboxKindRoleHoldersPublish)}); err != nil {
					t.Fatalf("make the rows due: %v", err)
				}
				if err := r.runner.RunOnce(ctx); err != nil {
					t.Fatalf("run the outbox: %v", err)
				}
			}

			var wantAnswers []string
			wantLines := map[string]int{}
			for _, write := range []struct{ write, issue, code string }{
				{"PATCH " + plain + " status in_progress", plain, statusScopeCode},
				{"POST " + parked + "/status-change status backlog include_children", parked, statusScopeCode},
				{"POST " + raisedChild + "/status-change status todo include_parent", raisedChild, statusScopeCode},
				{"PUT " + published + "/role-holders", published, roleHolderForbiddenCode},
			} {
				if !on {
					wantAnswers = append(wantAnswers, write.write+": taken")
					continue
				}
				refused := write.write + ": 403 " + write.code
				wantAnswers = append(wantAnswers, refused, refused)
				wantLines[write.issue+" "+write.code] = 2
			}
			answers := client.recorded()
			slices.Sort(answers)
			slices.Sort(wantAnswers)
			if !slices.Equal(answers, wantAnswers) {
				t.Fatalf("Dispatch answered the outbox's writes %q, want %q", answers, wantAnswers)
			}
			t.Logf("with %s=%s Dispatch answered the outbox's writes %q", legionStatusScope, scope, answers)

			kept := 0
			if on {
				kept = 1
			}
			for _, key := range []string{plain, parked, raisedChild} {
				if rows := statusRowsOf(t, r.pool, key); rows != kept {
					t.Errorf("status rows of %s after both runs = %d, want %d", key, rows, kept)
				}
			}
			var publications int
			if err := r.pool.QueryRow(ctx, "select count(*) from outbox where kind = $1 and issue = $2",
				string(record.OutboxKindRoleHoldersPublish), published).Scan(&publications); err != nil {
				t.Fatalf("count the role-holder rows of %s: %v", published, err)
			}
			if publications != kept {
				t.Errorf("role-holder rows of %s after both runs = %d, want %d", published, publications, kept)
			}

			lines := map[string]int{}
			for _, line := range append(
				loggedLines(t, logs.String(), statusScopeRefusedLine),
				loggedLines(t, logs.String(), roleHolderRefusedLine)...) {
				lines[fmt.Sprint(line["issue"], " ", line["code"])]++
			}
			if !maps.Equal(lines, wantLines) {
				t.Errorf("refusal lines by issue and code = %v, want %v: one for each refusal", lines, wantLines)
			}

			var told []record.Notice
			for _, key := range []string{plain, parked, raisedChild, published} {
				told = append(told, controllerNoticesOfKind(t, r.pool, key, record.StatusScopeNotice)...)
			}
			if len(told) != kept {
				t.Fatalf("status-scope notices = %+v, want %d: one for the daemon run, whichever refusal comes first", told, kept)
			}
			for _, notice := range told {
				if (!strings.Contains(notice.Reason, "403 "+statusScopeCode) && !strings.Contains(notice.Reason, "403 "+roleHolderForbiddenCode)) || !containsAll(notice.Reason, statusScopeSteps) {
					t.Errorf("status-scope notice reason = %q, want the refusal's status and code and the three changes that give the daemon the scope", notice.Reason)
				}
			}

			written := map[string]string{plain: "in_progress", parked: "backlog", parkedChild: "backlog", raised: "in_progress", raisedChild: "todo"}
			for key, was := range before {
				now := shown(key)
				switch {
				case on && (now.Status != was.Status || now.LastSeq != was.LastSeq):
					t.Errorf("Dispatch shows %s %s at %d after the refusals, want it as it stood, %s at %d", key, now.Status, now.LastSeq, was.Status, was.LastSeq)
				case !on && now.Status != written[key]:
					t.Errorf("Dispatch shows %s %s, want the %s the outbox wrote", key, now.Status, written[key])
				}
			}
			var wantHolders []dispatch.RoleHolder
			if !on {
				wantHolders = []dispatch.RoleHolder{{Role: "tester", Session: "ses-tester"}}
			}
			if got := holders(); !slices.Equal(got, wantHolders) {
				t.Errorf("Dispatch lists the role holders %+v of %s, want %+v", got, published, wantHolders)
			}
		})
	}
}

// answeredDispatch is the daemon's real Dispatch client with a record of Dispatch's answer to
// each status write and role-holder publication the outbox made through it: the route, the
// issue, what the write carried, and "taken", the status and code of Dispatch's refusal, or the
// error of a call that reached no answer.
type answeredDispatch struct {
	dispatch.Client
	mu      sync.Mutex
	answers []string
}

func (d *answeredDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	written, err := d.Client.SetStatusAt(ctx, issue, status)
	d.answered(fmt.Sprintf("PATCH %s status %s", issue.Key, status), err)
	return written, err
}

func (d *answeredDispatch) ChangeStatus(ctx context.Context, key string, change dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
	result, err := d.Client.ChangeStatus(ctx, key, change)
	write := fmt.Sprintf("POST %s/status-change status %s", key, change.Status)
	for _, option := range []struct {
		name string
		set  bool
	}{{"include_children", change.IncludeChildren}, {"unlink_children", change.UnlinkChildren}, {"include_parent", change.IncludeParent}} {
		if option.set {
			write += " " + option.name
		}
	}
	d.answered(write, err)
	return result, err
}

func (d *answeredDispatch) PutRoleHolders(ctx context.Context, key string, holders []dispatch.RoleHolder) error {
	err := d.Client.PutRoleHolders(ctx, key, holders)
	d.answered(fmt.Sprintf("PUT %s/role-holders", key), err)
	return err
}

// answered records Dispatch's answer err to write.
func (d *answeredDispatch) answered(write string, err error) {
	answer := "taken"
	var refused *dispatch.Error
	switch {
	case errors.As(err, &refused):
		answer = fmt.Sprintf("%d %s", refused.Status, refused.Code)
	case err != nil:
		answer = err.Error()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers = append(d.answers, write+": "+answer)
}

// recorded is a copy of every answer recorded so far, oldest first.
func (d *answeredDispatch) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.answers)
}
