package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// NATSHold is what the NATS writer hold's routes need: the tables they write
// (internal/cipher/natshold, migration 0041_nats_hold), the operator's public keys every write
// must be signed by, and the one caller they admit, the operator entry of BROKER_NATS_SERVICES.
// The routes are the only writer of those tables a deployment runs: the operator's commands
// (envoy-infra hold, through internal/holdrecord) call them, and the auth service reads the hold
// row they write at every decision while BROKER_NATS_HOLD_READ is true.
type NATSHold struct {
	// Pool is the broker's Postgres pool, whose schema holds the hold's tables.
	Pool *pgxpool.Pool
	// Schema is the schema the hold's tables are in, which every statement names them by: the one
	// the broker's migrations ran in, public in a deployment, and a test's own in a test.
	Schema string
	// Keys are BROKER_NATS_HOLD_SIGNING_KEYS. A write whose signature none of them verifies is
	// 400 bad_signature, so with none every write is.
	Keys []ed25519.PublicKey
	// OperatorToken verifies a platform identity token for the NATS audiences, as the auth service
	// verifies a service's (cmd/cipherd passes natscallout.PlatformTokens.Verify), and answers the
	// account it names, whether that is a Google account, and when the token was issued and
	// expires. Nil, a broker with no NATS audience, admits no caller.
	OperatorToken func(ctx context.Context, raw string) (account string, google bool, issued, expiry time.Time, err error)
	// OperatorAccount and OperatorGoogle are the operator entry's account and whether it is a
	// Google account: the one caller the routes admit. An empty OperatorAccount, a list with no
	// operator entry, admits none.
	OperatorAccount string
	OperatorGoogle  bool
}

// longestOperatorToken is the longest life the operator's token may have from its issue, the bound
// the auth service holds every service's platform token to (natscallout's longestServiceToken): a
// token login's, beside the proof clock skew. The operator's commands mint ten minutes.
const longestOperatorToken = machine.MaxTokenLoginLifetime + ciphertoken.ProofClockSkew

// operatorCaller authenticates the operator's own platform token for authenticate, the one
// credential operatorAuth's routes take: verified as the auth service verifies a service's, no
// longer lived than an hour from its issue and not expired, naming the operator entry's account.
// Anything else is 403 not_operator, whatever was wrong with it, so a caller learns nothing of the
// operator's identity from the refusal, and none repeats the token.
func (s *server) operatorCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	hold := s.deps.NATSHold
	if hold.OperatorToken == nil || hold.OperatorAccount == "" {
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "this broker admits no operator: BROKER_NATS_SERVICES lists no operator entry, or no NATS audience verifies its token", nil)
		return caller{}, false
	}
	account, google, issued, expiry, err := hold.OperatorToken(r.Context(), bearer(r))
	switch {
	case err != nil:
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token does not verify as a platform token of the NATS audiences", nil)
		return caller{}, false
	case issued.IsZero() || expiry.Sub(issued) > longestOperatorToken:
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token names no issue time or lives longer than an hour from it", nil)
		return caller{}, false
	case !expiry.After(time.Now()):
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token has expired", nil)
		return caller{}, false
	case account != hold.OperatorAccount || google != hold.OperatorGoogle:
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token names an account other than the operator entry's", nil)
		return caller{}, false
	}
	return caller{operator: account}, true
}

// holdStore is the store of the hold's tables the routes write.
func (s *server) holdStore() natshold.Store {
	return natshold.Store{Pool: s.deps.NATSHold.Pool, Schema: s.deps.NATSHold.Schema}
}

// holdRow is the writer hold's row as every route answers it, in the request and refusal bodies
// alike: every column the operator's commands verify the row by, with its signature.
type holdRow struct {
	// The row's gen, which only goes up: one above the gen a move named, and on a restore one
	// above every gen nats_hold_log holds.
	Gen int64 `json:"gen"`
	// released, prepare or held.
	Phase string `json:"phase"`
	// The hold's id: the one a prepare began, which held and the release after it keep.
	HoldID string `json:"hold_id"`
	// Why the operator wrote the row.
	Reason string `json:"reason"`
	// When the broker stored the row, RFC 3339 in UTC.
	UpdatedAt string `json:"updated_at"`
	// The account whose token wrote the row: the operator entry's.
	UpdatedBy string `json:"updated_by"`
	// When the operator signed the row, RFC 3339 with nanoseconds in UTC, the text its signature's
	// lines carry (signedrecord.HoldRow).
	SignedAt string `json:"signed_at"`
	// The operator's Ed25519 signature over the row's lines, base64url without padding.
	Sig string `json:"sig"`
}

