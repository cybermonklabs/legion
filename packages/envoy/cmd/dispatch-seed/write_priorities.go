package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
)

// priorityPlan is one priority: the id every address of it carries, the three attributes a person
// writes, and the key its place in the one order is drawn with. It holds no project, because a
// priority belongs to none (INFRA-2947).
type priorityPlan struct {
	ID          string
	Title       string
	Description string
	Owner       string
	Rank        string
}

// priorityDescriptionBytes is how much markdown a priority's description holds. A description is
// markdown of any length, as the route's own text says, so it is a paragraph rather than a
// phrase: a reader of the priority page meets the shape a person writes.
const priorityDescriptionBytes = 400

// advancesPerIssue is how many priorities each issue that states a relation of its own advances.
// Two rather than one, because a deployment holds work that serves more than one goal and the
// reads are written for it: a parked issue's row carries every priority it advances, and the
// issue read answers them as a list. It is far below contracts.MaxIssueAdvances, the most one
// issue may state, which advancesLinks holds every issue to.
const advancesPerIssue = 2

// The two statuses lists the faults of GET /api/v1/priorities read, as SQL literal lists. They
// are api/priority_queries.go's priorityInFlightStatuses and priorityParkedStatuses, restated
// here because that package does not export them, and the report's own reading of the two faults
// (reportDistributions) has to ask what the route asks.
const (
	priorityInFlightStatuses = `'in_progress', 'testing', 'needs_review', 'retro'`
	priorityParkedStatuses   = `'backlog', 'icebox'`
)

// priorityMemberTerms are the issues that advance each priority, as the route resolves them: the
// relations stated now, and every issue below one of those, since a child inherits the priorities
// of the issues above it. It is api/priority_queries.go's priorityMemberTerms over the whole
// database rather than over the relations one request read, and the descent is bounded by
// parentChainCap, the deepest chain this command writes, rather than by the server's
// parentDepthCap: a descendant below that bound is one the seeded corpus does not hold.
//
// It is the terms of a `with recursive`, so a reading of it begins `with recursive ` and ends
// with its own select.
var priorityMemberTerms = `stated as (
		select distinct t.from_id as key, t.to_id as priority
		from priority_tuples t
		where t.from_kind = '` + prioritystore.KindIssue + `' and t.kind = '` + prioritystore.RelationAdvances + `'
			and t.to_kind = '` + prioritystore.KindPriority + `' and t.asserted
	), members_spread as (
		select key, priority, 0 as depth from stated
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m join issues c on c.parent_key = m.key
		where m.depth < ` + fmt.Sprint(parentChainCap) + `
	), members as (
		select distinct key, priority from members_spread
	)`

// writePriorities writes the cross-project priorities and the `advances` relations the corpus's
// issues state on them: the rows GET /api/v1/priorities, the issue read's `advances` and the
// inheritance walk are measured over.
//
// A priority belongs to no project, so the priorities are written once for the whole corpus and
// the work of every project relates to them. Every row here is written through prioritystore,
// the one place in the tree that touches `priority_nodes` and `priority_tuples`: Create writes
// the node with its visibility, its state and each attribute, each appending its own value tuple,
// and Advance appends the one tuple that states a relation. Nothing here writes SQL of its own,
// so a corpus holds what a route would have written.
//
// No event is appended beside them: `events` takes one owner, an issue or a project, and a
// priority is neither, which is what prioritystore.NoEvent says on every row it writes.
func (r *seedRun) writePriorities(ctx context.Context) error {
	priorities := r.priorityPlans()
	if err := r.inBatches(ctx, "priorities", len(priorities), batchRows,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			for index := from; index < to; index++ {
				priority := priorities[index]
				if err := prioritystore.Create(ctx, tx, priority.ID, map[string]string{
					prioritystore.AttributeTitle:       priority.Title,
					prioritystore.AttributeDescription: priority.Description,
					prioritystore.AttributeOwner:       priority.Owner,
					prioritystore.AttributeRank:        priority.Rank,
				}, seedAuthor, timeAt(index, len(priorities))); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
		return err
	}
	links := r.plan.advancesLinks(len(priorities))
	return r.inBatches(ctx, "advances relations", len(links), batchRows,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			for _, link := range links[from:to] {
				issue := r.plan.issues[link[0]]
				if err := prioritystore.Advance(ctx, tx, issue.Key, priorities[link[1]].ID,
					seedAuthor, issue.CreatedAt); err != nil {
					return err
				}
			}
			return nil
		})
}

