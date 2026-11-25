// nats_hold_test.go drives the NATS writer hold's routes (/v1/nats-hold and the three under it)
// over HTTP on a test Postgres, with the operator's real token verifier: the auth service's
// platform-token verifier (natscallout.PlatformTokens) over two local issuers standing in for two
// clusters' issuers of BROKER_K8S_OIDC_ISSUER, first and second, and the operator entry
// BROKER_NATS_SERVICES lists beside one entry of every other kind, each naming no issuer and so
// taking the first alone. Every write is signed with the test's own Ed25519 key, the one the
// routes are given.
package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// holdNATSAudience is the audience the hold rig's platform tokens are minted for and verified at,
// as BROKER_NATS_K8S_AUDIENCE names it in a deployment.
const holdNATSAudience = "envoy-nats"

// The accounts of the hold rig's BROKER_NATS_SERVICES: the operator, and one entry of every other
// kind, whose tokens the routes must each refuse.
const (
	holdOperatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"
	holdListenerAccount = "system:serviceaccount:example-ns:example-listener"
	holdDispatchAccount = "system:serviceaccount:example-ns:example-dispatch"
	holdDaemonAccount   = "system:serviceaccount:example-ns:example-daemon"
	holdMeasureAccount  = "system:serviceaccount:example-ns:example-measure"
	holdProbeAccount    = "system:serviceaccount:envoy-operator:envoy-hold-probe"
)

// holdServices is the hold rig's BROKER_NATS_SERVICES.
var holdServices = strings.Join([]string{
	"operator/-/*=" + holdOperatorAccount,
	"listener/example-hub/*=" + holdListenerAccount,
	"dispatch/example-dispatch/*=" + holdDispatchAccount,
	"daemon/-/*=" + holdDaemonAccount,
	"measure/-/*=" + holdMeasureAccount,
	"probe/-/*=" + holdProbeAccount,
}, " ")

// holdRig is the hold's routes on an httptest server over a migrated schema of the test's own:
// the pool a test's admin reaches the tables through, the operator's signing key, the issuer
// that mints every entry's platform token, the first of the rig's BROKER_K8S_OIDC_ISSUER, and
// the second issuer, whose tokens no entry takes. restart mounts the routes afresh on the same
// tables, as a broker process that starts again does.
type holdRig struct {
	t            *testing.T
	url          string
	pool         *pgxpool.Pool
	key          ed25519.PrivateKey
	issuer       *oidctest.Issuer
	signer       *oidctest.Key
	second       *oidctest.Issuer
	secondSigner *oidctest.Key
	deps         api.Deps
	mux          atomic.Pointer[http.ServeMux]
}

func newHoldRig(t *testing.T) *holdRig {
	t.Helper()
	dsn := storetest.URL(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, second := oidctest.New(t), oidctest.New(t)
	signer, secondSigner := issuer.PublishKey(t, "nats-key"), second.PublishKey(t, "nats-key-second")
	issuers := []oidc.NamedIssuer{{Name: "first", URL: issuer.URL()}, {Name: "second", URL: second.URL()}}
	tokens := natscallout.PlatformTokens{K8sIssuers: issuers, K8s: enroll.K8sPodVerifier{Issuers: oidc.NewIssuerSet(issuers, holdNATSAudience, 0)}}
	services, err := config.ParseNATSServices(holdServices, issuers)
	if err != nil {
		t.Fatal(err)
	}
	var operator config.NATSService
	for _, entry := range services {
		if entry.Kind == config.NATSOperator {
			operator = entry
		}
	}
	rig := &holdRig{t: t, pool: pool, key: private, issuer: issuer, signer: signer, second: second, secondSigner: secondSigner, deps: api.Deps{NATSHold: api.NATSHold{
		Pool: pool, Schema: parsed.Query().Get("search_path"), Keys: []ed25519.PublicKey{public},
		OperatorToken: tokens.Verify, OperatorAccount: operator.Account, OperatorGoogle: operator.Google, OperatorIssuers: operator.Issuers,
	}}}
	rig.restart()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { rig.mux.Load().ServeHTTP(w, req) }))
	t.Cleanup(server.Close)
	rig.url = server.URL
	return rig
}

// restart mounts the hold's routes afresh on the rig's tables, as a broker process that starts
// again does: everything the routes kept in memory is gone, and every later request is served by
// the new routes.
func (r *holdRig) restart() {
	mux := http.NewServeMux()
	api.Register(mux, r.deps)
	r.mux.Store(mux)
}

// token is account's platform token for the NATS audience from the rig's first issuer, live for
// ten minutes, as `kubectl create token --duration 10m` mints the operator's.
func (r *holdRig) token(account string) string {
	return mintHoldToken(r.t, r.issuer, r.signer, account)
}

// secondToken is token from the rig's second issuer, which no entry of the rig names: the
// operator's account name in another cluster, which is another ServiceAccount.
func (r *holdRig) secondToken(account string) string {
	return mintHoldToken(r.t, r.second, r.secondSigner, account)
}

// mintHoldToken is account's platform token for the NATS audience from issuer, signed with key,
// live for ten minutes.
func mintHoldToken(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, account string) string {
	t.Helper()
	claims := issuer.Claims(account, holdNATSAudience)
	now := time.Now()
	claims["iat"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(10*time.Minute).Unix()
	return issuer.Mint(t, key, claims)
}

// holdAnswer is one answer of the hold's routes: its status and its JSON members.
type holdAnswer struct {
	status int
	fields map[string]json.RawMessage
	body   []byte
}

// str is the answer's string member name, "" for none.
func (a holdAnswer) str(name string) string {
	var s string
	_ = json.Unmarshal(a.fields[name], &s)
	return s
}

// num is the answer's number member name, -1 for none.
func (a holdAnswer) num(name string) int64 {
	n := int64(-1)
	if raw, ok := a.fields[name]; ok {
		_ = json.Unmarshal(raw, &n)
	}
	return n
}

// do sends method path with token as its bearer and body as its JSON, and answers what came back.
func (r *holdRig) do(method, path, token string, body any) holdAnswer {
	r.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			r.t.Fatal(err)
		}
	}
	return r.doRaw(method, path, token, raw)
}