// holdRowAnswer is row as the routes answer it.
func holdRowAnswer(row natshold.Row) holdRow {
	return holdRow{
		Gen: int64(row.Gen), Phase: row.Phase, HoldID: row.HoldID, Reason: row.Reason,
		UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339Nano), UpdatedBy: row.UpdatedBy,
		SignedAt: row.SignedAt.UTC().Format(time.RFC3339Nano), Sig: row.Sig,
	}
}

// holdRefusal is every refusal of the hold's routes: its code, in error as RFC 6749 and the
// operator's client read it, and what was refused, with the hold row it was refused against.
type holdRefusal struct {
	// The refusal's code: bad_request, too_large, bad_signature, bad_transition, not_operator,
	// no_row, gen_mismatch, hold_mismatch, row_exists or below_floor.
	Error string `json:"error"`
	// What was refused.
	Message string `json:"message"`
	// The hold row as the refused write read it, as GET /v1/nats-hold answers it, on a 409 the row
	// decides; absent when there is no row.
	Current *holdRow `json:"current,omitempty"`
	// On POST /v1/nats-hold/restore's 409 gen_mismatch, the gen a restore must carry now, one above
	// every gen nats_hold_log holds, which the operator signs afresh; absent otherwise.
	RestoreGen *int64 `json:"restore_gen,omitempty"`
}

// writeHoldRefusal answers the hold routes' refusal of code with message, carrying the row and
// the gen a restore must carry when conflict names them. It is the routes' one refusal writer:
// their bodies name the code in error, as the operator's client reads it, rather than the
// broker's {code, error}.
func writeHoldRefusal(w http.ResponseWriter, status int, code, message string, conflict *natshold.Conflict) {
	body := holdRefusal{Error: code, Message: message}
	if conflict != nil {
		if conflict.Current != nil {
			current := holdRowAnswer(*conflict.Current)
			body.Current = &current
		}
		if conflict.RestoreGen != 0 {
			gen := int64(conflict.RestoreGen)
			body.RestoreGen = &gen
		}
	}
	writeJSON(w, status, body)
}

// readHoldBody decodes r's body into v as readJSON does, at most maxJSONBodyBytes (413
// too_large), no field v does not declare and nothing after the value (400 bad_request),
// answering in the hold routes' refusal.
func readHoldBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(v)
	if err == nil && decoder.More() {
		err = errors.New("body must contain one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeHoldRefusal(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", maxJSONBodyBytes), nil)
	default:
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "body must be one valid JSON object: "+err.Error(), nil)
	}
	return false
}

