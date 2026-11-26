package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// NATSHold is what the NATS writer hold's routes need: the tables they write
// (internal/cipher/natshold, migration 0044_nats_hold), the operator's public keys every write
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
	// verifies a service's (cmd/cipherd passes natscallout.PlatformTokens.Verify), and answers what
	// it says of its account (PlatformIdentity). Nil, a broker with no NATS audience, admits no
	// caller.
	OperatorToken func(ctx context.Context, raw string) (PlatformIdentity, error)
	// OperatorAccount and OperatorGoogle are the operator entry's account and whether it is a
	// Google account: the one caller the routes admit. An empty OperatorAccount, a list with no
	// operator entry, admits none. OperatorIssuers are the names of the issuers of
	// BROKER_K8S_OIDC_ISSUER a Kubernetes operator's token is taken from (config.NATSService's
	// Issuers: those the entry names, or the first issuer alone where it names none), as the auth
	// service takes it: the same account name in another cluster is another ServiceAccount.
	OperatorAccount string
	OperatorGoogle  bool
	OperatorIssuers []string
	// RowFloor is this process's one floor of the hold row (signedrecord.Floor): the routes put
	// every verified row they serve, write or restore through it, and the auth service's hold view
	// (natscallout.NewHoldView) puts every row it reads through the same one, so neither half of the
	// process trusts a row the other has moved past. cmd/cipherd makes one; nil gives the routes a
	// floor of their own, as a test that runs no view does.
	RowFloor *signedrecord.Floor
}

// PlatformIdentity is what a verified platform identity token says of its account, as the auth
// service's verifier answers it (natscallout.PlatformTokens.Verify). Both of its readers read this
// one struct: the auth service (natscallout, as ServiceIdentity) of a service's token, and the
// hold routes (NATSHold.OperatorToken) of the operator's.
type PlatformIdentity struct {
	// Account is the account the token names.
	Account string
	// Google is whether Account is a Google account.
	Google bool
	// Issuer is the name of the issuer of BROKER_K8S_OIDC_ISSUER that signed a Kubernetes token:
	// "" for a Google token, and for the one issuer of a bare URL.
	Issuer string
	// Issued and Expiry are when the token was issued and when it expires.
	Issued, Expiry time.Time
}

// TakenFrom reports whether an entry whose issuer names are issuers takes p's token: a Google
// token always, since a Google entry names no issuer, and a Kubernetes token only from an issuer
// the entry names, since the same account name in another cluster is another ServiceAccount, which
// the entry does not list. It is the one issuer rule of every entry that lists an account: the
// operator entry's (operatorCaller), a BROKER_API_ACCOUNTS entry's and an edge listener's
// (issuerNamed), and every BROKER_NATS_SERVICES entry's at the auth service (natscallout).
func (p PlatformIdentity) TakenFrom(issuers []string) bool {
	return p.Google || slices.Contains(issuers, p.Issuer)
}

// longestOperatorToken is the longest life the operator's token may have from its issue, the bound
// the auth service holds every service's platform token to (natscallout's longestServiceToken): a
// token login's, beside the proof clock skew. The operator's commands mint ten minutes.
const longestOperatorToken = machine.MaxTokenLoginLifetime + ciphertoken.ProofClockSkew

