package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The kinds of the objects Dispatch holds that a project or a conversation owns, as the graph
// addresses each (refs' nodeRegistry). These are the one spelling of them: every writer of one of
// these nodes, and every read that names its kind, takes it from here. A comment's is KindComment
// (comment.go), which is its supertag too.
const (
	KindIssue    = "issue"
	KindArtifact = "artifact"
	KindAsk      = "ask"
	KindMessage  = "message"
)

// The supertag each of those kinds' nodes carries, from the closed list the store seeds
// (20261109051232_graph_supertags.up.sql). An artifact is a document whatever it holds: a spec, a
// plan, a file a conversation was sent.
const (
	SupertagIssue    = "issue"
	SupertagDocument = "document"
	SupertagAsk      = "ask"
	SupertagMessage  = "message"
)

// PartyPerson and PartySession are the two kinds of party a node no project owns is between: a
// person, by the login every node of theirs is keyed by (model.CanonicalLogin), and a session, by
// its id. Each is written `<kind>:<id>` (party), the kind being the store's own node kind of that
// party, so a party names the node it is.
func PartyPerson(login string) string { return party(KindPerson, model.CanonicalLogin(login)) }

func PartySession(id string) string { return party(KindSession, id) }

// party is the party the node kind/id is: `<kind>:<id>`.
func party(kind, id string) string { return kind + ":" + id }

// IssueNode is an issue's node as its create writes it: project-visible, live, in its project, and
// with no cause yet, since the issue's node is written before its row and its event after both
// (Caused gives it the event).
func IssueNode(key, project string, who model.Actor, at time.Time) Node {
	return objectNode(KindIssue, key, SupertagIssue, project, nil, who, at)
}

// ArtifactNode is a document's node, addressed by its ref_key (ArtifactRefKey), as its upload
// writes it. A document of an issue or a project is in that project; an agent conversation's
// document is in none, and is between the parties of that conversation, its session (SessionScope).
// Every write of it states beside it the facts its row projects: the part_of to its issue where an
// issue owns it (ArtifactPartOf), and the bytes of an image or a file (ArtifactBytes).
func ArtifactNode(refKey, project, session string, who model.Actor, at time.Time) Node {
	project, parties := SessionScope(project, session)
	return objectNode(KindArtifact, refKey, SupertagDocument, project, parties, who, at)
}

// ArtifactPartOf is the fact that hangs a document under the issue that owns it: the part_of tuple
// from the document's node, addressed by its ref_key, to the issue's node, which
// artifacts.issue_key projects (R16 of the issue-tree spec). Every write of the node of an
// issue's document states it beside that node, and a document no issue owns - a project's, or a
// file of an agent's conversation - states none. It carries who wrote it and when; its cause is
// Caused's to fill, or the sentinel where no event recorded the write.
func ArtifactPartOf(refKey, issueKey string, who model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: KindArtifact, FromID: refKey, Kind: TuplePartOf,
		ToKind: KindIssue, ToID: issueKey,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
	}
}

// The attributes the node of an image or a file carries of the bytes its newest version holds,
// each named as the artifact_versions column that holds it: the media type the bytes are stored
// and served under, how many bytes there are, in decimal, and their sha256, in lower-case hex. A
// markdown document carries none of them, since its body is its blocks.
const (
	AttributeMIME   = "mime"
	AttributeSize   = "size"
	AttributeSHA256 = "sha256"
)

// ArtifactBytes is what the node of an image or a file states of the bytes one version of it holds
// (R16): their mime, their size and their sha256, each a value of the node, since none of them
// points at anything. Each version states them again, so the node's current values are its newest
// version's. They are appended to tuples, so a writer stating a batch of documents grows one
// slice. Their cause is Caused's to fill, or the sentinel where no event recorded the write.
func ArtifactBytes(tuples []Tuple, refKey, mime string, size int64, sha256 string, who model.Actor, at time.Time) []Tuple {
	for _, value := range [...]struct{ attribute, value string }{
		{AttributeMIME, mime}, {AttributeSize, strconv.FormatInt(size, 10)}, {AttributeSHA256, sha256},
	} {
		tuples = append(tuples, Tuple{
			FromKind: KindArtifact, FromID: refKey, Kind: value.attribute,
			ToKind: KindValue, ToID: value.value,
			At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
		})
	}
	return tuples
}

