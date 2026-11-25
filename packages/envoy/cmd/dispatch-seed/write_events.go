package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// eventTypes are the event types the filler events carry, in the proportions a type that is not
// read in bulk needs: none of the measured statements filters on the type, so what matters is
// that the column holds the types a reader knows rather than one made-up value. No `ask.*` type
// is among them: a read of an event of one of those decodes its payload as an ask and fails the
// whole request when it names none (api.attachAskEventFields), so the only ask events the seed
// writes are the opening events writeAsks writes with their asks and the answers and declines
// writeDeliveries writes, each carrying its own ask.
var eventTypes = []string{
	"issue.created", "issue.updated", "comment.created", "message.created",
	"artifact.version.created",
}

// fillerEventType is the type of the filler event at position in the event plan. The types take
// turns, so the events of each owner hold every type in about equal numbers.
func fillerEventType(position int) string {
	return eventTypes[position%len(eventTypes)]
}

// eventColumns are the columns of `events` the seed writes, shared by the three writers of that
// table: the filler events here, the opening event each ask is written with, and the deliveries
// (writeDeliveries), which write published_destinations beside them.
var eventColumns = []string{
	"id", "issue_key", "artifact_id", "project_key", "seq", "type", "actor",
	"payload", "notify", "published_at", "created_at",
}

// eventOwnerShares is how the events are split over the three owners an event can have. An event
// has at most one (events_one_owner), and `last_seq` on that owner's row must equal the highest
// seq it carries or the next write to it fails the unique (owner, seq) - and those next writes
// are the five write routes the measurement times, so a counter left behind would fail the
// first write of a measured run rather than time it.
const (
	issueEventShare    = 80
	artifactEventShare = 16
	projectEventShare  = 4
)

// eventRow is one planned filler event: its owner, its sequence under that owner and its time.
type eventRow struct {
	owner     string // "issue", "artifact" or "project"
	index     int
	seq       int
	createdAt time.Time
}

// eventBudget is one owner kind's share of the filler events: how many of them that kind takes
// and how many owners of it share them. planEvents lays the rows out over the budgets and
// askEventSeqs numbers each ask's opening event past the rows its owner was left, so both read
// one split of the log.
type eventBudget struct {
	owner string
	count int
	over  int
}

func (r *seedRun) eventBudgets() []eventBudget {
	total := r.sizes.Events
	issues := total * issueEventShare / 100
	artifacts := total * artifactEventShare / 100
	return []eventBudget{
		{"issue", issues, len(r.plan.issues)},
		{"artifact", artifacts, len(r.plan.documents)},
		{"project", total - issues - artifacts, len(r.plan.projects)},
	}
}

// planEvents lays the filler events out over the owners: each owner takes its share of its
// kind's budget, numbered from one, so the highest seq of an owner is the count of its filler
// events and an ask's opening event continues from there.
func (r *seedRun) planEvents() []eventRow {
	rows := make([]eventRow, 0, r.sizes.Events)
	for _, budget := range r.eventBudgets() {
		for index := range budget.over {
			count := share(index, budget.over, budget.count)
			for seq := 1; seq <= count; seq++ {
				rows = append(rows, eventRow{owner: budget.owner, index: index, seq: seq})
			}
		}
	}
	for position := range rows {
		rows[position].createdAt = timeAt(position, len(rows))
	}
	return rows
}