// holdSignedAt reads a body's signed_at: RFC 3339 at a whole microsecond, the precision a
// timestamptz stores, since a row stored at another time than the one signed never verifies.
func holdSignedAt(text string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !at.Equal(at.Truncate(time.Microsecond)) {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// readNATSHold answers the hold row, 404 no_row while there is none.
func (s *server) readNATSHold(w http.ResponseWriter, r *http.Request, _ string) {
	row, found, err := s.holdStore().Row(r.Context())
	switch {
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the hold row", err)
		return
	case !found:
		writeHoldRefusal(w, http.StatusNotFound, "no_row", "there is no hold row; the operator restores it through POST /v1/nats-hold/restore", nil)
		return
	}
	writeJSON(w, http.StatusOK, holdRowAnswer(row))
}

// holdMoveBody is PUT /v1/nats-hold's body.
type holdMoveBody struct {
	// The gen the row holds, which the move stores one above.
	ExpectedGen int64 `json:"expected_gen"`
	// The phase to move to: released, prepare or held.
	Phase string `json:"phase"`
	// The hold's id: a new one to prepare, the row's own to hold or release.
	HoldID string `json:"hold_id"`
	// Why the operator moves the row.
	Reason string `json:"reason"`
	// When the operator signed the move, RFC 3339 at a whole microsecond.
	SignedAt string `json:"signed_at"`
	// The operator's signature over the row's lines at expected_gen + 1 (signedrecord.HoldRow).
	Sig string `json:"sig"`
}

// moveNATSHold moves the hold row, once its signature verifies over the gen it will store.
func (s *server) moveNATSHold(w http.ResponseWriter, r *http.Request, operator string) {
	var body holdMoveBody
	if !readHoldBody(w, r, &body) {
		return
	}
	signedAt, ok := holdSignedAt(body.SignedAt)
	if body.ExpectedGen < 0 || !ok {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "expected_gen is a gen, at least 0, and signed_at an RFC 3339 time at a whole microsecond", nil)
		return
	}
	gen := uint64(body.ExpectedGen) + 1
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, body.Sig, signedrecord.HoldRow(gen, body.Phase, body.HoldID, signedAt)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at the gen it would store", nil)
		return
	}
	row, err := s.holdStore().Move(r.Context(), uint64(body.ExpectedGen), natshold.Row{
		Phase: body.Phase, HoldID: body.HoldID, Reason: body.Reason, UpdatedBy: operator, SignedAt: signedAt, Sig: body.Sig,
	})
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "move the hold row", err)
		return
	case errors.Is(err, natshold.ErrNoRow):
		writeHoldRefusal(w, http.StatusNotFound, "no_row", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		writeHoldRefusal(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrBadTransition):
		writeHoldRefusal(w, http.StatusBadRequest, "bad_transition", err.Error(), conflict)
		return
	default:
		writeInternal(w, "move the hold row", err)
		return
	}
	writeJSON(w, http.StatusOK, holdRowAnswer(row))
}

// holdRestoreBody is POST /v1/nats-hold/restore's body.
type holdRestoreBody struct {
	// The gen to restore at: one above every gen nats_hold_log holds, 1 for an empty log.
	Gen int64 `json:"gen"`
	// released, or held inside a hold.
	Phase string `json:"phase"`
	// The hold's id, which a held row names.
	HoldID string `json:"hold_id"`
	// Why the operator restores the row.
	Reason string `json:"reason"`
	// When the operator signed the row, RFC 3339 at a whole microsecond.
	SignedAt string `json:"signed_at"`
	// The operator's signature over the row's lines at gen (signedrecord.HoldRow).
	Sig string `json:"sig"`
}

// restoreNATSHold writes the hold row while there is none, once its signature verifies.
func (s *server) restoreNATSHold(w http.ResponseWriter, r *http.Request, operator string) {
	var body holdRestoreBody
	if !readHoldBody(w, r, &body) {
		return
	}
	signedAt, ok := holdSignedAt(body.SignedAt)
	if body.Gen < 1 || !ok {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "gen is a gen, at least 1, and signed_at an RFC 3339 time at a whole microsecond", nil)
		return
	}
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, body.Sig, signedrecord.HoldRow(uint64(body.Gen), body.Phase, body.HoldID, signedAt)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at its gen", nil)
		return
	}
	row, err := s.holdStore().Restore(r.Context(), natshold.Row{
		Gen: uint64(body.Gen), Phase: body.Phase, HoldID: body.HoldID, Reason: body.Reason, UpdatedBy: operator, SignedAt: signedAt, Sig: body.Sig,
	})
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "restore the hold row", err)
		return
	case errors.Is(err, natshold.ErrBadTransition):
		writeHoldRefusal(w, http.StatusBadRequest, "bad_transition", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrRowExists):
		writeHoldRefusal(w, http.StatusConflict, "row_exists", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "restore the hold row", err)
		return
	}
	writeJSON(w, http.StatusOK, holdRowAnswer(row))
}

// holdRecordBody is PUT /v1/nats-hold/records' body.
type holdRecordBody struct {
	// The hold the record is of, which must be the hold row's.
	HoldID string `json:"hold_id"`
	// cohort, convert, done or selection.
	Kind string `json:"kind"`
	// The record's key within its hold and kind.
	Key string `json:"key"`
	// The record's value.
	Value json.RawMessage `json:"value"`
	// The gen the record holds, which the write stores one above; 0 inserts a record that is not
	// there.
	ExpectedGen int64 `json:"expected_gen"`
	// The operator's signature over the record's lines at expected_gen + 1 (signedrecord.Record).
	Sig string `json:"sig"`
}

