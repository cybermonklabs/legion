package refs

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// IdentityKinds are the node kinds an identity mention points at: a person, an agent's session and
// a role (R5). A mention of one is a `mentions` tuple from the comment, issue message or document
// block that holds it to the identity's node, in the store alone: the graph's refs and block_refs
// rows hold no party (TargetOf), so a build of the release before this one, which reconciles those
// rows by deleting every row of its source (that build's replaceRefs and replaceBlockRefs),
// deletes none of these. This release's index of the text states each and retracts each the text
// no longer names (identityMentions, unnamedMentions).
var IdentityKinds = []string{graphstore.KindPerson, graphstore.KindSession, graphstore.KindRole}

// identityTargets is each identity a text's references point at, once each in the order the text
// first names it: a person by their canonical login (model.CanonicalLogin), so the spellings a
// text can write of one person meet at one node, an agent's session by its id and a role by its
// token. A party a code context holds names nobody - the text shows the address rather than
// naming its party, as every renderer of the record draws it and as the comment route's party set
// reads it (api.commentPartiesOf) - and is none of them.
func identityTargets(located []text.Located) []Target {
	var found []Target
	for _, mention := range located {
		if mention.InCode || !mention.IsParty() {
			continue
		}
		target := Target{Kind: mention.Kind, ID: mention.ID}
		switch mention.Kind {
		case graphstore.KindPerson:
			target.ID = model.CanonicalLogin(mention.ID)
		case graphstore.KindSession, graphstore.KindRole:
		default:
			continue
		}
		if target.ID == "" || slices.Contains(found, target) {
			continue
		}
		found = append(found, target)
	}
	return found
}

// identityMentions plans the `mentions` tuples from fromKind/fromID to each of targets the store
// does not hold from that node now (standing), made by who at at, each split to the writer that
// appends it (stateMention). A re-index of unchanged text appends nothing, and neither does a
// second write of the same text in one transaction (standing is read, and then held, by the index
// that plans it). An end past the store's address bound is no tuple, and a node reserved for its
// author mentions no person but that author (mentionReaches).
func identityMentions(
	fromKind, fromID string,
	visibility string,
	who model.Actor,
	at time.Time,
	targets []Target,
	standing map[Target]struct{},
) (tuples []graphstore.UncausedTuple, people []graphstore.Tuple) {
	for _, target := range targets {
		if _, held := standing[target]; held || !graphstore.AddressFits(target.ID) || !mentionReaches(visibility, who, target) {
			continue
		}
		tuples, people = stateMention(tuples, people, fromKind, fromID, target, who, at, true)
	}
	return tuples, people
}

// unnamedMentions plans the retraction of each mention the store holds from fromKind/fromID now
// (standing) whose identity targets, what the node's text names now, does not name, made by who at
// at, in (kind, id) order, each split as identityMentions splits its own (stateMention). So the
// identity mentions out of a node follow its text, as its other edges do (PlanItemReplace, the
// links of the block index): once an edit or a delete takes a mention out of the text, the
// mention no longer stands. kept is the identities the node mentions apart from its text - a
// comment's delivery routes, which its comment_mentions rows record (api's commentMentionFacts) -
// and no change of the text retracts one of them. What a mention caused stands: the delivered_to
// and the participates_in a mention of a person brought (R2, R4) are facts of their own, which no
// retraction here touches. A node reserved for its author holds no mention of a person but that
// author, so it retracts no other (mentionReaches).
func unnamedMentions(
	fromKind, fromID string,
	visibility string,
	who model.Actor,
	at time.Time,
	targets []Target,
	standing, kept map[Target]struct{},
) (tuples []graphstore.UncausedTuple, people []graphstore.Tuple) {
	order := func(first, second Target) int {
		return cmp.Or(strings.Compare(first.Kind, second.Kind), strings.Compare(first.ID, second.ID))
	}
	for _, target := range slices.SortedFunc(maps.Keys(standing), order) {
		if _, keeps := kept[target]; keeps || slices.Contains(targets, target) || !mentionReaches(visibility, who, target) {
			continue
		}
		tuples, people = stateMention(tuples, people, fromKind, fromID, target, who, at, false)
	}
	return tuples, people
}

