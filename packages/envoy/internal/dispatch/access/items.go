package access

// The read rules of what a thread is made of - an ask, a comment and a message - over the row a
// statement reads, for the viewer that statement binds at viewerParam. A row a project owns is
// read through ProjectClause, and a row of a conversation no project owns through PartyClause
// over that conversation's parties, the ones the node writers state (graphstore.ScopeOf,
// graphstore.ThreadParties). Each answers true or false, never NULL, as every clause here does
// (clause).

// IssueProjectClause passes the issue keyExpr names when the viewer sees the issue's project. An
// issue no row holds passes nobody.
func IssueProjectClause(keyExpr, viewerParam string) string {
	return clause("IssueProjectClause", ProjectClause(issueProjectSQL(keyExpr), viewerParam))
}

// ItemOwnerClause passes an ask or a comment, a row that an issue (issueExpr) or a document
// (artifactExpr) owns: when the viewer sees the project that owner is in - the issue's, else the
// document's - or, for a file of an agent's conversation, which no project owns, when the viewer
// may read that conversation, whose one party is the conversation's session.
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
// the parties graphstore.ArtifactNode states on the document's node). It reads the row's own
// columns, so a statement that holds the row binds it rather than ItemOwnerClause, which reads the
// document again by its id. A document its reader may not read is a row the statement does not
// find, and answers exactly as an id nobody holds.
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
// over MessagePartiesSQL). A conversation whose parties were never stated, a message whose node
// Dispatch does not hold or whose node names no party, passes an open viewer alone while no
// project is private, as every conversation did before a project could be, and no viewer but the
// server's once one is.
func MessageClause(alias, viewerParam string) string {
	return clause("MessageClause", `(case when `+alias+`.issue_key is not null then `+IssueProjectClause(alias+`.issue_key`, viewerParam)+`
		else `+PartyClause(MessagePartiesSQL(alias+`.id::text`), viewerParam)+` end)`)
}

// issueProjectSQL is the project of the issue keyExpr names, null where no issue holds the key.
func issueProjectSQL(keyExpr string) string {
	return `(select clause_issue.project_key from issues clause_issue where clause_issue.key = ` + keyExpr + `)`
}
