package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// On an issue that Legion runs, a version of the issue's spec (its primary document) or of its
// Verification document is written only by a person, by an architect of the issue, or by an
// architect of an issue above it: the session of an architect row of issue_role_holders on the
// issue's parent chain (dispatch://INFRA-4764/spec, R1 and R2). The chain climbs only to a parent
// of the issue's own project, as "Legion runs" does (issueChainSQL), so an issue whose parent is of
// another project is the root of its own project's tree here. Every route that writes such a
// version calls refuseSpecWriter inside its transaction, after the owner's lock and before its
// first document call and its first write, so a write it refuses with 403
// DOCUMENT_WRITER_NOT_ARCHITECT stores nothing and appends no event; the source guard
// (spec_writer_route_guard_test.go) holds every route of the table to that. The check sits in the
// routes, where the writer is known, and not in the document service: many versions are written
// later, by a settlement, which has no request writer.
//
// The check passes a person at once: a request whose credential holds the person scope, a person
// at a browser among them, read before the handler's transaction opens (specWriterOf), and
// Dispatch's own system writer (parentStatusFixWriter). For every other writer it reads one
// statement over the chain of the write: whether Legion runs it, by the legion-label walk that
// every "Legion runs" answer reads (issueChainSQL, legionOnChainSQL); whether the project is marked
// (project_role_holder_marks, which the first role-holder publication Dispatch takes for an issue
// of the project writes, issue_role_holders.go); the architect sessions on the chain, nearest
// first; the role the writer's session holds on the issue; and the project's proven_sessions
// setting. An issue that Legion does not run, and an issue of a project that no publication marked
// yet, pass: nothing changes for a project until the daemon publishes its first role holders. An
// architect's session passes. For a close, a reopen and a write that takes an issue out of Legion,
// the session that the Legion daemon of the issue's project writes as (legionDaemonSessionOf)
// passes too. While the project's proven_sessions is on, each of those sessions passes only on a
// credential that proves it (model.Actor.Proven): a broker token of that session. Every other
// writer is refused, and the server logs one line for each refusal.
//
// A create of an issue under a parent, and a move of an issue under a parent, are judged on the row
// as the write leaves it: Legion runs it when its own labels or its new parent's chain carry the
// label (legionManagedStatusOfRow), and its architects are those of the parent's chain. A parent of
// another project is not on the row's chain, so a create or a move under it is judged as at a root
// of the row's own project, which this check does not judge (judgeSpecWriter). Every move under a
// parent is also judged, wherever the new parent is, on the chain of each issue of the moved
// subtree that Legion runs before the move, as it stands: the issue's own chain when Legion runs
// it, and else the chain of each labelled issue under it (refuseSpecWriterOfMovedSubtree). So a
// move of a subtree that holds such an issue passes only a person, or an architect at or above each
// of those issues where it stands that is also at or above the new parent when the new parent's
// check holds the move: no architect takes another's issue by moving it, or an unlabelled issue
// above it, under itself, not even after it took the label off its own tree or moved its own issue
// out to the root. The Legion daemon, which writes no parent, passes none of these checks. A create
// under no parent is not checked: the controller files a root with the label legion when a person's
// answer asks Legion to run it.

// specWriterArchitectRole is the role whose sessions write the spec and the Verification document
// of an issue Legion runs: the first role of legionagents.Roles.
const specWriterArchitectRole = "architect"

// specDocumentName and verificationDocumentName are the names the check gives an issue's spec and
// its Verification document (specWriterDocumentName).
const (
	specDocumentName         = "spec.md"
	verificationDocumentName = "verification.md"
)

// specWriterNotArchitectRefusal is the check's refusal as an /api/v2 row declares it: every
// /api/v2 route whose write the check judges, and may refuse, names it among its refusals
// (v2_spec_writer_test.go holds the rows to it).
var specWriterNotArchitectRefusal = apicode.Refusal{Condition: apicode.SpecWriterNotArchitect, Code: apicode.DocumentWriterNotArchitect}

