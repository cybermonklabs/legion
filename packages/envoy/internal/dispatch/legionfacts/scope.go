package legionfacts

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// scope.go holds the scope of one page: the projects this loop computes and which of them are
// to be read whole, the rows another build wrote that the build a project's record names
// computes again, and the rows one project's changes reach. The set a page states for a project
// is worked out here and nowhere else, and the page writes or deletes no row outside it.
//
// It stands apart from the loop (consumer.go) because the loop asks it one question - what does
// this page owe this project - and the answer is read from the issues' links, from the stored
// rows and from the record of each project's whole read rather than from the feeds.

// projectStanding is what one pass owes one project beyond what its feeds name: whether to read
// it whole (whole), whether the project's record names a newer build this process yields to
// (yielded): another build, which recorded itself after this process started and is rolling in
// while this one still serves, and the build of a record this process takes from a build no live
// process serves any more (taken, "" for none), which its whole read records itself over
// (markComputedWhole). A write of a yielded project deletes no row or filing, and stamps each it
// would have deleted for the newer build to compute again (ProjectWrite.Yielded,
// FilingsWrite.Yielded).
type projectStanding struct {
	whole   bool
	yielded bool
	taken   string
}

// legionProjects is every project this loop computes, by key, each with its standing on this
// pass, and the moment the roll row holds after this page: now where the page finds a roll, and
// rollAt, the moment the page read from it, otherwise. A project is one of them once it names a
// Legion daemon (its row of legion_settings), once Dispatch has recorded that daemon's state (its
// observations) or once it holds a stored row. The setting is enough on its own: a daemon whose
// state Dispatch has never recorded is still a daemon working the project, whose issues carry
// phase work the event log holds, and four of the six Legion evals (work time, phase time, rework
// and splitting) read the facts of that work and no observation. An event of a project none of
// the three answers for is no work of this loop: its setting or its first observation brings the
// project in, and the whole read then computes it.
//
// A project is read whole - a pass of this same loop that runs ComputeProject over the project
// rather than computeKeys over a key set - where no build has read it whole (it holds no record
// of issue_facts_computed), where another build read it whole before this process started, where
// the rows that record counted have gone, where a roll of two stamped builds has ended since the
// record was written (the roll row, below), or where lost says the change cursor may have passed
// over changes it never read, which loses the changes of every project at once. That is
// delivery.CursorLost's test: the log's newest seq stands below the cursor's, or the change the
// cursor rests on has left the log or been replaced while it was younger than the retention, or
// after the consumer last read the log to its end more than the retention ago. The first run on
// an empty table is the first of those cases and needs no second code path.
//
// The mark is a row of issue_facts_computed, which the pass that read the project whole writes
// (markComputedWhole). "Never computed" is read from that record and not from "this project
// holds no stored row", because a project whose issues carry no Legion phase work is computed
// whole and answers no row: read from the absence of a row, such a project would be computed
// whole on every pass for as long as it held no worked issue, which is the whole-project read
// this loop exists to make rare.
//
// The mark names the build that made that whole read (R27). The rule that decides which issues
// carry Legion phase work lives in the build: the six role names and the title shape of
// legion/session_title.go, which legion.ParseSessionTitle reads, and legion.PhaseWorkKeys's
// reading of handoffs. A release can therefore give an issue phase work or take it away while no
// event, observation or delivery change names that issue: the issue that gains work holds no row
// and writes no event of its own, and the issue that loses it holds a row nothing names. The
// whole read under the new rule is the one pass that reaches both: it writes the row the one now
// earns and deletes the row the other no longer does (deleteStaleProjectRows). Every other way
// an issue gains or loses its work is an input a feed carries: a retitle is an event of the
// retitled session, whose page states the issues that session wrote on (sessionIssues), and a
// handoff is an event on its issue. The daemon's own copy of the title shape
// (packages/pi-legion/src/session-title.ts) is no such release: a plugin that writes another
// title writes a session and its events, which the feeds carry as they carry every other write.
// What reaches the rule outside every write path - a hand edit of events or handoffs, a restore
// of the database - is answered by the same whole read, which deleting a project's record asks
// the next pass for.
//
// The record is judged against this process's own start, and that is what lets two builds of a
// roll stand side by side. A record that names another build and was written before this process
// started is a build this one replaces, and this process reads the project whole once and takes
// the record. A record that names another build and was written after this process started is a
// newer build rolling in while this one still serves: this process yields to it. It reads nothing
// whole for it, takes nothing from it (markComputedWhole), computes nothing of the newer build's
// rows and deletes none: a row the older rule computes none for may be one the newer rule keeps,
// and a row the older build deleted would leave no row of its own for the newer build to find
// (otherBuildsRows), so it stamps the row with its own build instead and the newer build computes
// it again and keeps or deletes it by its own rule. A record someone rewrote to another build
// string keeps its old time, so the next process reads that project whole once. The lost change
// cursor is the one exception: it is the deployment's one cursor, so the process that meets the
// loss reads every project whole, and the newer build then computes again the rows that read
// wrote, or stamped, under the older rule.
//
// A yield ends when no live process serves the build the record names (processes.Live, which
// names each process by the commit the image build stamped into it): the yielding process then
// reads the project whole once and takes the record (projectStanding.taken). The build a record
// names is not always the newer one. A process of the previous release that starts again during
// a roll finds the new release's record written before it started, reads the project whole and
// takes the record, and the new release then yields to it; without this end the new release
// would yield for as long as it runs, and the rows of the previous release's rule would stay
// after that release's last process stopped. The end is judged only for a record written
// registryLag or more before the pass's moment: a process registers only once it serves, after
// its first pass may have recorded itself, and Register waits up to LiveWindow and one
// HeartbeatInterval for its admission, so a younger record may name a process that runs and is
// not yet in the registry. A build no release stamped registers no commit, so the registry
// cannot say whether the process of such a record still runs: a record whose build is no stamped
// commit (releaseCommit) keeps the yield, and two such processes never trade whole reads.
//
// That end is one case of a wider rule: once no live process serves another stamped build, the
// running build reads each project whole once and takes the record, and a record of its own
// build owes it as well. This build's inserts into legion_observations take the observations'
// lock before they draw their ids (legion.ObservationsLockKey), and the previous release's intake
// takes none. So while a process of that release serves, one of its inserts can commit an id
// below one the observations cursor has passed (readObservationPage), and that issue's row misses
// the observation until the issue's next input. A roll is two stamped builds serving at once: the
// stamped commits of the live processes, with this process's own build where it is stamped,
// number two or more. The registry keeps no row of a process that has stopped, so each page that
// finds a roll writes its moment into the roll row of issue_facts_cursor (rollCursor), in the
// transaction that writes its records (advanceCursors). Once a page finds no roll, the process
// reads whole each project whose record names its own build and was written at or before the
// roll row's moment, and the whole read moves the record's moment to the pass's
// (markComputedWhole). Every process of the build reads the one row, so a process that started
// after the other build's last process stopped owes the read as much as one that served beside
// it, and a process that finds the record written after the roll row's moment reads nothing
// whole. Both moments are the clocks of the processes that wrote them, and passes run one at a
// time under the lease: only a clock that stands behind the roll row's writer's by more than the
// time between their passes makes a later whole read compare at or before the moment, and that
// costs one whole read on each pass of that process until its clock passes it, which writes no
// wrong row. A build no release stamped registers no commit, so no process counts it: two such
// processes, or one beside a single stamped build, are no roll, and the stamped build never reads
// whole for a moment a process it cannot see wrote.
//
// The registry is read on every page, one read of server_processes, which holds one row per
// serving process. No record can stand in for it: a record names only the build that last read
// its project whole, and a build that serves beside this one need not be that build.
//
// The build the pass compares is the one the process runs under. The server gives it to the
// loop (Deps.Build), and the pass carries it both to this read and to the record it writes
// (markComputedWhole). In the image it is the commit the image build stamped into the binary:
// 40 hex characters, which packages/envoy/docker/Dockerfile refuses to leave short. Two releases
// are two commits, so a release compares different from the record the release before it left,
// and reads each project whole once under its own commit.
//
// A build no release stamped answers a string of that one process: cmd/dispatch takes what
// internal/buildversion answers - `devel` for a build made outside a checkout, which is every
// image a local compose file builds - and adds a suffix drawn when the process starts
// (processBuild). A developer's `go run`, a `go build` with no ldflags and a test binary
// therefore each answer a string no other process answers, and each start reads every project
// whole once.
//
// That costs a developer one whole read per project per restart, which is 286 ms over the 252
// issues measured below. It buys the thing the comparison is for: the rule that decides which
// issues carry Legion phase work lives in the binary, so a developer who edits
// legion/session_title.go or legion.PhaseWorkKeys and restarts reads every project whole under
// the edited rule. Were the string the same across restarts, that developer's next process
// would find its own record, read nothing whole, and read facts the rule before the edit wrote,
// with nothing on screen to say so. 286 ms a restart is the cheaper of the two mistakes, so the
// suffix errs on reading. A stamped build is the other side of it: its string is the image's
// commit, which every process of that release shares, so the second of them to start reads
// nothing whole and writes no row.
//
// A settled deployment pays one whole project read per build per project. Over the pod's test
// Postgres a whole read takes 286 ms over 252 issues, 554 ms over 1,008, 844 ms over 2,016,
// 1.17 s over 4,032 and 2.59 s over 13,104 issues and 10,920 rows, which is 1.72 s to compute and
// 868 ms to write.
//
// The count the mark carries is what still answers for a project whose stored rows went
// missing under the consumer, an emptied table among them: a project whose last whole read
// answered rows and which now holds none has lost them to something this loop did not do, and
// no narrow pass can name rows that are not there. It costs one whole read, because that read
// writes the count it answered: a project that truly holds no row any more is marked with none
// and is left alone from then on.
func legionProjects(ctx context.Context, q Querier, self process, now, rollAt time.Time, lost bool) (map[string]projectStanding, time.Time, error) {
	live, err := processes.Live(ctx, q)
	if err != nil {
		return nil, time.Time{}, err
	}
	serving := map[string]bool{}
	stamped := map[string]bool{}
	if releaseCommit.MatchString(self.build) {
		stamped[self.build] = true
	}
	for _, one := range live {
		serving[one.Commit] = true
		if releaseCommit.MatchString(one.Commit) {
			stamped[one.Commit] = true
		}
	}
	roll := len(stamped) > 1
	rows, err := q.Query(ctx, `
		select project.key,
		       mark.project_key is null,
		       mark.computed_build is not distinct from $1,
		       coalesce(mark.computed_at < $2, false),
		       coalesce(mark.computed_rows > 0, false)
		           and not exists (select 1 from issue_facts where project_key = project.key),
		       coalesce(mark.computed_build, ''),
		       coalesce(mark.computed_at <= $3, false),
		       coalesce(mark.computed_at <= $4, false)
		from projects project
		left join issue_facts_computed mark on mark.project_key = project.key
		where exists (select 1 from legion_settings where project = project.key)
		   or exists (select 1 from legion_observations where project = project.key)
		   or exists (select 1 from issue_facts where project_key = project.key)
	`, self.build, self.started, now.Add(-registryLag()), rollAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	defer rows.Close()
	projects := map[string]projectStanding{}
	for rows.Next() {
		var key, recorded string
		var unrecorded, ours, before, emptied, aged, rolledOver bool
		if err := rows.Scan(&key, &unrecorded, &ours, &before, &emptied, &recorded, &aged, &rolledOver); err != nil {
			return nil, time.Time{}, fmt.Errorf("read the projects Legion works: %w", err)
		}
		recordedOther := !unrecorded && !ours
		// rolledOver is a record written at or before the roll row's moment, the last page that
		// found a roll: once no roll is found, a record of this build owes the whole read that
		// takes it again.
		standing := projectStanding{
			whole:   lost || unrecorded || (recordedOther && before) || (ours && (emptied || (rolledOver && !roll))),
			yielded: recordedOther && !before,
		}
		// The yield ends where the registry can judge the record's build, a stamped commit
		// recorded registryLag or more before now, and no live process serves it.
		if standing.yielded && aged && releaseCommit.MatchString(recorded) && !serving[recorded] {
			standing = projectStanding{whole: true, taken: recorded}
		}
		projects[key] = standing
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	if roll {
		rollAt = now
	}
	return projects, rollAt, nil
}

// releaseCommit is the build a stamped release runs under: the commit the image build stamped
// into the binary, 40 hex characters (packages/envoy/docker/Dockerfile), which the registry of
// live processes records for each process (processes.Instance.Commit). A build no release stamped
// answers a string of its process that is no commit (cmd/dispatch's processBuild) and registers
// none.
var releaseCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// registryLag is how long after a process's first pass the registry of live processes may still
// not name it: Register writes a process's row only once it serves, and waits up to LiveWindow
// and one HeartbeatInterval for its admission (processes.Register). A function rather than a
// constant because the two are processes' variables.
func registryLag() time.Duration {
	return processes.LiveWindow + processes.HeartbeatInterval
}

// otherBuildsRows is every stored row whose writer was another build than build, of a project
// whose record names build, by project: what the build a project's record names computes again
// on the first page of each pass (passPage), as it computes any key a feed names.
//
// It is how the running build learns that a roll has ended, with no timer. While two builds run,
// the older one can still write rows by its own rule, and every row carries the build that wrote
// it (written_build, migration 20261117100000). A row the older build's rule computes none for is
// not deleted but stamped with the older build (ProjectWrite.Yielded), so it is found here too.
// Each of the older build's writes and stamps logs an issue_facts change, whose drain notifies
// the delivery change channel and so wakes the newer process (Deps.Deliveries), and that pass
// computes the row again under the newer rule. Once the older process has stopped, the last row
// it wrote or stamped has been computed again by the pass it woke, and nothing is left that
// another build wrote. A pass of a build the record does not name reads none of this, so two
// builds never write each other's rows back and forth. The cursors are the two processes' shared
// cursors (one lease, one cursor table), which is why this reads the rows' writer rather than
// the change feed: a change row the older process's pass read is one the newer process never
// reads.
//
// It reads issue_facts_written_build, (project_key, written_build), as two ranges of each named
// project's rows: those whose writer sorts before build and those that sort after it. A settled
// deployment holds neither, so the read is two index probes a project; the rows of a roll are
// the rows the ranges hold.
func otherBuildsRows(ctx context.Context, q Querier, build string) (map[string][]string, error) {
	return readOtherBuilds(ctx, q, "stored rows", otherBuildsRowsQuery, build)
}

// otherBuildsRowsQuery is otherBuildsRows' read: the projects whose record names $1, and of each
// the rows written by a build that sorts before $1 or after it, each a range of
// issue_facts_written_build.
const otherBuildsRowsQuery = `
	select fact.project_key, fact.issue_key
	from issue_facts_computed mark
	join issue_facts fact on fact.project_key = mark.project_key
	where mark.computed_build = $1
	  and (fact.written_build < $1 or fact.written_build > $1)
	order by fact.project_key, fact.issue_key`

// otherBuildsFilings is otherBuildsRows for the filings: every stored filing whose writer was
// another build than build, of a project whose record names build, by project and filed issue,
// which the build a project's record names computes again on the same page as the rows
// (passPage), as it computes any filing a feed names. The rule that decides whose filing an
// issue is lives in the build as the phase-work rule does, so a filing the older build of a roll
// wrote or stamped (FilingsWrite.Yielded) is one the newer build decides again, and once the
// older process has stopped no filing is left that it wrote. It reads issue_filings_written_build
// (migration 20261117100007) as otherBuildsRows reads the rows' index.
func otherBuildsFilings(ctx context.Context, q Querier, build string) (map[string][]string, error) {
	return readOtherBuilds(ctx, q, "stored filings", otherBuildsFilingsQuery, build)
}

// otherBuildsFilingsQuery is otherBuildsFilings' read, as otherBuildsRowsQuery is the rows'.
const otherBuildsFilingsQuery = `
	select filing.project_key, filing.issue_key
	from issue_facts_computed mark
	join issue_filings filing on filing.project_key = mark.project_key
	where mark.computed_build = $1
	  and (filing.written_build < $1 or filing.written_build > $1)
	order by filing.project_key, filing.issue_key`

// readOtherBuilds runs one of the two reads of what another build wrote, query under build, and
// answers its keys by project, in the order the read gave them. what names the read in an error.
func readOtherBuilds(ctx context.Context, q Querier, what, query, build string) (map[string][]string, error) {
	rows, err := q.Query(ctx, query, build)
	if err != nil {
		return nil, fmt.Errorf("read the %s another build wrote: %w", what, err)
	}
	defer rows.Close()
	other := map[string][]string{}
	for rows.Next() {
		var project, issue string
		if err := rows.Scan(&project, &issue); err != nil {
			return nil, fmt.Errorf("read the %s another build wrote: %w", what, err)
		}
		other[project] = append(other[project], issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the %s another build wrote: %w", what, err)
	}
	return other, nil
}

// rowsToCompute is the whole set one pass computes for a project, sorted: the rows the changes
// touch through the issues' links now (rowsTouchedBy), the rows of the tree a moved issue left
// (leftTrees), the issues below an issue whose repository moved by the parent link
// (repoDescendants) and, where admission says the page read a move of the project's admission
// block, the project's open rows (openRows). It is the set the pass states before it writes, and
// it writes no row outside it. keys is the issues the feeds named and the rows another build
// wrote (otherBuildsRows).
//
// moved names the issues a row of a feed took out of one tree and into another (touch), and
// repos the issues whose repository an event moved (touch.repo). Only the tree a moved issue left
// is read from the stored rows: the links as they stand now answer for every other issue a change
// can move, the issues below a key (rowsTouchedBy) and below a repository move (repoDescendants)
// among them.
//
// The issues below a repository move and the open rows are added as they are, with no climb and
// no descent. A repository moves a row's repo and no other fact (rowsOf, ReadRepos). The block
// moves a row's waiting reason and no other fact (rowsOf), and each row's reason is computed
// from the block, its own observations and the tree above it, which the population of its key
// reads whole (metrics.ReadIssues).
func rowsToCompute(ctx context.Context, q Querier, project string, keys []string, moved, repos map[string]bool, admission bool, above map[string]string) ([]string, error) {
	stored, err := leftTrees(ctx, q, project, keys, moved, above)
	if err != nil {
		return nil, err
	}
	inherited, err := repoDescendants(ctx, q, project, keys, repos)
	if err != nil {
		return nil, err
	}
	stored = append(stored, inherited...)
	if admission {
		open, err := openRows(ctx, q, project)
		if err != nil {
			return nil, err
		}
		stored = append(stored, open...)
	}
	return withRows(rowsTouchedBy(keys, above), stored), nil
}

// openRows is the issue of every open stored row of the project: the rows a move of the
// project's admission block can move (readObservationPage). A row's waiting reason places its
// issue in the daemon's line by the block (reasonOf), and the line orders work the daemon has not
// finished; the row of an issue that closed was computed by its close, an event of its own. Each
// open row is computed again, and upsert writes only those whose reason moved: an open row is
// closed at the newest input its facts read (metrics.ProjectFactsClosed), so a row the block did
// not move computes to the row the table holds and is written by nothing.
//
// The statement names the project and closed_at IS NULL, a range of issue_facts_closed
// (project_key, closed_at desc, migration 20261117100000), the index the board reads the same
// rows through (openWhere). It runs with the planner's settings as they stand, unlike the board's
// read (readByIndex), so where the open rows are a large share of the table the planner may scan
// the table instead: ReadOpen records that scan at 6.3 to 10.0 ms and the range at 4.5 to 5.9 ms
// at INFRA's size, and the pass then computes every one of those rows.
func openRows(ctx context.Context, q Querier, project string) ([]string, error) {
	rows, err := q.Query(ctx, openRowsQuery, project)
	if err != nil {
		return nil, fmt.Errorf("read the open rows of %s: %w", project, err)
	}
	open, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the open rows of %s: %w", project, err)
	}
	return open, nil
}

// openRowsQuery is openRows' read: the issue of each stored row of $1 with no close.
const openRowsQuery = `select issue_key from issue_facts where project_key = $1 and closed_at is null`

// withRows is touched, sorted, with the keys of left that it does not hold yet, in one sorted set.
func withRows(touched, left []string) []string {
	if len(left) == 0 {
		return touched
	}
	whole := make(map[string]bool, len(touched)+len(left))
	for _, key := range touched {
		whole[key] = true
	}
	for _, key := range left {
		whole[key] = true
	}
	return slices.Sorted(maps.Keys(whole))
}

// filingsToCompute is the set of filed issues one pass computes the project's filings over,
// sorted: the filings the page's sessions moved that the project owes, the ones it joined and the
// ones it left (filed, from feed.filings, passPage), and the project's stored filings whose filed
// issue or filer's issue is in keys, the set the pass states for the rows (rowsToCompute). It is
// the set the pass states for the filings, and it writes or deletes no filing outside it
// (deleteStaleFilings).
//
// A stored filing moves with the inputs of the rows that set already holds. The filed issue's
// tree root moves with a move of that issue or of an issue above it, and rowsTouchedBy puts the
// issues below a moved issue in the set. The class and the filer's phase move with the filer's
// own events and observations and with its tree, which name the filer's issue. A filing session
// that writes under a new title moves its filings itself, which the page reads of that session
// (sessionFilings). The stored filing is read back from the table because the event that made it
// is long past the cursor, as leftTrees reads a moved issue's old tree from the stored rows.
func filingsToCompute(ctx context.Context, q Querier, project string, keys []string, filed map[string]bool) ([]string, error) {
	set := make(map[string]bool, len(filed))
	maps.Copy(set, filed)
	if len(keys) > 0 {
		rows, err := q.Query(ctx, storedFilingsOfQuery, project, keys)
		if err != nil {
			return nil, fmt.Errorf("read the stored filings of %d issues of %s: %w", len(keys), project, err)
		}
		var key string
		if _, err := pgx.ForEachRow(rows, []any{&key}, func() error {
			set[key] = true
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the stored filings of %d issues of %s: %w", len(keys), project, err)
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// storedFilingsOfQuery is filingsToCompute's read of the stored filings of a project whose filed
// issue or filer's issue is among $2: the filed issue through the primary key, and the filer's
// issue through issue_filings_filer (migration 20261117100007), whose predicate (a filer issue
// that is not the empty string) the statement carries, since `filer_issue = any($2)` implies it
// only for the values it runs with and pgx's cached statement settles on a generic plan.
const storedFilingsOfQuery = `
	select issue_key from issue_filings
	where project_key = $1 and (issue_key = any($2::text[]) or (filer_issue = any($2::text[]) and filer_issue <> ''))
`

// leftTrees is every stored row of the tree a moved issue left, sorted. An issue a move took
// out of one tree and into another takes its own hours with it, and the hours of every issue
// below it, so the old tree's root goes on counting the time it waited while those issues worked
// until its row is computed again. No pass computes a row its inputs did not name, so a pass
// that left the old tree out would leave that row wrong for good.
//
// Two rows move an issue that way and both are marked the same (touch): an event whose
// `changed` names `parent`, and an observation of legion.FieldTree. This reads the tree left
// for either of them, because the tree rule reads the daemon's `tree` before the parent link
// (legion.TreeChain) and an issue leaves a tree whichever of the two moved it.
//
// The old tree is the tree as the last computation recorded it: the stored root_key of the moved
// issue and of every issue below it (issuesBelow, the descent rowsTouchedBy states for the moved
// issues), and every issue the table holds under those roots. The stored rows are the one record
// of it, because the links the pass reads are the links after the move. The moved issue's own row
// is not enough: an issue the daemon reports and no Legion role worked holds no row
// (computeKeys), and the tree rule climbs through it all the same, so the rows below it can be
// all that records the tree it left. The issues below a moved issue are read from the links after
// the move, and they are the issues the move carried, because a move changes what stands above
// the moved issue and not what stands below it.
//
// It is two queries, and only where an issue moved. A move is uncommon: a parent link is moved
// by hand, and the Legion state intake writes a `tree` observation only where the tree the daemon
// reports differs from the newest one recorded (legion's recordObservations and recordEvents), so
// a daemon that goes on reporting one tree writes one row.
//
// The stored roots are taken whether or not the links now give others. There is one root rule,
// legion.TreeChain, which is what wrote each root_key, and a walk of the raw parent links is a
// second rule that can answer differently: it reads neither the daemon's tree nor the stop at a
// parent with no Legion work. Taking the stored roots always states a superset of the rows a
// comparison of the two would state, which the scope rule allows: the set is stated before
// anything is written, and a row inside it is written or deleted only because the computation
// of that issue said so.
func leftTrees(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool, above map[string]string) ([]string, error) {
	wasMoved := make([]string, 0, len(moved))
	for _, key := range keys {
		if moved[key] {
			wasMoved = append(wasMoved, key)
		}
	}
	if len(wasMoved) == 0 {
		return nil, nil
	}
	carried := slices.Sorted(maps.Keys(issuesBelow(wasMoved, above)))
	stored, err := q.Query(ctx, `
		select distinct root_key from issue_facts
		where project_key = $1 and issue_key = any($2::text[]) and root_key <> ''
	`, project, carried)
	if err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s and the issues below them: %w", len(wasMoved), project, err)
	}
	roots, err := pgx.CollectRows(stored, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s and the issues below them: %w", len(wasMoved), project, err)
	}
	if len(roots) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select issue_key from issue_facts
		where project_key = $1 and (root_key = any($2::text[]) or issue_key = any($2::text[]))
	`, project, roots)
	if err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	slices.Sort(left)
	return left, nil
}

// rowsTouchedBy is every issue whose stored row a change to one of keys can move, sorted: the
// issues themselves, the issues above them and the issues below them. above is the issue above
// each issue of the project (metrics.TreeLinks).
//
// An issue above a key is touched because an issue's hours carry the time it waited while a
// child worked, so a child's own change moves every row above it.
//
// An issue below a key is touched because an input that names an issue can move where every
// issue under that issue stands. That is one rule for every input this pass reads, and it is
// written here, where the set is stated, rather than at the feed that read the input:
//
//  1. A parent link an event's `changed` names and a `tree` an observation names take the
//     issue itself into another tree (touch), and the chain above every issue below it moves
//     with it.
//  2. An input that names one issue and moves nothing moves the issues below it just as far.
//     The placement rule stops a chain below a parent that is not a Legion issue
//     (legion.TreeChain), so an issue below stands under its parent only while that parent is
//     a Legion issue, and which issues are Legion issues is moved by an event of a worker
//     session, by a handoff, by the daemon's first report of an issue and by a release or a
//     retitle that takes an issue's phase work away (legion.TreeNode's Legion field holds the
//     rule). Each of those names the parent alone.
//
// A repository an event's `changed` names moves the issues below the issue too, by a walk of its
// own and not by this rule. A row's repo is the repository its issue resolves to up the parent
// link alone (ReadRepos, refs.IssueRepoTerms), and the links this set descends take the daemon's
// `tree` before that link, so an issue the daemon reports under a root above its parent stands
// below the root here and not below the parent. Those issues are read by the parent link
// (repoDescendants), and rowsToCompute states them beside this set.
//
// A narrower descent would start from the keys whose Legion standing moved, and the stored
// rows cannot answer which keys those are. A row is held for an issue that carries Legion
// phase work (computeKeys), and the parent whose standing moves commonly carries none and holds
// no row at all, so there is nothing to read this pass's standing against. A second record of the
// standing is what that descent would need, and the wider set needs none: the set is stated
// before anything is written, and a row inside it is written or deleted only because the
// computation of that issue said so. A row below whose facts the change did not move is closed at
// the same input as before (metrics.ProjectFactsClosed), so it is written by nothing.
//
// It climbs and descends the links metrics.TreeLinks gives, which are the one tree step
// (legion.TreeAbove) under the scope rule the metrics hold, because the computation this set
// is stated for reads its own scope by that same rule. The issues below an issue are therefore
// the issues the step puts below it: the one whose `tree` names it as well as the one whose
// parent link does.
//
// It reads the links as they stand now, so it cannot name the tree a moved issue left: that one
// is read from the stored roots of the moved issue and of the issues this descent puts below it
// (leftTrees, issuesBelow), and rowsToCompute states the two together.
//
// Each walk keeps the keys it has already visited, so a chain that loops stops rather than
// running for ever, which is the stop every tree walk holds (legion.TreeChain). What the
// descent costs a pass is measured over the rig's histories and recorded with the pass's other
// costs (PassRetry).
func rowsTouchedBy(keys []string, above map[string]string) []string {
	touched := issuesBelow(keys, above)
	climbed := map[string]bool{}
	for _, key := range keys {
		clear(climbed)
		climbed[key] = true
		for up := above[key]; up != "" && !climbed[up]; up = above[up] {
			climbed[up] = true
			touched[up] = true
		}
	}
	return slices.Sorted(maps.Keys(touched))
}

// issuesBelow is keys and every issue below them by the links above holds (metrics.TreeLinks):
// the descent rowsTouchedBy states, and the issues a move carried, whose stored roots leftTrees
// reads. The walk keeps the keys it has already visited, so a chain that loops stops.
func issuesBelow(keys []string, above map[string]string) map[string]bool {
	children := map[string][]string{}
	for key, up := range above {
		if up != "" {
			children[up] = append(children[up], key)
		}
	}
	below := make(map[string]bool, len(keys))
	descending := slices.Clone(keys)
	for len(descending) > 0 {
		key := descending[len(descending)-1]
		descending = descending[:len(descending)-1]
		if below[key] {
			continue
		}
		below[key] = true
		descending = append(descending, children[key]...)
	}
	return below
}

// repoDescendants is every issue of the project below an issue of keys whose repository moved
// (repos, touch.repo), by the parent link alone, sorted. A row's repo is the repository its
// issue resolves to by the server's one rule (ReadRepos, refs.IssueRepoTerms): the nearest one
// its parent chain names inside its own project. A move of an issue's own repository, or of the
// one it resolves to, therefore moves the stored repo of every issue below it by that chain that
// names none of its own, while the event names that issue alone. The links rowsTouchedBy descends
// need not hold those issues: they take the daemon's `tree` before the parent link, so a child
// the daemon reports under a root above its parent stands below that root there.
//
// It walks the parent link inside the project, as the repository rule climbs it: an issue of
// another project takes no repository from a parent here, and passes on none to an issue below
// it. It answers every issue below, an issue that names a repository of its own and the issues
// below that one among them, whose repo the move left where it was. That is a superset, which the
// scope rule allows, and a walk that stopped at an issue's own repository would be a second copy
// of the repository rule; a row whose repo did not move computes to the row the table holds and
// is written by nothing (upsert).
//
// It is one recursive read of issues through issues_parent_key (migration 0032), and only where
// an event named `repo`. The walk stops at refs.IssueDepthCap links, the bound of every recursive
// issue walk, so a parent chain that loops ends.
func repoDescendants(ctx context.Context, q Querier, project string, keys []string, repos map[string]bool) ([]string, error) {
	moved := make([]string, 0, len(repos))
	for _, key := range keys {
		if repos[key] {
			moved = append(moved, key)
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, repoDescendantsQuery, project, moved, refs.IssueDepthCap)
	if err != nil {
		return nil, fmt.Errorf("read the issues below %d issues of %s whose repository moved: %w", len(moved), project, err)
	}
	below, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the issues below %d issues of %s whose repository moved: %w", len(moved), project, err)
	}
	return below, nil
}

// repoDescendantsQuery is repoDescendants' read: every issue of project $1 below one of the
// issues $2 by the parent link, to $3 links down.
const repoDescendantsQuery = `
	with recursive below as (
		select child.key, 1 as depth
		from issues child
		where child.project_key = $1 and child.parent_key = any($2::text[])
		union
		select child.key, below.depth + 1
		from issues child join below on child.parent_key = below.key
		where child.project_key = $1 and below.depth < $3
	)
	select distinct key from below order by key`
