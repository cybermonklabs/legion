package legionfacts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// feeds.go holds the three feeds one page of a pass reads and the cursors it reads them from: a
// page of the issues' events, a page of the daemon's observations and a page of the delivery
// changes. Each page answers the issues its rows name and where its feed now stands. This file
// also holds the read of the three cursor rows and the write that moves them.
//
// It stands apart from the loop (consumer.go) because each feed carries rules of its own: where
// a page stops, which issues a row of it names, and when its cursor may move. The loop reads a
// page, states the work the page names and writes it. The page size, the cursor names and the
// change kind this loop keeps are consts of the loop (consumer.go).

// pages is what one page of a pass read of the three feeds: a page of each, whether this page
// acts on a change cursor that had fallen below the oldest change the log still holds (lost),
// whether a feed holds more rows, which makes the pass read another page at once (more), and
// whether that page must carry the work no feed names, because this one met a lost change cursor
// it may not answer and held it (unfed, readPages).
type pages struct {
	events       feed
	observations feed
	changes      feed
	lost         bool
	more         bool
	unfed        bool
}

// readPages is one page of each of the three feeds, read from where each cursor stands. checked
// is this page's own reading of the consumer's clock (passPage), which the change cursor is judged
// at and a read of the change log to its end is stamped with; the rows are computed as of the
// pass's moment.
//
// A database holding no cursor row at all (committed false) has committed no pass, because every
// pass writes the three together (advanceCursors), so every feed would be read from its start. It
// is read from its head instead (readFeedHeads): the first page of such a pass computes every
// project whole, because no project has a record of a whole read, and a whole project is computed
// from the history itself rather than from the feeds, so the pages below that head hold nothing the
// whole read has not already answered. Reading them would recompute every project once per page
// - about 813 pages of a log the size of INFRA's - while the rows they named waited behind it.
//
// The change cursor's ReadAt moves to checked where the page read the change log to its end (a
// page shorter than its limit, readChangePage) or moved the cursor to the log's head, and stands
// where it stood on a full page and on a held cursor (delivery.ChangeCursor and its Through), so
// the lost-cursor rule measures how long since a page last read the whole log, however many full
// pages came since.
//
// A change cursor that may have passed over changes it never read (delivery.CursorLost, judged
// on the database's clock for the change at the cursor and on checked against the cursor's
// ReadAt) is answered by computing every project whole, which covers every change the log ever
// held. The page that carries the work no feed names (unfed, passPage) is the page that carries
// that whole read, so it is the only page that may move this cursor.
//
// On such a page the cursor moves to the head of the change log rather than to the end of the
// page it read. The whole read leaves nothing below the head to read, and a cursor left where it
// stood would read lost again on the next pass and compute every project whole on every pass for
// as long as no change commits.
//
// Any other page leaves the cursor where it stood, its ReadAt with it, and answers that the page
// after it must carry the work no feed names (pages.unfed), which the pass reads at once.
// readChangePage has already moved the position it answers past the pruned gap, to the first
// change the log still holds, so a page that wrote that position back would take the loss away
// from every later page: the next would read a change that exists and answer lost false. The
// pruned gap's rows would then stay stale for ever, since a pull request merged inside the gap is
// read by no page and no pass computes a row its feeds did not name. A held cursor stamped with
// checked would take the loss away as well where its reader stalled, since the page after it
// would find it read a moment ago. The issues that page's own changes named are kept and
// computed, which is work the next page makes again from the held cursor.
func readPages(ctx context.Context, q Querier, cursors feedCursors, committed bool, safe int64, checked time.Time, limit int, unfed bool) (pages, error) {
	if !committed {
		return readFeedHeads(ctx, q, safe, checked)
	}
	read := pages{}
	var err error
	if read.events, err = readEventPage(ctx, q, cursors.events, safe, limit); err != nil {
		return pages{}, err
	}
	if read.observations, err = readObservationPage(ctx, q, cursors.observations, limit); err != nil {
		return pages{}, err
	}
	if read.changes, read.lost, err = readChangePage(ctx, q, cursors.changes, checked, limit); err != nil {
		return pages{}, err
	}
	read.more = read.events.full || read.observations.full
	if !read.lost {
		read.more = read.more || read.changes.full
		return read, nil
	}
	if !unfed {
		held := cursors.changes
		read.changes.position, read.changes.at, read.changes.readAt, read.lost = held.Seq, held.At, held.ReadAt, false
		read.more, read.unfed = true, true
		return read, nil
	}
	head, err := delivery.ChangesHead(ctx, q)
	if err != nil {
		return pages{}, fmt.Errorf("read the head of the delivery changes: %w", err)
	}
	read.changes.position, read.changes.at, read.changes.readAt = head.Seq, head.At, checked
	return read, nil
}

