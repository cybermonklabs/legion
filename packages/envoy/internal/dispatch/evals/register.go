package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Registration is one register write: the eval it names, how the version is written, who writes
// it, and each field it sets. Every field is a Field: one the write leaves unset is copied
// forward from the newest version, or takes the default of an eval registered here for the
// first time (kind experiment, source logged, state active, no target, and the caller as owner,
// as ownerOf reads one).
//
// DocText, when set, is the eval's method: it replaces the method block of the eval's section of
// the project's `evals-md` document (a section is added where the document holds none for it, the
// document created first where the project has none), and the new version's doc_block names the
// new block. A write sets DocText or DocBlock, not both. An eval registered here with neither
// gets a section whose method is one sentence saying who registered it, through which write and
// when, so every eval has a section (R5).
//
// Series are the series the write registers or changes, each copied forward the same way. A
// series registered here for the first time is the headline and primary series when the eval has
// no series yet, and a control series otherwise, folded by its last value (agg last) and listed,
// unless the change sets those fields.
//
// The computation fields (ComputationChange, and a series' SeriesComputationChange) are a logged
// eval's alone: on a computed eval the catalogue states them (computationField). A source is
// logged: a computed eval is the seeded catalogue's alone, and a register write naming another
// source is refused. A min_n is taken on pr-ci's job series alone, the one series a read ranks by
// it (rankedByMinN), and refused on any other.
//
// A write records every field it names that is a person's to set (personFields) in the version's
// set_by_person, whether or not it moves that field's value, and the version after it carries
// that record forward. So a person who sets a target to none on an eval whose target is already
// none leaves a version saying the target is theirs, and the seed fills the catalogue's target
// only where no version of the eval says so (R5, the seed's takeCatalogueFields).
type Registration struct {
	Project string
	Slug    string
	Caller  model.Actor

	ComputationChange
	DocBlock        Field[string]
	Target          Field[*float64]
	TargetInclusive Field[bool]
	TargetJudges    Field[[]string]
	OwnerRole       Field[string]
	Person          Field[string]
	IssueKey        Field[string]
	State           Field[string]
	ReplacedBy      Field[string]
	// Links is the eval's whole list of links. Each link's RecordedAt is the server's: a link the
	// newest version already holds (the same relation, address and label) keeps the time it was
	// first stored, and any other is stored at this write's time.
	Links   Field[[]Link]
	DocText Field[string]

	Series []SeriesChange
}

// ComputationChange is a register write's change of each field of a definition's Computation,
// copied forward as Registration's fields are.
type ComputationChange struct {
	Name         Field[string]
	Question     Field[string]
	NumberRule   Field[string]
	Decision     Field[string]
	FailureModes Field[[]FailureMode]
	Unit         Field[string]
	Direction    Field[string]
	Kind         Field[string]
	Source       Field[string]
}

// applyTo writes into next each field the change sets.
func (change ComputationChange) applyTo(next *Computation) {
	change.Name.applyTo(&next.Name)
	change.Question.applyTo(&next.Question)
	change.NumberRule.applyTo(&next.NumberRule)
	change.Decision.applyTo(&next.Decision)
	change.FailureModes.applyTo(&next.FailureModes)
	change.Unit.applyTo(&next.Unit)
	change.Direction.applyTo(&next.Direction)
	change.Kind.applyTo(&next.Kind)
	change.Source.applyTo(&next.Source)
	next.FailureModes = nonNil(next.FailureModes)
}

// named is the first field the change sets, as the body spells it (Computation's json name), or
// "" where it sets none.
func (change ComputationChange) named() string {
	return firstSet([]namedField{
		{"name", change.Name.Set}, {"question", change.Question.Set}, {"number_rule", change.NumberRule.Set},
		{"decision", change.Decision.Set}, {"failure_modes", change.FailureModes.Set}, {"unit", change.Unit.Set},
		{"direction", change.Direction.Set}, {"kind", change.Kind.Set}, {"source", change.Source.Set},
	})
}

// SeriesChange is one series a register write registers or changes, by name, with each field it
// sets, copied forward as Registration's are.
type SeriesChange struct {
	Series string
	SeriesComputationChange
	MinN Field[*int]
}

// SeriesComputationChange is a register write's change of each field of a series'
// SeriesComputation, copied forward as Registration's fields are.
type SeriesComputationChange struct {
	Unit      Field[string]
	Role      Field[string]
	IsPrimary Field[bool]
	Listed    Field[bool]
	Agg       Field[string]
	Links     Field[[]Link]
}

// applyTo writes into next each field the change sets, a link the series already holds keeping
// the time it was first stored (stampLinks) and any other stored at now.
func (change SeriesComputationChange) applyTo(next *SeriesComputation, now time.Time) {
	change.Unit.applyTo(&next.Unit)
	change.Role.applyTo(&next.Role)
	change.IsPrimary.applyTo(&next.IsPrimary)
	change.Listed.applyTo(&next.Listed)
	change.Agg.applyTo(&next.Agg)
	if change.Links.Set {
		next.Links = stampLinks(change.Links.Value, next.Links, now)
	}
}

// named is the first field the change sets, as the body spells it (SeriesComputation's json
// name), or "" where it sets none.
func (change SeriesComputationChange) named() string {
	return firstSet([]namedField{
		{"unit", change.Unit.Set}, {"role", change.Role.Set}, {"is_primary", change.IsPrimary.Set},
		{"listed", change.Listed.Set}, {"agg", change.Agg.Set}, {"links", change.Links.Set},
	})
}

// namedField is one field of a change by the name a body spells it with, and whether the change
// sets it.
type namedField struct {
	name string
	set  bool
}