// KindVersion is what this store calls one version of a document: one accepted write of it, which
// its artifact_versions row projects (R16 of the issue-tree spec). A version's address is its
// document's ref_key, `@v` and its number (VersionID).
const KindVersion = "version"

// The supertags a version and a query carry (the root's R21 list): a version of a document is a
// document version, and a version of a query node is a query version; a document that a query
// replaces becomes a query node (QueryNode). `document version` is seeded with the closed list
// (20261109051232); `query` and `query version` are this child's
// (20261123174750_issue_tree_query_supertags).
const (
	SupertagDocumentVersion = "document version"
	SupertagQuery           = "query"
	SupertagQueryVersion    = "query version"
)

// The attributes a version node and a query node carry, each a value of the node, since none
// points at anything: a version's number and the time its row was written, each named as the
// artifact_versions column that holds it; a query's name, the supertags it takes, the node its
// walk starts at, and the version of the definition its record is built by, which a query version
// carries too. The node its walk starts at is `subtree` and not `from`, because no attribute of
// that name stands among the store's type nodes (R2).
const (
	AttributeNumber            = "number"
	AttributeCreatedAt         = "created_at"
	AttributeQuery             = "query"
	AttributeTakes             = "takes"
	AttributeSubtree           = "subtree"
	AttributeDefinitionVersion = "definition_version"
)

// QueryDefinitionVersion is the definition_version of every query this build writes (QueryNode),
// and so of every query version a version write makes of one (VersionFacts): the version of the
// layout of a query's record, the bytes docs' IssueTree.QueryRecord writes and an approval covers
// the digest of (R14). A change of that layout - a field's order, its prefix, a value left in or
// out, the order of the values or of the relations - raises it, so no approval of a version
// written under one layout reads as an approval of a version written under another.
const QueryDefinitionVersion = 1

// The four queries of an issue (R8): each is a query node, a view of the issue's one tree.
const (
	QuerySpec         = "spec"
	QueryVerification = "verification"
	QueryPlan         = "plan"
	QueryDecisions    = "decisions"
)

// queryTakes is the supertags each query takes out of the issue's tree, in the order its
// definition names them: the spec its memo callout, its criteria and its requirements; the
// verification its checks and their results; the plan its plan nodes and the role blocks of their
// owners (plan_role, a type no block schema of this build declares, so a schema that declares it
// reaches the view with no change here); and the decisions the asks, the decisions and the
// readings under an agent's decision (the decisions-tree spec's supertags).
var queryTakes = map[string][]string{
	QuerySpec:         {"callout", "criterion", "requirement"},
	QueryVerification: {"check", "result"},
	QueryPlan:         {"plan_task", "plan_role"},
	QueryDecisions:    {"ask", "decision", "reading"},
}

// QueryTakes is the supertags the query named query takes, nil for a name that is no query.
func QueryTakes(query string) []string { return slices.Clone(queryTakes[query]) }

// QueryNode is the node of a document a query replaces, addressed by its ref_key as the document's
// node is (ArtifactRefKey), in the issue's project: supertag `query`, and as values the query's
// name, the supertags it takes (QueryTakes), the node its walk starts at, `issue:<KEY>`, the root
// of the issue's tree (R1, R8), and the version of the definition its record is built by
// (QueryDefinitionVersion). The issue is read off refKey, `<issue key>/<slug>`, since a query node
// is always an issue's and no issue key holds a `/`. The values ride on the node's Attributes,
// which its row projects, and AttributeFacts states them as the tuples they are, so a writer writes
// the node with AttributeFacts(nil, node). No column of its artifacts row holds a role (R8).
//
// A document's node becomes a query node through Write, whose node move rewrites a stored node's
// supertag and attributes, never through WriteArms, whose node arm moves nothing on a node already
// there. The query's writer owns the values it states, so the node does not restate the document
// (Node.Restates false): a restating write keeps the attributes its row stores (writtenAttributes),
// which would leave the row of a document made a query without the query's values, and a version
// written after it would read no definition_version off that row (VersionOwnerColumns).
func QueryNode(refKey, project, query string, who model.Actor, at time.Time) Node {
	issue, _, _ := strings.Cut(refKey, "/")
	node := objectNode(KindArtifact, refKey, SupertagQuery, project, nil, who, at)
	node.Attributes = map[string]any{
		AttributeQuery:             query,
		AttributeTakes:             QueryTakes(query),
		AttributeSubtree:           KindIssue + ":" + issue,
		AttributeDefinitionVersion: QueryDefinitionVersion,
	}
	node.Restates = false
	return node
}

