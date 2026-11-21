package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ruleSet is a version of the event rules as a fill runs it: the version, the rule of each event
// type it reads, the read of each kind of post's whole body (its present row's later types and
// its body history), and whether it reads the body history. types and askTypes are the types its
// rules read and the ask's among them, sorted. A fill runs this build's (currentRules); a test
// runs an earlier version's to stand for a fill of an earlier build, which wrote the rows this
// build's re-read reads again (rereadBatch).
type ruleSet struct {
	version         int
	rules           map[string]rule
	posts           map[string]postBodyRead
	history         bool
	types, askTypes []string
}

// newRuleSet is the rule set of version, with rules, posts and whether it reads the body history.
func newRuleSet(version int, rules map[string]rule, posts map[string]postBodyRead, history bool) ruleSet {
	types := slices.Sorted(maps.Keys(rules))
	asks := slices.DeleteFunc(slices.Clone(types), func(eventType string) bool {
		return !strings.HasPrefix(eventType, "ask.")
	})
	return ruleSet{version: version, rules: rules, posts: posts, history: history, types: types, askTypes: asks}
}

// currentRules is this build's rules: version rulesVersion, versionRules, postBodyReads and the
// body history read.
var currentRules = newRuleSet(rulesVersion, versionRules, postBodyReads, true)

// rulesAdded names, for each rules version after the first, the event types its rules read that
// the version before it did not: the types a fill of a lower stored version passed over, which a
// batch of this build reads again (rereadTypes).
var rulesAdded = map[int][]string{
	2: {"comment.deleted", "message.deleted", "message.edited"},
}

// rereadTypes is the types the rules of version current read that version stored's did not,
// sorted: those rulesAdded names for each version above stored up to current.
func rereadTypes(stored, current int) []string {
	var types []string
	for version := stored + 1; version <= current; version++ {
		types = append(types, rulesAdded[version]...)
	}
	slices.Sort(types)
	return slices.Compact(types)
}

// changeTypes is the types again, which a re-read decides again, and every type that changes a
// post's text after the event that wrote it under r (postBodyRead.laterTypes), sorted: the types
// whose events a re-read batch reads (rereadEventsQuery).
func (r *ruleSet) changeTypes(again []string) []string {
	types := slices.Clone(again)
	for _, read := range r.posts {
		types = append(types, read.laterTypes...)
	}
	slices.Sort(types)
	return slices.Compact(types)
}

// rereadProgress is where a re-read under way stands, which the filler keeps between its
// batches: the cursor it reads up to, and the last event its committed batches read. A batch that
// finds another cursor starts the re-read again from the log's first event, and so does a filler
// that starts again: its batches write only what a batch of this build writes, and a version the
// table holds is kept (insertAgainSQL), so a re-read read again writes nothing twice.
type rereadProgress struct {
	cursor, after int64
}

