package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The seed writes the catalogue into a project (dispatch://INFRA-2916, R1 and R5): every computed
// eval as version 1, written_via seed, with its series, and one section of the project's
// `evals-md` per eval, whose method block the version's doc_block names. It runs at three places,
// so every project holds the catalogue however it was made: at boot under SeedLease, for every
// project (SeedEveryProject); in the transaction that creates a project through the API (api's
// createProject, through EnsureTx); and on the refresher's pass, for a project that a command or
// a fixture wrote by SQL while the server ran (UnseededProjects, then Ensure).
//
// A seed that finds an eval of the catalogue already written writes a new version of it only
// where the release changed what the catalogue states of it, its own fields (the name, the
// question, the decision, the failure modes, the number rule, the unit, the direction, the kind,
// the source) or its series, or where its section is gone. The change is read against the version
// the seed last wrote, which holds every field the catalogue owns as that seed's build stated it:
// so every version the seed writes takes all of them from the catalogue, and a person's change of
// one stands until the seed next writes a version of that eval. Every field the catalogue does
// not own is carried forward from the newest version: the target, its inclusiveness and its
// judges, the owner role and the person, the issue, the links, the state and what replaced it,
// and the doc block. A series is read and written the same way, each on its own, its links
// carried forward. A method block that exists is never written: a person's edit of it stands. An
// eval whose doc block the document no longer holds (no version yet, the block deleted, or the
// document gone) gets a section at the end of the document, and a version naming that section's
// block.

// SeedLease is the lease the boot pass seeds every project under (SeedEveryProject), so two
// processes that boot together write each project's catalogue once.
const SeedLease = "eval-seed"

// seedRetry is how long the boot pass waits before it tries again, when another process held the
// lease or a project's seed failed.
const seedRetry = time.Minute

// Seeded is what one seed of a project wrote: the versions of definitions, the versions of
// series, the sections of `evals-md`, and whether the seed created the document. A seed that
// found the catalogue written wrote none of them.
type Seeded struct {
	Versions int
	Series   int
	Sections int
	Created  bool
}

// wrote reports whether the seed wrote anything.
func (s Seeded) wrote() bool {
	return s.Versions+s.Series+s.Sections > 0
}

// Ensure writes the catalogue into project in a transaction of its own (the seed's rules above),
// and answers what it wrote. A second Ensure with the same catalogue writes nothing.
func (s *Store) Ensure(ctx context.Context, project string) (Seeded, error) {
	var seeded Seeded
	err := s.inWrite(ctx, project, func(ctx context.Context, w *writeTx) error {
		var err error
		seeded, err = s.ensure(ctx, w)
		return err
	})
	if err != nil {
		return Seeded{}, err
	}
	return seeded, nil
}

// PendingSeed is a seed written in a caller's transaction (EnsureTx): what it wrote, and what the
// caller's commit owes, which Committed pays.
type PendingSeed struct {
	Seeded
	store *Store
	write *writeTx
}

// EnsureTx writes the catalogue into project in tx, the transaction that created the project, so
// the project and its evals commit together or not at all. ctx is the context the document
// service's Join of tx returned, whose ledger the caller commits. Once tx has committed, the caller
// calls Committed on the answer, and never when tx did not commit.
func (s *Store) EnsureTx(ctx context.Context, tx pgx.Tx, project string) (PendingSeed, error) {
	w, err := openWrite(ctx, tx, project)
	if err != nil {
		return PendingSeed{}, err
	}
	seeded, err := s.ensure(ctx, w)
	if err != nil {
		return PendingSeed{}, err
	}
	return PendingSeed{Seeded: seeded, store: s, write: w}, nil
}

// Committed schedules the settlement of the `evals-md` the seed wrote and publishes the document
// events it appended, as Ensure does once its own transaction commits.
func (p PendingSeed) Committed() {
	if p.store != nil {
		p.store.committed(p.write)
	}
}

// UnseededProjects is every project that holds no version the seed wrote, in key order: a project
// made by SQL since the boot pass, which the refresher's pass seeds (Ensure). It is one statement,
// whose probe of each project is an index range of eval_versions_project.
func (s *Store) UnseededProjects(ctx context.Context) ([]string, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select p.key from projects p
		where not exists (
			select 1 from eval_versions v where v.project_key = p.key and v.written_via = 'seed'
		)
		order by p.key
	`)
	if err != nil {
		return nil, fmt.Errorf("read the projects with no seeded eval: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects with no seeded eval: %w", err)
	}
	return keys, nil
}

// SeedEveryProject is the boot pass: Ensure for every project, under SeedLease, which covers a
// project made by SQL and a catalogue the release changed. Another process that holds the lease
// is seeding with its own build's catalogue, so a pass that finds it held tries again after
// seedRetry until a pass of its own runs, and so does a pass in which a project's seed failed. It
// returns when ctx is done.
func (s *Store) SeedEveryProject(ctx context.Context, leases duty.Leases) {
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  SeedLease,
		Leases: leases,
		MinGap: seedRetry,
		Pass: func(ctx context.Context) time.Time {
			if err := s.seedEveryProject(ctx); err != nil {
				if ctx.Err() == nil {
					slog.Warn("dispatch: the eval seed failed; it runs again", "error", err, "after", seedRetry)
				}
				return time.Now().Add(seedRetry)
			}
			return time.Time{}
		},
	})
}

// seedEveryProject is one pass of the boot seed: Ensure of each project in turn, each in a
// transaction of its own, so one project's failure leaves the others seeded.
func (s *Store) seedEveryProject(ctx context.Context) error {
	rows, err := s.database.Pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return fmt.Errorf("read the projects: %w", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the projects: %w", err)
	}
	var failed []error
	var total Seeded
	written := 0
	for _, project := range projects {
		seeded, err := s.Ensure(ctx, project)
		if err != nil {
			failed = append(failed, fmt.Errorf("seed the evals of %s: %w", project, err))
			continue
		}
		if seeded.wrote() {
			written++
		}
		total.Versions, total.Series, total.Sections = total.Versions+seeded.Versions, total.Series+seeded.Series, total.Sections+seeded.Sections
	}
	slog.Info("dispatch: the eval seed ran", "projects", len(projects), "projects_written", written,
		"versions", total.Versions, "series", total.Series, "sections", total.Sections, "failed", len(failed))
	return errors.Join(failed...)
}

// seedWrite is one eval's write of a seed: the version of its definition and the versions of its
// series.
type seedWrite struct {
	definition Definition
	series     []Series
}

// ensure is the seed of w's project in w's transaction. It takes the lock of every eval of the
// catalogue, in the catalogue's order, before it reads what the project holds, so a second seed
// of the project, a register write and a readings write of one of those evals each wait for it
// and then read what it wrote. It then reads the newest version of each eval, the version the seed
// last wrote, the current series and the series as the seed last wrote them, and the blocks of
// `evals-md`; writes the missing sections in one write of the document; and writes the versions.
// The locks are one batch of statements and the versions another, each sent in one round trip
// and run in the order it was queued: a project's first seed writes 20 versions and 95 of series,
// which one statement each would make 115 round trips.
func (s *Store) ensure(ctx context.Context, w *writeTx) (Seeded, error) {
	ids := make([]string, len(catalogue))
	locks := &pgx.Batch{}
	for i, entry := range catalogue {
		ids[i] = EvalID(w.project, entry.definition.Slug)
		locks.Queue(lockEvalStatement, ids[i])
	}
	if err := w.SendBatch(ctx, locks).Close(); err != nil {
		return Seeded{}, fmt.Errorf("lock the seeded evals of %s: %w", w.project, err)
	}
	newest, err := readDefinitions(ctx, w.Tx, `
		select `+definitionColumns+` from evals_current where project_key = $1 and eval_id = any($2)
	`, w.project, ids)
	if err != nil {
		return Seeded{}, err
	}
	lastSeeded, err := readDefinitions(ctx, w.Tx, `
		select distinct on (eval_id) `+definitionColumns+` from eval_versions
		where project_key = $1 and eval_id = any($2) and written_via = 'seed'
		order by eval_id, version desc
	`, w.project, ids)
	if err != nil {
		return Seeded{}, err
	}
	current, err := readSeriesByEval(ctx, w.Tx, `
		select `+seriesColumns+` from eval_series_current where eval_id = any($1)
	`, ids)
	if err != nil {
		return Seeded{}, err
	}
	// The series as the seed last wrote them: the newest version of each that the seed wrote,
	// which is a version written in the transaction of a version of the definition the seed
	// wrote, since every seed that writes a series writes a version of its definition too.
	seededSeries, err := readSeriesByEval(ctx, w.Tx, `
		select distinct on (eval_id, series) `+seriesColumns+` from eval_series s
		where eval_id = any($1) and exists (
			select 1 from eval_versions v
			where v.eval_id = s.eval_id and v.written_via = 'seed' and v.created_at = s.created_at
		)
		order by eval_id, series, version desc
	`, ids)
	if err != nil {
		return Seeded{}, err
	}
	document, found, err := existingDocument(ctx, w)
	if err != nil {
		return Seeded{}, err
	}
	var blocks []model.ArtifactBlock
	if found {
		if blocks, err = s.documents.Blocks(ctx, document.id); err != nil {
			return Seeded{}, fmt.Errorf("read the blocks of the %s document of %s: %w", DocumentSlug, w.project, err)
		}
	}

	var sections []string
	var writes []seedWrite
	for i, entry := range catalogue {
		existing, has := newest[ids[i]]
		last, seeded := lastSeeded[ids[i]]
		missing := !has || !hasBlock(blocks, existing.DocBlock)
		series := seriesToWrite(ids[i], entry.series, current[ids[i]], seededSeries[ids[i]])
		if has && seeded && !missing && len(series) == 0 && !catalogueChanged(entry.definition, last) {
			continue
		}
		next := existing
		if !has {
			next = Definition{EvalID: ids[i], ProjectKey: w.project, Slug: entry.definition.Slug, State: StateActive, Links: []Link{}}
			next.Target, next.TargetInclusive, next.TargetJudges = entry.definition.Target, entry.definition.TargetInclusive, entry.definition.TargetJudges
		}
		takeCatalogueFields(&next, entry.definition)
		if missing {
			next.DocBlock = uuid.NewString()
			sections = append(sections, sectionMarkdown(entry.definition.Slug, next.DocBlock, entry.method()))
		}
		next.Version = existing.Version + 1
		next.WrittenVia, next.WrittenBy = ViaSeed, RegistryActor
		writes = append(writes, seedWrite{definition: next, series: series})
	}

	seeded := Seeded{Sections: len(sections), Created: !found}
	switch {
	case !found:
		if err := s.createDocument(ctx, w, strings.Join(sections, "\n"), RegistryActor); err != nil {
			return Seeded{}, err
		}
	case len(sections) > 0:
		if err := lockDocumentOwner(ctx, w, document); err != nil {
			return Seeded{}, err
		}
		ops := []model.EditOp{{Op: "insert", After: "end", Markdown: strings.Join(sections, "\n")}}
		if err := s.editDocument(ctx, w, document, ops, RegistryActor); err != nil {
			return Seeded{}, err
		}
	}
	inserts := &pgx.Batch{}
	for _, write := range writes {
		statement, args, err := definitionInsert(write.definition, false)
		if err != nil {
			return Seeded{}, err
		}
		inserts.Queue(statement, args...)
		seeded.Versions++
		for _, series := range write.series {
			statement, args, err := seriesInsert(series)
			if err != nil {
				return Seeded{}, err
			}
			inserts.Queue(statement, args...).Exec(func(tag pgconn.CommandTag) error {
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("version %d of series %s of %s was written by a write that holds no lock of the eval", series.Version, series.Series, series.EvalID)
				}
				return nil
			})
			seeded.Series++
		}
	}
	if inserts.Len() == 0 {
		return seeded, nil
	}
	if err := w.SendBatch(ctx, inserts).Close(); err != nil {
		return Seeded{}, fmt.Errorf("write the seeded versions of %s: %w", w.project, err)
	}
	return seeded, nil
}

// catalogueChanged reports whether the catalogue states a field it owns otherwise than last, the
// version the seed last wrote: whether the release changed the eval.
func catalogueChanged(catalogue, last Definition) bool {
	return catalogue.Name != last.Name || catalogue.Question != last.Question || catalogue.NumberRule != last.NumberRule ||
		catalogue.Decision != last.Decision || !slices.Equal(catalogue.FailureModes, last.FailureModes) ||
		catalogue.Unit != last.Unit || catalogue.Direction != last.Direction || catalogue.Kind != last.Kind ||
		catalogue.Source != last.Source
}

// takeCatalogueFields writes into next every field the catalogue owns (catalogueChanged's).
func takeCatalogueFields(next *Definition, catalogue Definition) {
	next.Name, next.Question, next.NumberRule, next.Decision = catalogue.Name, catalogue.Question, catalogue.NumberRule, catalogue.Decision
	next.FailureModes, next.Unit, next.Direction = catalogue.FailureModes, catalogue.Unit, catalogue.Direction
	next.Kind, next.Source = catalogue.Kind, catalogue.Source
}

// seriesToWrite is the versions of the series of eval id a seed writes: version 1 of each series
// of the catalogue the eval does not hold, and the next version of each one the seed never wrote
// or whose fields the catalogue states otherwise than the version the seed last wrote of it
// (seeded), with every field of the catalogue's and its links carried forward. A series the
// release did not change is left as it is, whatever a person set on it since.
func seriesToWrite(id string, catalogue []Series, current, seeded map[string]Series) []Series {
	var writes []Series
	for _, want := range catalogue {
		existing, has := current[want.Series]
		if last, wasSeeded := seeded[want.Series]; has && wasSeeded && !seriesChanged(want, last) {
			continue
		}
		next := want
		next.EvalID, next.Version, next.Links = id, existing.Version+1, []Link{}
		if has {
			next.Links = existing.Links
		}
		writes = append(writes, next)
	}
	return writes
}

// seriesChanged reports whether the catalogue states a series otherwise than last, the version
// the seed last wrote of it: every field but the version's own and its links.
func seriesChanged(catalogue, last Series) bool {
	return catalogue.Unit != last.Unit || catalogue.Role != last.Role || catalogue.IsPrimary != last.IsPrimary ||
		!sameMinN(catalogue.MinN, last.MinN) || catalogue.Listed != last.Listed || catalogue.Agg != last.Agg
}

// readDefinitions answers the definitions statement reads, by eval id.
func readDefinitions(ctx context.Context, tx pgx.Tx, statement string, args ...any) (map[string]Definition, error) {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read the seeded evals: %w", err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
	if err != nil {
		return nil, fmt.Errorf("read the seeded evals: %w", err)
	}
	byID := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		byID[definition.EvalID] = definition
	}
	return byID, nil
}

// readSeriesByEval answers the series statement reads, by eval id and then by name.
func readSeriesByEval(ctx context.Context, tx pgx.Tx, statement string, args ...any) (map[string]map[string]Series, error) {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read the series of the seeded evals: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
	if err != nil {
		return nil, fmt.Errorf("read the series of the seeded evals: %w", err)
	}
	byEval := map[string]map[string]Series{}
	for _, series := range all {
		if byEval[series.EvalID] == nil {
			byEval[series.EvalID] = map[string]Series{}
		}
		byEval[series.EvalID][series.Series] = series
	}
	return byEval, nil
}