// specWriterDocumentText is the sentence an /api/v2 route that writes a version of a document
// says of the check: write is the write, as "an edit of its spec or Verification document".
func specWriterDocumentText(write string) string {
	return "On an issue Legion runs, " + write + " by a session that is no person and no architect of the issue " +
		"or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT."
}

// specWriter is who writes, as the check judges it: actor is the actor the write records, and person
// is that the check passes the write at once, because the request's credential holds the person
// scope or because the writer is Dispatch's own system writer.
type specWriter struct {
	actor  model.Actor
	person bool
}

// specWriterOf is the writer of r, recorded as actor. It resolves the request's credential
// (callerHoldsScope), which reads the store, so a handler calls it before it opens its
// transaction: the pool refuses a second connection inside one (store.ErrNestedAcquire).
func (s *server) specWriterOf(r *http.Request, actor model.Actor) specWriter {
	return specWriter{actor: actor, person: s.callerHoldsScope(r, model.ScopePerson)}
}

// specWriter is the check's writer of an issue write (writeIssue, a status change's plan): a person
// at a browser or on a credential that holds the person scope, read from the credential the writer
// carries, with no second read of the pool. Dispatch's system writer passes as Dispatch's other own
// writes do, so the check never stops the boot fix of parent and child statuses.
func (writer issueWriter) specWriter() specWriter {
	return specWriter{
		actor:  writer.actor,
		person: writer.human || writer.system || writer.credential.Holds(model.ScopePerson),
	}
}

// specWriteAct is what a checked write does. It words the refusal and the refusal's last sentence
// (refusalEnding), and it decides whether the session of the Legion daemon passes.
type specWriteAct int

const (
	// specWriteDocument writes a version of the document: an upload, an edit, a named version, the
	// resolution, the retraction or the edit of a block ask, the accept or the reject of a
	// suggestion.
	specWriteDocument specWriteAct = iota
	// specWriteCreate creates an issue, with its spec, under a parent.
	specWriteCreate
	// specWriteMove moves an issue under a parent, judged on the new parent's chain.
	specWriteMove
	// specWriteMoveAway moves an issue under a parent, judged as it stands on the chain of the
	// issue, or of a labelled issue under it, that Legion runs before the move.
	specWriteMoveAway
	// specWriteClose closes the issue, which writes the blocks of its open asks withdrawn.
	specWriteClose
	// specWriteReopen reopens the issue, which writes the blocks of its withdrawn asks open again.
	specWriteReopen
	// specWriteOutOfLegion writes the labels or the parent of the issue so that Legion no longer
	// runs it, after which the check no longer holds its documents.
	specWriteOutOfLegion
)

// daemonPasses reports whether the session of the Legion daemon of the issue's project passes the
// check for act: a close, a reopen and a write that takes the issue out of Legion, which the daemon
// makes as it runs an issue.
func (act specWriteAct) daemonPasses() bool {
	return act == specWriteClose || act == specWriteReopen || act == specWriteOutOfLegion
}

// refusalEnding is the last sentence of the check's refusal of act, which the proof refusal does
// not hold (specWriterRefusal.message). A suggestion or a comment cannot carry a move under a
// parent (specWriteMove, specWriteMoveAway) or a write that takes the issue out of Legion
// (specWriteOutOfLegion: a labels write that removes the label legion, the clear of the issue's
// parent, or the unlink of a child by unlink_children), so their refusal sends the writer to a
// writer the message names. The refusal of every other act asks for a suggestion or a comment.
func (act specWriteAct) refusalEnding() string {
	switch act {
	case specWriteMove, specWriteMoveAway, specWriteOutOfLegion:
		return "A suggestion or a comment cannot carry this write: ask one of the writers this message names, a person by `dispatch ask`."
	default:
		return "Propose the change with a suggestion or a comment."
	}
}