// ArtifactQueryRoleSQL is the SQL expression, over the artifacts table unaliased, of the role the
// row's document plays as a query node (QueryNode): its query value, `verification`, `plan` or
// `decisions`, and null for the spec's `spec`, whose part is the row's is_primary, and for a
// document whose node is no query node. A document of an issue born converted writes no role in its
// row (decision 2 of the issue-tree plan), so a reader of the role it plays reads this
// beside the row's own.
const ArtifactQueryRoleSQL = `(select nullif(node.attributes->>'` + AttributeQuery + `', '` + QuerySpec + `')
	from graph_nodes node
	where node.kind = '` + KindArtifact + `' and node.id = artifacts.ref_key and node.supertag = '` + SupertagQuery + `')`

// VersionID is the address of a document's version node: the document's ref_key, `@v` and the
// version's number, which no ref_key holds since a slug holds no `@`.
func VersionID(refKey string, number int) string { return refKey + "@v" + strconv.Itoa(number) }

// VersionOwner is what a version's node takes of its document (VersionFacts): the document's
// address, its project or its conversation's session, whether the document's node is a query
// node, which makes its versions query versions, and the definition_version that query node holds
// as the version is written, 0 where it holds none. An owner read after its version was written
// names no definition version (graphcensus' owedVersionNodes), since the query node it reads then
// can be newer than the version.
type VersionOwner struct {
	RefKey, Project, Session string
	Query                    bool
	DefinitionVersion        int
}

// VersionOwnerColumns is the select list that reads VersionOwner of the document the SQL
// expression artifactID names, in the order Targets scans it. A version's insert returns it beside
// the row, so its writer reads what the version's node needs in the statement that writes the
// version and in no round trip of its own.
func VersionOwnerColumns(artifactID string) string {
	owner := func(column string) string {
		return `(select ` + column + ` from artifacts owner where owner.id = ` + artifactID + `)`
	}
	query := func(column string) string {
		return `(select ` + column + ` from artifacts owner
			join graph_nodes n on n.kind = '` + KindArtifact + `' and n.id = owner.ref_key
			where owner.id = ` + artifactID + ` and n.supertag = '` + SupertagQuery + `')`
	}
	return owner("owner.ref_key") + ", " + owner("coalesce(owner.project_key, '')") + ", " +
		owner("coalesce(owner.session_id, '')") + ", exists " + query("1") + ", coalesce(" +
		query(`(n.attributes->>'`+AttributeDefinitionVersion+`')::int`) + ", 0)"
}

// Targets are the scan targets of VersionOwnerColumns, in its order.
func (o *VersionOwner) Targets() []any {
	return []any{&o.RefKey, &o.Project, &o.Session, &o.Query, &o.DefinitionVersion}
}

