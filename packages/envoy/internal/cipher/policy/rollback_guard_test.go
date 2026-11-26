package policy_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestTheProjectRollbackGuardIsRefusedByTheMergeBaseAndIgnoredHere is the head half of the
// project tags' rollback guard check (rollbackguard_base_test.go). The golden is what the merge
// base 545b2116 answered for each case: it must refuse every guarded secret by name and serve
// every bare twin, the control that shows the guard, and no other tag of the shape, is what
// refuses. This broker must serve every guarded secret exactly as it serves its bare twin: the
// guard reaches no Secret field, so a requesters tag holding it alone is no list and a real list
// beside it keeps its meaning.
func TestTheProjectRollbackGuardIsRefusedByTheMergeBaseAndIgnoredHere(t *testing.T) {
	for base, head := range map[string]string{
		baseRollbackGuard:      policy.RequestersRollbackGuard,
		baseTagProject:         policy.TagProject,
		baseTagProjectChannels: policy.TagProjectChannels,
	} {
		if base != head {
			t.Fatalf("rollbackguard_base_test.go records %q, but this broker and agent-secrets write %q: the golden records another string than the one written", base, head)
		}
	}
	// The golden's loader registers no service, so it cannot show that no deployment's would
	// accept the guard as one: BROKER_SERVICES holds every name to record.ValidService, which
	// refuses it, in this release and at the merge base alike.
	if record.ValidService(policy.RequestersRollbackGuard) {
		t.Fatalf("%q is a valid service name, so a deployment could register it and a broker before the project tags would read the guard as that service", policy.RequestersRollbackGuard)
	}
	golden, cases := readRollbackGolden(t), rollbackGuardCases()
	for _, c := range cases {
		switch answer := golden[c.name]; {
		case c.guarded && (answer.Served || answer.Reason != policy.ReasonRequestersMalformed):
			t.Errorf("the merge base answered %s, which carries the guard, %+v; want it refused %s", c.name, answer, policy.ReasonRequestersMalformed)
		case !c.guarded && !answer.Served:
			t.Errorf("the merge base answered %s, the bare twin, %+v; want it served, or the guard is not what refuses its twin", c.name, answer)
		}
	}

	loader := policytest.Loader(rollbackGuardStore())
	loader.ProjectChecks = true
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range cases {
		got, served := set.Secrets[c.name]
		if !served {
			t.Errorf("this broker refuses %s as %+v; want it served", c.name, set.Refused[policytest.ID(c.name)])
			continue
		}
		if got.Requesters != c.list {
			t.Errorf("%s loaded with requesters %q, want %q: the guard is no requester", c.name, got.Requesters, c.list)
		}
		if !c.guarded {
			continue
		}
		twin, ok := set.Secrets["BARE_"+c.name[len("GUARDED_"):]]
		if !ok {
			continue
		}
		got.Name, got.Source = twin.Name, twin.Source
		if got != twin {
			t.Errorf("%s loaded as %+v, its bare twin as %+v; want them equal but for the name", c.name, got, twin)
		}
	}

	privStates := map[string]policy.ProjectState{"PRIV": policy.ProjectPrivate}
	for _, c := range []struct {
		name, why string
		requester policy.Requester
		outcome   string
		reason    string
	}{
		{"GUARDED_STORED", "a requesters tag holding the guard alone is no list", policy.Requester{Operator: guardOutsider, Projects: []string{"PRIV"}, Named: []string{"PRIV"}, ProjectStates: privStates}, policy.Automatic, ""},
		{"GUARDED_STORED", "the project rule still decides", policy.Requester{Operator: guardOutsider, ProjectStates: privStates}, policy.Deny, policy.ReasonProjectNotNamed},
		{"GUARDED_LISTED", "a real list beside the guard keeps its meaning", policy.Requester{Operator: guardOutsider, Projects: []string{"PRIV"}, Named: []string{"PRIV"}, ProjectStates: privStates}, policy.Deny, policy.ReasonRequesterNotListed},
		{"GUARDED_LISTED", "the person the list names asks as before", policy.Requester{Operator: guardOwner, Projects: []string{"PRIV"}, Named: []string{"PRIV"}, ProjectStates: privStates}, policy.Approval, ""},
	} {
		d, err := set.Evaluate(c.name, c.requester)
		if err != nil || d.Outcome != c.outcome || d.Reason != c.reason {
			t.Errorf("Evaluate(%s, %+v) = %+v, %v; want %s %s: %s", c.name, c.requester, d, err, c.outcome, c.reason, c.why)
		}
	}
}

// TestSplitGuardsAndWithGuards: each guard entry is taken out of a requesters tag wherever it
// stands, the rest answered single-spaced and each guard reported apart; a tag holding neither is
// answered unchanged, so a requesters tag no guard was written into reaches ParseRequesters exactly
// as the store holds it, and a guard joined to an entry by a comma is no guard. WithGuards writes
// the list, then the project guard, then the kinds guard, each named one once and each not named
// none, single-spaced, whatever guards and spacing the list it is given holds.
func TestSplitGuardsAndWithGuards(t *testing.T) {
	project, kinds := policy.RequestersRollbackGuard, policy.KindsRollbackGuard
	for _, c := range []struct {
		raw, rest     string
		project, kind bool
	}{
		{project, "", true, false},
		{kinds, "", false, true},
		{project + " " + kinds, "", true, true},
		{"carol@example.com " + project, "carol@example.com", true, false},
		{project + "  carol@example.com\tsvc", "carol@example.com svc", true, false},
		{"carol@example.com " + project + " " + project, "carol@example.com", true, false},
		{kinds + "  ada@example.com\tlegion-daemon", "ada@example.com legion-daemon", false, true},
		{"ada@example.com " + kinds + " " + project, "ada@example.com", true, true},
		{"carol@example.com  svc", "carol@example.com  svc", false, false},
		{"", "", false, false},
		{"carol@example.com," + project, "carol@example.com," + project, false, false},
		{"ada@example.com," + kinds, "ada@example.com," + kinds, false, false},
	} {
		rest, gotProject, gotKinds := policy.SplitGuards(c.raw)
		if rest != c.rest || gotProject != c.project || gotKinds != c.kind {
			t.Errorf("SplitGuards(%q) = %q, %v, %v; want %q, %v, %v", c.raw, rest, gotProject, gotKinds, c.rest, c.project, c.kind)
		}
	}
	for _, c := range []struct {
		list          string
		project, kind bool
		want          string
	}{
		{"", false, false, ""},
		{"", true, false, project},
		{"", false, true, kinds},
		{"", true, true, project + " " + kinds},
		{"carol@example.com", true, false, "carol@example.com " + project},
		{"carol@example.com", true, true, "carol@example.com " + project + " " + kinds},
		{"carol@example.com " + kinds, true, true, "carol@example.com " + project + " " + kinds},
		{kinds + " carol@example.com  " + project, true, true, "carol@example.com " + project + " " + kinds},
		{"carol@example.com " + project + " " + project, true, false, "carol@example.com " + project},
		{"carol@example.com " + project + " " + kinds, false, true, "carol@example.com " + kinds},
		{"carol@example.com\t" + project + " " + kinds, false, false, "carol@example.com"},
		{project + " " + kinds, false, false, ""},
	} {
		if got := policy.WithGuards(c.list, c.project, c.kind); got != c.want {
			t.Errorf("WithGuards(%q, %v, %v) = %q, want %q", c.list, c.project, c.kind, got, c.want)
		}
	}
}