// specWrite is one write the check judges.
type specWrite struct {
	act specWriteAct
	// issue is the issue the write is on: the issue whose document it writes, or the issue it
	// moves, closes, reopens or takes out of Legion; for a specWriteMoveAway of an issue Legion
	// does not run, a labelled issue under it (moved). It is empty for a create, whose issue has no
	// key yet.
	issue string
	// document is the name the refusal gives the document a document write writes: spec.md or
	// verification.md. A create, a move, a close, a reopen and a write that takes the issue out of
	// Legion write no text of their own and leave it empty: the refusal names the issue's spec.md
	// for them (documentName).
	document string
	// under is the parent of a create or of a move, and labels are the labels the write leaves on
	// the row: the check reads the row as the write leaves it. under is empty for every other
	// write, which the check reads on the issue's own chain as it stands.
	under  string
	labels []string
	// project is the project of the row a create or a move leaves: the new issue's, or the moved
	// issue's own, which a move keeps. The walk up the row's chain climbs only to a parent of that
	// project (issueChainSQL), so a create or a move under a parent of another project is judged as
	// a root of its own project's tree, which the check does not judge (judgeSpecWriter). A create
	// or a move that leaves project empty is an error, never a pass.
	project string
	// to is the new parent of a specWriteMoveAway, wherever it is, which the refusal names; the
	// check reads that move on the issue's own chain, with under empty. toChecked is that the
	// check of the move on to's chain (specWriteMove) held the move: Legion runs the moved issue as
	// the move leaves it, its project is marked, and to is of its own project. The refusal then
	// says that the writer must be an architect at or above to too.
	to        string
	toChecked bool
	// moved is the issue a specWriteMoveAway moves when that is not issue: an issue Legion does not
	// run, above the labelled issue (refuseSpecWriterOfMovedSubtree). The refusal names it, and so
	// does the refusal's log line.
	moved string
}

// chainIssue is the first issue of the write's chain, the one its architects are read from and
// above: the parent of a create or of a move, and else the issue itself.
func (write specWrite) chainIssue() string {
	if write.under != "" {
		return write.under
	}
	return write.issue
}

// documentName is the name the refusal gives the document of write: the document a document write
// writes, and the issue's spec.md for every other act.
func (write specWrite) documentName() string {
	if write.act == specWriteDocument {
		return write.document
	}
	return specDocumentName
}

// specWriterReadSQL is the check's one statement over the chain from the row start selects
// (issueChainSQL, which carries each row's key): whether Legion runs that row (legionOnChainSQL);
// whether the chain names a subject, the first issue of the chain, which is the issue itself or,
// for a row that start gives no key, its parent when the parent is of the row's own project;
// whether project_role_holder_marks holds the subject's project; that project's proven_sessions
// and its key; the role the session $1 holds on the subject, empty for none; and the session and
// the issue of each architect row of issue_role_holders on the chain, nearest first.
func specWriterReadSQL(start string) string {
	return `
	with recursive ` + issueChainSQL(start, "key") + `,
	subject as (
		select c.key from chain c where c.key is not null order by c.depth limit 1
	),
	project as (
		select i.project_key as key from issues i join subject s on s.key = i.key
	),
	architects as (
		select h.session, c.key, c.depth
		from chain c join issue_role_holders h on h.issue_key = c.key and h.role = '` + specWriterArchitectRole + `'
	)
	select ` + legionOnChainSQL + `,
		exists (select 1 from subject),
		exists (select 1 from project_role_holder_marks m join project p on p.key = m.project_key),
		coalesce((select pr.proven_sessions from projects pr join project p on p.key = pr.key), false),
		coalesce((select p.key from project p), ''),
		coalesce((select h.role from issue_role_holders h join subject s on s.key = h.issue_key where h.session = $1), ''),
		coalesce((select array_agg(a.session order by a.depth) from architects a), '{}'),
		coalesce((select array_agg(a.key order by a.depth) from architects a), '{}')`
}