// firstSet is the name of the first of fields the change sets, "" where it sets none.
func firstSet(fields []namedField) string {
	for _, field := range fields {
		if field.set {
			return field.name
		}
	}
	return ""
}

// The names of the fields a person sets on an eval of the seeded catalogue (R5), as a body
// spells them: the ones a register write records in set_by_person and the seed reads back, each
// spelled once so a write and the seed cannot drift apart.
const (
	fieldTarget          = "target"
	fieldTargetInclusive = "target_inclusive"
	fieldTargetJudges    = "target_judges"
	fieldOwnerRole       = "owner_role"
	fieldPerson          = "person"
	fieldIssueKey        = "issue_key"
	fieldState           = "state"
	fieldReplacedBy      = "replaced_by"
	fieldLinks           = "links"
	fieldDocText         = "doc_text"
	fieldDocBlock        = "doc_block"
	fieldMinN            = "min_n"
)

// personFields is every field of the write that is a person's to set on an eval, as the body
// spells it: the fields R5 names as a person's, which a register write of a computed eval may
// set and the catalogue never states. The fields of the computation are not among them: on a
// computed eval the catalogue states them and this write is refused (computationField), and on a
// logged eval nothing of the registry fills them. The eval's record of them is what the seed
// reads to leave a field a person set alone (Definition.SetByPerson).
func (change Registration) personFields() []string {
	return setFields([]namedField{
		{fieldTarget, change.Target.Set}, {fieldTargetInclusive, change.TargetInclusive.Set},
		{fieldTargetJudges, change.TargetJudges.Set}, {fieldOwnerRole, change.OwnerRole.Set},
		{fieldPerson, change.Person.Set}, {fieldIssueKey, change.IssueKey.Set},
		{fieldState, change.State.Set}, {fieldReplacedBy, change.ReplacedBy.Set},
		{fieldLinks, change.Links.Set}, {fieldDocText, change.DocText.Set},
		{fieldDocBlock, change.DocBlock.Set},
	})
}

// personFields is every field of the series the write sets that is a person's: min_n, the one
// field of a series a person sets on a computed eval (R5), which the seed fills from the
// catalogue where no person set it (seriesToWrite).
func (change SeriesChange) personFields() []string {
	return setFields([]namedField{{fieldMinN, change.MinN.Set}})
}

// setFields is the name of each of fields the change sets, in the order given.
func setFields(fields []namedField) []string {
	var set []string
	for _, field := range fields {
		if field.set {
			set = append(set, field.name)
		}
	}
	return set
}

// withFields is record with each of fields it does not hold, sorted and each once: the record a
// version leaves behind it, which holds every field a person has set by the time it is written,
// since each version carries the record before it forward. Sorted, so two records of one set of
// fields are one value, whatever order the writes named them in.
func withFields(record, fields []string) []string {
	widened := slices.Clone(record)
	for _, field := range fields {
		if !slices.Contains(widened, field) {
			widened = append(widened, field)
		}
	}
	slices.Sort(widened)
	return widened
}

// Registered is what a register write left: the eval's newest definition, the newest version of
// each of its series (sorted by name), and whether the write stored a new version of the
// definition or of any series. A write that changes no field of the definition stores none of it,
// and one that changes no field of a series stores no version of that series; a write that stores
// neither wrote nothing. The record of which fields a person set is one of those fields
// (Definition.SetByPerson), so the first write that names a field stores a version whether or
// not it moves the value, and a repeat of it stores nothing.
type Registered struct {
	Definition Definition
	Series     []Series
	Wrote      bool
}