// doRaw is do with raw as the body's bytes, none when raw is nil.
func (r *holdRig) doRaw(method, path, token string, raw []byte) holdAnswer {
	r.t.Helper()
	var payload io.Reader
	if raw != nil {
		payload = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, r.url+path, payload)
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answered, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	answer := holdAnswer{status: response.StatusCode, body: answered}
	if len(bytes.TrimSpace(answered)) > 0 {
		if err := json.Unmarshal(answered, &answer.fields); err != nil {
			r.t.Fatalf("%s %s answered %d with a body that is no JSON object: %s", method, path, response.StatusCode, answered)
		}
	}
	return answer
}

// sign signs lines with the operator's key.
func (r *holdRig) sign(lines []string) string {
	r.t.Helper()
	sig, err := signedrecord.Sign(r.key, lines)
	if err != nil {
		r.t.Fatal(err)
	}
	return sig
}

// signingTime is a signing time Postgres stores exactly: a timestamptz keeps microseconds.
func signingTime() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// moveBody is PUT /v1/nats-hold's body moving the row from expected to phase under holdID,
// signed over the gen it will store.
func (r *holdRig) moveBody(expected uint64, phase, holdID string) map[string]any {
	at := signingTime()
	return map[string]any{
		"expected_gen": expected, "phase": phase, "hold_id": holdID, "reason": "the test moves the row",
		"signed_at": at.Format(time.RFC3339Nano), "sig": r.sign(signedrecord.HoldRow(expected+1, phase, holdID, at)),
	}
}

// restoreBody is POST /v1/nats-hold/restore's body writing the row at gen, signed over it.
func (r *holdRig) restoreBody(gen uint64, phase, holdID string) map[string]any {
	at := signingTime()
	return map[string]any{
		"gen": gen, "phase": phase, "hold_id": holdID, "reason": "the test restores the row",
		"signed_at": at.Format(time.RFC3339Nano), "sig": r.sign(signedrecord.HoldRow(gen, phase, holdID, at)),
	}
}

// recordBody is PUT /v1/nats-hold/records' body writing value at expected, signed over the gen it
// will store.
func (r *holdRig) recordBody(holdID, kind, key string, expected uint64, value string) map[string]any {
	return map[string]any{
		"hold_id": holdID, "kind": kind, "key": key, "value": json.RawMessage(value), "expected_gen": expected,
		"sig": r.sign(signedrecord.Record(holdID, kind, key, expected+1, []byte(value))),
	}
}

// deletePath is DELETE /v1/nats-hold/records' path deleting holdID's record of kind and key at
// gen, with sig as its signature (none when sig is empty).
func deletePath(holdID, kind, key string, gen uint64, sig string) string {
	query := url.Values{"hold_id": {holdID}, "kind": {kind}, "key": {key}, "gen": {strconv.FormatUint(gen, 10)}}
	if sig != "" {
		query.Set("sig", sig)
	}
	return "/v1/nats-hold/records?" + query.Encode()
}

// deleteSig is the operator's signature of holdID's record of kind and key deleted at gen.
func (r *holdRig) deleteSig(holdID, kind, key string, gen uint64) string {
	return r.sign(signedrecord.RecordDelete(holdID, kind, key, gen))
}

// signedDelete is deletePath signed over gen.
func (r *holdRig) signedDelete(holdID, kind, key string, gen uint64) string {
	return deletePath(holdID, kind, key, gen, r.deleteSig(holdID, kind, key, gen))
}

// cutoverBody is PUT /v1/nats-hold/cutover's body, signed over holdGen.
func (r *holdRig) cutoverBody(family, state, holdID string, holdGen uint64) map[string]any {
	return map[string]any{
		"family": family, "state": state, "hold_id": holdID,
		"sig": r.sign(signedrecord.Cutover(family, state, holdID, holdGen)),
	}
}

// operator is the operator entry's token.
func (r *holdRig) operator() string { return r.token(holdOperatorAccount) }

// require fails t unless the answer has status and, when code is not empty, that error code.
func (a holdAnswer) require(t *testing.T, what string, status int, code string) holdAnswer {
	t.Helper()
	if a.status != status || (code != "" && a.str("error") != code) {
		t.Fatalf("%s = %d %s, want %d %s", what, a.status, a.body, status, code)
	}
	return a
}

// bootstrap writes the first row, released at gen 1, as `envoy-infra hold restore --bootstrap`
// does, and answers it.
func (r *holdRig) bootstrap() holdAnswer {
	r.t.Helper()
	return r.do(http.MethodPost, "/v1/nats-hold/restore", r.operator(), r.restoreBody(1, "released", "")).
		require(r.t, "the bootstrap restore", http.StatusOK, "")
}

// move PUTs the row from expected to phase under holdID as the operator, requiring 200 at
// expected+1, and answers the row.
func (r *holdRig) move(expected uint64, phase, holdID string) holdAnswer {
	r.t.Helper()
	answer := r.do(http.MethodPut, "/v1/nats-hold", r.operator(), r.moveBody(expected, phase, holdID)).
		require(r.t, "PUT "+phase+" "+holdID+" at "+strconv.FormatUint(expected, 10), http.StatusOK, "")
	if answer.num("gen") != int64(expected)+1 || answer.str("phase") != phase || answer.str("hold_id") != holdID {
		r.t.Fatalf("PUT %s %s at %d answered %s, want that row at gen %d", phase, holdID, expected, answer.body, expected+1)
	}
	return answer
}

