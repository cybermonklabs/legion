package writerhold

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/holdrecord/holdrecordtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// holdTables is every row of the hold's four tables (migration 0041_nats_hold), each as its JSON
// text, in a fixed order: what a write that "stores nothing" must leave as it was.
func holdTables(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var all []string
	for _, table := range []string{"nats_hold", "nats_hold_log", "nats_hold_records", "nats_cutover"} {
		rows, err := pool.Query(t.Context(), `select row_to_json(t)::text from `+table+` t order by 1`)
		require.NoError(t, err, "read %s", table)
		for rows.Next() {
			var row string
			require.NoError(t, rows.Scan(&row))
			all = append(all, table+" "+row)
		}
		require.NoError(t, rows.Err())
	}
	return all
}

// holdRoute sends method path to the hold's routes at base with token as its bearer and body as
// its JSON, and answers the status and the refusal's code ("" for none).
func holdRoute(t *testing.T, base, token, method, path string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err, "%s %s", method, path)
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var refusal struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(answer, &refusal)
	return response.StatusCode, refusal.Error
}

// holdSign signs lines with key, failing t when it cannot.
func holdSign(t *testing.T, key ed25519.PrivateKey, lines []string) string {
	t.Helper()
	sig, err := signedrecord.Sign(key, lines)
	require.NoError(t, err)
	return sig
}

// holdBodies are one write of each of the hold's four routes, each signed by sign over the lines
// the route verifies it against: the move of the row from gen to released under holdID, a cohort
// record of holdID, family's cutover row under holdID at the row's gen, and a restore of a
// released row at restoreGen. sign "" sends each unsigned.
type holdBodies struct {
	move, record, cutover, restore map[string]any
}

func newHoldBodies(t *testing.T, sign func([]string) string, gen uint64, holdID, key, family string, restoreGen uint64) holdBodies {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	value := json.RawMessage(`{"written":"` + key + `"}`)
	return holdBodies{
		move: map[string]any{"expected_gen": gen, "phase": "released", "hold_id": holdID, "reason": "the row's test", "signed_at": at.Format(time.RFC3339Nano),
			"sig": sign(signedrecord.HoldRow(gen+1, "released", holdID, at))},
		record: map[string]any{"hold_id": holdID, "kind": "cohort", "key": key, "value": value, "expected_gen": 0,
			"sig": sign(signedrecord.Record(holdID, "cohort", key, 1, value))},
		cutover: map[string]any{"family": family, "state": "done", "hold_id": holdID, "sig": sign(signedrecord.Cutover(family, "done", holdID, gen))},
		restore: map[string]any{"gen": restoreGen, "phase": "released", "hold_id": "", "reason": "the row's test", "signed_at": at.Format(time.RFC3339Nano),
			"sig": sign(signedrecord.HoldRow(restoreGen, "released", "", at))},
	}
}