// Register writes the next version of one eval's definition, and of the series it names, in one
// transaction, copying forward every field the write does not set (Registration). Two writes of
// one eval at once both land, in turn, as the next two versions: each takes the eval's lock
// (lockEval) before it reads the version it writes after. A write of a computed eval that names a
// field of its computation is refused, and stores nothing (computationField, R5), and so is a
// write of a min_n no read ranks by (rankedByMinN), and one that names a series twice
// (CodeSeriesInvalid, validateRegistration). A write that adds a series to a logged eval
// that would then hold more than MaxEvalSeries series is refused (CodeSeriesLimit), counted under
// the eval's lock, so two writes cannot pass the bound together; one that adds none is never
// refused for the series the eval holds. A write that would store a version of the definition, or
// of a series, past MaxVersionBytes as JSON is refused (CodeVersionTooLarge), judged once the
// fields it leaves out are copied forward, under the eval's lock, so two writes each within the
// bound cannot leave a version past it, and before it does its document work or stores anything.
// So is one whose definition would hold a target its unit cannot hold (CheckTarget), judged on the
// version the write leaves: the target the body sets, and one a change of the unit carries forward.
// A version that lists or unlists a logged eval's series lists or unlists that series' stored day
// rows in the same transaction (relistDays), and one that moves a logged eval's fold key (foldKeyOf:
// a listing, or the fold or the role of a listed series) marks stale, in it too, each cut that holds
// a stored figure of the eval or a row the write relisted (markMovedKey), so a read of the eval in
// them folds its day rows, under the new listing, from the write's commit until the refresher's
// refold has folded them again, in budgeted batches. A write that may write the eval's section, one
// carrying doc_text or the first of an eval, takes the document's owner lock before it stores
// anything (lockDocument) and does the section's document work there, in its read phase, queued
// for the commit after its versions' rows (writeMethod). A write carrying doc_block takes the
// same lock and reads the document's blocks under it, and is refused (CodeFieldInvalid) where
// none of them is the block it names: the eval would have no method, and the seed, which writes a
// section of its own for an eval of the catalogue whose doc_block names no block (ensure), would
// replace what the write set at the next boot (R5).
func (s *Store) Register(ctx context.Context, change Registration) (Registered, error) {
	if err := validateRegistration(change); err != nil {
		return Registered{}, err
	}
	evalID := EvalID(change.Project, change.Slug)
	var answer Registered
	err := s.inWrite(ctx, change.Project, func(ctx context.Context, w *writeTx) error {
		answer = Registered{}
		if err := lockEval(ctx, w.Tx, evalID); err != nil {
			return err
		}
		current, found, err := newestDefinition(ctx, w.Tx, evalID)
		if err != nil {
			return err
		}
		series, err := currentSeries(ctx, w.Tx, evalID)
		if err != nil {
			return err
		}
		// key is the fold key the eval's stored figures hold before the write.
		key := foldKeyOf(Entry{Definition: current, Series: slices.Collect(maps.Values(series))})
		if found && current.Computed() {
			if field := computationField(change, series); field != "" {
				return fieldRefusal(CodeFieldComputed, field,
					"%s is a field of the computation of %s, which its code defines (source %s): a register write of a computed eval sets "+
						"its target, target_inclusive and target_judges, the min_n of %s's series %s, owner_role, person, issue_key, links, state, replaced_by, doc_text and doc_block",
					field, evalID, current.Source, SlugPRCI, SeriesJobs)
			}
		}
		for index, one := range change.Series {
			if one.MinN.Set && !rankedByMinN(current, one.Series) {
				return fieldRefusal(CodeFieldInvalid, fmt.Sprintf("series[%d].min_n", index),
					"series %s of %s: min_n is the fewest timed results a job needs before it is ranked among %s's slowest jobs, and no read ranks this series by one: only %s's series %s takes a min_n",
					one.Series, evalID, SlugPRCI, SlugPRCI, SeriesJobs)
			}
		}
		if !current.Computed() {
			if index, count, total := seriesPastLimit(series, len(change.Series), func(index int) string { return change.Series[index].Series }); index >= 0 {
				return fieldRefusal(CodeSeriesLimit, fmt.Sprintf("series[%d].series", index), "%s", seriesLimitReason(evalID, change.Series[index].Series, count, total))
			}
		}
		next := current
		if !found {
			next = firstDefinition(change.Project, change.Slug, change.Caller)
		}
		change.applyTo(&next, w.now)
		next.Version, next.WrittenVia, next.WrittenBy, next.CreatedAt = current.Version+1, ViaRegister, change.Caller.Recorded(), w.now
		// Every version the write would store is judged here (CheckTarget, definitionTooLarge,
		// seriesTooLarge), after the copy forward and before the document work and the inserts
		// below, so a refused write stores nothing. The definition's version is stored where the
		// eval holds none, where the write changes a field of it, and where doc_text is a method the
		// eval's section does not hold yet, which gives it block, the id of the method's new block:
		// it is judged holding the longer of block and the block it holds now. Its target is judged
		// in its unit there, and where the body names the target or the unit and stores no version,
		// so a body naming a target its unit cannot hold is refused whatever the eval holds; a write
		// of neither that stores no version of the definition, a write of a series alone, leaves the
		// standing version as it is and is not judged by it.
		block := uuid.NewString()
		writesDefinition := !found || change.DocText.Set || !sameDefinition(current, next)
		if writesDefinition || change.Target.Set || change.Unit.Set {
			if err := CheckTarget(next.Unit, next.Target); err != nil {
				return err
			}
		}
		if writesDefinition {
			judged := next
			if (change.DocText.Set || (!found && next.DocBlock == "")) && len(judged.DocBlock) < len(block) {
				judged.DocBlock = block
			}
			if err := definitionTooLarge(judged); err != nil {
				return err
			}
		}
		// Each version of a series the write stores, in the order the body names the series, each
		// once (validateRegistration); standing is the eval's series as the write leaves them, so
		// the first series an eval with none registers is its headline.
		standing := maps.Clone(series)
		var seriesWrites []seriesWrite
		for index, seriesChange := range change.Series {
			existing, has := standing[seriesChange.Series]
			nextSeries := existing
			if !has {
				nextSeries = firstSeries(evalID, seriesChange.Series, len(standing) == 0, true, "")
			}
			seriesChange.applyTo(&nextSeries, w.now)
			if has && sameSeries(existing, nextSeries) {
				continue
			}
			nextSeries.Version, nextSeries.CreatedAt = existing.Version+1, w.now
			if err := seriesTooLarge(nextSeries, fmt.Sprintf("series[%d].", index)); err != nil {
				return err
			}
			standing[nextSeries.Series] = nextSeries
			seriesWrites = append(seriesWrites, seriesWrite{version: nextSeries, relist: has && existing.Listed != nextSeries.Listed})
		}
		// The section's document work runs here, before the versions are stored, and queues the
		// document's writes for the commit: nothing where the write leaves the eval's section as
		// it is.
		switch {
		case change.DocText.Set:
			document, documentFound, err := lockDocument(ctx, w)
			if err != nil {
				return err
			}
			held, err := s.holdsMethod(ctx, w, document, documentFound, current.DocBlock, change.DocText.Value)
			if err != nil {
				return err
			}
			// A method the eval's section holds already is no change: its block and the
			// definition stand as they are.
			if !held {
				next.DocBlock = block
				if err := s.writeMethod(ctx, w, document, documentFound, next.Slug, current.DocBlock, next.DocBlock, change.DocText.Value, change.Caller); err != nil {
					return err
				}
			}
		case change.DocBlock.Set:
			document, documentFound, err := lockDocument(ctx, w)
			if err != nil {
				return err
			}
			held := false
			if documentFound {
				blocks, err := s.documents.Blocks(ctx, document.id)
				if err != nil {
					return fmt.Errorf("read the blocks of the %s document of %s: %w", DocumentSlug, w.project, err)
				}
				held = hasBlock(blocks, change.DocBlock.Value)
			}
			if !held {
				return fieldRefusal(CodeFieldInvalid, "doc_block", "doc_block %q names no block of the project's %s document, which holds the eval's method",
					change.DocBlock.Value, DocumentSlug)
			}
		case !found && next.DocBlock == "":
			document, documentFound, err := lockDocument(ctx, w)
			if err != nil {
				return err
			}
			next.DocBlock = block
			if err := s.writeMethod(ctx, w, document, documentFound, next.Slug, "", next.DocBlock, registrationSentence(ViaRegister, change.Caller, w.now), RegistryActor); err != nil {
				return err
			}
		}
		answer.Definition = current
		if !found || !sameDefinition(current, next) {
			stored, err := insertDefinition(ctx, w.Tx, next)
			if err != nil {
				return err
			}
			answer.Definition, answer.Wrote = stored, true
		}
		// relisted is the repository of each row of a series the write listed or unlisted.
		var relisted []string
		for _, write := range seriesWrites {
			stored, inserted, err := insertSeries(ctx, w.Tx, write.version)
			if err != nil {
				return err
			}
			if !inserted {
				return fmt.Errorf("version %d of series %s of %s was written by a write that holds no lock of the eval", write.version.Version, write.version.Series, evalID)
			}
			if write.relist && !answer.Definition.Computed() {
				repos, err := relistDays(ctx, w.Tx, answer.Definition, stored)
				if err != nil {
					return err
				}
				relisted = append(relisted, repos...)
			}
			series[stored.Series], answer.Wrote = stored, true
		}
		answer.Series = sortedSeries(series)
		if found && !answer.Definition.Computed() && foldKeyOf(Entry{Definition: answer.Definition, Series: answer.Series}) != key {
			return markMovedKey(ctx, w.Tx, answer.Definition, relisted)
		}
		return nil
	})
	if err != nil {
		return Registered{}, err
	}
	return answer, nil
}

