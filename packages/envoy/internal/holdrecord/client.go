// Package holdrecord is the operator's client of the secrets broker's /v1/nats-hold routes, and the
// NATS writer hold's journal.
//
// The hold row, the hold's records and each family's cutover row live in the broker's Postgres,
// where no NATS user can write them, and each is signed with the operator's Ed25519 key
// (internal/bus/signedrecord), so a database writer other than the operator can write a row but
// not one that verifies. This client signs every write with that key and verifies every answer
// against the configured public keys: a row, record or cutover row that does not verify, or that
// comes without its signature, is ErrUnknown, never a phase or a record to act on.
//
// The journal is the hold's `done` record (kind `done`, key `hold`): the hold's workflow, its
// entries in order, and its completion. A command's admission and its `intent` entry are one
// compare-and-swap write (Admit), and Decide refuses ErrBusy while another command's intent is
// pending, so of two processes racing to admit two commands one is admitted and the other refused.
// A command's own pending intent admits its rerun, since a run cut by a crash leaves it, so two
// processes admitting the same command are both admitted and both past its intent. The hold's
// design assumes the operator runs one command of a hold at a time (A2), which the journal does
// not enforce: under A2 at most one command is past its intent at any time. Every command is
// Stoppable: one whose own check refuses after its intent writes a `stop` entry (AppendStop,
// AppendStopLeft), which ends the intent and records what the command left. `hold release` follows
// only a complete record or one whose stops all left nothing changed (LeftUnchanged), and `hold
// abort` is every workflow's way out once a command has stopped and no intent is pending, so a
// command that can never succeed cannot hold the hold. In hub-restore, after a done `hold prove`,
// abort is admitted only once the pending command's latest entry is a stop, and releases the row
// over the proven disks, which the hold's command compares with the prove's record (t-10's check):
// clients then reach the restored and proven hub before its census, until a streams-census hold
// runs clean. After a failed re-prove on a disk that moved, the operator restores that target back
// onto the done prove's disk before running abort. If the proven disk is gone, abort's check writes
// a stop naming it; the operator restores onto another disk and runs a new prove, and abort,
// admitted again after its own stop, releases over that new epoch's disks. The workflow table and
// Decide are the
// ones model_test.go explores: every workflow from its true start, with crashes between any two
// writes, two operator processes, restores landing outside the hold, the row lost and restored,
// and any command failing for good after its intent, in hub-restore the census and a prove after a
// done prove.
package holdrecord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// Config is the client's configuration. BrokerURL is the secrets broker's base URL, which every
// command that reaches the broker takes from its `--broker-url` flag. Token gives the operator's
// ServiceAccount token, with the audience `BROKER_NATS_K8S_AUDIENCE`. SigningKey is the private
// half of the operator's hold signing key, kept in memory only, and nil for a command that only
// reads. VerifyKeys are `BROKER_NATS_HOLD_SIGNING_KEYS`.
type Config struct {
	BrokerURL  string
	Token      func(context.Context) (string, error)
	SigningKey ed25519.PrivateKey
	VerifyKeys []ed25519.PublicKey
	HTTP       *http.Client
}

// Client is the operator's client of the broker's /v1/nats-hold routes. Each method signs its
// write with the configuration's SigningKey and verifies its answer against VerifyKeys.
type Client struct {
	cfg Config
}

// Row is the hold row. Its phase is natsgrant's type, the one the operator's grant is decided by.
type Row struct {
	Gen      uint64
	Phase    natsgrant.OperatorPhase
	HoldID   string
	SignedAt time.Time
}

// Record is one hold record, keyed by its hold id, kind and key.
type Record struct {
	HoldID string
	Kind   string
	Key    string
	Gen    uint64
	Value  json.RawMessage
}

// CutoverRow is one family's broker cutover record.
type CutoverRow struct {
	Family  string
	State   string
	HoldID  string
	HoldGen uint64
}

// RouteError is a route's 400, 403 or 409 answer, with its code: `bad_signature`,
// `bad_transition`, `not_operator`, `gen_mismatch`, `hold_mismatch`, `row_exists` or `below_floor`.
type RouteError struct {
	Status int
	Code   string
	Row    *Row
}

// RestoreGenError is a restore's 409 `gen_mismatch`: the route stores a restored row only at
// RestoreGen, one above the highest `gen` its log holds, which no command can read. Bootstrap
// answers it as it is, and so does RestoreRow for any row but a held one with its hold id. It
// unwraps to the route's RouteError.
type RestoreGenError struct {
	RestoreGen uint64
	Route      *RouteError
}

