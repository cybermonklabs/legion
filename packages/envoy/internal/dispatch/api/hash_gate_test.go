//go:build !race

package api

//ci:reads packages/contracts/fixtures/hash-gate/* TestHashGateEmitsDigestAndDropsSyntheticCarries

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The hash gate (R14, root R28): on a converted query document a document write commits in its
// batched window, appends artifact.version with digest_owed true and writes no carried approval;
// the fill after the COMMIT records the version's record digest and appends one artifact.digest
// saying whether the approval stands (ApprovalStand.Approves); and a person's review of a version
// whose digest is owed fills it first, so the review pins the record's digest.

// hashGateSpec is the spec of the issue the hash gate's test writes: a criterion, a requirement,
// and a paragraph and a decision the spec query does not take.
const hashGateSpec = ":::criterion{#k1}\nI want the urgent list.\n:::\n\n" +
	":::requirement{#r1}\nAn urgent ask comes first.\n:::\n\n" +
	"A paragraph the spec query does not take.\n\n" +
	":::decision{#d1}\nWe ship on Friday.\n:::\n"

// hashGateAsk is the ask block the test's spec takes through the edits route.
const hashGateAsk = ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"

// hashGateRig is a server on a traced pool holding one converted issue whose spec a person has
// approved, whose writes the test measures.
type hashGateRig struct {
	handler      http.Handler
	database     *store.Store
	service      *docs.Service
	tracer       *storetest.XidTracer
	issue        model.Issue
	spec         string
	verification string
	words        string
	counter      int
}

