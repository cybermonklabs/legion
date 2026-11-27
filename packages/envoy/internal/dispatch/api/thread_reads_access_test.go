package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// createdID is the id a create answered, failing the test unless the create succeeded.
func createdID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[struct {
		ID string `json:"id"`
	}](t, response)
	if created.ID == "" {
		t.Fatalf("create answered no id: %s", response.Body.String())
	}
	return created.ID
}
