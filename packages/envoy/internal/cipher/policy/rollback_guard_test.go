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

// TestSplitRequestersGuard: the guard entry is taken out of a requesters tag wherever it stands,
// the rest answered single-spaced; a tag without it is answered unchanged, so a requesters tag no
// guard was written into reaches ParseRequesters exactly as the store holds it.
func TestSplitRequestersGuard(t *testing.T) {
	guard := policy.RequestersRollbackGuard
	for _, c := range []struct {
		raw, rest string
		guarded   bool
	}{
		{guard, "", true},
		{"carol@example.com " + guard, "carol@example.com", true},
		{guard + "  carol@example.com\tsvc", "carol@example.com svc", true},
		{"carol@example.com " + guard + " " + guard, "carol@example.com", true},
		{"carol@example.com  svc", "carol@example.com  svc", false},
		{"", "", false},
		{"carol@example.com," + guard, "carol@example.com," + guard, false},
	} {
		rest, guarded := policy.SplitRequestersGuard(c.raw)
		if rest != c.rest || guarded != c.guarded {
			t.Errorf("SplitRequestersGuard(%q) = %q, %v; want %q, %v", c.raw, rest, guarded, c.rest, c.guarded)
		}
	}
	for list, want := range map[string]string{"": guard, "carol@example.com": "carol@example.com " + guard} {
		if got := policy.WithRequestersGuard(list); got != want {
			t.Errorf("WithRequestersGuard(%q) = %q, want %q", list, got, want)
		}
	}
}
