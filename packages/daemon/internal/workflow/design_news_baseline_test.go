package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// toldVersions is the versions each told row of tree's issues holds, keyed `<issue>/<audience>`;
// a row that knows no version holds an empty map.
func toldVersions(t *testing.T, pool *pgxpool.Pool, tree string) map[string]map[record.DesignStep]int {
	t.Helper()
	versions := map[string]map[record.DesignStep]int{}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		rows, err := record.NewStore().TreeTold(t.Context(), tx, tree)
		for _, row := range rows {
			versions[row.Issue+"/"+string(row.Audience)] = row.Versions
		}
		return err
	})
	return versions
}

// A tree's first opening is where its root architect's view of the design begins, and each
// member's first working phase is where its holder's begins: the architect's row is written at the
// versions that opening approves, and each member's holder row at the versions approved when its
// first phase starts. Nothing of those versions is news to either, so the tree owes nothing at its
// release. A later approval moves neither row: the next opening is not the tree's first, and a row
// that had moved with the gate would hide from its holder what that approval changed.
//
// Before the opening nothing is written: a registration whose document no person has approved
// releases nothing, so neither the architect nor any holder has taken a version of it yet.
func TestTheFirstOpeningAndEachFirstEntryTakeTheApprovedVersionsAsTheirBaseline(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	const child = "LEGION-209"

	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	applyMirrored(t, pool, engine, "child-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: gatedRoot, Rank: "V"})
	if got := toldVersions(t, pool, gatedRoot); len(got) != 0 {
		t.Fatalf("the told rows before any approval = %v, want none: nothing has been released", got)
	}

	apply("approve-4", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 7}))
	if got, want := toldVersions(t, pool, gatedRoot), map[string]map[record.DesignStep]int{
		gatedRoot + "/architect": {record.DesignStepSpec: 4},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the told rows at the tree's first opening = %v, want %v: the architect takes the version that released its tree", got, want)
	}

	// The opening starts no phase itself: admission's pass gives each issue whose first phase can
	// start the slot it needs, and that start is each member's first working phase.
	admissionPass(t, pool, engine)
	for _, key := range []string{gatedRoot, child} {
		if got := phaseOf(t, pool, key); got != phase.Planning {
			t.Fatalf("%s = %s after the opening's pass, want planning", key, got)
		}
	}
	released := map[string]map[record.DesignStep]int{
		gatedRoot + "/architect": {record.DesignStepSpec: 4},
		gatedRoot + "/holder":    {record.DesignStepSpec: 4},
		child + "/holder":        {record.DesignStepSpec: 4},
	}
	if got := toldVersions(t, pool, gatedRoot); !reflect.DeepEqual(got, released) {
		t.Fatalf("the told rows once the first phases started = %v, want %v: each holder takes the version approved when its work started", got, released)
	}

	// A new version closes the gate and a person approves it: the gate opens again, at 5, and no row
	// moves, so each is owed the difference between 4 and 5.
	apply("version-5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5})
	apply("approve-5", stepApproval(gatedRoot, gateDoc, 5, record.EventMark{Seq: 9}))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.ApprovedVersion == nil || *gate.ApprovedVersion != 5 {
		t.Fatalf("the spec gate = %+v, want it approved at 5: the opening this case reads never happened", gate)
	}
	if got := toldVersions(t, pool, gatedRoot); !reflect.DeepEqual(got, released) {
		t.Fatalf("the told rows after a later approval = %v, want %v left as they stood", got, released)
	}
}

// laterEntryRoot and laterEntryMember are the tree the later-entry cases stand on: a root waiting
// at admitted while its child is tested.
const laterEntryRoot, laterEntryMember = "LEGION-207", "LEGION-208"

// seedTestedMember records laterEntryMember in testing under laterEntryRoot, whose spec gate stands
// approved at approved, with the pull request and the two phase rows a tester's fail and an
// implementer's completion move it through, and answers the engine and an apply that refuses no
// fact.
func seedTestedMember(t *testing.T, pool *pgxpool.Pool, approved int) (*Engine, func(string, intake.Fact)) {
	t.Helper()
	root, member := laterEntryRoot, laterEntryMember
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root,
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "T"})
	seedIssue(t, pool, record.Issue{Key: member, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root,
		Parent: record.ParentOf(root), Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: gateDoc, LatestVersion: approved, ApprovedVersion: new(approved), EverOpened: true})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: member, Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/" + member, HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: member, Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: member, Role: claim.RoleTester, Claim: "test-claim"})
	engine := testEngine(config.DesignGateRootIssues, nil)
	return engine, func(eventID string, fact intake.Fact) {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "fact", eventID, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		if result.Refusal != nil {
			t.Fatalf("ApplyFact %s refused: %+v", eventID, *result.Refusal)
		}
	}
}

