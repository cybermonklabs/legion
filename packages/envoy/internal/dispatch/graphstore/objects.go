package graphstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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
// its id. Each is written `<kind>:<id>`, the kind being the store's own node kind of that party, so
// a party names the node it is.
func PartyPerson(login string) string { return KindPerson + ":" + model.CanonicalLogin(login) }

func PartySession(id string) string { return KindSession + ":" + id }

// IssueNode is an issue's node as its create writes it: project-visible, live, in its project, and
// with no cause yet, since the issue's node is written before its row and its event after both
// (Caused gives it the event).
func IssueNode(key, project string, who model.Actor, at time.Time) Node {
	return objectNode(KindIssue, key, SupertagIssue, project, nil, who, at)
}

// ArtifactNode is a document's node, addressed by its ref_key (ArtifactRefKey), as its upload
// writes it. A document of an issue or a project is in that project; an agent conversation's
// document is in none, and is between the parties of that conversation, its session.
func ArtifactNode(refKey, project, session string, who model.Actor, at time.Time) Node {
	var parties []string
	if session != "" {
		parties = []string{PartySession(session)}
	}
	return objectNode(KindArtifact, refKey, SupertagDocument, project, parties, who, at)
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

// PartiesValue is the value the `parties` attribute of a node between parties carries: its parties
// sorted, each once, and joined by one space, which is what graph_nodes.parties is split back from.
// No party is ever written with a space in it: a login and a session id hold none. Nil parties are
// the empty value, which no write states.
func PartiesValue(parties []string) string {
	if len(parties) == 0 {
		return ""
	}
	sorted := slices.Clone(parties)
	slices.Sort(sorted)
	return strings.Join(slices.Compact(sorted), " ")
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
	switch {
	case projectKey != nil:
		return *projectKey, nil, nil
	case session != nil:
		return "", []string{PartySession(*session)}, nil
	}
	return "", nil, nil
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
		parties = append(parties, kind+":"+id)
	}
	if rootTarget != nil && *rootTarget != "" {
		parties = append(parties, *rootTarget)
	}
	return parties
}
