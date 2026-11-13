package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// newsRoot is the root of the tree every case of OwedNews's table stands on.
const newsRoot = "LEGION-100"

// newsSpecDoc and newsCriteriaDoc are the root's primary and Verification documents.
const (
	newsSpecDoc     = "doc-spec"
	newsCriteriaDoc = "doc-criteria"
)

// newsMember is a member of newsRoot's tree standing in at, in the workflow.
func newsMember(key string, at phase.Phase) Issue {
	return Issue{Key: key, Tree: newsRoot, Phase: at, Status: "in_progress", Generation: 1}
}

// newsGate is a row of step at its document's latest version, approved at approved (0 for none).
func newsGate(step DesignStep, latest, approved int) DesignGate {
	document := newsSpecDoc
	if step == DesignStepCriteria {
		document = newsCriteriaDoc
	}
	gate := DesignGate{Issue: newsRoot, Step: step, ArtifactID: document, LatestVersion: latest, EverOpened: true}
	if approved > 0 {
		gate.ApprovedVersion = new(approved)
	}
	return gate
}

// newsRows is gates by step.
func newsRows(gates ...DesignGate) map[DesignStep]DesignGate {
	rows := map[DesignStep]DesignGate{}
	for _, gate := range gates {
		rows[gate.Step] = gate
	}
	return rows
}

// holderTook and architectTook are the told rows of issue's holder and of the root's architect.
func holderTook(issue string, versions map[DesignStep]int) DesignTold {
	return DesignTold{Issue: issue, Audience: AudienceHolder, Versions: versions}
}

func architectTook(versions map[DesignStep]int) DesignTold {
	return DesignTold{Issue: newsRoot, Audience: AudienceArchitect, Versions: versions}
}

// specNews and criteriaNews are the items of a step's document from a told version to the approved
// one; a nil from is a version the record does not know.
func specNews(from *int, to int) NewsItem {
	return NewsItem{Step: DesignStepSpec, ArtifactID: newsSpecDoc, From: from, To: to}
}

func criteriaNews(from *int, to int) NewsItem {
	return NewsItem{Step: DesignStepCriteria, ArtifactID: newsCriteriaDoc, From: from, To: to}
}