// laterEntryTesterFails is the tester's failed verdict on laterEntryMember, which starts the
// implementer.
var laterEntryTesterFails = intake.HandoffComplete{Generation: 1, Issue: laterEntryMember, Role: claim.RoleTester, Claim: "test-claim", Verdict: "fail",
	HandoffRecord: "test-0", HandoffVersion: 1}

// owedNewsOf is what record.OwedNews owes tree as the record holds it now, over the facts every
// reader of a tree's news reads (record.TreeNewsFacts).
func owedNewsOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, tree string) record.TreeNews {
	t.Helper()
	var news record.TreeNews
	seedRecord(t, pool, func(tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(t.Context(), record.NewStore(), tx, engine.cfg.Steps, tree)
		if err != nil {
			return err
		}
		news = record.OwedNews(facts)
		return nil
	})
	return news
}

// A member's holder row is written at its first working phase's entry and at no later one: a
// later entry's holder inherits what the issue's earlier holders took and is owed what was
// approved since, so a second entry that wrote the gate's version again would hide that news from
// the very holder it is owed to. The member here is a child, whose baseline is its tree root's gate,
// which is the gate its work stands on.
func TestALaterEntryKeepsTheHolderRowItsFirstEntryWrote(t *testing.T) {
	pool := migratedPool(t)
	root, member := laterEntryRoot, laterEntryMember
	engine, apply := seedTestedMember(t, pool, 3)

	// The tester's fail starts the implementer: the first entry this issue's record holds a holder
	// row for, written at the version its tree root's gate stands approved at.
	apply("test-0", laterEntryTesterFails)
	if got := phaseOf(t, pool, member); got != phase.Implementing {
		t.Fatalf("%s = %s after the tester's fail, want implementing", member, got)
	}
	baseline := map[string]map[record.DesignStep]int{member + "/holder": {record.DesignStepSpec: 3}}
	if got := toldVersions(t, pool, root); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("the told rows after the first entry = %v, want %v: the holder takes the version its work starts from", got, baseline)
	}

	// The spec is approved again at 4, and the implementer's completion starts the tester: an entry
	// of an issue whose holder row stands already, which it leaves at 3.
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4), EverOpened: true})
	apply("implement-1", intake.HandoffComplete{Generation: 1, Issue: member, Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fix-1",
		HandoffRecord: "implement-1", HandoffVersion: 1})
	if got := phaseOf(t, pool, member); got != phase.Testing {
		t.Fatalf("%s = %s after the implementer's completion, want testing", member, got)
	}
	if got := toldVersions(t, pool, root); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("the told rows after a later entry = %v, want %v: the tester inherits what was taken and is owed the version since", got, baseline)
	}
	// What that leaves owed is the version approved since, to the holder whose phase stands now.
	news := owedNewsOf(t, pool, engine, root)
	if len(news.Members) != 1 || news.Members[0].Issue != member || !reflect.DeepEqual(news.Members[0].Items,
		[]record.NewsItem{{Step: record.DesignStepSpec, ArtifactID: gateDoc, From: new(3), To: 4}}) {
		t.Fatalf("the news %s's tester is owed = %+v, want the spec from 3 to 4", member, news.Members)
	}
}

// A holder row that knows no version is what the upgrade seeds for work in flight under a gate a
// changes request had closed (20261113212423_design_news): what its holder took is unknown. The
// row stands as it is across the member's next phase entry, which takes no baseline over it, so
// the approval that reopened the gate stays news to the member, owed from a version the record
// does not know (From nil), which the reconcile reads from Dispatch's review history. Without the
// row, that entry would take the reopened version as its baseline and the news would be lost.
func TestAHolderRowThatKnowsNoVersionKeepsItAcrossTheNextEntry(t *testing.T) {
	pool := migratedPool(t)
	root, member := laterEntryRoot, laterEntryMember
	engine, apply := seedTestedMember(t, pool, 4)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().RaiseTold(t.Context(), tx, member, record.AudienceHolder, map[record.DesignStep]int{}, engine.now())
	})

	apply("test-0", laterEntryTesterFails)
	if got := phaseOf(t, pool, member); got != phase.Implementing {
		t.Fatalf("%s = %s after the tester's fail, want implementing", member, got)
	}
	unknown := map[string]map[record.DesignStep]int{member + "/holder": {}}
	if got := toldVersions(t, pool, root); !reflect.DeepEqual(got, unknown) {
		t.Fatalf("the told rows after the next entry = %v, want %v: a row that knows no version takes no baseline", got, unknown)
	}
	news := owedNewsOf(t, pool, engine, root)
	if len(news.Members) != 1 || news.Members[0].Issue != member || !reflect.DeepEqual(news.Members[0].Items,
		[]record.NewsItem{{Step: record.DesignStepSpec, ArtifactID: gateDoc, From: nil, To: 4}}) {
		t.Fatalf("the news %s's implementer is owed = %+v, want the spec at 4 from a version the record does not know", member, news.Members)
	}
}
