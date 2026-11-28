package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The list of terms of The Spec (R50): what the approved covered documents under a root define,
// every definition of each term in join order (approvedSpecTexts), the definition of each term that
// governs once the person approves the documents a judgment names (governingUnder), and the lock
// that a person's approval and a move into a root take before they change the list. The judgment
// that reads it is in spec_form.go, and the parent history that orders an arrival is in
// spec_parent_history.go.

// approvedUnder is what the approved covered documents under root hold (approvedSpecTexts). A
// judgment that locks takes the lock of root's list first (lockSpecTerms).
func (j *specFormJudge) approvedUnder(ctx context.Context, root string) (approvedSpec, error) {
	if approved, read := j.approved[root]; read {
		return approved, nil
	}
	if j.lock {
		if err := lockSpecTerms(ctx, j.q, root); err != nil {
			return approvedSpec{}, err
		}
	}
	approved, err := j.form.approvedSpecTexts(ctx, j.q, root)
	if err != nil {
		return approvedSpec{}, err
	}
	j.approved[root] = approved
	return approved, nil
}

// lockSpecTerms takes the lock of the list of terms of root until the transaction of q ends. A
// person's approval of a covered document decides against the list and then changes it, so it
// takes this lock before it reads the list: of two approvals under one root at one time, the
// second reads the list once the first committed, with the first one's terms in it, and a second
// definition in it is refused. It is the single-key form, keyed as the api package's other
// transaction locks are (lockProjectRankAllocation).
func lockSpecTerms(ctx context.Context, q queryer, root string) error {
	if err := q.QueryRow(ctx, `select pg_advisory_xact_lock(hashtext('spec-terms:' || $1))`, root).Scan(nil); err != nil {
		return fmt.Errorf("lock the list of terms of %s: %w", root, err)
	}
	return nil
}

// approvedSpec is what the approved covered documents under a root hold that a judgment reads: the
// names of the components and interfaces they define, which are nouns of the system, every
// definition of each term, in join order, whose first definitions are the root's list of terms,
// and each approved version that Dispatch cannot read, which gives nothing to compare. Each keeps
// the document that holds it, because a comparison reads the versions that a judgment names in
// place of their approved ones (governingUnder). An answer reads the approved versions alone
// (read).
type approvedSpec struct {
	names []approvedName
	// definitions is the approved definitions of each term, in join order (orderDefinitions),
	// the terms sorted by the term of their first definition, which governs.
	definitions [][]approvedTerm
	unread      []specFormUnread
}

type approvedName struct{ artifactID, name string }

type approvedTerm struct {
	artifactID string
	specFormTerm
}

// systemNames is the names of the system that the documents skip does not name define.
func (a approvedSpec) systemNames(skip map[string]bool) []string {
	var names []string
	for _, name := range a.names {
		if !skip[name.artifactID] {
			names = append(names, name.name)
		}
	}
	return names
}

// refusingDefinitions is the gate's view of unread, which no answer gives: each latest approved
// version that Dispatch cannot read of a document that skip does not name. No rule can compare a
// definition with its terms, so each one refuses every definition of the judgment
// (specform.UnreadListFinding). The version that a judgment names of a document stands in for its
// approved one, so skip leaves that document out, and an older approved version (orders) only
// orders the list, so it refuses nothing.
func (a approvedSpec) refusingDefinitions(skip map[string]bool) []specFormUnread {
	var refusing []specFormUnread
	for _, each := range a.unread {
		if !skip[each.ArtifactID] && !each.orders {
			refusing = append(refusing, each)
		}
	}
	return refusing
}

// read is the root's list of terms as every answer gives it: the definition of each term that
// governs, with the whole record of the approved versions that Dispatch could not read as it made
// the list (specFormList). It leaves out no document that a judgment names, since the list lacks
// the terms of such a version whoever reads it. Each answer shares the record, clipped, so an
// append to one answer's record writes nothing into another's.
func (a approvedSpec) read() specFormList {
	read := specFormList{Terms: make([]specFormTerm, len(a.definitions)), Unread: slices.Clip(a.unread)}
	for index, definitions := range a.definitions {
		read.Terms[index] = definitions[0].specFormTerm
	}
	return read
}

