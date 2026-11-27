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
// Decide are the ones model_test.go explores: every workflow from its true start, with crashes
// between any two writes, two operator processes, restores landing outside the hold, the row lost
// and restored, and any command failing for good after its intent, in hub-restore the census and a
// prove after a done prove.
//
// A caller reads a journal through one reader, never through Operation's strings: Done.Latest is a
// command's latest entry, of any state or of those it names (StateIntent, StateDone, StateStop),
// as an Entry with its index; Done.CutRun is whether a run of a command ended with no entry; and
// Entry.Left is what stopped commands left as LeftItems, a stop's own record (its `left` output)
// or, on a `hold abort` intent, the record of every stop before it (its `left.<index>` inputs).
// Decide's rules and the writers of those records read the journal through the same functions.
package holdrecord

import (
	"bytes"
	"cmp"
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
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/natsgrant"
	"github.com/sjawhar/envoy/redirects"
)

// Config is the client's configuration. BrokerURL is the secrets broker's base URL, which every
// command that reaches the broker takes from its `--broker-url` flag. Token gives the operator's
// ServiceAccount token, with the audience `BROKER_NATS_K8S_AUDIENCE`. SigningKey is the private
// half of the operator's hold signing key, kept in memory only, and nil for a command that only
// reads. VerifyKeys are `BROKER_NATS_HOLD_SIGNING_KEYS`. HTTP is the client the requests go
// through, with redirects refused on a copy of it; nil is http.DefaultClient.
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
	// broker is BrokerURL as New parsed it, which each route is joined onto (urljoin.JoinURL).
	broker *url.URL
	// http is Config.HTTP as every request goes through it: following no redirect, and keeping the
	// record of each request, which the answer's words print with (New).
	http *http.Client
	// seenMu guards seenGen, the highest gen of a hold row this client has verified, read or
	// written: the row floor it has seen, which bounds the restore_gen RestoreRow follows.
	seenMu  sync.Mutex
	seenGen uint64
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

// RouteError is a route's coded refusal: a hold route's `bad_request`, `too_large`,
// `bad_signature`, `bad_transition`, `not_operator`, `no_row`, `gen_mismatch`, `hold_mismatch`,
// `row_exists`, `below_floor` or `clock_skew` (a row signed more than five minutes from the broker's
// clock, or more than five minutes before the latest row the broker or its log took: nothing was
// stored), or the broker's own `DATABASE_UNAVAILABLE` or `INTERNAL`. Row is the verified current row
// a 409 carries.
// HoldID is, on a `hold_mismatch`, the hold id the broker's row or log names, which the refused
// call must name instead (a restore after a prepare entry names that entry's hold): no route lists
// the log, so this field is where the operator learns it. It is not in Error's text.
// Message is the broker's text for the refusal, which names what it refused: the hold whose
// journal holds a restore and whether a held restore is admitted, or the rows a database admin
// repairs by hand; the key id of a signature no listed key verifies; the times a `clock_skew`
// compared. It is not in Error's text either: part of what it names (a record's hold id) is a
// database writer's choice, so a caller prints it as data, quoted.
type RouteError struct {
	Status  int
	Code    string
	Row     *Row
	HoldID  string
	Message string
	// The request's method and route, which Error names.
	method, path string
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

// Error names the request's method and route, and the answer's status and code.
func (e *RouteError) Error() string {
	if e.method == "" {
		return fmt.Sprintf("holdrecord: %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("holdrecord: %s %s answered %d %s", e.method, e.path, e.Status, e.Code)
}

// New refuses an empty BrokerURL, a nil Token and an empty VerifyKeys. A BrokerURL that is not an
// absolute URL is refused printed through redact.URL, which prints it only when it is a plain
// http(s) URL with no place a credential can sit: a password in its user info, or the operator's
// token pasted where the URL goes, reaches no error.
//
// Every request carries the operator's token, and a DELETE the operator's signature in its query,
// so the client follows no redirect (redirects.Refused): a redirect's Location is the answering
// party's words, and net/http keeps the Authorization header on a redirect to the same host name.
// It keeps the record of each request (redact.RefusalClient), so the answer's words print with the
// credentials the request carried redacted.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.BrokerURL == "":
		return nil, errors.New("holdrecord: the configuration names no broker URL")
	case cfg.Token == nil:
		return nil, errors.New("holdrecord: the configuration gives no way to get the operator's token")
	case len(cfg.VerifyKeys) == 0:
		return nil, errors.New("holdrecord: the configuration holds no key to verify an answer with, so every row would read as unknown")
	}
	broker, err := url.Parse(cfg.BrokerURL)
	if err != nil || broker.Scheme == "" || broker.Host == "" {
		return nil, fmt.Errorf("holdrecord: the broker URL %s is not an absolute URL", redact.URL(cfg.BrokerURL))
	}
	for i, key := range cfg.VerifyKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("holdrecord: verify key %d is %d bytes, not an Ed25519 public key's %d", i+1, len(key), ed25519.PublicKeySize)
		}
	}
	return &Client{cfg: cfg, broker: broker, http: redact.RefusalClient(redirects.Refused(cmp.Or(cfg.HTTP, http.DefaultClient)))}, nil
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

