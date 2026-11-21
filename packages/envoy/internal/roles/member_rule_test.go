package roles_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
)

// TestTheMemberRuleDecidesWhatASessionSees: in the required mode a session sees a project its
// token lists in every state; a full member's session also sees a project a fresh read says is
// public, and stops seeing it once the project is switched private; a restricted member's session,
// and one whose record carries no restricted claim (an entry an earlier build wrote), sees its list
// alone. Each row carries its control: the same session with the restricted bit flipped, or the
// same read before the switch, gives the other answer, so no row passes for a reason it does not
// name.
func TestTheMemberRuleDecidesWhatASessionSees(t *testing.T) {
	private := map[string]bool{"PRIV": true, "PUB": false}
	reads := 0
	read := func(_ context.Context, project string) (bool, error) {
		reads++
		return private[project], nil
	}
	sees := func(t *testing.T, listed []string, restricted bool, project string) bool {
		t.Helper()
		got, err := roles.Sees(context.Background(), read, listed, restricted, project)
		if err != nil {
			t.Fatalf("Sees(%v, restricted %v, %s): %v", listed, restricted, project, err)
		}
		return got
	}

	// A full member listing PRIV gets PUB; control: the restricted member listing PRIV does not.
	if !sees(t, []string{"PRIV"}, false, "PUB") {
		t.Error("a full member listing PRIV does not get the public PUB")
	}
	if sees(t, []string{"PRIV"}, true, "PUB") {
		t.Error("control: a restricted member listing PRIV gets PUB, which it does not list")
	}
	// Both get the listed PRIV in every state, with nothing read.
	before := reads
	if !sees(t, []string{"PRIV"}, false, "PRIV") || !sees(t, []string{"PRIV"}, true, "PRIV") {
		t.Error("a session does not get the project its token lists")
	}
	if reads != before {
		t.Errorf("a listed project read the privacy source %d times, want none", reads-before)
	}

	// A project switched private stops reaching the unlisted full member; control: before the
	// switch the same session gets it, from the same reader.
	if !sees(t, nil, false, "PUB") {
		t.Fatal("control: the unlisted full member does not get PUB while it is public")
	}
	private["PUB"] = true
	if sees(t, nil, false, "PUB") {
		t.Error("PUB switched private still reaches the unlisted full member")
	}
	private["PUB"] = false

	// A record without the restricted claim reads as restricted: its session gets only its list;
	// control: the same record marked a full member's gets PUB.
	old := session.SessionEntry{Projects: []string{"PRIV"}}
	marked := session.SessionEntry{Projects: []string{"PRIV"}, Restricted: new(false)}
	if sees(t, old.Projects, old.IsRestricted(), "PUB") {
		t.Error("a session whose record carries no restricted claim gets PUB, which it does not list")
	}
	if !sees(t, marked.Projects, marked.IsRestricted(), "PUB") {
		t.Error("control: the same record marked a full member's does not get PUB")
	}

	// A message of no project, and a GitHub subject's, is everyone's.
	for _, project := range []string{contracts.NoProject, contracts.AnyToken} {
		if !sees(t, []string{"PRIV"}, true, project) {
			t.Errorf("a restricted member does not get a message of %q", project)
		}
	}

	// An unknown answer is the read's error, never a public project.
	failing := func(context.Context, string) (bool, error) { return false, errors.New("source down") }
	if got, err := roles.Sees(context.Background(), failing, nil, false, "PUB"); err == nil || got {
		t.Errorf("an unknown answer read %v, %v; want an error and not seen", got, err)
	}
}
