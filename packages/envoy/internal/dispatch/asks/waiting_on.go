package asks

// AskedOfPersonExpression is the one rule for which person a question reaches, resolved as the
// question is read, in three steps: the person somebody handed it to, the assignee of its issue
// until somebody does, and the default assignee of the question's project while the issue holds
// nobody - or where there is no issue at all, which is a question on a project document. Every
// project names a default and the column refuses nothing
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so every question
// reaches a person.
//
// It lives here, below api and docs, because both read it: api's reads of an ask's person
// (askedOfPersonExpression in api/ask_rows.go, which names this, and every read that names that)
// and the one writer of the threads a person takes part in, which an answer and a create from
// either package run (inboxwrite.ReadAsk, and the create's assignee arm in inboxwrite.Record),
// beside the move of that person by the writes that change who an ask waits on and the reopen's
// restore of withdrawn asks (inboxwrite.MoveWaiting). Each names this constant rather than
// spelling the rule again (api's TestOneExpressionDecidesWhoWaitsInOneSpelling).
//
// It names the ask a, its issue i and its project p, so every query reading it joins all three
// the way AskedOfPersonJoins does, the ask's own document among them. It compares with = rather
// than wrapping either column in lower(btrim(...)): asks.asked_of, issues.assignee and
// projects.default_assignee all hold what model.CanonicalLogin returns, so the sides match as they
// are stored.
const AskedOfPersonExpression = `coalesce(a.asked_of, i.assignee, p.default_assignee)`

// AskedOfPersonJoins are the three tables AskedOfPersonExpression reads beside the ask aliased a:
// its issue as i, its own document as ar, and the project of whichever of the two owns it as p.
// Each is an outer join: a question owned by an issue has no document row and one owned by a
// document has no issue row, and the constraint asks_one_owner (migration 0009) guarantees exactly
// one of the two, so the coalesce names the project either way. The leading newline makes it a
// block a caller appends to its own `from asks a`, whatever that caller indents its statement by.
const AskedOfPersonJoins = `
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	left join projects p on p.key = coalesce(i.project_key, ar.project_key)`
