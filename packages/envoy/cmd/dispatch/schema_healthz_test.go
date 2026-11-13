package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// schemaHealthWait bounds the wait for a running process to read a schema write: the write
// announces itself on the schema channel at commit, and the process's installer reads the store
// again on that wake, so this is the listener's round trip with room for a loaded pod.
const schemaHealthWait = 30 * time.Second

// A store schema whose version is above the one this binary was built for installs nothing,
// and the process that read it answers GET /healthz 503 with that version named as its reason,
// which is how the deployment learns to replace it (c-schema-holder). The store is written by
// the test alone while the process serves, as a deployment's newer pod would write it, so what
// is read is the server's own wake, its own installer and its own probe. Once the store is back
// at the binary's version the same process answers 200 again, with no reason.
//
// The probe answers 200 before anything is written, so a 503 afterwards is the write's and not
// a process that never served; and `db` reads true while it is 503, so the reason is the schema
// and not the database.
func TestAStoreSchemaAboveTheBuildAnswersHealthzWith503(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	server := startDispatchProcess(t, database.Pool.Config().ConnString())
	server.waitHealthy(t)
	built := pmdoc.BuiltSchemaVersion()
	if status, health := server.schemaHealth(t); status != http.StatusOK || !health.OK || health.NotReady != nil {
		t.Fatalf("/healthz before the store's schema moves = %d %+v, want 200 ok with no reason", status, health)
	}

	setStoreSchemaVersion(t, ctx, database.Pool, built+1)
	status, health := server.waitForSchemaHealth(t, "503 naming the store's schema version", func(status int, health schemaHealthBody) bool {
		return status == http.StatusServiceUnavailable
	})
	if health.OK || !health.DB {
		t.Errorf("/healthz with the store's schema above this build answers ok %v and db %v, want ok false with the database answering", health.OK, health.DB)
	}
	switch {
	case health.NotReady == nil:
		t.Errorf("/healthz answers %d with no not_ready reason, want the store's schema version %d named there", status, built+1)
	case !strings.Contains(*health.NotReady, strconv.Itoa(built+1)) || !strings.Contains(*health.NotReady, strconv.Itoa(built)):
		t.Errorf("/healthz answers %d because %q, want a reason naming the store's schema version %d and this build's %d",
			status, *health.NotReady, built+1, built)
	default:
		t.Logf("with the store's schema at version %d, /healthz answers %d because %q", built+1, status, *health.NotReady)
	}

	setStoreSchemaVersion(t, ctx, database.Pool, built)
	status, health = server.waitForSchemaHealth(t, "200 once the store is back at this build's version", func(status int, health schemaHealthBody) bool {
		return status == http.StatusOK
	})
	if !health.OK || health.NotReady != nil {
		t.Errorf("/healthz with the store back at version %d = %d %+v, want ok with no reason", built, status, health)
	}
}

// schemaHealthBody is the part of /healthz this test reads: whether the process says it can
// serve, whether its database answered, and the reason it gives when it cannot.
type schemaHealthBody struct {
	OK       bool    `json:"ok"`
	DB       bool    `json:"db"`
	NotReady *string `json:"not_ready"`
}

// schemaHealth reads /healthz once, bounded so a probe that hangs fails the test.
func (p *dispatchProcess) schemaHealth(t *testing.T) (int, schemaHealthBody) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
	if err != nil {
		t.Fatalf("build the /healthz request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	var health schemaHealthBody
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	return response.StatusCode, health
}

// waitForSchemaHealth reads /healthz until reached holds of an answer, and fails the test with
// the last answer and the process's output once schemaHealthWait has passed.
func (p *dispatchProcess) waitForSchemaHealth(t *testing.T, what string, reached func(int, schemaHealthBody) bool) (int, schemaHealthBody) {
	t.Helper()
	deadline := time.Now().Add(schemaHealthWait)
	for {
		status, health := p.schemaHealth(t)
		if reached(status, health) {
			return status, health
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz never answered %s within %v; last %d %+v:\n%s", what, schemaHealthWait, status, health, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// setStoreSchemaVersion moves the version every block type in the store carries, which moves
// the version the schema read from the store holds (schemanodes.Read takes the highest).
func setStoreSchemaVersion(t *testing.T, ctx context.Context, pool *store.Pool, version int) {
	t.Helper()
	changed, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = $2 and coalesce((attributes->>'block_type')::boolean, false)
	`, version, schemanodes.KindSupertag)
	if err != nil || changed.RowsAffected() == 0 {
		t.Fatalf("set the store's schema version to %d: %d row(s), %v", version, changed.RowsAffected(), err)
	}
}
