package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The results write takes the owner locks of the recording issue and of the check's issue
// (lockIssueOwners), and those locks serialise every writer of either issue. It judges the
// result by the check as the block index holds it (results_settled_check.go): it resolves the
// check before its transaction opens, and reads the block's version, digest and text again under
// the locks, in one statement (settledCheckUnderLocks), so it reads no document while it holds
// them. This file holds the write to the check a document version leaves when that version lands
// between the resolve and the locks.

// heldWriteCommit is the commit a held write records at. No pull request of the rig's issue has
// it as its head, so the write asks GitHub whether the repository holds it (refuseUnknownCommit),
// after its resolve and before its locks.
const heldWriteCommit = "4444444444444444444444444444444444444444"

// heldWrite is one results write held between its resolve and its locks, in GitHub's answer to
// whether the repository holds its commit: a read the route makes with no transaction open.
type heldWrite struct {
	answered <-chan *httptest.ResponseRecorder
	release  func()
}

// holdWriteBeforeItsLocks starts the write of body on the rig's issue under token, and returns
// once that write is held in its read of heldWriteCommit: it has resolved the check and holds no
// lock. A write that answers instead fails the test, since nothing held it there. The hold ends
// when release is called, and at the latest when the test ends.
func holdWriteBeforeItsLocks(t *testing.T, rig *ciJobRig, token string, body any) heldWrite {
	t.Helper()
	asked, let := make(chan struct{}), make(chan struct{})
	var asking, letting sync.Once
	release := func() { letting.Do(func() { close(let) }) }
	t.Cleanup(release)
	rig.github.holdCommitReads(func(ctx context.Context, sha string) {
		if sha != heldWriteCommit {
			return
		}
		asking.Do(func() { close(asked) })
		select {
		case <-let:
		case <-ctx.Done():
		}
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() { answered <- rig.post(t, rig.issue, token, body) }()
	select {
	case <-asked:
		t.Logf("the write asked GitHub for its commit %s after it started", time.Since(started).Round(time.Millisecond))
	case response := <-answered:
		t.Fatalf("the write answered %d %s before it asked GitHub for its commit: nothing held it between its resolve and its locks",
			response.Code, response.Body.String())
	}
	return heldWrite{answered: answered, release: release}
}

// ciJobVerificationReworded is the rig's Verification document with k1's words changed and c1's
// left as they are.
var ciJobVerificationReworded = ciCheckBlock("c1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`) +
	"\n" + ":::check{#k1 negative_control=\"" + ciJobNegativeControl + "\"}\nThe server starts at the head.\n:::\n"

// writeVerification uploads markdown as a new version of the rig's Verification document. The
// upload indexes the version's blocks in its own transaction (refs.BlockIndexing.Queue), so the
// block index holds the version once the upload has answered, as it holds a settlement's: a
// browser's edit settled by the test would hold it only once the room's pending settlement was
// marked, which is not when the edit shows in the live text.
func writeVerification(t *testing.T, rig *ciJobRig, markdown string) {
	t.Helper()
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md", markdown)
}

// A results write that is held before its locks, in GitHub's answer to whether the repository
// holds its commit (refuseUnknownCommit), holds no owner lock of either issue across that read: no
// transaction holds the recording issue's owner lock while the read is held, and a PATCH of that
// issue, which takes the lock, answers then, since the read ends only once the test lets it go.
// The test judges whom the writer waits for, never how long it took (main's #4776): each wait is
// testwait.Receive, whose one bound is the test binary's deadline. The PATCH alone cannot fail the
// test: a transaction left idle across the held read is ended by the database's idle timeout, and
// the PATCH then answers. The lock table is read on a rig whose document service settles after an
// hour, since the settlement of the uploaded Verification document holds the issue's owner lock
// for a moment of its own.
func TestAResultsWriteHoldsNoOwnerLockWhileGitHubIsAskedForItsCommit(t *testing.T) {
	t.Parallel()
	rig := newCIJobRigSettling(t, time.Hour)
	held := holdWriteBeforeItsLocks(t, rig, rig.sessionToken(),
		resultBody("k1", "pass", heldWriteCommit, map[string]any{"role": "tester"}))

	if holder := readIssueOwnerLockHolder(t, rig.database, rig.issue); holder != nil {
		t.Fatalf("while the results write asks GitHub for its commit, %s's owner lock is held by %s: the write holds its issues' owner locks across that read",
			rig.issue, holder)
	}
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+rig.issue,
			map[string]any{"labels_add": []string{"waited"}}, "alice")
	}()
	patch := testwait.Receive(t, "a PATCH of "+rig.issue+" while the results write's read of GitHub is held", patched)
	held.release()
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH the issue beside the results write: status=%d body=%s", patch.Code, patch.Body.String())
	}

	response := testwait.Receive(t, "the results write to answer", held.answered)
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A version that rewords the check between the write's resolve and its locks is the check the
// write judges the result by and records: the version it wrote and the digest it left, not the
// ones the resolve read. The write reads them under its locks in one statement with the text
// they name, and reads no document while it holds those locks.
func TestAResultsWriteRecordsTheCheckAVersionLeftBetweenItsResolveAndItsLocks(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	resolved := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	held := holdWriteBeforeItsLocks(t, rig, rig.sessionToken(),
		resultBody("k1", "pass", heldWriteCommit, map[string]any{"role": "tester"}))

	writeVerification(t, rig, ciJobVerificationReworded)
	reworded := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	if reworded.Version == resolved.Version || reworded.SHA256 == resolved.SHA256 {
		t.Fatalf("fixture: the rewording left k1 at version %d digest %s, want both moved from %d and %s",
			reworded.Version, reworded.SHA256, resolved.Version, resolved.SHA256)
	}
	held.release()

	response := <-held.answered
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response).Result
	if result.CheckVersion != reworded.Version || result.CheckSHA256 != reworded.SHA256 {
		t.Errorf("the result records k1 at version %d digest %s, want version %d digest %s: the check as the version before the locks left it, not as the resolve read it (version %d digest %s)",
			result.CheckVersion, result.CheckSHA256, reworded.Version, reworded.SHA256, resolved.Version, resolved.SHA256)
	}
}

// A check whose words one version replaced and a later version restored, both between the
// write's resolve and its locks, is a check those locks find moved, though the digest they find
// is the one the resolve read: changed_version has moved to the restoring version. The write
// compares both, so it records the check at the version the locks found, with that version's
// digest. Comparing the digest alone would keep the reading the resolve made, and record the
// check at a version the block index no longer holds for it.
func TestAResultsWriteRecordsTheVersionALaterVersionRestoredTheDigestIn(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	resolved := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	held := holdWriteBeforeItsLocks(t, rig, rig.sessionToken(),
		resultBody("k1", "pass", heldWriteCommit, map[string]any{"role": "tester"}))

	writeVerification(t, rig, ciJobVerificationReworded)
	writeVerification(t, rig, ciJobVerification)
	restored := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	if restored.SHA256 != resolved.SHA256 || restored.Version == resolved.Version {
		t.Fatalf("fixture: the restore left k1 at version %d digest %s, want the digest %s back at a version other than %d",
			restored.Version, restored.SHA256, resolved.SHA256, resolved.Version)
	}
	held.release()

	response := <-held.answered
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response).Result
	if result.CheckVersion != restored.Version || result.CheckSHA256 != restored.SHA256 {
		t.Errorf("the result records k1 at version %d digest %s, want version %d digest %s: the version the locks found, not the one the resolve read (%d)",
			result.CheckVersion, result.CheckSHA256, restored.Version, restored.SHA256, resolved.Version)
	}
}