// listedWhere is where a term of the list stands: "<document> version <n> of <issue key>".
func listedWhere(document string, version int, issue string) string {
	return fmt.Sprintf("%s version %d of %s", document, version, issue)
}

// approvedSpecTexts reads the latest approved version of each covered document under root: the
// root's terms.md, and the spec and the Verification document of each issue whose nearest named
// root is root (refs.IssueDescendants, less each named root below root and its own subtree). One
// statement reads every such document with the markdown of that version, in the order of
// docs.LatestApprovedReview, so the statements of a judgment do not grow with the documents under
// its root; a document that no person approved has no row. Only a person approves a covered
// document: a plan review approves a plan document alone (createArtifactPlanReview), whose role
// its slug gives (documentRole). So an agent's version adds nothing to the list until the person
// approves it.
//
// The list holds one definition of each term (R50), by specform.TermKey. A term that one document
// alone defines, or that the root's terms.md defines, has that definition. Of the definitions of a
// term in several other documents, the list holds the one that joined it first, and it keeps
// every other in join order (orderDefinitions): a judgment that names the document of the first
// and drops the term there compares with the next one (governingUnder). Each definition has its
// own date (definitionJoins): the later of the move that last brought its issue under the root
// and the first approval of the earliest approved version of its document from which each later
// approved version holds the term. So a term that a later version added joins at the approval of
// that version, and a definition that a later version changed keeps its date. The move is read
// from the events (readParentHistory, parentHistory.arrival), which every build of Dispatch writes
// for every move, so the build that handled a move changes no order, and a rollback changes none.
// So a definition that the list holds is never displaced by an arrival, and a change of a listed
// definition waits for the person's approval. Two definitions that joined at one move come in the
// order of their approvals, then in the order in which Dispatch created their documents. A
// reparent into the root, or an approval from before the setting named the root, can put a second
// definition under the root, and the read of its document names the definition that the list
// holds as a finding. When the document that the list keeps a term from leaves the root, the list
// holds the definition that joined next: the person approved each, and a move changes the tree and
// no text. The setting decides which issues are roots at every event: an issue named today is a
// root for the moves before it was named too. It reads the issues under root as everyReader: the
// judgment is the same for each person whose request or approval it judges, and its findings are
// kept as the reason of a request that others read, so an issue reserved for its author, and what
// is below it, gives no text to it.
func (f SpecForm) approvedSpecTexts(ctx context.Context, q queryer, root string) (approvedSpec, error) {
	issues, err := refs.IssueDescendants(ctx, q, everyReader, root)
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the issues under %s: %w", root, err)
	}
	for _, inner := range f.roots {
		if inner == root || !slices.Contains(issues, inner) {
			continue
		}
		below, err := refs.IssueDescendants(ctx, q, everyReader, inner)
		if err != nil {
			return approvedSpec{}, fmt.Errorf("read the issues under %s: %w", inner, err)
		}
		issues = slices.DeleteFunc(issues, func(issue string) bool { return issue == inner || slices.Contains(below, issue) })
	}
	rows, err := q.Query(ctx, `
		select covered.id::text, covered.name, covered.issue_key, covered.slug, covered.created_at,
			approved.version, approved.number, approved.markdown
		from artifacts covered
		cross join lateral (
			select latest.version, stored.number, coalesce(stored.markdown, '') as markdown
			from (
				select artifact_reviews.version from artifact_reviews
				where artifact_reviews.artifact_id = covered.id and artifact_reviews.state = 'approved'
				order by `+docs.ReviewOrder+`
				limit 1
			) latest
			left join artifact_versions stored on stored.artifact_id = covered.id and stored.number = latest.version
		) approved
		where covered.kind = 'doc' and (
			(covered.issue_key = any($1::text[]) and (covered.is_primary or `+documentRolePredicate("covered", model.ArtifactRoleVerification)+`))
			or (covered.issue_key = $2 and covered.slug = $3)
		)
		order by covered.issue_key, covered.id
	`, issues, root, termsSlug)
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the approved documents under %s: %w", root, err)
	}
	type approvedVersion struct {
		document approvedSpecDocument
		stored   *int
		markdown string
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (approvedVersion, error) {
		var each approvedVersion
		document := &each.document
		err := row.Scan(&document.id, &document.name, &document.issue, &document.slug, &document.createdAt, &document.version, &each.stored, &each.markdown)
		return each, err
	})
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the approved documents under %s: %w", root, err)
	}
	var (
		approved  = approvedSpec{unread: []specFormUnread{}} // an answer with no unread version answers [], not null
		documents []approvedSpecDocument
	)
	for _, each := range versions {
		document := each.document
		if each.stored == nil {
			return approvedSpec{}, errorf(http.StatusNotFound, "VERSION_NOT_FOUND", "%s has no version %d", document.name, document.version)
		}
		extracted, err := specform.ExtractDocument(document.name, document.version, each.markdown)
		if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
			approved.unread = append(approved.unread, unreadApproved(document, document.version, err, false))
			continue
		}
		if err != nil {
			return approvedSpec{}, err
		}
		for _, text := range extracted.Texts {
			if text.Kind == specform.KindName {
				approved.names = append(approved.names, approvedName{artifactID: document.id, name: text.Body})
			}
		}
		document.terms, document.termsMd = extracted.Terms, document.issue == root && document.slug == termsSlug
		documents = append(documents, document)
	}
	definers, keys := definersOf(documents)
	joins, older, err := definitionJoins(ctx, q, root, f.roots, documents, contestedOf(documents, definers, keys))
	if err != nil {
		return approvedSpec{}, err
	}
	approved.unread = append(approved.unread, older...)
	approved.definitions = orderDefinitions(documents, definers, keys, joins)
	return approved, nil
}