// seriesWrite is one version of a series a register write stores, and whether it lists or unlists
// a series the eval held (relistDays).
type seriesWrite struct {
	version Series
	relist  bool
}

// versionField is one field of a version whose value grows with what a write sends, by the name a
// body spells it with, and its value.
type versionField struct {
	name  string
	value any
}

// versionSize is the bytes version, a Definition or a Series, takes as JSON encodes it, as an
// answer that holds it writes it, and where that is past MaxVersionBytes the field of fields whose
// value takes the most bytes as JSON; "" where the version fits.
func versionSize(version any, fields []versionField) (int, string, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return 0, "", fmt.Errorf("encode a version: %w", err)
	}
	if len(encoded) <= MaxVersionBytes {
		return len(encoded), "", nil
	}
	largest, most := "", -1
	for _, field := range fields {
		value, err := json.Marshal(field.value)
		if err != nil {
			return 0, "", fmt.Errorf("encode the field %s of a version: %w", field.name, err)
		}
		if len(value) > most {
			largest, most = field.name, len(value)
		}
	}
	return len(encoded), largest, nil
}

// definitionTooLarge refuses definition, a version of an eval's definition a write would store,
// as it would be stored (its version number and the transaction's time set), where it takes more
// than MaxVersionBytes as JSON (CodeVersionTooLarge): naming its largest field as a body spells it,
// of every text and list a body sets and `actor`, the writer the version records (written_by),
// which a bearer caller names in its body. nil where it fits.
func definitionTooLarge(definition Definition) error {
	size, field, err := versionSize(definition, []versionField{
		{"name", definition.Name}, {"question", definition.Question}, {"number_rule", definition.NumberRule},
		{"decision", definition.Decision}, {"failure_modes", definition.FailureModes}, {"unit", definition.Unit},
		{"doc_block", definition.DocBlock}, {"target_judges", definition.TargetJudges}, {"owner_role", definition.OwnerRole},
		{"person", definition.Person}, {"issue_key", definition.IssueKey}, {"replaced_by", definition.ReplacedBy},
		{"links", definition.Links}, {"actor", definition.WrittenBy},
	})
	if err != nil || field == "" {
		return err
	}
	return fieldRefusal(CodeVersionTooLarge, field, "%s", versionTooLargeReason(fmt.Sprintf("version %d of %s", definition.Version, definition.EvalID), size, field))
}

// seriesTooLarge refuses series, a version of a series a register write would store, as
// definitionTooLarge refuses a definition's: at is what each of its fields' names starts with as
// the body spells them, `series[3].`.
func seriesTooLarge(series Series, at string) error {
	size, field, err := seriesVersionSize(series, at)
	if err != nil || field == "" {
		return err
	}
	return fieldRefusal(CodeVersionTooLarge, field, "%s", versionTooLargeReason(seriesVersionName(series), size, field))
}

// seriesVersionSize is versionSize of series, each of its fields named as the body spells it after
// at: its name, its unit and its links.
func seriesVersionSize(series Series, at string) (int, string, error) {
	return versionSize(series, []versionField{{at + "series", series.Series}, {at + "unit", series.Unit}, {at + "links", series.Links}})
}

// seriesVersionName names series' version in a refusal.
func seriesVersionName(series Series) string {
	return fmt.Sprintf("version %d of series %q of %s", series.Version, series.Series, series.EvalID)
}