// operatorCaller authenticates the operator's own platform token for authenticate, the one
// credential operatorAuth's routes take: verified as the auth service verifies a service's, no
// longer lived than an hour from its issue and not expired, naming the operator entry's account
// and, for a Kubernetes token, signed by an issuer that entry names, since the same account name
// in another cluster is another ServiceAccount. Anything else is 403 not_operator, whatever was
// wrong with it, so a caller learns nothing of the operator's identity from the refusal, and none
// repeats the token: a token of the operator's account name from another issuer is refused as
// every other account's is.
func (s *server) operatorCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	hold := s.deps.NATSHold
	if hold.OperatorToken == nil || hold.OperatorAccount == "" {
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "this broker admits no operator: BROKER_NATS_SERVICES lists no operator entry, or no NATS audience verifies its token", nil)
		return caller{}, false
	}
	identity, err := hold.OperatorToken(r.Context(), bearer(r))
	switch {
	case err != nil:
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token does not verify as a platform token of the NATS audiences", nil)
		return caller{}, false
	case identity.Issued.IsZero() || identity.Expiry.Sub(identity.Issued) > longestOperatorToken:
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token names no issue time or lives longer than an hour from it", nil)
		return caller{}, false
	case !identity.Expiry.After(time.Now()):
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token has expired", nil)
		return caller{}, false
	case identity.Account != hold.OperatorAccount || identity.Google != hold.OperatorGoogle ||
		!identity.TakenFrom(hold.OperatorIssuers):
		writeHoldRefusal(w, http.StatusForbidden, "not_operator", "the caller's token names an account other than the operator entry's", nil)
		return caller{}, false
	}
	return caller{operator: identity.Account}, true
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
	// The operator's Ed25519 signature over the row's lines, as signedrecord.Sign writes it: the
	// signing key's id, a dot, and the signature, base64url without padding.
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
	// no_row, gen_mismatch, hold_mismatch, row_exists, below_floor or clock_skew.
	Error string `json:"error"`
	// What was refused.
	Message string `json:"message"`
	// The hold row as the refused write read it, as GET /v1/nats-hold answers it, on a 409 the row
	// decides; absent when there is no row.
	Current *holdRow `json:"current,omitempty"`
	// On POST /v1/nats-hold/restore's 409 gen_mismatch, the gen a restore must carry now, one above
	// every gen nats_hold_log holds, which the operator signs afresh; absent otherwise.
	RestoreGen *int64 `json:"restore_gen,omitempty"`
	// On a 409 hold_mismatch, the hold id the hold row or log names, which the refused write must
	// name instead (a restore after a prepare entry names that entry's hold); absent otherwise. The
	// operator's client reads it as RouteError.HoldID, since no route lists the log.
	HoldID string `json:"hold_id,omitempty"`
}

// writeHoldRefusal answers the hold routes' refusal of code with message, carrying the row and
// the gen a restore must carry when conflict names them. It is the routes' one refusal writer
// (one that carries a row goes through writeHoldConflict first): their bodies name the code in
// error, as the operator's client reads it, rather than the broker's {code, error}.
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
		if code == "hold_mismatch" {
			body.HoldID = conflict.HoldID
		}
	}
	writeJSON(w, status, body)
}

// writeHoldConflict is writeHoldRefusal for a refusal that carries the row a write read
// (conflict.Current). That row goes through this broker's floor of the row first (acceptRow), as
// GET's does, so no route hands out a row the floor reads as a replay or a fork: the answer is then
// 409 below_floor with no row, which the operator's client reads as unknown. A hold_mismatch names
// the hold the row names (conflict.HoldID) only when that row verifies: a hold id from an unsigned
// row is a database writer's choice, so the refusal then names none.
func (s *server) writeHoldConflict(w http.ResponseWriter, status int, code, message string, conflict *natshold.Conflict) {
	if conflict != nil && conflict.Current != nil {
		if err := s.acceptRow(*conflict.Current); err != nil {
			writeHoldRefusal(w, http.StatusConflict, "below_floor", fmt.Sprintf("refused %s against a hold row that reads as unknown: %v: an older row put back, or another row at the gen this broker served or wrote", code, err), nil)
			return
		}
		if conflict.HoldID != "" && signedrecord.Verify(s.deps.NATSHold.Keys, conflict.Current.Sig, holdRowLines(*conflict.Current)) != nil {
			named := *conflict
			named.HoldID = ""
			conflict = &named
		}
	}
	writeHoldRefusal(w, status, code, message, conflict)
}

// readHoldBody decodes r's body into v as readJSON does, at most maxJSONBodyBytes (413
// too_large), no field v does not declare and nothing after the value (400 bad_request),
// answering in the hold routes' refusal, whose message names the field and the kind of failure
// (holdBodyFault), never a value the body holds.
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
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "body must be one valid JSON object: "+holdBodyFault(err), nil)
	}
	return false
}