// Row reads and verifies the row, through `GET /v1/nats-hold`. A row the broker answers below its
// floor (an older signed row put back, or another row at the gen it served) is ErrUnknown.
func (c *Client) Row(ctx context.Context) (Row, error) {
	var answer rowWire
	if err := c.call(ctx, http.MethodGet, holdPath, nil, nil, &answer); err != nil {
		var refusal *RouteError
		if errors.As(err, &refusal) && refusal.Code == codeBelowFloor {
			return Row{}, wrap(ErrUnknown, "the broker answers the hold row below its floor: an older row put back, or another row at the gen it served")
		}
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
// other gen 409 `gen_mismatch` naming that one in `restore_gen`. Every restore's gen is bounded by
// the highest row gen this client has verified, read or written (its seen gen): a client that has
// seen gen N sends a restore at N+1 alone, and answers any other gen ErrUnknown without sending it,
// since a gen at or below N, or more than one above it, is a gen a database writer chose by the log
// entries it deleted or added, and the operator would sign the restored row there. A client that
// has seen no row (a new process) has no seen gen, and only the route bounds it. For a `held` row
// with its hold id RestoreRow sends at N+1 when it has seen gen N, and at gen otherwise; it then
// follows the route's `restore_gen` once, signed afresh, when that is the bounded gen, and answers
// a second refusal, another writer's restore between the two, as it is. A `released` row is sent
// once at gen, and a refusal answered as a RestoreGenError. Before it restores a `held` row it reads the
// hold's journal, and refuses a hold that has ended, a done `hold release` or `hold abort`
// (ErrAlreadyDone): no command is admitted after the end, so a held row restored for it would keep
// every client out with no command left to release it. The route judges every restore by the log's
// latest entry, which carries the row the operator signed and must verify, and by its floor of the
// row, which refuses a restore at or below a row it served, read or wrote (409 `below_floor`, read
// as ErrUnknown). It stores held only after a prepare or held entry of the same hold id, so a hold
// that `hold abort` from `prepare` ended, which leaves no journal entry, is refused 400
// `bad_transition` too. A `released` row, the one a completed hold left, the first row's shape
// with no hold id, or a row lost while the hold was `prepare`, before its journal starts or after
// (restored with that hold's id, as `hold abort` from `prepare` leaves it), reads no journal and
// is sent once, at gen alone, the gen a RestoreGenError named: the route stores it after a
// released entry, after a prepare entry only with that entry's hold id (409 `hold_mismatch` for
// another or none: a released row under another id leaves the prepared hold where no command
// reaches it), since a prepare row refuses no client, or after a held one whose hold's signed
// journal shows it ended, and refuses it 400 `bad_transition` after any other held entry, so a
// released row never lands mid-hold. Whatever the latest entry, the route refuses a released row
// while any hold's journal, an ended hold's included, is below the broker's record floor, does not
// verify, or holds an entry whose row (Operation.Row) the verified log does not hold or that names
// none (409 `below_floor`, naming the hold, the key id when the signing key is no longer listed,
// the entry, and the hand repair), or while a hold's journal shows `hold set` done and no end, or
// `hold set` not done while the log holds the hold's held entry, unless the journal shows a done
// `hold release` or `hold abort`, or a verified released entry names the hold and its journal shows
// `hold release`'s or `hold abort`'s intent as that command's latest entry with no other intent
// pending (400 `bad_transition`, which advises restoring the row held only when the route admits
// that restore, and otherwise names the causes and the hand repair). The refusal's text is in
// RouteError.Message.
func (c *Client) RestoreRow(ctx context.Context, gen uint64, phase natsgrant.OperatorPhase, holdID, reason string) (Row, error) {
	if err := rowPhase(phase); err != nil {
		return Row{}, err
	}
	if phase == natsgrant.OperatorPhaseHeld && holdID != "" {
		d, _, err := c.Journal(ctx, holdID)
		if err != nil {
			return Row{}, err
		}
		for _, end := range []string{CommandRelease, CommandAbort} {
			if hasDone(d.Ops, end) {
				return Row{}, wrap(ErrAlreadyDone, fmt.Sprintf("hold %s has ended (%s is done), so its row is not restored held; restore the row released", holdID, end))
			}
		}
		if seen := c.seen(); seen != 0 {
			gen = seen + 1
		}
	}
	row, err := c.restore(ctx, gen, phase, holdID, reason)
	var mismatch *RestoreGenError
	if phase == natsgrant.OperatorPhaseHeld && holdID != "" && errors.As(err, &mismatch) && mismatch.RestoreGen != gen {
		return c.restore(ctx, mismatch.RestoreGen, phase, holdID, reason)
	}
	return row, err
}

// seen is the highest hold-row gen this client has verified, 0 for none.
func (c *Client) seen() uint64 {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	return c.seenGen
}

// bounded is whether a restore at gen may be sent: only one above the highest row gen this client
// has verified, when it has verified one.
func (c *Client) bounded(gen uint64) error {
	seen := c.seen()
	if seen == 0 || gen == seen+1 {
		return nil
	}
	return wrap(ErrUnknown, fmt.Sprintf("a restore at gen %d, and the latest hold row this client verified is at gen %d, so a restored row follows it at gen %d alone (restore_gen and the first row's gen 1 included)", gen, seen, seen+1))
}

// Bootstrap writes the first row, `released` with no hold id at gen 1, through `POST
// /v1/nats-hold/restore`, once. It answers any refusal as it is: `row_exists` while a row is
// there, and a RestoreGenError when the log holds entries, so a lost row is not replaced by the
// first row's at a gen it did not name. A client that has seen a row answers ErrUnknown without
// sending (RestoreRow's seen gen: gen 1 follows no row). Otherwise that holds only as long as
// nats_hold_log keeps its entries: the log is the broker database's, and a database writer who
// empties it lets Bootstrap write gen 1 again. The operator restores a lost row with RestoreRow:
// held with its hold id mid-hold, and released, at the RestoreGenError's gen, after a released or
// prepare entry or a held one whose hold has ended.
func (c *Client) Bootstrap(ctx context.Context) error {
	_, err := c.restore(ctx, 1, natsgrant.OperatorPhaseReleased, "", "bootstrap: the first row")
	return err
}

// restore sends one restore of the row at gen, once gen is bounded by the client's seen gen
// (bounded), and answers the row stored, a RestoreGenError for a gen_mismatch naming restore_gen,
// or the route's other refusal.
func (c *Client) restore(ctx context.Context, gen uint64, phase natsgrant.OperatorPhase, holdID, reason string) (Row, error) {
	if err := c.bounded(gen); err != nil {
		return Row{}, err
	}
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

// DeleteCohort deletes the cohort record, for `hold abort` from `prepare`, and for `hold abort` on
// a row lost in prepare and restored released: after its Admit while the journal shows prepare, and
// alone, with no Admit, when the hold has no journal. The broker keeps each delete, signed, beside
// the records, so after a delete that committed every replica reads the cohort as absent, and the
// abort's rerun, served by any of them, deletes nothing and answers nil.
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

// routeCodes are the codes a refusal of the hold's routes carries: each hold route's own, in the
// `error` field beside its `message` (the broker's holdRefusal), and the broker's own failures
// any route answers, in the `code` field beside its `error` text (the broker's writeError).
var routeCodes = struct{ hold, broker map[string]bool }{
	hold: map[string]bool{"bad_request": true, "too_large": true, "bad_signature": true, "bad_transition": true,
		"not_operator": true, "no_row": true, codeGenMismatch: true, "hold_mismatch": true, "row_exists": true, codeBelowFloor: true,
		"clock_skew": true},
	broker: map[string]bool{"DATABASE_UNAVAILABLE": true, "INTERNAL": true},
}

var errNoSigningKey = errors.New("holdrecord: the configuration holds no signing key, so this client only reads")

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
// signature or holds a phase outside the row's three. A row that verifies moves the client's seen
// gen (followable).
func (c *Client) verifyRow(wire rowWire) (Row, error) {
	signedAt, err := time.Parse(time.RFC3339Nano, wire.SignedAt)
	if err != nil {
		return Row{}, wrap(ErrUnknown, fmt.Sprintf("the hold row at gen %d has a signed_at that is no RFC 3339 time", wire.Gen))
	}
	if err := signedrecord.Verify(c.cfg.VerifyKeys, wire.Sig, signedrecord.HoldRow(wire.Gen, wire.Phase, wire.HoldID, signedAt)); err != nil {
		return Row{}, wrap(ErrUnknown, fmt.Sprintf("the hold row at gen %d: %v", wire.Gen, err))
	}
	phase := natsgrant.OperatorPhase(wire.Phase)
	if err := rowPhase(phase); err != nil {
		return Row{}, wrap(ErrUnknown, err.Error())
	}
	c.seenMu.Lock()
	c.seenGen = max(c.seenGen, wire.Gen)
	c.seenMu.Unlock()
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
		return fmt.Errorf("holdrecord: %s %s answered a body that does not decode: %s", method, path, jsonFailure(err))
	}
	return nil
}

// jsonFailure names what json refused by the field's path and the error's class, never by the
// error's text: a type error quotes the number that does not fit its field ("cannot unmarshal
// number 1844674407370955161600123456789 into ..."), and a syntax error a character of the input,
// so a body or a record's value would reach an error that is printed and logged.
func jsonFailure(err error) string {
	var typed *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &typed) && typed.Field != "":
		return fmt.Sprintf("its field %s does not fit %s (a type error)", typed.Field, typed.Type)
	case errors.As(err, &typed):
		return fmt.Sprintf("it does not fit %s (a type error)", typed.Type)
	case errors.As(err, &syntax):
		return fmt.Sprintf("it is not JSON at byte %d (a syntax error)", syntax.Offset)
	default:
		return "an error of another class"
	}
}

