package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// sessionReread asks the broker to reread name as a session, the caller the reread route answers
// once the reread is done, with whether the broker now serves the name, why not and as which type;
// a caller with no proof is answered 202 at once instead, before the reread reads the store. The
// session is one this helper writes straight into databaseURL, the broker's own database
// (brokertest.InsertSession), since nothing a row of this package holds can log a machine in to
// enrol one, and the proof names the public URL the binary derives from the address it bound
// (BROKER_PUBLIC_URL=http://127.0.0.1:0). It answers the status, the headers and the body as a
// JSON object, as post does.
func (p *brokerProcess) sessionReread(t *testing.T, databaseURL, name string) (int, http.Header, map[string]any) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	defer conn.Close(t.Context())
	enrollmentID, key := brokertest.InsertSession(t, conn, brokertest.RereaderOperator)
	url := "http://" + p.Addr + "/v1/secrets/" + name + "/reread"
	signed, err := proof.Sign(key, enrollmentID, http.MethodPost, url, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proof", signed)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s as a session: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, resp.Header, body
}