// versionTooLargeReason is why a write is refused that would store version, a version of size
// bytes as JSON whose largest field is field.
func versionTooLargeReason(version string, size int, field string) string {
	return fmt.Sprintf("%s would take %d bytes as JSON, and a version takes at most %d, so that every answer that holds it stays within what one statement keeps; %s takes the most of it",
		version, size, MaxVersionBytes, field)
}

// rankedByMinN reports whether a read ranks the series series of definition's eval by its min_n:
// the job series of the catalogue's pr-ci, whose jobs the items route sorted by median and the
// Pipeline rank among the slowest (SlowestJobsMinN, PipelineMinN). No read of any other
// series, a logged eval's included, reads a min_n, so a register write of one elsewhere would be
// stored and then ignored (R5) and is refused instead.
func rankedByMinN(definition Definition, series string) bool {
	return definition.Computed() && definition.Slug == SlugPRCI && series == SeriesJobs
}

// relistDays writes series' listing into every day row the readings write keeps of it (writer
// readings, the project's row and each repository's), definition being a logged eval: a window
// read folds the listed rows alone (DayWindowIn), so a row stored before a version that listed its
// series would otherwise stay out of every window, and one stored before a version that unlisted
// it in. A row that holds the listing already is left as it is. It answers the repository of each
// row it relisted, "" for the project's own, each once.
//
// It runs under the eval's lock (Register), which every other writer of these rows takes before
// it reads the listing it writes (WriteReadings, fillLoggedZeros), so no row is written with the
// listing this write replaces once it has committed. It locks the rows before it updates them, in
// the byte order of their keys (repository, day), the order every statement that inserts or locks
// one eval's day rows takes them in (writeDays), where the update alone would take them in the
// order its plan reads them.
func relistDays(ctx context.Context, tx pgx.Tx, definition Definition, series Series) ([]string, error) {
	rows, err := tx.Query(ctx, `
		with locked as (
			select repo, day from eval_days
			where project_key = $1 and eval_id = $2 and writer = $5 and series = $3 and listed <> $4
			order by repo collate "C", day
			for update
		), relisted as (
			update eval_days d set listed = $4, updated_at = now()
			from locked
			where d.project_key = $1 and d.eval_id = $2 and d.writer = $5 and d.series = $3
			  and d.repo = locked.repo and d.day = locked.day
			returning d.repo
		)
		select distinct repo from relisted
	`, definition.ProjectKey, definition.EvalID, series.Series, series.Listed, writerReadings)
	if err != nil {
		return nil, fmt.Errorf("list the day rows of series %s of %s as %t: %w", series.Series, definition.EvalID, series.Listed, err)
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the day rows of series %s of %s as %t: %w", series.Series, definition.EvalID, series.Listed, err)
	}
	return repos, nil
}

// firstDefinition is the definition an eval registered for the first time starts from, before
// the write's own fields: logged, an experiment, active, with no target, owned by caller.
func firstDefinition(project, slug string, caller model.Actor) Definition {
	role, person := ownerOf(caller)
	return Definition{
		EvalID:       EvalID(project, slug),
		ProjectKey:   project,
		Slug:         slug,
		FailureModes: []FailureMode{},
		TargetJudges: []string{},
		OwnerRole:    role,
		Person:       person,
		Kind:         KindExperiment,
		Source:       SourceLogged,
		State:        StateActive,
		Links:        []Link{},
	}
}

// firstSeries is version 1 of a series registered for the first time, before a register write's
// own fields: the eval's headline and primary series when it is the eval's first, a control
// series otherwise, folded by its last value.
func firstSeries(evalID, name string, first, listed bool, unit string) Series {
	role := RoleControl
	if first {
		role = RoleHeadline
	}
	return Series{
		EvalID: evalID, Series: name, Version: 1, Unit: unit, Role: role, IsPrimary: first, Listed: listed,
		Agg: AggLast, Links: []Link{},
	}
}

// ownerOf is the owner a caller makes of an eval it registers: the Legion role its session
// works as, read from the session title its actor carries (legion.ParseSessionTitle), and the
// person its credential names (a person's own login, or the person who owns an agent token).
// Either is "" where the caller carries none.
func ownerOf(caller model.Actor) (role, person string) {
	if caller.Kind == "session" && caller.Origin != nil {
		if title, ok := legion.ParseSessionTitle(caller.Origin.SessionTitle); ok {
			role = title.Role
		}
	}
	switch {
	case caller.Kind == "user":
		person = caller.ID
	case caller.Owner != nil:
		person = *caller.Owner
	}
	return role, person
}

// applyTo writes into next each field the change sets, and records in next's set_by_person every
// field of it a person sets, whether or not the value moves: a write of a target that leaves it
// as it was is still a person's choice of that target, which the seed must not fill afterwards
// (R5, personFields).
func (change Registration) applyTo(next *Definition, now time.Time) {
	change.ComputationChange.applyTo(&next.Computation)
	change.DocBlock.applyTo(&next.DocBlock)
	change.Target.applyTo(&next.Target)
	change.TargetInclusive.applyTo(&next.TargetInclusive)
	change.TargetJudges.applyTo(&next.TargetJudges)
	change.OwnerRole.applyTo(&next.OwnerRole)
	change.Person.applyTo(&next.Person)
	change.IssueKey.applyTo(&next.IssueKey)
	change.State.applyTo(&next.State)
	change.ReplacedBy.applyTo(&next.ReplacedBy)
	if change.Links.Set {
		next.Links = stampLinks(change.Links.Value, next.Links, now)
	}
	next.TargetJudges = nonNil(next.TargetJudges)
	next.SetByPerson = withFields(next.SetByPerson, change.personFields())
}

