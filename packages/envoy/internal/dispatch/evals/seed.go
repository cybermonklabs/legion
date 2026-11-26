package evals

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The seed writes the catalogue into a project (dispatch://INFRA-2916, R1 and R5): every computed
// eval as version 1, written_via seed, with its series, and one section of the project's
// `evals-md` per eval, whose method block the version's doc_block names. It runs at two places,
// so every project holds the catalogue however it was made: in the transaction that creates a
// project through the API (api's createProject, through EnsureTx); and on the day-row refresher's
// passes (refresh.load), which seed every project on the first pass of each process, so a
// catalogue the release changed reaches every project, and on each later pass every project no
// seed has reached (UnseededProjects: one a command or a fixture wrote by SQL while the server
// ran), with each project whose seed failed (seeding).
//
// A seed that finds an eval of the catalogue already written writes a new version of it only
// where the release changed what the catalogue states of it, its Computation or its series'
// SeriesComputation, or where its section is gone. The change is read against the version the
// seed last wrote, which holds every field the catalogue owns as that seed's build stated it, so
// every version the seed writes takes all of them from the catalogue. A person cannot set one of
// them on a computed eval (the register write refuses it, computationField). Every field a person
// sets is carried forward from the newest version, so no such write is accepted and then lost at
// the next boot (R5): the target, its inclusiveness and its judges, the owner role and the person,
// the issue, the links, the state and what replaced it, and the doc block. A series is read and
// written the same way, each on its own, its min_n carried forward: the catalogue states a series'
// min_n for its first version alone, as it states the target. A method block that exists is never
// written: a person's edit of it stands. An eval whose doc block the document no longer holds (no
// version yet, the block deleted, or the document gone) gets a section at the end of the
// document, and a version naming that section's block.
//
// An eval the seed takes over, one of the catalogue's slugs a reading or a register write
// registered before any seed of its project (a project made by SQL, or a slug a later release adds
// to the catalogue), holds what that registration defaulted where the catalogue states a value.
// The seed gives every eval of the catalogue a target before any editor runs (the spec's Order),
// so it takes the catalogue's target, its inclusiveness and its judges wherever no person set one
// of the three, whatever other field a register write set: a target a person set, one a person
// set and then turned off, and one a person set to none while it was already none are all kept.
// Which fields a person set is the version's own record, written by the register write that
// named them (Definition.SetByPerson), and not what the stored values moved to: a write that
// names a field and leaves its value as it was is a person's choice too, and a seed that filled
// such a field would be a write accepted and then ignored (R5). It takes the catalogue's min_n
// of a series it never wrote that holds none and whose record names none, which is every such
// series but one a person wrote a min_n on, since no write of a logged eval takes a min_n. And
// it takes the catalogue's method where no person set the eval's method and its section still
// holds the sentence its registration wrote (registrationSentence), which says no one has
// written one.

// Seeded is what one seed of a project wrote: the versions of definitions, the versions of
// series, the sections of `evals-md`, and whether the seed created the document. A seed that
// found the catalogue written wrote none of them.
type Seeded struct {
	Versions int
	Series   int
	Sections int
	Created  bool
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

// EnsureTx writes the catalogue into project, whose name is name, in tx, the transaction that
// creates the project, so the project and its evals commit together or not at all. ctx is the
// context the document service's Join of tx returned, and ledger is that Join's ledger, which the
// caller commits. The seed runs in the transaction's read phase, before the project's row is
// written: it reads, takes its locks and does its document work holding no transaction id, and
// queues every write on ledger, so they run in the write phase after the caller's own insert of
// the project, which their rows name. The seed's versions record their changes in the change
// log's outbox and take no lock, as every registry write's do (inWrite). Once tx has committed,
// the caller calls Committed on the answer, and never when tx did not commit.
func (s *Store) EnsureTx(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, project, name string) (PendingSeed, error) {
	now, err := ledger.Now(ctx)
	if err != nil {
		return PendingSeed{}, err
	}
	w := &writeTx{Tx: tx, ledger: ledger, now: now.UTC(), project: project, projectName: name}
	seeded, err := s.ensure(ctx, w)
	if err != nil {
		return PendingSeed{}, err
	}
	return PendingSeed{Seeded: seeded, store: s, write: w}, nil
}

// Committed schedules the settlement the seed's writes of `evals-md` left owed - none where it
// created the document and the document's seed owes none (createDocument) - publishes the document
// events it appended and moves its change rows into the change log, as Ensure does once its own
// transaction commits.
func (p PendingSeed) Committed(ctx context.Context) {
	if p.store != nil {
		p.store.committed(ctx, p.write)
	}
}

// UnseededProjects is every project that holds no version the seed wrote, in key order: a project
// made by SQL since the refresher's first pass, which its later passes seed (refresh.load). It is
// one statement, whose probe of each project is an index range of eval_versions_project.
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

// seeding is what the refresher's loop keeps of its seeds from one pass to the next
// (refresh.load): whether a pass of this process has seeded every project yet, the projects
// whose seed failed, which the next pass seeds again, and those whose seed was refused for their
// `evals-md` (seedRefusals); and, beside them, the process's watch of the release rule
// (releaseWatch, rebuild.go), which its first pass decides, and the UTC day on which its passes
// last wrote each project's failure rows whole (refresh.writeFailures). One goroutine runs the
// loop's passes, so it takes no lock.
type seeding struct {
	everyProject    bool
	failed          map[string]bool
	refused         seedRefusals
	release         releaseWatch
	failuresWritten map[string]time.Time
}

// owed is the projects a pass seeds besides those it reads to seed: every one whose seed failed,
// and every one whose seed was refused for its document, in key order.
func (s *seeding) owed() []string {
	return slices.Concat(slices.Sorted(maps.Keys(s.failed)), slices.Sorted(maps.Keys(s.refused)))
}

// note records what a seed of project answered: a refusal for its document is held (seedRefusals),
// any other failure is seeded again by the next pass, and a project no row holds any more is no
// failure to seed again, since its seed is refused PROJECT_NOT_FOUND, and so is every later one.
func (s *seeding) note(project string, err error) {
	s.refused.note(project, err)
	var refusal *Error
	if err != nil && !errors.As(err, &refusal) {
		if s.failed == nil {
			s.failed = map[string]bool{}
		}
		s.failed[project] = true
		return
	}
	delete(s.failed, project)
}

// seedRefusals is the projects whose seed was refused because their `evals-md` artifact is no
// document (CodeDocumentNotDocument). The seed reads that artifact whatever else it writes
// (ensure, lockDocument), so it refuses such a project again for as long as the artifact
// stands, and only a person's write of the artifact moves it. A seed of such a project is
// therefore not tried again while one read says the artifact is still no document (pending),
// rather than once a pass, each a transaction of about ten statements. The zero value holds none.
type seedRefusals map[string]bool

// note records what a seed of project answered: a refusal for its document is held, and any other
// answer lets go of one.
func (r *seedRefusals) note(project string, err error) {
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != CodeDocumentNotDocument {
		delete(*r, project)
		return
	}
	if *r == nil {
		*r = seedRefusals{}
	}
	(*r)[project] = true
}

// pending is projects, each once and in their order, less each whose refusal is held and whose
// `evals-md` is still no document, which one statement reads for all of them. A held project
// whose artifact is a document now, or gone, is let go of and kept, so its seed runs again.
func (r seedRefusals) pending(ctx context.Context, pool *store.Pool, projects []string) ([]string, error) {
	seen := make(map[string]bool, len(projects))
	var unique, held []string
	for _, project := range projects {
		if seen[project] {
			continue
		}
		seen[project] = true
		unique = append(unique, project)
		if r[project] {
			held = append(held, project)
		}
	}
	if len(held) == 0 {
		return unique, nil
	}
	rows, err := pool.Query(ctx, `
		select project_key from artifacts
		where project_key = any($1) and issue_key is null and session_id is null and slug = $2 and kind <> 'doc'
	`, held, DocumentSlug)
	if err != nil {
		return nil, fmt.Errorf("read the %s artifacts of the projects whose seed was refused: %w", DocumentSlug, err)
	}
	still, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the %s artifacts of the projects whose seed was refused: %w", DocumentSlug, err)
	}
	for _, project := range held {
		if !slices.Contains(still, project) {
			delete(r, project)
		}
	}
	return slices.DeleteFunc(unique, func(project string) bool { return slices.Contains(still, project) }), nil
}

// seedWrite is one eval's write of a seed: the version of its definition and the versions of its
// series.
type seedWrite struct {
	definition Definition
	series     []Series
}

// ensure is the seed of w's project in w's transaction. It takes the lock of every eval of the
// catalogue, in one statement and in the order of their lock keys (lockEvals), the order the zero
// fill takes several evals' locks in, before it reads what the project holds, so a second seed of
// the project, a register write, a readings write and the fill of one of those evals each wait for
// it and then read what it wrote, and none of them waits for it in a cycle. It then takes the
// owner lock of the project's `evals-md` (lockDocument), as every other registry write of the
// document takes it after its eval locks, and reads the newest version of each eval, the version
// the seed last wrote, the current series and the series as the seed last wrote them, the first
// version of an eval it takes over, and the blocks of `evals-md`. All of that, and the document
// work of the missing sections and the methods it takes over, one write of the document, runs in
// the transaction's read phase, holding no transaction id; the document's writes and then the
// versions are queued on the write's ledger, whose Commit sends them as one batch: a project's
// first seed writes 21 versions and 94 of series, which one round trip each would make 115.
func (s *Store) ensure(ctx context.Context, w *writeTx) (Seeded, error) {
	ids := make([]string, len(catalogue))
	for i, entry := range catalogue {
		ids[i] = EvalID(w.project, entry.definition.Slug)
	}
	if err := lockEvals(ctx, w.Tx, ids); err != nil {
		return Seeded{}, fmt.Errorf("lock the seeded evals of %s: %w", w.project, err)
	}
	document, found, err := lockDocument(ctx, w)
	if err != nil {
		return Seeded{}, err
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
	taken, err := readTakeovers(ctx, w, newest, lastSeeded)
	if err != nil {
		return Seeded{}, err
	}
	var markdown string
	var blocks []model.ArtifactBlock
	switch {
	case found && len(taken) > 0:
		if markdown, blocks, err = s.documents.TextWithBlocks(ctx, document.id); err != nil {
			return Seeded{}, fmt.Errorf("read the %s document of %s: %w", DocumentSlug, w.project, err)
		}
	case found:
		if blocks, err = s.documents.Blocks(ctx, document.id); err != nil {
			return Seeded{}, fmt.Errorf("read the blocks of the %s document of %s: %w", DocumentSlug, w.project, err)
		}
	}

	var sections []string
	var replaced []model.EditOp
	var writes []seedWrite
	for i, entry := range catalogue {
		existing, has := newest[ids[i]]
		last, seeded := lastSeeded[ids[i]]
		missing := !has || !hasBlock(blocks, existing.DocBlock)
		first, takeover := taken[ids[i]]
		unwritten := takeover && !missing && !setByPerson(existing.SetByPerson, fieldDocText, fieldDocBlock) &&
			holdsRegistrationSentence(markdown, blocks, existing.DocBlock, first)
		series := seriesToWrite(ids[i], entry.series, current[ids[i]], seededSeries[ids[i]])
		if has && seeded && !missing && len(series) == 0 && entry.definition.Computation.same(last.Computation) {
			continue
		}
		next := existing
		if !has {
			next = Definition{EvalID: ids[i], ProjectKey: w.project, Slug: entry.definition.Slug, State: StateActive, Links: []Link{}}
		}
		takeCatalogueFields(&next, entry.definition, !has || takeover)
		next.Computation = entry.definition.Computation
		switch {
		case missing:
			next.DocBlock = uuid.NewString()
			sections = append(sections, sectionMarkdown(entry.definition.Slug, next.DocBlock, entry.method()))
		case unwritten:
			next.DocBlock = uuid.NewString()
			replaced = append(replaced,
				model.EditOp{Op: "insert", After: "block:" + existing.DocBlock, Markdown: methodBlock(next.DocBlock, entry.method())},
				model.EditOp{Op: "delete", Block: existing.DocBlock})
		}
		next.Version = existing.Version + 1
		next.WrittenVia, next.WrittenBy = ViaSeed, RegistryActor
		writes = append(writes, seedWrite{definition: next, series: series})
	}

	seeded := Seeded{Sections: len(sections) + len(replaced)/2, Created: !found}
	switch {
	case !found:
		if err := s.createDocument(ctx, w, strings.Join(sections, "\n"), RegistryActor); err != nil {
			return Seeded{}, err
		}
	case len(sections) > 0 || len(replaced) > 0:
		ops := replaced
		if len(sections) > 0 {
			ops = append(ops, model.EditOp{Op: "insert", After: "end", Markdown: strings.Join(sections, "\n")})
		}
		if err := s.editDocument(ctx, w, document, ops, RegistryActor); err != nil {
			return Seeded{}, err
		}
	}
	// The versions' inserts, encoded here, in the read phase, and queued as one step after the
	// document's writes. A series insert writes nothing where that version exists, which only a
	// write that holds no lock of the eval could have stored, so its callback refuses that.
	type seedInsert struct {
		statement string
		args      []any
		series    *Series
	}
	var inserts []seedInsert
	for _, write := range writes {
		statement, args, err := definitionInsert(write.definition)
		if err != nil {
			return Seeded{}, err
		}
		inserts = append(inserts, seedInsert{statement: statement, args: args})
		seeded.Versions++
		for _, series := range write.series {
			statement, args, err := seriesInsert(series)
			if err != nil {
				return Seeded{}, err
			}
			inserts = append(inserts, seedInsert{statement: statement, args: args, series: &series})
			seeded.Series++
		}
	}
	if len(inserts) == 0 {
		return seeded, nil
	}
	what := "write the seeded versions of " + w.project
	if err := w.ledger.Queue(func(b *store.WriteBatch) error {
		for _, insert := range inserts {
			queued := b.Queue(what, insert.statement, insert.args...)
			if series := insert.series; series != nil {
				queued.Exec(func(tag pgconn.CommandTag) error {
					if tag.RowsAffected() != 1 {
						return fmt.Errorf("version %d of series %s of %s was written by a write that holds no lock of the eval", series.Version, series.Series, series.EvalID)
					}
					return nil
				})
			}
		}
		return nil
	}); err != nil {
		return Seeded{}, err
	}
	return seeded, nil
}

// setByPerson reports whether record, the set_by_person of a version or of a series, holds any
// of fields: a person's register write named it, whatever value it left (R5). The seed fills a
// field of the catalogue on an eval it takes over only where it does not, so no accepted write
// is reverted by a boot.
func setByPerson(record []string, fields ...string) bool {
	return slices.ContainsFunc(fields, func(field string) bool { return slices.Contains(record, field) })
}

// takeCatalogueFields writes the catalogue entry's target, its inclusiveness and its judges into
// next, the fields of a definition the seed fills rather than carries forward. first says this
// is the first version of the eval the seed writes: an eval of a project it seeds, or one of the
// catalogue's slugs a reading or a register write registered before any seed of its project,
// which the seed takes over. It fills the three there, and never where the eval's record says a
// person set one of them. The three go together: a person who names any of them has stated how
// the number is judged, and the catalogue's target over a person's inclusiveness would be
// neither theirs nor the catalogue's. Every other field of the definition a person sets is
// carried forward by the copy this writes into.
func takeCatalogueFields(next *Definition, entry Definition, first bool) {
	if !first || setByPerson(next.SetByPerson, fieldTarget, fieldTargetInclusive, fieldTargetJudges) {
		return
	}
	next.Target, next.TargetInclusive, next.TargetJudges = entry.Target, entry.TargetInclusive, entry.TargetJudges
}

// readTakeovers is version 1 of each eval newest holds a version of and lastSeeded none, by id:
// the registration whose defaults an eval the seed takes over holds (the seed's rules above),
// which its method is read against (holdsRegistrationSentence). It is one statement, and none
// where there is no such eval, as on every seed of a project seeded before.
//
// What a person set is read from the version's own record (set_by_person) and not from the
// values the versions hold: a register write of a target that moved no value, `target: null` on
// an eval whose target is already none, records the target as that person's and leaves every
// stored value as it was, and the seed must leave that target off (R5).
func readTakeovers(ctx context.Context, w *writeTx, newest, lastSeeded map[string]Definition) (map[string]Definition, error) {
	var ids []string
	for id := range newest {
		if _, seeded := lastSeeded[id]; !seeded {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return readDefinitions(ctx, w.Tx, `
		select `+definitionColumns+` from eval_versions where project_key = $1 and eval_id = any($2) and version = 1
	`, w.project, ids)
}

// holdsRegistrationSentence reports whether the block id of the document whose markdown and
// blocks these are still holds the sentence the registration of first, an eval's version 1 that a
// reading or a register write wrote, put in its section (registrationSentence): no one has
// written the eval's method since.
func holdsRegistrationSentence(markdown string, blocks []model.ArtifactBlock, id string, first Definition) bool {
	held, found := blockMethod(markdown, blocks, id)
	return found && sameMethod(held, registrationSentence(first.WrittenVia, first.WrittenBy, first.CreatedAt))
}

// seriesToWrite is the versions of the series of eval id a seed writes: version 1 of each series
// of the catalogue the eval does not hold, and the next version of each one the seed never wrote
// or whose SeriesComputation the catalogue states otherwise than the version the seed last wrote
// of it (seeded), with every field of the catalogue's. The min_n a series holds is carried
// forward where the seed wrote the series, so the version the seed last wrote may hold a
// person's, which no catalogue change is read against; where it holds one the seed never wrote;
// and where the series' record says a person set it, whatever value they left, as a definition's
// target is kept (takeCatalogueFields). A series the seed never wrote that holds no min_n no
// person set, one a reading or a register write of a logged eval registered, takes the
// catalogue's, as its eval's target does (the seed's rules above). A series the release did not
// change is left as it is, whatever a person set on it since.
//
// A series the eval holds listed or primary that the catalogue no longer states (a release dropped
// it, or a logged eval the seed takes over registered it) gets its next version unlisted and not
// primary, every other field carried forward: the series are the computation's
// (computationField), a listed series is one every read folds and states, and a series no day
// builder writes would read "no data" in every window. It stays a row of the registry, as every
// version does.
func seriesToWrite(id string, catalogue []Series, current, seeded map[string]Series) []Series {
	var writes []Series
	stated := make(map[string]bool, len(catalogue))
	for _, want := range catalogue {
		stated[want.Series] = true
		existing, has := current[want.Series]
		last, wasSeeded := seeded[want.Series]
		if has && wasSeeded && want.SeriesComputation.same(last.SeriesComputation) {
			continue
		}
		next := want
		next.EvalID, next.Version = id, existing.Version+1
		next.SetByPerson = existing.SetByPerson
		if has && (wasSeeded || existing.MinN != nil || setByPerson(existing.SetByPerson, fieldMinN)) {
			next.MinN = existing.MinN
		}
		writes = append(writes, next)
	}
	for _, name := range slices.Sorted(maps.Keys(current)) {
		existing := current[name]
		if stated[name] || (!existing.Listed && !existing.IsPrimary) {
			continue
		}
		next := existing
		next.Version++
		next.Listed, next.IsPrimary = false, false
		writes = append(writes, next)
	}
	return writes
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