// readFeedHeads is the three feeds standing at their heads and naming no issue: where the
// cursors of a database no pass has committed on start, the change cursor stamped checked, the
// page's own reading of the clock, since a move to the log's head is a read of it to its end. The
// events feed's head is the safe horizon rather than the newest id, because an event above the
// horizon may still be committing and a later pass reads it (readEventPage).
func readFeedHeads(ctx context.Context, q Querier, safe int64, checked time.Time) (pages, error) {
	var observations int64
	if err := q.QueryRow(ctx, `select coalesce(max(id), 0) from legion_observations`).Scan(&observations); err != nil {
		return pages{}, fmt.Errorf("read the head of the Legion observations: %w", err)
	}
	changes, err := delivery.ChangesHead(ctx, q)
	if err != nil {
		return pages{}, fmt.Errorf("read the head of the delivery changes: %w", err)
	}
	return pages{
		events:       feed{position: safe},
		observations: feed{position: observations},
		changes:      feed{position: changes.Seq, at: changes.At, readAt: checked},
	}, nil
}

// feed is what one page of one feed read: where that feed now stands, the issues it named, the
// filings it read (the events feed alone reads any), and whether the page was full. at and readAt
// are the changes feed's alone, which the change cursor keeps for the lost-cursor rule
// (delivery.CursorLost): at is when the change at position was recorded, nil where no change holds
// position, and readAt when a page last read the change log to its end, the page's own reading of
// the clock where it did and the cursor's earlier one where its page was full.
type feed struct {
	position int64
	at       *time.Time
	readAt   time.Time
	touches  []touch
	filings  []filing
	full     bool
}

// filing is one issue a session of the page filed whose stored filing the page can have moved
// (sessionFilings): the issue whose `issue.created` event the session wrote, that session's
// newest title (legion.ParseSessionTitle) and whether it is a Legion title at all, which say
// which project the filing is owed to now, and the project whose issue_filings holds it, "" for
// none, which is owed the filing's deletion where it is no longer that project's.
type filing struct {
	issue  string
	filer  legion.SessionTitle
	legion bool
	stored string
}

// touch is one issue a feed named: the project it belongs to, the issue itself, whether the row
// that named it moves the issue between trees, and whether the row is the project's admission
// block. Two rows of two feeds move an issue between trees, and they are one kind of change:
// Dispatch's parent link, which an event's `changed` names (readEventPage), and the `tree` the
// daemon reports, which is the field of an observation (readObservationPage). The rule that places
// an issue in a tree reads the `tree` first and the parent link only where no daemon reported one
// (legion.TreeChain), so either row can take the issue out of one tree and put it in another.
//
// A move states two trees: the tree the issue joined, which the links now give (rowsTouchedBy),
// and the tree it left, which is read from the issue's stored root_key (leftTrees).
//
// The admission block is an observation of the project under legion.AdmissionKey, which is no
// issue's key and holds no stored row. Where it moved what a waiting reason reads of it, it moves
// the reason of rows nothing else on the page names (readObservationPage), so the pass states the
// project's open rows for it (openRows).
type touch struct {
	project   string
	issue     string
	moved     bool
	admission bool
}

