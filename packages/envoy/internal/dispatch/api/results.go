package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results.go records and reads the results of an issue's checks. A check is a `check` block of
// a Verification document, and a result says one run of it passed or failed, at one commit.
// The record route takes a result for a check of the recording issue or of any issue above it
// (refs.IssueAncestors) and appends `result.recorded` on the check's own issue. The read route
// lists the checks of one issue's Verification document with every result recorded against
// each, from any issue, newest first, and says what each check compared those results with: a
// `before_merge` check is compared with the pull request heads of the check's own issue, so a
// child's pull request answers for nothing against its parent's check. The comparison itself
// is answered in results_comparison.go.

// checkResultRole bounds the role a result names: the word the pull-request body line prints.
var checkResultRole = regexp.MustCompile(`^[a-z-]{1,32}$`)

// resultCommitSHA is the commit a result ran at, as the column's own check spells it.
var resultCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The two values of a check's `when`, beside before_merge (docs.CheckWhenBeforeMerge), that the
// closed enum pmdoc/schema/blocks.json declares under `choices`.
const (
	checkWhenAfterMerge    = "after_merge"
	checkWhenDefaultBranch = "default_branch"
)

// checkResultReadingRunes is how much of the role's reading the pull-request body line carries,
// and how much of the check's own words the line of a derived result carries (ciVerdictLine),
// each counted on what lineWords keeps of the text.
const checkResultReadingRunes = 80

// checkResultColumns is the column list every check_results read scans, in scanCheckResult's
// order.
const checkResultColumns = `id::text, issue_key, check_issue_key, check_artifact_id::text, check_block_id,
	check_version, check_sha256, role, surface, run, observed, commit_sha, negative_control,
	failed, skipped, excluded, verdict, screenshots, session, actor, created_at, event_seq, commit_repo`

// checkBlockRef is where one `check` block a result names lives: the issue whose Verification
// document holds it, that document, and the block within it. The resolve settles it and nothing
// moves it: a result is recorded against this block or against none.
type checkBlockRef struct {
	IssueKey   string
	ArtifactID string
	BlockID    string
}

// indexedCheck is what document_blocks holds for one check block: the document version its
// words last changed in, and their digest. The two are read in one statement and judged
// together, never one without the other (indexedCheckBlock).
type indexedCheck struct {
	Version int
	SHA256  string
}

// checkResolution is one reading of a check block: the attributes the write judges (its `when`,
// run, negative control and ci_check) as one read of the live document found them, carried with
// the indexed version and digest they were read against. They travel as one value because the
// attributes in hand are the block's only while the index still holds that pair for the block,
// which the write half re-reads under its locks and compares (recordResultUnderOwnerLocks): a
// resolution the locks contradict is thrown away whole and made again, never patched in place.
type checkResolution struct {
	indexedCheck
	Attributes docs.CheckAttributes
}

// resolvedCheck is one `check` block a result names, as a resolve found it: where it lives, and
// the reading of it in hand.
type resolvedCheck struct {
	checkBlockRef
	checkResolution
}

// checkResultInput is the body POST /api/v1/issues/{key}/results takes.
type checkResultInput struct {
	Actor           *model.Actor `json:"actor"`
	Check           string       `json:"check" required:"true"`
	Role            string       `json:"role" required:"true"`
	Surface         string       `json:"surface"`
	Run             string       `json:"run"`
	Observed        string       `json:"observed"`
	Commit          string       `json:"commit" required:"true"`
	NegativeControl string       `json:"negative_control"`
	Verdict         string       `json:"verdict" required:"true"`
	Failed          int          `json:"failed"`
	Skipped         int          `json:"skipped"`
	Excluded        int          `json:"excluded"`
	Screenshots     []string     `json:"screenshots"`
}

// checkResultRecorded is the body POST /api/v1/issues/{key}/results answers.
type checkResultRecorded struct {
	Line   string            `json:"line"`
	Result model.CheckResult `json:"result"`
}

// checkResultDryRun is the body POST /api/v1/issues/{key}/results/dry-run answers.
type checkResultDryRun struct {
	Line string `json:"line"`
}

// resultRequest is a result's body as the record and its dry run have judged it before anything
// is written (readResultRequest): the input and its actor, the issue it is recorded on, the check
// it names, the one repository the check's issue resolves to, the linked repository the commit is
// taken to be of (refuseUnknownCommit, nil where it is taken to be repo's), and the rule the
// pull-request body line keeps words by.
type resultRequest struct {
	issueKey         string
	input            checkResultInput
	actor            model.Actor
	check            resolvedCheck
	repo             *string
	linkedCommitRepo *string
	words            lineWords
}