// holdRecord is a record as PUT /v1/nats-hold/records answers the one it stored.
type holdRecord struct {
	// The hold the record is of.
	HoldID string `json:"hold_id"`
	// cohort, convert, done or selection.
	Kind string `json:"kind"`
	// The record's key within its hold and kind.
	Key string `json:"key"`
	// The record's value, as Postgres holds it: its meaning is the value written, its keys
	// reordered and its white space dropped, which its signature's lines hash past.
	Value json.RawMessage `json:"value"`
	// The record's gen.
	Gen int64 `json:"gen"`
	// When the broker stored the record, RFC 3339 in UTC.
	WrittenAt string `json:"written_at"`
	// The operator's signature over the record's lines (signedrecord.Record).
	Sig string `json:"sig"`
}

// putNATSHoldRecord writes one record of the hold row's hold, once its signature verifies over
// the gen it will store.
func (s *server) putNATSHoldRecord(w http.ResponseWriter, r *http.Request, _ string) {
	var body holdRecordBody
	if !readHoldBody(w, r, &body) {
		return
	}
	if !natshold.RecordKind(body.Kind) || body.Key == "" || len(body.Value) == 0 || body.ExpectedGen < 0 {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "a record names its hold, its kind (cohort, convert, done or selection), its key and its value, at an expected_gen of at least 0", nil)
		return
	}
	gen := uint64(body.ExpectedGen) + 1
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, body.Sig, signedrecord.Record(body.HoldID, body.Kind, body.Key, gen, body.Value)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the record at the gen it would store", nil)
		return
	}
	floorKey := recordFloorKey{holdID: body.HoldID, kind: body.Kind, key: body.Key}
	if floor, above := s.holdFloors.above(floorKey, gen); !above {
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", fmt.Sprintf("this broker has written or served the record at gen %d, so a write storing gen %d is a replay", floor, gen), nil)
		return
	}
	stored, err := s.holdStore().PutRecord(r.Context(), natshold.Record{
		HoldID: body.HoldID, Kind: body.Kind, Key: body.Key, Value: body.Value, Sig: body.Sig,
	}, uint64(body.ExpectedGen))
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "write the hold record", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		writeHoldRefusal(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "write the hold record", err)
		return
	}
	s.holdFloors.raise(floorKey, stored.Gen)
	writeJSON(w, http.StatusOK, holdRecord{
		HoldID: stored.HoldID, Kind: stored.Kind, Key: stored.Key, Value: stored.Value, Gen: int64(stored.Gen),
		WrittenAt: stored.WrittenAt.UTC().Format(time.RFC3339Nano), Sig: stored.Sig,
	})
}

// holdRecordEntry is one record GET /v1/nats-hold/records lists, of the hold and kind it named.
type holdRecordEntry struct {
	// The record's key within its hold and kind.
	Key string `json:"key"`
	// The record's value, as Postgres holds it.
	Value json.RawMessage `json:"value"`
	// The record's gen.
	Gen int64 `json:"gen"`
	// When the broker stored the record, RFC 3339 in UTC.
	WrittenAt string `json:"written_at"`
	// The operator's signature over the record's lines (signedrecord.Record).
	Sig string `json:"sig"`
}

// holdRecords is GET /v1/nats-hold/records' answer.
type holdRecords struct {
	// Every record of the hold and kind the query named, by key; empty when there is none.
	Records []holdRecordEntry `json:"records"`
}

