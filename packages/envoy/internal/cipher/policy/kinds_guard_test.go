package policy_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestTheKindsRollbackGuardIsRefusedByTheMergeBaseAndIgnoredHere is the head half of the kinds'
// rollback guard check (kindsguard_base_test.go). The golden is what the broker before the kinds
// answered for each case: it must refuse every guarded secret by name, requesters-malformed, and
// serve every bare twin, the control that shows the guard, and no other tag of the shape, is what
// refuses, and that a broker before the kinds hands a secret behind a kind out as its value
// without it. This broker must serve every guarded secret exactly as it serves its bare twin,
// behind its kind: the guard reaches no Secret field, so a requesters tag holding it alone is no
// list and a real list beside it keeps its meaning.
func TestTheKindsRollbackGuardIsRefusedByTheMergeBaseAndIgnoredHere(t *testing.T) {
	for base, head := range map[string]string{
		baseKindsGuard:       policy.KindsRollbackGuard,
		baseTagProxy:         policy.TagProxy,
		baseTagProxyNarrowed: policy.TagProxyNarrowed,
	} {
		if base != head {
			t.Fatalf("kindsguard_base_test.go records %q, but this broker and agent-secrets write %q: the golden records another string than the one written", base, head)
		}
	}
	// The golden's loader registers no service, so it cannot show that no deployment's would
	// accept the guard as one: BROKER_SERVICES holds every name to record.ValidService, which
	// refuses it, in this release and at the merge base alike.
	if record.ValidService(policy.KindsRollbackGuard) {
		t.Fatalf("%q is a valid service name, so a deployment could register it and a broker before the kinds would read the guard as that service", policy.KindsRollbackGuard)
	}
	golden, cases := readKindsGuardGolden(t), kindsGuardCases()
	for _, c := range cases {
		switch answer := golden[c.name]; {
		case c.guarded && (answer.Served || answer.Reason != policy.ReasonRequestersMalformed):
			t.Errorf("the merge base answered %s, which carries the guard, %+v; want it refused %s", c.name, answer, policy.ReasonRequestersMalformed)
		case !c.guarded && !answer.Served:
			t.Errorf("the merge base answered %s, the bare twin, %+v; want it served, or the guard is not what refuses its twin", c.name, answer)
		}
	}

	loader := policytest.Loader(kindsGuardStore())
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile())
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
		if got.Requesters != c.list || got.Proxy == "" {
			t.Errorf("%s loaded with requesters %q behind %q, want %q behind its kind: the guard is no requester", c.name, got.Requesters, got.Proxy, c.list)
		}
		if !c.guarded {
			continue
		}
		twin, ok := set.Secrets["KBARE_"+c.name[len("KGUARDED_"):]]
		if !ok {
			continue
		}
		got.Name, got.Source = twin.Name, twin.Source
		if got != twin {
			t.Errorf("%s loaded as %+v, its bare twin as %+v; want them equal but for the name", c.name, got, twin)
		}
	}
}

// TestBothGuardsAreTakenOutOfTheRequesters: a secret of a private project behind a kind carries
// both guards, as `agent-secrets secret` writes them (WithGuards(list, true, true)), and this
// broker serves it with its list alone, as it serves the same secret with neither; a tag holding
// the two guards and no list is no list. The broker before the kinds refused the kinds guard (the
// golden), and the broker before the project tags the project one, so either refuses it.
func TestBothGuardsAreTakenOutOfTheRequesters(t *testing.T) {
	secret := func(name, requesters string) secrets.LocalSecret {
		s := policytest.Proxied(policytest.Secret(name, guardOwner, policy.TierHuman, "v"), "github")
		s.Tags[policy.TagProject] = "PRIV"
		if requesters != "" {
			s.Tags[policy.TagRequesters] = requesters
		}
		return s
	}
	both := policy.WithGuards(guardOwner, true, true)
	if both != guardOwner+" "+policy.RequestersRollbackGuard+" "+policy.KindsRollbackGuard {
		t.Fatalf("WithGuards(%q, true, true) = %q; want the list, the project guard, then the kinds guard", guardOwner, both)
	}
	loader := policytest.Loader(secrets.NewLocal(
		secret("BOTH_LISTED", both), secret("NONE_LISTED", guardOwner),
		secret("BOTH_ALONE", policy.WithGuards("", true, true)), secret("NONE_ALONE", ""),
	))
	loader.ProjectChecks = true
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile())
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for guarded, bare := range map[string]string{"BOTH_LISTED": "NONE_LISTED", "BOTH_ALONE": "NONE_ALONE"} {
		got, served := set.Secrets[guarded]
		twin, twinServed := set.Secrets[bare]
		if !served || !twinServed {
			t.Fatalf("%s served %t, %s served %t; want both served: %+v", guarded, served, bare, twinServed, set.Refused)
		}
		got.Name, got.Source = twin.Name, twin.Source
		if got != twin {
			t.Errorf("%s loaded as %+v, %s as %+v; want them equal but for the name: neither guard is a requester", guarded, got, bare, twin)
		}
	}
}
