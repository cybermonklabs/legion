package record

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The two kinds of row pull_request_settled_heads holds (migration 20261120144241): the pull
// request's own memory of its settled heads (PullRequest.SettledHeads), and the memory its open
// head run was anchored with (HeadRun.Settled).
const (
	settledOfPullRequest = "pull_request"
	settledOfHeadRun     = "head_run"
)

// storedFence is a remembered entry (Settlement) as the fence column of its row holds it. The row
// names the entry's head in a column of its own, so a run or a status of that head is stored with
// no head (fenceRun, fenceStatus), and the entry's own Head is left out: a field of the same JSON
// name shallower than an embedded struct's hides that struct's field in encoding/json, so Head,
// always nil, hides Settlement's, and CheckRuns and Statuses hide theirs. Every other field of
// Settlement is written as its own JSON writes it, a field added to it later with it. Each list is
// written as null when nil and as a list when empty, so it reads back as it was put.
type storedFence struct {
	Settlement
	Head      *struct{}     `json:"head,omitempty"`
	CheckRuns []fenceRun    `json:"checkRuns"`
	Statuses  []fenceStatus `json:"statuses"`
}

// fenceRun is a check run as a stored fence keeps it: Head is nil for a run of the row's head, and
// otherwise the run's own head, "" included. It hides AttemptRun's head as storedFence hides
// Settlement's, so every other field of the run is written as AttemptRun writes it.
type fenceRun struct {
	AttemptRun
	Head *string `json:"head,omitempty"`
}

// fenceStatus is a commit status as a stored fence keeps it, its head kept as fenceRun keeps a
// run's.
type fenceStatus struct {
	CommitStatus
	Head *string `json:"head,omitempty"`
}

// storedHead is the head a stored run or status of a fence of the row's head keeps: none for that
// head, which the row names, and its own otherwise.
func storedHead(own, row string) *string {
	if own == row {
		return nil
	}
	return &own
}

// readHead is the head a stored run or status of a fence of the row's head is of: the row's where
// it keeps none.
func readHead(stored *string, row string) string {
	if stored == nil {
		return row
	}
	return *stored
}

// eachOf is f of each of items, in order, and nil for nil: a stored fence keeps a nil list apart
// from an empty one, as the entry does.
func eachOf[T, U any](items []T, f func(T) U) []U {
	if items == nil {
		return nil
	}
	out := make([]U, len(items))
	for i, item := range items {
		out[i] = f(item)
	}
	return out
}

// fenceOfEntry is entry as the fence column of its row holds it.
func fenceOfEntry(entry Settlement) storedFence {
	return storedFence{Settlement: entry,
		CheckRuns: eachOf(entry.CheckRuns, func(run AttemptRun) fenceRun {
			return fenceRun{AttemptRun: run, Head: storedHead(run.Head, entry.Head)}
		}),
		Statuses: eachOf(entry.Statuses, func(status CommitStatus) fenceStatus {
			return fenceStatus{CommitStatus: status, Head: storedHead(status.Head, entry.Head)}
		})}
}

// entryOfFence is the entry a row of head holds in its fence column, every run and status stamped
// with head again where the column keeps none.
func entryOfFence(head string, fence []byte) (Settlement, error) {
	var stored storedFence
	if err := json.Unmarshal(fence, &stored); err != nil {
		return Settlement{}, fmt.Errorf("decode the fence of settled head %s: %w", head, err)
	}
	entry := stored.Settlement
	entry.Head = head
	entry.CheckRuns = eachOf(stored.CheckRuns, func(run fenceRun) AttemptRun {
		run.AttemptRun.Head = readHead(run.Head, head)
		return run.AttemptRun
	})
	entry.Statuses = eachOf(stored.Statuses, func(status fenceStatus) CommitStatus {
		status.CommitStatus.Head = readHead(status.Head, head)
		return status.CommitStatus
	})
	return entry, nil
}

// loadSettledHeads reads what pr remembers of its settled heads, both kinds in one query and each
// in its order: the pull request's own into SettledHeads, and its open run's into Run.Settled. A
// row of a run where pr has no run open is one a write left before the run closed, which the next
// write deletes, and is passed over. No row is a nil list.
func loadSettledHeads(ctx context.Context, tx pgx.Tx, pr *PullRequest) error {
	rows, err := tx.Query(ctx, `select kind, head, fence from pull_request_settled_heads where issue = $1 order by kind, position`, pr.Issue)
	if err != nil {
		return fmt.Errorf("read the settled heads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, head string
		var fence []byte
		if err := rows.Scan(&kind, &head, &fence); err != nil {
			return fmt.Errorf("read the settled heads: %w", err)
		}
		var list *[]Settlement
		switch {
		case kind == settledOfPullRequest:
			list = &pr.SettledHeads
		case kind == settledOfHeadRun && pr.Run != nil:
			list = &pr.Run.Settled
		default:
			continue
		}
		entry, err := entryOfFence(head, fence)
		if err != nil {
			return err
		}
		*list = append(*list, entry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the settled heads: %w", err)
	}
	return nil
}

// settledRow is one row of pull_request_settled_heads as putSettledHeads passes it to the
// statement.
type settledRow struct {
	Kind     string      `json:"kind"`
	Position int         `json:"position"`
	Head     string      `json:"head"`
	Fence    storedFence `json:"fence"`
}

// putSettledHeads writes what pr remembers of its settled heads, both kinds, in one statement,
// after pr's own row: it deletes each row of pr's issue whose kind and head pr no longer holds,
// the rows of a run when pr has none open among them, and writes each entry at its place in its
// list. An entry whose place and fence are as stored writes no new version of its row.
func putSettledHeads(ctx context.Context, tx pgx.Tx, pr PullRequest) error {
	size := len(pr.SettledHeads)
	if pr.Run != nil {
		size += len(pr.Run.Settled)
	}
	held := make([]settledRow, 0, size)
	for i, entry := range pr.SettledHeads {
		held = append(held, settledRow{Kind: settledOfPullRequest, Position: i, Head: entry.Head, Fence: fenceOfEntry(entry)})
	}
	if pr.Run != nil {
		for i, entry := range pr.Run.Settled {
			held = append(held, settledRow{Kind: settledOfHeadRun, Position: i, Head: entry.Head, Fence: fenceOfEntry(entry)})
		}
	}
	encoded, err := json.Marshal(held)
	if err != nil {
		return fmt.Errorf("encode the settled heads: %w", err)
	}
	if _, err := tx.Exec(ctx, `with held as (
			select kind, position, head, fence
			from jsonb_to_recordset($2::jsonb) as entry(kind text, position smallint, head text, fence jsonb)),
		gone as (
			delete from pull_request_settled_heads stored where stored.issue = $1
				and not exists (select 1 from held where held.kind = stored.kind and held.head = stored.head))
		insert into pull_request_settled_heads (issue, kind, position, head, fence)
		select $1, kind, position, head, fence from held
		on conflict (issue, kind, head) do update set position = excluded.position, fence = excluded.fence
		where (pull_request_settled_heads.position, pull_request_settled_heads.fence)
			is distinct from (excluded.position, excluded.fence)`, pr.Issue, encoded); err != nil {
		return fmt.Errorf("write the settled heads: %w", err)
	}
	return nil
}