// readNATSHoldRecords lists one hold's records of one kind, none below its floor and none missing
// that its floor has seen (recordFloors): either is 409 below_floor, which the operator's client
// reads as unknown. A read that meets the floor is made once more first, since a write that
// committed while it read moves the floor past what it read; an older record put back, or a record
// deleted outside the routes, meets it again. Only a record whose signature verifies raises its
// floor; one that does not is answered as it is, and the client refuses it by its signature.
func (s *server) readNATSHoldRecords(w http.ResponseWriter, r *http.Request, _ string) {
	holdID, kind := r.URL.Query().Get("hold_id"), r.URL.Query().Get("kind")
	if holdID == "" || !natshold.RecordKind(kind) {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "the query names hold_id and a kind: cohort, convert, done or selection", nil)
		return
	}
	verified := func(rec natshold.Record) bool {
		return signedrecord.Verify(s.deps.NATSHold.Keys, rec.Sig, signedrecord.Record(holdID, kind, rec.Key, rec.Gen, rec.Value)) == nil
	}
	var records []natshold.Record
	for attempt := 1; ; attempt++ {
		var err error
		records, err = s.holdStore().Records(r.Context(), holdID, kind)
		if err != nil {
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the hold records", err)
			return
		}
		refusal := s.holdFloors.serve(holdID, kind, records, verified)
		if refusal == "" {
			break
		}
		if attempt == 2 {
			writeHoldRefusal(w, http.StatusConflict, "below_floor", refusal, nil)
			return
		}
	}
	answer := holdRecords{Records: []holdRecordEntry{}}
	for _, rec := range records {
		answer.Records = append(answer.Records, holdRecordEntry{
			Key: rec.Key, Value: rec.Value, Gen: int64(rec.Gen), WrittenAt: rec.WrittenAt.UTC().Format(time.RFC3339Nano), Sig: rec.Sig,
		})
	}
	writeJSON(w, http.StatusOK, answer)
}

// recordFloors is a broker process's floor of each hold record: the highest gen of it the routes
// have written, deleted or served verified, and whether the routes deleted it. A database writer
// other than the operator cannot write a record that verifies, but can put back an older one the
// operator signed, which does, or delete one: the floor answers either as unknown (GET's 409
// below_floor) and refuses a write that would store a gen at or below it again (409
// gen_mismatch), so the operator's commands never act on a journal a command has moved past. A
// record the floor has seen and the routes did not delete is a tombstone once it is absent, since
// the absence is a database writer's; a record the routes deleted (the cohort, at the gen its
// delete was signed over) is absent as it should be, and reads as unknown if it comes back. Only a
// record whose signature verifies raises its floor, so a writer's unsigned record at a high gen
// moves nothing.
//
// The residuals, which the hold's design accepts:
//
//   - Each broker replica keeps its own floor, for as long as its process lives. A broker-database
//     writer can put back an older journal on a replica whose floor is below the put-back gen, at
//     any time, not only after a restart; production runs two replicas. A floor both replicas
//     share, kept outside the database (one Secret Manager secret the broker alone writes), is the
//     fix if that acceptance changes. A replica that starts again, or one that has not yet written
//     or served a record, has no floor for it at all. The auth service's floor of the hold row
//     (natscallout's holdView) is per replica too, but every connection decision reads the row, so
//     both replicas track its newest gen.
//   - After a legitimate restore of the broker's database to an earlier point, every broker
//     answers each record it has seen at a later gen 409 below_floor until it restarts.
//   - A kind with several keys put back as an older set that lacks a key is caught only by the
//     tombstone of the missing key; the keys put back at gens the floor has not passed read as
//     genuine.
//   - Cutover rows carry no gen of their own, so the routes keep no floor for them: an older signed
//     cutover row put back reads as genuine, on any replica.
//   - A token taken at CONNECT can read every hold route. The operator's platform token is the
//     NATS CONNECT password, and the routes take it as their bearer for its life, at most an hour:
//     every write needs the operator's signature as well, but no read does, so whoever holds the
//     token reads the hold row, the journal, the cohort and the cutover rows.
//
// Safe for concurrent use.
type recordFloors struct {
	mu   sync.Mutex
	gens map[recordFloorKey]recordFloor
}

// recordFloorKey names one hold record.
type recordFloorKey struct{ holdID, kind, key string }

// recordFloor is one record's floor: its gen, and whether the routes deleted the record at it.
type recordFloor struct {
	gen     uint64
	deleted bool
}

func newRecordFloors() *recordFloors { return &recordFloors{gens: map[recordFloorKey]recordFloor{}} }

