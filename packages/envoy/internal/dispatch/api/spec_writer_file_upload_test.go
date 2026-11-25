package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
)

// TestSpecWriteRefusesAWorkersFirstVerificationUploadedAsAFile: an issue that holds no
// Verification document takes its first verification.md only from a person or an architect of
// its tree (R1), whatever Content-Type the upload's file part carries. A multipart upload whose
// file part is text/plain is a file (artifactKind), and its slug verification-md is the one
// documentPlays reads as the issue's Verification document: this branch refuses such a file 400
// ARTIFACT_ROLE_NAME (fileUnderRoleNameRefusal), and the check judges it first
// (refuseFileUnderRoleName). So C's tester's file upload is refused with 403
// DOCUMENT_WRITER_NOT_ARCHITECT like its markdown upload, and writes nothing, and C's architect's
// Markdown upload of verification.md is then taken.
func TestSpecWriteRefusesAWorkersFirstVerificationUploadedAsAFile(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{noVerification: true})
	for _, contentType := range []string{"text/plain", "application/octet-stream"} {
		rig.requireRefused(t, specWriteAttempt{
			what: fmt.Sprintf("%s's first upload of %s's verification.md as a %s file", rig.cTester, rig.c.key, contentType),
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.uploadFile(t, rig.cTester, rig.c, "verification.md", contentType, "A worker's verification.\n")
			},
			refusal: rig.refusalOn(rig.c, "verification.md", rig.cTester),
		})
	}
	issue := decodeBody[struct {
		VerificationArtifactID string `json:"verification_artifact_id"`
	}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.c.key, nil, "alice"))
	if issue.VerificationArtifactID != "" {
		t.Errorf("%s answers verification_artifact_id %s after the refused uploads, want none", rig.c.key, issue.VerificationArtifactID)
	}
	if response := rig.upload(t, rig.cArchitect, rig.c, "verification.md", "C's architect's verification.\n"); response.Code != http.StatusCreated {
		t.Errorf("C's architect's first upload of %s's verification.md after the refused uploads: status=%d body=%s, want 201",
			rig.c.key, response.Code, clippedBody(response))
	}
}

// uploadFile uploads content as issue's artifact name in a multipart body whose file part carries
// contentType, on the shared token as session, which the actor field names.
func (rig *specWriterRig) uploadFile(t *testing.T, session string, issue specWriterIssue, name, contentType, content string) *httptest.ResponseRecorder {
	t.Helper()
	actor, err := json.Marshal(map[string]string{"kind": "session", "id": session})
	if err != nil {
		t.Fatalf("encode the actor: %v", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{"name": name, "actor": string(actor)} {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatalf("write the %s field: %v", field, err)
		}
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, name)},
		"Content-Type":        {contentType},
	})
	if err != nil {
		t.Fatalf("create the file part: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("write the file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish the multipart body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue.key+"/artifacts", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}
