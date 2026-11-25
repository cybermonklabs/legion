package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
func ArtifactNode(refKey, project, session string, who model.Actor, at time.Time) Node {
	project, parties := SessionScope(project, session)
	return objectNode(KindArtifact, refKey, SupertagDocument, project, parties, who, at)
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

func objectNode(kind, id, supertag, project string, parties []string, who model.Actor, at time.Time) Node {
	return Node{
		Kind: kind, ID: id, Supertag: supertag,
		Visibility: VisibilityProject, State: StateLive,
		Project: project, Parties: parties,
		MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
	}
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
		partiesColumn = &value
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
	if err := q.QueryRow(ctx, `
		select coalesce((select project_key from issues where key = $1), a.project_key), a.session_id
		from (select 1) as one
		left join artifacts a on a.id = $2::uuid
	`, issueKey, artifactID).Scan(&projectKey, &session); err != nil {
		return "", nil, fmt.Errorf("read the project of what an item belongs to: %w", err)
	}
	var owner, conversation string
	if projectKey != nil {
		owner = *projectKey
	}
	if session != nil {
		conversation = *session
	}
	project, parties = SessionScope(owner, conversation)
	return project, parties, nil
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