func newHashGateRig(t *testing.T) *hashGateRig {
	t.Helper()
	tracer := storetest.NewXidTracer()
	handler, database, deps := newTestServer(t, testServerOptions{tracer: tracer, settle: time.Hour, browserPeers: true})
	createRoomProject(t, handler, "HASH")
	issue, tree := createRoomIssue(t, handler, database, "HASH", "A converted issue under the hash gate", hashGateSpec, "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	rig := &hashGateRig{handler: handler, database: database, service: deps.Docs.(*docs.Service), tracer: tracer, issue: issue,
		spec: issue.PrimaryArtifactID, words: "A paragraph the spec query does not take."}
	settleIssue(t, rig.service, issue.Key)
	awaitSettled(t, database, rig.spec)
	if edited := editDocument(t, handler, rig.spec, insertAtEnd(hashGateAsk)); edited.Code != http.StatusOK {
		t.Fatalf("add the ask block to the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleIssue(t, rig.service, issue.Key)
	awaitSettled(t, database, rig.spec)
	uploaded := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	rig.verification = uploaded.Artifact.ID
	settleIssue(t, rig.service, issue.Key)
	awaitSettled(t, database, rig.verification)
	return rig
}

// measureHashGateWrite runs request under a marked context and holds every transaction it ran to
// storetest.XidBatchedWindow, the write's own and the fill's after it.
func (r *hashGateRig) measureHashGateWrite(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) {
	t.Helper()
	fixture := xidFixture{handler: r.handler, database: r.database, tracer: r.tracer}
	fixture.measureBatched(t, want, request)
}

// eventsOf reads the events of eventType whose payload names the document artifactID, oldest first.
func (r *hashGateRig) eventsOf(t *testing.T, eventType, artifactID string) []hashGateEvent {
	t.Helper()
	return loggedEvents(t, r.database, eventType, artifactID)
}

// versionEventOf is the artifact.version of version number of the document artifactID, and the
// artifact.digest events that name that version.
func (r *hashGateRig) versionEventOf(t *testing.T, artifactID string, number int) (version *hashGateEvent, digests []hashGateEvent) {
	t.Helper()
	for _, held := range r.eventsOf(t, "artifact.version", artifactID) {
		fields, _ := held.payload["version"].(map[string]any)
		if got, _ := fields["number"].(float64); int(got) == number {
			version = &held
		}
	}
	for _, held := range r.eventsOf(t, "artifact.digest", artifactID) {
		if got, _ := held.payload["version"].(float64); int(got) == number {
			digests = append(digests, held)
		}
	}
	return version, digests
}

// markdownDigest is the sha256 of version number of the document artifactID as stored.
func (r *hashGateRig) markdownDigest(t *testing.T, artifactID string, number int) string {
	t.Helper()
	sum := sha256.Sum256([]byte(versionMarkdownAt(t, r.database, artifactID, number)))
	return hex.EncodeToString(sum[:])
}

func (r *hashGateRig) approvalOf(t *testing.T, artifactID string) hashGateApproval {
	t.Helper()
	return artifactDigestApproval(t, r.handler, artifactID)
}

// counts is how many artifact.approved events, artifact.digest events and reviews the document
// artifactID holds.
func (r *hashGateRig) counts(t *testing.T, artifactID string) (approved, digests, reviews int) {
	t.Helper()
	return len(r.eventsOf(t, "artifact.approved", artifactID)), len(r.eventsOf(t, "artifact.digest", artifactID)), len(listReviews(t, r.handler, artifactID))
}

// requireDigestEvent fails, naming what, unless the version number of the document artifactID
// holds exactly one artifact.digest, after the version's artifact.version, whose members are the
// ones the fill writes: the record's digest, which is no stored markdown's, whether the approval
// of approved stands, and the version and the digest that approval stands on.
func (r *hashGateRig) requireDigestEvent(t *testing.T, what, artifactID string, number int, approved model.ArtifactReview, wantCurrent bool) {
	t.Helper()
	version, digests := r.versionEventOf(t, artifactID, number)
	if version == nil {
		t.Fatalf("%s: no artifact.version for %s@v%d", what, artifactID, number)
	}
	if len(digests) != 1 {
		t.Fatalf("%s: no artifact.digest for %s@v%d after its fill: %d events, want exactly one", what, artifactID, number, len(digests))
	}
	event := digests[0]
	record := versionDigestOf(t, r.database, artifactID, number)
	if event.id <= version.id {
		t.Errorf("%s: the artifact.digest of %s@v%d (event %d) comes before its artifact.version (event %d)", what, artifactID, number, event.id, version.id)
	}
	if record == r.markdownDigest(t, artifactID, number) {
		t.Errorf("%s: %s@v%d answers the digest of its stored markdown, %s, after its fill, want its record's", what, artifactID, number, record)
	}
	if got, _ := event.payload["content_sha256"].(string); got != record {
		t.Errorf("%s: the artifact.digest of %s@v%d carries content_sha256 %q, want the record's %s", what, artifactID, number, got, record)
	}
	if got, present := event.payload["approval_current"].(bool); !present || got != wantCurrent {
		t.Errorf("%s: the artifact.digest of %s@v%d carries approval_current %v, want %t", what, artifactID, number, event.payload["approval_current"], wantCurrent)
	}
	if got, _ := event.payload["approved_version"].(float64); int(got) != approved.Version {
		t.Errorf("%s: the artifact.digest of %s@v%d carries approved_version %v, want %d", what, artifactID, number, event.payload["approved_version"], approved.Version)
	}
	if got, _ := event.payload["approved_sha256"].(string); approved.ContentSHA256 == nil || got != *approved.ContentSHA256 {
		t.Errorf("%s: the artifact.digest of %s@v%d carries approved_sha256 %q, want the approval's %v", what, artifactID, number, got, approved.ContentSHA256)
	}
	if got, _ := event.payload["name"].(string); got == "" {
		t.Errorf("%s: the artifact.digest of %s@v%d names no document", what, artifactID, number)
	}
	if got, _ := event.payload["artifact_id"].(string); got != artifactID {
		t.Errorf("%s: the artifact.digest of %s@v%d names the document %q", what, artifactID, number, got)
	}
}

// requireWriteUnderApproval has alice approve the spec's newest version, runs request - a document
// write of status want - under the xid rig, and fails, naming what, unless the write committed in
// storetest.XidBatchedWindow (measure), appended one artifact.version with digest_owed true and
// the stored markdown's digest, wrote no review and no artifact.approved, and the fill appended
// the one artifact.digest of the version (requireDigestEvent). It answers the version written.
func (r *hashGateRig) requireWriteUnderApproval(t *testing.T, what string, want int, wantCurrent bool, request func(http.Handler) *httptest.ResponseRecorder) int {
	t.Helper()
	approved := reviewDocument(t, r.handler, r.spec, "alice", "approved", "")
	approvedBefore, _, reviewsBefore := r.counts(t, r.spec)
	latest := newestVersionOf(t, r.database, r.spec)
	r.measureHashGateWrite(t, want, request)
	number := newestVersionOf(t, r.database, r.spec)
	if number != latest+1 {
		t.Fatalf("%s: the write left version %d of the spec newest, want %d", what, number, latest+1)
	}
	version, _ := r.versionEventOf(t, r.spec, number)
	if version == nil {
		t.Fatalf("%s: no artifact.version for %s@v%d", what, r.spec, number)
	}
	if owed, _ := version.payload["digest_owed"].(bool); !owed {
		t.Errorf("%s: the artifact.version of %s@v%d carries digest_owed %v, want true", what, r.spec, number, version.payload["digest_owed"])
	}
	if got, _ := version.payload["content_sha256"].(string); got != r.markdownDigest(t, r.spec, number) {
		t.Errorf("%s: the artifact.version of %s@v%d carries content_sha256 %q, want the sha256 of its stored markdown, %s", what, r.spec, number, got, r.markdownDigest(t, r.spec, number))
	}
	approvedAfter, _, reviewsAfter := r.counts(t, r.spec)
	if approvedAfter != approvedBefore || reviewsAfter != reviewsBefore {
		t.Errorf("%s: the write left %d artifact.approved events and %d reviews of %s, want the %d and %d before it: it wrote a synthetic carried approval",
			what, approvedAfter, reviewsAfter, r.spec, approvedBefore, reviewsBefore)
	}
	r.requireDigestEvent(t, what, r.spec, number, approved, wantCurrent)
	read := r.approvalOf(t, r.spec)
	if read.Approval == nil || read.Approval.DigestOwed == nil || *read.Approval.DigestOwed || read.Approval.CurrentSHA256 == nil ||
		*read.Approval.CurrentSHA256 != versionDigestOf(t, r.database, r.spec, number) {
		t.Errorf("%s: GET /api/v1/artifacts/%s answers %+v after the fill, want digest_owed false and current_sha256 the record's digest of version %d", what, r.spec, read.Approval, number)
	}
	if wantCurrent && (read.Approval == nil || read.Approval.State != "approved" ||
		read.Approval.Version == nil || *read.Approval.Version != approved.Version || approved.Version >= read.Approval.LatestVersion) {
		t.Errorf("%s: approval %+v must stand at reviewed version %d, before latest version %d", what, read.Approval, approved.Version, number)
	}
	return number
}

// compareHashGateFixture checks the JSON members shared with Legion. The fixtures use stable
// sample IDs, digests and version numbers; substitutions bind those examples to this write.
// Other response members, such as authors and timestamps, are not part of this comparison.
func compareHashGateFixture(t *testing.T, name string, got map[string]any, bind map[any]any) {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/fixtures/hash-gate/" + name)
	if err != nil {
		t.Fatalf("read hash gate fixture %s: %v", name, err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode hash gate fixture %s: %v", name, err)
	}
	var compare func(string, map[string]any, map[string]any)
	compare = func(path string, want, got map[string]any) {
		for key, example := range want {
			actual, present := got[key]
			field := path + "." + key
			if !present {
				t.Errorf("%s is absent from Dispatch JSON", field)
				continue
			}
			if nested, ok := example.(map[string]any); ok {
				if object, ok := actual.(map[string]any); ok {
					compare(field, nested, object)
				} else {
					t.Errorf("%s = %v, want a JSON object", field, actual)
				}
				continue
			}
			if value, ok := bind[example]; ok {
				example = value
			}
			if !reflect.DeepEqual(actual, example) {
				t.Errorf("%s = %v, want %v", field, actual, example)
			}
		}
	}
	compare(name, want, got)
}

func (r *hashGateRig) requireWireFixtures(t *testing.T, number int) {
	t.Helper()
	const sampleArtifact = "00000000-0000-4000-8000-000000000001"
	bind := map[any]any{
		sampleArtifact:          r.spec,
		strings.Repeat("a", 64): versionDigestOf(t, r.database, r.spec, number),
		strings.Repeat("b", 64): r.markdownDigest(t, r.spec, number),
		float64(1):              float64(number - 1),
		float64(2):              float64(number),
	}
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/artifacts/"+r.spec, nil, "alice")
	succeeded(t, "read artifact for the hash gate fixture", response)
	artifact := decodeBody[map[string]any](t, response)
	compareHashGateFixture(t, "artifact.json", artifact, bind)
	approvedDigest := artifact["approval"].(map[string]any)["content_sha256"]
	owed := decodeBody[map[string]any](t, response)
	owedApproval := owed["approval"].(map[string]any)
	owedApproval["content_sha256"] = approvedDigest
	owedApproval["current_sha256"] = nil
	owedApproval["digest_owed"] = true
	owedBind := maps.Clone(bind)
	owedBind[strings.Repeat("b", 64)] = approvedDigest
	compareHashGateFixture(t, "artifact-owed.json", owed, owedBind)
	version, digests := r.versionEventOf(t, r.spec, number)
	if version == nil || len(digests) != 1 {
		t.Fatalf("version %d has artifact.version %v and %d artifact.digest events; want one of each", number, version, len(digests))
	}
	compareHashGateFixture(t, "artifact-version.json", map[string]any{"type": "artifact.version", "payload": version.payload}, bind)
	filled := maps.Clone(version.payload)
	filled["digest_owed"] = false
	compareHashGateFixture(t, "artifact-version-filled.json", map[string]any{"type": "artifact.version", "payload": filled}, bind)
	compareHashGateFixture(t, "artifact-digest.json", map[string]any{"type": "artifact.digest", "payload": digests[0].payload}, bind)
}

// edit is the request of the edits route that applies ops to the spec.
func (r *hashGateRig) edit(t *testing.T, ops ...map[string]any) func(http.Handler) *httptest.ResponseRecorder {
	return func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/edits", map[string]any{"ops": ops, "summary": "edit"}, "alice")
	}
}

func replaceOpOf(find, with string) map[string]any {
	return map[string]any{"op": "replace", "find": find, "with": with}
}

// nextWords is words the paragraph of the spec has not held yet, so each edit changes the spec.
func (r *hashGateRig) nextWords() (from, to string) {
	r.counter++
	from, to = r.words, fmt.Sprintf("A paragraph the spec query does not take, as edited %d times.", r.counter)
	r.words = to
	return from, to
}

// TestHashGateEmitsDigestAndDropsSyntheticCarries drives Dispatch's side of R14's hash gate on a
// converted issue (root R28, c-hash-gate). A prose edit, an ask-attribute rewrite, a reading and a
// restore of the approved text each commit in storetest.XidBatchedWindow (2 statements), emit
// artifact.version with digest_owed true and zero artifact.approved, and then artifact.digest from
// the fill with approval_current true. One write that adds a criterion and changes an ask attribute
// emits zero artifact.approved and artifact.digest with approval_current false. POST /reviews on a
// version whose digest is owed fills that digest before it records, so the review pins the record's
// digest. Recording a result on an approved Verification document leaves its approval current with
// zero carried review. GET /api/v1/artifacts/{id} answers current_sha256 and digest_owed.
func TestHashGateEmitsDigestAndDropsSyntheticCarries(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	rig := newHashGateRig(t)

	t.Run("a prose edit", func(t *testing.T) {
		from, to := rig.nextWords()
		number := rig.requireWriteUnderApproval(t, "a prose edit", http.StatusOK, true, rig.edit(t, replaceOpOf(from, to)))
		rig.requireWireFixtures(t, number)
	})
	t.Run("an ask-attribute rewrite", func(t *testing.T) {
		latest := newestVersionOf(t, rig.database, rig.spec)
		text := versionMarkdown(t, rig.handler, rig.spec, latest)
		rewritten := replacedOnce(t, text, `#a1 urgency="med"`, `#a1 urgency="high"`)
		base := uploadBase(t, rig.handler, rig.spec)
		rig.requireWriteUnderApproval(t, "an ask-attribute rewrite", http.StatusCreated, true, func(h http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+rig.issue.Key+"/artifacts",
				map[string]any{"name": "spec.md", "content": rewritten, "precondition": base}, "alice")
		})
	})
	t.Run("a reading", func(t *testing.T) {
		rig.requireWriteUnderApproval(t, "a reading", http.StatusOK, true,
			rig.edit(t, replaceOpOf("We ship on Friday.",
				"::::decision{#d1}\nWe ship on Friday.\n\n:::reading{#d1-r1}\nThe agent reads it as Friday.\n:::\n::::")))
	})
	t.Run("a restore of the approved text", func(t *testing.T) {
		approved := reviewDocument(t, rig.handler, rig.spec, "alice", "approved", "")
		text := versionMarkdown(t, rig.handler, rig.spec, approved.Version)
		// A write that moves the record leaves the approval stale; the restore then returns the
		// approved text, and the approval stands again by its digest alone.
		if moved := rig.edit(t, replaceOpOf("An urgent ask comes first.", "An urgent ask comes second."))(rig.handler); moved.Code != http.StatusOK {
			t.Fatalf("move the requirement: status=%d body=%s", moved.Code, moved.Body.String())
		}
		stale := newestVersionOf(t, rig.database, rig.spec)
		_, digests := rig.versionEventOf(t, rig.spec, stale)
		if len(digests) != 1 || digests[0].payload["approval_current"] != false {
			t.Fatalf("a restore of the approved text: the write that moved the requirement gave version %d the digest events %+v, want one with approval_current false", stale, digests)
		}
		request := requestApproval(t, rig.handler, rig.spec)
		base := uploadBase(t, rig.handler, rig.spec)
		approvedBefore, _, reviewsBefore := rig.counts(t, rig.spec)
		rig.measureHashGateWrite(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+rig.issue.Key+"/artifacts",
				map[string]any{"name": documentName(t, rig.database, rig.spec), "content": text, "precondition": base}, "alice")
		})
		restored := newestVersionOf(t, rig.database, rig.spec)
		if restored != stale+1 {
			t.Fatalf("a restore of the approved text: the upload left version %d newest, want %d", restored, stale+1)
		}
		version, _ := rig.versionEventOf(t, rig.spec, restored)
		if version == nil || version.payload["digest_owed"] != true {
			t.Errorf("a restore of the approved text: the artifact.version of %s@v%d is %+v, want digest_owed true", rig.spec, restored, version)
		}
		if approvedAfter, _, reviewsAfter := rig.counts(t, rig.spec); approvedAfter != approvedBefore || reviewsAfter != reviewsBefore {
			t.Errorf("a restore of the approved text: the write left %d artifact.approved events and %d reviews, want the %d and %d before it: it wrote a synthetic carried approval",
				approvedAfter, reviewsAfter, approvedBefore, reviewsBefore)
		}
		rig.requireDigestEvent(t, "a restore of the approved text", rig.spec, restored, approved, true)
		closed := readBlockAsk(t, rig.handler, request.ID)
		wantReason := fmt.Sprintf("No longer needed: spec.md version %d matches the standing approval of version %d.", restored, approved.Version)
		if closed.State != "resolved" || closed.Resolution == nil || closed.Resolution.Kind != "retracted" || closed.Resolution.Reason != wantReason {
			t.Errorf("the restore left the approval request %q with resolution %+v, want retracted with reason %q", closed.State, closed.Resolution, wantReason)
		}
		if counts := askEventCounts(t, rig.handler, rig.issue.Key, request.ID); counts["ask.resolved"] != 1 || counts["artifact.approved"] != 0 {
			t.Errorf("the restore's request events are %v, want one ask.resolved and no synthetic artifact.approved", counts)
		}
	})
	t.Run("a settlement that adds a criterion and changes an ask attribute", func(t *testing.T) {
		approved := reviewDocument(t, rig.handler, rig.spec, "alice", "approved", "")
		approvedBefore, _, reviewsBefore := rig.counts(t, rig.spec)
		latest := newestVersionOf(t, rig.database, rig.spec)
		peer := connectBrowserPeer(t, rig.handler, rig.service, rig.spec)
		defer peer.closeAndWait(t)
		peer.edit(t, func(tree *pmdoc.Node) error {
			criterion, err := pmdoc.Parse(":::criterion{#k2}\nI want the quiet list too.\n:::\n")
			if err != nil {
				return err
			}
			for _, block := range tree.Children {
				if block.Type == "ask" && block.Attrs[pmdoc.BlockIDAttr] == "a1" && block.Attrs["urgency"] != "low" {
					block.Attrs["urgency"] = "low"
					tree.Children = append(tree.Children, criterion.Children...)
					return nil
				}
			}
			return fmt.Errorf("the spec no longer holds ask a1 with an urgency to change")
		})
		peer.barrier(t)
		settleIssue(t, rig.service, rig.issue.Key)
		settled := newestVersionOf(t, rig.database, rig.spec)
		if settled != latest+1 {
			t.Fatalf("the settlement left version %d of the spec newest, want %d", settled, latest+1)
		}
		if version, _ := rig.versionEventOf(t, rig.spec, settled); version == nil || version.payload["digest_owed"] != true {
			t.Errorf("the settlement's artifact.version of %s@v%d is %+v, want digest_owed true", rig.spec, settled, version)
		}
		if approvedAfter, _, reviewsAfter := rig.counts(t, rig.spec); approvedAfter != approvedBefore || reviewsAfter != reviewsBefore {
			t.Errorf("the settlement left %d artifact.approved events and %d reviews, want %d and %d: it wrote a synthetic carried approval",
				approvedAfter, reviewsAfter, approvedBefore, reviewsBefore)
		}
		rig.requireDigestEvent(t, "a settlement that adds a criterion and changes an ask attribute", rig.spec, settled, approved, false)
	})
	t.Run("POST /reviews fills an owed digest before it records", func(t *testing.T) {
		defer rig.service.HoldFills(nil)
		hold := versionFillPause(t, rig.service, rig.issue.Key)
		from, to := rig.nextWords()
		done := versionFillEdit(t, rig.handler, rig.spec, from, to)
		testwait.Receive(t, "the fill of the edit's version", hold.held)
		number := newestVersionOf(t, rig.database, rig.spec)
		markdown := rig.markdownDigest(t, rig.spec, number)
		if row := versionFillRowOf(t, rig.database, rig.spec, number); !row.owed || row.digest != nil {
			t.Fatalf("version %d of the spec holds digest_owed %t and record %v while its fill is held, want it owed", number, row.owed, row.digest)
		}
		read := rig.approvalOf(t, rig.spec)
		if read.Approval == nil || read.Approval.DigestOwed == nil || !*read.Approval.DigestOwed || read.Approval.CurrentSHA256 == nil || *read.Approval.CurrentSHA256 != markdown {
			t.Errorf("GET /api/v1/artifacts/%s answers %+v while version %d's fill is held, want digest_owed true and current_sha256 the stored markdown's, %s", rig.spec, read.Approval, number, markdown)
		}
		approvedBefore, _, _ := rig.counts(t, rig.spec)
		review := reviewDocument(t, rig.handler, rig.spec, "alice", "approved", "")
		record := versionDigestOf(t, rig.database, rig.spec, number)
		if row := versionFillRowOf(t, rig.database, rig.spec, number); row.owed || row.digest == nil || *row.digest != record {
			t.Errorf("version %d of the spec holds digest_owed %t and record %v after the review, want the route to have filled it", number, row.owed, row.digest)
		}
		switch {
		case review.Version != number:
			t.Errorf("the review records version %d, want %d", review.Version, number)
		case review.ContentSHA256 == nil || *review.ContentSHA256 == markdown:
			t.Errorf("approval of %s@v%d pinned the markdown digest while its digest was owed: the review pins %v, want the record's digest %s", rig.spec, number, review.ContentSHA256, record)
		case *review.ContentSHA256 != record:
			t.Errorf("the review of %s@v%d pins %s, want the record's digest %s", rig.spec, number, *review.ContentSHA256, record)
		}
		if review.CarriedFromVersion != nil {
			t.Errorf("the review of %s@v%d is carried from version %d, want the person's own", rig.spec, number, *review.CarriedFromVersion)
		}
		approvals := rig.eventsOf(t, "artifact.approved", rig.spec)
		if len(approvals) != approvedBefore+1 {
			t.Errorf("the review left %d artifact.approved events, want the person's one beyond the %d before it", len(approvals), approvedBefore)
		} else if current, _ := approvals[len(approvals)-1].payload["approval_current"].(bool); !current {
			t.Errorf("the artifact.approved of the review carries approval_current %v, want true", approvals[len(approvals)-1].payload["approval_current"])
		}
		hold.release()
		if response := testwait.Receive(t, "the edit's answer", done); response.Code != http.StatusOK {
			t.Fatalf("the held edit answered %d: %s", response.Code, response.Body.String())
		}
		version, digests := rig.versionEventOf(t, rig.spec, number)
		if version == nil || len(digests) != 1 || len(approvals) != approvedBefore+1 {
			t.Fatalf("version %d holds artifact.version %v, %d digests and %d new approvals; want one of each", number, version, len(digests), len(approvals)-approvedBefore)
		}
		if approval := approvals[len(approvals)-1]; version.id >= digests[0].id || digests[0].id >= approval.id {
			t.Errorf("version %d event order: artifact.version %d, artifact.digest %d, artifact.approved %d; want strictly increasing", number, version.id, digests[0].id, approval.id)
		}
		read = rig.approvalOf(t, rig.spec)
		if read.Approval == nil || read.Approval.State != "approved" || read.Approval.DigestOwed == nil || *read.Approval.DigestOwed ||
			read.Approval.CurrentSHA256 == nil || *read.Approval.CurrentSHA256 != record {
			t.Errorf("GET /api/v1/artifacts/%s answers %+v after the review, want approved with digest_owed false and current_sha256 %s", rig.spec, read.Approval, record)
		}
	})
	t.Run("a result on an approved Verification document", func(t *testing.T) {
		approved := reviewDocument(t, rig.handler, rig.verification, "alice", "approved", "")
		approvedBefore, digestsBefore, reviewsBefore := rig.counts(t, rig.verification)
		latest := newestVersionOf(t, rig.database, rig.verification)
		digest := versionDigestOf(t, rig.database, rig.verification, latest)
		recorded := recordResult(t, rig.handler, rig.issue.Key, resultBody("k1", "pass", headCommit, nil))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a result on k1: status=%d body=%s", recorded.Code, recorded.Body.String())
		}
		result := decodeBody[recordedResult](t, recorded).Result
		requireStands := func(what string) {
			t.Helper()
			if newest := newestVersionOf(t, rig.database, rig.verification); newest != latest {
				t.Errorf("%s: the Verification document holds version %d, want version %d: the result moved no check, so no drained version", what, newest, latest)
			}
			approvedAfter, digestsAfter, reviewsAfter := rig.counts(t, rig.verification)
			if approvedAfter != approvedBefore || reviewsAfter != reviewsBefore || digestsAfter != digestsBefore {
				t.Errorf("%s: the Verification document holds %d artifact.approved events, %d artifact.digest events and %d reviews, want the %d, %d and %d before it: a result writes no carried review",
					what, approvedAfter, digestsAfter, reviewsAfter, approvedBefore, digestsBefore, reviewsBefore)
			}
			if got := versionDigestOf(t, rig.database, rig.verification, latest); got != digest {
				t.Errorf("%s: version %d of the Verification document answers the digest %s, want %s: a result moved the verification digest", what, latest, got, digest)
			}
			read := rig.approvalOf(t, rig.verification)
			if read.Approval == nil || read.Approval.State != "approved" || read.Approval.Version == nil || *read.Approval.Version != approved.Version ||
				read.Approval.DigestOwed == nil || *read.Approval.DigestOwed || read.Approval.CurrentSHA256 == nil || *read.Approval.CurrentSHA256 != digest {
				t.Errorf("%s: GET /api/v1/artifacts/%s answers %+v, want the approval of version %d standing (approval_current true) at digest %s", what, rig.verification, read.Approval, approved.Version, digest)
			}
		}
		requireStands("a recorded result")
		if retracted := retractAsPerson(t, rig.handler, result.ID, "recorded against the wrong commit"); retracted.Code != http.StatusCreated {
			t.Fatalf("retract the result: status=%d body=%s", retracted.Code, retracted.Body.String())
		}
		requireStands("a retracted result")
	})
	t.Run("a markdown document omits the hash gate flag", func(t *testing.T) {
		notes := uncoveredDocument(t, rig.handler, rig.issue.Key, "Some notes.\n")
		latest := newestVersionOf(t, rig.database, notes)
		agentEdit(t, rig.handler, notes, latest+1, replaceOp("Some notes.", "Updated notes."))
		if approval := rig.approvalOf(t, notes).Approval; approval == nil || approval.DigestOwed != nil {
			t.Errorf("the notes approval is %+v, want digest_owed absent on a non-query document", approval)
		}
		version, digests := rig.versionEventOf(t, notes, latest+1)
		if version == nil || len(digests) != 0 {
			t.Fatalf("notes emitted artifact.version %v and %d artifact.digest events; want a version alone", version, len(digests))
		}
		if value, present := version.payload["digest_owed"]; present {
			t.Errorf("notes artifact.version carries digest_owed %v, want it absent", value)
		}
	})
}