// above answers the record's floor (0 for none), and whether gen, the gen a write would store, is
// above it.
func (f *recordFloors) above(k recordFloorKey, gen uint64) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gens[k].gen, gen > f.gens[k].gen
}

// raise moves the record's floor up to gen, a gen the routes stored, with the record present.
func (f *recordFloors) raise(k recordFloorKey, gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gens[k] = recordFloor{gen: max(f.gens[k].gen, gen)}
}

// deleted moves the record's floor up to gen, the gen the routes deleted it at, and marks it
// deleted, so its absence is no tombstone and its return is a put-back.
func (f *recordFloors) deleted(k recordFloorKey, gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gens[k] = recordFloor{gen: max(f.gens[k].gen, gen), deleted: true}
}

// serve checks the records one read of holdID's kind answered against their floors, and answers
// why the read reads as unknown, or "" and raises the floor of every record verified accepts to
// its gen. A read reads as unknown when a record is below its floor, when a record the routes
// deleted is back, and when a record the floor has seen, and the routes did not delete, is absent.
func (f *recordFloors) serve(holdID, kind string, records []natshold.Record, verified func(natshold.Record) bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	present := map[string]bool{}
	for _, rec := range records {
		present[rec.Key] = true
		floor, seen := f.gens[recordFloorKey{holdID, kind, rec.Key}]
		switch {
		case seen && floor.deleted:
			return fmt.Sprintf("the %s record %s of hold %s, which the routes deleted at gen %d, is back at gen %d: a deleted record put back, which reads as unknown",
				kind, rec.Key, holdID, floor.gen, rec.Gen)
		case seen && rec.Gen < floor.gen:
			return fmt.Sprintf("the %s record %s of hold %s is at gen %d, below gen %d, which this broker has written or served: an older record put back, which reads as unknown",
				kind, rec.Key, holdID, rec.Gen, floor.gen)
		}
	}
	for k, floor := range f.gens {
		if k.holdID == holdID && k.kind == kind && !floor.deleted && !present[k.key] {
			return fmt.Sprintf("the %s record %s of hold %s, which this broker has written or served at gen %d, is absent: a record deleted outside the routes, which reads as unknown",
				kind, k.key, holdID, floor.gen)
		}
	}
	for _, rec := range records {
		if verified(rec) {
			f.gens[recordFloorKey{holdID, kind, rec.Key}] = recordFloor{gen: rec.Gen}
		}
	}
	return ""
}

// deleteNATSHoldCohort deletes the cohort record the query names (?hold_id=, ?kind=cohort, ?key=,
// ?gen=, ?sig=), for hold abort from prepare, its one caller: once the operator's signature over
// the record's delete at that gen verifies (signedrecord.RecordDelete), only while the row is
// prepare with that hold id, and only the record at that gen, so neither the operator's token alone
// nor an old signature deletes a record.
func (s *server) deleteNATSHoldCohort(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	holdID, kind, key := query.Get("hold_id"), query.Get("kind"), query.Get("key")
	gen, err := strconv.ParseUint(query.Get("gen"), 10, 63)
	if holdID == "" || kind != natshold.KindCohort || key == "" || err != nil || gen == 0 {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "the query names hold_id, kind=cohort, the one kind of record a delete takes, its key, the gen it deletes (at least 1) and the sig over them", nil)
		return
	}
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, query.Get("sig"), signedrecord.RecordDelete(holdID, kind, key, gen)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the record's delete at the gen it names", nil)
		return
	}
	err = s.holdStore().DeleteCohort(r.Context(), holdID, key, gen)
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "delete the cohort", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		writeHoldRefusal(w, http.StatusConflict, "hold_mismatch", err.Error()+": the cohort is deleted only while the row is prepare with its hold id", conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "delete the cohort", err)
		return
	}
	// The floor stays at the deleted gen, marked deleted: its absence is the route's, and a write
	// or a database writer putting an older signed cohort back is refused.
	s.holdFloors.deleted(recordFloorKey{holdID: holdID, kind: kind, key: key}, gen)
	w.WriteHeader(http.StatusNoContent)
}