// count is how many rows query counts.
func (r *holdRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// TestASecondHoldCanBeSetAfterARelease: a hold is set, held and released, and a second hold is
// set after it, each PUT at the gen the answer before it gave; a PUT at a stale gen is 409
// gen_mismatch and moves nothing, released to released is 400 bad_transition, and a listener
// entry's token is 403 not_operator. After the test's admin deletes the row, the restore makes it
// again above every logged gen, and a second restore is 409 row_exists. Fails if the gen check
// goes: the stale PUT lands.
func TestASecondHoldCanBeSetAfterARelease(t *testing.T) {
	rig := newHoldRig(t)
	rig.bootstrap()
	rig.move(1, "prepare", "hold-1")
	rig.move(2, "held", "hold-1")
	rig.move(3, "released", "hold-1")
	rig.move(4, "prepare", "hold-2")

	stale := rig.do(http.MethodPut, "/v1/nats-hold", rig.operator(), rig.moveBody(3, "released", "hold-2"))
	if stale.status != http.StatusConflict || stale.str("error") != "gen_mismatch" {
		t.Fatalf("a PUT at the stale gen 3 landed or was refused otherwise: %d %s, want 409 gen_mismatch", stale.status, stale.body)
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(stale.fields["current"], &current); err != nil || string(current["gen"]) != "5" {
		t.Fatalf("the stale PUT's refusal carries current %s, want the row at gen 5", stale.fields["current"])
	}
	if row := rig.do(http.MethodGet, "/v1/nats-hold", rig.operator(), nil); row.num("gen") != 5 || row.str("phase") != "prepare" {
		t.Fatalf("after the stale PUT the row is %s, want prepare at gen 5", row.body)
	}

	rig.move(5, "released", "hold-2")
	rig.do(http.MethodPut, "/v1/nats-hold", rig.operator(), rig.moveBody(6, "released", "hold-2")).
		require(t, "released to released", http.StatusBadRequest, "bad_transition")
	rig.do(http.MethodPut, "/v1/nats-hold", rig.token(holdListenerAccount), rig.moveBody(6, "prepare", "hold-3")).
		require(t, "a listener entry's PUT", http.StatusForbidden, "not_operator")
	if logged := rig.count(`select count(*) from nats_hold_log`); logged != 6 {
		t.Fatalf("nats_hold_log holds %d rows after the bootstrap and five moves, want 6", logged)
	}

	if _, err := rig.pool.Exec(context.Background(), `delete from nats_hold`); err != nil {
		t.Fatal(err)
	}
	restored := rig.do(http.MethodPost, "/v1/nats-hold/restore", rig.operator(), rig.restoreBody(7, "released", "hold-2")).
		require(t, "the restore after the admin's delete", http.StatusOK, "")
	if highest := rig.count(`select max(gen) from nats_hold_log where gen < 7`); restored.num("gen") <= int64(highest) {
		t.Fatalf("the restored row's gen is %d, want it above every earlier logged gen (%d)", restored.num("gen"), highest)
	}
	rig.do(http.MethodPost, "/v1/nats-hold/restore", rig.operator(), rig.restoreBody(8, "released", "hold-2")).
		require(t, "a second restore", http.StatusConflict, "row_exists")
}

// TestTheCutoverRecordIsWrittenOnlyUnderItsHold: under held with hold id H, a cutover PUT of
// session/done stores hold_gen equal to the row's gen, and a second PUT of session/rolled_back
// replaces it; the same PUT under prepare, or naming another hold id, is 409 hold_mismatch; a
// listener entry's token is 403; GET lists the row. Fails if the held check goes: the PUT under
// prepare lands.
func TestTheCutoverRecordIsWrittenOnlyUnderItsHold(t *testing.T) {
	rig := newHoldRig(t)
	rig.bootstrap()
	rig.move(1, "prepare", "hold-h")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.operator(), rig.cutoverBody("session", "done", "hold-h", 2)).
		require(t, "a cutover PUT under prepare", http.StatusConflict, "hold_mismatch")
	if stored := rig.count(`select count(*) from nats_cutover`); stored != 0 {
		t.Fatalf("the cutover PUT under prepare stored %d rows", stored)
	}

	rig.move(2, "held", "hold-h")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.operator(), rig.cutoverBody("session", "done", "hold-other", 3)).
		require(t, "a cutover PUT naming another hold id", http.StatusConflict, "hold_mismatch")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.token(holdListenerAccount), rig.cutoverBody("session", "done", "hold-h", 3)).
		require(t, "a listener entry's cutover PUT", http.StatusForbidden, "not_operator")

	done := rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.operator(), rig.cutoverBody("session", "done", "hold-h", 3)).
		require(t, "the cutover PUT under held", http.StatusOK, "")
	if done.num("hold_gen") != 3 || done.str("state") != "done" {
		t.Fatalf("the cutover PUT stored %s, want session done at hold_gen 3, the row's gen", done.body)
	}
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.operator(), rig.cutoverBody("session", "rolled_back", "hold-h", 3)).
		require(t, "the second cutover PUT", http.StatusOK, "")

	listed := rig.do(http.MethodGet, "/v1/nats-hold/cutover", rig.operator(), nil).require(t, "GET /v1/nats-hold/cutover", http.StatusOK, "")
	var families []map[string]any
	if err := json.Unmarshal(listed.fields["families"], &families); err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0]["family"] != "session" || families[0]["state"] != "rolled_back" ||
		families[0]["hold_id"] != "hold-h" || families[0]["hold_gen"] != float64(3) || families[0]["written_by"] != holdOperatorAccount {
		t.Fatalf("GET /v1/nats-hold/cutover lists %s, want the one session row, rolled_back under hold-h at hold_gen 3, written by the operator", listed.fields["families"])
	}
}

