package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The form of The Spec (R49) as the server holds it. A setting names the roots of The Spec
// (DISPATCH_SPEC_ROOTS); a document under one of them is covered, and this file is the one place
// that judges its form. refuseSpecForm refuses, at its two call sites: the opening or hand-back of
// an approval request (requestArtifactApproval), and an approval in a person's name
// (approvalDocuments), which gives the documents that approval covers and judges each of them.
// Every writer of an approval in a person's name asks approvalDocuments: her review on either
// review route and her answer to an approval request (queueApprovalReviews), and the carry of an
// approval to a new version (carryBreaksForm, docs.FormGate), which carries nothing over a break.
// Two calls read findings and refuse nothing: the read route (getArtifactSpecForm) and the
// restore of a reopen and of the repair pass (prepareRestoration), which judges a request at the
// versions it names once the reopen ends and leaves a broken approval request resolved rather
// than open it again (docs.WriteRestoredAsks, which only it calls, writes the others open). Every
// route that opens an approval request or answers one reaches one of these calls;
// TestOnlyTheFormGateOpensOrAnswersAPersonsApproval holds the code to that census.
//
// One way past the gate stays open, and it is known: an approval given before the setting named
// the root, or while the issue stood outside it, never passed the gate, and a reparent out of a
// root takes an issue's documents out of scope with one write; the read route's scope says so.

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

// specFormTerm is one term of the list of a root.
type specFormTerm struct {
	Term       string `json:"term"`
	Definition string `json:"definition"`
	Document   string `json:"document"`
	Version    int    `json:"version"`
}

// specFormUnread is an approved document under the root that a judgment did not compare with the
// documents it judged, because Dispatch cannot read that approved version
// (specform.UnreadableError). It refuses nothing, and line says so.
type specFormUnread struct {
	Document   string `json:"document"`
	ArtifactID string `json:"artifact_id"`
	Issue      string `json:"issue"`
	Version    int    `json:"version"`
	Line       string `json:"line"`
}

// specFormSummary is what an approval request's answer says of the form of the documents it
// handed the person, when the document it names is in scope: the root, the count of each kind of
// text it read, and each approved document under the root that it could not read. Terms counts
// the terms of the root's list.
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
	// Terms is the list of terms of the root. It is empty until the list is read.
	Terms []specFormTerm `json:"terms"`
	// Unread is each approved document under the root that the judgment could not read.
	Unread []specFormUnread `json:"unread"`
	// RefusesApproval is whether an approval request or an approval of this version is refused.
	RefusesApproval bool `json:"refuses_approval"`
}