// definer is one definition of a term among the documents that approvedSpecTexts read: the index
// of its document, and its term line.
type definer struct {
	document int
	term     specform.Term
}

// definersOf is the definitions of each key (specform.TermKey) that documents hold, one of each
// document, its first term line of the key, and each key in the order it was read.
func definersOf(documents []approvedSpecDocument) (map[string][]definer, []string) {
	definers := map[string][]definer{}
	var keys []string
	for index, document := range documents {
		for _, term := range document.terms {
			key := specform.TermKey(term.Name)
			held := definers[key]
			if len(held) > 0 && held[len(held)-1].document == index {
				continue // a second term line of the key in one document: the first one counts
			}
			if len(held) == 0 {
				keys = append(keys, key)
			}
			definers[key] = append(held, definer{document: index, term: term})
		}
	}
	return definers, keys
}

// contestedOf is, for each document by its index, the keys of its definitions whose join order
// definitionJoins reads: those of a key that two documents other than the root's terms.md define.
// The root's terms.md comes first at any date, but a judgment that names it at a version that
// drops the term compares with the next definition, so the order of the others counts too.
func contestedOf(documents []approvedSpecDocument, definers map[string][]definer, keys []string) map[int][]string {
	contested := map[int][]string{}
	for _, key := range keys {
		others := slices.DeleteFunc(slices.Clone(definers[key]), func(d definer) bool { return documents[d.document].termsMd })
		if len(others) > 1 {
			for _, d := range others {
				contested[d.document] = append(contested[d.document], key)
			}
		}
	}
	return contested
}

// orderDefinitions is the definitions of each key in join order (R50), the keys sorted by the term
// of their first definition: the root's terms.md first, then by the date each joined the list
// (joinOrder.joined), then by the first approval of its version, then in the order in which
// Dispatch created their documents.
func orderDefinitions(documents []approvedSpecDocument, definers map[string][]definer, keys []string, joins map[definitionOf]joinOrder) [][]approvedTerm {
	ordered := make([][]approvedTerm, 0, len(keys))
	for _, key := range keys {
		held := slices.Clone(definers[key])
		slices.SortFunc(held, func(a, b definer) int {
			if aTerms, bTerms := documents[a.document].termsMd, documents[b.document].termsMd; aTerms != bTerms {
				if aTerms {
					return -1
				}
				return 1
			}
			aJoin, bJoin := joins[definitionOf{a.document, key}], joins[definitionOf{b.document, key}]
			aDocument, bDocument := documents[a.document], documents[b.document]
			return cmp.Or(aJoin.joined().compare(bJoin.joined()), aJoin.approved.compare(bJoin.approved),
				aDocument.createdAt.Compare(bDocument.createdAt), strings.Compare(aDocument.id, bDocument.id))
		})
		definitions := make([]approvedTerm, len(held))
		for index, d := range held {
			document := documents[d.document]
			definitions[index] = approvedTerm{artifactID: document.id, specFormTerm: specFormTerm{
				Term: d.term.Name, Definition: d.term.Definition, Document: document.name, Version: document.version, Issue: document.issue,
			}}
		}
		ordered = append(ordered, definitions)
	}
	slices.SortStableFunc(ordered, func(a, b []approvedTerm) int {
		return strings.Compare(strings.ToLower(a[0].Term), strings.ToLower(b[0].Term))
	})
	return ordered
}

