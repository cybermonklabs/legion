package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
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

// The form of The Spec (R49) and its list of terms (R50) as the server holds them. A setting names
// the roots of The Spec (DISPATCH_SPEC_ROOTS); a document under one of them is covered, and this
// file is the one place that judges its form. The list of a root is the term lines of the latest
// version of each covered document under it that a person approved, its terms.md among them, with
// one definition of each term (approvedSpecTexts), and a covered document that defines a term of
// the list again is refused. Three calls refuse: refuseSpecForm, at the two call sites, the
// opening or hand-back of an approval request (requestArtifactApproval) and a person's approval
// (writeApprovalReviews); and carryBreaksForm, which the carry of an approval to a new version
// asks before it writes that approval in the person's name (docs.FormGate), and which carries
// nothing over a break. A person's approval decides against the list and then changes it, so it
// takes the lock of the root's list before it reads the list (lockSpecTerms), and two approvals
// under one root take turns. A move into a root changes the list too, and takes the same lock
// before it appends its event (lockSpecTermsOfMove), and the list reads that event. Two calls read
// findings and refuse nothing: the read route (getArtifactSpecForm) and the restore of a reopen
// and of the repair pass (restoreIssueAsksTx), which leaves a broken approval request resolved
// rather than open it again. Every route that opens an approval request or answers one reaches
// one of these calls;
// TestOnlyTheFormGateOpensOrAnswersAPersonsApproval holds the code to that census.
//
// One way past the gate stays open, and it is known: an approval given before the setting named
// the root, or while the issue stood outside it, never passed the gate, and a reparent out of a
// root takes an issue's documents out of scope with one write; the read route's scope says so. A
// reparent into a root is not refused either, and it can bring a second definition of a term under
// the root, as an approval from before the setting named the root can. The list still holds one
// definition of that term, the one that joined it first, each definition at its own date
// (approvedSpecTexts), and the read of the other document names it as a finding.

// specFormCode is the code of the refusal.
const specFormCode = "SPEC_FORM"

// specFormFindingsHeld is how many findings a refusal's body holds. findings_total counts them all.
const specFormFindingsHeld = 500

// termsSlug is the slug of a root's terms.md, which holds the list of terms of The Spec.
const termsSlug = "terms-md"

// The scope of a document, which the read route answers: covered and list are in scope, and the
// gate refuses an approval of them; the others say why it does not.
const (
	specScopeCovered = "covered"  // the spec or the Verification document of an issue under a root
	specScopeList    = "list"     // the terms.md of a root
	specScopeNoRoot  = "no_root"  // the setting names no root
	specScopeRoot    = "root"     // a document of a root other than its terms.md
	specScopeOutside = "outside"  // no root holds the issue, or the document has no issue
	specScopeNotSpec = "not_spec" // under a root, but not the spec or the Verification document
)

// actRead is the read route's act: it judges every document it names, in scope or not, and
// refuses nothing.
const actRead specform.Act = "read"

// parseSpecRoots reads DISPATCH_SPEC_ROOTS: issue keys separated by commas, each trimmed, a key
// named twice once. A blank value names no root. An item that is no issue key, an empty one
// among them, refuses startup. Startup reads no database, so a key with no issue covers nothing.
func parseSpecRoots(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var roots []string
	for item := range strings.SplitSeq(raw, ",") {
		key := strings.TrimSpace(item)
		if !issueKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid DISPATCH_SPEC_ROOTS %q (expected issue keys separated by commas)", raw)
		}
		if !slices.Contains(roots, key) {
			roots = append(roots, key)
		}
	}
	return roots, nil
}

// specScope is where a document stands for the form: its scope, and the nearest named root above
// it, nil when no root holds it.
type specScope struct {
	scope string
	root  *string
}

// inScope reports whether the gate refuses an approval of the document when a text breaks the form.
func (s specScope) inScope() bool {
	return s.scope == specScopeCovered || s.scope == specScopeList
}

// specScopeOf is where artifact stands for the form, by the setting: the nearest named root among
// its issue's ancestors (refs.IssueAncestors, parent first) is its root. A root's own documents
// other than its terms.md are not covered, nested in another root or not, and nor is any document
// of an issue under a root other than its spec and its Verification document.
func (s *server) specScopeOf(ctx context.Context, q queryer, artifact model.Artifact) (specScope, error) {
	roots := s.deps.SpecRoots
	if len(roots) == 0 {
		return specScope{scope: specScopeNoRoot}, nil
	}
	if artifact.IssueKey == nil {
		return specScope{scope: specScopeOutside}, nil
	}
	issue := *artifact.IssueKey
	if slices.Contains(roots, issue) {
		if artifact.Kind == "doc" && artifact.Slug == termsSlug {
			return specScope{scope: specScopeList, root: &issue}, nil
		}
		return specScope{scope: specScopeRoot, root: &issue}, nil
	}
	ancestors, err := refs.IssueAncestors(ctx, q, issue)
	if err != nil {
		return specScope{}, fmt.Errorf("read the ancestors of %s: %w", issue, err)
	}
	index := slices.IndexFunc(ancestors, func(ancestor string) bool { return slices.Contains(roots, ancestor) })
	if index < 0 {
		return specScope{scope: specScopeOutside}, nil
	}
	root := ancestors[index]
	if artifact.Kind == "doc" && (isSpecDocument(artifact) || documentPlays(artifact, model.ArtifactRoleVerification)) {
		return specScope{scope: specScopeCovered, root: &root}, nil
	}
	return specScope{scope: specScopeNotSpec, root: &root}, nil
}

