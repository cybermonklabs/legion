package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker's stored private list (0041's known_private_projects): the last list of private
// projects the broker read from Dispatch, with their Slack channels, and the projects a switch to
// private recorded before Dispatch committed it. The airlock's public Slack fence falls back to the
// held rows when a read of Dispatch's list fails (requests.Machine.privateChannels): nothing while
// none is held, the held rows' channels otherwise.
//
// Every number here comes from one sequence, known_private_projects_seq: each record, each read's
// stamp and each forget that names no record. So a number is never used twice, even after a row is
// deleted and made again, and a forget that names a record can change only that record. Every time
// is the database's statement_timestamp(), plus DatabaseClockOffset, never a time a caller passes
// or a pod's clock, so pods whose clocks differ cannot move a row's hold. A row is held while its
// newest number, listed or recorded, is above its forgotten number; only a held row fences.

// MaxPrivateSwitchDeadline is the longest deadline a switch to private may run under, from before
// its transaction begins to its commit: Dispatch's switch sends its deadline with its record, and
// the record's route refuses one over this.
const MaxPrivateSwitchDeadline = 90 * time.Second

// KnownPrivateHold is how long a record, a listing or a forget keeps a row against every read that
// does not name it, and how long a record is held off from a forget that names no record: twice
// MaxPrivateSwitchDeadline, measured on the database's clock alone. A read stamped more than this
// after a record asked Dispatch more than twice the switch's deadline after it, so the switch had
// committed or rolled back before that read asked: the read names the project when the switch
// committed.
const KnownPrivateHold = 2 * MaxPrivateSwitchDeadline

// maxKnownPrivateChannels bounds one list of channels a read or a record stores: Dispatch's own
// bound on a project's list.
const maxKnownPrivateChannels = 16

// ErrPrivateProjectInput is a record or a read naming a key that is no Dispatch project key, more
// than 16 channels, or a channel that is no Slack conversation id. The table's checks refuse the
// same rows.
var ErrPrivateProjectInput = errors.New("store: a stored private project's key or channels are not ones Dispatch would send")

var (
	knownPrivateKey     = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	knownPrivateChannel = regexp.MustCompile(`^[CDG][A-Z0-9]{8,}$`)
)

// ClockOffset is what the stored private list adds to the database's time: zero in production,
// and set by a test that moves the database's time with its own clock, which no other way could
// step a 3-minute hold or a 16-minute settle. It is safe for the proxy's goroutines to read while
// a test sets it.
type ClockOffset struct{ nanos atomic.Int64 }

// Set makes the database's time read d later than statement_timestamp().
func (o *ClockOffset) Set(d time.Duration) { o.nanos.Store(int64(d)) }

// Get is the offset Set last set, zero when none was.
func (o *ClockOffset) Get() time.Duration { return time.Duration(o.nanos.Load()) }

// micros is the offset in microseconds, the unit every statement here adds it in.
func (o *ClockOffset) micros() int64 { return o.Get().Microseconds() }

// dbNow is the database's time as every statement here reads it: statement_timestamp() plus the
// offset, its parameter $1.
const dbNow = `(statement_timestamp() + $1::bigint * interval '1 microsecond')`

// KnownPrivateProject is one held row: the project's key, its Slack channels, sorted (nil while
// unknown), when the stored set last gained a channel (zero while unknown), and the database's
// time of the statement that read the row, so a caller compares the two on the database's clock.
type KnownPrivateProject struct {
	Key              string
	SlackChannels    []string
	ChannelsStoredAt time.Time
	ReadAt           time.Time
}

// PrivateListStamp is one read's stamp: a number from the sequence and the database's time,
// taken before the read asks Dispatch.
type PrivateListStamp struct {
	Number int64
	At     time.Time
}

// ForgetOutcome is what a forget did.
type ForgetOutcome string

const (
	// ForgetForgotten: there was no held row, or the forget took effect.
	ForgetForgotten ForgetOutcome = "forgotten"
	// ForgetSuperseded: the forget named a record that is not the row's current one; a later
	// switch recorded the project since, and the row is left as it is.
	ForgetSuperseded ForgetOutcome = "superseded"
	// ForgetInFlight: the forget named no record while the row's record is younger than
	// KnownPrivateHold, so its switch may still commit; the row is left as it is.
	ForgetInFlight ForgetOutcome = "in_flight"
)

// ForgetAnswer is ForgetPrivateProject's answer. RetryAt is set only for ForgetInFlight: the
// record's time plus KnownPrivateHold, on the database's clock, after which the same forget takes
// effect.
type ForgetAnswer struct {
	Outcome ForgetOutcome
	RetryAt time.Time
}

