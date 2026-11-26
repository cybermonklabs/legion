package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The two shapes of the restore, which returns each answered ask to
// the spec it came from and moves every ask block of a decisions document into its spec. In the
// main shape, RestoreReturns, the answered ask's block left its spec and its row still names that
// spec, so the restore writes the block back under its old address. In the second, RestoreMoves,
// an ask's block sits in the issue's decisions document, whatever the ask's state (D1), and the
// restore moves it into the spec, which changes its address: that move is the one a moved_to
// tuple records (graphstore.TupleMovedTo). The store census (`envoy-dispatch census --store`)
// reads both shapes too, in its dangling-citation counter and collision lines (ReadRestoreWork).
const (
	RestoreReturns = "returns"
	RestoreMoves   = "moves"
)

// RestoreWork is one ask the restore owes its issue's spec, as the store holds it before the
// restore writes anything. The store census reads it too, through ReadRestoreWork.
type RestoreWork struct {
	// Shape is RestoreReturns or RestoreMoves.
	Shape string
	// Issue is the issue whose spec the ask returns to, and Ask the ask's id.
	Issue, Ask string
	// Address is the block address the ask's citations name: for RestoreReturns the spec's old
	// address of its block, `<spec ref_key>#<block id>`, which no ask block of the spec holds; for
	// RestoreMoves the address of its block in the decisions document, which that document holds.
	Address string
	// BlockID is the ask's block id, and Spec the spec's ref_key.
	BlockID, Spec string
	// Collides is whether a block of the spec holds BlockID now, so the restore cannot write the
	// ask's block under it and must give it a fresh id (R8's collision census).
	Collides bool
}

// decisionsDocuments is each issue's decisions document that stands, as a FROM item of (id,
// issue_key, ref_key): the catalog function decisions_documents()
// (20261117093337_decisions_documents.up.sql), the one place a decisions document is spelled, by
// its role or, for one stored before the role existed, by its slug. A retired document is none of
// them (artifacts.retired_at): it holds nothing the restore owes.
// kept for one release: decisions-documents-fn
const decisionsDocuments = `decisions_documents()`

// owesBlockIndex is true of the document whose uuid is the SQL expression artifact while it still
// owes its block index (document_block_versions.version null, as typedNodeOwed reads it): its
// document_blocks rows are not current, so what it holds is not known until the index catches up.
func owesBlockIndex(artifact string) string {
	return `exists (select 1 from document_block_versions owed where owed.artifact_id = ` + artifact + ` and owed.version is null)`
}

// answeredAsk is true of the asks row k whose node states it answered now: the newest answer_state
// tuple of the ask's node holds answered and asserts. The node holds the answer and the row is its
// projection (graphstore's TupleAnswerState, asks.Settle), so a row a writer of the release before
// answered counts once the step's backfill has stated its node, as docs' countDecisionStepOwed
// reads the same answer. The read takes graph_tuples_values, keyed by the node and the kind. The
// restore's finder reads it (restoreWorkRead), and so does the store census through that finder.
var answeredAsk = `exists (
		select 1 from (
			select t.to_id, t.asserted from graph_tuples t
			where t.from_kind = '` + graphstore.KindAsk + `' and t.from_id = k.id::text
			  and t.kind = '` + graphstore.TupleAnswerState + `' and t.to_kind = '` + graphstore.KindValue + `'
			order by t.seq desc limit 1
		) newest
		where newest.asserted and newest.to_id = '` + asks.StateAnswered + `')`