// specFormFinding is one finding of a refusal or of the read route: the document and the version
// whose text breaks the form, and the finding of specform.
type specFormFinding struct {
	Document   string `json:"document"`
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
	specform.Finding
}

// specFormError is the 409 SPEC_FORM body of a refused approval request or approval: the
// refusal's words, the findings it holds (at most 500) and the count of all of them. On a person's
// approval, changes_reason is the text a Request changes sends to the author.
type specFormError struct {
	Code          string            `json:"code"`
	Error         string            `json:"error"`
	ChangesReason string            `json:"changes_reason,omitempty"`
	Findings      []specFormFinding `json:"findings"`
	FindingsTotal int               `json:"findings_total"`
}

// specFormTerm is one term of the list of a root: the term, its definition, and the document, the
// version and the issue that define it.
type specFormTerm struct {
	Term       string `json:"term"`
	Definition string `json:"definition"`
	Document   string `json:"document"`
	Version    int    `json:"version"`
	Issue      string `json:"issue"`
}

// specFormSummary is what an approval request's answer says of the form of the documents it
// handed the person, when the document it names is in scope: the root, and the count of each kind
// of text it read. Terms counts the terms of the root's list.
type specFormSummary struct {
	Root  string                `json:"root"`
	Texts map[specform.Kind]int `json:"texts"`
	Terms int                   `json:"terms"`
}

// specFormRead is the body of GET /api/v1/artifacts/{id}/spec-form: one version of a document,
// where it stands for the form, the texts it holds and every finding, whether the gate refuses its
// approval or not.
type specFormRead struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	// InScope is whether the gate refuses an approval of the document when a text breaks the form.
	InScope bool `json:"in_scope"`
	// Scope is covered, list, no_root, root, outside or not_spec.
	Scope string `json:"scope"`
	// Plays is spec or verification, the part the document plays for its issue, or empty.
	Plays string `json:"plays"`
	// Root is the nearest named root above the document, null when none holds it.
	Root *string `json:"root"`
	// Texts counts each kind of text the document holds, so that no finding and no text read are
	// two answers.
	Texts         map[specform.Kind]int `json:"texts"`
	Findings      []specFormFinding     `json:"findings"`
	FindingsTotal int                   `json:"findings_total"`
	// Terms is the list of terms of the root, sorted by term: the term lines of the approved
	// versions of the root's terms.md and of the covered documents under it. It is empty for a
	// document that no root holds.
	Terms []specFormTerm `json:"terms"`
	// RefusesApproval is whether an approval request or an approval of this version is refused.
	RefusesApproval bool `json:"refuses_approval"`
}

// specFormDocument is one document a judgment named: the document, the version judged, where it
// stands, and, once judged, the count of each kind of text, every finding and the list of terms
// of its root.
type specFormDocument struct {
	artifact model.Artifact
	version  int
	specScope
	texts    map[specform.Kind]int
	findings []specFormFinding
	terms    []specFormTerm
}

// specFormResult is the judgment of the documents specFormOf named, in their order.
type specFormResult struct {
	documents []specFormDocument
}

// refusing is every finding of the documents in scope, in the order of the documents.
func (r specFormResult) refusing() []specFormFinding {
	var findings []specFormFinding
	for _, document := range r.documents {
		if document.inScope() {
			findings = append(findings, document.findings...)
		}
	}
	return findings
}

// refusal is the refusal of act for the findings of the documents in scope, nil when there are
// none.
func (r specFormResult) refusal(act specform.Act) *specFormRefusal {
	findings := r.refusing()
	if len(findings) == 0 {
		return nil
	}
	refusal := &specFormRefusal{act: act, findings: findings}
	for _, document := range r.documents {
		if document.artifact.ID == findings[0].ArtifactID && document.artifact.IssueKey != nil {
			refusal.more = fmt.Sprintf("dispatch spec-form --issue %s --artifact %s lists them all.", *document.artifact.IssueKey, document.artifact.Name)
			break
		}
	}
	return refusal
}