// StampPrivateList takes a read's stamp: the next number of the sequence and the database's time,
// in one statement. A read takes it before it asks Dispatch.
func (s *Store) StampPrivateList(ctx context.Context) (PrivateListStamp, error) {
	var stamp PrivateListStamp
	err := s.Pool.QueryRow(ctx, `select nextval('known_private_projects_seq'), `+dbNow, s.DatabaseClockOffset.micros()).
		Scan(&stamp.Number, &stamp.At)
	return stamp, err
}

// KeepPrivateProjects keeps a successful read of Dispatch's list, under the stamp taken before the
// read asked, in one transaction, and answers the projects it added or made held again.
//
// Each listed project is upserted: listed takes the greater of the row's and the stamp's number,
// with its time; the channels take the read's list (nil for an entry with no slack_channels key,
// which stores them unknown), unless the row's listing is already above the stamp, so a slower read
// stamped earlier changes nothing a later one wrote. channels_stored_at moves to the database's time
// when the new set holds a channel the stored one did not, or becomes known; else it stays. A row
// the read does not name is deleted only when its newest listing, record or forget is more than
// KnownPrivateHold older than the stamp's time.
func (s *Store) KeepPrivateProjects(ctx context.Context, stamp PrivateListStamp, listed []KnownPrivateProject) ([]string, error) {
	keys := make([]string, 0, len(listed))
	for _, project := range listed {
		if err := checkKnownPrivate(project.Key, project.SlackChannels); err != nil {
			return nil, err
		}
		keys = append(keys, project.Key)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after a commit is a no-op
	var added []string
	for _, project := range listed {
		var channels []string
		if project.SlackChannels != nil {
			channels = slices.Compact(slices.Sorted(slices.Values(project.SlackChannels)))
			if channels == nil {
				channels = []string{}
			}
		}
		heldBefore, err := heldKnownPrivate(ctx, tx, project.Key)
		if err != nil {
			return nil, err
		}
		var heldAfter bool
		err = tx.QueryRow(ctx, `insert into known_private_projects as k
				(project, slack_channels, channels_stored_at, listed, listed_at)
			values ($2, $3::text[], case when $3::text[] is null then null else `+dbNow+` end, $4, $5)
			on conflict (project) do update set
				slack_channels = case when k.listed > excluded.listed then k.slack_channels else excluded.slack_channels end,
				channels_stored_at = case
					when k.listed > excluded.listed then k.channels_stored_at
					when excluded.slack_channels is null then null
					when k.slack_channels is null then `+dbNow+`
					when exists (select 1 from unnest(excluded.slack_channels) c where c <> all (k.slack_channels)) then `+dbNow+`
					else k.channels_stored_at end,
				listed_at = case when k.listed > excluded.listed then k.listed_at else excluded.listed_at end,
				listed = greatest(k.listed, excluded.listed)
			returning greatest(coalesce(listed, 0), coalesce(record, 0)) > coalesce(forgotten, 0)`,
			s.DatabaseClockOffset.micros(), project.Key, channels, stamp.Number, stamp.At).Scan(&heldAfter)
		if err != nil {
			return nil, err
		}
		if heldAfter && !heldBefore {
			added = append(added, project.Key)
		}
	}
	if _, err := tx.Exec(ctx, `delete from known_private_projects
		where not (project = any ($1::text[]))
		and greatest(listed_at, recorded_at, forgotten_at) < $2::timestamptz - $3::bigint * interval '1 microsecond'`,
		keys, stamp.At, KnownPrivateHold.Microseconds()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return added, nil
}

// heldKnownPrivate locks key's row, when there is one, and answers whether it is held.
func heldKnownPrivate(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var held bool
	err := tx.QueryRow(ctx, `select greatest(coalesce(listed, 0), coalesce(record, 0)) > coalesce(forgotten, 0)
		from known_private_projects where project = $1 for update`, key).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return held, err
}

// RecordPrivateProject records key, with channels, for a switch to private, in one statement, and
// answers the record's number from the sequence. A new row takes channels ([] for none); a row of
// known channels takes their union with channels; a row of unknown channels keeps them unknown.
// channels_stored_at is the database's time for a new row and whenever the set gains a channel.
// The record's number is above every forget before it, so the row is held.
func (s *Store) RecordPrivateProject(ctx context.Context, key string, channels []string) (int64, error) {
	if err := checkKnownPrivate(key, channels); err != nil {
		return 0, err
	}
	sorted := slices.Compact(slices.Sorted(slices.Values(channels)))
	if sorted == nil {
		sorted = []string{}
	}
	var record int64
	err := s.Pool.QueryRow(ctx, `insert into known_private_projects as k
			(project, slack_channels, channels_stored_at, record, recorded_at)
		values ($2, $3::text[], `+dbNow+`, nextval('known_private_projects_seq'), `+dbNow+`)
		on conflict (project) do update set
			slack_channels = case when k.slack_channels is null then null
				else array(select distinct c from unnest(k.slack_channels || excluded.slack_channels) c order by c) end,
			channels_stored_at = case
				when k.slack_channels is null then null
				when exists (select 1 from unnest(excluded.slack_channels) c where c <> all (k.slack_channels)) then excluded.recorded_at
				else k.channels_stored_at end,
			record = excluded.record,
			recorded_at = excluded.recorded_at
		returning record`, s.DatabaseClockOffset.micros(), key, sorted).Scan(&record)
	return record, err
}

// ForgetPrivateProject forgets key's record, in one transaction with the row locked. record names
// the record a refused or rolled-back switch made; nil names none, which a switch to public and a
// switch whose outcome is unknown send.
//
// ForgetForgotten: there is no held row; or record is the row's current record, and the forgotten
// number becomes the greater of the row's and record, so a read that named the project after that
// record keeps it held; or record is nil and the row's record is absent or older than
// KnownPrivateHold, and the forgotten number is the sequence's next, above every listing and
// record so far. ForgetSuperseded: record is not the row's current record. ForgetInFlight: record
// is nil and the row's record is younger than KnownPrivateHold.
func (s *Store) ForgetPrivateProject(ctx context.Context, key string, record *int64) (ForgetAnswer, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ForgetAnswer{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after a commit is a no-op
	var (
		held       bool
		current    *int64
		recordedAt *time.Time
		now        time.Time
	)
	err = tx.QueryRow(ctx, `select greatest(coalesce(listed, 0), coalesce(record, 0)) > coalesce(forgotten, 0),
			record, recorded_at, `+dbNow+`
		from known_private_projects where project = $2 for update`,
		s.DatabaseClockOffset.micros(), key).Scan(&held, &current, &recordedAt, &now)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ForgetAnswer{Outcome: ForgetForgotten}, nil
	case err != nil:
		return ForgetAnswer{}, err
	case !held:
		return ForgetAnswer{Outcome: ForgetForgotten}, nil
	}
	var update string
	switch {
	case record != nil && (current == nil || *current != *record):
		return ForgetAnswer{Outcome: ForgetSuperseded}, nil
	case record != nil:
		update = `forgotten = greatest(coalesce(forgotten, 0), record)`
	case recordedAt != nil && !recordedAt.Add(KnownPrivateHold).Before(now):
		return ForgetAnswer{Outcome: ForgetInFlight, RetryAt: recordedAt.Add(KnownPrivateHold)}, nil
	default:
		update = `forgotten = nextval('known_private_projects_seq')`
	}
	if _, err := tx.Exec(ctx, `update known_private_projects set `+update+`, forgotten_at = `+dbNow+`
		where project = $2`, s.DatabaseClockOffset.micros(), key); err != nil {
		return ForgetAnswer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ForgetAnswer{}, err
	}
	return ForgetAnswer{Outcome: ForgetForgotten}, nil
}

// KnownPrivateProjects reads the held rows, sorted by key, in one statement, each with the
// database's time of that statement; an empty non-nil slice when none is held.
func (s *Store) KnownPrivateProjects(ctx context.Context) ([]KnownPrivateProject, error) {
	rows, err := s.Pool.Query(ctx, `select project, slack_channels, channels_stored_at, `+dbNow+`
		from known_private_projects
		where greatest(coalesce(listed, 0), coalesce(record, 0)) > coalesce(forgotten, 0)
		order by project`, s.DatabaseClockOffset.micros())
	if err != nil {
		return nil, err
	}
	known := []KnownPrivateProject{}
	for rows.Next() {
		var (
			project  KnownPrivateProject
			storedAt *time.Time
		)
		if err := rows.Scan(&project.Key, &project.SlackChannels, &storedAt, &project.ReadAt); err != nil {
			rows.Close()
			return nil, err
		}
		if storedAt != nil {
			project.ChannelsStoredAt = *storedAt
		}
		known = append(known, project)
	}
	return known, rows.Err()
}

// checkKnownPrivate refuses a key or channels Dispatch would not send (ErrPrivateProjectInput).
func checkKnownPrivate(key string, channels []string) error {
	switch {
	case !knownPrivateKey.MatchString(key):
		return fmt.Errorf("%w: %q is no project key", ErrPrivateProjectInput, key)
	case len(channels) > maxKnownPrivateChannels:
		return fmt.Errorf("%w: %d channels, more than %d", ErrPrivateProjectInput, len(channels), maxKnownPrivateChannels)
	}
	for _, channel := range channels {
		if !knownPrivateChannel.MatchString(channel) {
			return fmt.Errorf("%w: %q is no Slack conversation id", ErrPrivateProjectInput, channel)
		}
	}
	return nil
}