// Error names the gen a restore must carry.
func (e *RestoreGenError) Error() string {
	return fmt.Sprintf("holdrecord: 409 gen_mismatch: the route stores a restored row only at gen %d", e.RestoreGen)
}

// Unwrap is the route's RouteError.
func (e *RestoreGenError) Unwrap() error { return e.Route }

// Error names the answer's status and code.
func (e *RouteError) Error() string { return fmt.Sprintf("holdrecord: %d %s", e.Status, e.Code) }

// New refuses an empty BrokerURL, a nil Token and an empty VerifyKeys.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.BrokerURL == "":
		return nil, errors.New("holdrecord: the configuration names no broker URL")
	case cfg.Token == nil:
		return nil, errors.New("holdrecord: the configuration gives no way to get the operator's token")
	case len(cfg.VerifyKeys) == 0:
		return nil, errors.New("holdrecord: the configuration holds no key to verify an answer with, so every row would read as unknown")
	}
	if u, err := url.Parse(cfg.BrokerURL); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("holdrecord: the broker URL %q is not an absolute URL", cfg.BrokerURL)
	}
	for i, key := range cfg.VerifyKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("holdrecord: verify key %d is %d bytes, not an Ed25519 public key's %d", i+1, len(key), ed25519.PublicKeySize)
		}
	}
	return &Client{cfg: cfg}, nil
}

// VerifyKeys answers a copy of the keys the client verifies every answer under
// (Config.VerifyKeys), so a command that reads other records the operator signed, the session
// consumers' records copy among them, verifies them under the same keys. Changing the copy, a key's
// bytes included, changes nothing the client verifies with.
func (c *Client) VerifyKeys() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, len(c.cfg.VerifyKeys))
	for i, key := range c.cfg.VerifyKeys {
		out[i] = append(ed25519.PublicKey(nil), key...)
	}
	return out
}

// Barrier publishes a plain put to `$KV.<bucket>.hold.barrier.<holdID>` and returns once its
// acknowledgement arrives. Every held command calls it before its first read of each bucket.
func Barrier(ctx context.Context, js jetstream.JetStream, bucket, holdID string) error {
	if js == nil {
		return errors.New("holdrecord: the barrier needs a JetStream context")
	}
	for name, token := range map[string]string{"bucket": bucket, "hold id": holdID} {
		// Each is one subject token: the operator's held grant covers $KV.<bucket>.hold.barrier.*
		// alone, and a put of any other subject would be no barrier.
		if token == "" || strings.ContainsAny(token, ".*> \t\r\n") {
			return fmt.Errorf("holdrecord: the barrier's %s %q is not one subject token", name, token)
		}
	}
	subject := "$KV." + bucket + ".hold.barrier." + holdID
	// Publish returns with the stream's acknowledgement, which the stream's leader sends once it
	// has stored the put; every read of the bucket the command makes after that reads past it.
	if _, err := js.Publish(ctx, subject, []byte(holdID)); err != nil {
		return fmt.Errorf("holdrecord: the barrier put to %s: %w", subject, err)
	}
	return nil
}

// Row reads and verifies the row, through `GET /v1/nats-hold`.
func (c *Client) Row(ctx context.Context) (Row, error) {
	var answer rowWire
	if err := c.call(ctx, http.MethodGet, holdPath, nil, nil, &answer); err != nil {
		return Row{}, err
	}
	return c.verifyRow(answer)
}

// MoveRow moves the row through `PUT /v1/nats-hold`. It refuses any phase but
// `OperatorPhaseReleased`, `OperatorPhasePrepare` and `OperatorPhaseHeld`, the row's three.
func (c *Client) MoveRow(ctx context.Context, expectedGen uint64, phase natsgrant.OperatorPhase, holdID, reason string) (Row, error) {
	if err := rowPhase(phase); err != nil {
		return Row{}, err
	}
	signedAt, sig, err := c.signRow(expectedGen+1, phase, holdID)
	if err != nil {
		return Row{}, err
	}
	body := map[string]any{"expected_gen": expectedGen, "phase": phase, "hold_id": holdID, "reason": reason,
		"signed_at": signedAt.Format(time.RFC3339Nano), "sig": sig}
	var answer rowWire
	if err := c.call(ctx, http.MethodPut, holdPath, nil, body, &answer); err != nil {
		return Row{}, err
	}
	return c.writtenRow(answer, Row{Gen: expectedGen + 1, Phase: phase, HoldID: holdID, SignedAt: signedAt})
}