// TestHoldCutoverRecordsOnlyUnderTheHold shows that the broker writes a family's cutover row only
// under the hold (§3a): PUT /v1/nats-hold/cutover while the row is released or prepare, or with
// another hold id while it is held, answers 409 hold_mismatch and writes nothing. Control, run
// first: under held with the hold's own id, the same write stores the family's row with the row's
// gen. The row fails if the route stores a cutover row under any other row.
func TestHoldCutoverRecordsOnlyUnderTheHold(t *testing.T) {
	ctx := t.Context()
	b := holdrecordtest.Start(t)
	c := b.Client(t)
	_, err := c.RestoreRow(ctx, 1, natsgrant.OperatorPhaseReleased, "", "bootstrap")
	require.NoError(t, err)
	move := func(gen uint64, phase natsgrant.OperatorPhase, holdID string) {
		t.Helper()
		_, err := c.MoveRow(ctx, gen, phase, holdID, "the row's test")
		require.NoError(t, err)
	}

	move(1, natsgrant.OperatorPhasePrepare, "h-cutover")
	move(2, natsgrant.OperatorPhaseHeld, "h-cutover")
	written, err := c.PutCutover(ctx, "session", "done", "h-cutover")
	require.NoError(t, err, "control: the cutover write under held with the hold's own id")
	require.Equal(t, holdrecord.CutoverRow{Family: "session", State: "done", HoldID: "h-cutover", HoldGen: 3}, written)
	var stored uint64
	require.NoError(t, b.Pool.QueryRow(ctx, `select hold_gen from nats_cutover where family = 'session'`).Scan(&stored))
	require.Equal(t, uint64(3), stored, "control: the stored row's hold_gen")
	t.Logf("control: under held with its own id the write stored %+v, hold_gen %d in Postgres", written, stored)
	move(3, natsgrant.OperatorPhaseReleased, "h-cutover")

	refused := func(what, holdID string) {
		t.Helper()
		before := holdTables(t, b.Pool)
		_, err := c.PutCutover(ctx, "dispatch", "done", holdID)
		var route *holdrecord.RouteError
		require.ErrorAs(t, err, &route, "%s", what)
		require.Equal(t, http.StatusConflict, route.Status, "%s", what)
		require.Equal(t, "hold_mismatch", route.Code, "%s", what)
		require.Equal(t, before, holdTables(t, b.Pool), "%s wrote", what)
		t.Logf("%s: %v, and the tables are unchanged", what, err)
	}
	refused("under released, with the last hold's id", "h-cutover")
	move(4, natsgrant.OperatorPhasePrepare, "h-cutover-2")
	refused("under prepare, with the hold's own id", "h-cutover-2")
	move(5, natsgrant.OperatorPhaseHeld, "h-cutover-2")
	refused("under held, with another hold's id", "h-cutover")
	refused("under held, with an id no hold had", "h-never")
}

