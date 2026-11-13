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
// and whether a feed holds more rows, which makes the pass read another page at once (more).
type pages struct {
	events       feed
	observations feed
	changes      feed
	lost         bool
	more         bool
}

// readPages is one page of each of the three feeds, read from where each cursor stands.
//
// A database holding no cursor row at all has committed no pass, because every pass writes the
// three together (advanceCursors), so every feed would be read from its start. It is read from
// its head instead (readFeedHeads): the first page of such a pass computes every project whole,
// because no project has a stored row, and a whole project is computed from the history itself
// rather than from the feeds, so the pages below that head hold nothing the backfill has not
// already answered. Reading them would recompute every project once per page - about 813 pages
// of a log the size of INFRA's - and no sweep runs until the pass ends, so the open rows would
// stand still while it replayed.
//
// A change cursor whose own row the log no longer holds is answered by computing every project
// whole, which covers every change the log ever held. The sweep page is the page that carries
// that backfill (passPage), so the sweep page is the only page that may move this cursor.
//
// On the sweep page the cursor moves to the head of the change log rather than to the end of
// the page it read. The backfill leaves nothing below the head to read, and a cursor left where
// it stood would read lost again on the next pass and compute every project whole every
// SweepEvery for as long as no change commits.
//
// Any other page leaves the cursor where it stood and answers that no feed holds more rows,
// which ends the pass. readChangePage has already moved the position it answers past the pruned
// gap, to the first change the log still holds, so a page that wrote that position back would
// take the loss away from every later pass: the next pass would read a change that exists and
// answer lost false. The pruned gap's rows would then stay stale for ever. A pull request
// merged inside the gap is read by no page, and a closed issue's merge time stands wrong in its
// stored row, because the sweep recomputes the open rows alone. Ending the pass is what makes
// the next pass's first page a sweep page, which is the page that answers the loss. The issues
// that page's own changes named are kept and computed, which is work the next pass makes again
// from the held cursor.
func readPages(ctx context.Context, q Querier, cursors map[string]int64, safe int64, limit int, sweep bool) (pages, error) {
	if len(cursors) == 0 {
		return readFeedHeads(ctx, q, safe)
	}
	read := pages{}
	var err error
	if read.events, err = readEventPage(ctx, q, cursors[eventsCursor], safe, limit); err != nil {
		return pages{}, err
	}
	if read.observations, err = readObservationPage(ctx, q, cursors[observationsCursor], limit); err != nil {
		return pages{}, err
	}
	if read.changes, read.lost, err = readChangePage(ctx, q, cursors[changesCursor], limit); err != nil {
		return pages{}, err
	}
	read.more = read.events.full || read.observations.full
	if !read.lost {
		read.more = read.more || read.changes.full
		return read, nil
	}
	if !sweep {
		read.changes.position, read.lost, read.more = cursors[changesCursor], false, false
		return read, nil
	}
	head, err := delivery.LatestChangeSeq(ctx, q)
	if err != nil {
		return pages{}, fmt.Errorf("read the head of the delivery changes: %w", err)
	}
	read.changes.position = head
	return read, nil
}

// readFeedHeads is the three feeds standing at their heads and naming no issue: where the
// cursors of a database no pass has committed on start. The events feed's head is the safe
// horizon rather than the newest id, because an event above the horizon may still be committing
// and a later pass reads it (readEventPage).
func readFeedHeads(ctx context.Context, q Querier, safe int64) (pages, error) {
	var observations int64
	if err := q.QueryRow(ctx, `select coalesce(max(id), 0) from legion_observations`).Scan(&observations); err != nil {
		return pages{}, fmt.Errorf("read the head of the Legion observations: %w", err)
	}
	changes, err := delivery.LatestChangeSeq(ctx, q)
	if err != nil {
		return pages{}, fmt.Errorf("read the head of the delivery changes: %w", err)
	}
	return pages{
		events:       feed{position: safe},
		observations: feed{position: observations},
		changes:      feed{position: changes},
	}, nil
}

