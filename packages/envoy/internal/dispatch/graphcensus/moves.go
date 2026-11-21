package graphcensus

import (
	"context"
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
// tuple records (graphstore.TupleMovedTo).
const (
	RestoreReturns = "returns"
	RestoreMoves   = "moves"
)

// RestoreWork is one ask the restore owes its issue's spec, as the store holds it before the
// restore writes anything.
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
// issue_key, ref_key): by its role or, for one stored before the role existed, by its slug
// (refs.DecisionsDocumentSQL). A retired document is none of them (artifacts.retired_at): it holds
// nothing the restore owes. The dangling-citation counter reads the retired ones too
// (decisionsDocumentsEver), since an address one of them held stays cited after the retirement.
// The census reads the rule rather than the function decisions_documents() that holds it too
// (20261117093337_decisions_documents.up.sql): the boot's retirement drops that function once it
// answers no document, and the census reads on after.
var decisionsDocuments = `(select a.id, a.issue_key, a.ref_key from artifacts a where a.retired_at is null and ` +
	refs.DecisionsDocumentSQL("a") + `)`

// decisionsDocumentsEver is decisionsDocuments with every retired decisions document among them,
// found by the slug the retirement leaves it.
var decisionsDocumentsEver = `(select a.id, a.issue_key, a.ref_key from artifacts a where ` + refs.DecisionsDocumentSQL("a") + `)`

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
// reads the same answer. The read takes graph_tuples_values, keyed by the node and the kind.
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
// graph_tuples_current, keyed by the node and the kind.
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
		from ` + decisionsDocuments + ` decisions
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
	// DanglingMoved is the citations whose target is a moved_to source, or a block address a
	// decisions document held - the document a move of the second shape and the conversion empty,
	// standing or retired since - and that resolve to no block. It reads zero before the first
	// restore, while each such block stands where it is, and zero after the last one, while every
	// address a move left resolves through its tuple: a move that wrote no tuple leaves its
	// citations dangling in the decisions document, before its retirement and after it, and they
	// count here.
	DanglingMoved = "moved"
	// DanglingReturning is the citations whose target is the old spec address of an ask the
	// restore writes back (RestoreReturns), and that resolve to no block: it reads their count
	// before the restore and zero after it.
	DanglingReturning = "returning"
)

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
// addresses of the returns (ReadRestoreWork).
//
// A target is a block address: the moved_to sources the store states now, read out of every block
// node the log holds through graph_tuples_current, one probe per node (currentOutOfNodes); each
// block address a decisions document held at a version the block index recorded
// (document_block_texts, keyed by the document), whether the document stands or was retired since
// (decisionsDocumentsEver), since the restore and the conversion retire it in the transaction that
// moves its blocks out; and the addresses the returns name. One that resolves, through
// moved_to_reach, to a block a document_blocks row holds is no dangling target, and neither is one
// whose moves end at an ask whose row stands - the heading of a section that cites an answered ask
// a person removed, which answers that ask - nor one in a document that still owes its block
// index, whose rows are not current.
// The counter reads no other target: a `#b-` citation in prose is free text nothing validates,
// and a block of a document past the deferral bound resolves to no block until the index catches
// up, so a count of every unresolved citation of the instance is non-zero on a healthy one. A
// decisions document's address its versions never held is such a prose citation too.
//
// A citation is the node whose text cites a target, read off the five tables a citation of a block
// is stored in, each through its index on (to_kind, to_id): refs, by its source, and block_refs,
// ask_block_refs, comment_block_refs and message_block_refs, by the document, ask, comment or
// message that holds the citing block. A source that cites one target from its own text and from
// one of its blocks, or from two blocks, is one citation, so each set counts the nodes that cite
// each target.
var danglingRead = `
	with recursive ` + currentOutOfNodes("moved", []string{refs.BlockNodeKind}, []string{graphstore.TupleMovedTo}) + `,
	targets (address, counted) as (
		select moved.from_id, '` + DanglingMoved + `' from moved
		union
		select distinct decisions.ref_key || '#' || held.block_id, '` + DanglingMoved + `'
		from ` + decisionsDocumentsEver + ` decisions join document_block_texts held on held.artifact_id = decisions.id
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

// DanglingCitations is the dangling-citation counter: the citations
// of the moved set and of the returning set (DanglingMoved, DanglingReturning), read in q over the
// restore's work as ReadRestoreWork finds it. Both sets answer before the restore moves anything.
func DanglingCitations(ctx context.Context, q graphstore.Queryer) (DanglingReport, error) {
	work, err := ReadRestoreWork(ctx, q)
	if err != nil {
		return DanglingReport{}, err
	}
	return danglingCitationsOver(ctx, q, work)
}

// danglingCitationsOver is DanglingCitations over work already read, so the census reads the
// restore's work once for the counter and the collision lines. The moves of work need no address
// of their own: each is a block of a decisions document, which the moved set reads whole.
func danglingCitationsOver(ctx context.Context, q graphstore.Queryer, work []RestoreWork) (DanglingReport, error) {
	returning := []string{}
	for _, ask := range work {
		if ask.Shape == RestoreReturns {
			returning = append(returning, ask.Address)
		}
	}
	rows, err := q.Query(ctx, danglingRead, returning)
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

// DecisionsCensus is what the census prints of the decisions documents an earlier release stored:
// the ones that stand (decisionsDocuments), the
// sections they still hold, each one no rule has converted yet, and each spec the restore owes an
// ask - an answered ask that left it, or an ask block of the decisions document, which moves into
// it - with those asks. docs' ReadDecisionsCensus reads it, since only docs reads a decisions
// document's sections; the census command sets it on the report it prints (StoreReport.Decisions).
type DecisionsCensus struct {
	Documents, UnconvertedSections int64
	SpecsWithMovedAnswers          []SpecWithMovedAnswers
}

// SpecWithMovedAnswers is one spec the restore owes an ask: its issue, and the id of each ask the
// restore owes it, in the restore's order (ReadRestoreWork).
type SpecWithMovedAnswers struct {
	Issue string
	Asks  []string
}

// SpecsWithMovedAnswersOf is each spec the restore owes an ask over work, in work's order, each
// once: the issue whose spec an ask of either shape returns or moves to, with its asks.
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
func writeDecisions(w io.Writer, decisions DecisionsCensus) {
	fmt.Fprintf(w, "census: decisions documents=%s unconverted_sections=%s specs_with_moved_answers=%s\n",
		pgmigrate.Thousands(decisions.Documents), pgmigrate.Thousands(decisions.UnconvertedSections),
		pgmigrate.Thousands(int64(len(decisions.SpecsWithMovedAnswers))))
	for _, spec := range decisions.SpecsWithMovedAnswers {
		fmt.Fprintf(w, "census: spec_with_moved_answers issue=%s asks=%s\n", spec.Issue, strings.Join(spec.Asks, ","))
	}
}
