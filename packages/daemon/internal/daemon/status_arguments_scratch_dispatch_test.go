package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The three k37 rig proofs share this rig: the scratch Dispatch with its parent-status rule
// (DISPATCH_PARENT_STATUS_RULE) on and again off, a daemon booted over it, and the controller's
// grant, which the operator's token mints. Each holds that the daemon asks for the argument
// Dispatch's rule needs whatever the rule's setting, and that the write it queues with one lands
// on Dispatch's status-change route with its option, which moves, unlinks or raises the issues
// the option names with the setting on or off. The api package's tests of the same names are the
// unit cases against a fake Dispatch.

// statusArgumentsRig boots the rig with the parent-status rule at rule, "on" or "off".
func statusArgumentsRig(t *testing.T, rule string) *personHandoverRig {
	t.Helper()
	r := newPersonHandoverRigOn(t, scratchDispatchOptions{natsURL: testnats.URL(t), env: []string{parentStatusRule + "=" + rule}}, 8)
	r.boot()
	return r
}

// controllerGrant is a grant of the project's controller, minted with the operator's token.
func (r *personHandoverRig) controllerGrant() string {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants", map[string]any{}, true)
	var grant api.GrantResponse
	if status != http.StatusOK || json.Unmarshal(body, &grant) != nil {
		r.t.Fatalf("mint the controller's grant = %d %s, want 200", status, body)
	}
	return grant.GrantID
}

// dispatchView is what Dispatch shows of key now: its status and its parent, "" for none.
func (r *personHandoverRig) dispatchView(key string) (status, parent string) {
	r.t.Helper()
	var read struct {
		Status string  `json:"status"`
		Parent *string `json:"parent"`
	}
	dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
	if read.Parent != nil {
		parent = *read.Parent
	}
	return read.Status, parent
}

// awaitDispatch waits until what Dispatch shows of key satisfies holds.
func (r *personHandoverRig) awaitDispatch(what, key string, holds func(status, parent string) bool) {
	r.t.Helper()
	testwait.Eventually(r.t, "Dispatch to show "+key+" "+what, func() bool {
		return holds(r.dispatchView(key))
	})
}

// refusedWith holds a daemon answer to the refusal of code, naming each of names.
func refusedWith(t *testing.T, status int, body []byte, code string, names ...string) {
	t.Helper()
	var failure api.Failure
	if status/100 == 2 || json.Unmarshal(body, &failure) != nil || failure.Code != code {
		t.Fatalf("the daemon answered %d %s, want %s", status, body, code)
	}
	for _, name := range names {
		if !strings.Contains(failure.Error, name) {
			t.Fatalf("the %s refusal %q does not name %s", code, failure.Error, name)
		}
	}
}

func TestSetStatusTakesAChildrenArgument(t *testing.T) {
	for _, rule := range []string{"on", "off"} {
		t.Run("rule "+rule, func(t *testing.T) {
			r := statusArgumentsRig(t, rule)
			grant := r.controllerGrant()
			labelled := map[string]any{"labels": []string{"legion"}}
			for _, tc := range []struct {
				children      string
				parent, child string
			}{
				{"", "Alpine glacier sensor", "Basalt quarry ledger"},
				{intake.IncludeArgument, "Cobalt mine audit", "Delta estuary gauge"},
				{intake.UnlinkArgument, "Emerald forest census", "Fjord ferry timetable"},
			} {
				parent := r.create(asSession, tc.parent, labelled)
				r.admit(parent.Key)
				child := r.create(asPerson, tc.child, map[string]any{"parent": parent.Key})
				setDispatchStatus(t, r.baseURL, child.Key, "todo")
				r.quiet(parent.Key, child.Key)

				status, body := r.d.request(http.MethodPost, "/legion/v1/issues/status",
					api.IssueStatusRequest{GrantID: grant, Issue: parent.Key, Status: "backlog", Children: tc.children}, false)
				switch tc.children {
				case "":
					refusedWith(t, status, body, "CHILDREN_ARGUMENT_REQUIRED", child.Key)
					if shown, _ := r.dispatchView(parent.Key); shown == "backlog" {
						t.Fatalf("Dispatch shows %s backlog after the refused set_status", parent.Key)
					}
				case intake.IncludeArgument:
					if status != http.StatusOK {
						t.Fatalf("set_status backlog of %s with children include = %d %s, want 200", parent.Key, status, body)
					}
					r.awaitDispatch("backlog", parent.Key, func(status, _ string) bool { return status == "backlog" })
					r.awaitDispatch("backlog under "+parent.Key, child.Key, func(status, under string) bool { return status == "backlog" && under == parent.Key })
				case intake.UnlinkArgument:
					if status != http.StatusOK {
						t.Fatalf("set_status backlog of %s with children unlink = %d %s, want 200", parent.Key, status, body)
					}
					r.awaitDispatch("backlog", parent.Key, func(status, _ string) bool { return status == "backlog" })
					r.awaitDispatch("in work with no parent", child.Key, func(status, under string) bool { return under == "" && status != "backlog" })
				}
			}
		})
	}
}