// TestThePrepareOperatorWritesTheCohort is the routes' half of the prepare operator's step: a
// records PUT of kind cohort under the current prepare hold id is stored, and one naming another
// hold id is 409 hold_mismatch and stores nothing.
func TestThePrepareOperatorWritesTheCohort(t *testing.T) {
	rig := newHoldRig(t)
	rig.bootstrap()
	rig.move(1, "prepare", "hold-p")
	const cohort = `{"consumers":["envoy-session-a","envoy-session-b"],"threshold":3}`
	stored := rig.do(http.MethodPut, "/v1/nats-hold/records", rig.operator(), rig.recordBody("hold-p", "cohort", "cohort", 0, cohort)).
		require(t, "the cohort PUT under the prepare hold id", http.StatusOK, "")
	if stored.num("gen") != 1 || stored.str("hold_id") != "hold-p" || stored.str("kind") != "cohort" {
		t.Fatalf("the cohort PUT answered %s, want the cohort record at gen 1", stored.body)
	}
	rig.do(http.MethodPut, "/v1/nats-hold/records", rig.operator(), rig.recordBody("hold-other", "cohort", "cohort", 0, cohort)).
		require(t, "a cohort PUT naming another hold id", http.StatusConflict, "hold_mismatch")
	listed := rig.do(http.MethodGet, "/v1/nats-hold/records?hold_id=hold-p&kind=cohort", rig.operator(), nil).
		require(t, "GET the cohort", http.StatusOK, "")
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(listed.fields["records"], &records); err != nil || len(records) != 1 {
		t.Fatalf("GET the cohort lists %s, want the one record", listed.fields["records"])
	}
	var value map[string]any
	if err := json.Unmarshal(records[0]["value"], &value); err != nil || value["threshold"] != float64(3) {
		t.Fatalf("the cohort's value is %s, want the value written", records[0]["value"])
	}
	if other := rig.count(`select count(*) from nats_hold_records where hold_id = 'hold-other'`); other != 0 {
		t.Fatalf("the PUT naming another hold id stored %d records", other)
	}
}

// A record moves only from the gen it names, a second insert of one is 409 gen_mismatch, and the
// cohort's delete, hold abort's, deletes the cohort alone, while the journal stays, and keeps its
// signed delete in nats_hold_record_deletes; a delete naming another hold id, or another kind,
// deletes nothing and keeps nothing.
func TestARecordMovesByItsGenAndTheAbortDeletesTheCohortAlone(t *testing.T) {
	rig := newHoldRig(t)
	rig.bootstrap()
	rig.move(1, "prepare", "hold-a")
	op := rig.operator()
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-a", "done", "hold", 0, `{"ops":[]}`)).require(t, "the journal's insert", http.StatusOK, "")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-a", "done", "hold", 0, `{"ops":[]}`)).
		require(t, "a second insert of the journal", http.StatusConflict, "gen_mismatch")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-a", "done", "hold", 2, `{"ops":[1]}`)).
		require(t, "a journal write at a gen it does not hold", http.StatusConflict, "gen_mismatch")
	moved := rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-a", "done", "hold", 1, `{"ops":[1]}`)).
		require(t, "the journal's write at its gen", http.StatusOK, "")
	if moved.num("gen") != 2 {
		t.Fatalf("the journal's write answered %s, want gen 2", moved.body)
	}
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-a", "cohort", "cohort", 0, `{"threshold":3}`)).require(t, "the cohort's insert", http.StatusOK, "")

	rig.do(http.MethodDelete, rig.signedDelete("hold-other", "cohort", "cohort", 1), op, nil).
		require(t, "a cohort delete naming another hold id", http.StatusConflict, "hold_mismatch")
	rig.do(http.MethodDelete, rig.signedDelete("hold-a", "done", "hold", 2), op, nil).
		require(t, "a delete of the journal", http.StatusBadRequest, "bad_request")
	deleteCohort := rig.signedDelete("hold-a", "cohort", "cohort", 1)
	rig.do(http.MethodDelete, deleteCohort, op, nil).require(t, "the cohort's delete", http.StatusNoContent, "")
	if cohorts, journals := rig.count(`select count(*) from nats_hold_records where kind = 'cohort'`),
		rig.count(`select count(*) from nats_hold_records where kind = 'done'`); cohorts != 0 || journals != 1 {
		t.Fatalf("after the cohort's delete %d cohort and %d journal records remain, want 0 and 1", cohorts, journals)
	}
	// The delete keeps the request's own signature, over the gen it deleted, for every replica's read.
	deleted, err := url.Parse(deleteCohort)
	if err != nil {
		t.Fatal(err)
	}
	var gen int64
	var sig string
	if err := rig.pool.QueryRow(context.Background(), `select gen, sig from nats_hold_record_deletes where hold_id = 'hold-a' and kind = 'cohort' and key = 'cohort'`).Scan(&gen, &sig); err != nil ||
		gen != 1 || sig != deleted.Query().Get("sig") {
		t.Fatalf("the cohort's delete kept gen %d and sig %q (%v), want gen 1 and the request's sig", gen, sig, err)
	}
	if kept := rig.count(`select count(*) from nats_hold_record_deletes`); kept != 1 {
		t.Fatalf("the refused deletes and the cohort's delete kept %d delete rows, want 1", kept)
	}
}