// TestHoldOnlyTheOperatorMovesTheHold shows that only the operator moves the hold (§3a): PUT
// /v1/nats-hold, PUT /v1/nats-hold/records and PUT /v1/nats-hold/cutover, each with a body the
// operator's key signed, answer 403 not_operator to a listener's, Dispatch's and a session's
// credential, and the row and the records are unchanged. The routes are the broker's own
// (api.Register) over the test Postgres, verifying tokens with the auth service's platform-token
// verifier (natscallout.PlatformTokens) over a local issuer, as cmd/cipherd wires them, so the
// listener's and Dispatch's tokens verify and are refused for the account they name. Control, run
// first: the operator's token is accepted on each, and after the refusals the operator's token is
// accepted with the very bodies the others sent. The row fails if the routes admit any account but
// the operator entry's.
func TestHoldOnlyTheOperatorMovesTheHold(t *testing.T) {
	ctx := t.Context()
	b := holdrecordtest.Start(t)
	_, err := b.Client(t).RestoreRow(ctx, 1, natsgrant.OperatorPhaseReleased, "", "bootstrap")
	require.NoError(t, err)

	const audience = "envoy-nats"
	const listenerAccount = "system:serviceaccount:holdtest:listener-hub-a"
	const dispatchAccount = "system:serviceaccount:holdtest:dispatch"
	issuer := oidctest.New(t)
	signer := issuer.PublishKey(t, "nats-key")
	verifier, err := oidc.New(ctx, issuer.URL(), audience)
	require.NoError(t, err)
	services, err := config.ParseNATSServices(strings.Join([]string{
		"operator/-/*=" + holdtest.OperatorAccount, "listener/hub-a/*=" + listenerAccount, "dispatch/dispatch/*=" + dispatchAccount,
	}, " "))
	require.NoError(t, err)
	var operatorEntry config.NATSService
	for _, entry := range services {
		if entry.Kind == config.NATSOperator {
			operatorEntry = entry
		}
	}
	var schema string
	require.NoError(t, b.Pool.QueryRow(ctx, `select current_schema()`).Scan(&schema))
	tokens := natscallout.PlatformTokens{K8sIssuer: issuer.URL(), K8s: enroll.K8sPodVerifier{Verifier: verifier}}
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{
		AccountLimit: &ratelimit.Limit{Every: time.Microsecond, Burst: 1 << 20},
		NATSHold: api.NATSHold{Pool: b.Pool, Schema: schema, Keys: []ed25519.PublicKey{b.VerifyKey},
			OperatorToken: tokens.Verify, OperatorAccount: operatorEntry.Account, OperatorGoogle: operatorEntry.Google},
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mint := func(issuer *oidctest.Issuer, key *oidctest.Key, account string) string {
		claims := issuer.Claims(account, audience)
		now := time.Now()
		claims["iat"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(10*time.Minute).Unix()
		return issuer.Mint(t, key, claims)
	}
	operatorToken := mint(issuer, signer, holdtest.OperatorAccount)
	// A session's credential is the broker's own access token for that session, issued by the
	// broker and no platform issuer: here a token of an issuer the routes take no NATS token from.
	brokerIssuer := oidctest.New(t)
	brokerKey := brokerIssuer.PublishKey(t, "broker-key")
	others := []struct{ name, token string }{
		{"a listener's", mint(issuer, signer, listenerAccount)},
		{"Dispatch's", mint(issuer, signer, dispatchAccount)},
		{"a session's", mint(brokerIssuer, brokerKey, "session:ses_holdroutes")},
	}
	operatorSigned := func(lines []string) string { return holdSign(t, b.SigningKey, lines) }
	send := func(token string, bodies holdBodies) [3]string {
		t.Helper()
		var answers [3]string
		for i, write := range []struct {
			method, path string
			body         map[string]any
		}{
			{http.MethodPut, "/v1/nats-hold/records", bodies.record},
			{http.MethodPut, "/v1/nats-hold/cutover", bodies.cutover},
			{http.MethodPut, "/v1/nats-hold", bodies.move},
		} {
			status, code := holdRoute(t, server.URL, token, write.method, write.path, write.body)
			answers[i] = fmt.Sprintf("%s %s: %d %s", write.method, write.path, status, code)
		}
		return answers
	}
	operatorClient, err := holdrecord.New(holdrecord.Config{BrokerURL: server.URL, Token: func(context.Context) (string, error) { return operatorToken, nil },
		SigningKey: b.SigningKey, VerifyKeys: []ed25519.PublicKey{b.VerifyKey}})
	require.NoError(t, err)

	// Control: the operator's token is accepted on each route.
	_, err = operatorClient.MoveRow(ctx, 1, natsgrant.OperatorPhasePrepare, "h-routes", "the row's test")
	require.NoError(t, err)
	_, err = operatorClient.MoveRow(ctx, 2, natsgrant.OperatorPhaseHeld, "h-routes", "the row's test")
	require.NoError(t, err)
	require.Equal(t, [3]string{
		"PUT /v1/nats-hold/records: 200 ", "PUT /v1/nats-hold/cutover: 200 ", "PUT /v1/nats-hold: 200 ",
	}, send(operatorToken, newHoldBodies(t, operatorSigned, 3, "h-routes", "control", "session", 0)), "control: the operator's writes")
	t.Logf("control: the operator's token wrote a record, a cutover row and the row's move")
	_, err = operatorClient.MoveRow(ctx, 4, natsgrant.OperatorPhasePrepare, "h-routes-2", "the row's test")
	require.NoError(t, err)
	_, err = operatorClient.MoveRow(ctx, 5, natsgrant.OperatorPhaseHeld, "h-routes-2", "the row's test")
	require.NoError(t, err)

	bodies := newHoldBodies(t, operatorSigned, 6, "h-routes-2", "others", "dispatch", 0)
	before := holdTables(t, b.Pool)
	for _, other := range others {
		require.Equal(t, [3]string{
			"PUT /v1/nats-hold/records: 403 not_operator", "PUT /v1/nats-hold/cutover: 403 not_operator", "PUT /v1/nats-hold: 403 not_operator",
		}, send(other.token, bodies), "%s credential", other.name)
		require.Equal(t, before, holdTables(t, b.Pool), "%s credential changed the hold's tables", other.name)
		t.Logf("%s credential: 403 not_operator on each route, and the tables are unchanged", other.name)
	}
	row, err := operatorClient.Row(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(6), row.Gen)
	require.Equal(t, natsgrant.OperatorPhaseHeld, row.Phase)
	records, err := operatorClient.Records(ctx, "h-routes-2", "cohort")
	require.NoError(t, err)
	require.Empty(t, records, "a record of the second hold")

	// The very bodies the others sent are the operator's to write: each refusal was the token's.
	require.Equal(t, [3]string{
		"PUT /v1/nats-hold/records: 200 ", "PUT /v1/nats-hold/cutover: 200 ", "PUT /v1/nats-hold: 200 ",
	}, send(operatorToken, bodies), "the operator's token with the refused bodies")
}

// TestHoldEveryRowIsSigned shows the signed rows (plan E lines 2224-2228). An unsigned PUT
// /v1/nats-hold, records write, cutover write and POST /v1/nats-hold/restore, and each one signed
// with another key, answer 400 bad_signature and store nothing; each control, run first, is the
// same write signed with the operator's key, which the route takes (the restore, its signature
// good, reaches the store and is refused 409 row_exists). A row, a record and a cutover row
// written into the test Postgres directly with a signature that does not verify each read as
// holdrecord.ErrUnknown to the operator's client, and the row is unknown to the auth service,
// which refuses a listener hold_unknown and admits the operator with released's grant; an answer
// without its sig is ErrUnknown; and the bootstrap's own signed released, replayed below a
// running replica's floor, reads unknown. Control, run first: a replica whose HoldKeys accept the
// forger's key admits a listener under the forged released, during the hold; and a replica with
// no floor, one started after the replay, admits a listener under the replayed released, the
// residual of a broker that starts again. The `hold keygen` clause is the builder's. The row fails
// if any route stores, or any reader trusts, a row its keys do not verify.
func TestHoldEveryRowIsSigned(t *testing.T) {
	ctx := t.Context()
	s := holdStack(t)
	pool := s.Broker.Pool
	base := s.Broker.URL
	token := holdrecordtest.OperatorToken
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	operatorSigned := func(lines []string) string { return holdSign(t, s.Broker.SigningKey, lines) }
	otherSigned := func(lines []string) string { return holdSign(t, otherKey, lines) }
	unsigned := func([]string) string { return "" }
	bootstrap := holdTables(t, pool)[0]
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-signed")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-signed")

	// The routes. Each control first, then the unsigned write and the write another key signed.
	writes := []struct {
		name, method, path string
		body               func(holdBodies) map[string]any
		accepted           int
		code               string
	}{
		{"the records write", http.MethodPut, "/v1/nats-hold/records", func(b holdBodies) map[string]any { return b.record }, http.StatusOK, ""},
		{"the cutover write", http.MethodPut, "/v1/nats-hold/cutover", func(b holdBodies) map[string]any { return b.cutover }, http.StatusOK, ""},
		{"the restore", http.MethodPost, "/v1/nats-hold/restore", func(b holdBodies) map[string]any { return b.restore }, http.StatusConflict, "row_exists"},
		{"PUT /v1/nats-hold", http.MethodPut, "/v1/nats-hold", func(b holdBodies) map[string]any { return b.move }, http.StatusOK, ""},
	}
	for index, write := range writes {
		key := fmt.Sprintf("k%d", index)
		family := []string{"session", "dispatch", "daemon/HOLD", "daemon/SIGNED"}[index]
		status, code := holdRoute(t, base, token, write.method, write.path, write.body(newHoldBodies(t, operatorSigned, 3, "h-signed", key+"-operator", family, 4)))
		require.Equal(t, write.accepted, status, "control: %s signed with the operator's key", write.name)
		require.Equal(t, write.code, code, "control: %s signed with the operator's key", write.name)
		t.Logf("control: %s signed with the operator's key: %d %s", write.name, status, code)
		for _, forged := range []struct {
			name string
			sign func([]string) string
		}{{"unsigned", unsigned}, {"signed with another key", otherSigned}} {
			before := holdTables(t, pool)
			status, code := holdRoute(t, base, token, write.method, write.path, write.body(newHoldBodies(t, forged.sign, 3, "h-signed", key+"-forged", "daemon/FORGED", 4)))
			require.Equal(t, http.StatusBadRequest, status, "%s %s", write.name, forged.name)
			require.Equal(t, "bad_signature", code, "%s %s", write.name, forged.name)
			require.Equal(t, before, holdTables(t, pool), "%s %s stored", write.name, forged.name)
			t.Logf("%s %s: %d %s, and the tables are unchanged", write.name, forged.name, status, code)
		}
	}
	// The control of the move released the row at gen 4; hold it again for the readers' half.
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-signed-2")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-signed-2")
	heldRow := holdTables(t, pool)[0]

	// The readers. Control first: a replica whose HoldKeys accept the forger's key admits a
	// listener under a released the forger signed and wrote into Postgres, while the row is held.
	forgerPublic, forger, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Microsecond)
	forge := func() {
		t.Helper()
		_, err := pool.Exec(ctx, `update nats_hold set gen = 7, phase = 'released', signed_at = $1, sig = $2`,
			at, holdSign(t, forger, signedrecord.HoldRow(7, "released", "h-signed-2", at)))
		require.NoError(t, err)
	}
	restore := func(text string) {
		t.Helper()
		_, err := pool.Exec(ctx, `update nats_hold set (gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig) =
			(select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from json_populate_record(null::nats_hold, $1::json))`,
			strings.TrimPrefix(text, "nats_hold "))
		require.NoError(t, err)
	}
	trusting := s.StartReplica(t, holdtest.ReplicaConfig{HoldKeys: []ed25519.PublicKey{s.Broker.VerifyKey, forgerPublic}})
	forge()
	listener, err := connectListener(s)
	require.NoError(t, err, "control: a replica that takes the forger's key refused the listener")
	listener.Close()
	t.Logf("control: a replica whose HoldKeys accept the forger's key admitted a listener under the forged released, during the hold")
	trusting.Close()

	replica := s.StartReplica(t, holdtest.ReplicaConfig{})
	_, err = connectListener(s)
	requireRefusedAt(t, err, "a listener under the forged released")
	require.Equal(t, int64(1), replica.Decisions("service", "refused", "hold_unknown"))
	op, err := connectOperator(s)
	require.NoError(t, err, "the operator under the forged row")
	require.Equal(t, natsgrant.OperatorPhaseReleased, operatorPhase(t, op), "the operator's grant under the forged row")
	op.Close()
	_, err = s.Records.Row(ctx)
	require.ErrorIs(t, err, holdrecord.ErrUnknown, "the operator's client read the forged row")
	t.Logf("a row the forger signed: the auth service refused the listener hold_unknown and gave the operator released's grant; holdrecord: %v", err)
	restore(heldRow)
	_, err = connectListener(s)
	requireRefusedAt(t, err, "a listener under the held row again")
	require.Equal(t, int64(1), replica.Decisions("service", "refused", "held"), "the replica's floor is the held row's gen")

	for _, tamper := range []struct {
		name, sql string
		read      func() error
	}{
		{"a record rewritten by a database writer", `update nats_hold_records set value = '{"written":"forged"}' where kind = 'cohort' and key = 'k0-operator'`,
			func() error { _, err := s.Records.Records(ctx, "h-signed", "cohort"); return err }},
		{"a cutover row rewritten by a database writer", `update nats_cutover set state = 'rolled_back' where family = 'dispatch'`,
			func() error { _, err := s.Records.Cutovers(ctx); return err }},
	} {
		require.NoError(t, tamper.read(), "control: %s, before the tamper", tamper.name)
		_, err := pool.Exec(ctx, tamper.sql)
		require.NoError(t, err)
		err = tamper.read()
		require.ErrorIs(t, err, holdrecord.ErrUnknown, "%s", tamper.name)
		t.Logf("%s: %v", tamper.name, err)
	}

	s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/nats-hold" {
			return false
		}
		recorder := httptest.NewRecorder()
		s.Broker.Routes().ServeHTTP(recorder, r)
		var answer map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			return false
		}
		delete(answer, "sig")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
		return true
	})
	_, err = s.Records.Row(ctx)
	require.ErrorIs(t, err, holdrecord.ErrUnknown, "an answer without its sig")
	t.Logf("an answer without its sig: %v", err)
	s.Broker.Intercept(nil)

	restore(bootstrap)
	_, err = connectListener(s)
	requireRefusedAt(t, err, "a listener under the replayed released")
	require.Equal(t, int64(2), replica.Decisions("service", "refused", "hold_unknown"), "the replayed released below the floor")
	t.Logf("the bootstrap's signed released replayed below the replica's floor: refused hold_unknown (%v)", lastReading(t, replica).Phase)
	replica.Close()
	fresh := s.StartReplica(t, holdtest.ReplicaConfig{})
	listener, err = connectListener(s)
	require.NoError(t, err, "control: a replica with no floor refused the listener under the replayed released")
	listener.Close()
	t.Logf("control: a replica started after the replay, with no floor, admitted a listener under the replayed released (the stated residual)")
	fresh.Close()
	restore(heldRow)
}