// priorityPlans is every priority the run writes, in the order it writes them, which is the order
// their keys rank them in. The text comes from the run's own draw, as a component's and an issue's
// title do, so two runs at one scale write the same words.
//
// The order keys are drawn in sequence with the issue board's own rank.Between, the way planIssues
// draws an issue's, rather than read off the rows already written (prioritystore.RankAt). The two
// answer the same keys — that read orders the key as bytes, which is the order rank.Between builds
// one in (prioritystore's rankKey) — and the sequence is what a seed writing every priority at
// once already holds: asking for each key instead would take the one rank allocation lock and read
// the node table again per priority, for the key the one before it was just given.
func (r *seedRun) priorityPlans() []priorityPlan {
	priorities := make([]priorityPlan, r.sizes.Priorities)
	order := ""
	for index := range priorities {
		var description strings.Builder
		description.WriteString("# " + r.plan.vocabulary.phrase(r.random, 4) + "\n\n")
		r.plan.vocabulary.paragraph(&description, r.random, priorityDescriptionBytes)
		order = rank.Between(order, "")
		priorities[index] = priorityPlan{
			// The id is a slug of lowercase letters, digits and single hyphens, which is what
			// the route holds a caller's own id to (api/priorities.go), numbered so the ids sort
			// as the priorities are written.
			ID:          fmt.Sprintf("priority-%03d", index+1),
			Title:       r.plan.vocabulary.phrase(r.random, 5),
			Description: description.String(),
			Owner:       seedAuthor.ID,
			Rank:        order,
		}
	}
	return priorities
}

// advancesLinks is every (issue, priority) pair the seed states, in the order it states them: the
// index of an issue and the index of the priority it advances. The pairs are distinct and no
// issue holds more than contracts.MaxIssueAdvances of them, which is what a write of the relation
// allows, so the corpus holds nothing a route would have refused.
//
// The heavily linked issue comes first, so the widest tree in the corpus hangs under a priority
// and the inheritance walk has the most to carry. The rest are spread over every project with a
// stride, and each issue the stride lands on states advancesPerIssue of them, so the corpus holds
// work that advances more than one priority as a deployment's does, and the issues below each of
// them inherit what it states and state nothing of their own.
func (p *seedPlan) advancesLinks(priorities int) [][2]int {
	total := p.sizes.Advances
	if total <= 0 || priorities <= 0 || len(p.issues) == 0 {
		return nil
	}
	links := make([][2]int, 0, total)
	seen := make(map[[2]int]struct{}, total)
	held := make([]int, len(p.issues))
	add := func(issue, priority int) {
		if len(links) >= total || issue < 0 || issue >= len(p.issues) ||
			held[issue] >= contracts.MaxIssueAdvances {
			return
		}
		pair := [2]int{issue, priority}
		if _, stated := seen[pair]; stated {
			return
		}
		seen[pair] = struct{}{}
		held[issue]++
		links = append(links, pair)
	}
	add(p.hub, 0)
	// The issues the stride lands on are the share of the corpus stating a relation of its own,
	// advancesPerIssue each.
	stride := max(1, advancesPerIssue*len(p.issues)/total)
	// The statuses cycle every len(issueStatuses) issues (planIssues), so a stride that is a
	// multiple of that would state every relation on work of one status: no issue in flight
	// would be left advancing none, and none of the parked work would advance anything, which
	// is both faults of GET /api/v1/priorities left without a row.
	if stride%len(issueStatuses) == 0 {
		stride++
	}
	// Each offset is walked advancesPerIssue times, naming another priority each time, before
	// the walk moves to the next one; a pass that states nothing has landed on issues holding
	// every pair they can, and a whole round of offsets stating nothing is a corpus with no pair
	// left at all, which ends the walk.
	for pass, stalled := 0, 0; len(links) < total && stalled < stride*advancesPerIssue; pass++ {
		stated := len(links)
		offset := pass / advancesPerIssue % stride
		for index := offset; index < len(p.issues) && len(links) < total; index += stride {
			add(index, (index/stride+pass)%priorities)
		}
		if len(links) == stated {
			stalled++
			continue
		}
		stalled = 0
	}
	return links
}