// applyTo writes into next each field the change sets, and records a person's min_n as
// Registration's applyTo records the definition's fields.
func (change SeriesChange) applyTo(next *Series, now time.Time) {
	change.SeriesComputationChange.applyTo(&next.SeriesComputation, now)
	change.MinN.applyTo(&next.MinN)
	next.SetByPerson = withFields(next.SetByPerson, change.personFields())
}

// stampLinks is links with each RecordedAt the server's: the time a link already in previous
// (the same relation, address and label) was stored, else now.
func stampLinks(links, previous []Link, now time.Time) []Link {
	stamped := make([]Link, len(links))
	for index, link := range links {
		link.RecordedAt = now
		for _, earlier := range previous {
			if earlier.Rel == link.Rel && earlier.Href == link.Href && earlier.Label == link.Label {
				link.RecordedAt = earlier.RecordedAt
				break
			}
		}
		stamped[index] = link
	}
	return stamped
}

// sameDefinition reports whether two versions define the same eval: every field but the
// version's own number, time, writer and way of writing. The record of which fields a person set
// is one of them, so a person's write of a field that moves no value still writes a version,
// which is how that choice is kept (Definition.SetByPerson).
func sameDefinition(a, b Definition) bool {
	return a.EvalID == b.EvalID && a.Computation.same(b.Computation) && a.DocBlock == b.DocBlock &&
		sameTarget(a.Target, b.Target) && a.TargetInclusive == b.TargetInclusive && slices.Equal(a.TargetJudges, b.TargetJudges) &&
		a.OwnerRole == b.OwnerRole && a.Person == b.Person && a.IssueKey == b.IssueKey && a.State == b.State &&
		a.ReplacedBy == b.ReplacedBy && sameLinks(a.Links, b.Links) && slices.Equal(a.SetByPerson, b.SetByPerson)
}

// sameSeries reports whether two versions of a series are the same but for the version's own
// number and time, the record of a person's min_n among the fields compared, as sameDefinition
// compares the definition's.
func sameSeries(a, b Series) bool {
	return a.SeriesComputation.same(b.SeriesComputation) && sameMinN(a.MinN, b.MinN) &&
		slices.Equal(a.SetByPerson, b.SetByPerson)
}

