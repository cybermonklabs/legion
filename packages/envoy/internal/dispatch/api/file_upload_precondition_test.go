package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A file's or a picture's version carries a token, `sha256:` and the hash of its bytes, which an
// upload replacing it names as its precondition, as a document's upload names the document's:
// the tests of that token, apart from the documents' in upload_precondition_test.go.

// fileVersionTokenOf is the token a file's or a picture's version holding content carries: the
// SHA-256 of its bytes, written as a document token is.
func fileVersionTokenOf(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// uploadedFile is one file the upload tests replace: its name, the type it is sent as, its word
// in a refusal, and the bytes of three versions, each different from the others.
type uploadedFile struct {
	name, contentType, word string
	first, second, third    []byte
}

var uploadedFiles = []uploadedFile{
	{"notes.txt", "text/plain", "file", []byte("first\n"), []byte("second\n"), []byte("third\n")},
	{"diagram.png", "image/png", "picture", []byte("\x89PNG\r\n\x1a\nfirst"), []byte("\x89PNG\r\n\x1a\nsecond"), []byte("\x89PNG\r\n\x1a\nthird")},
}

// bytesOf is content of the file's kind holding text: a picture's begins with the PNG signature.
func (file uploadedFile) bytesOf(text string) []byte {
	if file.contentType == "image/png" {
		return []byte("\x89PNG\r\n\x1a\n" + text)
	}
	return []byte(text + "\n")
}

// uploadOwner is one owner a file is uploaded to: its artifacts route, and the lock every upload
// to that owner takes before it reads the version it replaces, which the race test holds. The
// project and the issue are createArtifactIssue's.
type uploadOwner struct {
	name string
	path func(issue model.Issue) string
	hold func(ctx context.Context, tx pgx.Tx, issue model.Issue) error
}

var uploadOwners = []uploadOwner{
	{
		"issue",
		func(issue model.Issue) string { return "/api/v1/issues/" + issue.Key + "/artifacts" },
		func(ctx context.Context, tx pgx.Tx, issue model.Issue) error {
			return store.LockOwner(ctx, tx, store.IssueOwner(issue.Key))
		},
	},
	{
		"project",
		func(model.Issue) string { return "/api/v1/projects/TEST/artifacts" },
		func(ctx context.Context, tx pgx.Tx, _ model.Issue) error {
			return lockProjectArtifacts(ctx, tx, "TEST")
		},
	},
	{
		"agent conversation",
		func(model.Issue) string { return "/api/v1/agents/" + agentSession + "/artifacts" },
		func(ctx context.Context, tx pgx.Tx, _ model.Issue) error {
			return lockAgentArtifacts(ctx, tx, agentSession)
		},
	},
}

// fileUpload is one case of the file token tests: a server of its own, the file, the owner it is
// uploaded to, and the file store beside the server (nil where the bytes stay in the row).
type fileUpload struct {
	handler  http.Handler
	database *store.Store
	file     uploadedFile
	owner    uploadOwner
	issue    model.Issue
	memory   *filestest.Memory
}

// upload is one multipart upload of content under the case's file name to its owner, naming
// token as the version it replaces, or naming none when token is empty.
func (c fileUpload) upload(t *testing.T, content []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]string{"name": c.file.name}
	if token != "" {
		fields["precondition"] = encodeUploadBase(t, map[string]string{"document": token})
	}
	return multipartRequest(t, c.handler, c.owner.path(c.issue), fields, c.file.name, c.file.contentType, content, "alice")
}

// create uploads the file's first version naming no token, and answers the artifact's id and
// that version's token as the artifact's read states it.
func (c fileUpload) create(t *testing.T) (string, string) {
	t.Helper()
	created := c.upload(t, c.file.first, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("first upload of %s = %d (want 201), body=%s", c.file.name, created.Code, created.Body.String())
	}
	artifactID := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact.ID
	versions := readVersions(t, c.handler, artifactID)
	if len(versions) != 1 || versions[0].Token == nil || *versions[0].Token != fileVersionTokenOf(c.file.first) {
		t.Fatalf("the read states versions %+v, want version 1 carrying token %s", versions, fileVersionTokenOf(c.file.first))
	}
	return artifactID, *versions[0].Token
}

// readVersions is the artifact's versions as its read states them.
func readVersions(t *testing.T, handler http.Handler, artifactID string) []model.Version {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", artifactID, read.Code, read.Body.String())
	}
	return decodeBody[model.Artifact](t, read).Versions
}