// holdCutoverBody is PUT /v1/nats-hold/cutover's body.
type holdCutoverBody struct {
	// The family: session, dispatch, or daemon/<project key>.
	Family string `json:"family"`
	// done or rolled_back.
	State string `json:"state"`
	// The hold the row is written under, which must be the held row's.
	HoldID string `json:"hold_id"`
	// The operator's signature over the row's lines with hold_gen the hold row's current gen
	// (signedrecord.Cutover).
	Sig string `json:"sig"`
}

// holdCutover is one family's cutover row, as PUT /v1/nats-hold/cutover answers the one it
// stored and GET lists each.
type holdCutover struct {
	// The family: session, dispatch, or daemon/<project key>.
	Family string `json:"family"`
	// done or rolled_back.
	State string `json:"state"`
	// The hold the row was written under.
	HoldID string `json:"hold_id"`
	// The hold row's gen when the row was written.
	HoldGen int64 `json:"hold_gen"`
	// When the broker stored the row, RFC 3339 in UTC.
	WrittenAt string `json:"written_at"`
	// The account whose token wrote the row: the operator entry's.
	WrittenBy string `json:"written_by"`
	// The operator's signature over the row's lines (signedrecord.Cutover).
	Sig string `json:"sig"`
}

// holdCutoverAnswer is c as the routes answer it.
func holdCutoverAnswer(c natshold.Cutover) holdCutover {
	return holdCutover{
		Family: c.Family, State: c.State, HoldID: c.HoldID, HoldGen: int64(c.HoldGen),
		WrittenAt: c.WrittenAt.UTC().Format(time.RFC3339Nano), WrittenBy: c.WrittenBy, Sig: c.Sig,
	}
}

// holdCutoverFamily reports whether family names a family a hold converts.
func holdCutoverFamily(family string) bool {
	switch family {
	case "session", "dispatch":
		return true
	}
	key, daemon := strings.CutPrefix(family, "daemon/")
	return daemon && key != ""
}

// putNATSHoldCutover writes one family's cutover row while the hold row is held with the body's
// hold id, once its signature verifies over that row's current gen.
func (s *server) putNATSHoldCutover(w http.ResponseWriter, r *http.Request, operator string) {
	var body holdCutoverBody
	if !readHoldBody(w, r, &body) {
		return
	}
	if !holdCutoverFamily(body.Family) || (body.State != natshold.CutoverDone && body.State != natshold.CutoverRolledBack) {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "a cutover row names its family (session, dispatch or daemon/<project key>) and its state (done or rolled_back)", nil)
		return
	}
	stored, err := s.holdStore().PutCutover(r.Context(), natshold.Cutover{
		Family: body.Family, State: body.State, HoldID: body.HoldID, WrittenBy: operator, Sig: body.Sig,
	}, func(holdGen uint64) error {
		return signedrecord.Verify(s.deps.NATSHold.Keys, body.Sig, signedrecord.Cutover(body.Family, body.State, body.HoldID, holdGen))
	})
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case isSignatureRefusal(err):
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at the hold row's current gen", nil)
		return
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "write the cutover row", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		writeHoldRefusal(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "write the cutover row", err)
		return
	}
	writeJSON(w, http.StatusOK, holdCutoverAnswer(stored))
}

// isSignatureRefusal reports whether err is signedrecord's refusal of a signature or of the
// fields it covers.
func isSignatureRefusal(err error) bool {
	return errors.Is(err, signedrecord.ErrNoSignature) || errors.Is(err, signedrecord.ErrBadSignature) || errors.Is(err, signedrecord.ErrBadField)
}

// holdCutovers is GET /v1/nats-hold/cutover's answer.
type holdCutovers struct {
	// Every family's cutover row, by family; empty when there is none.
	Families []holdCutover `json:"families"`
}

// readNATSHoldCutovers lists every family's cutover row.
func (s *server) readNATSHoldCutovers(w http.ResponseWriter, r *http.Request, _ string) {
	cutovers, err := s.holdStore().Cutovers(r.Context())
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the cutover rows", err)
		return
	}
	answer := holdCutovers{Families: []holdCutover{}}
	for _, c := range cutovers {
		answer.Families = append(answer.Families, holdCutoverAnswer(c))
	}
	writeJSON(w, http.StatusOK, answer)
}
