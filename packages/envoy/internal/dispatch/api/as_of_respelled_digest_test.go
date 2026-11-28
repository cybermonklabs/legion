package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// TestARespelledChecksDigestAtACutReadsAsItReadThen: a check whose text another renderer version
// stored keeps its changed version when the next version writes it in this build's spelling, and
// its digest moves to that spelling (refs.BlockIndexing.KeepRespelled), so the present read gives
// the check's sha256 of this build's spelling from then on. A read of the results at a cut gives
// each check's sha256 as the read taken at that cut gave it, or names it absent in
// Dispatch-As-Of-Absent: at a cut before the respelling, and at a cut after it, before a later
// version rewords the check.
func TestARespelledChecksDigestAtACutReadsAsItReadThen(t *testing.T) {
	handler, database, served := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, nil)
	issue := issueInRepository(t, handler, "RSPD", "acme/widgets", "Respelled digest at a cut")
	owner := "/api/v1/issues/" + issue.Key
	uploaded := uploadDocument(t, handler, owner, "verification.md", respelledCheck)
	writeHead(t, database, "acme/widgets", 65, headCommit, "open", issue.Key)
	respellVerification(t, database, uploaded.Artifact.ID, uploaded.Version.Number)

	type taken struct {
		name string
		cut  asof.Cut
		then string
	}
	take := func(name string) taken {
		t.Helper()
		cut := settledResultsCut(t, database, served)
		return taken{name: name, cut: cut, then: checkOf(t, readResults(t, handler, issue.Key), "k1").SHA256}
	}
	before := take("before the respelling")
	uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, "A sibling check nobody reworded.", "A sibling check, reworded.", 1))
	after := take("after the respelling")
	if before.then == after.then {
		t.Fatalf("the present read gave check k1 the digest %s before and after the respelling, want the digest to move", before.then)
	}
	uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, respelledCheckParagraph, "The server records a result against a check, reworded.", 1))

	for _, cut := range []taken{before, after} {
		response := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, owner+"/results", cut.cut), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("the read at the cut %s: status=%d body=%s", cut.name, response.Code, response.Body.String())
		}
		read := decodeBody[issueResults](t, response)
		index := -1
		for number, check := range read.Checks {
			if check.BlockID == "k1" {
				index = number
			}
		}
		if index < 0 {
			t.Fatalf("the read at the cut %s holds no check k1: %s", cut.name, response.Body.String())
		}
		place := "/checks/" + strconv.Itoa(index) + "/sha256"
		if namesAbsent(response.Header().Get(contracts.DispatchAsOfAbsentHeader), place) {
			continue
		}
		if at := read.Checks[index].SHA256; at != cut.then {
			t.Errorf("at the cut %s check k1 reads sha256 %s, where the read taken then gave %s, and %s does not name %s",
				cut.name, at, cut.then, contracts.DispatchAsOfAbsentHeader, place)
		}
	}
}

// TestADigestAReindexChangedBeforeACutReadsAsItReadThenAfterALaterVersion: a re-index of a stored
// version in place (docs' indexOwedDocument) writes in this build's spelling a check whose text
// another renderer version recorded, keeps its changed version, records no text and moves its
// digest (refs.BlockIndexing.KeepRespelled). Where that re-index ran before a cut, the read at the
// cut gave the moved digest. A later version that rewords another check keeps the check's changed
// version under that digest. A read of the results at the cut, after that later version, gives the
// check's sha256 as the read taken then gave it, or names it in Dispatch-As-Of-Absent.
func TestADigestAReindexChangedBeforeACutReadsAsItReadThenAfterALaterVersion(t *testing.T) {
	handler, database, served := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, nil)
	issue := issueInRepository(t, handler, "RIDB", "acme/widgets", "Re-indexed before the cut")
	owner := "/api/v1/issues/" + issue.Key
	uploaded := uploadDocument(t, handler, owner, "verification.md", respelledCheck)
	document, number := uploaded.Artifact.ID, uploaded.Version.Number
	writeHead(t, database, "acme/widgets", 65, headCommit, "open", issue.Key)
	respellVerification(t, database, document, number)
	before := checkOf(t, readResults(t, handler, issue.Key), "k1").SHA256

	if _, err := database.Pool.Exec(t.Context(), `update document_block_versions set version = null where artifact_id = $1`, document); err != nil {
		t.Fatalf("owe the document its block index: %v", err)
	}
	if _, err := served.deps.Docs.(*docs.Service).ReconcileBlockIndex(t.Context(), docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("reconcile the block index: %v", err)
	}
	requireIndexedAt(t, database, document, number)
	then := checkOf(t, readResults(t, handler, issue.Key), "k1")
	if then.Version != number || then.SHA256 == before {
		t.Fatalf("after the re-index the present read gives check k1 version %d and sha256 %s, want version %d under a sha256 other than %s",
			then.Version, then.SHA256, number, before)
	}
	cut := settledResultsCut(t, database, served)

	later := uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, "A sibling check nobody reworded.", "A sibling check, reworded.", 1))
	requireIndexedAt(t, database, document, later.Version.Number)

	response := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, owner+"/results", cut), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the read at the cut: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[issueResults](t, response)
	index := -1
	for number, check := range read.Checks {
		if check.BlockID == "k1" {
			index = number
		}
	}
	if index < 0 {
		t.Fatalf("the read at the cut holds no check k1: %s", response.Body.String())
	}
	place := "/checks/" + strconv.Itoa(index) + "/sha256"
	absent := response.Header().Get(contracts.DispatchAsOfAbsentHeader)
	if at := read.Checks[index].SHA256; at != then.SHA256 && !namesAbsent(absent, place) {
		t.Errorf("after the later version, check k1 reads sha256 %s at the cut, where the read taken then gave %s, and %s %q does not name %s",
			at, then.SHA256, contracts.DispatchAsOfAbsentHeader, absent, place)
	}
}

// namesAbsent reports whether header, a Dispatch-As-Of-Absent value, names place, a JSON Pointer:
// a pointer of the header names it where each of its segments is place's or "*".
func namesAbsent(header, place string) bool {
	segments := strings.Split(place, "/")
	for _, pointer := range strings.Split(header, ",") {
		named := strings.Split(strings.TrimSpace(pointer), "/")
		if len(named) != len(segments) {
			continue
		}
		match := true
		for index := range named {
			if named[index] != "*" && named[index] != segments[index] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