// askEventSeqs is the seq each ask's opening event takes under the ask's owner, by ask index.
// The filler events hold 1..count on every owner, so an ask's event continues from count + 1 and
// an owner carrying several asks numbers theirs in ask order; settleEventCounters then brings
// `last_seq` to the highest of them, since it runs after both writers.
func (r *seedRun) askEventSeqs() []int {
	var issueNext, artifactNext []int
	for _, budget := range r.eventBudgets() {
		if budget.owner != "issue" && budget.owner != "artifact" {
			continue
		}
		next := make([]int, budget.over)
		for index := range next {
			next[index] = share(index, budget.over, budget.count) + 1
		}
		if budget.owner == "issue" {
			issueNext = next
		} else {
			artifactNext = next
		}
	}
	seqs := make([]int, len(r.plan.asks))
	for index, ask := range r.plan.asks {
		// An ask is owned by an issue or by an unlinked document (asks_one_owner) and its event
		// is owned by that same row, as api/asks.go appends it on the ask's owner. A block ask's
		// block_artifact_id names the document its block stands in, which does not own it.
		next, at := issueNext, ask.Issue
		if ask.Document >= 0 {
			next, at = artifactNext, ask.Document
		}
		seqs[index] = next[at]
		next[at]++
	}
	return seqs
}

// askEventIDBase is the id an ask's opening event is numbered from, by ask index. The opening
// events are written with their asks, before the filler events, and still take the higher ids:
// a real log numbers one owner's events in the order their seqs run (events/broker.go's Append
// draws both under the owner's row lock), so a reader paging an owner by id must not meet its
// seqs out of order. writeEvents holds the filler events to exactly this count.
func (r *seedRun) askEventIDBase() int64 {
	return int64(r.sizes.Events)
}

// askEventIDEnd is the id of the last ask's opening event, the highest id the seed writes before
// the deliveries, which number their own events past it (writeDeliveries).
func (r *seedRun) askEventIDEnd() int64 {
	return r.askEventIDBase() + int64(len(r.plan.asks))
}

// askOpenedPayload is the payload of an ask's opening event, built by model.NewAskEventPayload,
// which is how a producer builds an ask payload (api/asks.go's create route builds the same
// value through events.AskEventPayload), so a reader finds on a seeded event the fields it finds
// on a written one. The ask is the ask as the write that opened it saw it - open, with no answer
// and no resolution, whatever state its row ends in - because that is what an opening event
// records. Its reference changes are empty, as they are on every transition that writes no text:
// they tell a live subscriber which backlink counts to refresh, a seeded run has no subscriber,
// and the edges themselves are the `refs` rows writeAsks writes.
func askOpenedPayload(ask model.Ask) (string, error) {
	encoded, err := json.Marshal(model.NewAskEventPayload(ask, model.ReferenceChanges{}))
	if err != nil {
		return "", fmt.Errorf("encode the opening event of ask %s: %w", ask.ID, err)
	}
	return string(encoded), nil
}

// statusMoveOneIn is how many filler `issue.updated` events there are for each one that records
// a status move. The status is one of the fields an issue update can move (api's
// issueChangedFields names fourteen: the title, the rank, the priority, the assignee, the labels,
// the parent, the blockers and the rest). Legion's workflow moves an issue's status at each step
// of its life: out of triage, into the queue or a parked status, and through each phase to
// retro. So the status is a common field of an update, but the other fields together are moved
// more often. One in three is the seed's own estimate of that share, not a count from
// production. Each issue holds about ten filler `issue.updated` events at every scale (fifty
// filler events, one type in five), so one in three gives an issue about three moves, which is
// the walk from triage of an issue in the middle of its lifecycle.
const statusMoveOneIn = 3

// statusMoveEvent is the SQL predicate, on the event aliased e, that holds for a status move: an
// `issue.updated` event whose payload names `status` in `changed`, which are the parked read's own
// conditions on the event beside its status (api's backlogMoveLateral). The report counts the moves
// by it, and the seed's test reads them by it.
const statusMoveEvent = `e.type = 'issue.updated' and e.payload->'changed' @> '"status"'::jsonb`

