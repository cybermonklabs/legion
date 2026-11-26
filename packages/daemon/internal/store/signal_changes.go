package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The record of changes of admission by signals (S6 of dispatch://INFRA-5899): per project, one
// entry for each change a reader of the signals sees, each of the nine members S6 names, as
// migrations/20261126072843_signal_settings.up.sql defines them (signal_changes). A person's change
// of the switch or of a limit writes its entry with the setting (ChangeSignalSettingTx); each
// admission pass writes the entries of the holds and the values it computed (admit's
// writeSignalChanges). The ids of a project rise by one for each entry, given out in the
// transaction that writes the entries (signal_change_ids), so a reader sees a lost entry as a gap
// and a rolled-back transaction leaves none. The state carries the newest SignalChangesShown, and
// each state the daemon publishes the entries SignalChangesForPublishTx chooses.

// SignalChangeKind is what an entry records the change of.
type SignalChangeKind string

const (
	// SignalChangeValue is a series' value, or that the daemon cannot compute it.
	SignalChangeValue SignalChangeKind = "value"
	// SignalChangeLimit is a series' limit, which a person set or cleared.
	SignalChangeLimit SignalChangeKind = "limit"
	// SignalChangeHolds is whether a series holds admission, 1 or 0.
	SignalChangeHolds SignalChangeKind = "holds"
	// SignalChangeSwitch is the switch of admission by signals, 1 on or 0 off.
	SignalChangeSwitch SignalChangeKind = "switch"
)

// SignalChangesShown is how many of the newest entries `legion state` carries, and the fewest a
// published state does.
const SignalChangesShown = 50

// SignalChange is one entry of the record, with exactly S6's nine members on the wire, each null
// where S6 says: Signal and Part null for the switch, Part null for every signal but the provider,
// Value null for a cleared limit and for a value the daemon cannot compute, Absent the reason it
// cannot (null otherwise), and SetBy and Reason who changed a limit or the switch and why (null in
// every other entry). A hold and the switch are 1 or 0.
type SignalChange struct {
	ID     int64            `json:"id"`
	At     time.Time        `json:"at"`
	Signal *string          `json:"signal"`
	Part   *string          `json:"part"`
	What   SignalChangeKind `json:"what"`
	Value  *float64         `json:"value"`
	Absent *string          `json:"absent"`
	SetBy  *PauseActor      `json:"setBy"`
	Reason *string          `json:"reason"`
}

// SeriesValueChange is the value entry of series at at: value, or, where value is nil, that the
// daemon cannot compute it, and why.
func SeriesValueChange(series string, at time.Time, value *float64, absent string) SignalChange {
	entry := SignalChange{At: at, What: SignalChangeValue, Value: value}
	entry.Signal, entry.Part = seriesNames(series)
	if value == nil {
		entry.Absent = &absent
	}
	return entry
}

// SeriesHoldsChange is the holds entry of series at at: 1 where it holds admission, 0 where not.
func SeriesHoldsChange(series string, at time.Time, holds bool) SignalChange {
	flag := 0.0
	if holds {
		flag = 1
	}
	entry := SignalChange{At: at, What: SignalChangeHolds, Value: &flag}
	entry.Signal, entry.Part = seriesNames(series)
	return entry
}

// AppendSignalChangesTx writes entries as project's next entries, in their order, giving each the
// next id of the project in the same transaction (signal_change_ids), so the ids are contiguous
// and a transaction rolled back gives its ids back with its entries. The ids it gave are set on
// entries.
func AppendSignalChangesTx(ctx context.Context, tx pgx.Tx, project string, entries []SignalChange) error {
	if len(entries) == 0 {
		return nil
	}
	var last int64
	if err := tx.QueryRow(ctx, `insert into signal_change_ids (project, last) values ($1, $2)
		on conflict (project) do update set last = signal_change_ids.last + excluded.last returning last`, project, len(entries)).Scan(&last); err != nil {
		return fmt.Errorf("give out the ids of %d entries of the record of changes of %s: %w", len(entries), project, err)
	}
	batch := &pgx.Batch{}
	for i := range entries {
		entry := &entries[i]
		entry.ID = last - int64(len(entries)-1-i)
		var kind, session *string
		if entry.SetBy != nil {
			by, of := string(entry.SetBy.Kind), entry.SetBy.Session
			kind, session = &by, &of
		}
		batch.Queue(`insert into signal_changes (project, id, at, signal, part, what, value, absent, by_kind, by_session, reason)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			project, entry.ID, entry.At, entry.Signal, entry.Part, string(entry.What), entry.Value, entry.Absent, kind, session, entry.Reason)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("write %d entries of the record of changes of %s: %w", len(entries), project, err)
	}
	return nil
}

// SeriesEntries is the newest entry of one series of each kind a pass compares with: its value
// entry and its holds entry, each nil where the record holds none.
type SeriesEntries struct {
	Value *SignalChange
	Holds *SignalChange
}

// NewestSeriesEntriesTx is, by series name of signals.Names(), the newest value entry and holds
// entry of each series of project, read through tx: what the last committed pass wrote of each,
// which the next pass and the state compare with. It reads one row of the index of each series
// and kind (newestSeriesEntries), however long the project's record is.
func NewestSeriesEntriesTx(ctx context.Context, tx pgx.Tx, project string) (map[string]SeriesEntries, error) {
	rows, err := tx.Query(ctx, newestSeriesEntries, project)
	if err != nil {
		return nil, fmt.Errorf("read the newest entries of each series of %s: %w", project, err)
	}
	entries, err := pgx.CollectRows(rows, scanSignalChange)
	if err != nil {
		return nil, fmt.Errorf("read the newest entries of each series of %s: %w", project, err)
	}
	newest := make(map[string]SeriesEntries, len(entries))
	for i := range entries {
		entry := &entries[i]
		part := ""
		if entry.Part != nil {
			part = *entry.Part
		}
		series, named := SignalSeries(*entry.Signal, part)
		if !named {
			return nil, fmt.Errorf("entry %d of %s names signal %q and part %q, which is no series", entry.ID, project, *entry.Signal, part)
		}
		of := newest[series]
		if entry.What == SignalChangeValue {
			of.Value = entry
		} else {
			of.Holds = entry
		}
		newest[series] = of
	}
	return newest, nil
}

// newestSeriesEntries is the statement NewestSeriesEntriesTx reads: for each kind it compares and
// each series, the newest entry of that series and kind of the project $1, each a probe of
// signal_changes_newest (project, what, signal, part, id) that stops at its first row, rather
// than a read of every entry the project ever wrote.
var newestSeriesEntries = func() string {
	var probes []string
	for _, what := range []SignalChangeKind{SignalChangeValue, SignalChangeHolds} {
		for _, series := range signals.Names() {
			signal, part := SignalPart(series)
			ofPart := "part is null"
			if part != "" {
				ofPart = "part = '" + part + "'"
			}
			probes = append(probes, fmt.Sprintf(`(select %s from signal_changes where project = $1 and what = '%s' and signal = '%s' and %s order by id desc limit 1)`,
				signalChangeColumns, what, signal, ofPart))
		}
	}
	return strings.Join(probes, " union all ")
}()

// SignalChangesTx is project's newest n entries, newest first, read through tx: the state's
// `admission.signalChanges` with n SignalChangesShown, never nil.
func SignalChangesTx(ctx context.Context, tx pgx.Tx, project string, n int) ([]SignalChange, error) {
	return readSignalChanges(ctx, tx, project, `select `+signalChangeColumns+` from signal_changes where project = $1 order by id desc limit $2`, project, n)
}

// SignalChangesForPublishTx is the entries a state the daemon publishes carries (S6), newest first:
// every entry of project above published, the id of the newest entry the state published before
// carried (0 for none), and never fewer than the newest SignalChangesShown. The ids of a project
// are contiguous, so the newest SignalChangesShown are those above the newest id less that many,
// and one statement reads both in one snapshot.
func SignalChangesForPublishTx(ctx context.Context, tx pgx.Tx, project string, published int64) ([]SignalChange, error) {
	return readSignalChanges(ctx, tx, project, `select `+signalChangeColumns+` from signal_changes where project = $1
		and id > least($2, coalesce((select last from signal_change_ids where project = $1), 0) - $3) order by id desc`,
		project, published, SignalChangesShown)
}

// signalChangeColumns is an entry as scanSignalChange reads it.
const signalChangeColumns = `id, at, signal, part, what, value, absent, by_kind, by_session, reason`

func scanSignalChange(row pgx.CollectableRow) (SignalChange, error) {
	var entry SignalChange
	var what string
	var kind, session *string
	if err := row.Scan(&entry.ID, &entry.At, &entry.Signal, &entry.Part, &what, &entry.Value, &entry.Absent, &kind, &session, &entry.Reason); err != nil {
		return SignalChange{}, err
	}
	entry.At, entry.What = entry.At.UTC(), SignalChangeKind(what)
	if kind != nil {
		entry.SetBy = &PauseActor{Kind: PauseActorKind(*kind), Session: *session}
	}
	return entry, nil
}

// readSignalChanges is the entries query reads, never nil.
func readSignalChanges(ctx context.Context, tx pgx.Tx, project, query string, args ...any) ([]SignalChange, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the record of changes of %s: %w", project, err)
	}
	entries, err := pgx.CollectRows(rows, scanSignalChange)
	if err != nil {
		return nil, fmt.Errorf("read the record of changes of %s: %w", project, err)
	}
	if entries == nil {
		entries = []SignalChange{}
	}
	return entries, nil
}

// SignalsReading is the state's `admission.signals` (S2): whether admission by signals is on, who
// set the switch last, when and why (null before a person first did), each of the six series in the
// order of signals.Names() as the live signals read it at the state's moment (SignalSeriesReading),
// and the provider's record of the hour the provider's two series read (ProviderReading). Every
// reading is computed at the read from what the daemon keeps of the inputs, never from an entry of
// the record; only since is read from the record. admit's LiveSignals.State builds it.
type SignalsReading struct {
	On       bool                  `json:"on"`
	Switch   *SignalSet            `json:"switch"`
	Series   []SignalSeriesReading `json:"series"`
	Provider ProviderReading       `json:"provider"`
}

// SignalSeriesReading is one series of SignalsReading: its signal and part (SignalPart), its value,
// or null with the reason the daemon cannot compute it (absent), its limit (null for none) with
// its last change (null before a person first set or cleared it), whether it holds admission now
// (never while admission by signals is off, nor where its value cannot be computed), and since
// when it stands as it does: the moment of the record's newest holds entry of the series where
// that entry says what the series does now, and null where no committed pass recorded it so, a
// hold no pass has dated yet.
type SignalSeriesReading struct {
	Signal   string     `json:"signal"`
	Part     *string    `json:"part"`
	Value    *float64   `json:"value"`
	Absent   *string    `json:"absent"`
	Limit    *float64   `json:"limit"`
	LimitSet *SignalSet `json:"limitSet"`
	Holds    bool       `json:"holds"`
	Since    *time.Time `json:"since"`
}

// ProviderReading is the provider's record the provider's headroom and refusals read at the state's
// moment, so a reader compares the daemon's own input with another record of the provider: each
// whole minute of the hour before the minute under way, oldest first (ProviderMinuteReading). It
// names no request and no session.
type ProviderReading struct {
	Minutes []ProviderMinuteReading `json:"minutes"`
}

// ProviderMinuteReading is one minute of ProviderReading: its start, whether every request of it is
// known to have been reported (held; a minute that is not is absent from both of the provider's
// series), the uncached input tokens of the models the project counts, the token limit in force
// when it began (0 for none), and its refused requests by class: the project's own limit (an
// unknown refusal among them) and the provider.
type ProviderMinuteReading struct {
	At                  time.Time `json:"at"`
	Held                bool      `json:"held"`
	UncachedInputTokens int64     `json:"uncachedInputTokens"`
	TokenLimit          int64     `json:"tokenLimit"`
	OwnRefusals         int64     `json:"ownRefusals"`
	ProviderRefusals    int64     `json:"providerRefusals"`
}
