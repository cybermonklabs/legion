package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The reads the hash gate's tests share: the events a document's writes and fills append, and the
// approval the artifact read answers. They build with and without the race detector, since the
// digest's tests that run under it read them too, while hash_gate_test.go, whose test measures its
// writes' xid windows, builds without it as the other xid window tests do (`//go:build !race`).

// documentName is the name of the document artifactID, which an upload over it names.
func documentName(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	var name string
	if err := database.Pool.QueryRow(context.Background(), `select name from artifacts where id = $1::uuid`, artifactID).Scan(&name); err != nil {
		t.Fatalf("read the name of %s: %v", artifactID, err)
	}
	return name
}

// blockIDOfText is the id of the block of the document artifactID whose text, trimmed, is text.
func blockIDOfText(t *testing.T, handler http.Handler, artifactID, text string) string {
	t.Helper()
	for held, id := range blockIDsByText(t, handler, artifactID) {
		if strings.TrimSpace(held) == text {
			return id
		}
	}
	t.Fatalf("the document %s holds no block reading %q", artifactID, text)
	return ""
}

// hashGateEvent is one event of the log as the test reads it: its id and its payload.
type hashGateEvent struct {
	id      int64
	payload map[string]any
}

// loggedEvents reads the events of eventType whose payload names the document artifactID, oldest
// first.
func loggedEvents(t *testing.T, database *store.Store, eventType, artifactID string) []hashGateEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select id, payload from events where type = $1 and payload->>'artifact_id' = $2 order by id`, eventType, artifactID)
	if err != nil {
		t.Fatalf("read the %s events of %s: %v", eventType, artifactID, err)
	}
	defer rows.Close()
	var read []hashGateEvent
	for rows.Next() {
		var held hashGateEvent
		var payload []byte
		if err := rows.Scan(&held.id, &payload); err != nil {
			t.Fatalf("read a %s event of %s: %v", eventType, artifactID, err)
		}
		if err := json.Unmarshal(payload, &held.payload); err != nil {
			t.Fatalf("decode a %s event of %s: %v", eventType, artifactID, err)
		}
		read = append(read, held)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s events of %s: %v", eventType, artifactID, err)
	}
	return read
}

// hashGateApproval is the artifact read's approval as the hash gate answers it.
type hashGateApproval struct {
	Approval *struct {
		State         string  `json:"state"`
		LatestVersion int     `json:"latest_version"`
		Version       *int    `json:"version"`
		ContentSHA256 *string `json:"content_sha256"`
		CurrentSHA256 *string `json:"current_sha256"`
		DigestOwed    *bool   `json:"digest_owed"`
	} `json:"approval"`
}

// artifactDigestApproval reads the approval GET /api/v1/artifacts/{id} answers of the document
// artifactID, with the digest it stands against and whether that digest is owed.
func artifactDigestApproval(t *testing.T, handler http.Handler, artifactID string) hashGateApproval {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read artifact %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	return decodeBody[hashGateApproval](t, response)
}
