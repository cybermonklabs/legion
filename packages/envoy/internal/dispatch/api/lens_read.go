package api

// lens_read.go serves GET /api/v1/lenses?on=<address>, the lens read of
// dispatch://INFRA-1950/spec#b-r-lens and #b-r-lens-trigger: the lenses invoked on one node, in
// the order they were invoked, each with its kind, its author, its time, its verdict, its state
// and visibility, the lenses that triggered it and the lenses it triggered, its findings and its
// risks; and each chain of triggered lenses those lenses stand in.
//
// It is a named read of the graph and no traversal: it takes the node and nothing else, so no
// caller chooses a kind, an edge, a direction or a depth. Every statement it issues binds the
// visibility predicate of each kind it answers through refs.VisibilityArg, and the walk of a chain
// steps only along the triggered_by tuples those statements answer, so a lens, a finding or a
// risk the reader may not see is in no list, no chain and no count, and no chain passes through
// one.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// lensChainDepth is the depth the lens read walks one chain of triggered lenses to, counted in
// lenses: a chain lists at most this many, and names the first lens past them where the walk
// reached one. R8 (dispatch://INFRA-1950/spec#b-r-lens-trigger) cuts every chain at 50, and the
// reason is the read's cost: a chain is a run of triggered_by tuples any writer may make as long
// as it likes, and the bound holds one chain to fifty lenses listed and fifty-one loaded whatever
// the store holds, so no chain anybody writes makes the read of its node slow. A cycle is no
// reason for it: the walk's visited set lists each lens once, bound or none.
//
// The depth counts lenses rather than steps away from the lens the walk began at, because a walk
// begins at each lens on the node: a chain whose every lens stands on the node is one step from
// a beginning wherever it is cut, and is as long as the lenses it holds.
const lensChainDepth = 50

// lensStandsOnKinds are the kinds of node a lens may stand on, as decodeNodeAddress names them:
// the kinds of a node under an issue, which is where P9 (dispatch://INFRA-1950/artifact/plan-md)
// lets a lens be invoked - the issue, a document and its blocks, an ask, a comment or a message
// and its blocks, and the lenses, findings, risks and constraints under those, the kinds that
// stand on a node by their part_of tuple alone (refs.StandingKinds) - as the arms of refs'
// backlink counts that count a lens on a node (lensStandsOn) name them. A node of any other
// kind holds no lens, and the read answers it the 404 a node nobody holds answers before it reads
// the store: a person, a session, a role or a page is a node the store holds and every reader may
// see, so answering one 200 would tell any caller whether that person exists, which the person
// page keeps from them (D12b).
var lensStandsOnKinds = append([]string{
	graphstore.KindIssue, graphstore.KindArtifact, refs.BlockNodeKind,
	string(refs.ItemAsk), refs.ItemAsk.BlockKind(), string(refs.ItemComment), refs.ItemComment.BlockKind(),
	string(refs.ItemMessage), refs.ItemMessage.BlockKind(),
}, refs.StandingKinds()...)

// lensesQuery is GET /api/v1/lenses's query: the one node whose lenses are read.
type lensesQuery struct {
	// On is the node's dispatch:// address, or its store address as the /nodes/{id} routes take
	// one (`lens/<uuid>`, a block's node id). A node the caller may not see answers 404, as one
	// that does not exist does.
	On string `query:"on"`
}

// lensReadKind is the kind a lens is of: the id of the lens_kind node its has_kind tuple names,
// and the words a person reads for that kind.
type lensReadKind struct {
	ID    string `json:"id"`
	Words string `json:"words"`
}