// unreadApproved is version of document, an approved version that the parser refuses (err, a
// specform.UnreadableError): it gives nothing to compare, so a judgment goes on without its texts,
// logs it once, and names it to the writer (specFormUnread). older says that it is an older
// approved version, which only orders the definitions of document; a judgment of a document that
// holds a definition refuses the latest one (specform.UnreadListFinding).
func unreadApproved(document approvedSpecDocument, version int, err error, older bool) specFormUnread {
	slog.Warn("dispatch: the form of The Spec compares no text with an approved version that it cannot read",
		"artifact", document.id, "document", document.name, "issue", document.issue, "version", version, "older", older, "error", err)
	cause := err.Error()
	if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
		cause = unreadable.Err.Error()
	}
	line := specform.UnreadLine(document.name, version, document.issue)
	if older {
		line = specform.UnreadOlderLine(document.name, version, document.issue)
	}
	return specFormUnread{
		Document: document.name, ArtifactID: document.id, Issue: document.issue, Version: version,
		Line: line, cause: cause, orders: older,
	}
}

// approvedSpecDocument is a covered document with an approved version, as the list reads it: its
// id, its name, its issue, its slug and when Dispatch created it, its latest approved version, the
// term lines of that version, and whether it is the root's terms.md.
type approvedSpecDocument struct {
	id, name, issue, slug string
	createdAt             time.Time
	version               int
	terms                 []specform.Term
	termsMd               bool
}

// moment is a place in the order of Dispatch's writes: the id of an event (events.id), which
// orders the writes that the lock of a root's list serialises (lockSpecTerms), or, for an
// approval written before reviews kept their event (migration 0120), no event and its time. Such
// an approval comes before every write that has an event.
type moment struct {
	event int64
	at    time.Time
}

func (m moment) compare(other moment) int {
	return cmp.Or(cmp.Compare(m.event, other.event), m.at.Compare(other.at))
}

// joinOrder is when one definition joined the list of a root: the first approval of the earliest
// approved version of its document from which each later approved version holds its term, and
// the move that last brought its issue under the root, if a move did.
type joinOrder struct {
	approved moment
	arrived  *moment
}

// joined is the later of the approval and the arrival.
func (o joinOrder) joined() moment {
	if o.arrived != nil && o.arrived.compare(o.approved) > 0 {
		return *o.arrived
	}
	return o.approved
}

// definitionOf names one definition of the list: the index of its document among the documents
// approvedSpecTexts read, and the key of its term.
type definitionOf struct {
	document int
	key      string
}

// definitionJoinsQuery reads each approval of each document that $1 names, newest first in the
// order the approvals' events committed (docs.ReviewOrder): its version, its event (events.id, 0
// for an approval from before reviews kept their event, migration 0120) and its time. The newest
// approval of each version other than the one $2 names for its document also carries that
// version's markdown, so one query reads every older approved version. The approvals are read from
// artifact_reviews alone, under that name, since docs.ReviewOrder names its columns so.
const definitionJoinsQuery = `
	with approved as (
		select artifact_id, version, event_seq, created_at,
			row_number() over (partition by artifact_id order by ` + docs.ReviewOrder + `) as newest,
			row_number() over (partition by artifact_id, version order by ` + docs.ReviewOrder + `) as of_version
		from artifact_reviews
		where artifact_id = any($1::uuid[]) and state = 'approved'
	)
	select ap.artifact_id::text, ap.version, coalesce(e.id, 0), ap.created_at,
		case when ap.version <> latest.version and ap.of_version = 1 then coalesce(v.markdown, '') end
	from approved ap
	join unnest($1::uuid[], $2::int[]) as latest(id, version) on latest.id = ap.artifact_id
	join artifacts a on a.id = ap.artifact_id
	left join events e on e.issue_key = a.issue_key and e.seq = ap.event_seq
	left join artifact_versions v on v.artifact_id = ap.artifact_id and v.number = ap.version
	order by ap.artifact_id, ap.newest
`