// exchange sends one request with the operator's token and returns the answer's status and body.
// The route is joined onto the broker URL (urljoin.JoinURL): its path after the URL's path, its
// query after the URL's own, and the URL's fragment dropped, so a broker URL with a path, a query
// or a fragment still names the route. No error it answers carries the request's URL: a DELETE's
// query holds the operator's signature, and net/http's *url.Error quotes the URL whole
// (withoutURL). net/http's error of a request that got no answer also quotes what the answering
// party sent (a Location, a status line or a header line that does not parse), which whatever
// answers at the broker's address can write the operator's token into, so that error prints as
// redact.Unanswered names it: net/http's operation, the URL without its query, and the failure's
// class. A redirect is the broker's answer, refused by its status, and its Location is asked
// nothing (New).
func (c *Client) exchange(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	ref := path
	if len(query) > 0 {
		ref += "?" + query.Encode()
	}
	asked, err := urljoin.JoinURL(c.broker, ref)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: %s %s: %w", method, path, err)
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("holdrecord: encode the body of %s %s: %s", method, path, jsonFailure(err))
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, asked.String(), payload)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: %s %s: %w", method, path, redact.Class(err))
	}
	token, err := c.cfg.Token(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: the operator's token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: %s %s: %w", method, path, redact.Unanswered(err, req, ""))
	}
	defer func() { _ = resp.Body.Close() }()
	if redirects.Status(resp.StatusCode) {
		return 0, nil, fmt.Errorf("holdrecord: %s %s answered %d, a redirect, which this client never follows", method, path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("holdrecord: read the answer to %s %s: %w", method, path, withoutURL(err))
	}
	return resp.StatusCode, data, nil
}