// summary is the answer's spec_form for a request that named artifactID, nil when that document
// is not in scope.
func (r specFormResult) summary(artifactID string) *specFormSummary {
	var summary *specFormSummary
	for _, document := range r.documents {
		if document.artifact.ID == artifactID && document.inScope() {
			summary = &specFormSummary{Root: *document.root, Texts: map[specform.Kind]int{}, Terms: len(document.terms)}
		}
	}
	if summary == nil {
		return nil
	}
	for _, document := range r.documents {
		if document.inScope() {
			for kind, count := range document.texts {
				summary.Texts[kind] += count
			}
		}
	}
	return summary
}

// specFormRefusal is the error refuseSpecForm answers: writeHandlerError writes its body whole,
// at 409.
type specFormRefusal struct {
	act      specform.Act
	findings []specFormFinding
	// more is the command that lists every finding, which the refusal names after the first 20.
	more string
}

func (r *specFormRefusal) plain() []specform.Finding {
	plain := make([]specform.Finding, len(r.findings))
	for index, finding := range r.findings {
		plain[index] = finding.Finding
	}
	return plain
}

// Error is the refusal's words (specform.Message).
func (r *specFormRefusal) Error() string {
	return specform.Message(r.act, r.plain(), len(r.findings), r.more)
}

// body is the refusal's 409 SPEC_FORM body.
func (r *specFormRefusal) body() specFormError {
	body := specFormError{
		Code:          specFormCode,
		Error:         r.Error(),
		Findings:      r.findings[:min(len(r.findings), specFormFindingsHeld)],
		FindingsTotal: len(r.findings),
	}
	if r.act == specform.ActApproval {
		body.ChangesReason = specform.ChangesReason(r.plain(), len(r.findings), r.more)
	}
	return body
}

// specFormOf judges the form of documents, each at the version it names, in q. A document in
// scope is judged on every act; the read route's act (actRead) judges every document, in scope or
// not. With no root named, the other acts read nothing at all. It reads each version's stored
// markdown, the projects (a bare key of a project is a link), the emails of the people who signed
// in (a piece of one is a name) and, under a root, the approved covered documents under it
// (approvedSpecTexts): the names of their components and interfaces, which are nouns of the
// system and no person, and their terms, which are the root's list of terms (R50). It reads every
// document before it judges one, so that a line of the term-line form outside Terms is judged
// against the definitions of every document of the call, whatever their order. A person's
// approval (specform.ActApproval) judges in the transaction that writes it, and it takes the lock
// of each root's list before it reads that list (lockSpecTerms).
func (s *server) specFormOf(ctx context.Context, q queryer, documents []model.AskApprovalDocument, act specform.Act) (specFormResult, error) {
	var result specFormResult
	if len(s.deps.SpecRoots) == 0 && act != actRead {
		return result, nil
	}
	judge := &specFormJudge{
		server: s, q: q, lock: act == specform.ActApproval,
		skip: map[string]bool{}, approved: map[string]approvedSpec{},
		defined: map[string][]requestTerm{}, proposed: map[string][]specform.Term{},
	}
	for _, document := range documents {
		judge.skip[document.ArtifactID] = true
	}
	var read []*specform.Extracted // each document's texts, nil for a document that is not judged
	for _, named := range documents {
		artifact, err := artifactMetadata(ctx, q, named.ArtifactID)
		if err != nil {
			return specFormResult{}, err
		}
		scope, err := s.specScopeOf(ctx, q, artifact)
		if err != nil {
			return specFormResult{}, err
		}
		document := specFormDocument{artifact: artifact, version: named.Version, specScope: scope}
		var extracted *specform.Extracted
		if scope.inScope() || act == actRead {
			texts, err := judge.read(ctx, document)
			if err != nil {
				return specFormResult{}, err
			}
			extracted = &texts
		}
		result.documents = append(result.documents, document)
		read = append(read, extracted)
	}
	for index, extracted := range read {
		if extracted != nil {
			if err := judge.judge(ctx, &result.documents[index], *extracted); err != nil {
				return specFormResult{}, err
			}
		}
	}
	return result, nil
}

// refuseSpecForm judges documents (specFormOf) and answers the judgment, with a *specFormRefusal
// of act when a text of a document in scope breaks the form. It has two call sites: the opening or
// hand-back of an approval request, and a person's approval.
func (s *server) refuseSpecForm(ctx context.Context, tx pgx.Tx, documents []model.AskApprovalDocument, act specform.Act) (specFormResult, error) {
	result, err := s.specFormOf(ctx, tx, documents, act)
	if err != nil {
		return specFormResult{}, err
	}
	if refusal := result.refusal(act); refusal != nil {
		return result, refusal
	}
	return result, nil
}

