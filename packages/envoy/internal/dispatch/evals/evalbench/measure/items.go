package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// items measures the first read of one view after the caller restarts the seeded server.
// The test database logs each execution with its parameters. Select the one items snapshot
// transaction before opening the explainer, so replay is never included in the capture.
func items(ctx context.Context, r *report, o options, viewFile string, budget float64) error {
	if budget < 0 || math.IsNaN(budget) || math.IsInf(budget, 0) {
		return errors.New("-budget-ms must be finite and non-negative")
	}
	raw, err := os.ReadFile(viewFile)
	if err != nil {
		return fmt.Errorf("read -view-file: %w", err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("decode -view-file: %w", err)
	}
	project, _ := view["project"].(string)
	slug, _ := view["slug"].(string)
	if view["kind"] != "items" || project == "" || slug == "" {
		return errors.New("-view-file must name an items view with project and slug")
	}
	offset, err := fileSize(o.pgLog)
	if err != nil {
		return fmt.Errorf("read -pg-log: %w", err)
	}
	_, body, err := liveSnapshot(ctx, o.url, o.token, view)
	if err != nil {
		return err
	}
	statements, err := waitItemsStatements(ctx, o.pgLog, offset, evals.EvalID(project, slug))
	if err != nil {
		return err
	}
	explain, err := newExplainer(ctx, o.database)
	if err != nil {
		return err
	}
	defer explain.conn.Close(context.WithoutCancel(ctx))
	replay := func(ctx context.Context, text string, params []string) (string, float64, error) {
		return explain.explainAfter(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "set transaction read only")
			return err
		}, text, params)
	}
	total, err := explainItems(ctx, r, statements, replay)
	if err != nil {
		return err
	}
	r.result("items project=%s slug=%s statements=%d db_ms=%.3f budget_ms=%.3f gzip_bytes=%d",
		project, slug, len(statements), total, budget, gzipSize(body))
	if budget > 0 && total > budget {
		r.fail("items project=%s slug=%s: %.3f ms exceeds the database budget of %.3f ms", project, slug, total, budget)
	}
	return nil
}

// waitItemsStatements allows the Postgres log collector to flush the read's commit after the
// socket sends the snapshot. It needs duration logging, not pg_stat_statements.
func waitItemsStatements(ctx context.Context, path string, offset int64, evalID string) ([]logged, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		executions, err := readExecutions(path, offset, true)
		if err != nil {
			return nil, err
		}
		statements, err := itemsStatements(executions, evalID)
		if err != nil || len(statements) > 0 {
			return statements, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no complete items snapshot transaction for %s in -pg-log (requires jsonlog and log_min_duration_statement=0): %w", evalID, ctx.Err())
		case <-tick.C:
		}
	}
}

// itemsStatements finds the read-only snapshot that looks up this eval's definition. It keeps
// query executions in order, even if two use the same query id with different parameters.
func itemsStatements(executions []logged, evalID string) ([]logged, error) {
	type snapshot struct {
		statements []logged
		matches    bool
	}
	pending := map[int64]*snapshot{}
	var found []logged
	for _, stmt := range executions {
		sql := strings.ToLower(oneLine(stmt.text))
		switch sql {
		case "begin isolation level repeatable read read only":
			pending[stmt.pid] = &snapshot{}
		case "commit", "rollback":
			read := pending[stmt.pid]
			delete(pending, stmt.pid)
			if sql == "commit" && read != nil && read.matches {
				if found != nil {
					return nil, fmt.Errorf("more than one snapshot of %s in the statement log; measure a single subscription", evalID)
				}
				found = read.statements
			}
		default:
			if read := pending[stmt.pid]; read != nil && !utility.MatchString(sql) {
				read.statements = append(read.statements, stmt)
				if strings.Contains(sql, " from evals_current where eval_id = $1") && len(stmt.params) == 1 && stmt.params[0] == quote(evalID) {
					read.matches = true
				}
			}
		}
	}
	return found, nil
}

var planningTimeLine = regexp.MustCompile(`(?m)^Planning Time: ([0-9.]+) ms$`)

// explainItems adds Postgres's planning and execution times for each captured statement.
func explainItems(ctx context.Context, r *report, statements []logged,
	explain func(context.Context, string, []string) (string, float64, error),
) (float64, error) {
	var total float64
	for i, stmt := range statements {
		plan, execution, err := explain(ctx, stmt.text, stmt.params)
		if err != nil {
			return 0, fmt.Errorf("explain items statement %d: %w", i+1, err)
		}
		match := planningTimeLine.FindStringSubmatch(plan)
		if match == nil || execution < 0 {
			return 0, fmt.Errorf("items statement %d: explain did not report planning and execution time", i+1)
		}
		planning, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, err
		}
		total += planning + execution
		r.result("items-statement ordinal=%d queryid=%d plan_ms=%.3f exec_ms=%.3f", i+1, stmt.queryID, planning, execution)
		r.block(fmt.Sprintf("PLAN items-statement ordinal=%d", i+1), plan)
	}
	return total, nil
}