// withoutURL is err with every *url.Error in its chain replaced by the error it wraps, so the
// request's URL, its query and the signature in it, reaches no error; the caller names the method
// and the route instead. An error with no *url.Error is answered as it is.
func withoutURL(err error) error {
	var withURL *url.Error
	for errors.As(err, &withURL) {
		if withURL.Err == nil {
			return fmt.Errorf("the request failed: %s", withURL.Op)
		}
		err = withURL.Err
	}
	return err
}

// refusal is the error a refused request answers: a *RouteError for an answer coded with one of
// routeCodes, with the verified current row it carries and the broker's message, and otherwise a
// plain error naming the method, the route, the status and "an unrecognized body". No text of the
// body reaches the error, the code apart, and only a code the client knows: an answer it cannot
// read may hold anything, a record's value among it, and an error is printed and logged. A coded
// answer's message is kept in RouteError.Message, outside the error's text. A current row that
// does not verify is ErrUnknown beside the route's code, and so is any `below_floor`, the broker's
// refusal of a row or record it reads as an older one put back or another at the gen it served.
func (c *Client) refusal(method, path string, status int, data []byte) error {
	var answer struct {
		Error   string   `json:"error"`
		Code    string   `json:"code"`
		Message string   `json:"message"`
		Current *rowWire `json:"current"`
		HoldID  string   `json:"hold_id"`
	}
	var code string
	switch {
	case json.Unmarshal(data, &answer) != nil:
	case routeCodes.hold[answer.Error]:
		code = answer.Error
	case routeCodes.broker[answer.Code]:
		code = answer.Code
	}
	if code == "" {
		return fmt.Errorf("holdrecord: %s %s answered %d with an unrecognized body", method, path, status)
	}
	refusal := &RouteError{Status: status, Code: code, Message: answer.Message, method: method, path: path}
	if code == "hold_mismatch" {
		refusal.HoldID = answer.HoldID
	}
	if answer.Current != nil {
		row, err := c.verifyRow(*answer.Current)
		if err != nil {
			return errors.Join(refusal, err)
		}
		refusal.Row = &row
	}
	if code == codeBelowFloor {
		return fmt.Errorf("%w: %w", ErrUnknown, refusal)
	}
	return refusal
}