// carryBreaksForm is the form gate on the version that a carry of an approval would approve in
// the person's name (docs.FormGate): whether a text of that version of artifactID breaks the form,
// judged as a person's approval is (specFormOf). A document no root covers reads nothing more.
func (s *server) carryBreaksForm(ctx context.Context, tx pgx.Tx, artifactID string, version int) (bool, error) {
	result, err := s.specFormOf(ctx, tx, []model.AskApprovalDocument{{ArtifactID: artifactID, Version: version}}, specform.ActApproval)
	if err != nil {
		return false, err
	}
	return result.refusal(specform.ActApproval) != nil, nil
}

// FormGate is the gate of the carry (carryBreaksForm) for a document service that a process
// builds before its API: the roots that specRoots names, as DISPATCH_SPEC_ROOTS writes them, and
// the server's address. NewDeps sets the same gate on the service it is given.
func FormGate(specRoots, serverURL string) (docs.FormGate, error) {
	roots, err := parseSpecRoots(specRoots)
	if err != nil {
		return nil, err
	}
	return (&server{deps: Deps{SpecRoots: roots, ServerURL: strings.TrimSuffix(serverURL, "/")}}).carryBreaksForm, nil
}

// specFormJudge judges the documents of one specFormOf call, reading what every document shares
// once.
type specFormJudge struct {
	server *server
	q      queryer
	// lock is whether the judgment takes the lock of each root's list before it reads the list:
	// a person's approval does, because it writes in q what it decides against the list.
	lock bool
	// skip is every document the call names: the version it names stands in for an approved one.
	skip map[string]bool
	// base is the context every document shares, read at the first judgment.
	base *specform.Context
	// approved is what the approved covered documents under each root hold, read at the first
	// judgment under it.
	approved map[string]approvedSpec
	// defined is the definitions under Terms of every document of the call, under each root, each
	// with its document: a line of the term-line form outside Terms that names one of another
	// document is refused, whichever document comes first.
	defined map[string][]requestTerm
	// proposed is the terms that the documents judged so far define, under each root: one call
	// holds one definition of a term, so a later document of the call that defines it again is
	// refused.
	proposed map[string][]specform.Term
}

// requestTerm is a definition under Terms of a document of one call, with the document.
type requestTerm struct {
	artifactID string
	specform.Term
}

// read reads document's named version and its texts, and under a root keeps each definition
// under Terms that it holds (defined), with its place as the list names one.
func (j *specFormJudge) read(ctx context.Context, document specFormDocument) (specform.Extracted, error) {
	markdown, err := specFormMarkdown(ctx, j.q, document.artifact, document.version)
	if err != nil {
		return specform.Extracted{}, err
	}
	extracted, err := specform.ExtractDocument(document.artifact.Name, document.version, markdown)
	if err != nil {
		return specform.Extracted{}, err
	}
	if document.root != nil {
		where := listedWhere(document.artifact.Name, document.version, *document.artifact.IssueKey)
		for _, term := range extracted.Terms {
			term.Where = where
			j.defined[*document.root] = append(j.defined[*document.root], requestTerm{artifactID: document.artifact.ID, Term: term})
		}
	}
	return extracted, nil
}

// judge judges the texts that read gave of document, each definition of a term among them, and
// under a root each definition against the root's list (R50): the approved terms of the documents
// the call does not name, and the definitions under Terms of the documents it judged before this
// one, since one request holds one definition of each term. A line of the term-line form outside
// Terms is no definition: it is judged against the list and against the definitions under Terms
// of every document of the call (defined), and it adds nothing.
func (j *specFormJudge) judge(ctx context.Context, document *specFormDocument, extracted specform.Extracted) error {
	context, err := j.baseContext(ctx)
	if err != nil {
		return err
	}
	document.terms = []specFormTerm{}
	var list, defined []specform.Term
	if document.root != nil {
		approved, err := j.approvedUnder(ctx, *document.root)
		if err != nil {
			return err
		}
		context.SystemNames = approved.systemNames(j.skip)
		list = append(approved.list(j.skip), j.proposed[*document.root]...)
		for _, other := range j.defined[*document.root] {
			if other.artifactID != document.artifact.ID {
				defined = append(defined, other.Term)
			}
		}
		document.terms = approved.read()
	}
	document.texts = extracted.Counts
	document.findings = []specFormFinding{}
	for _, finding := range specform.Check(context, specform.Input{Texts: extracted.Texts, Proposed: extracted.Terms, List: list, Defined: defined}) {
		document.findings = append(document.findings, specFormFinding{
			Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version, Finding: finding,
		})
	}
	if document.root != nil {
		where := listedWhere(document.artifact.Name, document.version, *document.artifact.IssueKey)
		for _, term := range extracted.Terms {
			term.Where = where
			j.proposed[*document.root] = append(j.proposed[*document.root], term)
		}
	}
	return nil
}