// forEachFileUpload runs check for each of uploadedFiles and each of uploadOwners, each on a
// server of its own, twice: once keeping the bytes in the version's row, and once beside a file
// store, whose objects check counts, since a file store is written before the upload's
// transaction opens (putUploadedFile).
func forEachFileUpload(t *testing.T, check func(t *testing.T, c fileUpload)) {
	for _, file := range uploadedFiles {
		for _, owner := range uploadOwners {
			for _, withStore := range []bool{false, true} {
				name := file.name + "/" + owner.name + "/in its row"
				if withStore {
					name = file.name + "/" + owner.name + "/in a file store"
				}
				t.Run(name, func(t *testing.T) {
					// The settle delay is stagedAhead, staged state for the settlement's timer and not a
					// wait, so no settlement runs between the row's requests however long they take.
					options := testServerOptions{settle: stagedAhead}
					var memory *filestest.Memory
					if withStore {
						memory = filestest.NewMemory()
						options.files = memory
					}
					handler, database, _ := newTestServer(t, options)
					issue := createArtifactIssue(t, handler)
					check(t, fileUpload{handler, database, file, owner, issue, memory})
				})
			}
		}
	}
}

// takenAndRefused sorts two answers into the one taken (201) and the one refused (409), failing
// on any other answer, or when the two are not one of each. Each answer is waited for through the
// shared wait (testwait.Receive), whose one bound is the test's give-up.
func takenAndRefused(t *testing.T, answers <-chan *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	var taken, refused []*httptest.ResponseRecorder
	for range 2 {
		answer := testwait.Receive(t, "an upload to answer", answers)
		switch answer.Code {
		case http.StatusCreated:
			taken = append(taken, answer)
		case http.StatusConflict:
			refused = append(refused, answer)
		default:
			t.Fatalf("an upload answered %d, want 201 or 409: %s", answer.Code, answer.Body.String())
		}
	}
	if len(taken) != 1 || len(refused) != 1 {
		t.Fatalf("%d uploads were taken and %d refused, want one of each", len(taken), len(refused))
	}
	return taken[0], refused[0]
}

// A file's or a picture's version carries a token, which the artifact's reads state beside it
// and `dispatch doc-read` prints: an upload replacing that version names it, as one replacing a
// document's does. The upload naming the version that stands is taken; one naming a version
// since replaced is refused 409 PRECONDITION_FAILED with the token and number of the version
// that stands, in words naming the artifact's kind, and writes nothing, its object in a file
// store included.
func TestFileUploadNamesTheTokenOfTheVersionItReplaces(t *testing.T) {
	t.Parallel()
	forEachFileUpload(t, func(t *testing.T, c fileUpload) {
		artifactID, firstToken := c.create(t)

		replaced := c.upload(t, c.file.second, firstToken)
		if replaced.Code != http.StatusCreated {
			t.Fatalf("upload naming the token of the version that stands = %d (want 201), body=%s", replaced.Code, replaced.Body.String())
		}
		second := decodeBody[struct {
			Version model.Version `json:"version"`
		}](t, replaced).Version
		if second.Number != 2 || second.Token == nil || *second.Token != fileVersionTokenOf(c.file.second) {
			t.Fatalf("the replacing upload answered version %+v, want version 2 carrying token %s", second, fileVersionTokenOf(c.file.second))
		}

		stale := c.upload(t, c.file.third, firstToken)
		if stale.Code != http.StatusConflict {
			t.Fatalf("upload naming the token of a replaced version = %d (want 409), body=%s", stale.Code, stale.Body.String())
		}
		conflict := decodeBody[uploadPreconditionConflict](t, stale)
		if conflict.Code != "PRECONDITION_FAILED" ||
			conflict.Current.Document != fileVersionTokenOf(c.file.second) ||
			conflict.Current.Version == nil || *conflict.Current.Version != 2 {
			t.Fatalf("the refusal = %s, want PRECONDITION_FAILED naming version 2 and its token %s", stale.Body.String(), fileVersionTokenOf(c.file.second))
		}
		if len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "document" ||
			conflict.Mismatches[0].Expected != firstToken ||
			conflict.Mismatches[0].Current == nil || *conflict.Mismatches[0].Current != fileVersionTokenOf(c.file.second) {
			t.Fatalf("the refusal's mismatches = %+v, want the one document mismatch from %s to %s", conflict.Mismatches, firstToken, fileVersionTokenOf(c.file.second))
		}
		if !strings.HasPrefix(conflict.Error, c.file.word+" upload precondition failed") ||
			!strings.Contains(conflict.Error, "read the "+c.file.word+" with `dispatch doc-read`") ||
			strings.Contains(conflict.Error, "document edit") || strings.Contains(conflict.Error, "read the document") {
			t.Fatalf("the refusal reads %q, want it to name the %s and its read with dispatch doc-read, and no document", conflict.Error, c.file.word)
		}
		if count := uploadedVersionCount(t, c.database, artifactID); count != 2 {
			t.Fatalf("%s holds %d versions after the refused upload, want 2", c.file.name, count)
		}
		if c.memory != nil && c.memory.Len() != 2 {
			t.Fatalf("the file store holds %d objects after the refused upload, want the two versions' alone", c.memory.Len())
		}
	})
}