// RestoreRow writes a lost row through `POST /v1/nats-hold/restore`, refusing the same phases as
// MoveRow. A row restored `held` inside a hold keeps its hold id at a new `gen`, and the record's
// completion stands, because no command is admitted while the row is missing. The route stores the
// row only at one above the highest `gen` its log holds, which no command can read, and answers any
// other gen 409 `gen_mismatch` naming that one in `restore_gen`. For a `held` row with its hold id
// RestoreRow then signs the row afresh at `restore_gen` and sends it once more, and answers a
// second refusal, another writer's restore between the two, as it is; for any other row it answers
// the first as a RestoreGenError. It refuses a `released` row with no hold id, the first row's
// shape, which Bootstrap alone writes, at gen 1: following `restore_gen` with it would land a
// released row mid-hold, freeing every client while a command runs.
func (c *Client) RestoreRow(ctx context.Context, gen uint64, phase natsgrant.OperatorPhase, holdID, reason string) (Row, error) {
	if err := rowPhase(phase); err != nil {
		return Row{}, err
	}
	if phase == natsgrant.OperatorPhaseReleased && holdID == "" {
		return Row{}, errors.New("holdrecord: a released row with no hold id is the first row, which Bootstrap writes at gen 1 alone; RestoreRow restores a lost row")
	}
	row, err := c.restore(ctx, gen, phase, holdID, reason)
	var mismatch *RestoreGenError
	if phase == natsgrant.OperatorPhaseHeld && holdID != "" && errors.As(err, &mismatch) && mismatch.RestoreGen != gen {
		return c.restore(ctx, mismatch.RestoreGen, phase, holdID, reason)
	}
	return row, err
}

// Bootstrap writes the first row, `released` with no hold id at gen 1, through `POST
// /v1/nats-hold/restore`, once. It answers any refusal as it is: `row_exists` while a row is
// there, and a RestoreGenError when the log holds entries, so a row deleted mid-hold is never
// replaced by a released one; the operator restores that row with RestoreRow.
func (c *Client) Bootstrap(ctx context.Context) error {
	_, err := c.restore(ctx, 1, natsgrant.OperatorPhaseReleased, "", "bootstrap: the first row")
	return err
}

// restore sends one restore of the row at gen, and answers the row stored, a RestoreGenError for
// a gen_mismatch naming restore_gen, or the route's other refusal.
func (c *Client) restore(ctx context.Context, gen uint64, phase natsgrant.OperatorPhase, holdID, reason string) (Row, error) {
	signedAt, sig, err := c.signRow(gen, phase, holdID)
	if err != nil {
		return Row{}, err
	}
	body := map[string]any{"gen": gen, "phase": phase, "hold_id": holdID, "reason": reason,
		"signed_at": signedAt.Format(time.RFC3339Nano), "sig": sig}
	status, data, err := c.exchange(ctx, http.MethodPost, restorePath, nil, body)
	if err != nil {
		return Row{}, err
	}
	if status/100 == 2 {
		var answer rowWire
		if err := decodeAnswer(http.MethodPost, restorePath, data, &answer); err != nil {
			return Row{}, err
		}
		return c.writtenRow(answer, Row{Gen: gen, Phase: phase, HoldID: holdID, SignedAt: signedAt})
	}
	refused := c.refusal(http.MethodPost, restorePath, status, data)
	var refusal struct {
		Error      string  `json:"error"`
		RestoreGen *uint64 `json:"restore_gen"`
	}
	var route *RouteError
	if json.Unmarshal(data, &refusal) == nil && refusal.Error == codeGenMismatch && refusal.RestoreGen != nil && errors.As(refused, &route) {
		return Row{}, &RestoreGenError{RestoreGen: *refusal.RestoreGen, Route: route}
	}
	return Row{}, refused
}