// definitionJoins is when each definition that contested names joined the list of root: for each
// document, by its index among documents, the keys of its terms whose join order counts
// (contestedOf). It reads the approvals of those documents and their older approved versions
// in one query (definitionJoinsQuery), and the moves of their issues (readParentHistory), and
// nothing when contested is empty. A definition joins at the first approval of the earliest
// approved version from which each later approved version holds its term, or at the arrival of its
// issue under root (parentHistory.arrival), when that came later. An older approved version that
// the parser refuses holds no term, so a definition joins at a version that Dispatch can read, and
// the version is named in unread as one that only orders the list (specFormUnread.orders).
func definitionJoins(ctx context.Context, q queryer, root string, roots []string, documents []approvedSpecDocument, contested map[int][]string) (map[definitionOf]joinOrder, []specFormUnread, error) {
	joins := map[definitionOf]joinOrder{}
	if len(contested) == 0 {
		return joins, nil, nil
	}
	var (
		ids      = make([]string, 0, len(contested))
		versions = make([]int, 0, len(contested))
		moved    []string
		unread   []specFormUnread
	)
	byID := make(map[string]approvedSpecDocument, len(contested))
	queued := make(map[string]bool, len(contested)) // each issue of moved once
	for index := range contested {
		document := documents[index]
		ids = append(ids, document.id)
		versions = append(versions, document.version)
		byID[document.id] = document
		if issue := document.issue; issue != root && !queued[issue] {
			queued[issue] = true
			moved = append(moved, issue)
		}
	}
	type approval struct {
		version int
		at      moment
	}
	approvals := map[string][]approval{}
	held := map[string]map[int]map[string]bool{} // the keys of each approved version of each document
	rows, err := q.Query(ctx, definitionJoinsQuery, ids, versions)
	if err != nil {
		return nil, nil, fmt.Errorf("read the approved versions of %d covered documents: %w", len(ids), err)
	}
	var (
		id         string
		one        approval
		event      int64
		approvedAt time.Time
		markdown   *string
	)
	if _, err := pgx.ForEachRow(rows, []any{&id, &one.version, &event, &approvedAt, &markdown}, func() error {
		one.at = moment{event: event}
		if event == 0 {
			one.at = moment{at: approvedAt}
		}
		approvals[id] = append(approvals[id], one)
		if markdown != nil {
			extracted, err := specform.ExtractDocument(byID[id].name, one.version, *markdown)
			if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
				unread = append(unread, unreadApproved(byID[id], one.version, err, true))
				return nil
			}
			if err != nil {
				return err
			}
			if held[id] == nil {
				held[id] = map[int]map[string]bool{}
			}
			held[id][one.version] = termKeys(extracted.Terms)
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read the approved versions of %d covered documents: %w", len(ids), err)
	}
	history, err := readParentHistory(ctx, q, moved)
	if err != nil {
		return nil, nil, err
	}
	for index, keys := range contested {
		document := documents[index]
		latest := termKeys(document.terms)
		holds := func(version int, key string) bool {
			if version == document.version {
				return latest[key]
			}
			return held[document.id][version][key]
		}
		var arrived *moment
		if event, ok := history.arrival(document.issue, root, roots); ok {
			arrived = &moment{event: event}
		}
		list := approvals[document.id] // newest first
		for _, key := range keys {
			order := joinOrder{arrived: arrived}
			for at := 0; at < len(list) && holds(list[at].version, key); at++ {
				order.approved = list[at].at
			}
			joins[definitionOf{index, key}] = order
		}
	}
	return joins, unread, nil
}

// termKeys is the set of the keys (specform.TermKey) of terms.
func termKeys(terms []specform.Term) map[string]bool {
	keys := make(map[string]bool, len(terms))
	for _, term := range terms {
		keys[specform.TermKey(term.Name)] = true
	}
	return keys
}

// governingUnder is the definition of each term under root that governs once the person approves
// the call (R50), each with its document: of the term's approved definitions in join order
// (approvedSpec.definitions), the first whose document the call does not name, or names at a
// version that still defines the term, and then that version's definition; and of a term that no
// such definition holds, the first definition of a document of the call, in the order of the call,
// since each of them joins at the approval. So a request that names the document of the listed
// definition and drops the term there is judged against the definition that joins next, and a
// document of the call that adds the term again is refused, naming it. It is made once for the
// call, after every document of it was read.
func (j *specFormJudge) governingUnder(root string, approved approvedSpec) []requestTerm {
	if governing, made := j.governing[root]; made {
		return governing
	}
	named := map[string][]requestTerm{} // the definitions of the call of each key, in its order
	var keys []string
	for _, term := range j.defined[root] {
		key := specform.TermKey(term.Name)
		if _, seen := named[key]; !seen {
			keys = append(keys, key)
		}
		named[key] = append(named[key], term)
	}
	governing := []requestTerm{}
	listed := make(map[string]bool, len(approved.definitions))
	for _, definitions := range approved.definitions {
		key := specform.TermKey(definitions[0].Term)
		listed[key] = true
		if first, ok := firstGoverning(definitions, named[key], j.skip); ok {
			governing = append(governing, first)
		} else if len(named[key]) > 0 {
			governing = append(governing, named[key][0])
		}
	}
	for _, key := range keys {
		if !listed[key] {
			governing = append(governing, named[key][0])
		}
	}
	j.governing[root] = governing
	return governing
}

// firstGoverning is the first of definitions, one term's approved definitions in join order, that
// governs once the person approves the call: one of a document that skip does not name, or the
// definition of the version the call names (named, the call's definitions of the term) of a
// document whose version still defines it. It answers false when the call names each of them at a
// version that holds the term no longer.
func firstGoverning(definitions []approvedTerm, named []requestTerm, skip map[string]bool) (requestTerm, bool) {
	for _, definition := range definitions {
		if !skip[definition.artifactID] {
			return requestTerm{artifactID: definition.artifactID, Term: specform.Term{
				Name: definition.Term, Definition: definition.Definition, Where: listedWhere(definition.Document, definition.Version, definition.Issue),
			}}, true
		}
		if at := slices.IndexFunc(named, func(term requestTerm) bool { return term.artifactID == definition.artifactID }); at >= 0 {
			return named[at], true
		}
	}
	return requestTerm{}, false
}

// lockSpecTermsOfMove takes the lock of the list of terms of the root that a reparent from the
// parent before to the parent after brings the moved issue under, in tx, which holds the move,
// when the move changes the nearest named root above it (lockSpecTerms). It takes the lock before
// the move appends its event, so the event's id orders the arrival among the approvals that the
// list reads (approvedSpecTexts). A move inside one root, or out of every root, takes none. It
// writes nothing: the list reads each arrival from the move's event, which every build writes.
func (f SpecForm) lockSpecTermsOfMove(ctx context.Context, tx pgx.Tx, before, after *string) error {
	if len(f.roots) == 0 {
		return nil
	}
	left, err := f.nearestSpecRoot(ctx, tx, before)
	if err != nil {
		return err
	}
	joined, err := f.nearestSpecRoot(ctx, tx, after)
	if err != nil || joined == "" || joined == left {
		return err
	}
	return lockSpecTerms(ctx, tx, joined)
}

// nearestSpecRoot is the nearest named root at or above parent: parent itself, or the nearest named
// one of its ancestors, "" for none or for no parent. So it is the root of an issue under parent,
// which a move reads (lockSpecTermsOfMove), and the root of parent itself (specScopeOf).
func (f SpecForm) nearestSpecRoot(ctx context.Context, q queryer, parent *string) (string, error) {
	if parent == nil {
		return "", nil
	}
	if slices.Contains(f.roots, *parent) {
		return *parent, nil
	}
	ancestors, err := refs.IssueAncestors(ctx, q, *parent)
	if err != nil {
		return "", fmt.Errorf("read the ancestors of %s: %w", *parent, err)
	}
	for _, ancestor := range ancestors {
		if slices.Contains(f.roots, ancestor) {
			return ancestor, nil
		}
	}
	return "", nil
}