// readEventPage is the page of events after the cursor: up to limit events above after and at or
// below safe, with the issues those events name and the issues the sessions those events name can
// have moved through their titles (sessionIssues).
//
// The page stops at safe because events.id is not commit order. An event draws its id when its
// write inserts the row, and two writes that wait on nothing commit out of id order, so a cursor
// moved by an uncapped `id > cursor` read could pass an event that was still committing and
// never read it (events, the "Safe horizon" section). safe is the horizon the pass awaited, which
// is at or above every event committed before the pass started (pass), so an event above it
// committed after that, and its own commit's notification wakes the pass that reads it.
//
// `changed` holds the fields an issue write moved (api's issueChangedFields), so an event naming
// `parent` is the parent link moving. That is one of the two rows that move an issue between
// trees (touch), and a move widens the set a change touches by the tree the issue left
// (leftTrees). The issues below the issue are in that set whatever the event was
// (rowsTouchedBy).
//
// An event that is on no issue at all is kept where its actor is a session, because such an
// event carries that session's title as much as an event on an issue does, and a session's
// newest title decides which issues every event of that session works (legion.PhaseWorkKeys). A
// session that writes on a project or on an artifact therefore moves the facts of the issues
// its own title can give work to or take it from, and no issue of its own names it. The event's
// own project is not the work: the issues are read from the session and from the title the
// event carries (sessionIssues).
//
// An `issue.created` event a session wrote is a filing, and the session's newest title says which
// project counts it, as metrics reads it (legion.SessionTitle's OfProject): wherever the filed
// issue lives. So every session the page names can have moved a filing: one it files on this
// page, and one it filed long ago, whose project, filer and class move the moment the session
// writes under a title that names another issue (sessionFilings). The page answers each such
// filing with the filer its newest title names and the project that stores it, and the pass owes
// it to both projects (passPage), so one pass writes it into the one and takes it out of the
// other.
//
// A project.updated event, a write of the project's row (its default repository among what it can
// move, api's putProjectDefaultRepository), moves no stored row: a row's repo is the repository its
// issue's own chain names, or ':default' where none does (ReadRepos), which the evals' reads take
// as the project's published default repository, so a default move rewrites no row.
func readEventPage(ctx context.Context, q Querier, after, safe int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select event.id, coalesce(issue.project_key, ''), coalesce(event.issue_key, ''),
		       coalesce(event.payload -> 'changed' @> '["parent"]', false),
		       case when event.actor ->> 'kind' = 'session' then coalesce(event.actor ->> 'id', '') else '' end,
		       case when event.actor ->> 'kind' = 'session'
		            then coalesce(event.actor -> 'origin' ->> 'session_title', '') else '' end
		from events event
		left join issues issue on issue.key = event.issue_key
		where event.id > $1 and event.id <= $2
		order by event.id
		limit $3
	`, after, safe, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	count := 0
	sessions := map[string]bool{}
	titles := map[string]bool{}
	for rows.Next() {
		var touched touch
		var session, title string
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved, &session, &title); err != nil {
			return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
		}
		count++
		if session != "" {
			sessions[session] = true
		}
		if title != "" {
			titles[title] = true
		}
		if touched.project != "" {
			page.touches = append(page.touches, touched)
		}
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	page.full = count == limit
	named := slices.Sorted(maps.Keys(sessions))
	worked, filed, err := sessionIssues(ctx, q, named, slices.Sorted(maps.Keys(titles)))
	if err != nil {
		return feed{}, err
	}
	page.touches = append(page.touches, worked...)
	if page.filings, err = sessionFilings(ctx, q, filed); err != nil {
		return feed{}, err
	}
	return page, nil
}

// sessionFiling is one issue a session of the page filed, as sessionIssues reads it: the issue
// whose `issue.created` event the session wrote, the session, and the issue_filings row that
// holds the filing (its project, the filer's role and the filer's issue), each "" where no row
// does.
type sessionFiling struct {
	issue, session, project, role, filer string
}

// sessionFilings is every filing of filed that its session's newest title says is stored wrong:
// where the session's newest title (read by primary key, delivery.ResolveSessionTitles, because
// that title and not the one the event carried is what metrics classifies a filing by) is a
// Legion title and no row holds the filing, or a row holds it under another project, another role
// or another filer's issue than that title names, or under any project once the title is no
// Legion title. A filing whose row already names the filer the newest title names is left out:
// its project and its filer stand, and the pass recomputes it with its filer's issue
// (filingsToCompute) whenever that issue's own inputs move.
//
// That is the filings a new filing on the page adds, and the filings a retitle moves: a session
// moves its filings only by writing under a new title, and an event that carries the title names
// the session on the page that carries it, so the page's sessions are every session that can
// have moved one.
//
// filed is every filing the page's sessions made, which sessionIssues reads in the scan of those
// sessions' events it makes for the issues they worked. A broad session, a controller's, has
// filed many issues, and the read answers them all on every page it writes on, while the pass
// computes only the ones its newest title moved.
func sessionFilings(ctx context.Context, q Querier, filed []sessionFiling) ([]filing, error) {
	if len(filed) == 0 {
		return nil, nil
	}
	filers := map[string]bool{}
	for _, one := range filed {
		filers[one.session] = true
	}
	titles, err := delivery.ResolveSessionTitles(ctx, q, slices.Sorted(maps.Keys(filers)))
	if err != nil {
		return nil, fmt.Errorf("resolve the titles of %d filing sessions: %w", len(filers), err)
	}
	filings := []filing{}
	for _, one := range filed {
		filer, legionSession := legion.ParseSessionTitle(titles[one.session])
		stands := legionSession && one.project != "" && filer.OfProject(one.project) &&
			filer.Role == one.role && filer.Issue == one.filer
		if stands || (!legionSession && one.project == "") {
			continue
		}
		filings = append(filings, filing{issue: one.issue, filer: filer, legion: legionSession, stored: one.project})
	}
	return filings, nil
}

// sessionIssues is what one page of events can have moved through the titles its sessions
// carry: of the issues those sessions wrote an event on, the ones that hold a stored row, and
// the issues the titles on that page name. Each comes with the project it belongs to. sessions
// is every session the page's events name and titles every session title they carry. Beside
// them it answers every issue those sessions filed (sessionFiling), in key order, which
// sessionFilings judges.
//
// It reads nothing where both inputs are empty, and it answers no issue. A page of events is
// not known to reach that state, because a title is read off an event whose actor is a session
// and such an event names that session as well. The guard asks for both inputs all the same,
// since either one alone names issues the pass must compute.
//
// A session's newest title decides which of the issues it wrote on carry its work
// (legion.PhaseWorkKeys), so one write anywhere by one session can take the work off one issue and
// give it to another. Neither issue is named by the event itself: the issue that lost the work
// has no event of its own, and the issue that gained it is named by a title rather than by a
// key. They join the keys the pass computes, and nothing downstream treats them differently - a
// row is written or deleted only because the computation of that issue said so.
//
// Those two are the whole of what these events can move. A session gives its work to the one
// issue its newest title names (legion.WorkerTitle, whose Works reads the title's own key), and
// a title moves only by an event that carries it, so the page that carries the title is the
// page that must answer for the gain. The titles of the page therefore name every issue these
// events can give work to. A title on the page that a newer title of the same session has
// already replaced names an issue that gains nothing, and the computation of that issue
// answers the row it answered before.
//
// A session takes its work away from an issue by no longer naming it, and what that
// takes away is a stored row, so an issue holding none has nothing to lose: its computation
// answered no row before this page and answers none after it. The other half of the phase-work
// rule is a handoff, which is recorded in an event on the issue it names, so the page names
// that issue itself, and a handoff is never taken back.
//
// The set is narrow because a session can be broad. A controller, an architect or a person's
// agent writes on many issues, and one event of such a session would otherwise state every one
// of them on every page it appears in, each with its ancestors and its tree behind it
// (rowsTouchedBy). Compute takes 133 ms over 1,008 issues and 695 ms over 4,032, which is about
// 2.3 s for each such page at INFRA's 13,103 issues.
//
// It is one read, and it costs the page no second read of the titles: a title is read off the
// events the page already holds. It scans events_session_actor_created_at_idx (migration 0026),
// the partial index on ((actor->>'id'), created_at desc) for a session actor, once for each
// session the page named, over every event that session ever wrote, because the read has no
// order and no limit; the scan is materialized once and read twice, for the issues worked and
// for the `issue.created` events, which is why the filings take no scan of their own. Each issue
// it finds is joined to the row issue_facts holds under that project and key, which is that
// table's primary key, and each filed issue to its issue_filings row by that table's primary
// key. The index bounds the read to the sessions the page named and not to the events the page
// read, which is the trap delivery/agents.go documents of the same index. legion.PhaseWorkKeys
// would answer the same pairs and reads the session half of the whole log to do it, which is why
// a page does not call it.
func sessionIssues(ctx context.Context, q Querier, sessions, titles []string) ([]touch, []sessionFiling, error) {
	named := make([]string, 0, len(titles))
	for _, title := range titles {
		if worked, worker := legion.WorkerTitle(title); worker {
			named = append(named, worked.Issue)
		}
	}
	if len(sessions) == 0 && len(named) == 0 {
		return nil, nil, nil
	}
	rows, err := q.Query(ctx, `
		with written as materialized (
			select event.id, event.type, event.issue_key, event.actor ->> 'id' as session
			from events event
			where event.actor ->> 'id' = any($1::text[])
			  and event.actor ->> 'kind' = 'session'
			  and event.issue_key is not null
		)
		select false as filed, issue.project_key, issue.key, '' as session, '' as role, '' as filer, 0::bigint as id
		from written
		join issues issue on issue.key = written.issue_key
		join issue_facts fact on fact.project_key = issue.project_key and fact.issue_key = issue.key
		union
		select false, issue.project_key, issue.key, '', '', '', 0
		from issues issue
		where issue.key = any($2::text[])
		union all
		select true, coalesce(filing.project_key, ''), written.issue_key, written.session,
		       coalesce(filing.filer_role, ''), coalesce(filing.filer_issue, ''), written.id
		from written
		left join issue_filings filing on filing.issue_key = written.issue_key
		where written.type = 'issue.created'
		order by filed, key, id
	`, sessions, named)
	if err != nil {
		return nil, nil, fmt.Errorf("read the issues %d sessions can have moved: %w", len(sessions), err)
	}
	worked := []touch{}
	var filed []sessionFiling
	var isFiling bool
	var touched touch
	var one sessionFiling
	if _, err := pgx.ForEachRow(rows, []any{&isFiling, &touched.project, &touched.issue, &one.session, &one.role, &one.filer, new(int64)}, func() error {
		if !isFiling {
			worked = append(worked, touched)
			return nil
		}
		one.project, one.issue = touched.project, touched.issue
		filed = append(filed, one)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read the issues %d sessions can have moved: %w", len(sessions), err)
	}
	return worked, filed, nil
}

// readObservationPage is the page of the daemon's observations after the cursor: up to limit
// rows above after, each naming its project, its issue, whether it moves that issue between
// trees and whether it moves the project's admission block in what a waiting reason reads.
//
// An observation of legion.FieldTree is the daemon naming the issue's tree, and that is a move
// of the same kind as a parent link moving: it takes the issue out of the tree it stood in and
// puts it in the tree the field names. It is marked as one, so the pass states the tree the
// issue left beside the tree it joined (leftTrees), and no rule of its own is needed.
//
// An observation that repeats the tree the issue already had is marked a move as well, and the
// move then states the same tree twice: the tree the stored root_key records is the tree the
// links give. That costs the two reads of leftTrees over one tree's rows and writes no wrong
// row, because every row of the set is computed before it is written. The stored root_key is
// the only record of where the issue was, and reading it is the work leftTrees does.
//
// An observation of legion.FieldAdmission under legion.AdmissionKey is the daemon's admission
// block, which belongs to the project and names no issue. Every stored row's waiting reason reads
// it (rowsOf, reasonOf): the issue's place in the daemon's line, the blockers the block names and
// so the column the issue stands in. A blocker that finishes moves an issue from the block's
// Blocked to its Waiting, and the Legion state intake writes the block alone, from the daemon's
// admission event or from a whole read, since nothing of the issue itself moved. It writes the
// block whenever any part of it moved, its cap, its active slots and its live trees among them,
// and those move no reason, so the row is compared with the project's block before it
// (blockMoved): that block's value is the newest row of the same field observed before this one,
// one probe of legion_observations_point (migration 20261103170923) in this page's own statement,
// and only for an admission row. A row that moved the block in what a reason reads is marked
// (touch.admission), and the pass states the project's open rows beside the rows its other
// changes touch (rowsToCompute), so the reasons the block moved are computed again; any other row
// of the block, and the project's settings, which no row reads, mark nothing.
//
// It needs no horizon, and that is the one way this cursor differs from the events cursor. The
// intake writes legion_observations on one runner per project, so the writes of two projects run
// at once, but every insert into the table takes the observations' lock immediately before it
// draws its ids and holds it to its commit (legion.ObservationsLockKey): no writer can hold a
// lower id open while a higher one commits, and the ids a reader pages by are commit order.
func readObservationPage(ctx context.Context, q Querier, after int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select observation.id, observation.project, observation.issue_key, observation.field = $2,
		       observation.issue_key = $3 and observation.field = $4,
		       case when observation.issue_key = $3 and observation.field = $4
		            then nullif(observation.value, 'null'::jsonb) end,
		       case when observation.issue_key = $3 and observation.field = $4 then (
		           select nullif(earlier.value, 'null'::jsonb)
		           from legion_observations earlier
		           where earlier.project = observation.project and earlier.issue_key = observation.issue_key
		             and earlier.field = observation.field and earlier.observed_at < observation.observed_at
		           order by earlier.observed_at desc
		           limit 1) end
		from legion_observations observation
		where observation.id > $1
		order by observation.id
		limit $5
	`, after, legion.FieldTree, legion.AdmissionKey, legion.FieldAdmission, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	for rows.Next() {
		var touched touch
		var block, before []byte
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved, &touched.admission, &block, &before); err != nil {
			return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
		}
		if touched.admission {
			touched.admission = blockMoved(before, block)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	page.full = len(page.touches) == limit
	return page, nil
}

// blockMoved is whether an admission row whose value is after moved what a waiting reason reads of
// the project's block from before, the value of the admission row before it (each nil for no row
// and for a `null` one): whether the two build another waiting line (legion.SameLine, which holds
// the fields legion.NewWaitingLine reads). A value is read as the facts read it (metrics'
// readReported, through legion.ParseAdmission), and a value of another shape is no block.
func blockMoved(before, after []byte) bool {
	read := func(value []byte) *legion.Admission {
		if value == nil {
			return nil
		}
		block, _ := legion.ParseAdmission(value)
		return block
	}
	return !legion.SameLine(read(before), read(after))
}

// readChangePage is the page of delivery changes after the cursor (delivery.ChangesSince, whose
// cursor never skips a change row): up to limit changes above after, of which it keeps the pull
// requests. A change's repo and ref name a row of delivery_pull_requests, whose issue_key is the
// issue whose facts moved, and a pull request no issue carries names none.
//
// A change also moves the facts of the issue the pull request left. The attribution re-resolves a
// stored pull request and rewrites its issue_key, to another issue or to none
// (delivery.AttributePullRequests), and a pull request can be deleted, and the change row of
// either names the repository, the number and the merge times alone (migration 0100), so nothing
// the page reads of the pull request names the issue that held it. That issue's stored merged_at
// is the earliest merge of its pull requests (metrics' readMergeTimes), so the change moved it
// only where the pull request's merge before the change, the change's t_old, is the stored
// merge, and no pull request of the issue merged at that moment any more (lostMerges).
//
// It also answers whether the cursor may have passed over changes it never read, by the change
// log's one rule (delivery.CursorLost), judged at checked, the page's own reading of the clock: a
// log holding rows whose newest seq stands below the cursor, its sequence restarted; or the change
// at the cursor gone from the log, or another change at its seq, where that change was younger
// than its retention (a log reset, a delete by hand) or no page read the change log to its end for
// longer than the retention before checked, so a prune may have taken changes after the cursor
// that no page read. That is a backfill (legionProjects): the rows those changes would have named
// cannot be known, so every project is computed whole. A change pruned after its retention while
// pages read the log to its end within it is no loss: the routine prune of a quiet day takes the
// change the cursor rests on, or every row, and the pass after it reads on.
//
// The page it answers stands at the last change it read, stamped checked where the page read the
// log to its end (it was shorter than limit) and carrying the cursor's ReadAt where it was full
// (delivery.ChangeCursor.Through, feed).
func readChangePage(ctx context.Context, q Querier, after delivery.ChangeCursor, checked time.Time, limit int) (feed, bool, error) {
	changes, err := delivery.ChangesSince(ctx, q, after.Seq, limit)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the delivery changes after %d: %w", after.Seq, err)
	}
	moved := after.Through(changes, limit, checked)
	page := feed{position: moved.Seq, at: moved.At, readAt: moved.ReadAt, full: len(changes) == limit}
	repos, refs := make([]string, 0, len(changes)), make([]string, 0, len(changes))
	merges := []time.Time{}
	for _, change := range changes {
		// A pull request's merge time is the one delivery fact an issue's facts read.
		if change.Kind != delivery.ChangeKindPullRequest {
			continue
		}
		repos = append(repos, change.Repo)
		refs = append(refs, change.Ref)
		if change.TOld != nil {
			merges = append(merges, *change.TOld)
		}
	}
	lost, err := delivery.CursorLost(ctx, q, after, checked)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the delivery change at %d: %w", after.Seq, err)
	}
	if len(repos) == 0 {
		return page, lost, nil
	}
	rows, err := q.Query(ctx, `
		select distinct issue.project_key, request.issue_key
		from unnest($1::text[], $2::text[]) as given(repo, ref)
		join delivery_pull_requests request on request.repo = given.repo and request.number::text = given.ref
		join issues issue on issue.key = request.issue_key
	`, repos, refs)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
	}
	defer rows.Close()
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&touched.project, &touched.issue); err != nil {
			return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
	}
	left, err := lostMerges(ctx, q, merges)
	if err != nil {
		return feed{}, false, err
	}
	page.touches = append(page.touches, left...)
	return page, lost, nil
}

