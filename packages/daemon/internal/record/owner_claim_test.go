package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The owner of an issue's task is the architect of that issue itself: a root's is the tree's
// architect, which ArchitectOfTree takes for it, and a child's is the child's own architect, never
// the tree's. A project that makes no claim token names no owner.
func TestTheOwnerOfATaskIsTheArchitectOfItsIssue(t *testing.T) {
	token := func(key string) claim.Token {
		t.Helper()
		tok, err := claim.NewToken("legion", key, claim.RoleArchitect)
		if err != nil {
			t.Fatalf("NewToken(%s): %v", key, err)
		}
		return tok
	}
	for _, tc := range []struct {
		issue Issue
		want  claim.Token
	}{
		{Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "Legion"}, token("LEGION-1")},
		{Issue{Key: "LEGION-3", Tree: "LEGION-1", Project: "Legion"}, token("LEGION-3")},
	} {
		got, err := OwnerClaim(tc.issue)
		if err != nil || got != tc.want {
			t.Errorf("OwnerClaim(%s of the tree %s) = %q, %v; want %q", tc.issue.Key, tc.issue.Tree, got, err, tc.want)
		}
	}
	root := Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "Legion"}
	owner, err := OwnerClaim(root)
	if err != nil {
		t.Fatalf("OwnerClaim(the root): %v", err)
	}
	if ok, err := ArchitectOfTree(t.Context(), nil, nil, owner, root); err != nil || !ok {
		t.Errorf("ArchitectOfTree(%s, the root) = %v, %v; want the root's owner to be the tree's architect", owner, ok, err)
	}
	if got, err := OwnerClaim(Issue{Key: "LEGION-3", Tree: "LEGION-1", Project: "--"}); err == nil {
		t.Errorf("OwnerClaim of a project that makes no claim token = %q, want an error", got)
	}
}