// POST /api/v1/issues/{key}/results  (any authenticated actor)
//
// Records one result against one check. {key} is the recording issue, which need not be the
// check's: a result against a check of an issue above it is taken, and the event goes on that
// issue. Such a result is no proof of the check, since it stands at a basis only at a head of
// a pull request of the check's own issue (comparisonReader). The answer carries the stored
// row and the pull-request body line it implies, so every writer writes one grammar.
func (s *server) recordCheckResult(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readResultRequest(w, r)
	if !ok {
		return
	}
	// The row keeps linkedCommitRepo, so the results read compares the result with the branches
	// of that linked repository (comparisonReader, onNamedBranch), reads its pull requests
	// through it (readThroughHead) and decides a `before_merge` result by it (commitStands):
	// asked of repo, the repository the check's issue resolves to, GitHub answers 404 for such a
	// commit and the result reads unknown. Such a repository is one GitHub said holds the commit.
	// Every other row keeps none and is compared in the repository the check's issue resolves to,
	// as every row recorded before the column existed is, at the cost it always had.
	//
	// The reading resolveCheck made is the block's only while the version and digest the index
	// holds for it have not moved since, which the locked half re-reads and judges; the one
	// re-read a moved pair needs runs there too, with every lock given back.
	written, err := s.recordResultWithFreshWords(
		r.Context(), request.issueKey, request.check, request.input, request.linkedCommitRepo, request.actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The read-through runs before the event: the Verification tab refetches on
	// result.recorded, and no later event refreshes it, so a tab that read the results before
	// the pull request row landed would show this result standing at no basis until something
	// else happened. It reads through the repository the commit is taken to be of: the linked
	// one, and else repo.
	readThrough := request.repo
	if request.linkedCommitRepo != nil {
		readThrough = request.linkedCommitRepo
	}
	s.readThroughPullRequests(r.Context(), request.check.IssueKey, written.result.CommitSHA, readThrough)
	s.publish(r.Context(), written.event)
	s.storeDisplayTrees(r.Context(), written.result.ID, written.result.Observed)
	WriteJSON(w, http.StatusCreated, checkResultRecorded{
		// The check as the transaction that wrote the row saw it, which after a redo is not the
		// one resolveCheck read: the line names the `when` the result was judged by.
		Line:   checkResultLine(written.check, written.result, request.words),
		Result: written.result,
	})
}

// POST /api/v1/issues/{key}/results/dry-run  (any authenticated actor)
//
// The record's dry run: it takes the record's body, makes every refusal the record makes before
// it opens its transaction (readResultRequest), and answers the pull-request body line the record
// would answer, writing nothing. The refusals the record makes under its locks - a closed issue,
// a screenshot of another issue, a check a document version rewords at every attempt - are the
// record's alone. It is a path of its own, not a flag on the record's, because a release that
// does not serve it answers a path it does not know 404, where it would drop a flag it does not
// know and record the result.
func (s *server) dryRunCheckResult(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readResultRequest(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, checkResultDryRun{Line: checkResultLine(request.check, model.CheckResult{
		Role: request.input.Role, Observed: request.input.Observed,
		CommitSHA: request.input.Commit, Verdict: request.input.Verdict,
	}, request.words)})
}

// readResultRequest decodes a result's body and judges it as far as the record goes before it
// opens its transaction, for the record and its dry run alike. A refusal is written to w and
// answers false.
func (s *server) readResultRequest(w http.ResponseWriter, r *http.Request) (resultRequest, bool) {
	refused := func(err error) (resultRequest, bool) {
		s.writeHandlerError(w, err)
		return resultRequest{}, false
	}
	var input checkResultInput
	if err := decodeJSON(r, &input); err != nil {
		return refused(err)
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return resultRequest{}, false
	}
	if err := input.refusal(); err != nil {
		return refused(err)
	}
	issueKey := r.PathValue("key")
	pool, err := s.readyPool()
	if err != nil {
		return refused(err)
	}

	// The check is resolved before the transaction opens. Resolving it folds the live
	// Verification document, which for a document no room holds is a cold read bounded by a
	// minute (docs/read_cache.go), and no issue's owner lock is held across a read that long:
	// four writers parked on one issue's lock already cost a read of an unrelated issue the
	// whole seventeen seconds the lock was held (store/pool.go). Nothing the lock protects is
	// settled here — the check's words and its `when` are the document's, not the issue's — and
	// what it does protect, the block's indexed version and digest, is read again under it
	// below.
	check, err := s.resolveCheck(r.Context(), pool, issueKey, input.Check)
	if err != nil {
		return refused(err)
	}
	// The check may be on an issue of another project, which a parent of another project puts
	// above the recording issue, and the result is appended to that issue. The pre-handler
	// check read only the path's issue, so a credential scoped to one project is refused such a
	// check here, before anything is written.
	callerProject, _ := s.callerProject(r)
	if err := refuseCheckOutsideCallerProject(r.Context(), pool, callerProject, check.IssueKey); err != nil {
		return refused(err)
	}
	// A check CI proves takes no result from a role, refused here, before refuseUnknownCommit,
	// so no GitHub read is spent on it.
	if err := refuseCheckProvedByCI(r.Context(), pool, check); err != nil {
		return refused(err)
	}

	// The one repository the check's issue resolves to, read once for the refusal below of a
	// commit no repository the result may name holds. That refusal answers the linked repository
	// the commit is taken to be of, if any, and the read-through of the pull requests GitHub
	// names for that commit reads after the write through it, or through this one. It is
	// nil when there is nothing to ask GitHub about — a deployment with no App key, an issue that
	// resolves to no repository, and a read of Dispatch's own row of the issue's repository that
	// failed, which is logged; with no repository to ask about, none of the refusals below is
	// asked, and the result is stored, as with no App key. A failed read of Dispatch's own rows of
	// the issue's links is another read, which the
	// refusal below makes where there is a repository to ask about and GitHub does not say it
	// holds the commit, and it refuses the record with 500 and stores nothing
	// (refuseUnknownCommit, results_linked.go).
	//
	// It is the check's issue's repository, not the recording issue's, and refuseUnknownCommit
	// reads the links of the check's issue too: the results read resolves check_issue_key, and a
	// refusal keyed on the recording issue would disagree with the read about which repository
	// the commit belongs to. It is resolved through the one helper the results read resolves
	// through (issue_repo.go, server.checkIssueRepository), under the canonical name the
	// repository is called today: a refusal asking under a name the repository was renamed away
	// from reads the 404 GitHub answers for that name's installation, where the read reads the
	// commit — so it would store every result of such an issue.
	//
	// The App key is checked here and not in the helper: every use of the answer below is a
	// GitHub round trip, while the results read needs the repository even with no App key, for
	// the default-branch answers it reads from Dispatch's own stored rows.
	//
	// The helper says whether its resolve answered at all, which the results read needs to tell
	// a check that compares with nothing from one it could not compare (results_comparison.go,
	// nothingReason). This route takes the repository alone, on purpose: the refusal below and
	// the read-through it answers for are already answered by a nil, and the two nothings would
	// earn one answer here anyway. Only GitHub's proof that the repository holds no such commit,
	// a read of a linked repository that did not answer and a failed read of the issue's links
	// refuse a record, and none of them is asked of a nil repository, so a resolve that did not
	// answer stores the result, exactly as a deployment with no App key and an issue with no
	// repository do.
	var checkRepo *string
	if s.deps.GitHub != nil {
		checkRepo, _ = s.checkIssueRepository(r.Context(), pool, check.IssueKey)
	}

	// Asked here for the reason resolveCheck is asked here, before the transaction opens: it is
	// one GitHub round trip or more, and the comment over that resolve records what a slow read
	// under the two issues' owner locks costs. Nothing is written before it, so a refusal leaves
	// no row and no event behind. linkedCommitRepo is the linked repository the commit is taken
	// to be of, and nil where it is taken to be checkRepo's: a result at the head of a pull
	// request the issue links in another repository names a commit of that repository. A linked
	// read that did not answer, where no repository was shown to hold the commit, refuses the
	// record for now rather than storing it under a repository no read showed to hold it
	// (refuseUnknownCommit, results_linked.go).
	linkedCommitRepo, err := s.refuseUnknownCommit(r.Context(), pool, check.IssueKey, checkRepo, input.Commit)
	if err != nil {
		return refused(err)
	}

	// The project keys the line drops issue keys by are read before the transaction too, so a
	// read that fails refuses the record with nothing written: read after the commit, it would
	// lose the answer to a write that landed.
	words, err := s.readLineWords(r.Context(), pool)
	if err != nil {
		return refused(err)
	}
	return resultRequest{
		issueKey: issueKey, input: input, actor: actor, check: check,
		repo: checkRepo, linkedCommitRepo: linkedCommitRepo, words: words,
	}, true
}

// checkContended is the code of a results write whose check a document version moved at every
// attempt. Nothing is recorded, and the caller's retry resolves the check again.
const checkContended = "CHECK_CONTENDED"

// checkContendedRetryAfter is the Retry-After, in whole seconds, that refusal carries. What the
// caller waits for is another writer's document version landing on the check's block - a
// transaction already committing, not a budget this caller has spent - so the wait is the
// shortest whole second HTTP can ask for, which is what Dispatch already answers a caller whose
// write another transaction is in the middle of (writeKeyInFlight's 409 KEY_IN_FLIGHT,
// request_keys.go). Nothing longer is honest here: this is no rate limit, so there is no budget
// to refill and no burst to spread out, and a longer wait would only hold back a retry that can
// land as soon as the version that moved the check has committed.
const checkContendedRetryAfter = 1

// recordedCheckResult is what one results write produces: the row it wrote, the event it
// appended, and the check as the transaction that wrote them saw it. That check is not the one
// the route resolved when a redo made the resolution again, so the pull-request body line is
// written from this one.
type recordedCheckResult struct {
	result model.CheckResult
	event  model.Event
	check  resolvedCheck
}

// recordResultWithFreshWords records one result against the check as the transaction that writes
// it sees the check, and never reads the live document under the two issues' owner locks. It is
// this route's resolve-then-lock cycle (envoy_resolve.go): resolveThenLock owns the redo and its
// bound, resolutionAttempts, and a resolution the locks contradict at every attempt is the 409
// CHECK_CONTENDED below, with nothing recorded.
//
// A check's attributes - its `when`, run, negative control and ci_check - live in the document,
// which for a document no room holds is a cold read bounded by a minute (docs/read_cache.go);
// the version its words last changed in and their digest live in document_blocks, which the
// same transaction as the version writes. The index row is read first and the document after it
// (resolveCheck), and the pair is read in one statement and compared whole, so a version that
// moved either half of it is seen: a pair that still stands under the locks is the version and
// digest the reading in hand was read against. Either of them moving makes that reading a
// version this transaction does not see, and the locked half gives the locks back rather than
// reading the document again under them - that read is the slow one, because a version landing
// in between is exactly what leaves the read cache cold, so the one case it answers differently
// is the one it answers slowest in, and meanwhile every writer of either issue waits behind it
// (lockIssueOwners, store.LockOwner).
//
// Exactly one read of the live document is paid on the common path: resolveCheck already made it
// before the transaction opened, and that reading is what the first attempt is handed. Only a
// redo reads the document again, and it reads the attributes alone, with nothing held, against
// the version and digest the refused attempt's locks found.
//
// linkedCommitRepo is what the row keeps as commit_repo (recordCheckResult): the linked
// repository refuseUnknownCommit answered, or nil. A redo writes the same one, since it is the
// commit's answer and no rewording of the check moves it.
func (s *server) recordResultWithFreshWords(
	ctx context.Context, issueKey string, check resolvedCheck, input checkResultInput,
	linkedCommitRepo *string, actor model.Actor,
) (recordedCheckResult, error) {
	refusal := errorf(http.StatusConflict, checkContended,
		"check %s was reworded at each of %d attempts while this request waited to lock its issues, so nothing was recorded: read the check and retry",
		check.BlockID, resolutionAttempts)
	refusal.retryAfter = checkContendedRetryAfter

	// What the last attempt's locks found in document_blocks, which the redo reads the attributes
	// against. The locked half hands it back rather than writing into the resolution it was
	// given: a resolution is one reading, whole, and a redo makes a new one of it.
	var locked indexedCheck
	resolved := false
	return resolveThenLock(ctx,
		func(ctx context.Context) (checkResolution, error) {
			if !resolved {
				resolved = true
				return check.checkResolution, nil
			}
			attributes, err := s.checkBlockAttributes(ctx, check.ArtifactID, check.BlockID)
			if err != nil {
				return checkResolution{}, err
			}
			return checkResolution{indexedCheck: locked, Attributes: attributes}, nil
		},
		func(ctx context.Context, resolution checkResolution) (recordedCheckResult, error) {
			written, found, err := s.recordResultUnderOwnerLocks(
				ctx, issueKey, check.checkBlockRef, resolution, input, linkedCommitRepo, actor)
			locked = found
			return written, err
		},
		refusal)
}

// recordResultUnderOwnerLocks is the locked half of that cycle - resolveThenLock's `write` - and
// the only place this route opens a transaction: it takes both issues' owner locks, re-reads
// under them the version and digest the index holds for the check, judges the result by the
// resolution those confirm, writes the row and its event and commits.
//
// A resolution the locks contradict is errStaleResolution, with the transaction's deferred
// rollback run before the error reaches resolveThenLock and nothing written; the version and
// digest the locks found come back beside it, for the redo to read the words of.
func (s *server) recordResultUnderOwnerLocks(
	ctx context.Context, issueKey string, check checkBlockRef, resolved checkResolution,
	input checkResultInput, linkedCommitRepo *string, actor model.Actor,
) (recordedCheckResult, indexedCheck, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	defer tx.Rollback(ctx)

	// Both issues' owner locks are taken in one key order, whether or not either is open: a
	// result for an after-merge check is taken on a closed issue, and the lock still stops it
	// closing or reopening under the write. The check's issue is usually the recording issue's
	// ancestor, so locking them in call order would let one transaction hold the child while
	// waiting for the parent and another hold the parent while waiting for the child;
	// lockIssueOwners is the one key order every writer of two issues takes. It is the owner
	// lock and not the issue rows, so this read phase holds no transaction id.
	if _, err := lockIssueOwners(ctx, tx, issueKey, check.IssueKey); err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	recordingOpen, checkOpen, err := issuesOpenForResult(ctx, tx, issueKey, check.IssueKey)
	if err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	// The version and digest the row is judged by are the block's as this transaction sees them,
	// read under the locks: a document version that landed between the resolve and the lock would
	// otherwise have the result recorded against the words it replaced, and a check reworded from
	// before_merge to after_merge would be judged by the `when` it carried before the lock. The
	// words and the attributes are never read here, only judged (indexedCheckBlock).
	indexed, err := indexedCheckBlock(ctx, tx, check, input.Check)
	if err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	if indexed != resolved.indexedCheck {
		return recordedCheckResult{}, indexed, errStaleResolution
	}
	// Asked again under the locks: a document write can have given the block a ci_check since the
	// resolve, and a result recorded since takes the block back from CI. Both are read in this
	// transaction, so whichever of a racing result and a racing ci_check commits, every reader
	// reads the block as one kind of check.
	if err := refuseCheckProvedByCI(ctx, tx, resolvedCheck{checkBlockRef: check, checkResolution: resolved}); err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	// A closed issue takes a result only for a check that runs after the merge or on the default
	// branch: such a check can only be proved once the issue has closed, so refusing it would make
	// the production check unrecordable. A result for a pre-merge check stays refused there, as a
	// document version does; a comment and a message are the other writes a closed issue takes.
	if (!recordingOpen || !checkOpen) && resolved.Attributes.When != checkWhenAfterMerge && resolved.Attributes.When != checkWhenDefaultBranch {
		return recordedCheckResult{}, indexedCheck{}, errorf(http.StatusConflict, "ISSUE_CLOSED",
			"issue is closed: only a result for a check whose when is after_merge or default_branch is recorded there, and %s is %s", check.BlockID, resolved.Attributes.When)
	}
	if err := s.refuseResultScreenshots(ctx, tx, issueKey, check.IssueKey, input.Screenshots); err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}

	result := model.CheckResult{
		ID: uuid.NewString(), IssueKey: issueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: resolved.Version, CheckSHA256: resolved.SHA256,
		Role: input.Role, Surface: input.Surface, Run: input.Run, Observed: input.Observed,
		CommitSHA: input.Commit, CommitRepo: linkedCommitRepo, NegativeControl: input.NegativeControl,
		Failed: input.Failed, Skipped: input.Skipped, Excluded: input.Excluded,
		Verdict: input.Verdict, Screenshots: nonNilStrings(input.Screenshots),
		Session: sessionOf(actor), Actor: actor,
	}
	event, err := s.appendEvent(ctx, tx, issueOwner(check.IssueKey).event("result.recorded", actor, model.CheckResultEventPayload{
		ResultID: result.ID, Issue: issueKey, CheckIssue: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID, CheckVersion: resolved.Version,
		When: resolved.Attributes.When, Verdict: result.Verdict, Role: result.Role, CommitSHA: result.CommitSHA,
	}))
	if err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	seq := int64(event.Seq)
	result.EventSeq = &seq
	if err := insertCheckResult(ctx, tx, &result); err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return recordedCheckResult{}, indexedCheck{}, err
	}
	return recordedCheckResult{
		result: result,
		event:  event,
		check:  resolvedCheck{checkBlockRef: check, checkResolution: resolved},
	}, indexed, nil
}