// OwedNews is the one rule for what a tree owes of its approved design, and each row of this table
// is one of its conditions: the step must be one the tree is gated on, the standing gate open, the
// approved version above what the holder took or a version the record does not know, the step one
// that concerns the member, the member in the workflow with a phase someone holds in a tree that
// does not linger, and its pull request not merged. The root architect's row is weighed on its own
// rule beside the members'.
func TestOwedNewsOwesEachHolderTheVersionsItHasNotTaken(t *testing.T) {
	specOnly := []DesignStep{DesignStepSpec}
	child, sibling, third := "LEGION-101", "LEGION-102", "LEGION-103"
	until := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	lingering := newsMember(newsRoot, phase.Merging)
	lingering.LingerUntil = &until
	heldImplementing := newsMember(sibling, phase.Held)
	heldImplementing.Hold = &Hold{From: phase.Implementing}
	heldFromMerging := newsMember(child, phase.Held)
	heldFromMerging.Hold = &Hold{From: phase.Merging}
	done := newsMember(third, phase.Done)
	done.Status = "done"

	for _, tc := range []struct {
		name  string
		facts NewsFacts
		want  TreeNews
	}{
		{
			name: "a spec change owes every member with a holder, and the root architect",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing), newsMember(child, phase.Testing), newsMember(sibling, phase.Reviewing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4}), holderTook(sibling, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{
				Members: []MemberNews{
					{Issue: newsRoot, Phase: phase.Implementing, Items: []NewsItem{specNews(new(4), 5)}},
					{Issue: child, Phase: phase.Testing, Items: []NewsItem{specNews(new(4), 5)}},
					{Issue: sibling, Phase: phase.Reviewing, Items: []NewsItem{specNews(new(4), 5)}},
				},
				Architect: []NewsItem{specNews(new(4), 5)},
			},
		},
		{
			name: "a criteria change owes the root alone and no child",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5), newsGate(DesignStepCriteria, 3, 3)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing), newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 5, DesignStepCriteria: 2}),
					holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 5, DesignStepCriteria: 2}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 5, DesignStepCriteria: 2})}},
			want: TreeNews{
				Members:   []MemberNews{{Issue: newsRoot, Phase: phase.Implementing, Items: []NewsItem{criteriaNews(new(2), 3)}}},
				Architect: []NewsItem{criteriaNews(new(2), 3)},
			},
		},
		{
			name: "the standing gate closed by a new version owes nothing",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 6, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing), newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{},
		},
		{
			name: "the first of two approvals owes nothing",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5), newsGate(DesignStepCriteria, 3, 2)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing), newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 2}),
					holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 2}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{},
		},
		{
			name: "the second of two approvals owes both, once",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5), newsGate(DesignStepCriteria, 3, 3)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing), newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 2}),
					holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 2}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{
				Members: []MemberNews{
					{Issue: newsRoot, Phase: phase.Implementing, Items: []NewsItem{specNews(new(4), 5), criteriaNews(new(2), 3)}},
					{Issue: child, Phase: phase.Implementing, Items: []NewsItem{specNews(new(4), 5)}},
				},
				Architect: []NewsItem{specNews(new(4), 5), criteriaNews(new(2), 3)},
			},
		},
		{
			name: "admitted, awaiting_person, done and held owe nothing",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.AwaitingPerson), newsMember(child, phase.Admitted), heldImplementing, done},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 5}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4}), holderTook(sibling, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(third, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{},
		},
		{
			name: "a merged root in merging, a member held from merging and one still reviewing whose pull request merged owe nothing",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Merging), heldFromMerging, newsMember(sibling, phase.Reviewing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 5}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4}), holderTook(sibling, map[DesignStep]int{DesignStepSpec: 4})},
				Merged: map[string]bool{newsRoot: true, child: true, sibling: true}},
			want: TreeNews{},
		},
		{
			name: "a change to the checks alone on a merged root reaches its architect and nobody else",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5), newsGate(DesignStepCriteria, 3, 3)),
				Members: []Issue{newsMember(newsRoot, phase.Merging), newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 5, DesignStepCriteria: 2}),
					holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 5, DesignStepCriteria: 2}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 5})},
				Merged: map[string]bool{newsRoot: true}},
			want: TreeNews{Architect: []NewsItem{criteriaNews(new(2), 3)}},
		},
		{
			name: "a lingering tree owes nothing, its architect included",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{lingering, newsMember(child, phase.Implementing)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{},
		},
		{
			name: "an unknown baseline owes, with no version to start from",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Admitted), newsMember(child, phase.Implementing), newsMember(sibling, phase.Testing)},
				Told:    []DesignTold{holderTook(sibling, map[DesignStep]int{})}},
			want: TreeNews{
				Members: []MemberNews{
					{Issue: child, Phase: phase.Implementing, Items: []NewsItem{specNews(nil, 5)}},
					{Issue: sibling, Phase: phase.Testing, Items: []NewsItem{specNews(nil, 5)}},
				},
				Architect: []NewsItem{specNews(nil, 5)},
			},
		},
		{
			name: "a version taken while none was approved owes from 0",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 2, 2)),
				Members: []Issue{newsMember(child, phase.Implementing)},
				Told:    []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 2}), holderTook(child, map[DesignStep]int{DesignStepSpec: 0})}},
			want: TreeNews{Members: []MemberNews{{Issue: child, Phase: phase.Implementing, Items: []NewsItem{specNews(new(0), 2)}}}},
		},
		{
			name: "a member released after the approval is not owed it: its baseline is that version",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Admitted), newsMember(child, phase.Planning)},
				Told:    []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4}), holderTook(child, map[DesignStep]int{DesignStepSpec: 5})}},
			want: TreeNews{Architect: []NewsItem{specNews(new(4), 5)}},
		},
		{
			name: "accepting is held by the member's owning architect, the root's or a sub-architect's",
			facts: NewsFacts{Steps: specOnly, Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Accepting), newsMember(child, phase.Accepting)},
				Told: []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 5}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4}),
					holderTook(child, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{Members: []MemberNews{
				{Issue: newsRoot, Phase: phase.Accepting, HeldByArchitect: true, Items: []NewsItem{specNews(new(4), 5)}},
				{Issue: child, Phase: phase.Accepting, HeldByArchitect: true, Items: []NewsItem{specNews(new(4), 5)}},
			}},
		},
		{
			name: "a deployment that gates no step owes nothing",
			facts: NewsFacts{Gates: newsRows(newsGate(DesignStepSpec, 5, 5)),
				Members: []Issue{newsMember(newsRoot, phase.Implementing)},
				Told:    []DesignTold{architectTook(map[DesignStep]int{DesignStepSpec: 4}), holderTook(newsRoot, map[DesignStep]int{DesignStepSpec: 4})}},
			want: TreeNews{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OwedNews(tc.facts); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("OwedNews = %+v, want %+v", got, tc.want)
			}
		})
	}
}
