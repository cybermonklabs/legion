package evals

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestATargetChangeWritesANewVersionAndCopiesEveryOtherFieldForward: a register write that sets
// the target alone writes version 2, which holds that target and every field version 1 held; a
// write that changes nothing writes no version; and the versions read newest first.
func TestATargetChangeWritesANewVersionAndCopiesEveryOtherFieldForward(t *testing.T) {
	r := newRig(t)
	first, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "review-rounds", Caller: alice,
		Name: Set("Review rounds"), Question: Set("How many rounds does a pull request take?"), Unit: Set("rounds"),
		Direction: Set(DirectionLowerBetter), TargetJudges: Set([]string{"rounds.median"}),
		Links:  Set([]Link{{Rel: "describedby", Href: "https://wiki.example/review-rounds", Label: "the rule"}}),
		Series: []SeriesChange{{Series: "rounds", Agg: Set(AggQuantile)}},
	})
	if err != nil {
		t.Fatalf("register the eval: %v", err)
	}
	target := 2.0
	second, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-rounds", Caller: implementer, Target: Set(&target)})
	if err != nil {
		t.Fatalf("set the target: %v", err)
	}
	unchanged, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-rounds", Caller: alice, Target: Set(&target)})
	if err != nil {
		t.Fatalf("set the same target again: %v", err)
	}
	versions, err := r.registry.Versions(t.Context(), first.Definition.EvalID)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	t.Logf("%d versions; v1 %+v; v2 %+v", len(versions), first.Definition, second.Definition)
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("the eval holds versions %v, want 2 then 1", versionNumbers(versions))
	}
	if !second.Wrote || unchanged.Wrote || unchanged.Definition.Version != 2 {
		t.Errorf("the target write wrote %v, the repeat wrote %v at version %d; want a version for the change and none for the repeat",
			second.Wrote, unchanged.Wrote, unchanged.Definition.Version)
	}
	v1, v2 := versions[1], versions[0]
	if v1.Target != nil || v2.Target == nil || *v2.Target != 2 {
		t.Errorf("the target is %v at version 1 and %v at version 2, want off then 2", v1.Target, v2.Target)
	}
	copied := v1
	copied.Version, copied.Target, copied.WrittenBy, copied.CreatedAt = v2.Version, v2.Target, v2.WrittenBy, v2.CreatedAt
	if !sameDefinition(copied, v2) || v2.WrittenVia != ViaRegister || v2.WrittenBy.ID != implementer.ID {
		t.Errorf("version 2 is %+v, want version 1's fields with the target set, written by the implementer through the register write", v2)
	}
	if v2.Name != "Review rounds" || v2.Unit != "rounds" || v2.Direction != DirectionLowerBetter || len(v2.Links) != 1 ||
		!v2.Links[0].RecordedAt.Equal(v1.Links[0].RecordedAt) || v2.DocBlock != v1.DocBlock || v2.DocBlock == "" {
		t.Errorf("version 2 did not carry version 1's name, unit, direction, link and doc block forward: %+v", v2)
	}
}