// issueCheckResults is the body GET /api/v1/issues/{key}/results answers.
type issueCheckResults struct {
	Checks                 []model.CheckBlock `json:"checks"`
	Issue                  string             `json:"issue"`
	VerificationArtifactID string             `json:"verification_artifact_id"`
}

// GET /api/v1/issues/{key}/results  (any authenticated caller)
//
// The checks of the issue's Verification document in document order, each with what its
// results are compared with and whether a pass of it stands, and every result recorded
// against it from any issue, newest first, each with where its commit stands and whether the
// check's words moved since. An issue with no Verification document has no checks and no
// error.
//
// A check CI proves carries `ci` too, read from GitHub at the issue's newest pull request
// head, and its one result is the one derived from the run its `ci_check` names
// (deriveCIResults), which is judged as a recorded result is.
//
// ?at=<commit> replaces what every check is compared with by the one commit the reader names.
// The only read of GitHub it leaves is the check runs of a check CI proves, read at the issue's
// newest pull request head as on every read: the derived result is that head's, and stands at
// the named commit only where that commit is the head.
func (s *server) listCheckResults(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	at := r.URL.Query().Get("at")
	if at != noCommitNamed && !resultCommitSHA.MatchString(at) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"at must be a commit of 40 lowercase hex characters")
		return
	}
	issueKey := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifact, held, err := s.verificationDocument(r.Context(), s.deps.Store.Pool, issueKey, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	body := issueCheckResults{Checks: []model.CheckBlock{}, Issue: issueKey, VerificationArtifactID: artifact.ID}
	if !held {
		WriteJSON(w, http.StatusOK, body)
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	checks, err := s.checkBlocksOf(r.Context(), s.deps.Store.Pool, artifact, trees)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	recorded, err := loadCheckResults(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	retracted, err := loadCheckResultRetractions(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index := range checks {
		results := recorded[checks[index].BlockID]
		if results == nil {
			results = []model.CheckResult{}
		}
		for result := range results {
			if retraction, held := retracted[results[result].ID]; held {
				results[result].Retracted = &retraction
			}
		}
		checks[index].Results = results
	}
	// The reader learns the heads GitHub names before any check CI proves is derived
	// (deriveCIResults). A derived result is read at the issue's newest pull request head, which
	// may be a head this read's read-through learns: derived first, a first read would answer
	// that Dispatch holds no head while it names the head it learned as another check's basis. A
	// derived result is at that newest head, which the learned heads already hold, so it leaves
	// nothing to learn. Every head GitHub has to be asked for is learned before the first check is
	// answered, and where the learn stored or learned any head every check's bases are read again,
	// so each check of the answer is answered on the heads as they stand after this read's
	// read-throughs, and two `before_merge` checks of one issue name one list of bases and not two
	// (comparisonReader.learn).
	reader := newComparisonReader(r.Context(), s, at)
	reader.learn(r.Context(), checks)
	if err := s.deriveCIResults(r.Context(), reader, issueKey, checks); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Each result's observed text is registered for its tree once no check's results are replaced
	// any more (deriveCIResults), since what is registered is a pointer into the row.
	for index := range checks {
		for result := range checks[index].Results {
			trees.text(&checks[index].Results[result].ObservedTree, checks[index].Results[result].Observed)
		}
	}
	for index := range checks {
		reader.answer(r.Context(), &checks[index])
	}
	answerCompatibilityCurrency(checks, at)
	body.Checks = checks
	trees.resolve()
	WriteJSON(w, http.StatusOK, body)
}

// refusal is every refusal a result's own fields earn, before anything is read. The three counts
// are each a non-negative number check_results keeps in an integer column, so a count past that
// column (fitsIntegerColumn) is refused here, before the query that would fail to encode it.
func (input checkResultInput) refusal() error {
	if strings.TrimSpace(input.Check) == "" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "check names the check block the result proves")
	}
	if !checkResultRole.MatchString(input.Role) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT",
			"role is required and is at most 32 characters of a-z and -, the word the pull-request line prints")
	}
	if input.Verdict != "pass" && input.Verdict != "fail" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "verdict is pass or fail")
	}
	if !resultCommitSHA.MatchString(input.Commit) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "commit is the 40-character lowercase hex commit the check ran at")
	}
	for _, count := range []struct {
		name  string
		cases int
	}{{"failed", input.Failed}, {"skipped", input.Skipped}, {"excluded", input.Excluded}} {
		if count.cases < 0 || !fitsIntegerColumn(count.cases) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT", "%s counts cases, from 0 to %d", count.name, maxIntegerColumn)
		}
	}
	if input.Verdict == "pass" && (input.Failed > 0 || input.Skipped > 0 || input.Excluded > 0) {
		return errorf(http.StatusBadRequest, "RESULT_CONTRADICTS",
			"a pass counts no failed, skipped or excluded case; this one counts %d failed, %d skipped and %d excluded",
			input.Failed, input.Skipped, input.Excluded)
	}
	return nil
}