var (
	// specWriterOfIssueSQL is the check's statement for a write on the issue $2, over its chain as
	// it stands.
	specWriterOfIssueSQL = specWriterReadSQL(`select i.key, i.parent_key, i.labels, i.project_key, 0 as depth from issues i where i.key = $2`)
	// specWriterOfRowSQL is the check's statement for a create or a move under the parent $2, over
	// the row as the write leaves it, $3 its labels and $4 its project: the walk of
	// legionManagedStatusOfRowSQL. The row's own key is not carried, so its architects are read from
	// the parent's chain alone. A parent of another project ends the chain at the row, so the chain
	// names no subject (judgeSpecWriter).
	specWriterOfRowSQL = specWriterReadSQL(`select null::text as key, $2::text as parent_key, $3::text[] as labels, $4::text as project_key, 0 as depth`)
)

// specWriterRead is what the check's one statement reads over the chain of a write.
type specWriterRead struct {
	legion, subject, marked, provenSessions bool
	project, role                           string
	// architects are the architect sessions on the chain, nearest first, and architectIssues the
	// issue of each, in the same order.
	architects, architectIssues []string
}

// specWriterRefusal is a write the check refuses: the write, the writer's session, what the check
// read, and whether the writer's session would pass but the request's credential does not prove
// it while the project's proven_sessions is on.
type specWriterRefusal struct {
	write    specWrite
	session  string
	read     specWriterRead
	unproven bool
}

// specWriterRefusalLine is the message of the line the server logs for each write the check
// refuses (specWriterRefusal.refuse).
const specWriterRefusalLine = "dispatch: refused a document write by a session that is not an architect of the issue"

// refuseSpecWriter is the check of who writes a Legion-run issue's spec or Verification document,
// which every route that writes one calls (the file's comment above). It answers nil for a write it
// passes, and for a write it refuses it logs one line and answers 403
// DOCUMENT_WRITER_NOT_ARCHITECT (specWriterRefusal.refuse). q is the caller's transaction, after
// the owner's lock, or the pool for a file upload's answer before its object is written
// (refuseUploadBeforeItsObject), which the transaction's check still decides.
func refuseSpecWriter(ctx context.Context, q queryer, writer specWriter, write specWrite) error {
	refusal, _, err := judgeSpecWriter(ctx, q, writer, write)
	if err != nil || refusal == nil {
		return err
	}
	return refusal.refuse()
}

// refuse logs the refusal's one line and answers 403 DOCUMENT_WRITER_NOT_ARCHITECT. The line of a
// move refused on a labelled issue under the moved issue names the moved issue too.
func (refusal *specWriterRefusal) refuse() error {
	write := refusal.write
	issue := write.issue
	if issue == "" {
		issue = write.under
	}
	logged := []any{"issue", issue, "document", write.documentName(), "session", refusal.session,
		"reason", refusal.reason(), "architects", refusal.read.architects}
	if write.moved != "" {
		logged = append(logged, "moved", write.moved)
	}
	slog.Info(specWriterRefusalLine, logged...)
	return errorf(apicode.DocumentWriterNotArchitect, "%s", refusal.message())
}