// feed is what one page of one feed read: where that feed now stands, the issues it named, the
// filings it read (the events feed alone reads any), whether the page was full, and whether it
// read a change of a repository's mapping (the changes feed alone reads any, readChangePage).
type feed struct {
	position int64
	touches  []touch
	filings  []filing
	full     bool
	remapped bool
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

// touch is one issue a feed named: the project it belongs to, the issue itself, and whether the
// row that named it moves the issue between trees. Two rows of two feeds move an issue that
// way, and they are one kind of change: Dispatch's parent link, which an event's `changed`
// names (readEventPage), and the `tree` the daemon reports, which is the field of an
// observation (readObservationPage). The rule that places an issue in a tree reads the `tree`
// first and the parent link only where no daemon reported one (legion.TreeChain), so either
// row can take the issue out of one tree and put it in another.
//
// A move states two trees: the tree the issue joined, which the links now give (rowsTouchedBy),
// and the tree it left, which is read from the issue's stored root_key (leftTrees).
type touch struct {
	project string
	issue   string
	moved   bool
}

// readEventPage is the page of events after the cursor: up to limit events above after and at or
// below safe, with the issues those events name and the issues the sessions those events name
// can have moved through their titles (sessionIssues).
//
// The page stops at safe because events.id is not commit order. An event draws its id when its
// write inserts the row, and two writes that wait on nothing commit out of id order, so a cursor
// moved by an uncapped `id > cursor` read could pass an event that was still committing and
// never read it (events, the "Safe horizon" section). An event above safe is read by a later
// pass: its own commit notifies the events channel, and a pass falls every SweepEvery anyway.
//
// `changed` holds the fields an issue write moved (api's issueChangedFields), so an event naming
// `parent` is the parent link moving. That is one of the two rows that move an issue between
// trees (touch), and a move widens the set a change touches by the tree the issue left
// (leftTrees). The issues below the issue are in that set whatever the event was
// (rowsTouchedBy).
//
// An event that is on no issue at all is kept where its actor is a session, because such an
// event carries that session's title as much as an event on an issue does, and a session's
// newest title decides which issues every event of that session works (legion.PhaseWork). A
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
	worked, err := sessionIssues(ctx, q, named, slices.Sorted(maps.Keys(titles)))
	if err != nil {
		return feed{}, err
	}
	page.touches = append(page.touches, worked...)
	if page.filings, err = sessionFilings(ctx, q, named); err != nil {
		return feed{}, err
	}
	return page, nil
}