// Two uploads naming the token of the one version that stands, sent together, are one write
// after the other under the owner's lock: the first is taken and the second, which reads the
// version the first wrote, is refused. The test holds that lock while both arrive and wait for it,
// so each has passed every check made before the lock, the file store's included, by the time
// either reads the version it replaces.
func TestTwoFileUploadsNamingOneTokenTakeOne(t *testing.T) {
	t.Parallel()
	forEachFileUpload(t, func(t *testing.T, c fileUpload) {
		artifactID, firstToken := c.create(t)
		ctx := context.Background()
		holder, err := c.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if err := c.owner.hold(ctx, holder, c.issue); err != nil {
			t.Fatalf("hold the %s's upload lock: %v", c.owner.name, err)
		}
		answers := make(chan *httptest.ResponseRecorder, 2)
		for _, content := range [][]byte{c.file.second, c.file.third} {
			go func() { answers <- c.upload(t, content, firstToken) }()
		}
		storetest.WaitForLockWaiters(t, holder, 2)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("release the lock: %v", err)
		}
		taken, refused := takenAndRefused(t, answers)
		won := decodeBody[struct {
			Version model.Version `json:"version"`
		}](t, taken).Version
		conflict := decodeBody[uploadPreconditionConflict](t, refused)
		if won.Number != 2 || won.Token == nil || conflict.Code != "PRECONDITION_FAILED" ||
			conflict.Current.Document != *won.Token || conflict.Current.Version == nil || *conflict.Current.Version != 2 {
			t.Fatalf("taken %+v and refused %s, want version 2 taken and the other refused naming it", won, refused.Body.String())
		}
		if count := uploadedVersionCount(t, c.database, artifactID); count != 2 {
			t.Fatalf("%s holds %d versions, want 2", c.file.name, count)
		}
	})
}

// fileRaceRounds is how many times TestTwoFileUploadsRacingOneTokenTakeOne races two uploads on
// one server: each round names the version the round before it wrote.
const fileRaceRounds = 4

// Two uploads naming one token, released together with nothing held, race as two clients do: on
// either storage path, whichever order the server runs them in, exactly one takes the next
// version and the other is refused 409 naming it. Each round names the token of the version the
// round before it wrote, so the rounds race on a fresh token each time.
func TestTwoFileUploadsRacingOneTokenTakeOne(t *testing.T) {
	t.Parallel()
	forEachFileUpload(t, func(t *testing.T, c fileUpload) {
		artifactID, token := c.create(t)
		for round := 1; round <= fileRaceRounds; round++ {
			start := make(chan struct{})
			answers := make(chan *httptest.ResponseRecorder, 2)
			for _, side := range []string{"left", "right"} {
				content := c.file.bytesOf(fmt.Sprintf("round %d, %s", round, side))
				go func() {
					<-start
					answers <- c.upload(t, content, token)
				}()
			}
			close(start)
			taken, refused := takenAndRefused(t, answers)
			won := decodeBody[struct {
				Version model.Version `json:"version"`
			}](t, taken).Version
			conflict := decodeBody[uploadPreconditionConflict](t, refused)
			if won.Number != round+1 || won.Token == nil || conflict.Current.Document != *won.Token ||
				conflict.Current.Version == nil || *conflict.Current.Version != round+1 {
				t.Fatalf("round %d: taken %+v and refused %s, want version %d taken and the other refused naming it", round, won, refused.Body.String(), round+1)
			}
			token = *won.Token
		}
		if count := uploadedVersionCount(t, c.database, artifactID); count != fileRaceRounds+1 {
			t.Fatalf("%s holds %d versions after %d rounds, want %d", c.file.name, count, fileRaceRounds, fileRaceRounds+1)
		}
	})
}