// lostMerges is every stored row a change of a pull request left with a merge no pull request
// backs: the rows whose merged_at is one of merges, the merges the changed pull requests held
// before their changes (readChangePage), where no pull request of the row's issue that the
// computation reads merged at that moment any more. They are read from every project, since the
// issue a pull request left can be in a project the issue it joined is not, or it joined none;
// the pass keeps the ones of the projects it computes (passPage).
//
// No column of the change names the issue the pull request left, and its merge does: each merge
// is a probe of issue_facts_merged (migration 20261117100000), which answers the rows stored with
// that merge, and each of those a probe of delivery_pull_requests_merged_at (migration 0080) by
// the same moment. A row whose issue still holds a pull request merged then keeps its merge and
// is left out, and a row the change did not move is one the computation answers the same row for
// and upsert writes nothing of.
func lostMerges(ctx context.Context, q Querier, merges []time.Time) ([]touch, error) {
	if len(merges) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, lostMergesQuery, merges)
	if err != nil {
		return nil, fmt.Errorf("read the stored merges %d changed pull requests held: %w", len(merges), err)
	}
	defer rows.Close()
	left := []touch{}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&touched.project, &touched.issue); err != nil {
			return nil, fmt.Errorf("read the stored merges %d changed pull requests held: %w", len(merges), err)
		}
		left = append(left, touched)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored merges %d changed pull requests held: %w", len(merges), err)
	}
	return left, nil
}