// VersionFacts is what this store holds of one version of a document (R16): its node, addressed by
// VersionID, supertag `document version`, or `query version` where its document is a query node,
// in its document's project or between its conversation's parties, made by who at at, the time its
// row was written; and, appended to tuples, its part_of its document's node and its number and its
// time as values (AttributeFacts), and for a query version whose owner names the definition
// version its query node held as the version was written, that definition_version as a value too
// (R14). A version is a fact of the write that stored it, which owns it whole; the node restates
// the version (Node.Restates) all the same, so a write of it again keeps a state a later write
// gave it. ok is false where the version's address is past the store's bound, of which nothing is
// stated. The cause is Caused's to fill, or the sentinel where no event recorded the write.
func VersionFacts(tuples []Tuple, owner VersionOwner, number int, who model.Actor, at time.Time) (node Node, facts []Tuple, ok bool) {
	id := VersionID(owner.RefKey, number)
	if !AddressFits(id) {
		return Node{}, tuples, false
	}
	supertag := SupertagDocumentVersion
	if owner.Query {
		supertag = SupertagQueryVersion
	}
	project, parties := SessionScope(owner.Project, owner.Session)
	node = objectNode(KindVersion, id, supertag, project, parties, who, at)
	node.Attributes = map[string]any{
		AttributeNumber:    number,
		AttributeCreatedAt: at.UTC().Format(time.RFC3339Nano),
	}
	if owner.Query && owner.DefinitionVersion != 0 {
		node.Attributes[AttributeDefinitionVersion] = owner.DefinitionVersion
	}
	tuples = append(tuples, Tuple{
		FromKind: KindVersion, FromID: id, Kind: TuplePartOf,
		ToKind: KindArtifact, ToID: owner.RefKey,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
	})
	return node, AttributeFacts(tuples, node), true
}

// AttributeFacts appends to tuples each attribute node carries, in name order, as the value tuple
// it is: its name the tuple's kind, and its value as text, a string as itself and any other value
// as its JSON, as every type node's values are written (20261109051232). A node row's attributes
// column is the projection of these tuples, so a writer that gives a node attributes states them
// with it. Each carries the node's own time, writer and cause. A value that has no JSON states no
// tuple here, and the write of the node carrying it refuses it (nodeParams), so neither lands.
func AttributeFacts(tuples []Tuple, node Node) []Tuple {
	for _, name := range slices.Sorted(maps.Keys(node.Attributes)) {
		value, isText := node.Attributes[name].(string)
		if !isText {
			encoded, err := json.Marshal(node.Attributes[name])
			if err != nil {
				continue
			}
			value = string(encoded)
		}
		tuples = append(tuples, Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: name,
			ToKind: KindValue, ToID: value,
			At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
		})
	}
	return tuples
}

// Joined is several writes held for one cause as one write held for it: QueueCaused moves the
// nodes of each and appends the facts of each under that cause, in the order given, each in an
// append of its own, since each carries the mark its own node write left (nodeMark), and
// QueueNodes writes the nodes of each. Its Nodes and Tuples are theirs, in that order, and a fact
// a writer appends to its Tuples after joining them is appended after every one's (Uncaused's
// parts). A writer whose caller causes one write it answers adds its own to it here.
func Joined(writes ...Uncaused) Uncaused {
	var joined Uncaused
	for _, write := range writes {
		joined.Nodes = append(joined.Nodes, write.Nodes...)
		joined.Tuples = append(joined.Tuples, write.Tuples...)
		joined.parts = append(joined.parts, write.split()...)
	}
	return joined
}

// SessionScope is the scope of a document, or of what belongs to one, whose project is project and
// whose agent conversation is session: the project, or for a document no project owns the party of
// that conversation's session. A document has one of the two (the artifacts_one_owner check,
// 0071_agent_artifacts.up.sql). Every writer of a document's node, of an ask, a comment or a block
// on a document, and the census's sessionParties, follow this one rule.
func SessionScope(project, session string) (string, []string) {
	if project == "" && session != "" {
		return "", []string{PartySession(session)}
	}
	return project, nil
}

// AskNode is an ask's node, addressed by the ask's id, in the project of the issue or the document
// it is asked on, or between the parties of the conversation whose file it is asked on (ScopeOf).
func AskNode(id, project string, parties []string, who model.Actor, at time.Time) Node {
	return objectNode(KindAsk, id, SupertagAsk, project, parties, who, at)
}

// MessageNode is a message's node, addressed by the message's id. A message on an issue is in that
// issue's project; one on none is between parties, the person and the session of its thread.
func MessageNode(id, project string, parties []string, who model.Actor, at time.Time) Node {
	return objectNode(KindMessage, id, SupertagMessage, project, parties, who, at)
}

