package legion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The kinds of change event a daemon pushes (dispatch://INFRA-979, "Where the data comes from").
// Each but the heartbeat becomes legion_observations rows at the event's own time, which is what
// the board and the metrics already read.
const (
	KindPhase         = "phase"
	KindClaimState    = "claim_state"
	KindSlot          = "slot"
	KindWaitingOn     = "waiting_on"
	KindChecksVerdict = "checks_verdict"
	KindWaitingLine   = "waiting_line"
	KindSetting       = "setting"
	KindHeartbeat     = "heartbeat"
)

// StaleAfter is how long a project's daemon may send neither an event nor a heartbeat before the
// board says its data is old.
const StaleAfter = 120 * time.Second

// PushEvent is one change event as the daemon sends it. Issue is empty for a project-level kind
// (waiting_line, setting, heartbeat). A heartbeat's seq is not its own: it is stored nowhere.
type PushEvent struct {
	Seq   int64           `json:"seq"`
	At    time.Time       `json:"at"`
	Kind  string          `json:"kind"`
	Issue string          `json:"issue,omitempty"`
	Value json.RawMessage `json:"value"`
}

// Push is one delivery: every event the daemon had pending, in one write. Snapshot marks a full
// copy of the daemon's current values, which replaces the project's.
type Push struct {
	Project    string      `json:"project"`
	Generation string      `json:"generation"`
	Events     []PushEvent `json:"events"`
	Snapshot   bool        `json:"snapshot,omitempty"`
}

// claimStateValue and settingValue are the values of the two kinds that carry a key of their own.
type claimStateValue struct {
	Role  string `json:"role"`
	State string `json:"state"`
}

type settingValue struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value"`
	Source json.RawMessage `json:"source"`
}

// Validate refuses a push the route cannot store, naming the first problem.
func (p Push) Validate() error {
	if strings.TrimSpace(p.Generation) == "" {
		return errors.New("generation is required")
	}
	for index, event := range p.Events {
		if event.At.IsZero() {
			return fmt.Errorf("events[%d].at is required", index)
		}
		switch event.Kind {
		case KindHeartbeat, KindWaitingLine, KindSetting:
		case KindPhase, KindClaimState, KindSlot, KindWaitingOn, KindChecksVerdict:
			if event.Issue == "" {
				return fmt.Errorf("events[%d] (%s) needs an issue", index, event.Kind)
			}
		default:
			return fmt.Errorf("events[%d].kind %q is not a known kind", index, event.Kind)
		}
		if event.Kind != KindHeartbeat && event.Seq <= 0 {
			return fmt.Errorf("events[%d].seq must be positive", index)
		}
	}
	return nil
}

// fieldKey is one (issue, field) pair of legion_observations.
type fieldKey struct{ issue, field string }

// timedObservation is one row to write, at the event's own time.
type timedObservation struct {
	fieldKey
	value json.RawMessage
	at    time.Time
}

// ApplyPush stores one delivery in one transaction and answers the last sequence number the
// project holds with no gap below it, which the daemon compares with its own.
//
//   - Each event is stored once per (project, generation, seq); a repeated one changes nothing.
//   - Each new event writes the observation row its kind maps to, at its own `at`, where the value
//     differs from the newest the field holds.
//   - A snapshot writes null for every value the project holds that the snapshot does not carry,
//     and moves last_seq to the snapshot's highest seq.
//   - The newest event or heartbeat time becomes the project's freshness.
//   - The board's channel is notified, delivered at commit.
func ApplyPush(ctx context.Context, pool *store.Pool, push Push) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The project's freshness is its newest event or heartbeat. A snapshot carries each value's
	// own change time, which may be old, so its arrival counts as now; it also clears what it did
	// not carry at now.
	var newestAt time.Time
	for _, event := range push.Events {
		if event.At.After(newestAt) {
			newestAt = event.At
		}
	}
	clearAt := time.Now().UTC()
	if push.Snapshot || newestAt.IsZero() {
		newestAt = clearAt
	}
	if _, err := tx.Exec(ctx, `
		insert into legion_pushes (project, generation, last_seq, last_at) values ($1, $2, 0, $3)
		on conflict (project) do nothing
	`, push.Project, push.Generation, newestAt); err != nil {
		return 0, fmt.Errorf("open the Legion push row of %s: %w", push.Project, err)
	}
	var generation string
	var lastSeq int64
	if err := tx.QueryRow(ctx, `select generation, last_seq from legion_pushes where project = $1 for update`,
		push.Project).Scan(&generation, &lastSeq); err != nil {
		return 0, err
	}
	if generation != push.Generation {
		lastSeq = 0
	}

	fresh, err := insertEvents(ctx, tx, push)
	if err != nil {
		return 0, err
	}
	current, err := currentValues(ctx, tx, push.Project)
	if err != nil {
		return 0, err
	}
	rows := observationsOf(fresh, current, push.Snapshot, clearAt)
	if err := writeObservations(ctx, tx, push.Project, rows); err != nil {
		return 0, err
	}

	if push.Snapshot {
		for _, event := range push.Events {
			if event.Kind != KindHeartbeat && event.Seq > lastSeq {
				lastSeq = event.Seq
			}
		}
	}
	// Advance past every seq stored without a gap: an event that arrived after a lost one waits
	// below the gap until a snapshot covers it.
	seqs, err := tx.Query(ctx, `select seq from legion_events where project = $1 and generation = $2 and seq > $3 order by seq`,
		push.Project, push.Generation, lastSeq)
	if err != nil {
		return 0, err
	}
	for seqs.Next() {
		var seq int64
		if err := seqs.Scan(&seq); err != nil {
			seqs.Close()
			return 0, err
		}
		if seq != lastSeq+1 {
			break
		}
		lastSeq = seq
	}
	seqs.Close()
	if err := seqs.Err(); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `
		update legion_pushes
		set generation = $2, last_seq = $3, last_at = greatest(case when generation = $2 then last_at end, $4), updated_at = now()
		where project = $1
	`, push.Project, push.Generation, lastSeq, newestAt); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, StateChannel, push.Project); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return lastSeq, nil
}