// issuesOpenForResult answers whether the recording issue and the check's issue are open, in
// one query under the owner locks the caller has already taken (lockIssueOwners). It makes no
// refusal of its own for a closed issue, as requireOpenOwnerStatus does: the results route
// decides for itself, since a closed issue takes a result for an after-merge check. An issue
// with no row is the route's 404, which the locks never answer: lockIssueOwners takes an owner's
// lock whether or not a row holds that key.
func issuesOpenForResult(ctx context.Context, tx pgx.Tx, issueKey, checkIssueKey string) (bool, bool, error) {
	open := map[string]bool{}
	rows, err := tx.Query(ctx, `select key, closed_at is null from issues where key = any($1)`,
		[]string{issueKey, checkIssueKey})
	if err != nil {
		return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var isOpen bool
		if err := rows.Scan(&key, &isOpen); err != nil {
			return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
		}
		open[key] = isOpen
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
	}
	recording, held := open[issueKey]
	if !held {
		return false, false, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
	}
	check, held := open[checkIssueKey]
	if !held {
		return false, false, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
	}
	return recording, check, nil
}

// requireIssueExists is the read route's 404 for an issue nobody has.
func (s *server) requireIssueExists(ctx context.Context, key string) error {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	var exists bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `select true from issues where key = $1`, key).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
		}
		return fmt.Errorf("read issue %s: %w", key, err)
	}
	return nil
}

// verificationDocument is one issue's Verification document and whether it has one. slug, when
// it is not "", holds the answer to the document filed under it, which is how a canonical block
// reference names one. A row whose role is null answers to its slug alone: a document the
// release before this one created during the rolling deploy carries no role until the next
// version written under its row repairs it (documentRole), and the slug is meanwhile the only
// thing that says what the document is. Every reader of an issue's Verification document in
// this package goes through here — the results routes, the check resolver and the approval
// request's second document.
func (s *server) verificationDocument(ctx context.Context, q queryer, issueKey, slug string) (model.Artifact, bool, error) {
	artifact, err := scanArtifact(q.QueryRow(ctx, `
		select `+artifactColumns+` from artifacts
		where issue_key = $1 and `+verificationRolePredicate+` and ($2 = '' or slug = $2)
		limit 1
	`, issueKey, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, false, nil
	}
	if err != nil {
		return model.Artifact{}, false, fmt.Errorf("read the Verification document of %s: %w", issueKey, err)
	}
	return artifact, true, nil
}

// resolveCheck finds the check a result names among the Verification documents of the recording
// issue and of every issue above it. The value is a block id, or the canonical block reference
// (`dispatch://KEY/artifact/verification-md#b-<id>`) that names one document when a block id is
// not unique in that set: an id two documents share is 409 CHECK_AMBIGUOUS, and an id no
// Verification document of the set holds — or one that is not a `check` block — is 400
// UNKNOWN_CHECK.
func (s *server) resolveCheck(ctx context.Context, q queryer, issueKey, value string) (resolvedCheck, error) {
	ancestors, err := refs.IssueAncestors(ctx, q, issueKey)
	if err != nil {
		return resolvedCheck{}, err
	}
	scope := append([]string{issueKey}, ancestors...)

	blockID, artifactFilter := value, ""
	if ref, ok := text.CanonicalBlockRef(value); ok {
		// A block of a document carries its document's fields and no Item; a block of a comment
		// or a message names that item in Item and is no check.
		if ref.Item != "" || ref.IssueKey == "" || ref.ID == "" || ref.Block == "" {
			return resolvedCheck{}, unknownCheck(value)
		}
		blockID = ref.Block
		document, held, err := s.verificationDocument(ctx, q, ref.IssueKey, ref.ID)
		if err != nil {
			return resolvedCheck{}, err
		}
		if !held {
			return resolvedCheck{}, unknownCheck(value)
		}
		artifactFilter = document.ID
	}

	rows, err := q.Query(ctx, `
		select a.issue_key, a.slug, d.artifact_id::text, d.changed_version, d.text_sha256
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where `+aliasedVerificationRole+`
		  and a.issue_key = any($1) and d.type = 'check' and d.block_id = $2
		  and ($3 = '' or d.artifact_id = $3::uuid)
		order by a.issue_key
	`, scope, blockID, artifactFilter)
	if err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	defer rows.Close()
	var found []resolvedCheck
	var references []string
	for rows.Next() {
		check := resolvedCheck{checkBlockRef: checkBlockRef{BlockID: blockID}}
		var slug string
		if err := rows.Scan(&check.IssueKey, &slug, &check.ArtifactID, &check.Version, &check.SHA256); err != nil {
			return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
		}
		found = append(found, check)
		if reference, ok := text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: check.IssueKey, ID: slug, Block: blockID,
		}); ok {
			references = append(references, reference)
		}
	}
	if err := rows.Err(); err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	if len(found) == 0 {
		return resolvedCheck{}, unknownCheck(value)
	}
	if len(found) > 1 {
		issues := make([]string, len(found))
		for index, check := range found {
			issues[index] = check.IssueKey
		}
		return resolvedCheck{}, errorf(http.StatusConflict, "CHECK_AMBIGUOUS",
			"check %s is a check of the Verification document of %s: name one with its reference, %s",
			blockID, strings.Join(issues, " and "), strings.Join(references, " or "))
	}
	check := found[0]
	if check.Attributes, err = s.checkBlockAttributes(ctx, check.ArtifactID, check.BlockID); err != nil {
		return resolvedCheck{}, err
	}
	return check, nil
}