// rereadBatch writes one batch of the re-read a batch of this build makes where the cursor row
// holds a lower rules version (stored) than the fill's, in tx, which holds the cursor row and has
// checked the hold and the log (batch). It makes the table what a fill of this build's rules alone
// would have made of the events up to the cursor, which a fill of the lower version wrote some of:
//
//   - It reads, after the last event the re-read's earlier batches read, the events of the types
//     that change a post's text (changeTypes, rereadLogTypes), within the batch's bounds, through
//     their own index (rereadEventsQuery, events_post_text_changes), to the log's end: a lower
//     version's row of a post can hold a text an event of these types changed, whatever that
//     event's id, since the row was read from the post when that version's batch wrote it.
//   - It decides again (decideReread) each of those events of the types this build's rules read
//     and version stored's did not (rereadTypes), whose id is at or below the cursor: the events
//     above it a batch of this build writes after the re-read.
//   - It redoes each row of the posts those events name whose body the row's event cut and that
//     version stored read from another source than this build reads first (decider.redo).
//   - Its last batch, the one whose read reaches the log's end, raises the cursor row's rules
//     version (raiseRulesQuery) in the same transaction.
//
// Its decisions, its redo and its inserts write only rows of posts, which only the fill writes,
// one batch at a time under the cursor row: a delete of a comment writes no row of the post, whose
// empty version the fill writes from the delete's event; so they take no lock. Each batch is
// bounded as a batch is: its read takes at most maxEvents events and maxBytes of what the read
// counts, under the statement time batchBoundsQuery set; its decision ends it at the first event
// that takes what the batch reads of the events, of their versions' whole bodies and of the bodies
// it redoes past maxBytes; its writes take the statement time of its bytes; the pause runs before
// it and the rest after it (batchesTo). The cursor does not move. A re-read stopped before its
// last batch commits leaves the rules version stored as it was, so the next batch reads again; and
// a batch of a lower version that writes again lowers it again (advanceCursorQuery), so the next
// batch of this build reads again everything up to the new cursor.
func (f *filler) rereadBatch(ctx context.Context, tx pgx.Tx, started time.Time, cursor int64, stored int) (batchResult, error) {
	if f.reread.cursor != cursor {
		f.reread = rereadProgress{cursor: cursor}
	}
	again := rereadTypes(stored, f.rules.version)
	changes := f.rules.changeTypes(again)
	read, candidates, err := f.readLog(ctx, tx, rereadEventsQuery, f.reread.after, math.MaxInt64, changes, nil)
	if err != nil {
		return batchResult{}, err
	}
	got, n, err := decideReread(ctx, tx, f.logger, f.rules, read, cursor, again, f.maxBytes)
	if err != nil && !errors.Is(err, errUnreadablePayload) {
		return batchResult{}, fmt.Errorf("keyword fill: read again: %w", err)
	}
	unreadable := err
	// The read reached the log's end: it took every candidate, and fewer than its bound.
	end := len(read) == candidates && candidates < f.maxEvents
	whole := n == len(read)
	read = read[:n]
	result := batchResult{
		reread: true, events: n, bytes: got.Bytes, rows: len(got.Versions), cut: got.Cut,
		redone: len(got.redone), cursor: cursor,
	}
	if n > 0 {
		result.first = read[0].ID
	}
	raise := unreadable == nil && whole && end
	if _, err := tx.Exec(ctx, batchStatementQuery, statementTimeout(result.bytes)); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: bound the re-read's statements: %w", err)
	}
	if err := writeRedone(ctx, tx, got.redone); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: read again: %w", err)
	}
	if err := insertVersions(ctx, tx, got.Versions, true); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: read again: %w", err)
	}
	if raise {
		if _, err := tx.Exec(ctx, raiseRulesQuery, f.rules.version); err != nil {
			return batchResult{}, fmt.Errorf("keyword fill: raise the rules version to %d: %w", f.rules.version, err)
		}
		result.raised = true
	}
	if err := tx.Commit(ctx); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: commit the re-read's batch: %w", err)
	}
	result.took = time.Since(started)
	switch {
	case raise:
		f.reread = rereadProgress{}
	case n > 0:
		f.reread.after = read[n-1].ID
	}
	if unreadable != nil {
		return result, fmt.Errorf("keyword fill: read again: %w", unreadable)
	}
	return result, nil
}

// rereadDecided is what decideReread made of a re-read batch's events: the versions of the
// events it decided again, with their bytes and the bytes of every body it read (decided), and
// the rows it redid, each with its kind, its id, its event_seq and the body and body_from this
// build's rules give it.
type rereadDecided struct {
	decided
	redone []version
}

// rereadPost is what decideReread read of one event of its batch: the post it names, where it
// names one the keyword table holds (named), and whether it is decided again (again).
type rereadPost struct {
	key          item
	named, again bool
}