// objectNode is the node of an issue, a document, an ask or a message as the write that stores it
// states it. Each of those writers - a create, an upload, the seed, the backfill of the nodes an
// older build wrote without a project (internal/dispatch/graphcensus) - writes the node for its
// address, its project and its parties, and owns neither its state nor its visibility, which a
// later write may mark (a proposed issue, say). So the node restates the object (Node.Restates):
// one written again keeps what the store holds of both, unless it was gone, when it comes back
// live.
func objectNode(kind, id, supertag, project string, parties []string, who model.Actor, at time.Time) Node {
	return Node{
		Kind: kind, ID: id, Supertag: supertag,
		Visibility: VisibilityProject, State: StateLive,
		Project: project, Parties: parties,
		MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
		Restates: true,
	}
}

// Created is node marked as the node its write creates (Node's Changed), as a create states each
// node it makes: a create made for a run names the node it makes as changed by that run.
func Created(node Node) Node {
	node.Changed = true
	return node
}

// ArtifactRefKey is the address an artifacts row takes, exactly as the artifacts_ref_key trigger
// computes it (0071_agent_artifacts.up.sql): `<issue key>/<slug>` for an issue's document,
// `<project key>/<slug>` for a project's, and `agent/<session id>/<slug>` for a conversation's. A
// document's node is written before its row, so its writer reads the address here rather than off
// the row it has not written yet; TestArtifactRefKeyIsTheTriggers holds the two together. An empty
// key is the null the trigger's coalesce passes over.
func ArtifactRefKey(issueKey, projectKey, session, slug string) string {
	switch {
	case issueKey != "":
		return issueKey + "/" + slug
	case projectKey != "":
		return projectKey + "/" + slug
	default:
		return "agent/" + session + "/" + slug
	}
}

// StatementTime is the one time a writer of a document dates what it writes in its transaction:
// the row it creates (its created_at) and every statement it makes about the document's node at
// address id. It is the database's clock as the call runs (clock_timestamp(), never the
// transaction's start), and one microsecond past the newest statement the store holds about the
// node where the clock reads no later. A writer reads it once, after it holds the lock that orders
// every writer of the address - the document's owner lock (api's queueNewArtifact and
// removeArtifact) - so two writers of one address read it one after the other, the second once the
// first has committed, and the second's time is strictly later than every statement the first made,
// by the clock and, should the clock step back, by the greatest(). So every statement about an address is
// dated in the order the store appends it, and a read of a node as of a time reads what the store
// held then. Which row a statement names is not read from its time but from its cause, the row's
// own create event (graphcensus's ownLiveStatement).
func StatementTime(ctx context.Context, q Queryer, kind, id string) (time.Time, error) {
	var at time.Time
	if err := q.QueryRow(ctx, `
		select greatest(clock_timestamp(), (
			select max(at) + interval '1 microsecond' from graph_tuples where from_kind = $1 and from_id = $2))
	`, kind, id).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("read the time of a write of %s/%s: %w", kind, id, err)
	}
	return at.UTC(), nil
}

// PartiesValue is the value the `parties` attribute of a node between parties carries: its parties
// sorted, each once, and joined by one space, which is what graph_nodes.parties is split back from.
// It is the one place the parties are joined, so it refuses a party that is empty or holds white
// space with ErrBadParty: such a party would come back from the column as two parties, or none. A
// login and a path's session id hold none, but a broadcast's session ids are only trimmed, so the
// refusal is here rather than at each source. Nil parties are the empty value, which no write
// states.
func PartiesValue(parties []string) (string, error) {
	if len(parties) == 0 {
		return "", nil
	}
	for _, party := range parties {
		if party == "" || strings.IndexFunc(party, unicode.IsSpace) >= 0 {
			return "", fmt.Errorf("party %q: %w", party, ErrBadParty)
		}
	}
	sorted := slices.Clone(parties)
	slices.Sort(sorted)
	return strings.Join(slices.Compact(sorted), " "), nil
}