func unknownCheck(value string) error {
	return errorf(http.StatusBadRequest, "UNKNOWN_CHECK",
		"%s is no check of this issue's Verification document or of one above it", value)
}

// indexedCheckBlock reads, under whatever locks its caller holds, what document_blocks holds for
// one check block: the version its words last changed in and their digest. A block the index no
// longer holds is no check any more. It reads and returns what it found; whether that still
// agrees with the resolution in hand is the locked half's judgement
// (recordResultUnderOwnerLocks), which compares the pair.
//
// The digest is document_blocks.text_sha256, the sha256 of the block's whole markdown as the
// index writes it (refs.BlockTextSum over markdown[From:To]), its opening line and that line's
// attributes included, so a check reworded from before_merge to after_merge, or given a
// ci_check, carries a digest of its own. The version is changed_version, which every version that
// changes the block's text moves, one that restores words an earlier version replaced included.
//
// Both are compared against the resolution's, though it is the version that catches every
// rewording, since changed_version moves on each of them. The digest is the backstop for a
// block whose text the index rewrote without that number moving, which no writer does today:
// refs.BlockIndexing.Queue writes both in one upsert. The two are read in one statement, so the
// pair costs the same read as either of them.
//
// Neither is a read of the live document, which never happens under these locks: that read is
// the slow one to hold the two issues' owner locks across (lockIssueOwners), since a document
// version landing between the resolve and the lock is exactly what leaves the read cache cold
// (docs/read_cache.go), so the one case it would answer differently is the one it would answer
// slowest in. The redo reads the words, with every lock given back
// (recordResultWithFreshWords).
func indexedCheckBlock(ctx context.Context, q queryer, check checkBlockRef, named string) (indexedCheck, error) {
	var indexed indexedCheck
	err := q.QueryRow(ctx, `
		select changed_version, text_sha256 from document_blocks
		where artifact_id = $1::uuid and block_id = $2 and type = 'check'
	`, check.ArtifactID, check.BlockID).Scan(&indexed.Version, &indexed.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexedCheck{}, unknownCheck(named)
	}
	if err != nil {
		return indexedCheck{}, fmt.Errorf("read the check %s of document %s: %w", check.BlockID, check.ArtifactID, err)
	}
	return indexed, nil
}

// checkBlockAttributes reads the attributes of one check block out of the live document. The
// index holds a block's type and digest but not its attributes, so the document itself is the one
// place they are written.
func (s *server) checkBlockAttributes(ctx context.Context, artifactID, blockID string) (docs.CheckAttributes, error) {
	// The index holds the block, so the live document has it too unless a write landed between
	// the two reads; such a block carries the schema's defaults alone, `when` before_merge.
	attributes := docs.CheckAttributes{When: docs.CheckWhenBeforeMerge}
	err := s.typedBlocksIn(ctx, artifactID, "check", false, func(node *pmdoc.Node, id, _ string) {
		if id == blockID {
			attributes = docs.CheckAttributesOf(node)
		}
	})
	if err != nil {
		return docs.CheckAttributes{}, err
	}
	return attributes, nil
}

// validCICheck reports whether check, a block the results read loaded with its results, is one
// CI proves: it carries ci_check and keeps the four rules of such a check, the three of
// docs.CICheckRule and that no result is recorded against it, which holdsRecordedResult answers
// from the rows the read loaded. Only such a check is CI's to the results routes; any other is
// an ordinary check, whatever its ci_check says.
func validCICheck(check model.CheckBlock, holdsRecordedResult bool) bool {
	attributes := docs.CheckAttributesOfBlock(check)
	return docs.CarriesCICheck(attributes) && docs.CICheckRule(attributes) == "" && !holdsRecordedResult
}

// refuseCheckProvedByCI refuses a result against a check CI proves (docs.CICheckRuleIn): 409
// CHECK_PROVED_BY_CI, naming the block and the check run that answers for it. Whether a result
// is recorded against the block is read through q, the pool before the transaction opens and the
// transaction itself under the locks, and only for a check that would otherwise be CI's.
func refuseCheckProvedByCI(ctx context.Context, q queryer, check resolvedCheck) error {
	if !docs.CarriesCICheck(check.Attributes) {
		return nil
	}
	rule, err := docs.CICheckRuleIn(ctx, q, check.ArtifactID, check.BlockID, check.Attributes)
	if err != nil || rule != "" {
		return err
	}
	return errorf(http.StatusConflict, "CHECK_PROVED_BY_CI",
		"check %s is proved by CI run %q: dispatch results reads its result from that run, and no role records one",
		check.BlockID, check.Attributes.CICheck)
}

// typedBlocksIn walks the live markdown of one document and hands visit each block of blockType
// in document order, with its block id. The markdown is read live and parsed; nothing is
// written. A rendering can repeat an id; the block index keys one row per id, and so does this,
// so only the first holder of an id is visited. Both readers of a live document's typed blocks
// come through here: the checks of a Verification document (checkBlocksIn) and the requirements
// of a spec (requirementBlocksIn).
//
// When trees is true, visit is handed as content the markdown the block's text_tree is read
// from, and "" otherwise. That markdown is blockTextContent of the block's text as the block
// index stores it: the block's slice of the document's markdown, between the byte range the
// document read itself gives for the block (docs.Service.TextWithBlocks), as refs.PlanBlocks
// cuts it. GET /api/v1/reference-targets has only that stored text to read a block's tree from,
// so the coverage and the results reads take the same text through the same rule, and one block
// answers one tree in all three reads, the whole stored text of a typed block in a quote included.
// The ranges come with the markdown from that one read, which serves both from the read cache
// without a render where it can (typedBlockMarkdown); a block the read gives no range for answers
// the block's own content (blockContent).
func (s *server) typedBlocksIn(ctx context.Context, artifactID, blockType string, trees bool, visit func(node *pmdoc.Node, id, content string)) error {
	markdown, texts, err := s.typedBlockMarkdown(ctx, artifactID, blockType, trees)
	if err != nil {
		return err
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return fmt.Errorf("read the %s blocks of document %s: %w", blockType, artifactID, err)
	}
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != blockType {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		if _, repeated := seen[id]; repeated {
			return true
		}
		seen[id] = struct{}{}
		content := ""
		if trees {
			if whole, held := texts[id]; held {
				content = blockTextContent(whole, true)
			} else {
				content = blockContent(node)
			}
		}
		visit(node, id, content)
		return true
	})
	return nil
}

