package access

import "fmt"

// The read rules of what a thread is made of - an ask, a comment and a message - over the row a
// statement reads, for the viewer that statement binds at viewerParam. A row a project owns is
// read through ProjectClause, and a row of a conversation no project owns through PartyClause
// over that conversation's parties, the ones the node writers are to state, which a later slice
// adds. Each answers true or false, never NULL, as every clause here does (clause).

// IssueProjectClause passes the issue keyExpr names when the viewer sees the issue's project. An
// issue no row holds passes nobody.
func IssueProjectClause(keyExpr, viewerParam string) string {
	return clause("IssueProjectClause", ProjectClause(issueProjectSQL(keyExpr), viewerParam))
}

// ItemOwnerClause passes an ask or a comment, a row that an issue (issueExpr) or a document
// (artifactExpr) owns: when the viewer sees the project that owner is in - the issue's, else the
// document's - or, for a file of an agent's conversation, which no project owns, when the viewer
// may read that conversation, whose one party is the conversation's session (PartyClause, which
// holds that session to the content rule as well, SessionContentClause).
func ItemOwnerClause(issueExpr, artifactExpr, viewerParam string) string {
	project := `coalesce(` + issueProjectSQL(issueExpr) + `,
		(select item_document.project_key from artifacts item_document where item_document.id = ` + artifactExpr + `))`
	// Each parties expression is a scalar subquery cast to text[], so PartyClause's `= any(...)`
	// reads it as an array; a bare subquery there is compared row by row.
	file := `(select array['session:' || item_file.session_id] from artifacts item_file
		where item_file.id = ` + artifactExpr + ` and item_file.session_id is not null)::text[]`
	return clause("ItemOwnerClause", `(case when `+project+` is not null then `+ProjectClause(project, viewerParam)+`
		else `+PartyClause(file, viewerParam)+` end)`)
}

// DocumentClause passes the artifacts row alias names: a document of an issue or of a project when
// the viewer sees that project (ProjectClause over the row's own project_key, which is its issue's
// project for an issue's document), and a file of an agent's conversation, which no project owns,
// when the viewer may read that conversation, whose one party is the file's session (PartyClause,
// the parties a later slice's node writer states on the document's node). A file can hold whatever
// its session read, so PartyClause holds that session to the content rule the live agent view keeps
// (SessionContentClause) as well as to SessionClause. It reads the row's own columns, so a
// statement that holds the row binds it rather than ItemOwnerClause, which reads the document again
// by its id. A document its reader may not read is a row the statement does not find, and answers
// exactly as an id nobody holds.
func DocumentClause(alias, viewerParam string) string {
	return clause("DocumentClause", `(case when `+alias+`.project_key is not null then `+ProjectClause(alias+`.project_key`, viewerParam)+
		` else `+PartyClause(`array['session:' || `+alias+`.session_id]`, viewerParam)+` end)`)
}

// MessagePartiesSQL is the parties of the conversation the message idExpr (its id as text) is in:
// those its node carries, which are its thread root's, since every message of a thread on no
// issue is written between its root's parties.
func MessagePartiesSQL(idExpr string) string {
	return `(select message_node.parties from graph_nodes message_node
		where message_node.kind = 'message' and message_node.id = ` + idExpr + `)::text[]`
}

// MessageClause passes the messages row alias names: one on an issue when the viewer sees the
// issue's project, and one on no issue when the viewer may read its conversation (PartyClause
// over MessagePartiesSQL, which holds each session party to SessionContentClause too, since a
// session's messages can hold what it read). A conversation whose parties were never stated, a
// message whose node Dispatch does not hold or whose node names no party, passes an open viewer
// alone while no project is private, as every conversation did before a project could be, and no
// viewer but the server's once one is.
func MessageClause(alias, viewerParam string) string {
	return clause("MessageClause", `(case when `+alias+`.issue_key is not null then `+IssueProjectClause(alias+`.issue_key`, viewerParam)+`
		else `+PartyClause(MessagePartiesSQL(alias+`.id::text`), viewerParam)+` end)`)
}

// BroadcastClause passes the broadcast idExpr names (its uuid) - one person's message to many
// sessions, a conversation of no project with each - when the viewer may see every session it was
// sent to (SessionClause over each copy's target), and is open or is the person who sent it. A
// viewer that is not open - a restricted member - reads its
// own direct messages alone (R4), so it reads only its person's broadcasts; and a credential reads
// its person's broadcast only where it sees every recipient, since it sees what its own list lets
// it see and not all its person sees (R3). It reads the broadcast again by its id, so a statement
// binds a copy it reads by the copy's own broadcast_id as it binds the broadcast by its id. A
// broadcast no row holds passes no viewer. SessionClause answers true or false, never NULL, so its
// negation lets no hidden recipient pass.
func BroadcastClause(idExpr, viewerParam string) string {
	return clause("BroadcastClause", fmt.Sprintf(`exists (
		select 1 from broadcasts clause_broadcast
		where clause_broadcast.id = (%[1]s)
			and (%[2]s or (clause_broadcast.author->>'kind' = 'user' and clause_broadcast.author->>'id' = %[3]s))
			and not exists (
				select 1 from messages clause_broadcast_copy
				where clause_broadcast_copy.broadcast_id = clause_broadcast.id
					and not %[4]s
				offset 0)
		offset 0)`, idExpr, ViewerFlag(viewerParam, "open"), ViewerPerson(viewerParam),
		SessionClause("substr(clause_broadcast_copy.target, 9)", viewerParam)))
}

// issueProjectSQL is the project of the issue keyExpr names, null where no issue holds the key.
func issueProjectSQL(keyExpr string) string {
	return `(select clause_issue.project_key from issues clause_issue where clause_issue.key = ` + keyExpr + `)`
}