// planStatusMoves chooses the filler `issue.updated` events that record a status move, and the
// status each one moves its issue into, by the position of the event in events. Each such event
// is a move with odds of one in statusMoveOneIn, drawn from a random source of its own, so the
// draws of the other steps do not move. The moves of an issue walk the lifecycle
// (model.IssueStatuses) from triage, where every issue is created (model.IssueStatusTriage).
// Each move goes to a status the issue does not stand in, as a status write must for its event
// to name `status` in `changed` (api's issueChangedFields). The newest move, by seq, goes into
// the status that the issue's row holds now. So the newest move of an issue in `backlog` or
// `icebox` is into that status, which is the event the parked read finds (api's
// backlogMoveLateral), and the moves before it go into and out of the other statuses, the two
// parked statuses among them.
//
// An issue that stands outside triage was moved there, so it takes at least one move: where the
// odds chose none, one of its `issue.updated` events is drawn to be the move. An issue that
// stands in triage takes no move or at least two, since one move takes it out of triage. No move
// goes into `done`: a move into `done` is an `issue.closed` event (api's patch route), which the
// filler log does not carry. So an issue that stands in `done` takes no move.
func (r *seedRun) planStatusMoves(events []eventRow) map[int]string {
	random := rand.New(rand.NewPCG(randomSeed+5, randomSeed^0xbf58476d1ce4e5b9))
	moves := map[int]string{}
	var updates, chosen []int
	candidates := make([]string, 0, len(model.IssueStatuses))
	// planEvents lays out the events of the issues first, each issue's events together.
	for position := 0; position < len(events) && events[position].owner == "issue"; {
		issue := events[position].index
		updates = updates[:0]
		for ; position < len(events) && events[position].owner == "issue" && events[position].index == issue; position++ {
			if fillerEventType(position) == "issue.updated" {
				updates = append(updates, position)
			}
		}
		status := r.plan.issues[issue].Status
		if status == "done" {
			continue
		}
		chosen = chosen[:0]
		for _, update := range updates {
			if random.IntN(statusMoveOneIn) == 0 {
				chosen = append(chosen, update)
			}
		}
		switch {
		case len(chosen) == 0 && len(updates) > 0 && status != model.IssueStatusTriage:
			chosen = append(chosen, updates[random.IntN(len(updates))])
		case len(chosen) == 1 && status == model.IssueStatusTriage:
			chosen = chosen[:0]
		}
		from := model.IssueStatusTriage
		for at, update := range chosen {
			into := status
			if at < len(chosen)-1 {
				// The move before the newest one does not go into the issue's status, so the
				// newest one moves it.
				candidates = candidates[:0]
				for _, candidate := range model.IssueStatuses {
					if candidate != "done" && candidate != from && (at < len(chosen)-2 || candidate != status) {
						candidates = append(candidates, candidate)
					}
				}
				into = candidates[random.IntN(len(candidates))]
			}
			moves[update] = into
			from = into
		}
	}
	return moves
}

// movedIssueQuery reads each issue as the payload of a status move carries it: the columns of
// its row that api's issueRowStatement reads, its blockers, its primary document, and the
// repository it resolves to by the server's own rule (refs.IssueRepoTerms).
var movedIssueQuery = `with recursive subjects as (select key from issues), ` + refs.IssueRepoTerms + `
	select i.key, i.project_key, i.number, i.title, i.priority, i.rank, i.labels, i.parent_key,
	       coalesce((select array_agg(l.target_key order by l.target_key) from issue_links l
	                 where l.issue_key = i.key and l.kind = 'blocked_by'), '{}'),
	       i.assignee, i.route, i.created_by, i.created_at, i.closed_at,
	       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
	       ` + refs.IssueRepoColumns + `
	from issues i
	` + refs.IssueRepoJoin