// mentionReaches is whether a mention out of a node of visibility, made by who, may name target:
// a node reserved for its author (graphstore.VisibilityAuthor) mentions no person but the author
// who proved (R2, graphstore.AuthorIsPerson), since such a tuple would be mail its person can
// never read, and graphstore.ToPerson, which asks the same rule, refuses it, asserted or
// retracted.
func mentionReaches(visibility string, who model.Actor, target Target) bool {
	if target.Kind != graphstore.KindPerson || visibility != graphstore.VisibilityAuthor {
		return true
	}
	return graphstore.AuthorIsPerson(who, target.ID)
}

// stateMention appends the mentions tuple from fromKind/fromID to target, made by who at at,
// asserting it or, where asserted is false, retracting it, to the facts of the writer that
// appends it: a person's to people, since graphstore.ToPerson is the one writer of a tuple that
// names a person (graphstore.Uncaused.People), and a session's or a role's to tuples, with the
// node's other facts. Either takes the id of the event it is stated under as its
// legacy_source_seq, a session's and a role's because LegacyFromCause marks them and a person's
// because graphstore.QueueCaused gives one to every fact of People, and a writer with no write
// phase dates both alike (graphstore.Cause): graph_edges' identity arm answers that as the edge's
// source_seq, so a ?since= read of References whose window holds the event lists the mention, and
// one stated at the sentinel (graphstore.NoEvent), by a pass no event records, carries none and
// stays out of that read.
func stateMention(tuples []graphstore.UncausedTuple, people []graphstore.Tuple, fromKind, fromID string, target Target, who model.Actor, at time.Time, asserted bool) ([]graphstore.UncausedTuple, []graphstore.Tuple) {
	tuple := graphstore.Tuple{
		FromKind: fromKind, FromID: fromID, Kind: graphstore.TupleMentions,
		ToKind: target.Kind, ToID: target.ID, At: at, Who: who, Asserted: asserted,
	}
	if target.Kind == graphstore.KindPerson {
		return tuples, append(people, tuple)
	}
	return append(tuples, graphstore.UncausedTuple{Tuple: tuple, LegacyFromCause: true}), people
}

// statedMentions is each identity mention among tuples, asserted or retracted, the facts an index
// states with its node's others: the session and role mentions identityMentions and
// unnamedMentions planned.
func statedMentions(tuples []graphstore.UncausedTuple) []graphstore.Tuple {
	var mentions []graphstore.Tuple
	for _, held := range tuples {
		if held.Kind == graphstore.TupleMentions && slices.Contains(IdentityKinds, held.ToKind) {
			mentions = append(mentions, held.Tuple)
		}
	}
	return mentions
}

// mentionsAfter is held, the identities the store holds a mention of out of one node, once the
// mentions of stated out of that node are appended: each one it asserts added and each one it
// retracts taken out. It changes held and answers it, or a new map where held is nil and stated
// asserts one.
func mentionsAfter(held map[Target]struct{}, stated []graphstore.Tuple) map[Target]struct{} {
	for _, tuple := range stated {
		target := Target{Kind: tuple.ToKind, ID: tuple.ToID}
		if !tuple.Asserted {
			delete(held, target)
			continue
		}
		if held == nil {
			held = map[Target]struct{}{}
		}
		held[target] = struct{}{}
	}
	return held
}