// insertEvents stores every event but the heartbeats and answers the ones not stored before, in
// seq order.
func insertEvents(ctx context.Context, tx pgx.Tx, push Push) ([]PushEvent, error) {
	bySeq := map[int64]PushEvent{}
	seqs, ats, kinds, issues, values := []int64{}, []time.Time{}, []string{}, []string{}, []string{}
	for _, event := range push.Events {
		if event.Kind == KindHeartbeat {
			continue
		}
		value := event.Value
		if len(value) == 0 {
			value = json.RawMessage("null")
		}
		bySeq[event.Seq] = event
		seqs, ats, kinds, issues, values = append(seqs, event.Seq), append(ats, event.At), append(kinds, event.Kind),
			append(issues, event.Issue), append(values, string(value))
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		insert into legion_events (project, generation, seq, at, kind, issue_key, value)
		select $1, $2, e.seq, e.at, e.kind, e.issue, e.value::jsonb
		from unnest($3::bigint[], $4::timestamptz[], $5::text[], $6::text[], $7::text[]) as e(seq, at, kind, issue, value)
		on conflict do nothing
		returning seq
	`, push.Project, push.Generation, seqs, ats, kinds, issues, values)
	if err != nil {
		return nil, fmt.Errorf("store the Legion events of %s: %w", push.Project, err)
	}
	defer rows.Close()
	var fresh []PushEvent
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		fresh = append(fresh, bySeq[seq])
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(fresh, func(a, b PushEvent) int { return int(a.Seq - b.Seq) })
	return fresh, nil
}

// heldValue is a field's newest value and when it took it.
type heldValue struct {
	value json.RawMessage
	at    time.Time
}

// currentValues is the newest value of every field the project holds.
func currentValues(ctx context.Context, tx pgx.Tx, project string) (map[fieldKey]heldValue, error) {
	rows, err := tx.Query(ctx, `
		select distinct on (issue_key, field) issue_key, field, value, observed_at
		from legion_observations where project = $1
		order by issue_key, field, observed_at desc
	`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	current := map[fieldKey]heldValue{}
	for rows.Next() {
		var key fieldKey
		var held heldValue
		if err := rows.Scan(&key.issue, &key.field, &held.value, &held.at); err != nil {
			return nil, err
		}
		current[key] = held
	}
	return current, rows.Err()
}

// pushedField is whether a field is one the pushed kinds set, which is what a snapshot replaces.
func pushedField(key fieldKey) bool {
	if key.issue == AdmissionKey {
		return key.field == FieldAdmission || key.field == FieldSettings || key.field == FieldSettingSources
	}
	switch key.field {
	case FieldPhase, FieldSlot, FieldWaitingOn, FieldPullRequestChecksVerdict:
		return true
	}
	return strings.HasPrefix(key.field, ClaimFieldPrefix)
}

// observationsOf turns the new events into the rows to write: one per event whose value differs
// from the field's newest, in seq order, so two changes of one field in one batch both land. An
// event older than the field's newest value (a lost event resent after later ones landed) is
// written into the history at its own time and leaves the newest value as it is. A snapshot also
// clears every pushed field it did not carry, at clearAt.
func observationsOf(events []PushEvent, current map[fieldKey]heldValue, snapshot bool, clearAt time.Time) []timedObservation {
	newest := maps.Clone(current)
	var rows []timedObservation
	set := func(key fieldKey, value json.RawMessage, at time.Time) {
		if len(value) == 0 {
			value = json.RawMessage("null")
		}
		held, ok := newest[key]
		switch {
		case !ok && string(value) == "null":
			return
		case ok && at.Before(held.at):
			rows = append(rows, timedObservation{fieldKey: key, value: value, at: at})
			return
		case ok && jsonEqual(held.value, value):
			return
		}
		newest[key] = heldValue{value: value, at: at}
		rows = append(rows, timedObservation{fieldKey: key, value: value, at: at})
	}
	settings, sources := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	if !snapshot {
		_ = json.Unmarshal(current[fieldKey{AdmissionKey, FieldSettings}].value, &settings)
		_ = json.Unmarshal(current[fieldKey{AdmissionKey, FieldSettingSources}].value, &sources)
	}
	carried := map[fieldKey]bool{}
	var settingsAt time.Time
	for _, event := range events {
		var key fieldKey
		value := event.Value
		switch event.Kind {
		case KindPhase:
			key = fieldKey{event.Issue, FieldPhase}
		case KindSlot:
			key = fieldKey{event.Issue, FieldSlot}
		case KindWaitingOn:
			key = fieldKey{event.Issue, FieldWaitingOn}
		case KindChecksVerdict:
			key = fieldKey{event.Issue, FieldPullRequestChecksVerdict}
		case KindWaitingLine:
			key = fieldKey{AdmissionKey, FieldAdmission}
		case KindClaimState:
			var claim claimStateValue
			if json.Unmarshal(event.Value, &claim) != nil || claim.Role == "" {
				continue
			}
			key = fieldKey{event.Issue, ClaimFieldPrefix + claim.Role}
			value = encodeValue(optional(claim.State))
		case KindSetting:
			var setting settingValue
			if json.Unmarshal(event.Value, &setting) != nil || setting.Key == "" {
				continue
			}
			settings[setting.Key] = setting.Value
			if len(setting.Source) > 0 {
				sources[setting.Key] = setting.Source
			}
			carried[fieldKey{AdmissionKey, FieldSettings}] = true
			carried[fieldKey{AdmissionKey, FieldSettingSources}] = true
			if snapshot {
				if event.At.After(settingsAt) {
					settingsAt = event.At
				}
				continue
			}
			set(fieldKey{AdmissionKey, FieldSettings}, encodeValue(settings), event.At)
			set(fieldKey{AdmissionKey, FieldSettingSources}, encodeValue(sources), event.At)
			continue
		default:
			continue
		}
		carried[key] = true
		set(key, value, event.At)
	}
	if !snapshot {
		return rows
	}
	if settingsAt.IsZero() {
		settingsAt = clearAt
	}
	set(fieldKey{AdmissionKey, FieldSettings}, encodeValue(settings), settingsAt)
	set(fieldKey{AdmissionKey, FieldSettingSources}, encodeValue(sources), settingsAt)
	for key := range newest {
		if pushedField(key) && !carried[key] && key.field != FieldSettings && key.field != FieldSettingSources {
			set(key, json.RawMessage("null"), clearAt)
		}
	}
	return rows
}

// jsonEqual compares two JSON texts by value, as Postgres's jsonb comparison does.
func jsonEqual(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return string(a) == string(b)
	}
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}

// writeObservations inserts the rows, each at its own time. Two values of one field at one
// instant keep the later one.
func writeObservations(ctx context.Context, tx pgx.Tx, project string, rows []timedObservation) error {
	if len(rows) == 0 {
		return nil
	}
	issues, fields, values, ats := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows)), make([]time.Time, len(rows))
	for index, row := range rows {
		issues[index], fields[index], values[index], ats[index] = row.issue, row.field, string(row.value), row.at
	}
	for start := 0; start < len(rows); {
		// One statement may not touch one (issue, field, at) twice under on conflict, so a batch
		// is cut wherever a point repeats.
		seen := map[string]bool{}
		end := start
		for ; end < len(rows); end++ {
			point := rows[end].issue + "\x00" + rows[end].field + "\x00" + rows[end].at.Format(time.RFC3339Nano)
			if seen[point] {
				break
			}
			seen[point] = true
		}
		if _, err := tx.Exec(ctx, `
			insert into legion_observations (project, issue_key, field, value, observed_at)
			select $1, o.issue, o.field, o.value::jsonb, o.at
			from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[]) as o(issue, field, value, at)
			on conflict (project, issue_key, field, observed_at) do update set value = excluded.value
		`, project, issues[start:end], fields[start:end], values[start:end], ats[start:end]); err != nil {
			return fmt.Errorf("record the Legion observations of %s: %w", project, err)
		}
		start = end
	}
	return nil
}

// PushFreshness is when the project's daemon last sent an event or a heartbeat; ok false when it
// never pushed.
func PushFreshness(ctx context.Context, pool *store.Pool, project string) (time.Time, bool, error) {
	var at time.Time
	err := pool.QueryRow(ctx, `select last_at from legion_pushes where project = $1`, project).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return at, err == nil, err
}