// judgeSpecWriter is the check's decision on write, with no log line: nil when it passes, and else
// the refusal. The bool is that the check held write: Legion runs the chain, the project is marked,
// and the chain names a subject. Every refusal is held. It reads one statement through q, and none
// for a person, whom it passes unheld. A create or a move that names no project is an error, never
// a pass.
func judgeSpecWriter(ctx context.Context, q queryer, writer specWriter, write specWrite) (*specWriterRefusal, bool, error) {
	if writer.person {
		return nil, false, nil
	}
	if write.under != "" && write.project == "" {
		return nil, false, fmt.Errorf("read who may write the documents of a row under %s: the write names no project", write.under)
	}
	session := sessionOf(writer.actor)
	statement, args := specWriterOfIssueSQL, []any{session, write.issue}
	if write.under != "" {
		statement, args = specWriterOfRowSQL, []any{session, write.under, write.labels, write.project}
	}
	var read specWriterRead
	if err := q.QueryRow(ctx, statement, args...).Scan(
		&read.legion, &read.subject, &read.marked, &read.provenSessions, &read.project, &read.role, &read.architects, &read.architectIssues,
	); err != nil {
		return nil, false, fmt.Errorf("read who may write the documents of %s: %w", write.chainIssue(), err)
	}
	if write.under != "" && !read.subject {
		// A create or a move under a parent of another project: the row's chain ends at the row
		// (issueChainSQL), so it names no subject. The row is a root of its own project's tree,
		// and the check judges it as a create under no parent, which it does not check: the row's
		// Legion tree has no parent in its own project.
		return nil, false, nil
	}
	if !read.legion || !read.marked {
		return nil, false, nil
	}
	passes := session != "" && (slices.Contains(read.architects, session) ||
		(write.act.daemonPasses() && session == legionDaemonSessionOf(write.issue)))
	if passes && (!read.provenSessions || writer.actor.Proven) {
		return nil, true, nil
	}
	return &specWriterRefusal{write: write, session: writer.actor.ID, read: read, unproven: passes}, true, nil
}

// reason is why the check refused, as its log line gives it.
func (refusal *specWriterRefusal) reason() string {
	if refusal.unproven {
		return "the project records proven sessions, and the request's credential does not prove the session"
	}
	return "the session is not an architect of the issue or of an issue above it"
}

// message is the one wording of the check's refusal, in plain short sentences: what the session
// cannot do, on which issue and to which document, who can, the role the session holds on the
// issue, and each architect session Dispatch records on the chain with its issue. It ends with the
// sentence of the write's act (specWriteAct.refusalEnding). The proof refusal names the project,
// says that the request's credential does not prove the session, and ends with neither sentence.
func (refusal *specWriterRefusal) message() string {
	write, session, read := refusal.write, refusal.session, refusal.read
	chain, document := write.chainIssue(), write.documentName()
	var act, why string
	switch write.act {
	case specWriteCreate:
		act = "create an issue under " + chain
		why = "Legion runs the new issue, and the create writes its " + document + "."
	case specWriteMove:
		act = "move " + write.issue + " under " + chain
		why = "Legion runs " + write.issue + " under " + chain + ", and the move changes who can write its " + document + "."
	case specWriteMoveAway:
		moved, under := write.issue, ""
		if write.moved != "" {
			moved, under = write.moved, ", which sits under "+write.moved+","
		}
		act = "move " + moved + " under " + write.to
		why = "Legion runs " + write.issue + under + " where it stands, and the move under " + write.to + " changes who can write its " + document + "."
	case specWriteClose:
		act = "close " + write.issue
		why = "Legion runs " + write.issue + ", and the close writes the asks in its " + document + "."
	case specWriteReopen:
		act = "reopen " + write.issue
		why = "Legion runs " + write.issue + ", and the reopen writes the asks in its " + document + "."
	case specWriteOutOfLegion:
		act = "take " + write.issue + " out of Legion"
		why = "Legion runs " + write.issue + ". When Legion stops running it, any session can write its " + document + "."
	default:
		act = "write " + document + " of " + write.issue
		why = "Legion runs " + write.issue + "."
	}
	var message strings.Builder
	fmt.Fprintf(&message, "Session %s cannot %s.", session, act)
	if refusal.unproven {
		fmt.Fprintf(&message, " Project %s records proven sessions, so it takes this write only on a credential that proves the writer's session."+
			" This request's credential does not prove session %s.", read.project, session)
		return message.String()
	}
	who := "a person or an architect of " + chain + " or of an issue above it"
	switch {
	case write.act.daemonPasses():
		who = "a person, an architect of " + chain + " or of an issue above it, or the Legion daemon of its project"
	case write.act == specWriteMoveAway && write.toChecked:
		who += " that is also an architect of " + write.to + " or of an issue above it"
	}
	fmt.Fprintf(&message, " %s Only %s can %s.", why, who, act)
	if read.role != "" {
		fmt.Fprintf(&message, " Session %s is the %s of %s.", session, read.role, chain)
	} else {
		fmt.Fprintf(&message, " Session %s holds no role on %s.", session, chain)
	}
	if len(read.architects) == 0 {
		fmt.Fprintf(&message, " Dispatch records no architect on %s or above it.", chain)
	} else {
		named := make([]string, len(read.architects))
		for index, architect := range read.architects {
			named[index] = "session " + architect + " on " + read.architectIssues[index]
		}
		fmt.Fprintf(&message, " Dispatch records these architects on %s and above it: %s.", chain, strings.Join(named, ", "))
	}
	message.WriteString(" " + write.act.refusalEnding())
	return message.String()
}