// statusMovePayloads is the payload of a status move of each issue, by key, built as the patch
// route builds it (api's newIssueEventPayload, over model.NewIssueEventPayload): the issue as its
// row holds it (movedIssueQuery); no reference changes, as on every write that indexes no text;
// `changed` naming the status alone; the issue's parent chain, parent first up to the root, as
// `ancestors`; and an empty `thread_logins`, as on every status move that moves no assignee and
// closes or reopens nothing. statusMovePayload fills in the status, the time and the seq of each
// move.
//
// The issue's advances and its components are left empty. The server resolves both up the
// parent chain on each read of the issue (api's issueAdvancesTerms and issueComponentsTerms,
// which no other package can call), and no reader of an `issue.updated` event reads either: the
// delivery hub reads its key, status and priority, the outbox and the graph census its route,
// the keyword fill its title and primary document, the browser's refresh rules its `changed`,
// `ancestors` and `thread_logins`, and the activity list and the parked read its status and
// `changed`. The seed writes no claim, no external link and no document of a role, so the issue
// holds none of them.
func (r *seedRun) statusMovePayloads(ctx context.Context) (map[string]model.IssueEventPayload, error) {
	rows, err := r.database.Pool.Query(ctx, movedIssueQuery)
	if err != nil {
		return nil, fmt.Errorf("read the issues the status moves carry: %w", err)
	}
	defer rows.Close()
	payloads := make(map[string]model.IssueEventPayload, len(r.plan.issues))
	for rows.Next() {
		var issue model.Issue
		var createdBy []byte
		var tooDeep bool
		if err := rows.Scan(&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Priority,
			&issue.Rank, &issue.Labels, &issue.Parent, &issue.BlockedBy, &issue.Assignee, &issue.Route,
			&createdBy, &issue.CreatedAt, &issue.ClosedAt, &issue.PrimaryArtifactID,
			&issue.Repo, &issue.ResolvedRepo, &tooDeep); err != nil {
			return nil, fmt.Errorf("read the issues the status moves carry: %w", err)
		}
		if tooDeep {
			return nil, fmt.Errorf("issue %s has a parent chain deeper than the repository walk reads", issue.Key)
		}
		if err := json.Unmarshal(createdBy, &issue.CreatedBy); err != nil {
			return nil, fmt.Errorf("decode the author of issue %s: %w", issue.Key, err)
		}
		issue.ExternalLinks = []model.ExternalLink{}
		payload := model.NewIssueEventPayload(issue, model.ReferenceChanges{})
		payload.Changed = []string{"status"}
		payload.ThreadLogins = []string{}
		payloads[issue.Key] = payload
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the issues the status moves carry: %w", err)
	}
	for key, payload := range payloads {
		ancestors := []string{}
		for parent := payload.Parent; parent != nil; parent = payloads[*parent].Parent {
			ancestors = append(ancestors, *parent)
		}
		payload.Ancestors = ancestors
		payloads[key] = payload
	}
	return payloads, nil
}

// statusMovePayload is the payload of event, a move of its issue into status: the issue's payload
// (statusMovePayloads) with that status, the event's time as the time the row was last written,
// and the event's seq as the issue's last_seq, as the append fills it in
// (model.IssueEventPayload.WithOwnerLastSeq).
func statusMovePayload(payload model.IssueEventPayload, status string, event eventRow) (string, error) {
	payload.Status = status
	payload.UpdatedAt = event.createdAt
	encoded, err := json.Marshal(payload.WithOwnerLastSeq(event.seq))
	if err != nil {
		return "", fmt.Errorf("encode the status move of issue %s at seq %d: %w", payload.Key, event.seq, err)
	}
	return string(encoded), nil
}

