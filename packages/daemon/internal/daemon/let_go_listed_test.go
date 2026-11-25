package daemon

import (
	"context"
	"encoding/json"
	"testing"
)

// R8 of dispatch://INFRA-3686 keeps as a field each fact main's text gave. Main's let-go notice
// named a child that Dispatch's listing no longer holds as "no longer listed" (tellLetGoWaits at
// d46ec4e0), and the record gave such a child an empty status, which no reader can tell from a
// status left out. Each child of a let-go notice says whether the listing holds it: here one child
// the listing shows in backlog, and one it no longer holds, which the notice names with no status.
func TestALetGoNoticeSaysWhetherTheListingHoldsEachChild(t *testing.T) {
	const tree, shown, gone = "LEGION-988", "LEGION-989", "LEGION-990"
	ctx := context.Background()
	w, client, _, _ := preparationRig(t, &installations{})
	liveRoot(t, w, client, tree)
	client.show(issue(shown, strangerRepo, "backlog", false, tree, 1))
	listing, err := client.ListIssues(ctx, "LEGION", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := len(treeNotices(t, w, tree))
	dropped := []letGo{{repo: strangerRepo, why: letGoNoTodo, waits: map[string]releaseWait{
		shown: {tree: tree, parent: tree}, gone: {tree: tree, parent: tree}}}}
	if err := w.tellLetGoWaits(ctx, dropped, listing); err != nil {
		t.Fatalf("tell the let-go waits: %v", err)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 {
		t.Fatalf("notices to %s's architect = %q, want one let-go notice", tree, notices)
	}
	var payload struct {
		LetGo []map[string]any `json:"let_go"`
	}
	if err := json.Unmarshal([]byte(notices[0]), &payload); err != nil {
		t.Fatal(err)
	}
	children := map[string]map[string]any{}
	for _, child := range payload.LetGo {
		key, _ := child["child"].(string)
		children[key] = child
	}
	for key, want := range map[string]struct {
		listed bool
		status string
	}{shown: {true, "backlog"}, gone: {false, ""}} {
		child, named := children[key]
		if !named {
			t.Fatalf("the let-go notice names %v, want %s among them: %s", payload.LetGo, key, notices[0])
		}
		if listed, said := child["listed"].(bool); !said || listed != want.listed || child["status"] != want.status {
			t.Errorf("the let-go notice names %s as %v, want listed: %t with status %q: whether the listing holds a child is a fact of its own, not an empty status",
				key, child, want.listed, want.status)
		}
	}
}