// holdBodyFault is err, a JSON decoder's, as the kind of failure alone: a syntax error at its
// byte, a field whose value does not decode to its Go type, an unknown field, a body that ends
// early or holds more than one value. json's own message holds a number's digits when it is out of
// range and a character of the input at a syntax error, and a hold body holds a record's value, so
// the message is never json's.
func holdBodyFault(err error) string {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("a JSON syntax error at byte %d", syntax.Offset)
	case errors.As(err, &typed) && typed.Field != "":
		return fmt.Sprintf("the field %q does not decode to Go %s", typed.Field, typed.Type)
	case errors.As(err, &typed):
		return "the body is not a JSON object"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the body ends before its JSON value does"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "it holds a field the route does not take"
	}
	return "the body holds more than one JSON value, or does not decode"
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

// readNATSHold answers the hold row, 404 no_row while there is none, and 409 below_floor for a
// verified row this broker's floor of the row (holdRowFloor) reads as a replay or a fork, which the
// operator's client reads as unknown. A row that does not verify is answered as it is, and the
// client refuses it by its signature. A read that meets the floor is made once more first, since a
// move that committed while it read moves the floor past what it read.
func (s *server) readNATSHold(w http.ResponseWriter, r *http.Request, _ string) {
	for attempt := 1; ; attempt++ {
		row, found, err := s.holdStore().Row(r.Context())
		switch {
		case err != nil:
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the hold row", err)
			return
		case !found:
			writeHoldRefusal(w, http.StatusNotFound, "no_row", "there is no hold row; the operator restores it through POST /v1/nats-hold/restore", nil)
			return
		}
		err = s.acceptRow(row)
		if err == nil {
			writeJSON(w, http.StatusOK, holdRowAnswer(row))
			return
		}
		if attempt == 2 {
			writeHoldRefusal(w, http.StatusConflict, "below_floor", fmt.Sprintf("the hold row at gen %d reads as unknown: %v: an older row put back, or another row at the gen this broker served or wrote", row.Gen, err), nil)
			return
		}
	}
}

// holdRowLines are row's signed lines (signedrecord.HoldRow).
func holdRowLines(row natshold.Row) []string {
	return signedrecord.HoldRow(row.Gen, row.Phase, row.HoldID, row.SignedAt)
}

// acceptRow puts row, read from the table, through this broker's floor of the row when a key of
// BROKER_NATS_HOLD_SIGNING_KEYS signed it, and answers the floor's ErrReplay, ErrFork or
// ErrBackdated; a row that does not verify moves nothing and is answered nil, for its reader to
// refuse.
func (s *server) acceptRow(row natshold.Row) error {
	if signedrecord.Verify(s.deps.NATSHold.Keys, row.Sig, holdRowLines(row)) != nil {
		return nil
	}
	return s.holdRowFloor.Accept(row.Gen, row.SignedAt, holdRowLines(row))
}

// errWrittenUnverified is a write whose row, as the store answered it (its RETURNING row), does
// not verify: a database trigger, say, changed it between the write and the answer.
var errWrittenUnverified = errors.New("the row the store answered for the write does not verify")

// acceptWritten puts row, the row a move or restore wrote as the store answered it, through this
// broker's floor of the row once it verifies, and answers errWrittenUnverified otherwise, which
// moves nothing: an unverified answer, a gen a trigger raised among them, would wedge the floor
// above every genuine row until the process starts again.
func (s *server) acceptWritten(row natshold.Row) error {
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, row.Sig, holdRowLines(row)); err != nil {
		return fmt.Errorf("%w: gen %d: %w", errWrittenUnverified, row.Gen, err)
	}
	return s.holdRowFloor.Accept(row.Gen, row.SignedAt, holdRowLines(row))
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

// errRowSkewed is a row a move or restore would write whose signed_at the broker refuses: more than
// signedrecord.SignedAtSkew from the broker's own clock, or more than that before the latest row
// this broker's floor of the row accepted, or the latest signing time among the log's verified
// entries, which every replica shares. It is answered 409 clock_skew, naming both times and the gen
// compared with, and nothing is stored: a row signed by a clock far fast, once stored, would raise
// the floor past every genuine move, and one signed far behind would be stored and then read as
// unknown, on this replica or on another whose floor took a later time.
var errRowSkewed = errors.New("the row's signed_at is outside the clock-skew allowance")