// typedBlockMarkdown is the live markdown of one document and, when trees is true, the text the
// block index stores for each block of blockType in it, by id: the block's slice of that markdown
// between the byte range the same read gives for it (docs.Service.TextWithBlocks). The read lists
// blocks in document order, so the text kept for an id is its first holder's, the block
// typedBlocksIn visits. When trees is false the document is read with Text alone.
//
// A read with blocks that fails is logged, and the document is read again with Text and given no
// text at all, so every block answers its own content: a value inside a block that no block token
// can encode refuses TextWithBlocks and not Text, and such a document still answers its blocks'
// words. A failure Text meets as well is answered with Text's error.
func (s *server) typedBlockMarkdown(ctx context.Context, artifactID, blockType string, trees bool) (string, map[string]string, error) {
	if trees {
		markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, artifactID)
		if err == nil {
			texts := map[string]string{}
			for _, block := range blocks {
				if block.Type != blockType {
					continue
				}
				if _, held := texts[block.ID]; !held {
					texts[block.ID] = sliceMarkdown(markdown, block.From, block.To)
				}
			}
			return markdown, texts, nil
		}
		slog.Warn("dispatch: read a document's blocks for its typed blocks' trees", "artifact", artifactID, "type", blockType, "error", err)
	}
	markdown, err := s.deps.Docs.Text(ctx, artifactID)
	return markdown, nil, err
}

// checkBlocksIn is every `check` block of one document, in document order, with its attributes
// and its words (typedBlocksIn). trees is the request's own reader, nil for a caller that asked
// for no tree: each check registers with it, as its text_tree, the markdown typedBlocksIn reads
// the block's tree from (the rule GET /api/v1/reference-targets reads the same block by), once
// the slice has stopped growing, since what is registered is a pointer into the row.
func (s *server) checkBlocksIn(ctx context.Context, artifactID string, trees *displayTreeReader) ([]model.CheckBlock, error) {
	var checks []model.CheckBlock
	var contents []string
	err := s.typedBlocksIn(ctx, artifactID, "check", trees != nil, func(node *pmdoc.Node, _, content string) {
		check := docs.CheckBlockOf(node)
		check.ArtifactID, check.Text, check.Results = artifactID, blockWords(node), []model.CheckResult{}
		checks = append(checks, check)
		if trees != nil {
			contents = append(contents, content)
		}
	})
	if err != nil {
		return nil, err
	}
	for index, content := range contents {
		trees.text(&checks[index].TextTree, content)
	}
	return checks, nil
}

// checkBlocksOf is checkBlocksIn with each block's reference, issue and indexed version and
// digest filled in. The reference is built from the document's own slug, which is a property of
// the artifacts row and not of a block row, so a Verification document whose blocks the index
// does not hold yet still answers a reference every check can be addressed by.
func (s *server) checkBlocksOf(ctx context.Context, q queryer, artifact model.Artifact, trees *displayTreeReader) ([]model.CheckBlock, error) {
	checks, err := s.checkBlocksIn(ctx, artifact.ID, trees)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	issueKey := ""
	if artifact.IssueKey != nil {
		issueKey = *artifact.IssueKey
	}
	indexed := make(map[string]model.CheckBlock, len(checks))
	rows, err := q.Query(ctx, `
		select block_id, changed_version, text_sha256
		from document_blocks where artifact_id = $1 and type = 'check'
	`, artifact.ID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var block model.CheckBlock
		if err := rows.Scan(&block.BlockID, &block.Version, &block.SHA256); err != nil {
			return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
		}
		indexed[block.BlockID] = block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	for index := range checks {
		checks[index].IssueKey = issueKey
		checks[index].Ref, _ = text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: issueKey, ID: artifact.Slug, Block: checks[index].BlockID,
		})
		if block, ok := indexed[checks[index].BlockID]; ok {
			checks[index].Version, checks[index].SHA256 = block.Version, block.SHA256
		}
	}
	return checks, nil
}

// blockWords is a block's text as a reader sees it: every text node under it, joined by single
// blanks, with the text of a link and every word holding `://` (holdsLink) dropped. Dispatch reads
// the markdown as GFM does, so a bare `www.` address is link text and is dropped too, while a host
// written with neither a scheme nor `www.`, such as `example.com/logs`, stays
// (TestALineKeepsAPublicHostWithNoSchemeAsTheParseLeavesIt). It is a check's or a requirement's
// `text`, and the line of a check CI proves keeps a check's words by lineWords (ciVerdictLine).
// blockContent is the same block with both kept.
func blockWords(node *pmdoc.Node) string {
	var words []string
	pmdoc.Walk(node, func(child *pmdoc.Node) bool {
		if child.Type != "text" || child.Text == "" {
			return true
		}
		for _, mark := range child.Marks {
			if mark.Type == "link" {
				return true
			}
		}
		for _, word := range strings.Fields(child.Text) {
			if holdsLink(word) {
				continue
			}
			words = append(words, word)
		}
		return true
	})
	return strings.Join(words, " ")
}

// holdsLink reports whether a word holds `://`, which a dispatch:// reference and every URL written
// with a scheme and a host carry; a link written another way, such as GitHub's `www.` autolink,
// holds none. blockWords and lineWords drop such a word by this one rule.
func holdsLink(word string) bool {
	return strings.Contains(word, "://")
}

// lineWords is the rule a pull-request body line keeps the words of a text by: the reading a
// recorded result's line carries (checkResultLine) and the check's words a CI result's line
// carries (ciVerdictLine). The line is written into a pull request unchanged, and a pull request
// can be public, so it keeps only a plain word (plainWord), and drops every word that holds `://`
// (holdsLink), a private host (privateHost, tailnetHost), this Dispatch's own host, or an issue key
// of a project this Dispatch holds. No line is stored: each is written when the record, its dry run
// or the results read answers it, by the projects Dispatch holds then.
type lineWords struct {
	// issueKey matches a word holding an issue key of a project this Dispatch holds; nil when it
	// holds no project (lineWordsFor).
	issueKey *regexp.Regexp
	// host is the host name of this Dispatch's own URL in lower case, so a link to it written
	// without its scheme is dropped too; "" when the configured URL names no host (none is
	// configured, or it does not parse).
	host string
	// punctuation is the punctuation a plain word may hold: plainPunctuation, which lineWordsFor
	// sets, and which a test widens by one rune to show its property can fail.
	punctuation string
}