// TestHoldTheReadFlag shows the auth service's half of BROKER_NATS_HOLD_READ: a replica with no
// HoldRead, the setting false, reads no row and admits the hold's probe while the row is held, its
// envoy_broker_hold_unknown 0; with the setting true the probe is refused held. With the row
// missing (deleted by the instance admin) or with a bad sig, every client but the operator is
// refused hold_unknown, the probe and a listener included, the operator is admitted, and
// envoy_broker_hold_unknown reads 1; `hold restore` reaches POST /v1/nats-hold/restore, which the
// operator's client answers 409 gen_mismatch and signs again at its restore_gen, and the
// restored row refuses the listener held again. Control, run first: the replica with no HoldRead,
// which admits the probe during the hold. The broker's start rule (true with no
// BROKER_NATS_HOLD_SIGNING_KEYS refuses the start) is cmd/cipherd's refuseHoldReadWithoutOperator,
// unexported, and the clause that a hold set stops at step 2 is the builder's. The row fails if
// the setting true reads no row, or reads a missing or forged row as anything but unknown.
func TestHoldTheReadFlag(t *testing.T) {
	ctx := t.Context()
	s := holdStack(t)
	pool := s.Broker.Pool
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-flag")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-flag")
	connectProbe := func() (*holdClient, error) { return connectToHold(s, s.ProbeCredential()) }

	off := s.StartReplica(t, holdtest.ReplicaConfig{NoHoldRead: true})
	probe, err := connectProbe()
	require.NoError(t, err, "control: the probe, with BROKER_NATS_HOLD_READ false, during the hold")
	probe.Close()
	require.Equal(t, "0", off.HoldUnknown(), "envoy_broker_hold_unknown with the setting false")
	t.Logf("control: with no HoldRead the probe connected during the held row; envoy_broker_hold_unknown %s", off.HoldUnknown())
	off.Close()

	on := s.StartReplica(t, holdtest.ReplicaConfig{})
	_, err = connectProbe()
	requireRefusedAt(t, err, "the probe with the setting true")
	require.Equal(t, int64(1), on.Decisions("service", "refused", "held"), "the probe's refusal")
	require.Equal(t, "0", on.HoldUnknown())

	unknown := func(what string) {
		t.Helper()
		before := on.Decisions("service", "refused", "hold_unknown")
		_, err := connectProbe()
		requireRefusedAt(t, err, "the probe with "+what)
		_, err = connectListener(s)
		requireRefusedAt(t, err, "a listener with "+what)
		require.Equal(t, before+2, on.Decisions("service", "refused", "hold_unknown"), "%s: the probe's and the listener's refusals", what)
		op, err := connectOperator(s)
		require.NoError(t, err, "the operator with %s", what)
		op.Close()
		require.Equal(t, "1", on.HoldUnknown(), "%s: envoy_broker_hold_unknown", what)
		t.Logf("%s: the probe and a listener refused hold_unknown, the operator admitted, envoy_broker_hold_unknown %s", what, on.HoldUnknown())
	}

	_, err = pool.Exec(ctx, `delete from nats_hold`)
	require.NoError(t, err)
	unknown("the row missing")
	row, err := s.Records.RestoreRow(ctx, 1, natsgrant.OperatorPhaseHeld, "h-flag", "the row's test")
	require.NoError(t, err, "hold restore through POST /v1/nats-hold/restore")
	require.Equal(t, uint64(4), row.Gen, "the restore at the restore_gen the first answer named")
	_, err = connectListener(s)
	requireRefusedAt(t, err, "a listener under the restored row")
	require.Equal(t, int64(2), on.Decisions("service", "refused", "held"), "the restored row is held")
	require.Equal(t, "0", on.HoldUnknown())
	t.Logf("hold restore reached /v1/nats-hold/restore and restored %+v; the listener is refused held again", row)

	_, err = pool.Exec(ctx, `update nats_hold set sig = 'not-a-signature'`)
	require.NoError(t, err)
	unknown("a bad sig")
}