// specWriterDocumentName is the name the check gives a stored document, and whether the check
// holds the document at all: spec.md for an issue's spec (isSpecDocument), and verification.md for
// its Verification document (documentPlays, so an older row whose role is still null is held by
// its slug). The check holds no other document, and no document of a project or of a session.
func specWriterDocumentName(artifact model.Artifact) (string, bool) {
	switch {
	case artifact.IssueKey == nil:
		return "", false
	case isSpecDocument(artifact):
		return specDocumentName, true
	case documentPlays(artifact, model.ArtifactRoleVerification):
		return verificationDocumentName, true
	}
	return "", false
}

// refuseSpecWriterOfDocument is the check of a write of the stored document artifact, which it
// passes for a document the check does not hold.
func refuseSpecWriterOfDocument(ctx context.Context, q queryer, writer specWriter, artifact model.Artifact) error {
	document, held := specWriterDocumentName(artifact)
	if !held {
		return nil
	}
	return refuseSpecWriter(ctx, q, writer, specWrite{issue: *artifact.IssueKey, document: document})
}

// refuseSpecWriterOfDocuments is the check of a write of the stored documents ids, read through q
// in their order. The first document the check holds decides, and the refusal names it: the
// documents one write changes together are documents of one owner (a block ask's document and
// each copy of the ask), so one check of the issue decides them all.
func refuseSpecWriterOfDocuments(ctx context.Context, q queryer, writer specWriter, ids ...string) error {
	if writer.person {
		return nil
	}
	for _, id := range ids {
		artifact, err := loadArtifactRowByID(ctx, q, id)
		if err != nil {
			return err
		}
		if document, held := specWriterDocumentName(artifact); held {
			return refuseSpecWriter(ctx, q, writer, specWrite{issue: *artifact.IssueKey, document: document})
		}
	}
	return nil
}

// refuseSpecWriterOfAsk is the check of a write that settles a block ask's closed state into its
// document and into each copy of the ask (writeClosedBlockState): the block's document first, then
// the copies (docs.API.CopiesOf). The copies are read only when the block's document does not
// decide: a document the check holds decides alone, and a copy is a document of the same owner, so
// a block on a document of no issue has no copy the check holds. ctx is joined to the caller's
// transaction tx, which holds the ask's owner lock. An ask with no block writes no document.
func (s *server) refuseSpecWriterOfAsk(ctx context.Context, tx pgx.Tx, writer specWriter, ask model.Ask) error {
	if writer.person || ask.BlockID == nil {
		return nil
	}
	block, err := blockArtifactOf(ask)
	if err != nil {
		return err
	}
	artifact, err := loadArtifactRowByID(ctx, tx, block)
	if err != nil {
		return err
	}
	if _, held := specWriterDocumentName(artifact); held || artifact.IssueKey == nil {
		return refuseSpecWriterOfDocument(ctx, tx, writer, artifact)
	}
	copies, err := s.deps.Docs.CopiesOf(ctx, ask)
	if err != nil {
		return err
	}
	return refuseSpecWriterOfDocuments(ctx, tx, writer, copies...)
}