// RemovedByAPerson is true of the asks row k whose node's newest removed_by tuple asserts
// (graphstore.TupleRemovedBy): a person took its block out of the document its row names, which
// stays taken, so the restore never writes it back. A block of the ask written back into that
// document retracts it (docs' settlement), and a later removal is judged afresh. The read takes
// graph_tuples_current, keyed by the node and the kind. The restore reads it, and so does the store
// census, through the restore's finder (restoreWorkRead).
var RemovedByAPerson = `exists (
		select 1 from (
			select t.asserted from graph_tuples t
			where t.from_kind = '` + graphstore.KindAsk + `' and t.from_id = k.id::text
			  and t.kind = '` + graphstore.TupleRemovedBy + `' and t.to_kind <> '` + graphstore.KindValue + `'
			order by t.seq desc limit 1
		) newest
		where newest.asserted)`

// restoreWorkRead is the restore's work in one statement, as ReadRestoreWork answers it.
var restoreWorkRead = `
	select work.shape, work.issue_key, work.ask_id, work.address, work.block_id, spec.ref_key,
	       exists (select 1 from document_blocks held where held.artifact_id = spec.id and held.block_id = work.block_id)
	from (
		select '` + RestoreReturns + `' as shape, k.issue_key, k.id::text as ask_id,
		       spec.ref_key || '#' || k.block_id as address, k.block_id, spec.id as spec_id
		from asks k
		join artifacts spec on spec.id = k.block_artifact_id and spec.is_primary and spec.issue_key = k.issue_key
		where k.kind = 'question' and k.block_id is not null and ` + answeredAsk + ` and not ` + RemovedByAPerson + `
		  and not exists (
			select 1 from document_blocks d
			where d.artifact_id = spec.id and d.block_id = k.block_id and d.type = 'ask')
		  and not ` + owesBlockIndex("spec.id") + `
		union all
		select '` + RestoreMoves + `', decisions.issue_key, k.id::text, d.node_id, d.block_id, spec.id
		from ` + decisionsDocuments + ` decisions -- kept for one release: decisions-documents-fn
		join document_blocks d on d.artifact_id = decisions.id and d.type = 'ask'
		join asks k on k.block_artifact_id = decisions.id and block_id_key(k.block_id) = block_id_key(d.block_id)
		join artifacts spec on spec.issue_key = decisions.issue_key and spec.is_primary
		where not ` + owesBlockIndex("decisions.id") + `
		  and not ` + owesBlockIndex("spec.id") + `
	) work
	join artifacts spec on spec.id = work.spec_id
	order by work.issue_key, work.shape, work.address, work.ask_id`

// ReadRestoreWork answers every ask the restore owes, in one read of
// q, ordered by issue, shape and address. It is the one finder of that work: the census's
// dangling-citation counter and collision lines read it here, and the restore, its dry run and
// the step's test for work read the same function.
//
//   - RestoreReturns: an answered `question` ask whose row names its issue's spec
//     (asks.block_artifact_id is the issue's primary document) and whose block the spec does not
//     hold as an ask block - no document_blocks row of type ask holds the ask's block id in it.
//     Address is the spec's old address of that block. An ask is answered as its node holds it
//     (answeredAsk), and an ask a person removed from the spec is none (RemovedByAPerson): the
//     step reads who removed the rest before it writes one back (docs' restore).
//   - RestoreMoves: an ask block a decisions document holds (decisionsDocuments), open, answered,
//     declined, resolved or withdrawn alike (ruling D1: every ask block of that document moves): a
//     document_blocks row of type ask of that document, whose asks row names the document and
//     that block id. Address is the block's address there.
//
// A document that still owes its block index is left out, both the spec and the decisions
// document, until the index catches up: its rows are not current, so neither what it holds nor
// what collides with it is known.
func ReadRestoreWork(ctx context.Context, q graphstore.Queryer) ([]RestoreWork, error) {
	rows, err := q.Query(ctx, restoreWorkRead)
	if err != nil {
		return nil, fmt.Errorf("read the restore's work: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RestoreWork, error) {
		var work RestoreWork
		err := row.Scan(&work.Shape, &work.Issue, &work.Ask, &work.Address, &work.BlockID, &work.Spec, &work.Collides)
		return work, err
	})
}

