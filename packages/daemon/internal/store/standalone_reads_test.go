package store

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sentStatements records every statement a pool's connections send (pgx.QueryTracer).
type sentStatements struct {
	mu  sync.Mutex
	sql []string
}

func (s *sentStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sql = append(s.sql, data.SQL)
	return ctx
}

func (*sentStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *sentStatements) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sql)
}

// A read the store opens a transaction of its own for, which writes nothing and takes no row or
// advisory lock, opens it read-only (TransactWith, pgx.ReadOnly): PostgreSQL then refuses a write
// the read would make, and the commit costs no read of the transaction's id, which only a
// transaction that may write needs. Each method runs the store on a pool whose connections record
// what they send. Controller is the control: a read that opened read-only before the others were
// held to it.
func TestEachStandaloneReadOfTheStoreOpensReadOnlyAndReadsNoID(t *testing.T) {
	seed := migratedStore(t)
	for _, read := range []struct {
		name string
		run  func(ctx context.Context, s *Store) error
	}{
		{name: "Controller", run: func(ctx context.Context, s *Store) error {
			_, _, err := s.Controller(ctx, "LEGION")
			return err
		}},
		{name: "SessionConflicts", run: func(ctx context.Context, s *Store) error {
			_, err := s.SessionConflicts(ctx, "legion")
			return err
		}},
		{name: "IssueReleaseStop", run: func(ctx context.Context, s *Store) error {
			_, err := s.IssueReleaseStop(ctx, "legion", "LEGION-208", 1)
			return err
		}},
		{name: "IssueReleaseCheck", run: func(ctx context.Context, s *Store) error {
			_, err := s.IssueReleaseCheck(ctx, "legion", "LEGION-208", 1)
			return err
		}},
		// The reads that run through ReadOnly, the one form of a standalone read.
		{name: "SchemaVersion", run: func(ctx context.Context, s *Store) error {
			_, err := s.SchemaVersion(ctx)
			return err
		}},
		{name: "Boots", run: func(ctx context.Context, s *Store) error {
			_, _, err := s.Boots(ctx, "legion")
			return err
		}},
		{name: "Claims", run: func(ctx context.Context, s *Store) error {
			_, err := s.Claims(ctx)
			return err
		}},
		{name: "StuckPodResumes", run: func(ctx context.Context, s *Store) error {
			_, err := s.StuckPodResumes(ctx, "legion", time.Now())
			return err
		}},
		{name: "OpenPodHold", run: func(ctx context.Context, s *Store) error {
			_, err := s.OpenPodHold(ctx, "legion", "LEGION-208")
			return err
		}},
		{name: "PodReleaseRecords", run: func(ctx context.Context, s *Store) error {
			_, err := s.PodReleaseRecords(ctx, "legion", time.Time{})
			return err
		}},
		{name: "DuePodBackstops", run: func(ctx context.Context, s *Store) error {
			_, err := s.DuePodBackstops(ctx, "legion", time.Now())
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			ctx := context.Background()
			sent := &sentStatements{}
			cfg := seed.Pool().Config()
			cfg.ConnConfig.Tracer = sent
			traced, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("open the recording pool: %v", err)
			}
			t.Cleanup(traced.Close)
			if err := read.run(ctx, &Store{pool: NewPool(traced)}); err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			statements := sent.statements()
			if len(statements) == 0 {
				t.Fatal("UNKNOWN: the pool sent no statement, so nothing says how the read opened its transaction")
			}
			begins, readOnly, ids := 0, 0, 0
			for _, sql := range statements {
				lower := strings.ToLower(strings.TrimSpace(sql))
				if strings.HasPrefix(lower, "begin") {
					begins++
					if strings.Contains(lower, "read only") {
						readOnly++
					}
				}
				if strings.Contains(sql, "pg_current_xact_id_if_assigned") {
					ids++
				}
			}
			if begins != 1 || readOnly != 1 || ids != 0 {
				t.Errorf("%s began %d transactions, %d of them read-only, and read the transaction's id %d times; want one read-only transaction and no id read; sent %q",
					read.name, begins, readOnly, ids, statements)
			}
		})
	}
}