func TestATodoUnderAParentNotInWorkTakesAParentArgument(t *testing.T) {
	for _, rule := range []string{"on", "off"} {
		t.Run("rule "+rule, func(t *testing.T) {
			r := statusArgumentsRig(t, rule)
			grant := r.controllerGrant()
			for _, tc := range []struct {
				argument      string
				parent, child string
			}{
				{"", "Granite bridge inspection", "Harbour pilot roster"},
				{intake.IncludeArgument, "Iron foundry schedule", "Jasper canyon trail"},
			} {
				parent := r.create(asSession, tc.parent, nil)
				setDispatchStatus(t, r.baseURL, parent.Key, "backlog")
				child := r.create(asSession, tc.child, map[string]any{"parent": parent.Key, "labels": []string{"legion"}})
				setDispatchStatus(t, r.baseURL, child.Key, "backlog")
				r.quiet(parent.Key, child.Key)

				status, body := r.d.request(http.MethodPost, "/legion/v1/issues/status",
					api.IssueStatusRequest{GrantID: grant, Issue: child.Key, Status: "todo", Parent: tc.argument}, false)
				if tc.argument == "" {
					refusedWith(t, status, body, "PARENT_ARGUMENT_REQUIRED", parent.Key+" (backlog)")
					if shown, _ := r.dispatchView(child.Key); shown != "backlog" {
						t.Fatalf("Dispatch shows %s %s after the refused set_status, want backlog", child.Key, shown)
					}
					continue
				}
				if status != http.StatusOK {
					t.Fatalf("set_status todo of %s with parent include = %d %s, want 200", child.Key, status, body)
				}
				r.awaitDispatch("in work", child.Key, func(status, _ string) bool { return status != "backlog" })
				r.awaitDispatch("in_progress", parent.Key, func(status, _ string) bool { return status == "in_progress" })
			}
		})
	}
}

func TestReleaseChildrenRefusesAnIssueWhoseParentStaysOutOfWork(t *testing.T) {
	for _, rule := range []string{"on", "off"} {
		t.Run("rule "+rule, func(t *testing.T) {
			r := statusArgumentsRig(t, rule)
			root := r.create(asSession, "Kelp farm monitor", map[string]any{"labels": []string{"legion"}})
			grant := r.admit(root.Key)
			parent := r.create(asPerson, "Lagoon dredging plan", map[string]any{"parent": root.Key})
			setDispatchStatus(t, r.baseURL, parent.Key, "backlog")
			child := r.create(asPerson, "Moraine drainage study", map[string]any{"parent": parent.Key})
			setDispatchStatus(t, r.baseURL, child.Key, "backlog")
			r.quiet(parent.Key, child.Key)

			status, body := r.d.request(http.MethodPost, "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: grant(), Issues: []string{child.Key}}, false)
			refusedWith(t, status, body, "PARENT_NOT_RELEASED", parent.Key)
			if shown, _ := r.dispatchView(child.Key); shown != "backlog" {
				t.Fatalf("Dispatch shows %s %s after the refused release, want backlog", child.Key, shown)
			}

			status, body = r.d.request(http.MethodPost, "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: grant(), Issues: []string{child.Key, parent.Key}}, false)
			if status != http.StatusOK {
				t.Fatalf("release_children of %s and %s = %d %s, want 200", child.Key, parent.Key, status, body)
			}
			for _, key := range []string{parent.Key, child.Key} {
				r.awaitDispatch("released", key, func(status, _ string) bool { return status != "backlog" })
			}
		})
	}
}
