package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// preparedWrites keeps each fact a route gives its own status write, as the daemon's preparation
// of it receives it (Options.PrepareRepository), and gives it on unchanged.
type preparedWrites struct {
	mu    sync.Mutex
	facts []intake.DispatchIssue
}

func (p *preparedWrites) prepare(_ context.Context, fact intake.DispatchIssue) (intake.DispatchIssue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.facts = append(p.facts, fact)
	return fact, nil
}

// of is the event of the one fact the routes gave for key's own write.
func (p *preparedWrites) of(t *testing.T, key string) intake.DispatchIssue {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var found []intake.DispatchIssue
	for _, fact := range p.facts {
		if fact.Key == key && fact.Delivered != nil {
			found = append(found, *fact.Delivered)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the routes gave %d own writes of %s, want one: %+v", len(found), key, p.facts)
	}
	return found[0]
}

// A route's own status write says who asked for it on the fact it gives the write
// (intake.DispatchIssue.Requester): the controller's set_status is the controller's, the same
// route with the operator's grant (`legion issue status`) is a person's, and rerun_child and
// release_children are an architect's. Only the operator's names the person who asked
// (PersonActor): Dispatch records every such write as the daemon's own.
func TestTheRoutesOwnWriteSaysWhoAsked(t *testing.T) {
	t.Parallel()
	harnessWith := func(t *testing.T) (*harness, []intake.Handler, *statusRecorder, *preparedWrites) {
		t.Helper()
		writes := &preparedWrites{}
		h, handlers, statuses := statusHarness(t, func(opts *Options) { opts.PrepareRepository = writes.prepare })
		return h, handlers, statuses, writes
	}
	asked := func(t *testing.T, got intake.DispatchIssue, requester claim.Role, person string) {
		t.Helper()
		if got.Requester != requester || got.PersonActor != person {
			t.Fatalf("the own write's fact says Requester %q and PersonActor %q, want %q and %q", got.Requester, got.PersonActor, requester, person)
		}
	}

	t.Run("set_status with a controller session's grant is the controller's", func(t *testing.T) {
		t.Parallel()
		h, _, _, writes := harnessWith(t)
		registration := h.registeredController(h.mintedSecret(), "ses_controller")
		recorder := h.controllerSessionGrant("ses_controller", registration.Secret)
		if recorder.Code != http.StatusOK {
			t.Fatalf("controller-session grant = %d; body %s", recorder.Code, recorder.Body)
		}
		var grant GrantResponse
		decodeInto(t, recorder, &grant)
		if status := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "backlog"}, nil); status.Code != http.StatusOK {
			t.Fatalf("set_status = %d %s, want 200", status.Code, status.Body)
		}
		asked(t, writes.of(t, statusChild), claim.RoleController, "")
	})

	t.Run("set_status with the operator's grant is a person's", func(t *testing.T) {
		t.Parallel()
		h, _, _, writes := harnessWith(t)
		grant := controllerGrant(t, h)
		if status := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "backlog"}, nil); status.Code != http.StatusOK {
			t.Fatalf("set_status = %d %s, want 200", status.Code, status.Body)
		}
		asked(t, writes.of(t, statusChild), claim.Role("person"), "operator")
	})

	t.Run("rerun_child is an architect's", func(t *testing.T) {
		t.Parallel()
		h, handlers, statuses, writes := harnessWith(t)
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		ownersBacklog(t, h, handlers, statuses, architect, statusChild)
		if recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("rerun = %d %s, want 200", recorder.Code, recorder.Body)
		}
		asked(t, writes.of(t, statusChild), claim.RoleArchitect, "")
	})

	t.Run("release_children is an architect's", func(t *testing.T) {
		t.Parallel()
		h, _, statuses, writes := harnessWith(t)
		const released = "LEGION-210"
		root := statusRoot
		statuses.issues[released] = dispatch.Issue{Key: released, ResolvedRepo: "acme/widgets", Title: "a released child", Status: "backlog", Parent: &root, Rank: "C", LastSeq: 2}
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		if recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{released}}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("wave release = %d %s, want 200", recorder.Code, recorder.Body)
		}
		asked(t, writes.of(t, released), claim.RoleArchitect, "")
	})
}