// sessionFilings is every filing the sessions made that their newest titles say is stored wrong:
// each issue one of them wrote an `issue.created` event of, beside the issue_filings row that
// holds it, where the session's newest title (read by primary key, delivery.ResolveSessionTitles,
// because that title and not the one the event carried is what metrics classifies a filing by)
// is a Legion title and no row holds the filing, or a row holds it under another project, another
// role or another filer's issue than that title names, or under any project once the title is no
// Legion title. A filing whose row already names the filer the newest title names is left out:
// its project and its filer stand, and the pass recomputes it with its filer's issue
// (filingsToCompute) whenever that issue's own inputs move.
//
// That is the filings a new filing on the page adds, and the filings a retitle moves: a session
// moves its filings only by writing under a new title, and an event that carries the title names
// the session on the page that carries it, so the page's sessions are every session that can
// have moved one.
//
// It scans events_session_actor_created_at_idx (migration 0026) once for each session, over the
// events that session wrote, as sessionIssues scans it for the same sessions, keeping the
// `issue.created` ones, and reads issue_filings by its primary key for each. A broad session, a
// controller's, has filed many issues, and the read answers them all on every page it writes on,
// while the pass computes only the ones its newest title moved.
func sessionFilings(ctx context.Context, q Querier, sessions []string) ([]filing, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select event.issue_key, event.actor ->> 'id', coalesce(filing.project_key, ''),
		       coalesce(filing.filer_role, ''), coalesce(filing.filer_issue, '')
		from events event
		left join issue_filings filing on filing.issue_key = event.issue_key
		where event.actor ->> 'id' = any($1::text[]) and event.actor ->> 'kind' = 'session'
		  and event.type = 'issue.created' and event.issue_key is not null
		order by event.issue_key, event.id
	`, sessions)
	if err != nil {
		return nil, fmt.Errorf("read the filings of %d sessions: %w", len(sessions), err)
	}
	type made struct{ issue, session, project, role, filer string }
	var filed []made
	filers := map[string]bool{}
	var one made
	if _, err := pgx.ForEachRow(rows, []any{&one.issue, &one.session, &one.project, &one.role, &one.filer}, func() error {
		filed = append(filed, one)
		filers[one.session] = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the filings of %d sessions: %w", len(sessions), err)
	}
	if len(filed) == 0 {
		return nil, nil
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
// is every session the page's events name and titles every session title they carry.
//
// It reads nothing where both inputs are empty, and it answers no issue. A page of events is
// not known to reach that state, because a title is read off an event whose actor is a session
// and such an event names that session as well. The guard asks for both inputs all the same,
// since either one alone names issues the pass must compute.
//
// A session's newest title decides which of the issues it wrote on carry its work
// (legion.PhaseWork), so one write anywhere by one session can take the work off one issue and
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
// order and no limit. Each issue it finds is joined to the row issue_facts holds under that
// project and key, which is that table's primary key. The index bounds the read to the sessions
// the page named and not to the events the page read, which is the trap delivery/agents.go
// documents of the same index. legion.PhaseWorkKeys would answer the same pairs and reads the
// session half of the whole log to do it, which is why a page does not call it.
func sessionIssues(ctx context.Context, q Querier, sessions, titles []string) ([]touch, error) {
	named := make([]string, 0, len(titles))
	for _, title := range titles {
		if worked, worker := legion.WorkerTitle(title); worker {
			named = append(named, worked.Issue)
		}
	}
	if len(sessions) == 0 && len(named) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select issue.project_key, issue.key
		from events event
		join issues issue on issue.key = event.issue_key
		join issue_facts fact on fact.project_key = issue.project_key and fact.issue_key = issue.key
		where event.actor ->> 'id' = any($1::text[])
		  and event.actor ->> 'kind' = 'session'
		union
		select issue.project_key, issue.key
		from issues issue
		where issue.key = any($2::text[])
	`, sessions, named)
	if err != nil {
		return nil, fmt.Errorf("read the issues %d sessions can have moved: %w", len(sessions), err)
	}
	defer rows.Close()
	worked := []touch{}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&touched.project, &touched.issue); err != nil {
			return nil, fmt.Errorf("read the issues %d sessions can have moved: %w", len(sessions), err)
		}
		worked = append(worked, touched)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the issues %d sessions can have moved: %w", len(sessions), err)
	}
	return worked, nil
}