// baseContext is what Check knows of the system for every document: the project keys, the
// emails of the people who signed in, and the server's address.
func (j *specFormJudge) baseContext(ctx context.Context) (specform.Context, error) {
	if j.base == nil {
		projects, err := specFormStrings(ctx, j.q, `select key from projects`)
		if err != nil {
			return specform.Context{}, fmt.Errorf("read the project keys: %w", err)
		}
		people, err := specFormStrings(ctx, j.q, `select email from people`)
		if err != nil {
			return specform.Context{}, fmt.Errorf("read the people: %w", err)
		}
		j.base = &specform.Context{
			IsProjectKey: func(key string) bool { return slices.Contains(projects, key) },
			PersonNames:  people,
			ServerURL:    j.server.deps.ServerURL,
		}
	}
	return *j.base, nil
}

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
	approved, err := j.server.approvedSpecTexts(ctx, j.q, root)
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

// approvedSpec is what the approved covered documents under a root hold that a judgment of
// another document reads: the names of the components and interfaces they define, which are
// nouns of the system, and the terms they define, which are the root's list of terms, one
// definition of each term, sorted by term. Each keeps the document that holds it, because a
// judgment leaves out the documents it names: their named versions stand in for their approved
// ones.
type approvedSpec struct {
	names []approvedName
	terms []approvedTerm
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

// list is the root's list of terms as Check compares a definition with it, less the terms of the
// documents skip names. The list holds one definition of each term before those are left out, so
// a second definition that it does not hold does not come back when skip names the document of
// the first. Each term's Where is "<document> version <n> of <issue key>".
func (a approvedSpec) list(skip map[string]bool) []specform.Term {
	var list []specform.Term
	for _, term := range a.terms {
		if !skip[term.artifactID] {
			list = append(list, specform.Term{Name: term.Term, Definition: term.Definition, Where: listedWhere(term.Document, term.Version, term.Issue)})
		}
	}
	return list
}

// read is the root's whole list of terms, as the read route and an approval request's answer
// show it.
func (a approvedSpec) read() []specFormTerm {
	read := make([]specFormTerm, len(a.terms))
	for index, term := range a.terms {
		read[index] = term.specFormTerm
	}
	return read
}

// listedWhere is where a term of the list stands: "<document> version <n> of <issue key>".
func listedWhere(document string, version int, issue string) string {
	return fmt.Sprintf("%s version %d of %s", document, version, issue)
}

// approvedSpecTexts reads the latest approved version of each covered document under root: the
// root's terms.md, and the spec and the Verification document of each issue whose nearest named
// root is root (refs.IssueDescendants, less each named root below root and its own subtree). Only
// a person approves a covered document: a plan review approves a plan document alone
// (createArtifactPlanReview), whose role its slug gives (documentRole). So an agent's version adds
// nothing to the list until the person approves it.
//
// The list holds one definition of each term (R50), by specform.TermKey. A term that one document
// alone defines, or that the root's terms.md defines, has that definition. Of the definitions of a
// term in several other documents, the list holds the one that joined it first, and each
// definition has its own date (definitionJoins): the later of the move that last brought its
// issue under the root and the first approval of the earliest approved version of its document
// from which each later approved version holds the term. So a term that a later version added
// joins at the approval of that version, and a definition that a later version changed keeps its
// date. The move is read from the events (readParentHistory, parentHistory.arrival), which every
// build of Dispatch writes for every move, so the build that handled a move changes no order, and
// a rollback changes none. So a definition that the list holds is never displaced by an arrival,
// and a change of a listed definition waits for the person's approval. Two definitions that joined
// at one move come in the order of their approvals, then in the order in which Dispatch created
// their documents. A reparent into the root, or an approval from before the setting named the
// root, can put a second definition under the root, and the read of its document names the
// definition that the list holds as a finding. When the document that the list keeps a term from
// leaves the root, the list holds the definition that joined next: the person approved each, and a
// move changes the tree and no text. The setting decides which issues are roots at every event: an
// issue named today is a root for the moves before it was named too.
func (s *server) approvedSpecTexts(ctx context.Context, q queryer, root string) (approvedSpec, error) {
	issues, err := refs.IssueDescendants(ctx, q, root)
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the issues under %s: %w", root, err)
	}
	for _, inner := range s.deps.SpecRoots {
		if inner == root || !slices.Contains(issues, inner) {
			continue
		}
		below, err := refs.IssueDescendants(ctx, q, inner)
		if err != nil {
			return approvedSpec{}, fmt.Errorf("read the issues under %s: %w", inner, err)
		}
		issues = slices.DeleteFunc(issues, func(issue string) bool { return issue == inner || slices.Contains(below, issue) })
	}
	rows, err := q.Query(ctx, `
		select `+artifactColumns+` from artifacts
		where kind = 'doc' and (
			(issue_key = any($1::text[]) and (is_primary or `+documentRolePredicate("", model.ArtifactRoleVerification)+`))
			or (issue_key = $2 and slug = $3)
		)
		order by issue_key, id
	`, issues, root, termsSlug)
	if err != nil {
		return approvedSpec{}, fmt.Errorf("list the covered documents under %s: %w", root, err)
	}
	covered, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) { return scanArtifact(row) })
	if err != nil {
		return approvedSpec{}, fmt.Errorf("list the covered documents under %s: %w", root, err)
	}
	var (
		approved  approvedSpec
		documents []approvedSpecDocument
	)
	for _, artifact := range covered {
		review, err := docs.LatestApprovedReview(ctx, q, artifact.ID)
		if err != nil {
			return approvedSpec{}, err
		}
		if review == nil {
			continue
		}
		markdown, err := specFormMarkdown(ctx, q, artifact, review.Version)
		if err != nil {
			return approvedSpec{}, err
		}
		extracted, err := specform.ExtractDocument(artifact.Name, review.Version, markdown)
		if err != nil {
			return approvedSpec{}, err
		}
		for _, text := range extracted.Texts {
			if text.Kind == specform.KindName {
				approved.names = append(approved.names, approvedName{artifactID: artifact.ID, name: text.Body})
			}
		}
		documents = append(documents, approvedSpecDocument{artifact: artifact, version: review.Version, terms: extracted.Terms})
	}
	isTermsMd := func(document int) bool {
		return *documents[document].artifact.IssueKey == root && documents[document].artifact.Slug == termsSlug
	}
	// The definitions of each key, one of each document, and each key in the order it was read.
	type definer struct {
		document int
		term     specform.Term
	}
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
	contested := map[int][]string{}
	for _, key := range keys {
		if len(definers[key]) > 1 && !slices.ContainsFunc(definers[key], func(d definer) bool { return isTermsMd(d.document) }) {
			for _, d := range definers[key] {
				contested[d.document] = append(contested[d.document], key)
			}
		}
	}
	joins, err := definitionJoins(ctx, q, root, s.deps.SpecRoots, documents, contested)
	if err != nil {
		return approvedSpec{}, err
	}
	for _, key := range keys {
		keeper := slices.MinFunc(definers[key], func(a, b definer) int {
			if aTerms, bTerms := isTermsMd(a.document), isTermsMd(b.document); aTerms != bTerms {
				if aTerms {
					return -1
				}
				return 1
			}
			aJoin, bJoin := joins[definitionOf{a.document, key}], joins[definitionOf{b.document, key}]
			aArtifact, bArtifact := documents[a.document].artifact, documents[b.document].artifact
			return cmp.Or(aJoin.joined().compare(bJoin.joined()), aJoin.approved.compare(bJoin.approved),
				aArtifact.CreatedAt.Compare(bArtifact.CreatedAt), strings.Compare(aArtifact.ID, bArtifact.ID))
		})
		document := documents[keeper.document]
		approved.terms = append(approved.terms, approvedTerm{artifactID: document.artifact.ID, specFormTerm: specFormTerm{
			Term: keeper.term.Name, Definition: keeper.term.Definition, Document: document.artifact.Name, Version: document.version, Issue: *document.artifact.IssueKey,
		}})
	}
	slices.SortStableFunc(approved.terms, func(a, b approvedTerm) int {
		return strings.Compare(strings.ToLower(a.Term), strings.ToLower(b.Term))
	})
	return approved, nil
}