func sameTarget(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameMinN(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameLinks(a, b []Link) bool {
	return slices.EqualFunc(a, b, func(x, y Link) bool {
		return x.Rel == y.Rel && x.Href == y.Href && x.Label == y.Label && x.RecordedAt.Equal(y.RecordedAt)
	})
}

func sortedSeries(byName map[string]Series) []Series {
	all := make([]Series, 0, len(byName))
	for _, series := range byName {
		all = append(all, series)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Series < all[j].Series })
	return all
}

// Kinds are the kinds an eval may be, as eval_versions' check allows them: the kinds a register
// write takes and an eval read keeps with `kind`.
var Kinds = []string{KindDelivery, KindLegion, KindCI, KindExperiment}

// The values each other enumerated field may take, as its column's check allows them.
var (
	states     = []string{StateActive, StateStopped, StateReplaced}
	directions = []string{"", DirectionHigherBetter, DirectionLowerBetter}
	roles      = []string{RoleHeadline, RoleControl}
	aggs       = []string{AggSum, AggRatio, AggQuantile, AggMax, AggLast, AggWeightedMean}
)

// validateRegistration refuses a register write whose fields the tables would refuse, whose keys
// an index entry cannot hold (maxKeyBytes), whose links are not links, that names one series twice,
// or that names more series than an eval holds (MaxEvalSeries), before any transaction opens,
// naming the field it refuses as the body spells it. A series named again and again would store a
// version, and log a change row, for each entry that changes it, and relist its day rows for each
// that moves its listing, in the one transaction that holds the eval's lock: a body of a megabyte
// holds some 30,000 entries.
func validateRegistration(change Registration) error {
	if !slugPattern.MatchString(change.Slug) {
		return fieldRefusal(CodeSlugInvalid, "slug", "slug %q is not lower-case letters, digits and hyphens starting with a letter or a digit", change.Slug)
	}
	if reason := keyTooLong("slug", change.Slug); reason != "" {
		return fieldRefusal(CodeKeyTooLong, "slug", "%s", reason)
	}
	if change.DocText.Set && change.DocBlock.Set {
		return refusal(CodeFieldInvalid, "a register write sets doc_text, whose block it then names in doc_block, or doc_block alone, not both")
	}
	if change.DocText.Set && strings.TrimSpace(change.DocText.Value) == "" {
		return fieldRefusal(CodeDocTextEmpty, "doc_text", "doc_text holds no text, and a method block holds at least one paragraph")
	}
	for _, field := range []struct {
		name  string
		value Field[string]
		set   []string
	}{
		{"kind", change.Kind, Kinds},
		{"state", change.State, states},
		{"direction", change.Direction, directions},
	} {
		if field.value.Set && !slices.Contains(field.set, field.value.Value) {
			return fieldRefusal(CodeFieldInvalid, field.name, "%s %q is not one of %s", field.name, field.value.Value, strings.Join(field.set, ", "))
		}
	}
	// A computed eval is the seeded catalogue's alone, which the seed writes with the function its
	// slug's day rows are built for: a register write that named one would make a logged eval
	// computed with no builder of its rows, reading "no data" from then on, and the register write
	// of a computed eval refuses source logged (computationField), so no write could undo it.
	if change.Source.Set && change.Source.Value != SourceLogged {
		return fieldRefusal(CodeFieldInvalid, "source", "source %q is not %s: a register write registers or changes a logged eval, and only the seeded catalogue makes an eval computed",
			change.Source.Value, SourceLogged)
	}
	if change.TargetJudges.Set {
		for index, judge := range change.TargetJudges.Value {
			series, stat, found := strings.Cut(judge, ".")
			if !found || validSeriesName(series) != nil || stat == "" {
				return fieldRefusal(CodeFieldInvalid, fmt.Sprintf("target_judges[%d]", index), "target judge %q is not <series>.<stat>", judge)
			}
			if taken, delivered := deliveryJudges[change.Slug]; delivered && !slices.Contains(taken, judge) {
				return fieldRefusal(CodeFieldInvalid, fmt.Sprintf("target_judges[%d]", index),
					"target judge %q names no figure the delivery measures and the Pipeline state of %s, which they judge by its target, so they could not judge it: %s takes %s",
					judge, change.Slug, change.Slug, strings.Join(taken, ", "))
			}
		}
	}
	if change.Links.Set {
		if err := validateLinks(change.Links.Value, "links"); err != nil {
			return err
		}
	}
	named := make(map[string]int, len(change.Series))
	for index, series := range change.Series {
		at := fmt.Sprintf("series[%d]", index)
		if err := validSeriesName(series.Series); err != nil {
			return fieldRefusal(CodeSeriesInvalid, at+".series", "series %q: %v", series.Series, err)
		}
		if reason := keyTooLong(at+".series", series.Series); reason != "" {
			return fieldRefusal(CodeKeyTooLong, at+".series", "%s", reason)
		}
		if earlier, twice := named[series.Series]; twice {
			return fieldRefusal(CodeSeriesInvalid, at+".series", "series %q is named at series[%d] already, and a register write names each series once, with every field it sets of it",
				series.Series, earlier)
		}
		named[series.Series] = index
		if series.Role.Set && !slices.Contains(roles, series.Role.Value) {
			return fieldRefusal(CodeFieldInvalid, at+".role", "series %s: role %q is not one of %s", series.Series, series.Role.Value, strings.Join(roles, ", "))
		}
		if series.Agg.Set && !slices.Contains(aggs, series.Agg.Value) {
			return fieldRefusal(CodeFieldInvalid, at+".agg", "series %s: agg %q is not one of %s", series.Series, series.Agg.Value, strings.Join(aggs, ", "))
		}
		if series.MinN.Set && series.MinN.Value != nil {
			// eval_series.min_n is a Postgres integer, so a min_n past math.MaxInt32 is one the
			// insert could not bind, and is refused here, named, as one below 0 is.
			switch minN := *series.MinN.Value; {
			case minN < 0:
				return fieldRefusal(CodeFieldInvalid, at+".min_n", "series %s: min_n %d is below 0", series.Series, minN)
			case minN > math.MaxInt32:
				return fieldRefusal(CodeFieldInvalid, at+".min_n", "series %s: min_n %d is past %d, the largest the store holds", series.Series, minN, math.MaxInt32)
			}
		}
		if series.Links.Set {
			if err := validateLinks(series.Links.Value, at+".links"); err != nil {
				return err
			}
		}
	}
	if index, count, total := seriesPastLimit(nil, len(change.Series), func(index int) string { return change.Series[index].Series }); index >= 0 {
		return fieldRefusal(CodeSeriesLimit, fmt.Sprintf("series[%d].series", index),
			"series %q is series %d of the %d this write names, and an eval holds at most %d series, so that each of its stored rows is written in one statement whose parameters take at most %d bytes",
			change.Series[index].Series, count, total, MaxEvalSeries, statementBytes)
	}
	return nil
}

// targetRange is the targets a unit holds, from least to most, each bound itself among them.
type targetRange struct {
	least, most float64
}

// targetRanges is the range of targets each unit that states one holds, by the unit's name as a
// definition spells it (Definition.Unit): a ratio's from 0 to 1; a count's (the catalogue's
// issues, deploys, deploys/day and rounds), a duration's (minutes, hours, seconds, milliseconds)
// and a size's (bytes) 0 or more, since no reading in any of them is below 0. A target outside
// its unit's range judges every reading alike, so no reading could meet or miss it: -3 deploys a
// day is met by every day, and a share of 7 by none. No other unit states a range: a logged eval's
// unit is the writer's own words (`lines`, `usd`, `score`), or none.
var targetRanges = map[string]targetRange{
	unitRatio:         {least: 0, most: 1},
	"count":           {least: 0, most: math.Inf(1)},
	unitIssues:        {least: 0, most: math.Inf(1)},
	unitDeploys:       {least: 0, most: math.Inf(1)},
	unitDeploysPerDay: {least: 0, most: math.Inf(1)},
	unitRounds:        {least: 0, most: math.Inf(1)},
	unitMinutes:       {least: 0, most: math.Inf(1)},
	unitHours:         {least: 0, most: math.Inf(1)},
	"seconds":         {least: 0, most: math.Inf(1)},
	"milliseconds":    {least: 0, most: math.Inf(1)},
	"bytes":           {least: 0, most: math.Inf(1)},
}

// CheckTarget refuses target, the target a version of an eval whose unit is unit will hold, where
// no reading of the eval could meet or miss it: a number that is not finite (NaN, an infinity,
// which no JSON answer writes), or one outside the range unit holds (targetRanges), 400
// EVAL_FIELD_INVALID naming target (CodeFieldInvalid). nil where the target is off (nil), within
// the range, or any finite number of a unit that states no range. The range is read from the unit
// alone, never from the eval's slug, so a logged eval in ratio holds to it as deploy-run-success
// does. It is the one statement of the rule: every write of a target calls it on the version it
// will store, the register write (Store.Register) and any route that sets an eval's target.
func CheckTarget(unit string, target *float64) error {
	if target == nil {
		return nil
	}
	value := *target
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fieldRefusal(CodeFieldInvalid, fieldTarget, "target %v is no finite number, so no reading could meet or miss it", value)
	}
	bounds, ranged := targetRanges[unit]
	switch {
	case !ranged:
	case value < bounds.least:
		return fieldRefusal(CodeFieldInvalid, fieldTarget, "target %v is below %v, the least a target in %s takes: no reading in %s is below %v, so the target would judge every reading alike",
			value, bounds.least, unit, unit, bounds.least)
	case value > bounds.most:
		return fieldRefusal(CodeFieldInvalid, fieldTarget, "target %v is past %v, the most a target in %s takes: no reading in %s is past %v, so the target would judge every reading alike",
			value, bounds.most, unit, unit, bounds.most)
	}
	return nil
}