// lostMergesQuery is lostMerges' read: the stored rows merged at one of $1 whose issue holds no
// pull request the computation reads (one Dispatch can still fetch) merged at that moment. It
// states issue_facts_merged's predicate, a merge that is not null, which `merged_at = any($1)`
// implies only for the values it runs with, since pgx's cached statement settles on a generic
// plan.
const lostMergesQuery = `
	select fact.project_key, fact.issue_key
	from issue_facts fact
	where fact.merged_at = any($1::timestamptz[]) and fact.merged_at is not null
	  and not exists (
		select 1 from delivery_pull_requests request
		where request.issue_key = fact.issue_key and request.merged_at = fact.merged_at
		  and request.unfetchable_at is null)`

// feedCursors is where the three feeds stand, the rows of issue_facts_cursor: the id of the last
// event and of the last observation a committed page read, and the change cursor
// (delivery.ChangeCursor), which the lost-cursor rule judges.
type feedCursors struct {
	events, observations int64
	changes              delivery.ChangeCursor
}

// readCursors is where each feed stands: the ids the last committed page read through of the
// events and of the observations, and the change cursor's seq, when the change at it was recorded
// (At, nil for a position no change holds) and when a page last read the change log to its end
// (ReadAt, the row's updated_at), which the lost-cursor rule reads (delivery.CursorLost). A feed
// with no row stands at 0, which is a feed this database has read nothing of. It answers false
// where no row exists at all, a database no pass has committed on, which is what readPages reads
// its feeds' heads for.
func readCursors(ctx context.Context, q Querier) (feedCursors, bool, error) {
	rows, err := q.Query(ctx, `select name, position, at, updated_at from issue_facts_cursor where name = any($1::text[])`, cursorNames)
	if err != nil {
		return feedCursors{}, false, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	defer rows.Close()
	var cursors feedCursors
	committed := false
	for rows.Next() {
		var name string
		var position int64
		var at *time.Time
		var stamped time.Time
		if err := rows.Scan(&name, &position, &at, &stamped); err != nil {
			return feedCursors{}, false, fmt.Errorf("read the Legion facts' cursors: %w", err)
		}
		committed = true
		switch name {
		case eventsCursor:
			cursors.events = position
		case observationsCursor:
			cursors.observations = position
		case changesCursor:
			cursors.changes = delivery.ChangeCursor{Seq: position, At: at, ReadAt: stamped}
		}
	}
	if err := rows.Err(); err != nil {
		return feedCursors{}, false, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	return cursors, committed, nil
}

// advanceCursors writes the three cursors as cursors holds them, the rows readCursors reads back:
// the events and the observations cursors at the ids this page read through, each stamped with
// now, the moment its pass ran at, and the change cursor at its seq with when the change there was
// recorded (At), stamped with its ReadAt, when a page last read the change log to its end. A
// cursor whose feed held nothing new keeps its position, and the events and the observations
// cursors take the new stamp: the pass covered the feed up to there. The change cursor's stamp
// moves only with a read of the change log to its end (readChangePage), so a pass that catches up
// a backlog of changes a full page at a time leaves it at the last read of the whole log, which
// is what the lost-cursor rule reads (delivery.CursorLost). CursorAge measures from the oldest of
// the three stamps.
//
// It runs in the transaction that wrote the rows the positions cover (writePass).
func advanceCursors(ctx context.Context, tx pgx.Tx, cursors feedCursors, now time.Time) error {
	_, err := tx.Exec(ctx, `
		insert into issue_facts_cursor (name, position, at, updated_at)
		select name, position, at, read_at
		from unnest($1::text[], $2::bigint[], $3::timestamptz[], $4::timestamptz[]) as given(name, position, at, read_at)
		on conflict (name) do update
		set position = excluded.position, at = excluded.at, updated_at = excluded.updated_at
	`, []string{eventsCursor, observationsCursor, changesCursor},
		[]int64{cursors.events, cursors.observations, cursors.changes.Seq},
		[]*time.Time{nil, nil, cursors.changes.At},
		[]time.Time{now, now, cursors.changes.ReadAt})
	if err != nil {
		return fmt.Errorf("move the Legion facts' cursors: %w", err)
	}
	return nil
}

// CursorAge answers how long ago the consumer last read its feeds, as the oldest of the three
// cursors' updated_at measured against now, and false when no cursor row exists at all, which is a
// database no pass has committed on. It is what the board's header reads to say how fresh the
// stored facts are.
//
// Every pass stamps the events and the observations cursors with the moment it ran at, whether or
// not that feed held anything new, and the change cursor with the moment one of its pages last
// read the change log to its end (advanceCursors). So the age is the time since the last pass
// ran, or since a pass last read the change log to its end, whichever is older: a pass catching
// up a backlog of changes reads as old as its last read of the whole log. It is not how long ago
// the last change arrived. A pass runs on a wake alone (Run), so in a deployment where nothing is
// written the age grows with the clock while the stored facts lose nothing: an open issue's facts
// are closed at the reader's clock (Row.At), and nothing a pass reads has moved. A caller whose
// clock stands behind the writing process's reads a negative age, as it would of any moment
// another process stamped.
func CursorAge(ctx context.Context, q Querier, now time.Time) (time.Duration, bool, error) {
	var oldest *time.Time
	if err := q.QueryRow(ctx, `select min(updated_at) from issue_facts_cursor`).Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("read the age of the Legion facts' cursors: %w", err)
	}
	if oldest == nil {
		return 0, false, nil
	}
	return now.Sub(*oldest), true, nil
}
