package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The form of The Spec (R49) and its list of terms (R50) as the server holds them. A setting names
// the roots of The Spec (DISPATCH_SPEC_ROOTS); a document under one of them is covered, and this
// file is the one place that judges its form. A process builds its form once (NewSpecForm), which
// sets the gate of its document service, and the API judges with that same form (Deps.SpecForm).
// The list of a root is the term lines of the latest version of each covered document under it
// that a person approved, its terms.md among them, with one definition of each term
// (approvedSpecTexts), and a covered document that defines a term of the list again is refused.
// refuseSpecForm refuses, at its two call sites: the opening or hand-back of an approval request
// (requestArtifactApproval), and an approval in a person's name (approvalDocuments), which gives
// the documents that approval covers and judges each of them. Every writer of an approval in a
// person's name asks approvalDocuments: her review on either review route and her answer to an
// approval request (queueApprovalReviews), and the carry of an approval to a new version
// (carryBreaksForm, docs.FormGate), which carries nothing over a break. A person's approval
// decides against the list and then changes it, so it takes the lock of the root's list before it
// reads the list (lockSpecTerms), and two approvals under one root take turns. A move into a root
// changes the list too, and takes the same lock before it appends its event
// (lockSpecTermsOfMove), and the list reads that event. Two calls read findings and refuse
// nothing: the read route (getArtifactSpecForm) and the restore of a reopen and of the repair pass
// (prepareRestoration), which judges a request at the versions it names once the reopen ends and
// leaves a broken approval request resolved rather than open it again (docs.WriteRestoredAsks,
// which only it calls, writes the others open). Every route that opens an approval request or
// answers one reaches one of these calls; TestOnlyTheFormGateOpensOrAnswersAPersonsApproval holds
// the code to that census, and to NewSpecForm as the one setter of the gate.
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

// SpecForm is the form of The Spec that a process judges with: the roots that DISPATCH_SPEC_ROOTS
// names, and the server's origins, against which a judgment reads links (baseContext). The zero
// SpecForm names no root, so only the read route judges anything with it.
type SpecForm struct {
	roots   []string
	origins text.Origins
}

// NewSpecForm builds the form of The Spec from specRoots, as DISPATCH_SPEC_ROOTS writes them, and
// the server's origins (text.ServerOrigins), and sets its gate on documents (carryBreaksForm):
// each carry of an approval that service writes is then judged as the person's own Approve is. It
// is the one setter of a document service's gate. The server builds its form once, before the
// boot's restoration schedules a settlement, and builds its API with it; each backfill command
// builds one for the service it writes through. A value that is no list of issue keys refuses
// startup.
func NewSpecForm(specRoots string, origins text.Origins, documents docs.API) (SpecForm, error) {
	roots, err := parseSpecRoots(specRoots)
	if err != nil {
		return SpecForm{}, err
	}
	form := SpecForm{roots: roots, origins: origins}
	documents.SetFormGate(form.carryBreaksForm)
	return form, nil
}

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