// computationField is the first field change names that the computation of a computed eval
// defines, as the body spells it, or "" where it names none (R5). On a computed eval a person sets
// how the number is judged and who answers for it: the target, target_inclusive and
// target_judges; the min_n of pr-ci's job series (rankedByMinN); owner_role, person, issue_key,
// links, state and replaced_by; and the method, through doc_text or doc_block. The catalogue
// states the rest from the code, and the seed writes it again whenever a release changes it: every
// field of the definition's Computation and of each series' SeriesComputation, and the series
// themselves, which the day builders write. series is the eval's series by name.
func computationField(change Registration, series map[string]Series) string {
	if name := change.ComputationChange.named(); name != "" {
		return name
	}
	for index, one := range change.Series {
		at := fmt.Sprintf("series[%d]", index)
		if _, held := series[one.Series]; !held {
			return at + ".series"
		}
		if name := one.SeriesComputationChange.named(); name != "" {
			return at + "." + name
		}
	}
	return ""
}

// validSeriesName refuses a series name that is empty or holds whitespace, a control character
// or a `.`: a target judge is `<series>.<stat>`, which a `.` in the name would make ambiguous.
func validSeriesName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a series needs a name")
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("a series name holds no whitespace or control character")
	case strings.Contains(name, "."):
		return fmt.Errorf("a series name holds no `.`, which separates a target judge's series from its statistic")
	}
	return nil
}

// seriesPastLimit reads a write's n series names (name(i) the i-th) against held, the eval's
// series by name: the index of the first name that would make the eval hold more than
// MaxEvalSeries series, which series of the eval it would be, and how many the eval would hold
// after the write; index is -1 where no name would. A name counts once however many times the write
// names it, and a series the eval holds counts listed or not.
func seriesPastLimit(held map[string]Series, n int, name func(int) string) (index, count, total int) {
	if len(held)+n <= MaxEvalSeries {
		return -1, 0, 0
	}
	index, total = -1, len(held)
	added := map[string]bool{}
	for at := range n {
		if _, has := held[name(at)]; has || added[name(at)] {
			continue
		}
		added[name(at)] = true
		if total++; total > MaxEvalSeries && index < 0 {
			index, count = at, total
		}
	}
	return index, count, total
}

// seriesLimitReason is why a write is refused that would make series name the count-th series of
// the eval evalID, past MaxEvalSeries, and leave it holding total series.
func seriesLimitReason(evalID, name string, count, total int) string {
	return fmt.Sprintf("series %q would be series %d of %s, and the write would leave it holding %d where an eval holds at most %d series, so that each of its stored rows is written in one statement whose parameters take at most %d bytes",
		name, count, evalID, total, MaxEvalSeries, statementBytes)
}

// validateLinks refuses the first link of links that carries no relation or whose href is not an
// absolute http or https URL (linkProblem). where is the list as the body spells it (`links`,
// `series[0].links`), which names the refused field.
func validateLinks(links []Link, where string) error {
	for index, link := range links {
		if code, reason := linkProblem(link.Rel, link.Href); code != "" {
			at := fmt.Sprintf("%s[%d]", where, index)
			return fieldRefusal(code, at+"."+linkField(code), "%s: %s", at, reason)
		}
	}
	return nil
}

// linkProblem is the code and reason that refuse a link with relation rel and address href, or
// "" for a link the registry stores: one with a relation and an absolute URL whose scheme is http
// or https (R9), since a page draws the link for a person to click. The address is only parsed:
// nothing reads its far side.
func linkProblem(rel, href string) (string, string) {
	if strings.TrimSpace(rel) == "" {
		return CodeLinkRelMissing, "a link carries its relation in rel"
	}
	// url.Parse writes the scheme in lower case, so HTTPS: is https.
	parsed, err := url.Parse(href)
	switch {
	case href == "" || err != nil || parsed.Scheme == "":
		return CodeLinkHrefNotAbsolute, fmt.Sprintf("href %q is not an absolute URL with a scheme and a host", href)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return CodeLinkHrefNotHTTP, fmt.Sprintf("href %q is a %s: URL, and a link's href is an http or https URL", href, parsed.Scheme)
	case parsed.Host == "":
		return CodeLinkHrefNotAbsolute, fmt.Sprintf("href %q is not an absolute URL with a scheme and a host", href)
	}
	return "", ""
}

// linkField is the field of a link that the refusal code names: its rel, or its href.
func linkField(code string) string {
	if code == CodeLinkRelMissing {
		return "rel"
	}
	return "href"
}