// decideReread decides a re-read batch (rereadBatch) of events, in ascending id order, in tx under
// rules: each event of the types again whose id is at or below cursor is decided again, and the
// rows of every post an event names are redone first (decider.redo), so each decision reads them
// as this build's rules would have written them.
//
// An event decided again is decided against the row its post held just before the event's own
// seq (priorVersions), not the post's latest row: the table holds rows of the post's later
// events that a fill of the lower version wrote, past this one. A version decided earlier in the
// batch of the same post comes before it too, and the later of the two is the one read
// (decider.latest). An event's post is the one its rule names, or, for an event only redone, the
// one its payload names (decodePost): a payload no rule can read stops the batch at an event it
// decides again, as it stops a batch (errUnreadablePayload), and names no post at an event it
// only redoes, whose own batch above the cursor stops there in its turn.
//
// Each event counts against maxBytes, in order, what a batch counts of it (decider.size), and, at
// the first event of a post, the bodies of the post's redone rows: the first event past the bound,
// after the first, ends the events decided, and the redone rows of the posts it alone names are
// left with it. It answers what it decided, how many events from the first it took, and the error
// naming an unreadable payload where one ended them.
func decideReread(ctx context.Context, tx pgx.Tx, logger *slog.Logger, rules *ruleSet, events []event, cursor int64, again []string, maxBytes int64) (rereadDecided, int, error) {
	decide := make(map[string]bool, len(again))
	for _, eventType := range again {
		decide[eventType] = true
	}
	posts := make([]rereadPost, len(events))
	last := map[item]int32{}
	var items []item
	var priors []priorKey
	read := len(events)
	var unreadable error
	for index, e := range events {
		if index > 0 && e.ID <= events[index-1].ID {
			return rereadDecided{}, 0, fmt.Errorf("read again: event %d comes after event %d, out of id order", e.ID, events[index-1].ID)
		}
		var post rereadPost
		if apply, ok := rules.rules[e.Type]; ok && e.ID <= cursor && decide[e.Type] {
			v, started, err := apply(e, noLatest)
			if err != nil {
				if !errors.Is(err, errUnreadablePayload) {
					return rereadDecided{}, 0, eventError(e, err)
				}
				read, unreadable = index, eventError(e, err)
				break
			}
			if started {
				post = rereadPost{key: item{v.Kind, v.ID}, named: true, again: true}
				priors = append(priors, priorKey{event: e.ID, post: post.key, seq: e.Seq})
			}
		} else {
			kind, _, _ := strings.Cut(e.Type, ".")
			if p, _, ok, err := decodePost(e, kind); err == nil && ok {
				post = rereadPost{key: item{kind, *p.ID}, named: true}
			}
		}
		if !post.named {
			continue
		}
		posts[index] = post
		if seq, seen := last[post.key]; !seen {
			items = append(items, post.key)
		} else if seq > e.Seq {
			continue
		}
		last[post.key] = e.Seq
	}
	events, posts = events[:read], posts[:read]
	d := &decider{tx: tx, logger: logger, rules: rules, batch: map[item]*version{}, specs: map[specVersion]specText{}}
	var out rereadDecided
	redone, err := d.redo(ctx, items, last)
	if err != nil {
		return rereadDecided{}, 0, err
	}
	if d.before, err = priorVersions(ctx, tx, priors); err != nil {
		return rereadDecided{}, 0, err
	}
	for _, prior := range d.before {
		for _, row := range redone[item{prior.Kind, prior.ID}] {
			if row.EventSeq == prior.EventSeq {
				prior.Body, prior.BodyFrom = row.Body, row.BodyFrom
			}
		}
	}
	counted := map[item]bool{}
	for index, e := range events {
		post := posts[index]
		size := e.PayloadBytes + e.RowBodyBytes
		var v version
		var starts bool
		if post.again {
			if v, starts, err = d.start(ctx, e); err != nil {
				return rereadDecided{}, 0, eventError(e, err)
			}
			if size, err = d.size(e, v, starts); err != nil {
				return rereadDecided{}, 0, eventError(e, err)
			}
		}
		first := post.named && !counted[post.key]
		if first {
			for _, row := range redone[post.key] {
				size += int64(len(row.Body))
			}
		}
		if index > 0 && d.out.Bytes+size > maxBytes {
			out.decided = d.out
			return out, index, nil
		}
		d.out.Bytes += size
		if first {
			counted[post.key] = true
			out.redone = append(out.redone, redone[post.key]...)
		}
		d.flush()
		if starts {
			d.keep(v)
		}
	}
	out.decided = d.out
	return out, read, unreadable
}

// redoRowsQuery reads each row of each post ($1 kinds, $2 ids) whose event_seq is below $3, the
// greatest seq of the post's events in the batch, and whose body its event cut and was read from
// another source than the event's own payload or the post's body history: its present row, its
// first revision's blocks, or none (the event's cut text). It reads them by the primary key, with
// the columns of the row's event that the whole body's read needs and the event's own text.
const redoRowsQuery = `
	select r.kind, r.id, r.event_seq, r.body, r.body_from,
	       e.id, e.type, e.issue_key, e.artifact_id::text, e.seq, e.created_at, coalesce(e.payload->>'body', '')
	  from unnest($1::text[], $2::text[], $3::integer[]) as p (kind, id, seq)
	  join public.search_text_versions r on r.kind = p.kind and r.id = p.id and r.event_seq < p.seq
	  join public.events e on e.id = r.event_id
	 where r.body_from in ('row', 'blocks', 'cut')
	 order by r.kind, r.id, r.event_seq`

// redo decides again, under the decider's rules, the whole body of each row of the posts items
// that comes before an event of the post in the batch (last, the greatest seq of each post's) and
// whose body its event cut and was read from another source than the post's body history
// (redoRowsQuery), as a batch of these rules decides it (wholeBody), and answers, by post, the
// rows whose body or body_from that changes, each with its kind, its id, its event_seq and its new
// body and body_from.
//
// A fill of rules version 1 read such a body from the post's present row where no later edit of
// a comment changed it, and never from the body history: so a row it read from a message's row,
// or from a comment's row before its delete, holds a text a later edit or delete wrote, or the
// empty body, and a row it read from the blocks or kept cut, a body the history holds whole. A
// row this build's rules read from the present row with no later change of the post then is
// redone too where such a change came later: its body is the one the history gives it, which a
// fill of these rules alone would read now, and only its body_from changes.
func (d *decider) redo(ctx context.Context, items []item, last map[item]int32) (map[item][]version, error) {
	if len(items) == 0 {
		return nil, nil
	}
	kinds, ids, seqs := make([]string, len(items)), make([]string, len(items)), make([]int32, len(items))
	for index, key := range items {
		kinds[index], ids[index], seqs[index] = key.kind, key.id, last[key]
	}
	type stale struct {
		row        version
		e          event
		body, from string
	}
	rows, err := d.tx.Query(ctx, redoRowsQuery, kinds, ids, seqs)
	if err != nil {
		return nil, fmt.Errorf("read the rows to decide again of %d posts: %w", len(items), err)
	}
	var stales []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.row.Kind, &s.row.ID, &s.row.EventSeq, &s.body, &s.from,
			&s.e.ID, &s.e.Type, &s.e.IssueKey, &s.e.ArtifactID, &s.e.Seq, &s.e.CreatedAt, &s.row.Body); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the rows to decide again of %d posts: %w", len(items), err)
		}
		stales = append(stales, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the rows to decide again of %d posts: %w", len(items), err)
	}
	redone := map[item][]version{}
	for _, s := range stales {
		v := s.row
		v.BodyTruncated = true
		if err := d.wholeBody(ctx, s.e, &v); err != nil {
			return nil, err
		}
		if v.Body != s.body || v.BodyFrom != s.from {
			key := item{v.Kind, v.ID}
			redone[key] = append(redone[key], v)
		}
	}
	return redone, nil
}