// ItemIdentityMentions is the mentions tuple out of the comment or the message kind/itemID to each
// identity its text body names, as the item index states one of this release's own write of that
// text (PlanItemIndex, identityMentions): read off the references the text spells out, by the
// grammar every indexing path reads (identityTargets), a person by their canonical login, an
// agent's session by its id and a role by its token, a mention in a code context naming nobody;
// refused to a node of visibility reserved for its author but for that author, and to an identity
// past the store's bound; made by who, the node's maker, at `at`, the write's moment; and to none
// of standing, the identities the store holds a mention of out of the node already. No identity's
// address is a short id, a bare key or an `#id`, the forms a reader resolves against the database
// (Reader.Mentions), or a page of the dashboard, so the references body writes out
// (text.ExtractAt) name every identity the whole grammar names, whatever deployment it is read
// for. It answers the tuples split by the writer that appends them - a session's and a role's
// beside the node's other facts (graphstore.Write), a person's through graphstore.ToPerson - each
// stated under no event: the caller states each under the write's own event, dated by it
// (graphstore.Cause.Append), as this release's write of the text states its own (stateMention,
// graphstore.QueueCaused). It plans no retraction: what it states is what the write of the text
// named, and the retraction of what a later text no longer names is ItemUnnamedMentions'.
//
// The backfill reads it for a comment or an issue message an older build wrote, and for an edit
// that build made of either (graphcensus' commentPointersPass, messagesPass and its pass over the
// queue of that build's events): that build states none of these tuples and stores no row of a
// person's mention (TargetOf), so the item's own text is the one record of whom it names.
func ItemIdentityMentions(kind ItemKind, itemID, visibility string, who model.Actor, at time.Time, body string, standing []Target) (identities, people []graphstore.Tuple) {
	named := identityTargets(text.ExtractAt(body, ""))
	if len(named) == 0 {
		return nil, nil
	}
	stated, people := identityMentions(string(kind), itemID, visibility, who, at, named, targetSet(standing))
	return statedMentions(stated), people
}

// ItemUnnamedMentions is the retraction of each identity mention out of the comment or the
// message kind/itemID that its text body does not name, as the item index retracts one of this
// release's own write of that text (PlanItemIndex, unnamedMentions): each of standing, the
// identities the store holds a mention of out of the item's node, that no reference body spells
// out names, read by the grammar ItemIdentityMentions reads, but those kept names - a comment's
// delivery routes, its comment_mentions rows (CommentRoutesRead), which no change of its text
// retracts - and none of a person out of a node of visibility reserved for its author but that
// author (mentionReaches); made by who, the node's maker, at `at`, the write's moment, in (kind,
// id) order, split by the writer that appends them, and stated under no event, as
// ItemIdentityMentions states its own, for the caller to state under the write's. What a
// retracted mention caused stands: the delivered_to and the participates_in a mention of a person
// brought are facts of their own, which no retraction touches.
//
// The backfill reads it for an edit or a delete an older build made of a comment or an issue
// message (graphcensus' pass over the queue of that build's events), against the text the item
// holds when the pass runs: that build retracts no mention, and a later write of the text, of that
// build or of this release, is what the item's mentions follow once the pass has run.
func ItemUnnamedMentions(kind ItemKind, itemID, visibility string, who model.Actor, at time.Time, body string, standing, kept []Target) (identities, people []graphstore.Tuple) {
	if len(standing) == 0 {
		return nil, nil
	}
	stated, people := unnamedMentions(string(kind), itemID, visibility, who, at, identityTargets(text.ExtractAt(body, "")),
		targetSet(standing), targetSet(kept))
	return statedMentions(stated), people
}

// targetSet is targets as a set, each person by their canonical login (model.CanonicalLogin), the
// form identityTargets reads a text's person in and graphstore.ToPerson stores one in.
func targetSet(targets []Target) map[Target]struct{} {
	held := make(map[Target]struct{}, len(targets))
	for _, target := range targets {
		if target.Kind == graphstore.KindPerson {
			target.ID = model.CanonicalLogin(target.ID)
		}
		held[target] = struct{}{}
	}
	return held
}