// The cohort's delete, hold abort's from prepare, is signed over the cohort's current gen and runs
// only while the row is prepare with the delete's hold id, or released with it while that hold's
// journal shows prepare (holdrecord's TestTheHoldJournal rows of a row lost in prepare). In held it
// is 409 hold_mismatch; with no signature it is 400 bad_signature; signed over an older gen it is
// refused, 400 bad_signature when it names the current gen and 409 gen_mismatch when it names the
// older one; each deletes nothing. Signed over the current gen in prepare, it deletes the cohort.
// Fails if the route deletes on the operator's token alone, or in held.
func TestTheCohortDeleteIsSignedAndRunsOnlyInPrepare(t *testing.T) {
	cohorts := func(rig *holdRig) int {
		return rig.count(`select count(*) from nats_hold_records where kind = 'cohort'`)
	}
	// prepared is a rig whose row is prepare under hold-d, with the cohort written twice: at gen 2.
	prepared := func(t *testing.T) *holdRig {
		rig := newHoldRig(t)
		rig.bootstrap()
		rig.move(1, "prepare", "hold-d")
		op := rig.operator()
		rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-d", "cohort", "cohort", 0, `{"threshold":3}`)).require(t, "the cohort's insert", http.StatusOK, "")
		rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-d", "cohort", "cohort", 1, `{"threshold":4}`)).require(t, "the cohort's write", http.StatusOK, "")
		return rig
	}
	t.Run("a signed delete in held is refused", func(t *testing.T) {
		rig := prepared(t)
		rig.move(2, "held", "hold-d")
		answer := rig.do(http.MethodDelete, rig.signedDelete("hold-d", "cohort", "cohort", 2), rig.operator(), nil)
		if answer.status != http.StatusConflict || answer.str("error") != "hold_mismatch" || cohorts(rig) != 1 {
			t.Errorf("a signed delete in held answered %d %s and left %d cohorts, want 409 hold_mismatch and the cohort", answer.status, answer.body, cohorts(rig))
		}
	})
	t.Run("an unsigned delete is refused", func(t *testing.T) {
		rig := prepared(t)
		answer := rig.do(http.MethodDelete, deletePath("hold-d", "cohort", "cohort", 2, ""), rig.operator(), nil)
		if answer.status != http.StatusBadRequest || answer.str("error") != "bad_signature" || cohorts(rig) != 1 {
			t.Errorf("an unsigned delete answered %d %s and left %d cohorts, want 400 bad_signature and the cohort", answer.status, answer.body, cohorts(rig))
		}
	})
	t.Run("a delete signed over an older gen is refused", func(t *testing.T) {
		rig := prepared(t)
		op := rig.operator()
		current := rig.do(http.MethodDelete, deletePath("hold-d", "cohort", "cohort", 2, rig.deleteSig("hold-d", "cohort", "cohort", 1)), op, nil)
		older := rig.do(http.MethodDelete, rig.signedDelete("hold-d", "cohort", "cohort", 1), op, nil)
		if current.status != http.StatusBadRequest || current.str("error") != "bad_signature" ||
			older.status != http.StatusConflict || older.str("error") != "gen_mismatch" || cohorts(rig) != 1 {
			t.Errorf("a delete of gen 2 signed over gen 1 answered %d %s, and one of gen 1 signed over it %d %s, leaving %d cohorts; want 400 bad_signature, 409 gen_mismatch and the cohort",
				current.status, current.body, older.status, older.body, cohorts(rig))
		}
	})
	t.Run("a delete signed over the current gen in prepare deletes the cohort", func(t *testing.T) {
		rig := prepared(t)
		rig.do(http.MethodDelete, rig.signedDelete("hold-d", "cohort", "cohort", 2), rig.operator(), nil).
			require(t, "the signed delete in prepare", http.StatusNoContent, "")
		if n := cohorts(rig); n != 0 {
			t.Fatalf("after the signed delete in prepare %d cohorts remain", n)
		}
	})
}

// A broker process keeps, for each hold record, the highest gen it has written or served, and
// answers a record below it as unknown, 409 below_floor, never the older record: a database writer
// who puts an older signed journal back is refused, and a write naming the put-back gen is 409
// gen_mismatch, since it would store a gen at or below the floor again. A broker that starts again
// has no floor and serves the older record: the residual the hold's design states, asserted here
// as such. Fails if the floor goes: the put-back journal is answered 200.
func TestARecordBelowTheFloorReadsAsUnknown(t *testing.T) {
	rig := newHoldRig(t)
	op := rig.operator()
	rig.bootstrap()
	rig.move(1, "prepare", "hold-f")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-f", "done", "hold", 0, `{"ops":[]}`)).require(t, "the journal's insert", http.StatusOK, "")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-f", "done", "hold", 1, `{"ops":[1]}`)).require(t, "the journal's write at gen 2", http.StatusOK, "")
	var older, olderSig string
	if err := rig.pool.QueryRow(context.Background(), `select value::text, sig from nats_hold_records where hold_id = 'hold-f' and kind = 'done'`).Scan(&older, &olderSig); err != nil {
		t.Fatal(err)
	}
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-f", "done", "hold", 2, `{"ops":[1,2]}`)).require(t, "the journal's write at gen 3", http.StatusOK, "")
	if _, err := rig.pool.Exec(context.Background(), `update nats_hold_records set value = $1::jsonb, gen = 2, sig = $2 where hold_id = 'hold-f' and kind = 'done'`, older, olderSig); err != nil {
		t.Fatal(err)
	}
	const journal = "/v1/nats-hold/records?hold_id=hold-f&kind=done"
	read := rig.do(http.MethodGet, journal, op, nil)
	write := rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-f", "done", "hold", 2, `{"ops":[1,9]}`))
	if read.status != http.StatusConflict || read.str("error") != "below_floor" || write.status != http.StatusConflict || write.str("error") != "gen_mismatch" {
		t.Fatalf("with the signed gen-2 journal put back after gen 3, GET answered %d %s and a write at gen 2 %d %s; want 409 below_floor and 409 gen_mismatch",
			read.status, read.body, write.status, write.body)
	}
	if stored := rig.count(`select gen from nats_hold_records where hold_id = 'hold-f' and kind = 'done'`); stored != 2 {
		t.Fatalf("the refused write moved the journal to gen %d", stored)
	}
	// The residual: a broker process that starts again has no floor, and serves the older journal.
	rig.restart()
	again := rig.do(http.MethodGet, journal, op, nil).require(t, "GET the put-back journal after a restart", http.StatusOK, "")
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(again.fields["records"], &records); err != nil || len(records) != 1 || string(records[0]["gen"]) != "2" {
		t.Fatalf("after a restart GET lists %s, want the put-back journal at gen 2 (the stated residual)", again.fields["records"])
	}
}