// plainPunctuation is the punctuation a word of a pull-request body line may hold besides visible
// letters and digits (plainWord).
// Runes: . , ; : ! ? ' " - ( ) /
// The line drops whole every word that holds any other rune: every other ASCII punctuation mark
// and symbol (`*`, `_` and `~` for emphasis and strikethrough, where `_X_` at a word's edge hides
// where a key ends; the backtick; `<` and `>` for HTML tags and comments; `[` and `]` for links;
// `\` for escapes; `&` for character references; `$` for math; `@` and `#` for mentions and
// references; and the rest), every Unicode punctuation mark and symbol (dashes and quotation marks
// among them), every combining mark, every number that is no decimal digit (superscripts,
// fractions, Roman numerals), every default-ignorable code point (the zero-width space, variation
// selectors and the Hangul filler letters among them), every control code point that is no white
// space (white space ends a word), and every private-use and unassigned code point. Many of these
// can split, join, hide or forge a key or a host once GitHub renders the line, which one word read
// alone cannot see; rather than list which, the rule allows these few runes and nothing else, which
// closes that class.
// Three look-alikes pass by design: a letter or digit of another script that looks like a Latin
// letter or digit, such as Cyrillic `А` in `АСМЕ-12`; a letter that looks like the key's `-`, such
// as the prolonged sound mark `ー` in `ACMEー12`; and GitHub's `:name:` emoji shortcodes, such as
// `:one:`, a picture that can look like a digit. None is made of the characters of a key.
// Excluding the letters and digits of other scripts would break international text, and excluding
// `:` would drop from the line every word of ordinary prose that ends in `:`.
// The cost is that a word such as `Vec<u8>`, `$PATH`, `ENVOY_TOKEN`, `a->b` or `it’s` leaves the
// line, while the record keeps the whole reading.
const plainPunctuation = `.,;:!?'"-()/`

// plainWord reports whether every rune of word is a visible letter, a digit or one of
// punctuation. A letter that is a default-ignorable code point, such as the Hangul fillers
// U+115F, U+1160, U+3164 and U+FFA0, which render as nothing, is no plain letter: it could sit
// inside a key unseen.
func plainWord(word, punctuation string) bool {
	for _, r := range word {
		letter := unicode.IsLetter(r) &&
			!unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector, unicode.Cf)
		if !letter && !unicode.IsDigit(r) && !strings.ContainsRune(punctuation, r) {
			return false
		}
	}
	return true
}

// privateHost and tailnetHost are the two host rules of .github/scripts/check-private-names.sh,
// read in any case as that script reads them: a host below `.internal.`, and a Tailscale MagicDNS
// host, a DNS label then `.ts.net` with no ASCII letter or digit after it. privateHost also takes
// a host that ends in `.internal` with no ASCII letter or digit after it (a full stop, a
// parenthesis or a hyphen included). Both read case-insensitively, so the Kelvin sign U+212A and
// the long s U+017F, which fold to `k` and `s`, count as letters after a host too, while `é` does
// not. privateHost's trailing `.internal` is what the script refuses only in a pull request's own
// text (check-pr-text.sh turns it on there), where it still lets metadata.google.internal and
// host.docker.internal pass: over the whole repository it reads no such ending, since those names
// are well known and not private there. The line is pasted into public text with no reader in
// between, so it is stricter than either on purpose. The line drops every word holding one,
// written without a scheme too, the two well-known names and the script's reserved example hosts
// with them, since no line needs such a host.
var (
	privateHost = regexp.MustCompile(`(?i)[[:alnum:]-]+[.]internal([^[:alnum:]]|$)`)
	tailnetHost = regexp.MustCompile(`(?i)[[:alnum:]-]+[.]ts[.]net([^[:alnum:]]|$)`)
)

// readLineWords is lineWords for every project q reads and for this Dispatch's own URL.
func (s *server) readLineWords(ctx context.Context, q queryer) (lineWords, error) {
	rows, err := q.Query(ctx, `select key from projects`)
	if err != nil {
		return lineWords{}, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return lineWords{}, err
	}
	return lineWordsFor(keys, s.deps.ServerURL), nil
}

// lineWordsFor is lineWords for the projects keys names and the Dispatch at serverURL. A word
// holds an issue key where one of those keys stands in it, in any case, at the word's start or
// after a character that is neither a letter nor a digit, followed by `-` and a number that ends
// there: at the word's end, or before a character that is neither a letter, a digit, nor a `.`
// that a digit follows. So `ACME-12's`, `(ACME-12)`, `fix/acme-12-lines` and
// `legion-acme-acme-12-tester` hold ACME-12, and the lower case is caught because branch names and
// role tokens spell a key so. `UTF-8`, `SHA-256`, `x86-64` and `TEST-1` hold no key while no
// project bears that name, and `kernel-6.8` holds none even where a project KERNEL exists, since a
// number running on into a decimal part is a version. A shape alone, such as [A-Z]+-[0-9]+, would
// drop UTF-8 and SHA-256 too, which is why the keys are the projects Dispatch holds.
func lineWordsFor(keys []string, serverURL string) lineWords {
	words := lineWords{punctuation: plainPunctuation}
	if parsed, err := url.Parse(serverURL); err == nil {
		words.host = strings.ToLower(parsed.Hostname())
	}
	if len(keys) == 0 {
		return words
	}
	quoted := make([]string, len(keys))
	for index, key := range keys {
		quoted[index] = regexp.QuoteMeta(key)
	}
	words.issueKey = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:` + strings.Join(quoted, "|") +
		`)-[0-9]+(?:$|[^\p{L}\p{N}.]|\.(?:$|[^0-9]))`)
	return words
}

// drops reports whether the line leaves word out: when it is not a plain word (plainWord), or
// when the word as written, or as NFKC reads it (fullwidth and mathematical letters as their
// plain forms), holds what the line never carries.
func (w lineWords) drops(word string) bool {
	return !plainWord(word, w.punctuation) || w.holds(word) || w.holds(norm.NFKC.String(word))
}

// holds reports whether word holds `://`, a private host, this Dispatch's own host or an issue
// key of a project this Dispatch holds.
func (w lineWords) holds(word string) bool {
	return holdsLink(word) || privateHost.MatchString(word) || tailnetHost.MatchString(word) ||
		(w.host != "" && strings.Contains(strings.ToLower(word), w.host)) ||
		(w.issueKey != nil && w.issueKey.MatchString(word))
}

// keep is text's words joined by single blanks, less every word the line drops.
func (w lineWords) keep(text string) string {
	return w.keepThrough(text, math.MaxInt)
}

// keepThrough is keep stopped once what it has kept is longer than budget runes. cut reads no more
// of a text than that (text.HeadWords reads its first budget runes and whether there are more), so
// a word past that point can change no line, and judging it would cost work that grows with the
// text and changes nothing.
func (w lineWords) keepThrough(text string, budget int) string {
	var kept strings.Builder
	runes := 0
	for word := range strings.FieldsSeq(text) {
		if runes > budget {
			break
		}
		if w.drops(word) {
			continue
		}
		if kept.Len() > 0 {
			kept.WriteByte(' ')
			runes++
		}
		kept.WriteString(word)
		runes += utf8.RuneCountInString(word)
	}
	return kept.String()
}

// blockContent is a typed block's own content as markdown: what stands between its opening line
// and its closing one, rendered from node as a document of its own, so neither the directive's
// line nor its attributes are in it and every link and every dispatch:// reference is. Its one
// use is the content typedBlocksIn hands visit for a block the document's read gives no byte
// range for. That is every block of a document whose read with blocks failed, which
// typedBlockMarkdown then reads with Text alone and gives no text for any block. Every other
// typed block's content is cut from its stored text (blockTextContent). node comes from the
// parse of the document's live rendering, which writes every typed block's id, so this
// rendering writes no id that the document does not hold. A block holding nothing has no
// content and so no tree; one whose content cannot be rendered apart from its document is logged
// and has none either, since the read's other fields do not depend on it.
func blockContent(node *pmdoc.Node) string {
	if len(node.Children) == 0 {
		return ""
	}
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: node.Children})
	if err != nil {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		slog.Warn("dispatch: render a typed block's content for its tree", "type", node.Type, "block", id, "error", err)
		return ""
	}
	return markdown
}