// writeEvents writes the filler events, the log beside the opening events the asks were written
// with. Every event is published (`notify` false and `published_at` set), so a server with NATS
// publishes no backlog of a million events on the first boot after the seed. Some of the
// `issue.updated` events record a status move of their issue (planStatusMoves) and carry the
// payload the patch route gives one (statusMovePayloads); every other event carries a payload
// that only says it is seeded.
func (r *seedRun) writeEvents(ctx context.Context) error {
	events := r.planEvents()
	// The opening events are numbered past the filler events (askEventIDBase), which holds only
	// while the plan writes exactly the budget those ids were counted from.
	if len(events) != r.sizes.Events {
		return fmt.Errorf("the event plan holds %d rows, not the %d the asks' events were numbered past",
			len(events), r.sizes.Events)
	}
	sessions := r.plan.sessions
	moves := r.planStatusMoves(events)
	moved, err := r.statusMovePayloads(ctx)
	if err != nil {
		return err
	}
	if err := r.inBatches(ctx, "events", len(events), 10*batchRows,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			rows := make([][]any, 0, to-from)
			for index := from; index < to; index++ {
				event := events[index]
				var issueKey, projectKey, artifactID any
				switch event.owner {
				case "issue":
					issueKey = r.plan.issues[event.index].Key
				case "artifact":
					artifactID = r.plan.documents[event.index].ID
				default:
					projectKey = r.plan.projects[event.index].Key
				}
				// Three events in five carry a session actor, which is what
				// events_session_actor_created_at_idx is partial on and what
				// `agentLastActivity` groups. The session is chosen by how many session events
				// came before this one, not by the event's own index, so every session is an
				// actor rather than the three in five the index step would reach.
				actor := seedActor
				if index%5 < 3 {
					before := index/5*3 + min(index%5, 3)
					actor = sessionActor(sessions[before%len(sessions)])
				}
				payload := `{"seeded":true}`
				if status, move := moves[index]; move {
					var err error
					if payload, err = statusMovePayload(moved[r.plan.issues[event.index].Key], status, event); err != nil {
						return err
					}
				}
				rows = append(rows, []any{
					int64(index + 1), issueKey, artifactID, projectKey, event.seq,
					fillerEventType(index), actor, payload,
					false, event.createdAt, event.createdAt,
				})
			}
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, eventColumns, pgx.CopyFromRows(rows))
			return err
		}); err != nil {
		return err
	}
	// The sequence is set past every id the seed wrote, the asks' opening events included, so a
	// server appending its first event after the seed does not collide with one of them.
	_, err = r.database.Pool.Exec(ctx, `select setval('events_id_seq', $1, true)`, r.askEventIDEnd())
	if err != nil {
		return fmt.Errorf("set the events sequence past the seeded ids: %w", err)
	}
	return nil
}

// settleEventCounters brings each owner's `last_seq` to the highest seq it carries and each
// project's `next_number` past its highest issue number. events/broker.go's Append reads
// `last_seq` off the owner row (lockEventOwner) and inserts seq = last_seq + 1, so a seeded issue
// left at 0 fails the unique (issue_key, seq) on the first comment, ask, message, upload or title
// edit written to it. An issue's statement also writes the time and the seq of the event at its
// new `last_seq` as `last_event_at` and `last_event_at_seq`, as the append writes them beside
// `last_seq` (QueueAppendWithLogins), so a seeded database stands as a deployment does once its
// fill has run (20261125144002) and the priorities read takes each newest event from the row
// rather than probing it (api's priorityReadQuery).
func (r *seedRun) settleEventCounters(ctx context.Context) error {
	for _, statement := range []string{
		`update issues i set last_seq = e.seq, last_event_at = e.created_at, last_event_at_seq = e.seq
		   from (select distinct on (issue_key) issue_key, seq, created_at
		           from events where issue_key is not null
		          order by issue_key, seq desc) e
		  where e.issue_key = i.key`,
		`update artifacts a set last_seq = coalesce(e.top, 0)
		   from (select artifact_id, max(seq) as top from events where artifact_id is not null group by artifact_id) e
		  where e.artifact_id = a.id`,
		`update projects p set last_seq = coalesce(e.top, 0)
		   from (select project_key, max(seq) as top from events where project_key is not null group by project_key) e
		  where e.project_key = p.key`,
		`update projects p set next_number = coalesce(i.top, 0) + 1
		   from (select project_key, max(number) as top from issues group by project_key) i
		  where i.project_key = p.key`,
	} {
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