// specScopeOf is where artifact stands for the form, by the setting: the nearest named root at or
// above its issue (nearestSpecRoot) is its root. A root's own documents other than its terms.md
// are not covered, nested in another root or not, and nor is any document of an issue under a root
// other than its spec and its Verification document.
func (f SpecForm) specScopeOf(ctx context.Context, q queryer, artifact model.Artifact) (specScope, error) {
	if len(f.roots) == 0 {
		return specScope{scope: specScopeNoRoot}, nil
	}
	if artifact.IssueKey == nil {
		return specScope{scope: specScopeOutside}, nil
	}
	issue := *artifact.IssueKey
	root, err := f.nearestSpecRoot(ctx, q, &issue)
	switch {
	case err != nil:
		return specScope{}, err
	case root == "":
		return specScope{scope: specScopeOutside}, nil
	case root == issue && artifact.Kind == "doc" && artifact.Slug == termsSlug:
		return specScope{scope: specScopeList, root: &root}, nil
	case root == issue:
		return specScope{scope: specScopeRoot, root: &root}, nil
	case artifact.Kind == "doc" && (isSpecDocument(artifact) || documentPlays(artifact, model.ArtifactRoleVerification)):
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
// refusal's words, the findings it holds (at most 500) and the count of all of them. Error is the
// whole refusal, and Refusal and NextStep are its two parts (specform.Words), so that a client
// with a next step of its own shows Refusal beside it. On a person's approval, changes_reason is
// the text a Request changes sends to the author.
type specFormError struct {
	Code          string            `json:"code"`
	Error         string            `json:"error"`
	Refusal       string            `json:"refusal"`
	NextStep      string            `json:"next_step"`
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

// specFormUnread is an approved version of a document under the root that Dispatch cannot read
// (specform.UnreadableError), so no rule compares its texts. cause is why the parser refuses it.
// The latest approved version of a document is a source of the list, so the list holds none of its
// terms: it refuses nothing of a document with no definition, and line says so, and a document that
// holds a definition is refused for it (specform.UnreadListFinding), since no rule can compare the
// definition with its terms, unless the judgment names its document (refusingDefinitions). An older
// approved version (orders) is no source of the list: it only orders the definitions of its
// document, as a version that holds no term (definitionJoins), so it refuses nothing, and line
// says that (specform.UnreadOlderLine).
type specFormUnread struct {
	Document   string `json:"document"`
	ArtifactID string `json:"artifact_id"`
	Issue      string `json:"issue"`
	Version    int    `json:"version"`
	Line       string `json:"line"`
	cause      string
	orders     bool
}

// specFormList is the list of terms of a root as each answer gives it (R50), one value with the
// record of what Dispatch could not read as it made the list. The list can lack the terms of each
// latest approved version in Unread, so no answer gives Terms without Unread whole, the versions of
// the documents that the answer judges among them. Under a root approvedSpec.read makes this
// value, and with no root both are empty.
type specFormList struct {
	// Terms is the list of terms of the root, sorted by term: the term lines of the approved
	// versions of the root's terms.md and of the covered documents under it. It is empty for a
	// document that no root holds.
	Terms []specFormTerm `json:"terms"`
	// Unread is each approved version under the root that Dispatch could not read as it made
	// Terms, the versions of the documents that the answer judges among them.
	Unread []specFormUnread `json:"unread"`
}

// specFormSummary is what an approval request's answer says of the form of the documents it
// handed the person, when the document it names is in scope: the root, the count of each kind of
// text it read, the count of the terms of the root's list, and the record of what Dispatch could
// not read as it made that list (specFormList.Unread).
type specFormSummary struct {
	Root   string                `json:"root"`
	Texts  map[specform.Kind]int `json:"texts"`
	Terms  int                   `json:"terms"`
	Unread []specFormUnread      `json:"unread"`
}

// specFormRead is the body of GET /api/v1/artifacts/{id}/spec-form: one version of a document,
// where it stands for the form, the texts it holds and its findings, at most specFormFindingsHeld
// of them and the count of all of them, whether the gate refuses its approval or not.
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
	// FindingLines are the lines that list Findings, which `dispatch spec-form` prints as they
	// are (specform.Listing): when RefusesApproval, a head that counts the texts that break the
	// form; then the line of each finding that a refusal writes for it; then, when Findings holds
	// fewer than FindingsTotal, a line that says how many it lists. Empty with no finding.
	FindingLines []string `json:"finding_lines"`
	// specFormList is the list of terms of the root and the record of what Dispatch could not read
	// as it made the list, as one value.
	specFormList
	// RefusesApproval is whether an approval request or an approval of this version is refused.
	RefusesApproval bool `json:"refuses_approval"`
}

// specFormDocument is one document a judgment named: the document, the version judged, where it
// stands, and, once judged, the count of each kind of text, every finding, and the list of terms
// of its root with its record of what Dispatch could not read (specFormList). A named version
// that the parser refuses has its one finding in unreadable once read.
type specFormDocument struct {
	artifact model.Artifact
	version  int
	specScope
	texts      map[specform.Kind]int
	findings   []specFormFinding
	list       specFormList
	unreadable *specFormFinding
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

// unread is the record of what Dispatch could not read as it made the list of each document in
// scope (specFormList.Unread), each version once: the documents of one root share one record, so
// a version is the same entry in each.
func (r specFormResult) unread() []specFormUnread {
	type version struct {
		artifactID string
		version    int
		orders     bool
	}
	var lists [][]specFormUnread
	total := 0
	for _, document := range r.documents {
		if document.inScope() {
			lists = append(lists, document.list.Unread)
			total += len(document.list.Unread)
		}
	}
	unread := make([]specFormUnread, 0, total) // an answer with no unread version answers [], not null
	kept := make(map[version]bool, total)
	for _, list := range lists {
		for _, each := range list {
			if key := (version{each.ArtifactID, each.Version, each.orders}); !kept[key] {
				kept[key] = true
				unread = append(unread, each)
			}
		}
	}
	return unread
}

// refusal is the refusal of act for the findings of the documents in scope, nil when there are
// none.
func (r specFormResult) refusal(act specform.Act) *specFormRefusal {
	findings := r.refusing()
	if len(findings) == 0 {
		return nil
	}
	refusal := &specFormRefusal{act: act, findings: findings}
	for _, unread := range r.unread() {
		refusal.notes = append(refusal.notes, unread.Line)
	}
	lists := "lists them all."
	if len(findings) > specFormFindingsHeld {
		lists = fmt.Sprintf("lists the first %d of them.", specFormFindingsHeld)
	}
	for _, document := range r.documents {
		if document.artifact.ID == findings[0].ArtifactID && document.artifact.IssueKey != nil {
			refusal.more = fmt.Sprintf("dispatch spec-form --issue %s --artifact %s --version %d %s",
				*document.artifact.IssueKey, document.artifact.Name, document.version, lists)
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
			summary = &specFormSummary{Root: *document.root, Texts: map[specform.Kind]int{}, Terms: len(document.list.Terms), Unread: r.unread()}
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
	// more is the command that lists the findings, all of them or the first specFormFindingsHeld,
	// which the refusal names after the first 20.
	more string
	// notes is the line of each approved document under the root that the judgment could not read.
	notes []string
}

func (r *specFormRefusal) plain() []specform.Finding {
	return plainFindings(r.findings)
}

// plainFindings is the finding of specform in each of findings.
func plainFindings(findings []specFormFinding) []specform.Finding {
	plain := make([]specform.Finding, len(findings))
	for index, finding := range findings {
		plain[index] = finding.Finding
	}
	return plain
}

// words are the refusal's words (specform.Message).
func (r *specFormRefusal) words() specform.Words {
	return specform.Message(r.act, r.plain(), len(r.findings), r.more, r.notes...)
}

// Error is the refusal's words, whole.
func (r *specFormRefusal) Error() string {
	return r.words().String()
}

// body is the refusal's 409 SPEC_FORM body.
func (r *specFormRefusal) body() specFormError {
	words := r.words()
	body := specFormError{
		Code:          specFormCode,
		Error:         words.String(),
		Refusal:       words.Refusal,
		NextStep:      words.NextStep,
		Findings:      r.findings[:min(len(r.findings), specFormFindingsHeld)],
		FindingsTotal: len(r.findings),
	}
	if r.act == specform.ActApproval {
		body.ChangesReason = specform.ChangesReason(r.plain(), len(r.findings), r.more, r.notes...)
	}
	return body
}

// specFormOf judges the form of documents, each at the version it names, in q. A document in
// scope is judged on every act; the read route's act (actRead) judges every document, in scope or
// not. With no root named, the other acts read nothing at all. It reads each version's stored
// markdown, the projects (a bare key of a project names a record), the emails of the people who
// signed in (a piece of one is a name) and, under a root, the approved covered documents under it
// (approvedSpecTexts): the names of their components and interfaces, which are nouns of the
// system and no person, and their terms, which are the root's list of terms (R50). It reads every
// document before it judges one, so that a line of the term-line form outside Terms is judged
// against the definitions of every document of the call, whatever their order. A person's
// approval (specform.ActApproval) judges in the transaction that writes it, and it takes the lock
// of each root's list before it reads that list (lockSpecTerms).
func (f SpecForm) specFormOf(ctx context.Context, q queryer, documents []model.AskApprovalDocument, act specform.Act) (specFormResult, error) {
	var result specFormResult
	if len(f.roots) == 0 && act != actRead {
		return result, nil
	}
	judge := &specFormJudge{
		form: f, q: q, lock: act == specform.ActApproval,
		skip: map[string]bool{}, approved: map[string]approvedSpec{},
		defined: map[string][]requestTerm{}, governing: map[string][]requestTerm{},
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
		scope, err := f.specScopeOf(ctx, q, artifact)
		if err != nil {
			return specFormResult{}, err
		}
		document := specFormDocument{artifact: artifact, version: named.Version, specScope: scope}
		var extracted *specform.Extracted
		if scope.inScope() || act == actRead {
			texts, err := judge.read(ctx, &document)
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
// hand-back of an approval request, and an approval in a person's name (approvalDocuments).
func (f SpecForm) refuseSpecForm(ctx context.Context, tx pgx.Tx, documents []model.AskApprovalDocument, act specform.Act) (specFormResult, error) {
	result, err := f.specFormOf(ctx, tx, documents, act)
	if err != nil {
		return specFormResult{}, err
	}
	if refusal := result.refusal(act); refusal != nil {
		return result, refusal
	}
	return result, nil
}

// approvalDocuments is every document a review in a person's name of document covers, each at
// the version the review records for it, judged for the form when the review approves. With
// request, the approval request open on document, the review answers that request, so it covers
// every document request names, each at the version it names, and document at document.Version
// in its place; with none, document alone. An approval is judged as call site 2 judges
// (refuseSpecForm at ActApproval): a text of one of those documents that breaks the form refuses
// the whole approval. A request for changes is never judged. This is the one answer to which
// documents an approval covers, and the one judgment of them: the person's review on either
// review route and her answer to an approval request write a review of each document it returns
// (queueApprovalReviews), and the carry of an approval in her name asks it before it answers the
// request or writes its own review (carryBreaksForm).
func (f SpecForm) approvalDocuments(ctx context.Context, tx pgx.Tx, request *model.AskApproval, document model.AskApprovalDocument, state string) ([]model.AskApprovalDocument, error) {
	documents := []model.AskApprovalDocument{document}
	if request != nil {
		documents = make([]model.AskApprovalDocument, 0, len(request.Documents)+1)
		named := false
		for _, other := range request.Documents {
			if other.ArtifactID == document.ArtifactID {
				other.Version, named = document.Version, true
			}
			documents = append(documents, other)
		}
		if !named {
			documents = append(documents, document)
		}
	}
	if state == "approved" {
		if _, err := f.refuseSpecForm(ctx, tx, documents, specform.ActApproval); err != nil {
			return nil, err
		}
	}
	return documents, nil
}

// carryBreaksForm is the form gate of the carry of an approval (docs.FormGate): whether the
// approval that the carry would write in the person's name - of document, at its version, which
// answers request when one is open - is refused (approvalDocuments), as her own Approve of the
// same request is. A document that no root covers reads nothing more. NewSpecForm sets it as the
// gate of the process's document service.
func (f SpecForm) carryBreaksForm(ctx context.Context, tx pgx.Tx, request *model.AskApproval, document model.AskApprovalDocument) (bool, error) {
	_, err := f.approvalDocuments(ctx, tx, request, document, "approved")
	var refusal *specFormRefusal
	if errors.As(err, &refusal) {
		return true, nil
	}
	return false, err
}

// specFormJudge judges the documents of one specFormOf call, reading what every document shares
// once.
type specFormJudge struct {
	form SpecForm
	q    queryer
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
	// with its document, in the order of the call.
	defined map[string][]requestTerm
	// governing is the definition of each term under each root that a judgment compares the call's
	// definitions of the term with (governingUnder), made at the first judgment under the root.
	governing map[string][]requestTerm
}

// requestTerm is a definition under Terms of a document of one call, or of the list, with the
// document.
type requestTerm struct {
	artifactID string
	specform.Term
}

// read reads document's named version and its texts, and under a root keeps each definition
// under Terms that it holds (defined), with its place as the list names one. A version that the
// parser refuses (specform.UnreadableError) holds no text: it is one finding of its document,
// kept in unreadable, so the judgment that names it refuses it, with its way out: a new version
// of the document.
func (j *specFormJudge) read(ctx context.Context, document *specFormDocument) (specform.Extracted, error) {
	markdown, err := specFormMarkdown(ctx, j.q, document.artifact, document.version)
	if err != nil {
		return specform.Extracted{}, err
	}
	extracted, err := specform.ExtractDocument(document.artifact.Name, document.version, markdown)
	if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
		document.unreadable = &specFormFinding{
			Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version, Finding: unreadable.Finding(),
		}
		return specform.Extracted{Counts: map[specform.Kind]int{}}, nil
	}
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
// under a root each definition against the first approved definition of its term in join order
// whose document the call does not name, or names at a version that still defines the term
// (governingUnder), unless that one is the document's own. A line of the term-line form outside
// Terms is no definition: it is judged against those definitions and against the definitions
// under Terms of every other document of the call (defined), and it adds nothing. The answer gets
// the root's whole list with its whole record of what Dispatch could not read
// (approvedSpec.read): only the comparisons read the versions that the call names.
func (j *specFormJudge) judge(ctx context.Context, document *specFormDocument, extracted specform.Extracted) error {
	context, err := j.baseContext(ctx)
	if err != nil {
		return err
	}
	context.Pages = documentPages(document.artifact)
	document.list = specFormList{Terms: []specFormTerm{}, Unread: []specFormUnread{}}
	var (
		compared, defined []specform.Term
		refusing          []specFormUnread
	)
	if document.root != nil {
		approved, err := j.approvedUnder(ctx, *document.root)
		if err != nil {
			return err
		}
		context.SystemNames = approved.systemNames(j.skip)
		for _, governing := range j.governingUnder(*document.root, approved) {
			if governing.artifactID != document.artifact.ID {
				compared = append(compared, governing.Term)
			}
		}
		for _, other := range j.defined[*document.root] {
			if other.artifactID != document.artifact.ID {
				defined = append(defined, other.Term)
			}
		}
		document.list = approved.read()
		refusing = approved.refusingDefinitions(j.skip)
	}
	document.texts = extracted.Counts
	document.findings = []specFormFinding{}
	if document.unreadable != nil {
		document.findings = append(document.findings, *document.unreadable)
		return nil
	}
	found, err := specform.Check(ctx, context, specform.Input{Texts: extracted.Texts, Proposed: extracted.Terms, List: compared, Defined: defined})
	if err != nil {
		return err
	}
	for _, finding := range found {
		document.findings = append(document.findings, specFormFinding{
			Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version, Finding: finding,
		})
	}
	// R50 is a gate: a definition that no rule can compare with the terms of an approved document
	// that Dispatch cannot read is refused, naming that document (specform.UnreadListFinding).
	if len(extracted.Terms) > 0 {
		for _, unread := range refusing {
			document.findings = append(document.findings, specFormFinding{
				Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version,
				Finding: specform.UnreadListFinding(unread.Document, unread.Version, unread.Issue, unread.cause),
			})
		}
	}
	return nil
}

// documentPages is each path of the dashboard that draws artifact, under the server's address, as
// the dashboard's pages file lists them (text.DocumentPages): a spec on its issue's own page, on
// the Spec tab and on its Artifacts page, a document of a role on that role's tab and on its
// Artifacts page, another document of an issue on its Artifacts page, and a document of a project
// or of an agent's conversation on its own page. The form reads an address with no scheme of the
// document's texts against each, as each page does.
func documentPages(artifact model.Artifact) []string {
	owner := text.DocumentOwner{Project: artifact.Project, Slug: artifact.Slug, Role: artifact.Role, Primary: artifact.Primary}
	switch {
	case artifact.IssueKey != nil:
		owner.IssueKey = *artifact.IssueKey
	case artifact.SessionID != nil:
		owner.Session = *artifact.SessionID
	}
	return text.DocumentPages(owner)
}

// baseContext is what Check knows of the system for every document: the project keys, the
// emails of the people who signed in, and the server's origins.
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
			Origins:      j.form.origins,
		}
	}
	return *j.base, nil
}

// specFormMarkdown is the markdown of version of artifact as q sees it. When q is a transaction
// whose writes a ledger queues and version is the one it queued and has not written yet - the
// version a carry judges inside the version write that makes it (docs.FormGate) - that is the
// queued markdown (docs.QueuedVersionMarkdown), which no read of artifact_versions sees until the
// write phase has run; otherwise it is the stored row's. A version the document does not have is
// 404 VERSION_NOT_FOUND.
func specFormMarkdown(ctx context.Context, q queryer, artifact model.Artifact, version int) (string, error) {
	if tx, ok := q.(pgx.Tx); ok {
		if markdown, queued := docs.QueuedVersionMarkdown(ctx, tx, artifact.ID, version); queued {
			return markdown, nil
		}
	}
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

// specFormStrings is the one text column query selects, row by row.
func specFormStrings(ctx context.Context, q queryer, query string) ([]string, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