// refuseSpecWriterOfIssueWrite is the check of one write of an issue: a PATCH's (writeIssue) or a
// step of a status change, judged on the step as its writer read it under the owner locks. A move
// under a parent is checked on the new parent's chain, with the labels the write leaves, when the
// new parent is of the issue's own project (judgeSpecWriter). Every move under a parent is also
// checked, wherever the new parent is, as it stands, on the chain of each issue of the moved
// subtree that Legion runs before the move (refuseSpecWriterOfMovedSubtree): so a move of a subtree
// that holds such an issue passes only an architect at or above each of those issues that is also
// at or above the new parent when the new parent's check holds the move, and an architect that
// took the label off its own tree, or moved its own issue out to the root, cannot then move another
// tree's issue under it. The daemon, which writes no parent, passes none of these checks. They
// alone judge a move out of every tree Legion runs: when Legion runs the issue before the move, the
// issue's own chain judges it, with no pass for the daemon. A close and a reopen are checked on the
// issue's own chain, with the daemon's pass. A labels write or a write of no parent after which
// Legion no longer runs the issue - Legion runs it before the write (legionManagedStatus) and not
// as the write leaves it (legionManagedStatusOfRow) - is checked on the issue's chain as it stands
// before the write, with the daemon's pass. A labels write that keeps the label legion on the
// chain, and a labels_add, which cannot remove it, take nothing out.
func refuseSpecWriterOfIssueWrite(ctx context.Context, tx pgx.Tx, writer specWriter, step issueWriteStep) error {
	if writer.person {
		return nil
	}
	// The labels the write leaves: those it writes, or, for a write of the parent alone, those the
	// issue holds, read under the owner lock its writer holds. Only a move and a write that may take
	// the issue out of Legion read them.
	var labels []string
	switch {
	case step.labelsAfter != nil:
		labels = *step.labelsAfter
	case step.changesParent():
		if err := tx.QueryRow(ctx, `select labels from issues where key = $1`, step.key).Scan(&labels); err != nil {
			return err
		}
	}
	moves := step.changesParent() && step.parentAfter != nil
	if moves {
		refusal, held, err := judgeSpecWriter(ctx, tx, writer, specWrite{
			act: specWriteMove, issue: step.key, under: *step.parentAfter, labels: labels, project: step.project,
		})
		if err != nil {
			return err
		}
		if refusal != nil {
			return refusal.refuse()
		}
		// Every move under a parent is checked on the chain of each issue of the moved subtree that
		// Legion runs before the move too, wherever the new parent is. Its refusal names the new
		// parent's check only when that check held the move (held).
		if err := refuseSpecWriterOfMovedSubtree(ctx, tx, writer, step.key, step.project, *step.parentAfter, held); err != nil {
			return err
		}
	}
	var act specWriteAct
	switch {
	case step.statusAfter != nil && *step.statusAfter == "done" && step.statusBefore != "done":
		act = specWriteClose
	case step.statusAfter != nil && *step.statusAfter != "done" && step.statusBefore == "done":
		act = specWriteReopen
	case !moves && (step.changesParent() || step.labelsAfter != nil):
		before, err := legionManagedStatus(ctx, tx, step.key)
		if err != nil || !before {
			return err
		}
		parent := step.parentBefore
		if step.reparent {
			parent = step.parentAfter
		}
		after, err := legionManagedStatusOfRow(ctx, tx, step.key, step.project, parent, labels)
		if err != nil || after {
			return err
		}
		act = specWriteOutOfLegion
	default:
		return nil
	}
	return refuseSpecWriter(ctx, tx, writer, specWrite{act: act, issue: step.key})
}