// TestTwoRegisterWritesAtOnceLandAsTheNextTwoVersions: two register writes of one eval, on two
// connections at once, both succeed, as versions 2 and 3, and version 3 holds both writes'
// fields. The test holds the eval's lock until both writes wait for it.
func TestTwoRegisterWritesAtOnceLandAsTheNextTwoVersions(t *testing.T) {
	r := newRig(t)
	first, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "queue-depth", Caller: alice, Name: Set("Queue depth")})
	if err != nil {
		t.Fatalf("register the eval: %v", err)
	}
	holder, err := r.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(t.Context())
	if _, err := holder.Exec(t.Context(), `select pg_advisory_xact_lock(hashtext('eval:' || $1))`, first.Definition.EvalID); err != nil {
		t.Fatalf("hold the eval's lock: %v", err)
	}
	target := 10.0
	changes := []Registration{
		{Project: r.project, Slug: "queue-depth", Caller: alice, Target: Set(&target)},
		{Project: r.project, Slug: "queue-depth", Caller: alice, Unit: Set("issues")},
	}
	failures := make([]error, len(changes))
	var writers sync.WaitGroup
	for index, change := range changes {
		writers.Go(func() { _, failures[index] = r.registry.Register(t.Context(), change) })
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, time.Minute); waiting < 2 {
		t.Fatalf("%d register writes wait for the eval's lock, want both", waiting)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("release the eval's lock: %v", err)
	}
	writers.Wait()
	for index, err := range failures {
		if err != nil {
			t.Errorf("register write %d: %v", index, err)
		}
	}
	versions, err := r.registry.Versions(t.Context(), first.Definition.EvalID)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if len(versions) != 3 || versions[0].Version != 3 || versions[1].Version != 2 || versions[0].Target == nil {
		t.Fatalf("the eval holds versions %v, want 3, 2 and 1, the newest with a target", versionNumbers(versions))
	}
	t.Logf("versions %v; newest target %v, unit %q", versionNumbers(versions), *versions[0].Target, versions[0].Unit)
	if newest := versions[0]; newest.Target == nil || *newest.Target != 10 || newest.Unit != "issues" || newest.Name != "Queue depth" {
		t.Errorf("version 3 is %+v, want both writes' target and unit and version 1's name", newest)
	}
}

// TestDocTextReplacesTheSectionsMethodAndNamesTheNewBlock: a register write's doc_text becomes
// the method block of the eval's section of evals-md, and the version it writes names that
// block; a second doc_text replaces the block, whose id the next version names, and the first
// block and its text are gone, under the one heading of the eval.
func TestDocTextReplacesTheSectionsMethodAndNamesTheNewBlock(t *testing.T) {
	r := newRig(t)
	firstText := "Counts each pull request's review rounds.\n\n- A round is one review that requests changes."
	first, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "rework", Caller: alice, DocText: Set(firstText)})
	if err != nil {
		t.Fatalf("register the eval with its method: %v", err)
	}
	text, blocks := r.document(t)
	if !hasBlock(blocks, first.Definition.DocBlock) || !strings.Contains(text, "A round is one review that requests changes.") {
		t.Fatalf("doc_block %q names no block of evals-md holding the method, which reads:\n%s", first.Definition.DocBlock, text)
	}
	secondText := "Counts the rounds after the first review.\n\n:::callout{title=\"Why\"}\nThe first review always happens.\n:::"
	second, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "rework", Caller: alice, DocText: Set(secondText)})
	if err != nil {
		t.Fatalf("replace the method: %v", err)
	}
	text, blocks = r.document(t)
	t.Logf("doc_block %s then %s; evals-md reads:\n%s", first.Definition.DocBlock, second.Definition.DocBlock, text)
	if second.Definition.Version != 2 || second.Definition.DocBlock == first.Definition.DocBlock {
		t.Errorf("the second method wrote version %d naming block %s, want version 2 naming a new block", second.Definition.Version, second.Definition.DocBlock)
	}
	if !hasBlock(blocks, second.Definition.DocBlock) || hasBlock(blocks, first.Definition.DocBlock) {
		t.Errorf("evals-md holds the new block %v and the old one %v, want the new alone",
			hasBlock(blocks, second.Definition.DocBlock), hasBlock(blocks, first.Definition.DocBlock))
	}
	if strings.Contains(text, "requests changes") || !strings.Contains(text, "rounds after the first review") || !strings.Contains(text, "always happens") {
		t.Errorf("evals-md does not hold the second method in place of the first:\n%s", text)
	}
	if n := headings(text, "rework"); n != 1 {
		t.Errorf("evals-md holds %d headings of rework, want 1", n)
	}
}

func versionNumbers(versions []Definition) []int {
	numbers := make([]int, len(versions))
	for index, version := range versions {
		numbers[index] = version.Version
	}
	return numbers
}