// Collision is one block id the restore must change: the block id of an ask it owes an issue's
// spec, which a block of that spec already holds.
type Collision struct{ Issue, BlockID string }

// CollisionsOf is the collision census over work, in work's order: each block id that collides
// once for each issue, however many asks of that issue carry it.
func CollisionsOf(work []RestoreWork) []Collision {
	var collisions []Collision
	seen := make(map[Collision]struct{})
	for _, ask := range work {
		if !ask.Collides {
			continue
		}
		collision := Collision{Issue: ask.Issue, BlockID: ask.BlockID}
		if _, listed := seen[collision]; listed {
			continue
		}
		seen[collision] = struct{}{}
		collisions = append(collisions, collision)
	}
	return collisions
}

// The two sets of the dangling-citation counter.
const (
	// DanglingMoved is the citations whose target is a moved_to source - an address a moved_to was
	// ever stated from, asserted now or retracted since, since the tuple is the record of the move
	// (R8) - or an ask block the restore's dry run lists as a move of the second shape
	// (RestoreMoves), and that resolve to no block. It reads zero before the first restore, while
	// each such block stands where it is, and zero after the last one, while every address a move
	// left resolves through its tuple: a move whose tuple no longer asserts leaves its citations
	// dangling, and they count here. A block an edit deleted before this release, which no move
	// made, is no target of it, and neither is a block of a converted section that is no heading,
	// which no moved_to leaves.
	DanglingMoved = "moved"
	// DanglingReturning is the citations whose target is the old spec address of an ask the
	// restore writes back (RestoreReturns, but each the restore declines because a person took its
	// block out: ReturnDeclined), and that resolve to no block: it reads their count before the
	// restore and zero after it.
	DanglingReturning = "returning"
)

// ReturnDeclined answers, reading through q, whether the restore declines to write back the ask of
// work, a return of the main shape (RestoreReturns): whether a person took its block out of the
// spec, which the restore reads off the spec's stored versions before it writes one back (docs'
// removalOf) and records then on the ask's node (RemovedByAPerson). That read parses a stored
// version's tree, which docs reads and graphcensus does not, and docs imports graphcensus, so the
// caller supplies the test: docs.ReturnDeclined, the one the restore reads by.
type ReturnDeclined func(ctx context.Context, q graphstore.Queryer, work RestoreWork) (bool, error)

// ErrNoReturnDeclined is the counter's refusal to judge a return without the restore's test: the
// restore owes a return of the main shape and the caller passed no ReturnDeclined, so the counter
// cannot tell an ask the restore writes back from one a person removed, and guesses neither.
var ErrNoReturnDeclined = errors.New("the restore owes a return and the dangling-citation counter was given no test of which returns it declines (docs.ReturnDeclined)")

// DanglingCitation is one citation the counter counted: the set it is in, the node whose text
// cites the target - a document (its uuid), an ask, a comment or a message - and the target's
// block address.
type DanglingCitation struct {
	Set, SourceKind, SourceID, Target string
}

// DanglingReport is what the counter read: each citation of each set, in the order the read gave
// them.
type DanglingReport struct {
	Citations []DanglingCitation
}

// Count is how many citations the report holds of set.
func (r DanglingReport) Count(set string) int64 {
	var count int64
	for _, citation := range r.Citations {
		if citation.Set == set {
			count++
		}
	}
	return count
}