// labelledIssuesSQL keeps, of the issues $1, those whose own labels carry the label legion
// (legionLabelSQL), in the order of $1: the labelled issues of a moved subtree, in the order of the
// descendant walk (refuseSpecWriterOfMovedSubtree).
const labelledIssuesSQL = `
	select i.key from unnest($1::text[]) with ordinality as k(key, ord) join issues i on i.key = k.key
	where exists (select 1 from unnest(i.labels) as label where ` + legionLabelSQL + `)
	order by k.ord`

// refuseSpecWriterOfMovedSubtree is the check of writer's move of the issue key, of project,
// under the parent to, wherever to is, on the chain of each issue of the moved subtree that Legion
// runs before the move, as it stands. toChecked is that the check of the move on to's chain held
// the move (judgeSpecWriter), which the refusal then says. When Legion runs the issue before the
// move, its own chain decides: an architect at or above it is at or above every issue under it.
// Otherwise the chain of each labelled issue under it decides, nearest first, and the first
// refusal names that issue. The moved subtree is the issues whose chain climbs to key: those
// reached from it through parents of its own project alone, since a chain ends at the first
// ancestor of another project (issueChainSQL), so the move changes no chain below an issue of
// another project. The descendant walk (refs.IssueDescendantRows) reads the rows, and
// labelledIssuesSQL keeps, of the issues of the moved subtree, those that carry the label, by the
// one match every answer to "Legion runs" reads. A subtree that holds no issue Legion runs passes.
// A person passes before this runs, and the daemon passes none of these checks. As in
// legionManagedStatus, no lock holds the labels this reads of the issues around key: a concurrent
// label write can change the answer before the move commits.
func refuseSpecWriterOfMovedSubtree(ctx context.Context, tx pgx.Tx, writer specWriter, key, project, to string, toChecked bool) error {
	runs, err := legionManagedStatus(ctx, tx, key)
	if err != nil {
		return err
	}
	if runs {
		return refuseSpecWriter(ctx, tx, writer, specWrite{act: specWriteMoveAway, issue: key, to: to, toChecked: toChecked})
	}
	rows, err := refs.IssueDescendantRows(ctx, tx, key)
	if err != nil {
		return err
	}
	// The rows come nearest first, so a parent is placed before its children.
	moved := map[string]bool{key: true}
	subtree := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Project == project && moved[row.Parent] {
			moved[row.Key] = true
			subtree = append(subtree, row.Key)
		}
	}
	if len(subtree) == 0 {
		return nil
	}
	labelledRows, err := tx.Query(ctx, labelledIssuesSQL, subtree)
	if err != nil {
		return fmt.Errorf("read the labelled issues under %s: %w", key, err)
	}
	labelled, err := pgx.CollectRows(labelledRows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the labelled issues under %s: %w", key, err)
	}
	for _, issue := range labelled {
		if err := refuseSpecWriter(ctx, tx, writer, specWrite{
			act: specWriteMoveAway, issue: issue, to: to, toChecked: toChecked, moved: key,
		}); err != nil {
			return err
		}
	}
	return nil
}

// spareSettlementOfARefusedClose reports whether the check refuses writer's close of the issue key,
// read once on the pool before the close's transaction opens (writeIssue), so that a refused close
// does not settle the issue's documents first (settleIssueBeforeClose). It logs nothing and answers
// no refusal: the check under the close's owner locks decides, after the write's other refusals. A
// read that fails spares nothing.
func (s *server) spareSettlementOfARefusedClose(ctx context.Context, writer issueWriter, key string) bool {
	pool, err := s.readyPool()
	if err != nil {
		return false
	}
	refusal, _, err := judgeSpecWriter(ctx, pool, writer.specWriter(), specWrite{act: specWriteClose, issue: key})
	return err == nil && refusal != nil
}