// approvedSpecDocument is a covered document with an approved version: the document, its latest
// approved version, and the term lines of that version.
type approvedSpecDocument struct {
	artifact model.Artifact
	version  int
	terms    []specform.Term
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

// definitionJoinsQuery reads each approval of each document that $1 names, in the order the
// approvals' events committed (docs.ReviewOrder, oldest first): its version, its event (events.id,
// 0 for an approval from before reviews kept their event, migration 0120) and its time. The first
// approval of each version other than the one $2 names for its document also carries that
// version's markdown, so one query reads every older approved version.
const definitionJoinsQuery = `
	select r.artifact_id::text, r.version, coalesce(e.id, 0), r.created_at,
		case when r.version <> latest.version and row_number() over (
			partition by r.artifact_id, r.version order by r.event_seq nulls first, r.created_at, r.id
		) = 1 then coalesce(v.markdown, '') end
	from artifact_reviews r
	join unnest($1::uuid[], $2::int[]) as latest(id, version) on latest.id = r.artifact_id
	join artifacts a on a.id = r.artifact_id
	left join events e on e.issue_key = a.issue_key and e.seq = r.event_seq
	left join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.version
	where r.state = 'approved'
	order by r.artifact_id, r.event_seq nulls first, r.created_at, r.id
`

// definitionJoins is when each definition that contested names joined the list of root: for each
// document, by its index among documents, the keys of its terms that another document defines too
// (approvedSpecTexts). It reads the approvals of those documents and their older approved versions
// in one query (definitionJoinsQuery), and the moves of their issues (readParentHistory), and
// nothing when contested is empty. A definition joins at the first approval of the earliest
// approved version from which each later approved version holds its term, or at the arrival of its
// issue under root (parentHistory.arrival), when that came later.
func definitionJoins(ctx context.Context, q queryer, root string, roots []string, documents []approvedSpecDocument, contested map[int][]string) (map[definitionOf]joinOrder, error) {
	joins := map[definitionOf]joinOrder{}
	if len(contested) == 0 {
		return joins, nil
	}
	var (
		ids      []string
		versions []int
		moved    []string
	)
	for index := range contested {
		document := documents[index]
		ids = append(ids, document.artifact.ID)
		versions = append(versions, document.version)
		if issue := *document.artifact.IssueKey; issue != root && !slices.Contains(moved, issue) {
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
		return nil, fmt.Errorf("read the approved versions of %d covered documents: %w", len(ids), err)
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
			extracted, err := specform.ExtractDocument("", one.version, *markdown)
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
		return nil, fmt.Errorf("read the approved versions of %d covered documents: %w", len(ids), err)
	}
	history, err := readParentHistory(ctx, q, moved)
	if err != nil {
		return nil, err
	}
	for index, keys := range contested {
		document := documents[index]
		latest := termKeys(document.terms)
		holds := func(version int, key string) bool {
			if version == document.version {
				return latest[key]
			}
			return held[document.artifact.ID][version][key]
		}
		var arrived *moment
		if event, ok := history.arrival(*document.artifact.IssueKey, root, roots); ok {
			arrived = &moment{event: event}
		}
		list := approvals[document.artifact.ID]
		for _, key := range keys {
			order := joinOrder{arrived: arrived}
			for at := len(list) - 1; at >= 0 && holds(list[at].version, key); at-- {
				order.approved = list[at].at
			}
			joins[definitionOf{index, key}] = order
		}
	}
	return joins, nil
}

// termKeys is the set of the keys (specform.TermKey) of terms.
func termKeys(terms []specform.Term) map[string]bool {
	keys := make(map[string]bool, len(terms))
	for _, term := range terms {
		keys[specform.TermKey(term.Name)] = true
	}
	return keys
}

// parentChange is one write of an issue's parent that the issue's events hold: the id of the
// event, and the parent the write left ("" for none).
type parentChange struct {
	event  int64
	parent string
}

// parentHistory is the parents of the issues it holds as their events wrote them, each issue's
// changes in the order of its events: the parent it was made with, then each parent a write set.
type parentHistory map[string][]parentChange

// parentHistoryQuery reads each change of the parent of each issue that $1 names from its
// issue.created, issue.updated and issue.closed events. Each of them holds the issue as the write
// left it, its parent among the fields, in every release that writes them, so the events are the
// one record of a move whichever build made it. Every write of parent_key appends one of them
// (issue_create.go, issue_patch.go), and the parent a write left alone is no change.
const parentHistoryQuery = `
	select issue_key, id, coalesce(parent, '') from (
		select issue_key, id, payload->>'parent' as parent,
			lag(payload->>'parent') over (partition by issue_key order by id) as previous,
			row_number() over (partition by issue_key order by id) as n
		from events
		where issue_key = any($1::text[]) and type in ('issue.created', 'issue.updated', 'issue.closed')
	) written
	where n = 1 or parent is distinct from previous
	order by issue_key, id
`

// readParentHistory reads the parent history of each issue keys names, and of each issue that was
// ever a parent of one of them, and so on up, so that the parents of every ancestor an issue had at
// any event are known: one query for each step up. An issue with no such event, which only a
// direct write to the database can make, has no parent in the history, so no path to a root
// passes it, and a definition under it joins the list at its approval.
func readParentHistory(ctx context.Context, q queryer, keys []string) (parentHistory, error) {
	history := parentHistory{}
	for len(keys) > 0 {
		rows, err := q.Query(ctx, parentHistoryQuery, keys)
		if err != nil {
			return nil, fmt.Errorf("read the parents of %d issues from their events: %w", len(keys), err)
		}
		var (
			issue  string
			change parentChange
		)
		if _, err := pgx.ForEachRow(rows, []any{&issue, &change.event, &change.parent}, func() error {
			history[issue] = append(history[issue], change)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the parents of %d issues from their events: %w", len(keys), err)
		}
		for _, key := range keys {
			if _, read := history[key]; !read {
				history[key] = nil // no event: read, so no later step asks again
			}
		}
		var next []string
		for _, key := range keys {
			for _, change := range history[key] {
				if _, read := history[change.parent]; change.parent != "" && !read && !slices.Contains(next, change.parent) {
					next = append(next, change.parent)
				}
			}
		}
		keys = next
	}
	return history, nil
}

// parentAt is the parent of issue once the events up to event were written: the parent of its
// last change at or before event, and before its first change the parent it was made with.
func (h parentHistory) parentAt(issue string, event int64) string {
	changes := h[issue]
	if len(changes) == 0 {
		return ""
	}
	parent := changes[0].parent
	for _, change := range changes {
		if change.event > event {
			break
		}
		parent = change.parent
	}
	return parent
}

// lastChange is the id of the last event at or before event that changed the parent of issue, 0
// for none.
func (h parentHistory) lastChange(issue string, event int64) int64 {
	var last int64
	for _, change := range h[issue] {
		if change.event <= event {
			last = change.event
		}
	}
	return last
}

// pathTo is issue and each issue above it, up to the first named root of roots, once the events
// up to event were written, and whether that root is root. It climbs at most parentDepthCap
// links, as every walk of the issue tree does.
func (h parentHistory) pathTo(issue, root string, event int64, roots []string) ([]string, bool) {
	path := []string{issue}
	for parent := h.parentAt(issue, event); parent != ""; parent = h.parentAt(parent, event) {
		if slices.Contains(roots, parent) {
			return path, parent == root
		}
		if len(path) > parentDepthCap {
			break
		}
		path = append(path, parent)
	}
	return path, false
}

// arrival is the event at which issue last came under root, by the parents as the events left
// them at each move: the last event after which the nearest named root of roots above issue was
// root and before which it was not. It walks back from the present: the last change of a parent
// on the path from issue to root is an arrival when the path before it led to no root or to
// another one, and is a move inside the root when it led to root too, which it then walks past.
// A move of root itself, or of an issue above it, changes no path to it. An issue that has been
// under root since it was made, and one whose events put it under no root now, has no arrival.
func (h parentHistory) arrival(issue, root string, roots []string) (int64, bool) {
	for event := int64(math.MaxInt64); ; {
		path, under := h.pathTo(issue, root, event, roots)
		if !under {
			return 0, false
		}
		var last int64
		for _, key := range path {
			last = max(last, h.lastChange(key, event))
		}
		if last == 0 {
			return 0, false
		}
		if _, under := h.pathTo(issue, root, last-1, roots); !under {
			return last, true
		}
		event = last - 1
	}
}

// lockSpecTermsOfMove takes the lock of the list of terms of the root that a reparent from the
// parent before to the parent after brings the moved issue under, in tx, which holds the move,
// when the move changes the nearest named root above it (lockSpecTerms). It takes the lock before
// the move appends its event, so the event's id orders the arrival among the approvals that the
// list reads (approvedSpecTexts). A move inside one root, or out of every root, takes none. It
// writes nothing: the list reads each arrival from the move's event, which every build writes.
func (s *server) lockSpecTermsOfMove(ctx context.Context, tx pgx.Tx, before, after *string) error {
	if len(s.deps.SpecRoots) == 0 {
		return nil
	}
	left, err := s.nearestSpecRoot(ctx, tx, before)
	if err != nil {
		return err
	}
	joined, err := s.nearestSpecRoot(ctx, tx, after)
	if err != nil || joined == "" || joined == left {
		return err
	}
	return lockSpecTerms(ctx, tx, joined)
}

// nearestSpecRoot is the named root that an issue under parent has as its nearest named ancestor:
// parent itself, or the nearest named one of its ancestors, "" for none.
func (s *server) nearestSpecRoot(ctx context.Context, q queryer, parent *string) (string, error) {
	if parent == nil {
		return "", nil
	}
	if slices.Contains(s.deps.SpecRoots, *parent) {
		return *parent, nil
	}
	ancestors, err := refs.IssueAncestors(ctx, q, *parent)
	if err != nil {
		return "", fmt.Errorf("read the ancestors of %s: %w", *parent, err)
	}
	for _, ancestor := range ancestors {
		if slices.Contains(s.deps.SpecRoots, ancestor) {
			return ancestor, nil
		}
	}
	return "", nil
}

// specFormMarkdown is the stored markdown of version of artifact. A version the document does not
// have is 404 VERSION_NOT_FOUND.
func specFormMarkdown(ctx context.Context, q queryer, artifact model.Artifact, version int) (string, error) {
	var markdown string
	err := q.QueryRow(ctx, `select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2`, artifact.ID, version).Scan(&markdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errorf(http.StatusNotFound, "VERSION_NOT_FOUND", "%s has no version %d", artifact.Name, version)
	}
	if err != nil {
		return "", fmt.Errorf("read version %d of %s: %w", version, artifact.Name, err)
	}
	return markdown, nil
}

// specFormStrings is the one text column query selects with args, row by row.
func specFormStrings(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