// holdKeyRecord is the last record of key hold in KV_envoy_session_consumers, as
// STREAM.MSG.GET answers it: its sequence and its stored headers and data.
type holdKeyRecord struct {
	Sequence uint64 `json:"seq"`
	Header   []byte `json:"hdrs"`
	Data     []byte `json:"data"`
}

// readHoldKey reads that record on nc, the operator's connection.
func readHoldKey(t *testing.T, nc *nats.Conn, subject string) holdKeyRecord {
	t.Helper()
	var answer struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Message holdKeyRecord       `json:"message"`
	}
	stream := kvStreamPrefix + bus.SessionConsumersBucket
	require.NoError(t, apiRequest(t.Context(), nc, fmt.Sprintf(natsserver.JSApiMsgGetT, stream), map[string]string{"last_by_subj": subject}, &answer))
	require.Nil(t, answer.Error, "read %s", subject)
	return answer.Message
}

// TestHoldNothingTrustedIsInNATS shows that nothing the hold trusts is in NATS (§3a, "Answers the
// server writes for a client"): a client aims the server's own answers at a reply subject of its
// choosing, $KV.envoy_session_consumers.hold, a bucket its grant cannot publish to — a JetStream
// API answer (STREAM.INFO), a direct get's copy of a record, a pull request's status reply and a
// publish acknowledgement — once while the row is prepare and once after its release, and none of
// them changes the hold row or any record, read through the operator's client and directly in
// Postgres. Control: each attack writes the key hold of that bucket, where the retired design kept
// the hold: had the hold been in NATS, each would have overwritten it. The plan's attacker is a
// session user; here it is a listener: the stack admits no session (its auth service has no broker
// to introspect a session's token), and the reply subject the attacks aim is checked against no
// user's grant, a session's or a listener's, while a listener's grant holds every request the four
// attacks send. The plan's B0-B6 are ChosenSubjectPublish's measurement, which this row has not
// read: the four attacks are the four kinds the plan's §3a names.
func TestHoldNothingTrustedIsInNATS(t *testing.T) {
	ctx := t.Context()
	s := holdtest.Start(t)
	target := "$KV." + bus.SessionConsumersBucket + ".hold"

	// The first hold: the operator, under held, makes what the attacks read, the notification
	// stream and the session bucket (no answer but the held operator's holds a stream create,
	// natsgrant.ForCallout), and makes the bucket of key hold and writes it as the retired design
	// kept the hold; and the hold's records in Postgres.
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-nats")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-nats")
	op, err := connectOperator(s)
	require.NoError(t, err)
	opJS, err := jetstream.New(op.Conn)
	require.NoError(t, err)
	_, err = opJS.CreateStream(ctx, jetstream.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects(), Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	_, err = opJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bus.SessionBucket, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	bucket, err := opJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bus.SessionConsumersBucket, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	_, err = bucket.PutString(ctx, "hold", `{"phase":"held","hold_id":"h-nats"}`)
	require.NoError(t, err)
	op.Close()
	_, err = s.Records.PutRecord(ctx, "h-nats", "done", "hold", 0, json.RawMessage(`{"workflow":"cutover","ops":[],"complete":false,"hold_gen":0}`))
	require.NoError(t, err)
	_, err = s.Records.PutRecord(ctx, "h-nats", "cohort", "cohort", 0, json.RawMessage(`{"thresholds":[1,2]}`))
	require.NoError(t, err)
	_, err = s.Records.PutCutover(ctx, "session", "done", "h-nats")
	require.NoError(t, err)
	moveRow(t, s, natsgrant.OperatorPhaseReleased, "h-nats")

	// A listener sets up the rest of what the attacks read, as its grant allows: a pull consumer on
	// the notification stream that never has a message, and a record of the session bucket.
	setup, err := connectListener(s)
	require.NoError(t, err)
	js, err := jetstream.New(setup.Conn)
	require.NoError(t, err)
	_, err = js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{Durable: "attack", FilterSubject: "notifications.agent.never", AckPolicy: jetstream.AckNonePolicy})
	require.NoError(t, err)
	sessions, err := js.KeyValue(ctx, bus.SessionBucket)
	require.NoError(t, err)
	_, err = sessions.PutString(ctx, "ses_attack", "registered")
	require.NoError(t, err)
	setup.Close()

	type trusted struct {
		tables   []string
		row      holdrecord.Row
		journal  []holdrecord.Record
		cohort   []holdrecord.Record
		cutovers []holdrecord.CutoverRow
	}
	readTrusted := func() trusted {
		t.Helper()
		var got trusted
		got.tables = holdTables(t, s.Broker.Pool)
		got.row, err = s.Records.Row(ctx)
		require.NoError(t, err)
		got.journal, err = s.Records.Records(ctx, "h-nats", "done")
		require.NoError(t, err)
		got.cohort, err = s.Records.Records(ctx, "h-nats", "cohort")
		require.NoError(t, err)
		got.cutovers, err = s.Records.Cutovers(ctx)
		require.NoError(t, err)
		return got
	}
	attack := func(phase string) {
		t.Helper()
		attacker, err := connectListener(s)
		require.NoError(t, err, "a listener %s", phase)
		defer attacker.Close()
		reader, err := connectOperator(s)
		require.NoError(t, err)
		defer reader.Close()
		before := readTrusted()
		last := readHoldKey(t, reader.Conn, target)

		// The listener's grant does not reach the bucket: its own publish there is refused.
		require.NoError(t, attacker.Publish(target, []byte("direct")))
		require.False(t, grantedOn(t, attacker, target), "%s: the listener published into the bucket directly", phase)
		require.Equal(t, last.Sequence, readHoldKey(t, reader.Conn, target).Sequence, "%s: the refused publish wrote key hold", phase)

		for _, a := range []struct {
			name, subject string
			body          []byte
		}{
			{"a JetStream API answer", fmt.Sprintf(natsserver.JSApiStreamInfoT, bus.Stream), nil},
			{"a direct get's copy", "$JS.API.DIRECT.GET." + kvStreamPrefix + bus.SessionBucket + ".$KV." + bus.SessionBucket + ".ses_attack", nil},
			{"a pull request's status reply", "$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + ".attack", []byte(`{"batch":1,"no_wait":true}`)},
			{"a publish acknowledgement", "notifications.agent.attack", []byte("attack")},
		} {
			require.NoError(t, attacker.PublishRequest(a.subject, target, a.body))
			require.NoError(t, attacker.Flush())
			var written holdKeyRecord
			testwait.Eventually(t, fmt.Sprintf("%s, %s, to land on %s", a.name, phase, target), func() bool {
				written = readHoldKey(t, reader.Conn, target)
				return written.Sequence > last.Sequence
			})
			last = written
			t.Logf("control, %s, %s: the server wrote key hold of KV_%s at sequence %d: headers %q, data %.60q",
				phase, a.name, bus.SessionConsumersBucket, written.Sequence, written.Header, written.Data)
		}
		require.Equal(t, before, readTrusted(), "%s: the attacks changed the hold's row or records", phase)
		t.Logf("%s: after the four attacks the hold row (gen %d, %s %s), its records and the cutover rows are unchanged, in Postgres and through the operator's client",
			phase, before.row.Gen, before.row.Phase, before.row.HoldID)
	}

	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-nats-2")
	attack("while the row is prepare")
	moveRow(t, s, natsgrant.OperatorPhaseReleased, "h-nats-2")
	attack("after the release")
}