// rowSignedAtAdmitted judges the signed_at of a row a move or restore would write at gen, before
// the write, given log, every log entry as the write's transaction reads it: against the broker's
// own clock, against this broker's floor of the row (signedrecord.Floor.Admits, which moves
// nothing), and against the latest signing time among the log's verified entries
// (signedrecord.NotBackdated), so that, while every clock keeps the hold's requirement, a row one
// replica stores never reads as unknown on the other. Outside it, a row can: while the log holds a
// time more than signedrecord.SignedAtSkew ahead of this broker's clock (a broker clock that jumped
// back, or replicas or operator machines further apart than the allowance), NotBackdated judges a
// new row against this clock instead, and the other replica, whose floor took the far time and
// whose clock does not put it that far ahead, reads the row stored here as unknown until the next
// move or until it starts again: fail-closed, on that replica alone. It answers errRowSkewed,
// naming both times and the gen compared with.
func (s *server) rowSignedAtAdmitted(gen uint64, signedAt time.Time, log []natshold.LogEntry) error {
	now := time.Now()
	if err := signedrecord.WithinSkewOf(signedAt, now); err != nil {
		return fmt.Errorf("%w: %w", errRowSkewed, err)
	}
	if err := s.holdRowFloor.Admits(gen, signedAt); err != nil {
		return fmt.Errorf("%w: %w", errRowSkewed, err)
	}
	if latest, ok := s.latestSigned(log); ok && gen > latest.Gen {
		if err := signedrecord.NotBackdated(gen, signedAt, latest.Gen, latest.SignedAt, now); err != nil {
			return fmt.Errorf("%w: %w", errRowSkewed, err)
		}
	}
	return nil
}

// verifiedLogEntry reports whether a key of BROKER_NATS_HOLD_SIGNING_KEYS signed e, the row a log
// entry carries.
func (s *server) verifiedLogEntry(e natshold.LogEntry) bool { return s.logEntryVerified(e) == nil }

// logEntryVerified is signedrecord.Verify's answer for e, the row a log entry carries, against
// BROKER_NATS_HOLD_SIGNING_KEYS: nil when a listed key signed it.
func (s *server) logEntryVerified(e natshold.LogEntry) error {
	return signedrecord.Verify(s.deps.NATSHold.Keys, e.Sig, signedrecord.HoldRow(e.Gen, e.Phase, e.HoldID, e.SignedAt))
}

// latestSigned is the log entry with the latest signing time among those that verify, and false
// when none does. It verifies the entries from the latest signing time down, so it stops at the
// first that verifies.
func (s *server) latestSigned(log []natshold.LogEntry) (natshold.LogEntry, bool) {
	bySignedAt := slices.Clone(log)
	slices.SortFunc(bySignedAt, func(a, b natshold.LogEntry) int { return b.SignedAt.Compare(a.SignedAt) })
	for _, e := range bySignedAt {
		if s.verifiedLogEntry(e) {
			return e, true
		}
	}
	return natshold.LogEntry{}, false
}