// danglingRead counts the citations of the two sets in one statement. $1 is the old spec
// addresses of the returns the restore writes back, and $2 the addresses of the moves of the
// second shape it owes (ReadRestoreWork).
//
// A target is a block address: every address a moved_to was ever stated from, asserted now or
// retracted since, read out of every block node the log holds through graph_tuples_current, one
// probe per node (statedOutOfNodes); the ask blocks the restore's dry run lists as moves of the
// second shape; and the addresses the returns name. One that resolves, through moved_to_reach, to
// a block a document_blocks row holds is no dangling target, and neither is one whose moves end at
// an ask whose row stands - the heading of a section that cites an answered ask a person removed,
// which answers that ask - nor one in a document that still owes its block index, whose rows are
// not current.
// The counter reads no other target: a `#b-` citation in prose is free text nothing validates,
// a block of a document past the deferral bound resolves to no block until the index catches up,
// and a block an edit deleted, before this release or since, resolves to none for good, so a count
// of every unresolved citation of the instance is non-zero on a healthy one.
//
// A citation is the node whose text cites a target, read off the five tables a citation of a block
// is stored in, each through its index on (to_kind, to_id): refs, by its source, and block_refs,
// ask_block_refs, comment_block_refs and message_block_refs, by the document, ask, comment or
// message that holds the citing block. A source that cites one target from its own text and from
// one of its blocks, or from two blocks, is one citation, so each set counts the nodes that cite
// each target.
var danglingRead = `
	with recursive ` + statedOutOfNodes("moved", []string{refs.BlockNodeKind}, []string{graphstore.TupleMovedTo}) + `,
	targets (address, counted) as (
		select moved.from_id, '` + DanglingMoved + `' from moved
		union
		select unnest($2::text[]), '` + DanglingMoved + `'
		union
		select unnest($1::text[]), '` + DanglingReturning + `'
	),
	dangling as (
		select t.address, t.counted from targets t
		where (select reach.kind from moved_to_reach(t.address) reach) is null
		  and not exists (
			select 1 from artifacts a join document_block_versions owed on owed.artifact_id = a.id
			where a.ref_key = split_part(t.address, '#', 1) and owed.version is null)
	)
	select distinct d.counted, cited.kind, cited.id, d.address
	from dangling d
	cross join lateral (
		select r.from_kind as kind, r.from_id as id from refs r
		where r.to_kind = '` + refs.BlockNodeKind + `' and r.to_id = d.address
		union all
		select '` + graphstore.KindArtifact + `', b.artifact_id::text from block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `' and b.to_id = d.address
		union all
		select '` + graphstore.KindAsk + `', b.ask_id::text from ask_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `' and b.to_id = d.address
		union all
		select '` + graphstore.KindComment + `', b.comment_id::text from comment_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `' and b.to_id = d.address
		union all
		select '` + graphstore.KindMessage + `', b.message_id::text from message_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `' and b.to_id = d.address
	) cited
	order by d.counted, d.address, cited.kind, cited.id`

// danglingCitations is the dangling-citation counter: the citations of the moved set and of the
// returning set (DanglingMoved, DanglingReturning), read in q over work, the restore's work as the
// caller read it (ReadRestoreWork), so the census reads that work once for the counter and the
// collision lines. Both sets answer before the restore moves anything. A return the restore
// declines (declined, a ReturnDeclined) is no return it makes, and its old address no target: a
// person took the block out, and the address stays empty after the restore as before it. A return
// it cannot judge, since declined is nil, is ErrNoReturnDeclined, naming the ask.
func danglingCitations(ctx context.Context, q graphstore.Queryer, work []RestoreWork, declined ReturnDeclined) (DanglingReport, error) {
	returning, moving := []string{}, []string{}
	for _, ask := range work {
		switch ask.Shape {
		case RestoreMoves:
			moving = append(moving, ask.Address)
		case RestoreReturns:
			if declined == nil {
				return DanglingReport{}, fmt.Errorf("count the dangling citations: ask %s of %s: %w", ask.Ask, ask.Issue, ErrNoReturnDeclined)
			}
			refused, err := declined(ctx, q, ask)
			if err != nil {
				return DanglingReport{}, fmt.Errorf("count the dangling citations: judge the return of ask %s of %s: %w", ask.Ask, ask.Issue, err)
			}
			if !refused {
				returning = append(returning, ask.Address)
			}
		}
	}
	rows, err := q.Query(ctx, danglingRead, returning, moving)
	if err != nil {
		return DanglingReport{}, fmt.Errorf("count the dangling citations: %w", err)
	}
	citations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DanglingCitation, error) {
		var citation DanglingCitation
		err := row.Scan(&citation.Set, &citation.SourceKind, &citation.SourceID, &citation.Target)
		return citation, err
	})
	if err != nil {
		return DanglingReport{}, fmt.Errorf("count the dangling citations: %w", err)
	}
	return DanglingReport{Citations: citations}, nil
}