// A first upload of a file under a name its owner holds nothing under replaces no version, so a
// token it names names nothing: it is refused as a document's first upload naming one is, and
// nothing is written.
func TestFirstFileUploadNamingABaseIsRefused(t *testing.T) {
	t.Parallel()
	forEachFileUpload(t, func(t *testing.T, c fileUpload) {
		refused := c.upload(t, c.file.first, fileVersionTokenOf(c.file.first))
		if refused.Code != http.StatusConflict {
			t.Fatalf("a first upload naming a base = %d (want 409), body=%s", refused.Code, refused.Body.String())
		}
		conflict := decodeBody[uploadPreconditionConflict](t, refused)
		if conflict.Code != "PRECONDITION_FAILED" || !strings.Contains(conflict.Error, "has no token for an upload to name: send it again naming no token") {
			t.Fatalf("the refusal = %s, want PRECONDITION_FAILED asking for the upload again naming no token", refused.Body.String())
		}
		var artifacts int
		if err := c.database.Pool.QueryRow(context.Background(),
			`select count(*) from artifacts where name = $1`, c.file.name).Scan(&artifacts); err != nil {
			t.Fatalf("count the artifacts named %s: %v", c.file.name, err)
		}
		if artifacts != 0 {
			t.Fatalf("%d artifacts are named %s after the refused first upload, want none", artifacts, c.file.name)
		}
		if c.memory != nil && c.memory.Len() != 0 {
			t.Fatalf("the file store holds %d objects after the refused first upload, want none", c.memory.Len())
		}
	})
}

// A version stored with no hash carries no token: the reads state none, an upload naming any
// token is refused 409 as a first upload naming one is, and one naming none is taken.
func TestAFileVersionStoredWithNoHashCarriesNoToken(t *testing.T) {
	t.Parallel()
	forEachFileUpload(t, func(t *testing.T, c fileUpload) {
		artifactID, token := c.create(t)
		if _, err := c.database.Pool.Exec(context.Background(),
			`update artifact_versions set sha256 = null where artifact_id = $1`, artifactID); err != nil {
			t.Fatalf("clear the version's hash: %v", err)
		}
		if versions := readVersions(t, c.handler, artifactID); len(versions) != 1 || versions[0].Token != nil {
			t.Fatalf("the read states versions %+v, want version 1 carrying no token", versions)
		}
		refused := c.upload(t, c.file.second, token)
		if refused.Code != http.StatusConflict ||
			!strings.Contains(refused.Body.String(), "has no token for an upload to name: send it again naming no token") {
			t.Fatalf("an upload naming a token for a version with no hash = %d %s, want 409 asking for none", refused.Code, refused.Body.String())
		}
		if count := uploadedVersionCount(t, c.database, artifactID); count != 1 {
			t.Fatalf("%s holds %d versions after the refused upload, want 1", c.file.name, count)
		}
		if taken := c.upload(t, c.file.second, ""); taken.Code != http.StatusCreated {
			t.Fatalf("an upload naming no token = %d (want 201), body=%s", taken.Code, taken.Body.String())
		}
	})
}