// moveNATSHold moves the hold row, once its signature verifies over the gen it will store, and
// only from a row this broker's floor of the row (holdRowFloor) accepts, checked on the row the
// move's own transaction holds: a move from an older row put back (a replay) is 409 gen_mismatch
// with no row, and from another row at the gen the floor took (a fork) 409 below_floor with no row,
// which the operator's client reads as unknown. A move whose gen the log already holds, from a row
// put back below the log on a replica whose floor never passed it, is 409 below_floor with no row.
// The new row's signed_at is judged in the same transaction, before the write (rowSignedAtAdmitted):
// one outside the allowance is 409 clock_skew, and nothing is stored.
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
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at the gen it would store: "+err.Error(), nil)
		return
	}
	accept := func(current natshold.Row, log []natshold.LogEntry) error {
		if err := s.acceptRow(current); err != nil {
			return err
		}
		return s.rowSignedAtAdmitted(gen, signedAt, log)
	}
	row, err := s.holdStore().Move(r.Context(), uint64(body.ExpectedGen), natshold.Row{
		Phase: body.Phase, HoldID: body.HoldID, Reason: body.Reason, UpdatedBy: operator, SignedAt: signedAt, Sig: body.Sig,
	}, accept)
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case errors.Is(err, errRowSkewed):
		writeHoldRefusal(w, http.StatusConflict, "clock_skew", "the move stores nothing: "+err.Error(), nil)
		return
	case errors.Is(err, signedrecord.ErrReplay):
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", fmt.Sprintf("the hold row the move starts from reads as unknown: %v: an older row put back", err), nil)
		return
	case errors.Is(err, signedrecord.ErrFork), errors.Is(err, signedrecord.ErrBackdated):
		writeHoldRefusal(w, http.StatusConflict, "below_floor", fmt.Sprintf("the hold row the move starts from reads as unknown: %v: another row at the gen this broker served, read or wrote, or a row of an earlier history", err), nil)
		return
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "move the hold row", err)
		return
	case errors.Is(err, natshold.ErrGenLogged):
		writeHoldRefusal(w, http.StatusConflict, "below_floor", err.Error(), nil)
		return
	case errors.Is(err, natshold.ErrNoRow):
		s.writeHoldConflict(w, http.StatusNotFound, "no_row", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrBadTransition):
		s.writeHoldConflict(w, http.StatusBadRequest, "bad_transition", err.Error(), conflict)
		return
	default:
		writeInternal(w, "move the hold row", err)
		return
	}
	// The stored row is above the row the move started from, which the floor accepted, so it raises
	// the floor once it verifies as the store answered it.
	if err := s.acceptWritten(row); err != nil {
		writeHoldRefusal(w, http.StatusConflict, "below_floor", "the move's row reads as unknown: "+err.Error(), nil)
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
// the gen it will store. A record the routes deleted stays deleted while its signed delete stands
// and verifies: putting its key again is 409 gen_mismatch, on every replica.
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
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the record at the gen it would store: "+err.Error(), nil)
		return
	}
	floorKey := recordFloorKey{holdID: body.HoldID, kind: body.Kind, key: body.Key}
	if floor, above := s.holdFloors.above(floorKey, gen); !above {
		writeHoldRefusal(w, http.StatusConflict, "gen_mismatch", fmt.Sprintf("this broker has written or served the record at gen %d, so a write storing gen %d is a replay", floor, gen), nil)
		return
	}
	deletedByRoutes := func(d natshold.Deletion) bool {
		return signedrecord.Verify(s.deps.NATSHold.Keys, d.Sig, signedrecord.RecordDelete(body.HoldID, body.Kind, d.Key, d.Gen)) == nil
	}
	stored, err := s.holdStore().PutRecord(r.Context(), natshold.Record{
		HoldID: body.HoldID, Kind: body.Kind, Key: body.Key, Value: body.Value, Sig: body.Sig,
	}, uint64(body.ExpectedGen), deletedByRoutes)
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "write the hold record", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "write the hold record", err)
		return
	}
	// The record as the store answered it raises its floor only once it verifies: a trigger that
	// changed it between the write and the answer would otherwise wedge the floor.
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, stored.Sig, signedrecord.Record(stored.HoldID, stored.Kind, stored.Key, stored.Gen, stored.Value)); err != nil {
		writeHoldRefusal(w, http.StatusConflict, "below_floor", fmt.Sprintf("the record the store answered for the write does not verify: gen %d: %v", stored.Gen, err), nil)
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
// floor; one that does not is answered as it is, and the client refuses it by its signature. The
// deletes the routes kept are read in the same snapshot as the records, and only one whose
// signature verifies is the routes' delete.
func (s *server) readNATSHoldRecords(w http.ResponseWriter, r *http.Request, _ string) {
	holdID, kind := r.URL.Query().Get("hold_id"), r.URL.Query().Get("kind")
	if holdID == "" || !natshold.RecordKind(kind) {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "the query names hold_id and a kind: cohort, convert, done or selection", nil)
		return
	}
	verified := func(rec natshold.Record) bool {
		return signedrecord.Verify(s.deps.NATSHold.Keys, rec.Sig, signedrecord.Record(holdID, kind, rec.Key, rec.Gen, rec.Value)) == nil
	}
	deletedByRoutes := func(d natshold.Deletion) bool {
		return signedrecord.Verify(s.deps.NATSHold.Keys, d.Sig, signedrecord.RecordDelete(holdID, kind, d.Key, d.Gen)) == nil
	}
	var records []natshold.Record
	for attempt := 1; ; attempt++ {
		var deletions []natshold.Deletion
		var err error
		records, deletions, err = s.holdStore().Records(r.Context(), holdID, kind)
		if err != nil {
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the hold records", err)
			return
		}
		refusal := s.holdFloors.serve(holdID, kind, records, deletions, verified, deletedByRoutes)
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

// errJournalUnknown is a cohort delete on a released row whose hold's done record this broker's
// floor of that record reads as unknown: gone, where the floor saw it present (a journal a database
// writer deleted, not one never written), or at a gen below the floor (an older signed version put
// back).
var errJournalUnknown = errors.New("the hold's done record is gone, or older than one this broker wrote or served")

// holdJournalKey is the done record's one key (natshold's journalKey, internal/holdrecord's
// journal).
const holdJournalKey = "hold"

// deleteNATSHoldCohort deletes the cohort record the query names (?hold_id=, ?kind=cohort, ?key=,
// ?gen=, ?sig=), for hold abort from prepare, and for hold abort on a row lost in prepare and
// restored released: once the operator's signature over the record's delete at that gen verifies
// (signedrecord.RecordDelete), only while the row is prepare with that hold id, or released with
// it while the table admits hold abort on the released row (holdrecord.Request.RowReleased): the
// hold's done record verifies and shows prepare, or the hold has no done record, which the table
// reads as the zero Done; in either case read against this broker's floor of that record, so a
// journal it saw present and is now gone, or one below its gen, is 409 below_floor; and only the
// record at that gen, so neither the operator's token alone nor an old signature deletes a record.
func (s *server) deleteNATSHoldCohort(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	holdID, kind, key := query.Get("hold_id"), query.Get("kind"), query.Get("key")
	gen, err := strconv.ParseUint(query.Get("gen"), 10, 63)
	if holdID == "" || kind != natshold.KindCohort || key == "" || err != nil || gen == 0 {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "the query names hold_id, kind=cohort, the one kind of record a delete takes, its key, the gen it deletes (at least 1) and the sig over them", nil)
		return
	}
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, query.Get("sig"), signedrecord.RecordDelete(holdID, kind, key, gen)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the record's delete at the gen it names: "+err.Error(), nil)
		return
	}
	released := func(journal *natshold.Record) error {
		floorKey := recordFloorKey{holdID: holdID, kind: natshold.KindDone, key: holdJournalKey}
		switch {
		case journal == nil && s.holdFloors.gone(floorKey):
			return fmt.Errorf("%w: hold %s's done record is gone", errJournalUnknown, holdID)
		case journal != nil && s.holdFloors.below(floorKey, journal.Gen):
			return fmt.Errorf("%w: hold %s's done record is at gen %d", errJournalUnknown, holdID, journal.Gen)
		}
		var d holdrecord.Done // no done record: the zero Done, a hold with no journal
		if journal != nil {
			var err error
			if d, err = s.journalDone(holdID, journal); err != nil {
				return &natshold.Conflict{Err: fmt.Errorf("%w: the row is released, and hold %s's done record does not verify: %v", natshold.ErrHoldMismatch, holdID, err)}
			}
		}
		if err := holdrecord.Decide(d, holdrecord.Request{Command: holdrecord.CommandAbort, RowReleased: true}); err != nil {
			return &natshold.Conflict{Err: fmt.Errorf("%w: the row is released, and hold %s's journal does not show prepare", natshold.ErrHoldMismatch, holdID)}
		}
		return nil
	}
	err = s.holdStore().DeleteCohort(r.Context(), holdID, key, gen, query.Get("sig"), released)
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case errors.Is(err, errJournalUnknown):
		writeHoldRefusal(w, http.StatusConflict, "below_floor", "the cohort is not deleted: "+err.Error(), nil)
		return
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "delete the cohort", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "hold_mismatch", err.Error()+": the cohort is deleted only while the row is prepare with its hold id, or released with it while that hold's journal shows prepare", conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "delete the cohort", err)
		return
	}
	// The floor stays at the deleted gen, marked deleted: its absence is the route's, and a write
	// or a database writer putting an older signed cohort back is refused. Every other replica
	// marks its own once it reads the delete the store kept.
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
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at the hold row's current gen: "+err.Error(), nil)
		return
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "write the cutover row", err)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
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