// lensFindingRead is one finding of a lens: its address, `dispatch://lens/<uuid>#b-<id>`, its
// text, and its rank among the lens's findings, which is the seq of its part_of tuple: a sparse
// key, so no gap between the ranks a reader sees tells that a finding is hidden from them.
type lensFindingRead struct {
	Address    string `json:"address"`
	Text       string `json:"text"`
	Rank       int64  `json:"rank"`
	State      string `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility string `json:"visibility" enum:"project,author"`
}

// riskRead is one risk under a pre-mortem: its address, `dispatch://risk/<uuid>`, its attributes,
// and the address of each check its prevented_by tuples point at, in the order they were written,
// empty where no check prevents it.
type riskRead struct {
	Address      string   `json:"address"`
	Heading      string   `json:"heading"`
	EarlyWarning string   `json:"early_warning"`
	Prevention   string   `json:"prevention"`
	PreventedBy  []string `json:"prevented_by"`
	State        string   `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility   string   `json:"visibility" enum:"project,author"`
}

// lensRead is one lens as the lens read answers it, whole.
type lensRead struct {
	// Address is the lens's address, `dispatch://lens/<uuid>`.
	Address string `json:"address"`
	// Kind is the lens_kind its has_kind tuple names, null only where the store holds none the
	// reader may see, which no writer of a lens leaves.
	Kind *lensReadKind `json:"kind"`
	// On is the address of the node the lens stands on, the far end of its part_of tuple, which
	// is the log of its invocation; null where the reader may not see that node.
	On *string `json:"on"`
	// Author is the person or the session its authored_by tuple names, the writer
	// graphstore.AuthorOf names of its made_by: the session its body named for a lens written on
	// the shared bearer, and null only where the write named only a workload (spec v46 R12).
	Author    *reasoningAuthor `json:"author"`
	CreatedAt time.Time        `json:"created_at"`
	Verdict   *string          `json:"verdict"`
	// State and Visibility are the store's; a lens reserved for its author reaches that author
	// alone, who reads `author` here.
	State      string `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility string `json:"visibility" enum:"project,author"`
	// TriggeredBy are the lenses its triggered_by tuples point at, and Triggered the lenses whose
	// triggered_by tuples point at it, each by address in the order those tuples were written and
	// each a lens the reader may see. The store holds triggered_by as many-valued, so both are
	// lists; a lens written on one lens's ask holds one.
	TriggeredBy []string `json:"triggered_by"`
	Triggered   []string `json:"triggered"`
	// Findings are its findings in rank order, and Risks its risks in the order of their part_of
	// tuples, each one the reader may see.
	Findings []lensFindingRead `json:"findings"`
	Risks    []riskRead        `json:"risks"`
}

// lensChain is one chain of triggered lenses: the lenses joined to one another by triggered_by
// tuples the reader may see, in the order they were invoked, at most lensChainDepth of them.
type lensChain struct {
	Lenses []lensRead `json:"lenses"`
	// StoppedAt is the lens where the walk stopped: the first lens it reached past the
	// lensChainDepth it listed, which this chain does not list. It is null for a chain the walk
	// listed whole, a chain of exactly lensChainDepth lenses among them.
	StoppedAt *string `json:"stopped_at"`
}

// nodeLenses is what GET /api/v1/lenses answers of one node.
type nodeLenses struct {
	// On is the node read: its address, kind, state and visibility.
	On reasoningEnd `json:"on"`
	// Lens is the node itself, whole, where it is a lens - with the lenses that triggered it and
	// the lenses it triggered, so either end of a trigger reads both - and null otherwise.
	Lens *lensRead `json:"lens"`
	// Lenses are the lenses invoked on the node, in the order they were invoked: the seq of
	// their part_of tuples, so a later write of a lens's row does not move it.
	Lenses []lensRead `json:"lenses"`
	// Chains are the chains of triggered lenses the node's lenses stand in (lensWalk.chains).
	Chains []lensChain `json:"chains"`
}

// lensRowsStatement is the statement that reads lenses for the lens read: each lens's
// graph_nodes row, held, beside its current part_of tuple, placed (to_kind, to_id, seq), which
// source names; the rows narrow keeps that are not gone (gone) and that the reader, bound at
// viewer, may see through the predicate of a lens (refs.VisibleKindRow), in order. Beside each
// ride its stored state and visibility, off the row it holds (refs.StoredRowOf), its verdict, its
// kind with that kind's words (refs.LensKindSQL), the far end of its current authored_by tuple
// (refs.CurrentFarEndSQL), and the lenses at either end of its current triggered_by tuples, each a
// lens that is not gone and that the reader may see, by id in the order those tuples were written.
func lensRowsStatement(source, narrow, order, gone, viewer string) string {
	lens := `'` + graphstore.KindLens + `'`
	return `
	select held.id, placed.seq, placed.to_kind, placed.to_id, held.made_at,
	       ` + refs.StoredRowOf("held") + `,
	       nullif(held.attributes->>'` + graphstore.AttributeVerdict + `', ''),
	       chosen_kind.id, chosen_kind.words, author.to_kind, author.to_id,
	       coalesce(triggers.ids, '{}'), coalesce(triggered.ids, '{}')
	from ` + source + `
	left join lateral (
		` + refs.LensKindSQL("held", `kind_node.id, coalesce(kind_node.attributes->>'`+graphstore.AttributeWords+`', '') as words`, viewer) + `
	) chosen_kind on true
	left join lateral (
		` + refs.CurrentFarEndSQL(`newest.to_kind, newest.to_id`, lens, `held.id`, graphstore.TupleAuthoredBy) + `
	) author on true
	left join lateral (
		select array_agg(linked.to_id order by linked.seq) as ids
		from (
			` + graphstore.CurrentTupleRead(`t.to_id`, `t.to_id, t.seq, t.asserted`,
		`t.from_kind = `+lens+` and t.from_id = held.id and t.kind = '`+graphstore.TupleTriggeredBy+`'
				and t.to_kind = `+lens+` and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) linked
		join graph_nodes far on far.kind = ` + lens + ` and far.id = linked.to_id
		where linked.asserted and far.state <> ` + gone + ` and ` + refs.VisibleKindRow(graphstore.KindLens, "far", viewer) + `
	) triggers on true
	left join lateral (
		select array_agg(linked.from_id order by linked.seq) as ids
		from (
			` + graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.seq, t.asserted`,
		`t.to_kind = `+lens+` and t.to_id = held.id and t.kind = '`+graphstore.TupleTriggeredBy+`'
				and t.from_kind = `+lens+` and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) linked
		join graph_nodes far on far.kind = ` + lens + ` and far.id = linked.from_id
		where linked.asserted and far.state <> ` + gone + ` and ` + refs.VisibleKindRow(graphstore.KindLens, "far", viewer) + `
	) triggered on true
	where ` + narrow + ` and held.state <> ` + gone + ` and ` + refs.VisibleKindRow(graphstore.KindLens, "held", viewer) + order
}

// lensesOnNodeQuery reads the lenses invoked on one node, which their part_of tuples name by the
// kind and the id $1 and $2 (farEndOf): each lens whose current part_of tuple points at that node,
// in the order those tuples were appended, for the reader $4, gone being $3 (lensRowsStatement).
var lensesOnNodeQuery = lensRowsStatement(`(
		`+graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.to_kind, t.to_id, t.seq, t.asserted`,
	`t.to_kind = $1 and t.to_id = $2 and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+graphstore.KindLens+`' and t.to_kind <> '`+graphstore.KindValue+`'`)+`
	) placed
	join graph_nodes held on held.kind = '`+graphstore.KindLens+`' and held.id = placed.from_id`,
	`placed.asserted`, `
	order by placed.seq`, "$3", "$4")

// lensesByIDQuery reads the lenses whose ids are $1, wherever each stands, for the reader $3, gone
// being $2 (lensRowsStatement): the lenses a chain reaches that the read holds no row of yet.
var lensesByIDQuery = lensRowsStatement(`graph_nodes held
	left join lateral (
		`+refs.CurrentFarEndSQL(`newest.to_kind, newest.to_id, newest.seq`, `'`+graphstore.KindLens+`'`, `held.id`, graphstore.TuplePartOf)+`
	) placed on true`,
	`held.kind = '`+graphstore.KindLens+`' and held.id = any($1)`, ``, "$2", "$3")

// lensFindingsQuery reads the findings of the lenses whose ids are $1 for the reader $3: each
// block node whose current part_of tuple points at one of those lenses, that is not gone ($2) and
// that the reader may see through the predicate of a finding (refs.VisibleKindRow), with its text
// (refs.StandingTextSQL) and its stored state and visibility off the row it holds
// (refs.StoredRowOf), by lens and in rank order, the seq of that tuple.
var lensFindingsQuery = `
	select placed.to_id, held.id, placed.seq, ` + refs.StandingTextSQL("held", "$3") + `,
	       ` + refs.StoredRowOf("held") + `
	from (
		` + graphstore.CurrentTupleRead(`t.to_id, t.from_id`, `t.to_id, t.from_id, t.seq, t.asserted`,
	`t.to_kind = '`+graphstore.KindLens+`' and t.to_id = any($1) and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+refs.BlockNodeKind+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) placed
	join graph_nodes held on held.kind = '` + refs.BlockNodeKind + `' and held.id = placed.from_id
	where placed.asserted and held.id like '` + graphstore.KindLens + `/%#%' and held.state <> $2
		and ` + refs.VisibleKindRow(graphstore.KindLensBlock, "held", "$3") + `
	order by placed.to_id, placed.seq`

// lensRisksQuery reads the risks under the lenses whose ids are $1 for the reader $3: each risk
// whose current part_of tuple points at one of those lenses, that is not gone ($2) and that the
// reader may see through the predicate of a risk, with its attributes, its stored state and
// visibility off the row it holds (refs.StoredRowOf), and the far end of each of its current
// prevented_by tuples, in the order they were written. Those far ends are resolved afterwards
// through the block loader (refs.Nodes), which binds the block predicate and answers a block no
// current version holds as none.
var lensRisksQuery = `
	select placed.to_id, held.id,
	       coalesce(held.attributes->>'` + graphstore.AttributeHeading + `', ''),
	       coalesce(held.attributes->>'` + graphstore.AttributeEarlyWarning + `', ''),
	       coalesce(held.attributes->>'` + graphstore.AttributePrevention + `', ''),
	       ` + refs.StoredRowOf("held") + `,
	       coalesce(checks.ids, '{}')
	from (
		` + graphstore.CurrentTupleRead(`t.to_id, t.from_id`, `t.to_id, t.from_id, t.seq, t.asserted`,
	`t.to_kind = '`+graphstore.KindLens+`' and t.to_id = any($1) and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+graphstore.KindRisk+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) placed
	join graph_nodes held on held.kind = '` + graphstore.KindRisk + `' and held.id = placed.from_id
	left join lateral (
		select array_agg(prevented.to_id order by prevented.seq) as ids
		from (
			` + graphstore.CurrentTupleRead(`t.to_id`, `t.to_id, t.seq, t.asserted`,
	`t.from_kind = '`+graphstore.KindRisk+`' and t.from_id = held.id and t.kind = '`+graphstore.TuplePreventedBy+`'
				and t.to_kind = '`+refs.BlockNodeKind+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) prevented
		where prevented.asserted
	) checks on true
	where placed.asserted and held.state <> $2 and ` + refs.VisibleKindRow(graphstore.KindRisk, "held", "$3") + `
	order by placed.to_id, placed.seq`

// lensRow is one lens the read loaded: its answer but for its place and its children, the seq of
// its current part_of tuple and that tuple's far end (placed, where it holds one), and the ids of
// the lenses its triggered_by tuples join it to - those that triggered it, then those it
// triggered - which are the steps of a walk from it.
type lensRow struct {
	id     string
	read   lensRead
	seq    int64
	placed bool
	parent storedEnd
	linked []string
}

// invokedBefore orders two lenses as they were invoked: by the seq of their part_of tuples, a
// lens holding none after every lens holding one, and by id where those tie.
func invokedBefore(a, b *lensRow) int {
	if a.placed != b.placed {
		if a.placed {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(a.seq, b.seq), strings.Compare(a.id, b.id))
}

// lensAddresses is the address of each lens ids names.
func lensAddresses(ids []string) []string {
	addresses := make([]string, len(ids))
	for index, id := range ids {
		addresses[index] = lensAddress(id)
	}
	return addresses
}

// scanLensRows reads the rows a lensRowsStatement answers.
func scanLensRows(rows pgx.Rows, err error) ([]lensRow, error) {
	if err != nil {
		return nil, fmt.Errorf("read lenses: %w", err)
	}
	defer rows.Close()
	var lenses []lensRow
	for rows.Next() {
		var row lensRow
		var seq *int64
		var parentKind, parentID, kindID, kindWords, authorKind, authorID *string
		var stored, triggeredBy, triggered []string
		if err := rows.Scan(&row.id, &seq, &parentKind, &parentID, &row.read.CreatedAt, &stored, &row.read.Verdict,
			&kindID, &kindWords, &authorKind, &authorID, &triggeredBy, &triggered); err != nil {
			return nil, fmt.Errorf("scan a lens: %w", err)
		}
		state, visibility, err := refs.StoredStateOf("lens "+row.id, stored)
		if err != nil {
			return nil, err
		}
		row.read.Address, row.read.State, row.read.Visibility = lensAddress(row.id), state, visibility
		row.read.CreatedAt = row.read.CreatedAt.UTC()
		if seq != nil && parentKind != nil && parentID != nil {
			row.seq, row.placed, row.parent = *seq, true, storedEnd{kind: *parentKind, id: *parentID}
		}
		if kindID != nil && kindWords != nil {
			row.read.Kind = &lensReadKind{ID: *kindID, Words: *kindWords}
		}
		if authorKind != nil && authorID != nil {
			row.read.Author = &reasoningAuthor{Kind: *authorKind, ID: *authorID}
		}
		row.read.TriggeredBy, row.read.Triggered = lensAddresses(triggeredBy), lensAddresses(triggered)
		row.linked = append(slices.Clip(triggeredBy), triggered...)
		lenses = append(lenses, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lenses: %w", err)
	}
	return lenses, nil
}

// lensWalk is one read's walk of the chains of triggered lenses: every lens row it has loaded, by
// id, nil for one it asked for that is gone or that the reader may not see.
type lensWalk struct {
	ctx    context.Context
	q      refs.Queryer
	viewer any
	rows   map[string]*lensRow
}

// load reads the rows of the lenses of ids the walk has not asked for yet, in one statement.
func (w *lensWalk) load(ids []string) error {
	var missing []string
	for _, id := range ids {
		if _, asked := w.rows[id]; !asked {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	loaded, err := scanLensRows(w.q.Query(w.ctx, lensesByIDQuery, missing, graphstore.StateGone, w.viewer))
	if err != nil {
		return err
	}
	for _, id := range missing {
		w.rows[id] = nil
	}
	for index := range loaded {
		w.rows[loaded[index].id] = &loaded[index]
	}
	return nil
}

// walkedChain is one chain as the walk found it: the ids of its lenses, and the lens it stopped
// at, where it did.
type walkedChain struct {
	ids       []string
	stoppedAt *string
}

// chains walks the chains of triggered lenses from seeds, the lenses the read answers in their
// own right - the node itself where it is a lens, then the lenses on the node in the order they
// were invoked - each already loaded. A chain is the lenses joined to one another by triggered_by
// tuples the reader may see, walked in both directions, which is what lets either end of a
// trigger read the other.
//
// Each seed that no chain has listed begins a chain, and the walk lists from it, breadth first,
// each lens it reaches that no chain has listed, the visited set being the lenses listed so far,
// so a lens is in one chain at most and a cycle is read once. The walk steps only along the
// triggered_by tuples a lens's row answers, each of which binds the reader's predicate: it passes
// through no lens the reader may not see, so a lens reached only through a hidden one is in no
// chain of that reader's, and a lens on the node that only a hidden lens joins to another is in
// none either, the lenses list answering it all the same. A chain stops at lensChainDepth lenses
// and names the next lens it reached; a lens on the node past that cut is a seed no chain listed,
// and begins a chain of its own. A chain is answered only where it holds two lenses or more: a
// lens no tuple the reader may see joins to another is in no chain.
func (w *lensWalk) chains(seeds []string) ([]walkedChain, error) {
	listed := map[string]bool{}
	var chains []walkedChain
	for _, seed := range seeds {
		if listed[seed] || w.rows[seed] == nil {
			continue
		}
		chain, err := w.chainFrom(seed, listed)
		if err != nil {
			return nil, err
		}
		if len(chain.ids) < 2 {
			continue
		}
		slices.SortStableFunc(chain.ids, func(a, b string) int { return invokedBefore(w.rows[a], w.rows[b]) })
		chains = append(chains, chain)
	}
	return chains, nil
}

// chainFrom walks the chain seed stands in (chains), marking each lens it lists in listed.
func (w *lensWalk) chainFrom(seed string, listed map[string]bool) (walkedChain, error) {
	var chain walkedChain
	queued := map[string]bool{seed: true}
	for level := []string{seed}; len(level) > 0; {
		var next []string
		for index, id := range level {
			if _, asked := w.rows[id]; !asked {
				// This lens and the ones after it at this depth, as many as the chain may still
				// list and one more to tell whether it stops, in one statement.
				if err := w.load(level[index:min(len(level), index+lensChainDepth-len(chain.ids)+1)]); err != nil {
					return walkedChain{}, err
				}
			}
			row := w.rows[id]
			if row == nil {
				continue
			}
			if len(chain.ids) == lensChainDepth {
				chain.stoppedAt = &row.read.Address
				return chain, nil
			}
			chain.ids = append(chain.ids, id)
			listed[id] = true
			for _, linked := range row.linked {
				if !queued[linked] && !listed[linked] {
					queued[linked] = true
					next = append(next, linked)
				}
			}
		}
		level = next
	}
	return chain, nil
}

// lensChildren is the findings and the risks of the lenses whose ids are ids, by lens, each one
// the reader bound at viewer may see, with the ids of the checks each risk's prevented_by tuples
// point at, by risk address.
func lensChildren(ctx context.Context, q refs.Queryer, viewer any, ids []string) (map[string][]lensFindingRead, map[string][]riskRead, map[string][]string, error) {
	findings, risks, checks := map[string][]lensFindingRead{}, map[string][]riskRead{}, map[string][]string{}
	rows, err := q.Query(ctx, lensFindingsQuery, ids, graphstore.StateGone, viewer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the findings of lenses: %w", err)
	}
	for rows.Next() {
		var lens, id string
		var finding lensFindingRead
		var stored []string
		if err := rows.Scan(&lens, &id, &finding.Rank, &finding.Text, &stored); err != nil {
			rows.Close()
			return nil, nil, nil, fmt.Errorf("scan a finding of a lens: %w", err)
		}
		state, visibility, err := refs.StoredStateOf("finding "+id, stored)
		if err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		_, blockID, _ := strings.Cut(id, "#")
		finding.Address, finding.State, finding.Visibility = refs.BlockRef(lensAddress(lens), blockID), state, visibility
		findings[lens] = append(findings[lens], finding)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("read the findings of lenses: %w", err)
	}
	rows, err = q.Query(ctx, lensRisksQuery, ids, graphstore.StateGone, viewer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the risks of lenses: %w", err)
	}
	for rows.Next() {
		var lens, id string
		var risk riskRead
		var stored, prevented []string
		if err := rows.Scan(&lens, &id, &risk.Heading, &risk.EarlyWarning, &risk.Prevention, &stored, &prevented); err != nil {
			rows.Close()
			return nil, nil, nil, fmt.Errorf("scan a risk of a lens: %w", err)
		}
		state, visibility, err := refs.StoredStateOf("risk "+id, stored)
		if err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		risk.Address, risk.State, risk.Visibility = "dispatch://"+graphstore.KindRisk+"/"+id, state, visibility
		checks[risk.Address] = prevented
		risks[lens] = append(risks[lens], risk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("read the risks of lenses: %w", err)
	}
	return findings, risks, checks, nil
}

// readLenses is the lens read of the node address names, for who (nodeLenses). The node is
// decoded as the /nodes/{id} routes decode theirs (decodeNodeAddress: a dispatch:// address by
// text.ParseRef and refs.TargetOf, or a store address) and read through the registry with who's
// visibility (refs.Node), so an address that decodes to nothing, a node of a kind no lens stands on
// (lensStandsOnKinds), a node nobody holds and a node who may not see all answer one 404
// NOT_FOUND, in the same words but the address. A kind no lens stands on is refused before
// anything is read, so that answer is the same whatever the store holds.
//
// Its statements, beside the node's own: the lenses on the node; the lenses a chain reaches that
// stand elsewhere, one statement for each depth of a walk that meets any; the findings and the
// risks of every lens answered, one statement each; and one call of the registry (refs.Nodes)
// for the place of every lens answered that stands elsewhere than on the node or on a lens the
// read holds, and every check a risk names.
func readLenses(ctx context.Context, q refs.Queryer, who model.Actor, address string) (nodeLenses, error) {
	notFound := errorf(http.StatusNotFound, "NOT_FOUND", "no node %q", address)
	kind, id, ok := decodeNodeAddress(address)
	if !ok || !slices.Contains(lensStandsOnKinds, kind) {
		return nodeLenses{}, notFound
	}
	node, found, err := refs.Node(ctx, q, who, kind, id, true)
	if err != nil {
		return nodeLenses{}, err
	}
	if !found {
		return nodeLenses{}, notFound
	}
	on := farEndOf(node, address)
	walk := lensWalk{ctx: ctx, q: q, viewer: refs.VisibilityArg(who), rows: map[string]*lensRow{}}
	placed, err := scanLensRows(q.Query(ctx, lensesOnNodeQuery, on.kind, on.id, graphstore.StateGone, walk.viewer))
	if err != nil {
		return nodeLenses{}, err
	}
	var seeds []string
	isLens := node.Kind == graphstore.KindLens
	if isLens {
		if err := walk.load([]string{node.ID}); err != nil {
			return nodeLenses{}, err
		}
		if walk.rows[node.ID] == nil {
			// Gone, or no longer the reader's to see, since refs.Node read it.
			return nodeLenses{}, notFound
		}
		seeds = append(seeds, node.ID)
	}
	for index := range placed {
		walk.rows[placed[index].id] = &placed[index]
		seeds = append(seeds, placed[index].id)
	}
	chains, err := walk.chains(seeds)
	if err != nil {
		return nodeLenses{}, err
	}

	answered := slices.Clone(seeds)
	for _, chain := range chains {
		answered = append(answered, chain.ids...)
	}
	slices.Sort(answered)
	answered = slices.Compact(answered)
	findings, risks, checks, err := lensChildren(ctx, q, walk.viewer, answered)
	if err != nil {
		return nodeLenses{}, err
	}
	// The place of each lens that stands elsewhere than the node read. A lens standing on a lens
	// the read holds a row of names that lens's address: the row passed the reader's predicate and
	// is not gone, which is what the lens's own loader asks of its parent. Any other lens is read
	// through its own loader, which answers its parent only where the reader may see that parent;
	// the node itself, where it is a lens, carries its own already. And the checks each risk
	// names, through theirs.
	onAHeldLens := func(row *lensRow) *lensRow {
		if row.parent.kind != graphstore.KindLens {
			return nil
		}
		return walk.rows[row.parent.id]
	}
	var targets []refs.Target
	for _, id := range answered {
		if row := walk.rows[id]; row.placed && row.parent != on && !(isLens && id == node.ID) && onAHeldLens(row) == nil {
			targets = append(targets, refs.Target{Kind: graphstore.KindLens, ID: id})
		}
	}
	for _, ids := range checks {
		for _, check := range ids {
			targets = append(targets, refs.Target{Kind: refs.BlockNodeKind, ID: check})
		}
	}
	var resolved map[refs.Target]model.GraphNode
	if len(targets) > 0 {
		if resolved, err = refs.Nodes(ctx, q, who, true, targets); err != nil {
			return nodeLenses{}, err
		}
	}

	whole := func(id string) lensRead {
		row := walk.rows[id]
		lens := row.read
		switch {
		case isLens && id == node.ID:
			lens.On = parentAddress(node.Parent)
		case row.placed && row.parent == on:
			lens.On = addressOrNull(node.Ref)
		case row.placed && onAHeldLens(row) != nil:
			lens.On = addressOrNull(onAHeldLens(row).read.Address)
		case row.placed:
			lens.On = parentAddress(resolved[refs.Target{Kind: graphstore.KindLens, ID: id}].Parent)
		}
		lens.Findings = findings[id]
		if lens.Findings == nil {
			lens.Findings = []lensFindingRead{}
		}
		lens.Risks = []riskRead{}
		for _, risk := range risks[id] {
			risk.PreventedBy = []string{}
			for _, check := range checks[risk.Address] {
				if held, found := resolved[refs.Target{Kind: refs.BlockNodeKind, ID: check}]; found && held.Ref != "" {
					risk.PreventedBy = append(risk.PreventedBy, held.Ref)
				}
			}
			lens.Risks = append(lens.Risks, risk)
		}
		return lens
	}
	answer := nodeLenses{On: endOf(node), Lenses: make([]lensRead, len(placed)), Chains: make([]lensChain, len(chains))}
	if isLens {
		lens := whole(node.ID)
		answer.Lens = &lens
	}
	for index := range placed {
		answer.Lenses[index] = whole(placed[index].id)
	}
	for index, chain := range chains {
		answer.Chains[index] = lensChain{Lenses: make([]lensRead, len(chain.ids)), StoppedAt: chain.stoppedAt}
		for at, id := range chain.ids {
			answer.Chains[index].Lenses[at] = whole(id)
		}
	}
	return answer, nil
}

// parentAddress is the address of parent, null where there is none or it has none.
func parentAddress(parent *model.NodeParent) *string {
	if parent == nil {
		return nil
	}
	return addressOrNull(parent.Ref)
}

// addressOrNull is address, null where it is empty: a node with no address, an issue-less
// message's.
func addressOrNull(address string) *string {
	if address == "" {
		return nil
	}
	return &address
}

// listLenses is GET /api/v1/lenses?on=<address>: the lens read of one node (readLenses), as the
// caller may see it. A query naming no node is 400 INVALID_QUERY.
func (s *server) listLenses(w http.ResponseWriter, r *http.Request) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	on := strings.TrimSpace(r.URL.Query().Get("on"))
	if on == "" {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"on is required: the dispatch:// address of the node whose lenses are read")
		return
	}
	answer, err := readLenses(r.Context(), s.deps.Store.Pool, actor.Recorded(), on)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