// A record's floor rises only from a record whose signature verifies: an unsigned record a
// database writer stores at gen 999999 raises nothing (its read is the client's to refuse, by its
// signature), so the genuine record, put back, still reads afterwards. Fails if the floor rises from
// what the GET read: the genuine record reads 409 below_floor.
func TestAnUnverifiedRecordRaisesNoFloor(t *testing.T) {
	rig := newHoldRig(t)
	op := rig.operator()
	rig.bootstrap()
	rig.move(1, "prepare", "hold-v")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-v", "done", "hold", 0, `{"ops":[]}`)).require(t, "the journal's insert", http.StatusOK, "")
	var genuine, genuineSig string
	if err := rig.pool.QueryRow(context.Background(), `select value::text, sig from nats_hold_records where hold_id = 'hold-v' and kind = 'done'`).Scan(&genuine, &genuineSig); err != nil {
		t.Fatal(err)
	}
	const journal = "/v1/nats-hold/records?hold_id=hold-v&kind=done"
	if _, err := rig.pool.Exec(context.Background(), `update nats_hold_records set gen = 999999, sig = '' where hold_id = 'hold-v' and kind = 'done'`); err != nil {
		t.Fatal(err)
	}
	rig.do(http.MethodGet, journal, op, nil).require(t, "GET the unsigned journal at gen 999999", http.StatusOK, "")
	if _, err := rig.pool.Exec(context.Background(), `update nats_hold_records set value = $1::jsonb, gen = 1, sig = $2 where hold_id = 'hold-v' and kind = 'done'`, genuine, genuineSig); err != nil {
		t.Fatal(err)
	}
	again := rig.do(http.MethodGet, journal, op, nil)
	var records []map[string]json.RawMessage
	if again.status != http.StatusOK || json.Unmarshal(again.fields["records"], &records) != nil || len(records) != 1 || string(records[0]["gen"]) != "1" {
		t.Fatalf("after an unsigned record at gen 999999, the genuine journal put back answered %d %s, want 200 and the record at gen 1", again.status, again.body)
	}
}

// A record the floor has seen reads as unknown once it is absent: a database writer's delete is no
// route's, so GET answers 409 below_floor rather than an empty list, which the client would read
// as no record. A record the route deleted (the cohort's, at the gen it signed) is absent as it
// should be. Fails if the floor checks only the records present: the deleted journal reads as
// absent.
func TestASeenRecordDeletedOutsideTheRoutesReadsAsUnknown(t *testing.T) {
	rig := newHoldRig(t)
	op := rig.operator()
	rig.bootstrap()
	rig.move(1, "prepare", "hold-t")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-t", "done", "hold", 0, `{"ops":[]}`)).require(t, "the journal's insert", http.StatusOK, "")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-t", "cohort", "cohort", 0, `{"threshold":3}`)).require(t, "the cohort's insert", http.StatusOK, "")
	const journal = "/v1/nats-hold/records?hold_id=hold-t&kind=done"
	rig.do(http.MethodGet, journal, op, nil).require(t, "GET the journal", http.StatusOK, "")
	if _, err := rig.pool.Exec(context.Background(), `delete from nats_hold_records where hold_id = 'hold-t' and kind = 'done'`); err != nil {
		t.Fatal(err)
	}
	gone := rig.do(http.MethodGet, journal, op, nil)
	if gone.status != http.StatusConflict || gone.str("error") != "below_floor" {
		t.Fatalf("the seen journal deleted by SQL answered %d %s, want 409 below_floor", gone.status, gone.body)
	}
	rig.do(http.MethodDelete, rig.signedDelete("hold-t", "cohort", "cohort", 1), op, nil).require(t, "the cohort's signed delete", http.StatusNoContent, "")
	cohort := rig.do(http.MethodGet, "/v1/nats-hold/records?hold_id=hold-t&kind=cohort", op, nil).require(t, "GET the deleted cohort", http.StatusOK, "")
	if string(cohort.fields["records"]) != "[]" {
		t.Fatalf("the cohort the route deleted lists %s, want none", cohort.fields["records"])
	}
}

// holdTables is every row of the hold's five tables, as text, for a test that requires a refused
// call to change none.
func (r *holdRig) holdTables() string {
	r.t.Helper()
	var out strings.Builder
	for _, table := range []string{"nats_hold", "nats_hold_log", "nats_hold_records", "nats_hold_record_deletes", "nats_cutover"} {
		rows, err := r.pool.Query(context.Background(), `select t::text from `+table+` t order by 1`)
		if err != nil {
			r.t.Fatal(err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				r.t.Fatal(err)
			}
			out.WriteString(table + " " + row + "\n")
		}
		rows.Close()
	}
	return out.String()
}