// checkResultLine is the pull-request body line one recorded result implies. Dispatch writes it
// rather than each caller, so every writer writes one grammar. Its reading carries no word
// holding `://` (holdsLink), no private or Dispatch host, no word that is not plain but for the
// ellipsis that marks a cut, and no word holding an issue key of a project Dispatch holds as
// lineWordsFor reads one (lineWords). A public host written as `www.` and a domain, which GitHub
// renders as a link, is kept, and the check's block id and the role are printed as written.
//
// The tail is the reading the role recorded (Observed) and not the check's own words, which
// every reader of the pull request can read on the issue and which a pass and a fail would
// carry alike: the line's whole purpose is to say what was seen. What words keeps of a reading
// is cut, when it is longer than the budget, and marked with an ellipsis, so a reader can tell a
// cut line from a short one; the cut falls at a word boundary where the budget's first runes hold
// one (cut). A result with no reading says so, since a line that simply stopped after the role
// would read as a reading nobody recorded, and a reading of which nothing is left after the cut
// says that, whether words kept nothing of it or the cut's second judging dropped what it kept.
func checkResultLine(check resolvedCheck, result model.CheckResult, words lineWords) string {
	line := fmt.Sprintf("- Check %s (%s): %s at %s, recorded by %s.",
		check.BlockID, check.Attributes.When, result.Verdict, result.CommitSHA, result.Role)
	if strings.TrimSpace(result.Observed) == "" {
		return line + " The role recorded no reading."
	}
	tail := words.cut(words.keepThrough(result.Observed, checkResultReadingRunes))
	if tail == "" {
		return line + " The reading holds only words this line leaves out."
	}
	return line + " " + tail
}

// cut is kept, the words w kept of a text, cut at a word boundary to checkResultReadingRunes with
// an ellipsis where it was cut, so a reader can tell a cut line from a short one
// (text.HeadWords). HeadWords cuts inside a word when the budget's first runes hold no blank, and
// what it keeps of that word can hold a key the whole word did not (`ACME-12xyz` cut to
// `ACME-12`), so a cut text is kept by w once more. cut is empty when nothing is left: when kept
// is, or when w keeps nothing of the cut text, so a caller decides that nothing is left from what
// cut answers and never prints a bare ellipsis.
func (w lineWords) cut(kept string) string {
	head, cut := text.HeadWords(kept, checkResultReadingRunes)
	if !cut {
		return head
	}
	if head = w.keep(head); head == "" {
		return ""
	}
	return head + "…"
}

// sessionOf is the session a result was recorded by, empty for a person's.
func sessionOf(actor model.Actor) string {
	if actor.Kind == "session" {
		return actor.ID
	}
	return ""
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// refuseResultScreenshots holds a result's screenshots to image documents of the recording
// issue or of the check's issue, named by their dispatch:// reference.
func (s *server) refuseResultScreenshots(ctx context.Context, q queryer, issueKey, checkIssueKey string, screenshots []string) error {
	for _, screenshot := range screenshots {
		ref, ok := text.ParseRef(screenshot, "")
		if !ok || ref.Kind != "artifact" || ref.IssueKey == "" ||
			(ref.IssueKey != issueKey && ref.IssueKey != checkIssueKey) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT",
				"screenshot %q names no image document of %s or %s", screenshot, issueKey, checkIssueKey)
		}
		var found bool
		err := q.QueryRow(ctx, `
			select true from artifacts where issue_key = $1 and slug = $2 and kind = 'image'
		`, ref.IssueKey, ref.ID).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT",
				"screenshot %q names no image document of %s or %s", screenshot, issueKey, checkIssueKey)
		}
		if err != nil {
			return fmt.Errorf("read screenshot %q: %w", screenshot, err)
		}
	}
	return nil
}

// insertCheckResult writes one result's row and fills in the created_at the row takes, which
// the record route answers with: the pointer is the caller's own value, not a copy of it.
func insertCheckResult(ctx context.Context, tx pgx.Tx, result *model.CheckResult) error {
	actorJSON, err := encodeJSON(result.Actor)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `
		insert into check_results (
			id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, surface, run, observed, commit_sha, negative_control,
			failed, skipped, excluded, verdict, screenshots, session, actor, event_seq, commit_repo
		)
		values ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		returning created_at
	`,
		result.ID, result.IssueKey, result.CheckIssueKey, result.CheckArtifactID, result.CheckBlockID,
		result.CheckVersion, result.CheckSHA256, result.Role, result.Surface, result.Run, result.Observed,
		result.CommitSHA, result.NegativeControl, result.Failed, result.Skipped, result.Excluded,
		result.Verdict, result.Screenshots, result.Session, actorJSON, result.EventSeq, result.CommitRepo,
	).Scan(&result.CreatedAt)
}

// loadCheckResults is every result recorded against the checks of one Verification document,
// from any issue, by the block id of the check each names, newest first within each. One query
// serves the whole document: a Verification document holding twenty checks is one round trip,
// not twenty.
func loadCheckResults(ctx context.Context, q queryer, artifactID string) (map[string][]model.CheckResult, error) {
	rows, err := q.Query(ctx, `
		select `+checkResultColumns+`
		from check_results
		where check_artifact_id = $1::uuid
		order by check_block_id, created_at desc, id desc
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
	}
	defer rows.Close()
	results := map[string][]model.CheckResult{}
	for rows.Next() {
		result, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
		}
		results[result.CheckBlockID] = append(results[result.CheckBlockID], result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
	}
	return results, nil
}

func scanCheckResult(row pgx.Row) (model.CheckResult, error) {
	var result model.CheckResult
	var actorJSON []byte
	if err := row.Scan(
		&result.ID, &result.IssueKey, &result.CheckIssueKey, &result.CheckArtifactID, &result.CheckBlockID,
		&result.CheckVersion, &result.CheckSHA256, &result.Role, &result.Surface, &result.Run,
		&result.Observed, &result.CommitSHA, &result.NegativeControl, &result.Failed, &result.Skipped,
		&result.Excluded, &result.Verdict, &result.Screenshots, &result.Session, &actorJSON,
		&result.CreatedAt, &result.EventSeq, &result.CommitRepo,
	); err != nil {
		return model.CheckResult{}, err
	}
	if err := json.Unmarshal(actorJSON, &result.Actor); err != nil {
		return model.CheckResult{}, fmt.Errorf("decode a result's actor: %w", err)
	}
	return result, nil
}

// readThroughPullRequests teaches Dispatch the pull requests GitHub names for a result's
// commit when that commit is the head of none it already knows for the check's issue, so a
// result recorded at the head of a pull request no event has reached Dispatch for stands at
// that head at once. It asks about the check's own issue and reads through repo, the one the
// commit is taken to be of: the linked repository refuseUnknownCommit answered before the write
// (results_linked.go), and else the repository the check's issue resolves to. So the pull
// request of a linked repository is learned from that repository, not from the one the issue
// resolves to, which does not hold the commit. A nil repo — a deployment with no App key, an
// issue resolving to no repository, or a resolve that failed — leaves nothing to read through,
// and it is answered before the database is read, since no head this read found could be used.
//
// It never fails the record: the row is written, and a head Dispatch could not learn leaves
// the result read exactly as it would have been without the call.
func (s *server) readThroughPullRequests(ctx context.Context, checkIssueKey, sha string, repo *string) {
	if repo == nil || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	known, err := s.deps.Attributor.HeadAttributedTo(ctx, s.deps.Store.Pool, checkIssueKey, sha)
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a recorded result", "issue", checkIssueKey, "error", err)
		return
	}
	if known {
		return
	}
	if _, _, err := s.deps.Attributor.ReadThroughCommitPulls(
		ctx, s.deps.Store.Pool, s.deps.GitHub, s.deps.Events, *repo, sha, time.Now()); err != nil {
		slog.Info("dispatch: GitHub named no pull request for a recorded result's commit",
			"repo", *repo, "commit", sha, "error", err)
	}
}