// A document's version carries no file token, whatever its row holds: a document's token is its
// live tree's, which a hash in its version row does not change.
func TestADocumentVersionCarriesNoFileToken(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	before := readDocumentPrecondition(t, handler, artifactID)
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set sha256 = $2 where artifact_id = $1`, artifactID, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("give the document's version row a hash: %v", err)
	}
	for _, version := range readVersions(t, handler, artifactID) {
		if version.Token != nil {
			t.Fatalf("the document's version %d states token %s, want none", version.Number, *version.Token)
		}
	}
	if after := readDocumentPrecondition(t, handler, artifactID); after.Token != before.Token {
		t.Fatalf("the document's token moved from %s to %s, want it unchanged", before.Token, after.Token)
	}
}

// The reads that list a cited file with its versions state each version's token as the artifact's
// own read does: a document of another issue citing the file lists it among its outgoing
// references, and that issue's references list it among their members. The citing document is
// another issue's: the same citation from the file's own issue lists no member.
func TestReferenceReadsStateAFileVersionsToken(t *testing.T) {
	t.Parallel()
	handler, _ := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	file := uploadedFiles[0]
	c := fileUpload{handler: handler, file: file, owner: uploadOwners[0], issue: issue}
	_, token := c.create(t)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Citing issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the citing issue: status=%d body=%s", created.Code, created.Body.String())
	}
	citingIssue := decodeBody[model.Issue](t, created)
	citing := seedUploadedDocument(t, handler, citingIssue.Key, "cites.md", "# Cites\n\nSee dispatch://"+issue.Key+"/artifact/notes-txt for the notes.\n")

	for _, route := range []string{
		"/api/v1/artifacts/" + citing + "/references",
		"/api/v1/issues/" + citingIssue.Key + "/references",
	} {
		read := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", route, read.Code, read.Body.String())
		}
		if !strings.Contains(read.Body.String(), `"sha256":"`+strings.TrimPrefix(token, "sha256:")+`"`) {
			t.Fatalf("GET %s does not list %s's version: %s", route, file.name, read.Body.String())
		}
		if !strings.Contains(read.Body.String(), `"token":"`+token+`"`) {
			t.Fatalf("GET %s lists %s's version with no token %s: %s", route, file.name, token, read.Body.String())
		}
	}
}

// A file's upload is held to the precondition an upload can mean, as a document's is: the token
// of the version it replaces and nothing else.
func TestFileUploadNamingABlocksBaseIsRefused(t *testing.T) {
	t.Parallel()
	handler, _ := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	file := uploadedFiles[0]
	path := "/api/v1/issues/" + issue.Key + "/artifacts"
	if created := multipartRequest(t, handler, path, map[string]string{"name": file.name}, file.name, file.contentType, file.first, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("first upload of %s = %d (want 201), body=%s", file.name, created.Code, created.Body.String())
	}

	refused := multipartRequest(t, handler, path,
		map[string]string{"name": file.name, "precondition": `{"blocks":[{"id":"b1","token":"sha256:` + strings.Repeat("a", 64) + `"}]}`},
		file.name, file.contentType, file.second, "alice")
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("a file upload naming a blocks base = %d (want 400), body=%s", refused.Code, refused.Body.String())
	}
	if code := decodeBody[uploadPreconditionConflict](t, refused).Code; code != "INVALID_PRECONDITION" {
		t.Fatalf("the refusal's code = %q, want INVALID_PRECONDITION: %s", code, refused.Body.String())
	}
}

// unguardedFileReplacementSample is the sample line of the file counter in a /metrics body, ""
// when the body holds none, as unguardedReplacementSample reads the documents'.
func unguardedFileReplacementSample(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	for line := range strings.Lines(scrape(t, registry)) {
		if strings.HasPrefix(line, unguardedFileReplacementsMetric+" ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// A file replaced naming no token is taken unchecked, as a document replaced naming none is, and
// counted on its own counter and logged with the file it replaced, so the refusal that comes back
// for documents can cover files once every client sends a token. A replacement naming the
// standing token is checked and not counted, a first upload replaces nothing and is not counted,
// and a document's counter counts none of it. Serial: it sets the default logger.
func TestFileReplacementWithNoTokenIsTakenCountedAndLogged(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	registry := metrics.New()
	// The settle delay is stagedAhead, staged state for the settlement's timer and not a wait.
	handler, database, _ := newTestServer(t, testServerOptions{settle: stagedAhead, metrics: registry})
	issue := createArtifactIssue(t, handler)
	c := fileUpload{handler: handler, database: database, file: uploadedFiles[0], owner: uploadOwners[0], issue: issue}
	artifactID, token := c.create(t)
	if sample := unguardedFileReplacementSample(t, registry); sample != "" {
		t.Fatalf("after a first upload the counter reads %q, want no replacement counted", sample)
	}
	if guarded := c.upload(t, c.file.second, token); guarded.Code != http.StatusCreated {
		t.Fatalf("the replacement naming the standing token = %d (want 201), body=%s", guarded.Code, guarded.Body.String())
	}
	if unguarded := c.upload(t, c.file.third, ""); unguarded.Code != http.StatusCreated {
		t.Fatalf("the replacement naming no token = %d (want 201), body=%s", unguarded.Code, unguarded.Body.String())
	}
	if sample, want := unguardedFileReplacementSample(t, registry), unguardedFileReplacementsMetric+" 1"; sample != want {
		t.Fatalf("the file counter reads %q, want %q: the one replacement naming no token", sample, want)
	}
	if sample := unguardedReplacementSample(t, registry); sample != "" {
		t.Fatalf("the documents' counter reads %q, want no document replacement counted", sample)
	}
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, unguardedFileReplacementMessage) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "artifact="+artifactID) || !strings.Contains(lines[0], "file="+issue.Key+"/notes-txt") {
		t.Fatalf("the lines logging the replacement naming no token = %q, want one naming artifact %s and file %s/notes-txt", lines, artifactID, issue.Key)
	}
}