// writeMoves prints the counter's line, a line for each citation it counted when verbose, and the
// collision census, one line for each colliding block id of each issue. Each line prints counts,
// node addresses and block ids alone.
func writeMoves(w io.Writer, dangling DanglingReport, collisions []Collision, verbose bool) {
	fmt.Fprintf(w, "census: dangling_citations moved=%s returning=%s\n",
		pgmigrate.Thousands(dangling.Count(DanglingMoved)), pgmigrate.Thousands(dangling.Count(DanglingReturning)))
	if verbose {
		for _, citation := range dangling.Citations {
			fmt.Fprintf(w, "census: dangling_citation set=%s from=%s:%s to=%s\n",
				citation.Set, citation.SourceKind, citation.SourceID, citation.Target)
		}
	}
	for _, collision := range collisions {
		fmt.Fprintf(w, "census: collisions issue=%s block=%s\n", collision.Issue, collision.BlockID)
	}
}

// DecisionsCensus is what the census prints of the decisions step's third phase: the decisions
// documents that stand (decisions_documents()), the
// sections they still hold, each one no rule has converted yet, and each spec the restore owes an
// ask - an answered ask that left it, or an ask block of the decisions document, which moves into
// it - with those asks. docs' ReadDecisionsCensus reads it, since only docs reads a decisions
// document's sections; the census command sets it on the report it prints (StoreReport.Decisions).
// It carries ReadDecisionsCensus's answer alone, and goes with it.
//
// kept for one release: decisions-documents-fn
type DecisionsCensus struct {
	Documents, UnconvertedSections int64
	SpecsWithMovedAnswers          []SpecWithMovedAnswers
}

// SpecWithMovedAnswers is one spec the restore owes an ask: its issue, and the id of each ask the
// restore owes it, in the restore's order (ReadRestoreWork).
//
// kept for one release: decisions-documents-fn
type SpecWithMovedAnswers struct {
	Issue string
	Asks  []string
}

// SpecsWithMovedAnswersOf is each spec the restore owes an ask over work, in work's order, each
// once: the issue whose spec an ask of either shape returns or moves to, with its asks.
//
// kept for one release: decisions-documents-fn
func SpecsWithMovedAnswersOf(work []RestoreWork) []SpecWithMovedAnswers {
	var specs []SpecWithMovedAnswers
	for _, ask := range work {
		if len(specs) == 0 || specs[len(specs)-1].Issue != ask.Issue {
			specs = append(specs, SpecWithMovedAnswers{Issue: ask.Issue})
		}
		specs[len(specs)-1].Asks = append(specs[len(specs)-1].Asks, ask.Ask)
	}
	return specs
}

// writeDecisions prints the decisions line, and one line for each spec whose answered asks left
// it, naming the asks by id.
//
// kept for one release: decisions-documents-fn
func writeDecisions(w io.Writer, decisions DecisionsCensus) {
	fmt.Fprintf(w, "census: decisions documents=%s unconverted_sections=%s specs_with_moved_answers=%s\n",
		pgmigrate.Thousands(decisions.Documents), pgmigrate.Thousands(decisions.UnconvertedSections),
		pgmigrate.Thousands(int64(len(decisions.SpecsWithMovedAnswers))))
	for _, spec := range decisions.SpecsWithMovedAnswers {
		fmt.Fprintf(w, "census: spec_with_moved_answers issue=%s asks=%s\n", spec.Issue, strings.Join(spec.Asks, ","))
	}
}