// Records reads and verifies the hold's records of one kind, through `/v1/nats-hold/records`. A
// read the broker answers below its floor (an older signed record put back, or a record it has seen
// now missing, deleted outside the routes) is ErrUnknown.
func (c *Client) Records(ctx context.Context, holdID, kind string) ([]Record, error) {
	var answer struct {
		Records []recordWire `json:"records"`
	}
	query := url.Values{"hold_id": {holdID}, "kind": {kind}}
	if err := c.call(ctx, http.MethodGet, recordsPath, query, nil, &answer); err != nil {
		var refusal *RouteError
		if errors.As(err, &refusal) && refusal.Code == codeBelowFloor {
			return nil, wrap(ErrUnknown, fmt.Sprintf("the broker answers hold %s's %s records below their floor: an older record put back, or a record it has seen deleted outside the routes", holdID, kind))
		}
		return nil, err
	}
	out := make([]Record, 0, len(answer.Records))
	for _, wire := range answer.Records {
		// The list answers each record's key, value, gen and signature; its hold id and kind are
		// the query's, and a record of another hold or kind does not verify under them.
		wire.HoldID, wire.Kind = holdID, kind
		record, err := c.verifyRecord(wire)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// PutRecord writes one record at expectedGen, through `/v1/nats-hold/records`. A record moved since
// answers 409 `gen_mismatch`.
func (c *Client) PutRecord(ctx context.Context, holdID, kind, key string, expectedGen uint64, value json.RawMessage) (Record, error) {
	if len(c.cfg.SigningKey) == 0 {
		return Record{}, errNoSigningKey
	}
	sig, err := signedrecord.Sign(c.cfg.SigningKey, signedrecord.Record(holdID, kind, key, expectedGen+1, value))
	if err != nil {
		return Record{}, fmt.Errorf("holdrecord: sign the %s record %s of hold %s: %w", kind, key, holdID, err)
	}
	body := map[string]any{"hold_id": holdID, "kind": kind, "key": key, "value": value, "expected_gen": expectedGen, "sig": sig}
	var answer recordWire
	if err := c.call(ctx, http.MethodPut, recordsPath, nil, body, &answer); err != nil {
		return Record{}, err
	}
	record, err := c.verifyRecord(answer)
	if err != nil {
		return Record{}, err
	}
	if record.HoldID != holdID || record.Kind != kind || record.Key != key || record.Gen != expectedGen+1 {
		return Record{}, wrap(ErrUnknown, fmt.Sprintf("the answer to the write of the %s record %s of hold %s at gen %d is the %s record %s of hold %s at gen %d",
			kind, key, holdID, expectedGen+1, record.Kind, record.Key, record.HoldID, record.Gen))
	}
	return record, nil
}

// DeleteCohort deletes the cohort record, for `hold abort` from `prepare`.
func (c *Client) DeleteCohort(ctx context.Context, holdID string) error {
	if len(c.cfg.SigningKey) == 0 {
		return errNoSigningKey
	}
	// The route deletes a record only at the gen its delete is signed over, so each cohort record
	// is read and verified first, and its delete signed over the gen read: a write between the two
	// answers gen_mismatch, and the operator runs the abort again.
	records, err := c.Records(ctx, holdID, cohortKind)
	if err != nil {
		return err
	}
	for _, record := range records {
		sig, err := signedrecord.Sign(c.cfg.SigningKey, signedrecord.RecordDelete(holdID, cohortKind, record.Key, record.Gen))
		if err != nil {
			return fmt.Errorf("holdrecord: sign the delete of hold %s's cohort record %s: %w", holdID, record.Key, err)
		}
		query := url.Values{"hold_id": {holdID}, "kind": {cohortKind}, "key": {record.Key}, "gen": {strconv.FormatUint(record.Gen, 10)}, "sig": {sig}}
		if err := c.call(ctx, http.MethodDelete, recordsPath, query, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Cutovers reads and verifies every family's cutover row, through `/v1/nats-hold/cutover`.
func (c *Client) Cutovers(ctx context.Context) ([]CutoverRow, error) {
	var answer struct {
		Families []cutoverWire `json:"families"`
	}
	if err := c.call(ctx, http.MethodGet, cutoverPath, nil, nil, &answer); err != nil {
		return nil, err
	}
	out := make([]CutoverRow, 0, len(answer.Families))
	for _, wire := range answer.Families {
		row, err := c.verifyCutover(wire)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// PutCutover writes one family's cutover row, through `/v1/nats-hold/cutover`.
func (c *Client) PutCutover(ctx context.Context, family, state, holdID string) (CutoverRow, error) {
	if len(c.cfg.SigningKey) == 0 {
		return CutoverRow{}, errNoSigningKey
	}
	// The row is signed over the hold row's gen, which the route stores as its hold_gen, so the
	// command reads and verifies the hold row just before its write; a row moved between the two
	// answers bad_signature, and the command reads again.
	current, err := c.Row(ctx)
	if err != nil {
		return CutoverRow{}, err
	}
	sig, err := signedrecord.Sign(c.cfg.SigningKey, signedrecord.Cutover(family, state, holdID, current.Gen))
	if err != nil {
		return CutoverRow{}, fmt.Errorf("holdrecord: sign the %s cutover row: %w", family, err)
	}
	body := map[string]any{"family": family, "state": state, "hold_id": holdID, "sig": sig}
	var answer cutoverWire
	if err := c.call(ctx, http.MethodPut, cutoverPath, nil, body, &answer); err != nil {
		return CutoverRow{}, err
	}
	row, err := c.verifyCutover(answer)
	if err != nil {
		return CutoverRow{}, err
	}
	if want := (CutoverRow{Family: family, State: state, HoldID: holdID, HoldGen: current.Gen}); row != want {
		return CutoverRow{}, wrap(ErrUnknown, fmt.Sprintf("the answer to the write of %+v is %+v", want, row))
	}
	return row, nil
}

// StartJournal inserts the `done` record at `expected_gen: 0`, for `hold prepare` and `hold set`,
// with the step-1 map its caller read at step 1, before the row turns `held`.
func (c *Client) StartJournal(ctx context.Context, holdID string, w Workflow, step1 map[string]string) error {
	if len(current.rows[w]) == 0 {
		return fmt.Errorf("holdrecord: %q is no workflow of the hold's table", w)
	}
	return c.writeJournal(ctx, holdID, 0, Done{Workflow: w, Step1: step1})
}

// Journal returns the verified `done` record and its `gen`.
func (c *Client) Journal(ctx context.Context, holdID string) (Done, uint64, error) {
	records, err := c.Records(ctx, holdID, journalKind)
	if err != nil {
		return Done{}, 0, err
	}
	for _, record := range records {
		if record.Key != journalKey {
			continue
		}
		var d Done
		if err := json.Unmarshal(record.Value, &d); err != nil {
			return Done{}, 0, fmt.Errorf("holdrecord: hold %s's done record does not decode: %w", holdID, err)
		}
		return d, record.Gen, nil
	}
	return Done{}, 0, fmt.Errorf("holdrecord: hold %s has no done record", holdID)
}

// Admit is a command's admission and its `intent` entry in one write. It reads and verifies the
// row, which must be `held` with holdID, or for CommandSet alone `prepare` with holdID, else
// ErrNotHeld, and the `done` record. It takes Decide's answer, and on nil writes the command's
// `intent` entry, with inputs, by a PutRecord at the `gen` it read, so the decision and the write
// are one compare-and-swap. A 409 `gen_mismatch` means another write came between, and Admit
// reads and decides again. The entry sets `complete` to false, except `hold release`'s, which
// keeps it. CommandAbort's entry also carries each stop entry's record of what its command left,
// under `left.<the stop's index in the journal>` as `<command>: <left>`, so the operator ends the
// hold knowing the state; those keys replace any of inputs'. It returns the record's workflow.
//
// Admit refuses another command while one's intent is pending (ErrBusy), but admits the same
// command again, its rerun after a crash: a second process running the same command at once is
// admitted too. The operator runs one command of a hold at a time (assumption A2); the journal
// cannot tell a rerun from a second process.
func (c *Client) Admit(ctx context.Context, holdID string, req Request, inputs map[string]string) (Workflow, error) {
	for attempt := 1; ; attempt++ {
		row, err := c.Row(ctx)
		if err != nil {
			return "", err
		}
		held := row.Phase == natsgrant.OperatorPhaseHeld || (req.Command == CommandSet && row.Phase == natsgrant.OperatorPhasePrepare)
		if row.HoldID != holdID || !held {
			return "", wrap(ErrNotHeld, fmt.Sprintf("the row is %s with hold id %s, and %s asks for hold %s", row.Phase, row.HoldID, req.Command, holdID))
		}
		d, gen, err := c.Journal(ctx, holdID)
		if err != nil {
			return "", err
		}
		if err := Decide(d, req); err != nil {
			return d.Workflow, fmt.Errorf("holdrecord: %s in %s: %w", req.Command, d.Workflow, err)
		}
		if req.Command == CommandAbort {
			inputs = withStopRecords(inputs, d)
		}
		d.Ops = append(d.Ops, Operation{Command: req.Command, State: stateIntent, Inputs: inputs})
		if req.Command != CommandRelease {
			d.Complete = false
		}
		err = c.writeJournal(ctx, holdID, gen, d)
		if err == nil || !moved(err) || attempt == maxJournalWrites {
			return d.Workflow, err
		}
	}
}

// AppendDone writes the command's `done` entry, with outputs, after the command's own checks
// pass, by a PutRecord at the record's `gen`. It returns ErrNoIntent unless the command's latest
// entry is its `intent`. When Final holds for the record's workflow and the command, the same
// write sets `complete` to true, with `hold_gen` the row's current `gen`, so a final command's
// `done` entry and the completion are one write. Every other command's `done` entry sets it to
// false, but `hold release`'s, which keeps it, as its intent did: the hold ended as the record
// stood. `hold release` prints `hold_gen` and does not compare it with the row's `gen`.
func (c *Client) AppendDone(ctx context.Context, holdID, command string, outputs map[string]string) error {
	return c.appendEntry(ctx, holdID, command, func(d *Done) error {
		d.Ops = append(d.Ops, Operation{Command: command, State: stateDone, Outputs: outputs})
		if command == CommandRelease {
			return nil
		}
		d.Complete = false
		if Final(d.Workflow, command) {
			row, err := c.Row(ctx)
			if err != nil {
				return err
			}
			d.Complete, d.HoldGen = true, row.Gen
		}
		return nil
	})
}

// AppendStop writes the command's `stop` entry, with its reason, when Stoppable holds and the
// command's own check refuses. It ends the command's pending `intent` with no `done`, so another
// command can be admitted, and it keeps `complete` as it was. It records nothing of what the
// command left, which counts as changed: AppendStopLeft records it. It returns ErrNoIntent unless
// the command's latest entry is its `intent`.
func (c *Client) AppendStop(ctx context.Context, holdID, command, reason string) error {
	return c.appendStop(ctx, holdID, command, map[string]string{"reason": reason})
}

// AppendStopLeft is AppendStop that also records what the command left: LeftUnchanged when it
// changed nothing outside the journal, or what it changed (a recreate that deleted the bucket
// before it failed names the bucket). By LeftUnchanged's rule, left covers every run of the
// command in this hold: LeftUnchanged only when this run changed nothing and no earlier run of it
// recorded a change or ended with no entry (a run cut after its intent); otherwise left names what
// those runs left. The caller knows its earlier runs (the journal holds their entries) and folds
// what they left in, and the journal records left as given, but for the one case the journal
// itself shows: LeftUnchanged after a run of the command that ended with no entry (two intents of
// it in a row) is refused, naming the rule. An empty left is refused.
func (c *Client) AppendStopLeft(ctx context.Context, holdID, command, reason, left string) error {
	if left == "" {
		return fmt.Errorf("holdrecord: %s's stop names nothing it left: give LeftUnchanged or what it changed", command)
	}
	return c.appendStop(ctx, holdID, command, map[string]string{"reason": reason, stopLeftKey: left})
}

func (c *Client) appendStop(ctx context.Context, holdID, command string, outputs map[string]string) error {
	return c.appendEntry(ctx, holdID, command, func(d *Done) error {
		if !Stoppable(d.Workflow, command) {
			return fmt.Errorf("holdrecord: %s is not Stoppable in %s, so its refusal writes no stop entry", command, d.Workflow)
		}
		if outputs[stopLeftKey] == LeftUnchanged && cutRun(d.Ops, command) {
			return fmt.Errorf("holdrecord: %s's stop cannot record %q: an earlier run of %s in this hold ended with no entry, so what it left is unknown; the stop names what the runs left (LeftUnchanged's rule)",
				command, LeftUnchanged, command)
		}
		d.Ops = append(d.Ops, Operation{Command: command, State: stateStop, Outputs: outputs})
		return nil
	})
}

// withStopRecords is inputs with each stop entry's record of what its command left, keyed by
// the stop's index in the journal.
func withStopRecords(inputs map[string]string, d Done) map[string]string {
	out := make(map[string]string, len(inputs))
	for k, v := range inputs {
		out[k] = v
	}
	for i, o := range d.Ops {
		if o.State != stateStop {
			continue
		}
		left := o.Outputs[stopLeftKey]
		if left == "" {
			left = "nothing recorded, so counted as changed"
		}
		out["left."+strconv.Itoa(i)] = o.Command + ": " + left
	}
	return out
}

// The routes, the journal's record and how many times a journal write is tried against writes
// that come between its read and its write.
const (
	holdPath         = "/v1/nats-hold"
	restorePath      = "/v1/nats-hold/restore"
	recordsPath      = "/v1/nats-hold/records"
	cutoverPath      = "/v1/nats-hold/cutover"
	journalKind      = "done"
	journalKey       = "hold"
	cohortKind       = "cohort"
	codeGenMismatch  = "gen_mismatch"
	codeBelowFloor   = "below_floor"
	maxJournalWrites = 16
	// maxAnswer bounds the answer body the client reads: the largest is a hold's records of one
	// kind, a journal of some hundreds of entries.
	maxAnswer = 16 << 20
)

var errNoSigningKey = errors.New("holdrecord: the configuration holds no signing key, so this client only reads")

// appendEntry writes one `done` or `stop` entry of command, which change makes, at the record's
// gen, when the command's latest entry is its intent, and reads and writes again after a write
// that came between.
func (c *Client) appendEntry(ctx context.Context, holdID, command string, change func(*Done) error) error {
	for attempt := 1; ; attempt++ {
		d, gen, err := c.Journal(ctx, holdID)
		if err != nil {
			return err
		}
		latest := ""
		for _, o := range d.Ops {
			if o.Command == command {
				latest = o.State
			}
		}
		if latest != stateIntent {
			return wrap(ErrNoIntent, fmt.Sprintf("%s's latest entry is %q", command, latest))
		}
		if err := change(&d); err != nil {
			return err
		}
		err = c.writeJournal(ctx, holdID, gen, d)
		if err == nil || !moved(err) || attempt == maxJournalWrites {
			return err
		}
	}
}

// writeJournal writes d as the hold's `done` record at expectedGen.
func (c *Client) writeJournal(ctx context.Context, holdID string, expectedGen uint64, d Done) error {
	if d.Ops == nil {
		d.Ops = []Operation{}
	}
	value, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("holdrecord: encode hold %s's done record: %w", holdID, err)
	}
	_, err = c.PutRecord(ctx, holdID, journalKind, journalKey, expectedGen, value)
	return err
}

// moved reports whether err is a record write refused because the record moved since its read.
func moved(err error) bool {
	var refusal *RouteError
	return errors.As(err, &refusal) && refusal.Code == codeGenMismatch
}

// rowPhase refuses a phase the row cannot hold.
func rowPhase(phase natsgrant.OperatorPhase) error {
	switch phase {
	case natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld:
		return nil
	}
	return fmt.Errorf("holdrecord: the row holds released, prepare or held, not %q", phase)
}

// signRow signs the hold row at gen, with the time it signs it at. Postgres keeps a timestamp to
// the microsecond, so the time is truncated there, and the row the route stores verifies with the
// signed_at it gives back.
func (c *Client) signRow(gen uint64, phase natsgrant.OperatorPhase, holdID string) (time.Time, string, error) {
	if len(c.cfg.SigningKey) == 0 {
		return time.Time{}, "", errNoSigningKey
	}
	signedAt := time.Now().UTC().Truncate(time.Microsecond)
	sig, err := signedrecord.Sign(c.cfg.SigningKey, signedrecord.HoldRow(gen, string(phase), holdID, signedAt))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("holdrecord: sign the hold row at gen %d: %w", gen, err)
	}
	return signedAt, sig, nil
}

// rowWire is the hold row as the routes answer it.
type rowWire struct {
	Gen      uint64 `json:"gen"`
	Phase    string `json:"phase"`
	HoldID   string `json:"hold_id"`
	SignedAt string `json:"signed_at"`
	Sig      string `json:"sig"`
}

// verifyRow is the row an answer holds, or ErrUnknown when it does not verify, comes without its
// signature or holds a phase outside the row's three.
func (c *Client) verifyRow(wire rowWire) (Row, error) {
	signedAt, err := time.Parse(time.RFC3339Nano, wire.SignedAt)
	if err != nil {
		return Row{}, wrap(ErrUnknown, fmt.Sprintf("the hold row's signed_at %q: %v", wire.SignedAt, err))
	}
	if err := signedrecord.Verify(c.cfg.VerifyKeys, wire.Sig, signedrecord.HoldRow(wire.Gen, wire.Phase, wire.HoldID, signedAt)); err != nil {
		return Row{}, wrap(ErrUnknown, fmt.Sprintf("the hold row at gen %d: %v", wire.Gen, err))
	}
	phase := natsgrant.OperatorPhase(wire.Phase)
	if err := rowPhase(phase); err != nil {
		return Row{}, wrap(ErrUnknown, err.Error())
	}
	return Row{Gen: wire.Gen, Phase: phase, HoldID: wire.HoldID, SignedAt: signedAt}, nil
}

// writtenRow is the verified row a write answers, or ErrUnknown when it is not the row written.
func (c *Client) writtenRow(wire rowWire, want Row) (Row, error) {
	got, err := c.verifyRow(wire)
	if err != nil {
		return Row{}, err
	}
	if got.Gen != want.Gen || got.Phase != want.Phase || got.HoldID != want.HoldID || !got.SignedAt.Equal(want.SignedAt) {
		return Row{}, wrap(ErrUnknown, fmt.Sprintf("the answer to the write of %+v is %+v", want, got))
	}
	return got, nil
}

// recordWire is one hold record as the routes answer it.
type recordWire struct {
	HoldID string          `json:"hold_id"`
	Kind   string          `json:"kind"`
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value"`
	Gen    uint64          `json:"gen"`
	Sig    string          `json:"sig"`
}

func (c *Client) verifyRecord(wire recordWire) (Record, error) {
	if err := signedrecord.Verify(c.cfg.VerifyKeys, wire.Sig, signedrecord.Record(wire.HoldID, wire.Kind, wire.Key, wire.Gen, wire.Value)); err != nil {
		return Record{}, wrap(ErrUnknown, fmt.Sprintf("the %s record %s of hold %s at gen %d: %v", wire.Kind, wire.Key, wire.HoldID, wire.Gen, err))
	}
	return Record{HoldID: wire.HoldID, Kind: wire.Kind, Key: wire.Key, Gen: wire.Gen, Value: wire.Value}, nil
}

// cutoverWire is one family's cutover row as the routes answer it.
type cutoverWire struct {
	Family  string `json:"family"`
	State   string `json:"state"`
	HoldID  string `json:"hold_id"`
	HoldGen uint64 `json:"hold_gen"`
	Sig     string `json:"sig"`
}

func (c *Client) verifyCutover(wire cutoverWire) (CutoverRow, error) {
	if err := signedrecord.Verify(c.cfg.VerifyKeys, wire.Sig, signedrecord.Cutover(wire.Family, wire.State, wire.HoldID, wire.HoldGen)); err != nil {
		return CutoverRow{}, wrap(ErrUnknown, fmt.Sprintf("the %s cutover row: %v", wire.Family, err))
	}
	return CutoverRow{Family: wire.Family, State: wire.State, HoldID: wire.HoldID, HoldGen: wire.HoldGen}, nil
}

// call sends one request and decodes a 2xx answer into answer, or returns the route's refusal.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, answer any) error {
	status, data, err := c.exchange(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return c.refusal(method, path, status, data)
	}
	if answer == nil {
		return nil
	}
	return decodeAnswer(method, path, data, answer)
}

func decodeAnswer(method, path string, data []byte, answer any) error {
	if err := json.Unmarshal(data, answer); err != nil {
		return fmt.Errorf("holdrecord: %s %s answered a body that does not decode: %w", method, path, err)
	}
	return nil
}

// exchange sends one request with the operator's token and returns the answer's status and body.
func (c *Client) exchange(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	target := strings.TrimSuffix(c.cfg.BrokerURL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("holdrecord: encode the body of %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: %s %s: %w", method, path, err)
	}
	token, err := c.cfg.Token(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: the operator's token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.cfg.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: read the answer to %s %s: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}

// refusal is the error a refused request answers: a *RouteError for a route's coded answer, with
// the verified current row it carries, and a plain error naming the status otherwise. A current
// row that does not verify is ErrUnknown beside the route's code.
func (c *Client) refusal(method, path string, status int, data []byte) error {
	var answer struct {
		Error   string   `json:"error"`
		Current *rowWire `json:"current"`
	}
	if json.Unmarshal(data, &answer) != nil || answer.Error == "" {
		text := strings.TrimSpace(string(data))
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("holdrecord: %s %s answered %d: %s", method, path, status, text)
	}
	refusal := &RouteError{Status: status, Code: answer.Error}
	if answer.Current != nil {
		row, err := c.verifyRow(*answer.Current)
		if err != nil {
			return errors.Join(refusal, err)
		}
		refusal.Row = &row
	}
	return refusal
}