// TestOnlyTheOperatorWritesTheHold: every route of the hold, called with the token of a listener,
// dispatch, daemon, measure and probe entry, with the operator's account's token from the second
// issuer, which the operator entry does not name, and with a person's broker access token, is 403
// not_operator and changes no row, though every write it sends is signed by the operator's key
// and would land as the operator's. Fails if the routes accept any listed service, or the
// operator's account from any issuer: the listener's PUT, or the second issuer's, lands.
func TestOnlyTheOperatorWritesTheHold(t *testing.T) {
	rig := newHoldRig(t)
	rig.bootstrap()
	rig.move(1, "prepare", "hold-o")
	rig.move(2, "held", "hold-o")
	rig.do(http.MethodPut, "/v1/nats-hold/records", rig.operator(), rig.recordBody("hold-o", "cohort", "cohort", 0, `{"threshold":3}`)).require(t, "the cohort", http.StatusOK, "")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", rig.operator(), rig.cutoverBody("session", "done", "hold-o", 3)).require(t, "the cutover", http.StatusOK, "")
	before := rig.holdTables()
	// The control: the second issuer's token of the operator's account verifies, as the second
	// issuer's, so its refusal below is the operator entry's issuer rule and not its verification.
	second, err := rig.deps.NATSHold.OperatorToken(context.Background(), rig.secondToken(holdOperatorAccount))
	if err != nil || second.Account != holdOperatorAccount || second.Issuer != "second" {
		t.Fatalf("the second issuer's token of the operator's account = %+v, %v; want it verified as the second issuer's", second, err)
	}

	_, person, _ := approvedLogin(t, ciphertest.NewRig(t))
	callers := []struct{ name, token string }{
		{"a listener entry", rig.token(holdListenerAccount)},
		{"a dispatch entry", rig.token(holdDispatchAccount)},
		{"a daemon entry", rig.token(holdDaemonAccount)},
		{"a measure entry", rig.token(holdMeasureAccount)},
		{"a probe entry", rig.token(holdProbeAccount)},
		{"the operator's account from an issuer its entry does not name", rig.secondToken(holdOperatorAccount)},
		{"a person's broker access token", person.AccessToken},
	}
	for _, caller := range callers {
		for _, call := range []struct {
			method, path string
			body         map[string]any
		}{
			{http.MethodGet, "/v1/nats-hold", nil},
			{http.MethodPut, "/v1/nats-hold", rig.moveBody(3, "released", "hold-o")},
			{http.MethodPost, "/v1/nats-hold/restore", rig.restoreBody(4, "released", "hold-o")},
			{http.MethodPut, "/v1/nats-hold/records", rig.recordBody("hold-o", "cohort", "cohort", 1, `{"threshold":9}`)},
			{http.MethodGet, "/v1/nats-hold/records?hold_id=hold-o&kind=cohort", nil},
			{http.MethodDelete, rig.signedDelete("hold-o", "cohort", "cohort", 1), nil},
			{http.MethodPut, "/v1/nats-hold/cutover", rig.cutoverBody("session", "rolled_back", "hold-o", 3)},
			{http.MethodGet, "/v1/nats-hold/cutover", nil},
		} {
			var body any
			if call.body != nil {
				body = call.body
			}
			rig.do(call.method, call.path, caller.token, body).require(t, caller.name+"'s "+call.method+" "+call.path, http.StatusForbidden, "not_operator")
		}
	}
	rig.do(http.MethodGet, "/v1/nats-hold", "", nil).require(t, "a GET with no token", http.StatusForbidden, "not_operator")
	if after := rig.holdTables(); after != before {
		t.Fatalf("a refused caller changed the hold's tables:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// requireSigned fails t, and goes on, unless fields carries a sig that verifies over lines with
// the rig's key, naming what it read: a test checks every answer's signature in one run.
func (r *holdRig) requireSigned(what string, fields map[string]json.RawMessage, lines []string) {
	r.t.Helper()
	var sig string
	if err := json.Unmarshal(fields["sig"], &sig); err != nil || sig == "" {
		r.t.Errorf("%s carries no sig: %s", what, mustJSON(fields))
		return
	}
	if err := signedrecord.Verify([]ed25519.PublicKey{r.key.Public().(ed25519.PublicKey)}, sig, lines); err != nil {
		r.t.Errorf("%s's sig does not verify over its fields: %v", what, err)
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// TestTheHoldRoutesCarryTheirSignatures: GET /v1/nats-hold answers the row with its signed_at and
// sig, and GET of the records and of the cutover rows each row with its sig, each verifying
// against the operator's key; a record write and a cutover write with no sig, or with one over
// another gen, are 400 bad_signature and store nothing; and /restore with the row absent and a
// wrong gen is 409 gen_mismatch naming restore_gen, one above the log's highest, at which the
// body signed afresh is stored. Fails if an answer drops its sig or signed_at, or the refusal its
// restore_gen.
func TestTheHoldRoutesCarryTheirSignatures(t *testing.T) {
	rig := newHoldRig(t)
	op := rig.operator()
	rig.bootstrap()
	rig.move(1, "prepare", "hold-s")
	rig.move(2, "held", "hold-s")

	row := rig.do(http.MethodGet, "/v1/nats-hold", op, nil).require(t, "GET /v1/nats-hold", http.StatusOK, "")
	signedAt, err := time.Parse(time.RFC3339Nano, row.str("signed_at"))
	if err != nil {
		t.Errorf("GET /v1/nats-hold answers signed_at %q, which is no RFC 3339 time: %v", row.str("signed_at"), err)
	}
	rig.requireSigned("GET /v1/nats-hold", row.fields, signedrecord.HoldRow(uint64(row.num("gen")), row.str("phase"), row.str("hold_id"), signedAt))

	const value = `{"set":["envoy-session-a"],"b":{"z":1,"a":[true,null]}}`
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, rig.recordBody("hold-s", "convert", "set", 0, value)).require(t, "the convert record", http.StatusOK, "")
	unsigned := rig.recordBody("hold-s", "convert", "other", 0, value)
	delete(unsigned, "sig")
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, unsigned).require(t, "a record write with no sig", http.StatusBadRequest, "bad_signature")
	otherGen := rig.recordBody("hold-s", "convert", "other", 0, value)
	otherGen["sig"] = rig.sign(signedrecord.Record("hold-s", "convert", "other", 2, []byte(value)))
	rig.do(http.MethodPut, "/v1/nats-hold/records", op, otherGen).require(t, "a record write signed over another gen", http.StatusBadRequest, "bad_signature")
	listed := rig.do(http.MethodGet, "/v1/nats-hold/records?hold_id=hold-s&kind=convert", op, nil).require(t, "GET the convert records", http.StatusOK, "")
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(listed.fields["records"], &records); err != nil || len(records) != 1 {
		t.Fatalf("GET the convert records lists %s, want the one record the signed write stored", listed.fields["records"])
	}
	var key string
	var gen uint64
	_ = json.Unmarshal(records[0]["key"], &key)
	_ = json.Unmarshal(records[0]["gen"], &gen)
	rig.requireSigned("the convert record", records[0], signedrecord.Record("hold-s", "convert", key, gen, records[0]["value"]))
	if _, ok := records[0]["written_at"]; !ok {
		t.Fatalf("the convert record carries no written_at: %s", mustJSON(records[0]))
	}

	noSig := rig.cutoverBody("dispatch", "done", "hold-s", 3)
	delete(noSig, "sig")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", op, noSig).require(t, "a cutover write with no sig", http.StatusBadRequest, "bad_signature")
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", op, rig.cutoverBody("dispatch", "done", "hold-s", 2)).
		require(t, "a cutover write signed over another gen", http.StatusBadRequest, "bad_signature")
	if stored := rig.count(`select count(*) from nats_cutover`); stored != 0 {
		t.Fatalf("the unsigned and mis-signed cutover writes stored %d rows", stored)
	}
	rig.do(http.MethodPut, "/v1/nats-hold/cutover", op, rig.cutoverBody("dispatch", "done", "hold-s", 3)).require(t, "the signed cutover write", http.StatusOK, "")
	cutovers := rig.do(http.MethodGet, "/v1/nats-hold/cutover", op, nil).require(t, "GET /v1/nats-hold/cutover", http.StatusOK, "")
	var families []map[string]json.RawMessage
	if err := json.Unmarshal(cutovers.fields["families"], &families); err != nil || len(families) != 1 {
		t.Fatalf("GET /v1/nats-hold/cutover lists %s, want the one row", cutovers.fields["families"])
	}
	var family, state, holdID string
	var holdGen uint64
	_ = json.Unmarshal(families[0]["family"], &family)
	_ = json.Unmarshal(families[0]["state"], &state)
	_ = json.Unmarshal(families[0]["hold_id"], &holdID)
	_ = json.Unmarshal(families[0]["hold_gen"], &holdGen)
	rig.requireSigned("the dispatch cutover row", families[0], signedrecord.Cutover(family, state, holdID, holdGen))

	if _, err := rig.pool.Exec(context.Background(), `delete from nats_hold`); err != nil {
		t.Fatal(err)
	}
	refused := rig.do(http.MethodPost, "/v1/nats-hold/restore", op, rig.restoreBody(2, "held", "hold-s")).
		require(t, "a restore at a gen below the log's", http.StatusConflict, "gen_mismatch")
	want := rig.count(`select max(gen) + 1 from nats_hold_log`)
	if refused.num("restore_gen") != int64(want) {
		t.Fatalf("the restore's refusal names restore_gen %d (%s), want %d, one above the log's highest", refused.num("restore_gen"), refused.body, want)
	}
	// The log's latest entry is held, so a released restore, even signed at restore_gen, would free
	// every client mid-hold: it is refused and writes nothing.
	before := rig.holdTables()
	rig.do(http.MethodPost, "/v1/nats-hold/restore", op, rig.restoreBody(uint64(refused.num("restore_gen")), "released", "")).
		require(t, "a released restore at restore_gen after a held entry", http.StatusBadRequest, "bad_transition")
	if after := rig.holdTables(); after != before {
		t.Fatalf("the refused released restore changed the hold's tables:\n%s\nwant\n%s", after, before)
	}
	again := rig.do(http.MethodPost, "/v1/nats-hold/restore", op, rig.restoreBody(uint64(refused.num("restore_gen")), "held", "hold-s")).
		require(t, "the restore signed afresh at restore_gen", http.StatusOK, "")
	againAt, err := time.Parse(time.RFC3339Nano, again.str("signed_at"))
	if err != nil {
		t.Errorf("the restore answers signed_at %q, which is no RFC 3339 time: %v", again.str("signed_at"), err)
	}
	rig.requireSigned("the restored row", again.fields, signedrecord.HoldRow(uint64(again.num("gen")), again.str("phase"), again.str("hold_id"), againAt))
}

// A hold route's refusal of a body that does not decode names the field and the kind of failure,
// never the value: a records PUT whose expected_gen is a number out of range, and one whose body
// is not JSON, are each 400 bad_request, and the message holds no part of the value, which the
// operator's commands copy into their own errors. The routes store nothing.
//
// Run by hand, the control is the routes at bb2b623923, which echo json's message: `body must be
// one valid JSON object: json: cannot unmarshal number 1844674407370955161600123456789 into Go
// struct field holdRecordBody.expected_gen of type int64`.
func TestAHoldBodyRefusalNamesNoValue(t *testing.T) {
	rig := newHoldRig(t)
	op := rig.operator()
	rig.bootstrap()
	rig.move(1, "prepare", "hold-v")
	before := rig.holdTables()
	const canary = "1844674407370955161600123456789"
	body := rig.recordBody("hold-v", "cohort", "cohort", 0, `{"sessions":[]}`)
	body["expected_gen"] = json.Number(canary)
	refused := rig.do(http.MethodPut, "/v1/nats-hold/records", op, body).
		require(t, "a records PUT whose expected_gen is out of range", http.StatusBadRequest, "bad_request")
	if message := refused.str("message"); strings.Contains(message, canary) || !strings.Contains(message, `"expected_gen"`) {
		t.Errorf("the refusal's message is %q: it holds the value, or names no field", message)
	}
	notJSON := rig.doRaw(http.MethodPut, "/v1/nats-hold/records", op, []byte(`{"hold_id":"hold-v","kind":CANARY-BARE-4f1c}`)).
		require(t, "a records PUT that is not JSON", http.StatusBadRequest, "bad_request")
	if message := notJSON.str("message"); strings.Contains(message, "CANARY") || strings.Contains(message, "'C'") || !strings.Contains(message, "byte") {
		t.Errorf("the refusal's message is %q: it holds part of the value, or names no byte", message)
	}
	if after := rig.holdTables(); after != before {
		t.Fatalf("a refused body changed the hold's tables:\nbefore %s\nafter %s", before, after)
	}
}