// readObservationPage is the page of the daemon's observations after the cursor: up to limit
// rows above after, each naming its project, its issue and whether it moves that issue between
// trees.
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
// It needs no horizon, and that is the one way this cursor differs from the events cursor. One
// leased sampler writes legion_observations (legion.RunSampler, one pass a minute, one process
// at a time), so no second writer can hold a lower id open while a higher one commits, and the
// ids a reader pages by are commit order.
func readObservationPage(ctx context.Context, q Querier, after int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select id, project, issue_key, field = $2
		from legion_observations
		where id > $1
		order by id
		limit $3
	`, after, legion.FieldTree, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved); err != nil {
			return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	page.full = len(page.touches) == limit
	return page, nil
}

// readChangePage is the page of delivery changes after the cursor (delivery.ChangesSince, whose
// seq is allocated in commit order): up to limit changes above after, of which it keeps the pull
// requests. A change's repo and ref name a row of delivery_pull_requests, whose issue_key is the
// issue whose facts moved, and a pull request no issue carries names none.
//
// It also keeps whether the page holds a change of a repository's mapping in repo_projects, a
// `settings` change that names the repository (migration 20261113170752). A stored row's repo is
// the repository its issue resolves to, and the rule falls back to the project's default mapping
// (refs.IssueRepoTerms), so a mapping change moves the repo of rows no feed names: the pass then
// resolves every stored row's repository again (reresolveRepos).
//
// It also answers whether the cursor had fallen below the oldest change the log still holds.
// delivery_changes is pruned at delivery.ChangeRetention and oldest first, so a cursor whose own
// row is gone may have passed over rows it never read. That is a backfill (legionProjects): the
// rows those changes would have named cannot be known, so every project is computed whole.
func readChangePage(ctx context.Context, q Querier, after int64, limit int) (feed, bool, error) {
	changes, err := delivery.ChangesSince(ctx, q, after, limit)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the delivery changes after %d: %w", after, err)
	}
	page := feed{position: after, full: len(changes) == limit}
	repos, refs := make([]string, 0, len(changes)), make([]string, 0, len(changes))
	for _, change := range changes {
		page.position = change.Seq
		if change.Kind == settingsChange && change.Repo != "" {
			page.remapped = true
		}
		if change.Kind != pullRequestChange {
			continue
		}
		repos = append(repos, change.Repo)
		refs = append(refs, change.Ref)
	}
	lost, err := changesLost(ctx, q, after)
	if err != nil {
		return feed{}, false, err
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
	return page, lost, nil
}

// changesLost reports whether the change at the cursor is gone from the log, which is a cursor
// that may have lost rows it never read. A cursor of 0 has read nothing and has lost nothing.
func changesLost(ctx context.Context, q Querier, cursor int64) (bool, error) {
	if cursor == 0 {
		return false, nil
	}
	at, err := delivery.ChangeAt(ctx, q, cursor)
	if err != nil {
		return false, fmt.Errorf("read the delivery change at %d: %w", cursor, err)
	}
	return at == nil, nil
}

// readCursors is where each feed stands, by cursor name. A name with no row stands at 0, which is
// a feed this database has read nothing of, and no row at all is a database no pass has committed
// on, which is what readPages reads its feeds' heads for.
func readCursors(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `select name, position from issue_facts_cursor where name = any($1::text[])`, cursorNames)
	if err != nil {
		return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	defer rows.Close()
	cursors := make(map[string]int64, len(cursorNames))
	for rows.Next() {
		var name string
		var position int64
		if err := rows.Scan(&name, &position); err != nil {
			return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
		}
		cursors[name] = position
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	return cursors, nil
}

// advanceCursors moves the three cursors to the positions this page read, stamped with the
// moment its rows were computed as of, which is the moment CursorAge measures the stored facts
// from. A cursor whose feed held nothing new keeps its position and takes the new stamp: the
// pass covered the feed up to there, and the stamp is what says how fresh the facts are.
//
// It runs in the transaction that wrote the rows the positions cover (writePass).
func advanceCursors(ctx context.Context, tx pgx.Tx, positions map[string]int64, now time.Time) error {
	at := make([]int64, len(cursorNames))
	for index, name := range cursorNames {
		at[index] = positions[name]
	}
	_, err := tx.Exec(ctx, `
		insert into issue_facts_cursor (name, position, updated_at)
		select name, position, $3 from unnest($1::text[], $2::bigint[]) as given(name, position)
		on conflict (name) do update set position = excluded.position, updated_at = excluded.updated_at
	`, cursorNames, at, now)
	if err != nil {
		return fmt.Errorf("move the Legion facts' cursors: %w", err)
	}
	return nil
}

// CursorAge answers how long ago each cursor last moved, as the oldest of the three cursors'
// updated_at measured against now, and false when no cursor row exists at all, which is a
// database no pass has committed on. It is what the board's header reads to say how fresh the
// served facts are.
//
// Every pass stamps all three cursors with the moment it computed as of, whether or not that
// feed held anything new, so the age is how long ago the last pass ran and not how long ago the
// last change arrived. A caller whose clock stands behind the writing process's reads a negative
// age, as it would of any moment another process stamped.
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
