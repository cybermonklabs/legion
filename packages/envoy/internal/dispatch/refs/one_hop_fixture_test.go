package refs

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The one-hop fixture is the graph of INFRA-1098's shape that the references read is held to
// (newOneHopFixture), and planCounts is what TestTheReferencesReadReadsOnlyTheIssuesOwnLinks reads
// of the plans of the statements the read sends over it.

// oneHopLayers is the number of documents at each depth of INFRA-1098's closure on 2026-11-20 at
// 01:32Z, depth 1 first: the closure the walk this read replaces held the database with.
var oneHopLayers = []int{25, 97, 112, 391, 1307, 2108, 1808, 1264}

// oneHopMinimumRows is the fewest rows each table a recorded statement reads holds, so that the
// planner plans the default settings on tables of a real size, most of whose rows are outside the
// issue.
const oneHopMinimumRows = 10000

// oneHopTables is every table the one-hop fixture writes and the columns it writes of each, in
// the order it copies them.
var oneHopTables = []copyTable{
	{"projects", []string{"key", "name", "default_assignee"}},
	{"architecture_sources", []string{"project_key", "id", "repo", "branch", "installation_id", "created_by"}},
	{"architecture_snapshots", []string{"id", "project_key", "commit", "files"}},
	{"issues", []string{"key", "project_key", "number", "title", "created_by", "rank", "parent_key"}},
	{"events", []string{"issue_key", "seq", "type", "actor", "payload", "notify", "published_at"}},
	{"artifacts", []string{"id", "issue_key", "project_key", "slug", "name", "kind", "created_by", "ref_key"}},
	{"artifact_versions", []string{"artifact_id", "number", "authors"}},
	{"document_indexed_versions", []string{"artifact_id", "version"}},
	{"document_blocks", []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at"}},
	{"document_block_texts", []string{"artifact_id", "block_id", "version", "text"}},
	{"asks", []string{"id", "issue_key", "author", "question"}},
	{"ask_blocks", []string{"ask_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "changed_revision", "changed_at"}},
	{"comments", []string{"id", "issue_key", "author", "body"}},
	{"comment_blocks", []string{"comment_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "changed_revision", "changed_at"}},
	{"messages", []string{"id", "issue_key", "author", "body"}},
	{"message_blocks", []string{"message_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_at"}},
	{"components", []string{"project_key", "id", "title", "prose", "parent", "snapshot_id", "source_id"}},
	{"component_facts", []string{"project_key", "id", "made_at", "changed_at"}},
	{"refs", []string{"from_kind", "from_id", "to_kind", "to_id", "kind"}},
	{"block_refs", []string{"artifact_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_version", "to_types"}},
	{"ask_block_refs", []string{"ask_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
	{"comment_block_refs", []string{"comment_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
	{"message_block_refs", []string{"message_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
	{"graph_nodes", []string{"kind", "id", "supertag", "visibility", "state", "made_at", "made_by", "source_event"}},
}

// fixtureAuthors is the authors of every version of the one-hop fixture, whose rows are each made
// by edgesFixtureActor.
const fixtureAuthors = `[{"kind":"user","id":"alice"}]`

// seededUUID is a version 4 uuid made of the next two draws of random, so that a fixture seeded
// alike writes the same ids.
func seededUUID(random *rand.Rand) uuid.UUID {
	var id uuid.UUID
	binary.BigEndian.PutUint64(id[:8], random.Uint64())
	binary.BigEndian.PutUint64(id[8:], random.Uint64())
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// fixtureDocument is one document of a fixture: its id, its ref_key, which a link to it stores,
// and its name, the words the graph read shows for it.
type fixtureDocument struct {
	id     uuid.UUID
	refKey string
	name   string
}

// fixtureBlock is one block a fixture document holds: its id and its type.
type fixtureBlock struct{ id, kind string }

// relatedRows gathers the rows one field of the references read must answer, by the node at the
// far end: each with the distinct kinds of its links and the lowest via of their near ends.
type relatedRows map[[2]string]*relatedRow

type relatedRow struct {
	name  string
	kinds []string
	via   model.ReferenceVia
}

func (rows relatedRows) add(kind, id, name, linkKind string, via model.ReferenceVia) {
	key := [2]string{kind, id}
	row, found := rows[key]
	if !found {
		rows[key] = &relatedRow{name: name, kinds: []string{linkKind}, via: via}
		return
	}
	if !slices.Contains(row.kinds, linkKind) {
		row.kinds = append(row.kinds, linkKind)
		slices.Sort(row.kinds)
	}
	if viaBefore(via, row.via) {
		row.via = via
	}
}

// summaries is every row as relatedSummary writes it, sorted.
func (rows relatedRows) summaries() []string {
	summaries := make([]string, 0, len(rows))
	for key, row := range rows {
		summaries = append(summaries, relatedSummary(key[0], key[1], row.name, row.kinds, row.via))
	}
	slices.Sort(summaries)
	return summaries
}

// viaBefore is whether via a sorts before via b in the database's order: by kind, then by id.
// Every id the fixtures compare within one kind is a uuid, an issue key or a ref_key that differ
// first in a letter or a digit, which every collation orders as bytes do.
func viaBefore(a, b model.ReferenceVia) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.ID < b.ID
}

// relatedSummary is one row of links or linked_from as the tests compare it.
func relatedSummary(kind, id, name string, kinds []string, via model.ReferenceVia) string {
	return fmt.Sprintf("%s %s %q [%s] via %s %s", kind, id, name, strings.Join(kinds, ","), via.Kind, via.ID)
}

// oneHopFixture is the graph of INFRA-1098's shape that the references read is held to.
type oneHopFixture struct {
	database *store.Store
	issue    string
	// links and linkedFrom are the rows each field must hold, as relatedSummary writes them,
	// sorted; members is the documents of links, each at depth 1 with its via and of kind artifact
	// at its own address (documentMember), by ref_key.
	links, linkedFrom []string
	members           []Member
	// own is every node of the issue and its own content as a graph node names it, which neither
	// field may list.
	own map[[2]string]bool
	// reached is the ref_key of each document of the first layer, every one of which the issue's
	// own content links to.
	reached []string
	// namedKinds is the number of node kinds the two fields name, each of which the read may
	// send one statement to name.
	namedKinds int
}

// newOneHopFixture writes in bulk (loadFixture) the graph of INFRA-1098's closure on 2026-11-20
// at 01:32Z.
//
// Its out side is eight layers of documents of the sizes of oneHopLayers. Document j of layer k+1
// is cited by document (j mod the size of layer k) of layer k, and every document cites three
// documents of the next layer and three of the earlier layers, chosen from a fixed seed. The first
// document of the first layer belongs to the issue's parent and holds a requirement block. The
// issue's own content cites the 25 documents of the first layer: a comment no block index has
// reached, a block of a comment, a message no block index has reached, a block of a message, an
// ask's question, an option part of the ask and the issue's two documents, and a check block of
// the issue's spec points with serves at the parent's requirement block. The comment also cites
// an issue of another project and its ask, the issue itself and the issue's spec.
//
// whole adds the rest of c-one-hop's fixture. The in side: 400 sources outside the issue, which
// write 600 links into the issue node and every kind of its own content; 30 of them are check
// blocks of the issue's children's documents pointing serves at its requirement blocks, and 200
// others write two links each. The outside graph: 10,000 issues of another project with their
// events, 3,000 of their documents citing the targets the issue's spec cites and each other,
// 10,000 asks, comments and messages, each with a block citing a document of the first layer or
// another of its kind, and 10,000 components with their facts, so that every table a recorded
// statement reads holds oneHopMinimumRows or more. Without whole, the check block's link is a
// plain mention of the requirement block from the spec's first block, since a fixture main's
// statement is compared on holds no typed pointer.
func newOneHopFixture(t *testing.T, whole bool) oneHopFixture {
	t.Helper()
	database := storetest.Open(t)
	built := time.Now()
	at := time.Date(2026, 11, 20, 1, 32, 0, 0, time.UTC)
	random := rand.New(rand.NewPCG(4704, 1098))
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}

	rows := map[string][][]any{}
	add := func(table string, values ...any) { rows[table] = append(rows[table], values) }
	written := map[[4]string]bool{}
	ref := func(fromKind, fromID, toKind, toID string) {
		key := [4]string{fromKind, fromID, toKind, toID}
		if !written[key] {
			written[key] = true
			add("refs", fromKind, fromID, toKind, toID, "mentions")
		}
	}
	names := map[[2]string]string{}
	issue := func(key, project string, number int, title, parent string) {
		add("issues", key, project, number, title, edgesFixtureActor, fmt.Sprintf("%s%08d", project, number), nullable(parent))
		if whole {
			add("events", key, 1, "issue.created", edgesFixtureActor, "{}", "false", at)
		}
		names[[2]string{"issue", key}] = title
	}
	documents := map[string]fixtureDocument{}
	document := func(owner, project, slug, name string, blocks ...fixtureBlock) fixtureDocument {
		prefix := project
		if owner != "" {
			prefix = owner
		}
		doc := fixtureDocument{id: seededUUID(random), refKey: prefix + "/" + slug, name: name}
		id := doc.id.String()
		add("artifacts", id, nullable(owner), project, slug, name, "doc", edgesFixtureActor, doc.refKey)
		add("artifact_versions", id, 1, fixtureAuthors)
		add("document_indexed_versions", id, 1)
		for _, block := range blocks {
			add("document_blocks", id, block.id, doc.refKey+"#"+block.id, block.kind, "sha", name, 1, at)
			add("document_block_texts", id, block.id, 1, name)
		}
		documents[doc.refKey] = doc
		return doc
	}
	// docLink writes a link of block of doc: the block's row of block_refs, of kind (mentions, or
	// a pointer's kind with toTypes, the text of the array of the types it allows), and the
	// mention the document's own text holds. A link written twice is one row of each.
	blockLinks := map[[5]string]bool{}
	docLink := func(doc fixtureDocument, block, kind, toKind, toID string, toTypes *string) {
		ref("artifact", doc.id.String(), toKind, toID)
		key := [5]string{doc.id.String(), block, toKind, toID, kind}
		if blockLinks[key] {
			return
		}
		blockLinks[key] = true
		add("block_refs", doc.id.String(), block, doc.refKey+"#"+block, toKind, toID, kind, 1, toTypes)
	}
	// item writes an ask, a comment or a message of issueKey with one block for each of parts,
	// each of that part's type, b1 first.
	item := func(kind ItemKind, issueKey, words string, parts ...string) uuid.UUID {
		id := seededUUID(random)
		tables, err := kind.tables()
		if err != nil {
			t.Fatal(err)
		}
		add(tables.items, id.String(), issueKey, edgesFixtureActor, words)
		for position, part := range parts {
			blockID := "b" + strconv.Itoa(position+1)
			node := itemBlockNodeID(kind, id.String(), blockID)
			if kind == ItemMessage {
				add(tables.blocks, id.String(), blockID, node, position, part, "sha", words, at, at)
			} else {
				add(tables.blocks, id.String(), blockID, node, position, part, "sha", words, 1, at)
			}
		}
		names[[2]string{string(kind), id.String()}] = words
		return id
	}
	// itemLink writes a mention of the item's block (none: of the item's own row alone): the
	// block's row, and the item's own refs row, which a comment and a message keep for every
	// block's mentions and an ask for its question's alone.
	itemLink := func(kind ItemKind, id uuid.UUID, block, toKind, toID string) {
		if block == "" || kind != ItemAsk || block == "b1" {
			ref(string(kind), id.String(), toKind, toID)
		}
		if block != "" {
			tables, err := kind.tables()
			if err != nil {
				t.Fatal(err)
			}
			add(tables.refs, id.String(), block, itemBlockNodeID(kind, id.String(), block), toKind, toID, "mentions")
		}
	}

	add("projects", "CORE", "CORE project", "alice")
	add("projects", "OPS", "OPS project", "alice")
	issue("CORE-1", "CORE", 1, "The parent", "")
	issue("CORE-2", "CORE", 2, "The issue", "CORE-1")
	others := 1
	if whole {
		for number := 3; number <= 32; number++ {
			issue("CORE-"+strconv.Itoa(number), "CORE", number, "Child "+strconv.Itoa(number), "CORE-2")
		}
		others = oneHopMinimumRows
	}
	for number := 1; number <= others; number++ {
		issue("OPS-"+strconv.Itoa(number), "OPS", number, "Other issue "+strconv.Itoa(number), "")
	}

	// The layers. Without whole, only the first layer's blocks are written: the links read
	// through a block are the issue's own content's, which reach the first layer alone.
	layers := make([][]fixtureDocument, len(oneHopLayers))
	for k, size := range oneHopLayers {
		for j := range size {
			var blocks []fixtureBlock
			if whole || k == 0 {
				blocks = []fixtureBlock{{"b1", "paragraph"}}
			}
			owner := ""
			if k == 0 && j == 0 {
				owner = "CORE-1"
				blocks = append(blocks, fixtureBlock{"r1", "requirement"})
			}
			layers[k] = append(layers[k], document(owner, "CORE", fmt.Sprintf("l%d-%d", k+1, j),
				fmt.Sprintf("Layer %d document %d", k+1, j), blocks...))
		}
	}
	cite := func(from, to fixtureDocument) { ref("artifact", from.id.String(), "artifact", to.refKey) }
	var earlier []fixtureDocument
	for k, layer := range layers {
		for j, doc := range layer {
			if k > 0 {
				cite(layers[k-1][j%len(layers[k-1])], doc)
				for range 3 {
					cite(doc, earlier[random.IntN(len(earlier))])
				}
			}
			if k+1 < len(layers) {
				for range 3 {
					cite(doc, layers[k+1][random.IntN(len(layers[k+1]))])
				}
			}
		}
		earlier = append(earlier, layer...)
	}
	first := layers[0]
	parentRequirement := first[0].refKey + "#r1"

	// The issue's own content.
	spec := document("CORE-2", "CORE", "spec", "The issue's spec",
		fixtureBlock{"b1", "paragraph"}, fixtureBlock{"r1", "requirement"}, fixtureBlock{"r2", "requirement"}, fixtureBlock{"chk", "check"})
	notes := document("CORE-2", "CORE", "notes", "The issue's notes", fixtureBlock{"b1", "paragraph"})
	question := item(ItemAsk, "CORE-2", "Which documents of the first layer does the issue rest on?", AskQuestionPart, AskOptionPart)
	second := item(ItemAsk, "CORE-2", "Is the issue's spec complete?", AskQuestionPart)
	unindexed := item(ItemComment, "CORE-2", "Cites the first layer, another project's issue and its ask, the issue and its spec.")
	indexed := item(ItemComment, "CORE-2", "Intro.\n\nCites the first layer.", "paragraph", "paragraph")
	plain := item(ItemMessage, "CORE-2", "Cites the first layer.")
	blocked := item(ItemMessage, "CORE-2", "Intro.\n\nCites the first layer too.", "paragraph", "paragraph")
	otherAsk := item(ItemAsk, "OPS-1", "The other project's ask", AskQuestionPart)
	own := map[[2]string]bool{
		{"issue", "CORE-2"}: true, {"artifact", spec.id.String()}: true, {"artifact", notes.id.String()}: true,
		{"ask", question.String()}: true, {"ask", second.String()}: true,
		{"comment", unindexed.String()}: true, {"comment", indexed.String()}: true,
		{"message", plain.String()}: true, {"message", blocked.String()}: true,
	}

	// Each link out of the issue's content, and the row of links it gives: a block at the far end
	// counts as its document, and a link to the issue or its own content gives none.
	links := relatedRows{}
	out := func(via model.ReferenceVia, kind, toKind, toID string) {
		farKind, farID, name := toKind, toID, names[[2]string{toKind, toID}]
		if toKind == "artifact" || toKind == "block" {
			doc := documents[strings.SplitN(toID, "#", 2)[0]]
			farKind, farID, name = "artifact", doc.id.String(), doc.name
		}
		if !own[[2]string{farKind, farID}] {
			links.add(farKind, farID, name, kind, via)
		}
	}
	specVia := model.ReferenceVia{Kind: "artifact", ID: spec.id.String()}
	for _, j := range []int{20, 21, 22, 23, 8} {
		docLink(spec, "b1", "mentions", "artifact", first[j].refKey, nil)
		out(specVia, "mentions", "artifact", first[j].refKey)
	}
	docLink(spec, "b1", "mentions", "block", first[24].refKey+"#b1", nil)
	out(specVia, "mentions", "block", first[24].refKey+"#b1")
	if whole {
		docLink(spec, "chk", "serves", "block", parentRequirement, new("{requirement}"))
		out(specVia, "serves", "block", parentRequirement)
	} else {
		docLink(spec, "b1", "mentions", "block", parentRequirement, nil)
		out(specVia, "mentions", "block", parentRequirement)
	}
	docLink(notes, "b1", "mentions", "artifact", first[3].refKey, nil)
	out(model.ReferenceVia{Kind: "artifact", ID: notes.id.String()}, "mentions", "artifact", first[3].refKey)
	itemLinks := func(kind ItemKind, id uuid.UUID, block string, js ...int) {
		for _, j := range js {
			itemLink(kind, id, block, "artifact", first[j].refKey)
			out(model.ReferenceVia{Kind: string(kind), ID: id.String()}, "mentions", "artifact", first[j].refKey)
		}
	}
	itemLinks(ItemComment, unindexed, "", 1, 2, 3, 4)
	itemLinks(ItemComment, indexed, "b2", 4, 5, 6, 7)
	itemLinks(ItemMessage, plain, "", 8, 9, 10)
	itemLinks(ItemMessage, blocked, "b2", 11, 12, 13)
	itemLinks(ItemAsk, question, "b1", 14, 15, 16)
	itemLinks(ItemAsk, question, "b2", 17, 18, 19)
	commentVia := model.ReferenceVia{Kind: "comment", ID: unindexed.String()}
	for _, target := range [][2]string{{"issue", "OPS-1"}, {"ask", otherAsk.String()}, {"issue", "CORE-2"}, {"artifact", spec.refKey}} {
		itemLink(ItemComment, unindexed, "", target[0], target[1])
		out(commentVia, "mentions", target[0], target[1])
	}

	linkedFrom := relatedRows{}
	if whole {
		// The in side: every target a link into the issue's content can end at, with the via a
		// row of linked_from names for it.
		artifactVia := func(doc fixtureDocument) model.ReferenceVia {
			return model.ReferenceVia{Kind: "artifact", ID: doc.refKey}
		}
		itemVia := func(kind ItemKind, id uuid.UUID) model.ReferenceVia {
			return model.ReferenceVia{Kind: string(kind), ID: id.String()}
		}
		type ownTarget struct {
			kind, id string
			via      model.ReferenceVia
		}
		targets := []ownTarget{
			{"issue", "CORE-2", model.ReferenceVia{Kind: "issue", ID: "CORE-2"}},
			{"artifact", spec.refKey, artifactVia(spec)},
			{"artifact", notes.refKey, artifactVia(notes)},
			{"block", spec.refKey + "#b1", artifactVia(spec)},
			{"block", spec.refKey + "#r1", artifactVia(spec)},
			{"block", spec.refKey + "#r2", artifactVia(spec)},
			{"block", spec.refKey + "#chk", artifactVia(spec)},
			{"block", notes.refKey + "#b1", artifactVia(notes)},
			{"ask", question.String(), itemVia(ItemAsk, question)},
			{"ask", second.String(), itemVia(ItemAsk, second)},
			{"ask_block", itemBlockNodeID(ItemAsk, question.String(), "b1"), itemVia(ItemAsk, question)},
			{"ask_block", itemBlockNodeID(ItemAsk, question.String(), "b2"), itemVia(ItemAsk, question)},
			{"ask_block", itemBlockNodeID(ItemAsk, second.String(), "b1"), itemVia(ItemAsk, second)},
			{"comment", unindexed.String(), itemVia(ItemComment, unindexed)},
			{"comment", indexed.String(), itemVia(ItemComment, indexed)},
			{"comment_block", itemBlockNodeID(ItemComment, indexed.String(), "b1"), itemVia(ItemComment, indexed)},
			{"comment_block", itemBlockNodeID(ItemComment, indexed.String(), "b2"), itemVia(ItemComment, indexed)},
			{"message", plain.String(), itemVia(ItemMessage, plain)},
			{"message", blocked.String(), itemVia(ItemMessage, blocked)},
			{"message_block", itemBlockNodeID(ItemMessage, blocked.String(), "b1"), itemVia(ItemMessage, blocked)},
			{"message_block", itemBlockNodeID(ItemMessage, blocked.String(), "b2"), itemVia(ItemMessage, blocked)},
		}
		for source := range 400 {
			count := 1
			if source >= 30 && source < 230 {
				count = 2
			}
			picks := []ownTarget{targets[source*5%len(targets)], targets[(source*5+11)%len(targets)]}[:count]
			owner := "OPS-" + strconv.Itoa(source+2)
			switch {
			case source < 30:
				child := "CORE-" + strconv.Itoa(source+3)
				doc := document(child, "CORE", "checks", "Checks of "+child, fixtureBlock{"k1", "check"})
				requirement := []string{"r1", "r2"}[source%2]
				docLink(doc, "k1", "serves", "block", spec.refKey+"#"+requirement, new("{requirement}"))
				linkedFrom.add("artifact", doc.id.String(), doc.name, "serves", artifactVia(spec))
			case source < 150:
				doc := document(owner, "OPS", "cites", "The document of "+owner+" that cites the issue", fixtureBlock{"b1", "paragraph"})
				for _, pick := range picks {
					docLink(doc, "b1", "mentions", pick.kind, pick.id, nil)
					linkedFrom.add("artifact", doc.id.String(), doc.name, "mentions", pick.via)
				}
			case source < 220:
				words := "The ask of " + owner + " that cites the issue"
				id := item(ItemAsk, owner, words, AskQuestionPart, AskOptionPart)
				for index, pick := range picks {
					itemLink(ItemAsk, id, "b"+strconv.Itoa(index+1), pick.kind, pick.id)
					linkedFrom.add("ask", id.String(), words, "mentions", pick.via)
				}
			default:
				kind := ItemComment
				if source >= 310 {
					kind = ItemMessage
				}
				words := "The " + string(kind) + " of " + owner + " that cites the issue"
				id := item(kind, owner, words, "paragraph")
				for _, pick := range picks {
					itemLink(kind, id, "b1", pick.kind, pick.id)
					linkedFrom.add(string(kind), id.String(), words, "mentions", pick.via)
				}
			}
		}

		// The outside graph. Its documents cite the targets the issue's spec cites, so that a
		// read of block_refs or refs by target alone meets them, and each other.
		outside := make([]fixtureDocument, 3000)
		for index := range outside {
			owner := "OPS-" + strconv.Itoa(index+1)
			outside[index] = document(owner, "OPS", "d"+strconv.Itoa(index), "Outside document "+strconv.Itoa(index), fixtureBlock{"b1", "paragraph"})
		}
		for index, doc := range outside {
			docLink(doc, "b1", "mentions", "artifact", first[20+index%5].refKey, nil)
			docLink(doc, "b1", "mentions", "block", []string{first[24].refKey + "#b1", parentRequirement, first[8].refKey + "#b1"}[index%3], nil)
			docLink(doc, "b1", "mentions", "artifact", earlier[random.IntN(len(earlier))].refKey, nil)
			docLink(doc, "b1", "mentions", "artifact", outside[(index+1)%len(outside)].refKey, nil)
		}
		// Asks, comments and messages of the other project, each with a block that cites a
		// document of the first layer or another item of its kind.
		for _, kind := range []ItemKind{ItemAsk, ItemComment, ItemMessage} {
			part := "paragraph"
			if kind == ItemAsk {
				part = AskQuestionPart
			}
			ids := make([]uuid.UUID, oneHopMinimumRows)
			for index := range ids {
				ids[index] = item(kind, "OPS-"+strconv.Itoa(index+1), "Outside "+string(kind)+" "+strconv.Itoa(index), part)
			}
			for index, id := range ids {
				next := ids[(index+1)%len(ids)]
				switch index % 3 {
				case 0:
					itemLink(kind, id, "b1", "artifact", first[index%len(first)].refKey)
				case 1:
					itemLink(kind, id, "b1", kind.BlockKind(), itemBlockNodeID(kind, next.String(), "b1"))
				default:
					itemLink(kind, id, "b1", string(kind), next.String())
				}
			}
		}
		add("architecture_sources", "OPS", 1, "ops/architecture", "main", 1, edgesFixtureActor)
		add("architecture_snapshots", 1, "OPS", "c0ffee", "{}")
		for index := range oneHopMinimumRows {
			id := "c" + strconv.Itoa(index)
			parent := ""
			if index > 0 {
				parent = "c" + strconv.Itoa(index/2)
			}
			add("components", "OPS", id, "Component "+strconv.Itoa(index), "prose", nullable(parent), 1, 1)
			add("component_facts", "OPS", id, at, at)
			// Each component a node of the store, everyone in the project's to read, so that
			// graph_nodes, which every visibility predicate of the read reads (VisibleKind), holds
			// oneHopMinimumRows too.
			add("graph_nodes", "component", "OPS/"+id, "component", graphstore.VisibilityProject, graphstore.StateLive, at, edgesFixtureActor, int(graphstore.NoEvent))
		}
	}

	loadFixture(t, database.Pool, oneHopTables, rows)
	copiedRows := 0
	for _, table := range rows {
		copiedRows += len(table)
	}
	t.Logf("the one-hop fixture took %s to write, copy and analyze %d rows", time.Since(built).Round(time.Millisecond), copiedRows)

	fixture := oneHopFixture{
		database: database, issue: "CORE-2", own: own,
		links: links.summaries(), linkedFrom: linkedFrom.summaries(),
	}
	kinds := map[string]bool{}
	for key, row := range links {
		kinds[key[0]] = true
		if key[0] == "artifact" {
			for _, doc := range first {
				if doc.id.String() == key[1] {
					fixture.members = append(fixture.members, documentMember(t, database.Pool, doc.refKey, row.via))
				}
			}
		}
	}
	for key := range linkedFrom {
		kinds[key[0]] = true
	}
	fixture.namedKinds = len(kinds)
	sort.Slice(fixture.members, func(left, right int) bool { return fixture.members[left].RefKey < fixture.members[right].RefKey })
	for _, doc := range first {
		fixture.reached = append(fixture.reached, doc.refKey)
	}
	return fixture
}

// oneHopSizes is what the fixture's two first counts are held to, counted by keyed reads of each
// table. out is the rows of the five link tables whose source is the issue's own content, and in
// their rows whose target is the issue or its own content, a row of both kinds counted in each;
// held is the mentions of either set that a block of their source holds, which heldByABlock reads
// again in that block's table; own is the issue and every node of its own content. A block or a
// part of an item is own content, and a source, while it is current.
type oneHopSizes struct{ out, in, held, own int }

func (f oneHopFixture) sizes(t *testing.T) oneHopSizes {
	t.Helper()
	var sizes oneHopSizes
	if err := f.database.Pool.QueryRow(context.Background(), `
		with own_documents as (select id, ref_key from artifacts where issue_key = $1),
		own_asks as (select id from asks where issue_key = $1),
		own_comments as (select id from comments where issue_key = $1),
		own_messages as (select id from messages where issue_key = $1),
		own_sources (kind, id) as (
			select 'artifact', id::text from own_documents
			union all select 'ask', id::text from own_asks
			union all select 'comment', id::text from own_comments
			union all select 'message', id::text from own_messages
		),
		own_targets (kind, id) as (
			select 'issue', $1
			union all select 'artifact', ref_key from own_documents
			union all select 'block', d.node_id from document_blocks d join own_documents o on o.id = d.artifact_id
			union all select 'ask', id::text from own_asks
			union all select 'ask_block', b.node_id from ask_blocks b join own_asks o on o.id = b.ask_id where b.position is not null
			union all select 'comment', id::text from own_comments
			union all select 'comment_block', b.node_id from comment_blocks b join own_comments o on o.id = b.comment_id where b.position is not null
			union all select 'message', id::text from own_messages
			union all select 'message_block', b.node_id from message_blocks b join own_messages o on o.id = b.message_id where b.position is not null
		),
		out_refs as (select r.* from refs r join own_sources s on s.kind = r.from_kind and s.id = r.from_id),
		in_refs as (select r.* from refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
		select
			(select count(*) from out_refs)
			+ (select count(*) from block_refs r join own_documents o on o.id = r.artifact_id)
			+ (select count(*) from ask_block_refs r join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
			   where b.position is not null and r.ask_id in (select id from own_asks))
			+ (select count(*) from comment_block_refs r join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
			   where b.position is not null and r.comment_id in (select id from own_comments))
			+ (select count(*) from message_block_refs r join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
			   where b.position is not null and r.message_id in (select id from own_messages)),
			(select count(*) from in_refs)
			+ (select count(*) from block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from ask_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from comment_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from message_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id),
			(select count(*) from (select * from out_refs union all select * from in_refs) r where
			   (r.from_kind = 'artifact' and exists (select 1 from block_refs b
			     where b.artifact_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'ask' and exists (select 1 from ask_block_refs b
			     where b.ask_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'comment' and exists (select 1 from comment_block_refs b
			     where b.comment_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'message' and exists (select 1 from message_block_refs b
			     where b.message_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))),
			(select count(*) from own_targets)
	`, f.issue).Scan(&sizes.out, &sizes.in, &sizes.held, &sizes.own); err != nil {
		t.Fatalf("count the links of %s's own content: %v", f.issue, err)
	}
	return sizes
}

// checkAnswer holds the read's answer to the rows the fixture's links make.
func (f oneHopFixture) checkAnswer(t *testing.T, references References) {
	t.Helper()
	for _, field := range []struct {
		name string
		got  []model.ReferenceLink
		want []string
	}{{"links", references.Links, f.links}, {"linked_from", references.LinkedFrom, f.linkedFrom}} {
		got := make([]string, 0, len(field.got))
		for _, row := range field.got {
			if f.own[[2]string{row.Node.Kind, row.Node.ID}] {
				t.Errorf("%s lists the issue's own %s %s", field.name, row.Node.Kind, row.Node.ID)
			}
			got = append(got, relatedSummary(row.Node.Kind, row.Node.ID, row.Name, row.Kinds, row.Via))
		}
		slices.Sort(got)
		if !slices.Equal(got, field.want) {
			t.Errorf("%s holds %d rows; want %d, one for each node at the far end of a link: %s",
				field.name, len(got), len(field.want), lineDiff(got, field.want))
		}
	}
	if diff, _ := membersDiff(references.Members, f.members, "documents of links, each at depth 1 with its via"); diff != "" {
		t.Error(diff)
	}
}

// linkTables is every table a link is read from: a mention of a document, an ask, a comment or a
// message in refs, and a mention or a typed pointer of one of their blocks in the other four.
var linkTables = []string{"refs", "block_refs", "ask_block_refs", "comment_block_refs", "message_block_refs"}

// joinNodeTypes is every plan node that can carry a Join Filter.
var joinNodeTypes = []string{"Nested Loop", "Hash Join", "Merge Join"}

// artifactProbeKeys is every column a read of artifacts may probe by: a document's ref_key, its id
// (as the uuid or as its text) and the issue's key.
var artifactProbeKeys = []string{"ref_key", "id", "issue_key"}

// planCounts is what TestTheReferencesReadReadsOnlyTheIssuesOwnLinks reads of analyzed plans.
// linkRows is its first count: the rows every scan of a link table answered and its Filter
// removed, over every loop. nodeRows is its second: the rows every node answered and its Filter
// and Join Filter removed, over every loop. Each margin is the precision of the plan's own
// figures: 0.005 for each loop of an Actual Rows, which EXPLAIN rounds to two decimals, and 0.5
// for each loop of a Rows Removed, which it rounds to a whole number. unkeyed is its third: each
// read of a table that is no probe of an index by its first key column.
type planCounts struct {
	linkRows, linkMargin float64
	nodeRows, nodeMargin float64
	unkeyed              []string
	relations            []string
}

func (c *planCounts) add(other planCounts) {
	c.linkRows += other.linkRows
	c.linkMargin += other.linkMargin
	c.nodeRows += other.nodeRows
	c.nodeMargin += other.nodeMargin
	c.unkeyed = append(c.unkeyed, other.unkeyed...)
	for _, relation := range other.relations {
		if !slices.Contains(c.relations, relation) {
			c.relations = append(c.relations, relation)
		}
	}
}

// countPlan reads the three counts of one analyzed plan. A table is read keyed when an Index Scan
// or an Index Only Scan of it, or each Bitmap Index Scan under a Bitmap Heap Scan of it, carries
// an Index Cond naming its index's first key column; artifacts only by ref_key, id or the issue's
// key. Any other scan of a relation, a sequential scan first of all, is unkeyed.
func countPlan(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) planCounts {
	t.Helper()
	var counts planCounts
	var bitmapRelations []string
	for _, node := range explaintest.Nodes(t, plan) {
		rowsMargin := 0.005 * node.Loops
		var filterMargin, joinMargin float64
		if node.Filter != "" {
			filterMargin = 0.5 * node.Loops
		}
		if slices.Contains(joinNodeTypes, node.Type) {
			joinMargin = 0.5 * node.Loops
		}
		counts.nodeRows += (node.Rows + node.RowsRemovedByFilter + node.RowsRemovedByJoinFilter) * node.Loops
		counts.nodeMargin += rowsMargin + filterMargin + joinMargin
		if slices.Contains(linkTables, node.Relation) {
			counts.linkRows += (node.Rows + node.RowsRemovedByFilter) * node.Loops
			counts.linkMargin += rowsMargin + filterMargin
		}
		if node.Relation == "" {
			continue
		}
		if !slices.Contains(counts.relations, node.Relation) {
			counts.relations = append(counts.relations, node.Relation)
		}
		switch node.Type {
		case "Index Scan", "Index Only Scan":
			if problem := unkeyedProbe(t, keys, node.Relation, node.String(), node.Index, node.IndexCond); problem != "" {
				counts.unkeyed = append(counts.unkeyed, problem)
			}
		case "Bitmap Heap Scan":
			if !slices.Contains(bitmapRelations, node.Relation) {
				bitmapRelations = append(bitmapRelations, node.Relation)
			}
		default:
			counts.unkeyed = append(counts.unkeyed, node.String()+" reads its table through no index")
		}
	}
	for _, relation := range bitmapRelations {
		for _, scan := range explaintest.BitmapHeapScans(t, plan, relation) {
			if len(scan.BitmapIndexScans) == 0 {
				counts.unkeyed = append(counts.unkeyed, scan.Scan.String()+" reads a bitmap no index scan builds")
			}
			for _, index := range scan.BitmapIndexScans {
				if problem := unkeyedProbe(t, keys, relation, scan.Scan.String()+" through "+index.String(), index.Index, index.IndexCond); problem != "" {
					counts.unkeyed = append(counts.unkeyed, problem)
				}
			}
		}
	}
	return counts
}

// unkeyedProbe says why a scan of relation through index under cond is no keyed probe, or "" when
// it is one.
func unkeyedProbe(t *testing.T, keys *explaintest.IndexKeys, relation, scan, index, cond string) string {
	t.Helper()
	if cond == "" {
		return scan + " carries no Index Cond, so it reads its whole index"
	}
	firstKey := keys.FirstKey(t, index)
	if !explaintest.CondNamesFirstKey(cond, firstKey) {
		return fmt.Sprintf("%s reads on `%s`, which names nothing of its index's first key column `%s`", scan, cond, firstKey)
	}
	if relation == "artifacts" && !slices.ContainsFunc(artifactProbeKeys, func(key string) bool {
		return explaintest.CondNamesFirstKey(key, firstKey)
	}) {
		return fmt.Sprintf("%s probes artifacts by `%s`, not by ref_key, id or the issue's key", scan, firstKey)
	}
	return ""
}