// blockIdentityMentions plans the identity mentions of a document version's blocks: each
// identity a reference of located names, from the innermost of nodes holding its start, as a
// block's other mentions are placed (blockPairs), where that block is a node of the store (fits,
// beside each of nodes), and where the store holds no mention of it out of that block's node
// already (standing, by block id); and the retraction of each mention standing holds out of a
// block that the version's text of that block no longer names, a block the version no longer
// holds naming none (unnamedMentions). A block's node is made by the version's writer and read
// under its project's visibility (blockNode), which is who and what each mention is stated under,
// dated at, the version's time. It also answers what each block holds once these are appended,
// which a later index of the document in the same transaction plans against (BlockIndexing.Held).
func blockIdentityMentions(
	refKey string,
	nodes []model.ArtifactBlock,
	fits []bool,
	located []text.Located,
	standing map[string]map[Target]struct{},
	at time.Time,
	writer model.Actor,
) (tuples []graphstore.UncausedTuple, people []graphstore.Tuple, held map[string]map[Target]struct{}) {
	held = make(map[string]map[Target]struct{}, len(standing))
	for blockID, targets := range standing {
		held[blockID] = maps.Clone(targets)
	}
	parties := make([]text.Located, 0, len(located))
	for _, mention := range located {
		if !mention.InCode && mention.IsParty() {
			parties = append(parties, mention)
		}
	}
	starts := make([]int, len(parties))
	for index, mention := range parties {
		starts[index] = mention.Offset
	}
	var order []string
	named := map[string][]text.Located{}
	if len(parties) > 0 {
		for index, holder := range innermostBlocks(nodes, starts) {
			if holder < 0 || !fits[holder] {
				continue
			}
			blockID := nodes[holder].ID
			if _, seen := named[blockID]; !seen {
				order = append(order, blockID)
			}
			named[blockID] = append(named[blockID], parties[index])
		}
	}
	state := func(blockID string, blockTuples []graphstore.UncausedTuple, blockPeople []graphstore.Tuple) {
		tuples, people = append(tuples, blockTuples...), append(people, blockPeople...)
		if mentions := mentionsAfter(held[blockID], slices.Concat(statedMentions(blockTuples), blockPeople)); len(mentions) > 0 {
			held[blockID] = mentions
		} else {
			delete(held, blockID)
		}
	}
	for _, blockID := range order {
		blockTuples, blockPeople := identityMentions(BlockNodeKind, blockNodeAddress(refKey, blockID),
			graphstore.VisibilityProject, writer, at, identityTargets(named[blockID]), standing[blockID])
		state(blockID, blockTuples, blockPeople)
	}
	// In block id order, so the retractions come in an order a reader of the log can predict,
	// which a map's iteration would not give.
	for _, blockID := range slices.Sorted(maps.Keys(standing)) {
		blockTuples, blockPeople := unnamedMentions(BlockNodeKind, blockNodeAddress(refKey, blockID),
			graphstore.VisibilityProject, writer, at, identityTargets(named[blockID]), standing[blockID], nil)
		state(blockID, blockTuples, blockPeople)
	}
	return tuples, people, held
}

// IdentityMentionsRead is the select of the identities the store holds a mention of from one
// node now, its kind and id the SQL expressions kind and id - numbered parameters, or columns of
// the statement it is spliced into, which must not be a table aliased t, the alias the read takes
// for graph_tuples - and the identity kinds the array kinds (IdentityKinds): the newest `mentions`
// tuple of each pair of ends, kept only while it asserts (graphstore.CurrentTupleRead), since the
// index retracts a mention its node's text no longer names (unnamedMentions) and an asserted tuple
// older than that retraction stands no more. It reads graph_tuples_current, whose predicate the
// kind's literal and the far end's `<> 'value'` prove, and answers (to_kind, to_id), each once.
func IdentityMentionsRead(kind, id, kinds string) string {
	return `select newest.to_kind, newest.to_id from (` +
		graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = `+kind+` and t.from_id = `+id+` and t.kind = '`+graphstore.TupleMentions+`'
			  and t.to_kind = any(`+kinds+`) and t.to_kind <> '`+graphstore.KindValue+`'`) + `) newest
		where newest.asserted`
}

// CommentRoutesRead is the select of the identities the delivery routes of one comment name, its
// id the SQL expression id - a numbered parameter, or a column of the statement it is spliced into,
// which must not be a table aliased m or routed, the aliases the read takes: the session or the
// role each of its comment_mentions rows targets (graphstore.RouteSQL), and the session that row
// resolved to, each a mention the comment's write states apart from its text (api's
// commentMentionFacts), which no change of its text retracts (unnamedMentions' kept). It answers
// (to_kind, to_id), once for each row and end that names one.
func CommentRoutesRead(id string) string {
	role, session := graphstore.RouteSQL(`m.target`)
	return `select routed.to_kind, routed.to_id
		from comment_mentions m
		cross join lateral (values
			('` + graphstore.KindRole + `', ` + role + `),
			('` + graphstore.KindSession + `', ` + session + `),
			('` + graphstore.KindSession + `', m.resolved_session_id)
		) routed (to_kind, to_id)
		where m.comment_id = ` + id + ` and routed.to_id is not null`
}