// ErrBadParty is a party that is empty or holds white space (PartiesValue). A node write that
// names one is refused before it binds anything, and the caller's transaction ends.
var ErrBadParty = errors.New("a party is empty or holds white space")

// scopeColumns is a project and parties as graph_nodes' nullable project_key and parties columns
// bind them: nil for no project and for no parties, else the project and the parties' value
// (PartiesValue), whose refusal it answers.
func scopeColumns(project *string, parties []string) (*string, *string, error) {
	var projectColumn, partiesColumn *string
	if *project != "" {
		projectColumn = project
	}
	value, err := PartiesValue(parties)
	if err != nil {
		return nil, nil, err
	}
	if value != "" {
		held := value
		partiesColumn = &held
	}
	return projectColumn, partiesColumn, nil
}

// ScopeOf is the project, or for a thing no project owns the parties, of an ask or a comment that
// belongs to an issue or a document: the issue's project where it names an issue, else the
// document's project, else, for a file an agent's conversation owns, that conversation's session
// (ArtifactNode). It reads one row of issues or artifacts in the caller's transaction. Neither
// named, or neither found, answers nothing, which a writer then states nothing of.
func ScopeOf(ctx context.Context, q Queryer, issueKey, artifactID *string) (project string, parties []string, err error) {
	var projectKey, session *string
	if err := q.QueryRow(ctx, `select `+scopeRead, issueKey, artifactID).Scan(&projectKey, &session); err != nil {
		return "", nil, fmt.Errorf("read the project of what an item belongs to: %w", err)
	}
	project, parties = rowScope(projectKey, session)
	return project, parties, nil
}

// CommentScopeOf is ScopeOf of a comment that belongs to issueKey or artifactID, read in the same
// statement as whether the comment's row names its node (comments.node_kind): a comment left out
// of the store, or one a process of the release before wrote and the backfill has not stated yet,
// names none. It is the one read of an edit of a comment's words and of a comment's delete, each
// made for a run (api's planCommentChangedForRun, for editComment and deleteComment).
func CommentScopeOf(ctx context.Context, q Queryer, commentID string, issueKey, artifactID *string) (stored bool, project string, parties []string, err error) {
	var projectKey, session *string
	if err := q.QueryRow(ctx, `select (select node_kind is not null from comments where id = $3), `+scopeRead,
		issueKey, artifactID, commentID).Scan(&stored, &projectKey, &session); err != nil {
		return false, "", nil, fmt.Errorf("read whether the comment %s names its node, and the project of what it belongs to: %w", commentID, err)
	}
	project, parties = rowScope(projectKey, session)
	return stored, project, parties, nil
}

// scopeRead is the select list and the source of ScopeOf's read, the issue's key bound at $1 and
// the document's id at $2: the issue's project, else the document's, and the document's session.
const scopeRead = `coalesce((select project_key from issues where key = $1), a.project_key), a.session_id
		from (select 1) as one
		left join artifacts a on a.id = $2::uuid`

// rowScope is the scope ScopeOf answers for the project and the session its read found, either
// nil where the row holds none.
func rowScope(projectKey, session *string) (project string, parties []string) {
	var owner, conversation string
	if projectKey != nil {
		owner = *projectKey
	}
	if session != nil {
		conversation = *session
	}
	return SessionScope(owner, conversation)
}

// ThreadParties is the parties a message on no issue is between, read off its thread's root: who
// wrote the root (AuthorOf: a person by their login, or a session) and where the root was sent,
// its target route as the messages row stores it (`session:<id>`), which names a node of this
// store as a party does. A direct message, a broadcast copy and every reply in such a thread take
// the parties of the root they join, so a thread is one set of parties whoever wrote each message
// of it.
func ThreadParties(rootAuthor model.Actor, rootTarget *string) []string {
	var parties []string
	if kind, id, ok := AuthorOf(rootAuthor); ok {
		parties = append(parties, party(kind, id))
	}
	if rootTarget != nil && *rootTarget != "" {
		parties = append(parties, *rootTarget)
	}
	return parties
}