// priorKey names an event a re-read batch decides again: its id, its post and its seq.
type priorKey struct {
	event int64
	post  item
	seq   int32
}

// priorVersionsQuery reads, for each event ($1 its ids, $2 and $3 its post's kind and id, $4 its
// seq), the row its post held just before it: the one with the greatest event_seq below the
// event's, read backwards through the primary key, one probe for each event.
const priorVersionsQuery = `
	select k.event_id, l.kind, l.id, l.event_seq, l.body, l.body_from
	  from unnest($1::bigint[], $2::text[], $3::text[], $4::integer[]) as k (event_id, kind, id, seq)
	  cross join lateral (
	        select v.kind, v.id, v.event_seq, v.body, v.body_from
	          from public.search_text_versions v
	         where v.kind = k.kind and v.id = k.id and v.event_seq < k.seq
	         order by v.event_seq desc
	         limit 1) l`

// priorVersions reads, in tx, the row each event of keys's post held just before the event
// (priorVersionsQuery), by the event's id: an event whose post held none is not in the map, which
// is never nil. A version holds its kind, its id, its event_seq, its body and its body_from.
func priorVersions(ctx context.Context, tx pgx.Tx, keys []priorKey) (map[int64]*version, error) {
	before := make(map[int64]*version, len(keys))
	if len(keys) == 0 {
		return before, nil
	}
	events, kinds, ids, seqs := make([]int64, len(keys)), make([]string, len(keys)), make([]string, len(keys)), make([]int32, len(keys))
	for index, key := range keys {
		events[index], kinds[index], ids[index], seqs[index] = key.event, key.post.kind, key.post.id, key.seq
	}
	rows, err := tx.Query(ctx, priorVersionsQuery, events, kinds, ids, seqs)
	if err != nil {
		return nil, fmt.Errorf("read the rows before %d events read again: %w", len(keys), err)
	}
	defer rows.Close()
	for rows.Next() {
		var event int64
		var v version
		if err := rows.Scan(&event, &v.Kind, &v.ID, &v.EventSeq, &v.Body, &v.BodyFrom); err != nil {
			return nil, fmt.Errorf("read the rows before %d events read again: %w", len(keys), err)
		}
		before[event] = &v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the rows before %d events read again: %w", len(keys), err)
	}
	return before, nil
}

// redoQuery writes the rows a re-read redid ($1 kinds, $2 ids, $3 event_seqs): each one's body
// ($4), its body_from ($5) and its vector, made from the body as the fill's insert makes a post's
// (insertVersionsQuery). It is the one update of a row the fill wrote that this table takes.
const redoQuery = `
	update public.search_text_versions t
	   set body = r.body, body_from = r.body_from, search = search_vector('', search_text(r.body))
	  from unnest($1::text[], $2::text[], $3::integer[], $4::text[], $5::text[]) as r (kind, id, event_seq, body, body_from)
	 where t.kind = r.kind and t.id = r.id and t.event_seq = r.event_seq`

// writeRedone writes rows, the rows a re-read redid, in tx, in one statement (redoQuery), and
// nothing for none.
func writeRedone(ctx context.Context, tx pgx.Tx, rows []version) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	kinds, ids, bodies, froms, seqs := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]int32, n)
	for index, v := range rows {
		kinds[index], ids[index], seqs[index], bodies[index], froms[index] = v.Kind, v.ID, v.EventSeq, v.Body, v.BodyFrom
	}
	if _, err := tx.Exec(ctx, redoQuery, kinds, ids, seqs, bodies, froms); err != nil {
		return fmt.Errorf("write %d rows decided again: %w", n, err)
	}
	return nil
}
