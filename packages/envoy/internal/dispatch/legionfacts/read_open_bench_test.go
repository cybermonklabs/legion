package legionfacts

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The project BenchmarkReadOpenAtInfrasSize reads: INFRA's stored rows at the size R8's load gives
// it, one in five of them open, as 2,621 of the 13,105 rows of the measured fixture were.
const (
	benchmarkedRows     = 13000
	benchmarkedOpenRows = benchmarkedRows / 5
)

// BenchmarkReadOpenAtInfrasSize measures what the board's read of a project's open rows
// (ReadOpen) costs at INFRA's size: its statement, the scan of its rows, the decode of each row's
// hours by column and growth, and the close of each at the read's clock, the read_open_ms and
// read_open_grow_ms parts of the hub's recompute line. It makes a database of its own
// (storetest.Open) holding one project of benchmarkedRows rows, each open row with the hours of
// four columns to four decimals, as the computation rounds them, and the four rates an open
// issue's growth holds, and each closed row with six columns and no growth. The grow-ns/op
// metric is the part of each read its close took.
//
//	go test -run '^$' -bench '^BenchmarkReadOpenAtInfrasSize$' -benchtime 50x ./internal/dispatch/legionfacts/
func BenchmarkReadOpenAtInfrasSize(b *testing.B) {
	ctx := b.Context()
	pool := storetest.Open(b).Pool
	if _, err := pool.Exec(ctx, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at, hours_by_phase, work_hours,
		                         review_rounds, root_key, tree_depth, slot_hours, waiting_reason,
		                         work_first_at, work_last_at, updated_at, as_of, growth)
		select 'INFRA', 'INFRA-' || n, $1::timestamptz - make_interval(hours => 48 + n % 500),
		       case when n % 5 <> 0 then $1::timestamptz - make_interval(hours => n % 400) end,
		       case when n % 5 = 0
		            then jsonb_build_object('queue', 1.5 + (n % 37) * 0.7311, 'design', 2.25 + (n % 37) * 0.7311,
		                                    'implementing', 3.5 + (n % 37) * 0.7311, 'reviewing', 1.25 + (n % 37) * 0.7311)
		            else jsonb_build_object('queue', 1.5 + (n % 37) * 0.7311, 'design', 2.25 + (n % 37) * 0.7311,
		                                    'implementing', 3.5 + (n % 37) * 0.7311, 'reviewing', 1.25 + (n % 37) * 0.7311,
		                                    'testing', 0.75 + (n % 37) * 0.7311, 'merging', 0.5 + (n % 37) * 0.7311) end,
		       8.5 + (n % 37) * 0.7311, n % 3, 'INFRA-' || (n - n % 5), least(n % 5, 2), 6 + (n % 37) * 0.7311,
		       case when n % 20 = 0 then 'waiting for a slot' end,
		       $1::timestamptz - make_interval(hours => 47 + n % 500), $1::timestamptz - make_interval(hours => n % 400),
		       $1::timestamptz - make_interval(secs => n), $1::timestamptz,
		       case when n % 5 = 0
		            then '{"work_hours": 1, "slot_hours": 1, "work_last_at": 1, "phase_hours.implementing": 1}'::jsonb
		            else '{}'::jsonb end
		from generate_series(1, $2::int) as n
	`, time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC), benchmarkedRows); err != nil {
		b.Fatalf("seed the stored rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze issue_facts`); err != nil {
		b.Fatalf("analyze issue_facts: %v", err)
	}
	at := time.Date(2026, 11, 27, 12, 30, 0, 0, time.UTC)
	var growing time.Duration
	for b.Loop() {
		rows, grew, err := ReadOpen(ctx, pool, "INFRA", at)
		if err != nil {
			b.Fatalf("ReadOpen: %v", err)
		}
		if len(rows) != benchmarkedOpenRows {
			b.Fatalf("ReadOpen answered %d rows, want the %d open rows", len(rows), benchmarkedOpenRows)
		}
		growing += grew
	}
	b.ReportMetric(float64(growing.Nanoseconds())/float64(b.N), "grow-ns/op")
}