// specFormDocument is one document a judgment named: the document, the version judged, where it
// stands, and, once judged, the count of each kind of text, every finding, and each approved
// document under its root that the judgment could not read.
type specFormDocument struct {
	artifact model.Artifact
	version  int
	specScope
	texts    map[specform.Kind]int
	findings []specFormFinding
	unread   []specFormUnread
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

// unread is each approved document that the judgment of a document in scope could not read, once.
func (r specFormResult) unread() []specFormUnread {
	unread := []specFormUnread{}
	for _, document := range r.documents {
		for _, each := range document.unread {
			if document.inScope() && !slices.Contains(unread, each) {
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
			summary = &specFormSummary{Root: *document.root, Texts: map[specform.Kind]int{}, Unread: r.unread()}
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
	plain := make([]specform.Finding, len(r.findings))
	for index, finding := range r.findings {
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
// signed in (a piece of one is a name) and, under a root, the names of the components and
// interfaces of the approved covered documents under it (approvedSpecTexts), which are nouns of
// the system and no person.
func (s *server) specFormOf(ctx context.Context, q queryer, documents []model.AskApprovalDocument, act specform.Act) (specFormResult, error) {
	var result specFormResult
	if len(s.deps.SpecRoots) == 0 && act != actRead {
		return result, nil
	}
	judge := &specFormJudge{server: s, q: q, skip: map[string]bool{}, systemNames: map[string][]string{}, unread: map[string][]specFormUnread{}}
	for _, document := range documents {
		judge.skip[document.ArtifactID] = true
	}
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
		if scope.inScope() || act == actRead {
			if err := judge.judge(ctx, &document); err != nil {
				return specFormResult{}, err
			}
		}
		result.documents = append(result.documents, document)
	}
	return result, nil
}

// refuseSpecForm judges documents (specFormOf) and answers the judgment, with a *specFormRefusal
// of act when a text of a document in scope breaks the form. It has two call sites: the opening or
// hand-back of an approval request, and an approval in a person's name (approvalDocuments).
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
func (s *server) approvalDocuments(ctx context.Context, tx pgx.Tx, request *model.AskApproval, document model.AskApprovalDocument, state string) ([]model.AskApprovalDocument, error) {
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
		if _, err := s.refuseSpecForm(ctx, tx, documents, specform.ActApproval); err != nil {
			return nil, err
		}
	}
	return documents, nil
}

// carryBreaksForm is the form gate of the carry of an approval (docs.FormGate): whether the
// approval that the carry would write in the person's name - of document, at its version, which
// answers request when one is open - is refused (approvalDocuments), as her own Approve of the
// same request is. A document that no root covers reads nothing more.
func (s *server) carryBreaksForm(ctx context.Context, tx pgx.Tx, request *model.AskApproval, document model.AskApprovalDocument) (bool, error) {
	_, err := s.approvalDocuments(ctx, tx, request, document, "approved")
	var refusal *specFormRefusal
	if errors.As(err, &refusal) {
		return true, nil
	}
	return false, err
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
	// skip is every document the call names: the version it names stands in for an approved one.
	skip map[string]bool
	// base is the context every document shares, read at the first judgment.
	base *specform.Context
	// systemNames is the names of the system under each root, and unread each approved document
	// under it that Dispatch cannot read, both read at the first judgment under it.
	systemNames map[string][]string
	unread      map[string][]specFormUnread
}

// judge reads document's named version and judges its texts, each definition of a term among them.
// A version that the parser refuses is one finding of its document (specform.UnreadableError), so
// the judgment that names it refuses it, with its way out: a new version of the document.
func (j *specFormJudge) judge(ctx context.Context, document *specFormDocument) error {
	markdown, err := specFormMarkdown(ctx, j.q, document.artifact, document.version)
	if err != nil {
		return err
	}
	document.findings, document.unread = []specFormFinding{}, []specFormUnread{}
	extracted, err := specform.ExtractDocument(document.artifact.Name, document.version, markdown)
	if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
		document.texts = map[specform.Kind]int{}
		document.findings = append(document.findings, specFormFinding{
			Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version, Finding: unreadable.Finding(),
		})
		return nil
	}
	if err != nil {
		return err
	}
	context, err := j.contextFor(ctx, document.root)
	if err != nil {
		return err
	}
	context.Pages = documentPages(document.artifact)
	document.texts = extracted.Counts
	if document.root != nil {
		document.unread = append(document.unread, j.unread[*document.root]...)
	}
	found, err := specform.Check(ctx, context, specform.Input{Texts: extracted.Texts, Proposed: extracted.Terms})
	if err != nil {
		return err
	}
	for _, finding := range found {
		document.findings = append(document.findings, specFormFinding{
			Document: document.artifact.Name, ArtifactID: document.artifact.ID, Version: document.version, Finding: finding,
		})
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

// contextFor is what Check knows of the system for a document under root (nil for none).
func (j *specFormJudge) contextFor(ctx context.Context, root *string) (specform.Context, error) {
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
	context := *j.base
	if root == nil {
		return context, nil
	}
	names, read := j.systemNames[*root]
	if !read {
		approved, err := j.server.approvedSpecTexts(ctx, j.q, *root, j.skip)
		if err != nil {
			return specform.Context{}, err
		}
		names = approved.SystemNames
		j.systemNames[*root], j.unread[*root] = names, approved.Unread
	}
	context.SystemNames = names
	return context, nil
}

// approvedSpec is what the approved covered documents under a root hold that a judgment of
// another document reads: the names of the components and interfaces they define, which are
// nouns of the system, and each of those documents whose approved version Dispatch cannot read.
type approvedSpec struct {
	SystemNames []string
	Unread      []specFormUnread
}

// approvedSpecTexts reads the latest approved version of each covered document under root but
// those skip names: the root's terms.md, and the spec and the Verification document of each issue
// whose nearest named root is root (refs.IssueDescendants, less each named root below root and
// its own subtree). One statement reads every such document with the markdown of that version,
// in the order of docs.LatestApprovedReview, so the statements of a judgment do not grow with the
// documents under its root; a document that no person approved has no row. An approved version
// that the parser refuses gives nothing to compare: the judgment goes on without its texts, logs
// it once, and names it in Unread. It reads the issues under root as everyReader: the judgment is
// the same for each person whose request or approval it judges, and its findings are kept as the
// reason of a request that others read, so an issue reserved for its author, and what is below it,
// gives no text to it.
func (s *server) approvedSpecTexts(ctx context.Context, q queryer, root string, skip map[string]bool) (approvedSpec, error) {
	issues, err := refs.IssueDescendants(ctx, q, everyReader, root)
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the issues under %s: %w", root, err)
	}
	for _, inner := range s.deps.SpecRoots {
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
		select covered.id::text, covered.name, covered.issue_key, approved.version, stored.number, coalesce(stored.markdown, '')
		from artifacts covered
		cross join lateral (
			select artifact_reviews.version from artifact_reviews
			where artifact_reviews.artifact_id = covered.id and artifact_reviews.state = 'approved'
			order by `+docs.ReviewOrder+`
			limit 1
		) approved
		left join artifact_versions stored on stored.artifact_id = covered.id and stored.number = approved.version
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
		id, name string
		issue    *string
		version  int
		stored   *int
		markdown string
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (approvedVersion, error) {
		var each approvedVersion
		err := row.Scan(&each.id, &each.name, &each.issue, &each.version, &each.stored, &each.markdown)
		return each, err
	})
	if err != nil {
		return approvedSpec{}, fmt.Errorf("read the approved documents under %s: %w", root, err)
	}
	var approved approvedSpec
	for _, each := range versions {
		if skip[each.id] {
			continue
		}
		if each.stored == nil {
			return approvedSpec{}, errorf(http.StatusNotFound, "VERSION_NOT_FOUND", "%s has no version %d", each.name, each.version)
		}
		extracted, err := specform.ExtractDocument(each.name, each.version, each.markdown)
		if unreadable := (*specform.UnreadableError)(nil); errors.As(err, &unreadable) {
			issue := deref(each.issue)
			slog.Warn("dispatch: the form of The Spec compares no text with an approved version that it cannot read",
				"artifact", each.id, "document", each.name, "issue", issue, "version", each.version, "error", err)
			approved.Unread = append(approved.Unread, specFormUnread{
				Document: each.name, ArtifactID: each.id, Issue: issue, Version: each.version,
				Line: specform.UnreadLine(each.name, each.version, issue),
			})
			continue
		}
		if err != nil {
			return approvedSpec{}, err
		}
		for _, text := range extracted.Texts {
			if text.Kind == specform.KindName {
				approved.SystemNames = append(approved.SystemNames, text.Body)
			}
		}
	}
	return approved, nil
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
