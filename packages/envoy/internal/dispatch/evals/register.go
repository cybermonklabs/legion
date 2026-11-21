package evals

import (
	"context"
	"fmt"
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
// write of a min_n no read ranks by (rankedByMinN). A version that lists or unlists a logged
// eval's series lists or unlists that series' stored day rows in the same transaction
// (relistDays). A write that may write the eval's section, one carrying doc_text or the first of
// an eval, takes the document's owner lock before it stores anything (lockDocument) and does the
// section's document work there, in its read phase, queued for the commit after its versions'
// rows (writeMethod).
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
		next := current
		if !found {
			next = firstDefinition(change.Project, change.Slug, change.Caller)
		}
		change.applyTo(&next, w.now)
		next.WrittenVia, next.WrittenBy = ViaRegister, change.Caller.Recorded()
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
				next.DocBlock = uuid.NewString()
				if err := s.writeMethod(ctx, w, document, documentFound, next.Slug, current.DocBlock, next.DocBlock, change.DocText.Value, change.Caller); err != nil {
					return err
				}
			}
		case !found && next.DocBlock == "":
			document, documentFound, err := lockDocument(ctx, w)
			if err != nil {
				return err
			}
			next.DocBlock = uuid.NewString()
			if err := s.writeMethod(ctx, w, document, documentFound, next.Slug, "", next.DocBlock, registrationSentence(ViaRegister, change.Caller, w.now), RegistryActor); err != nil {
				return err
			}
		}
		answer.Definition = current
		if !found || !sameDefinition(current, next) {
			next.Version = current.Version + 1
			stored, err := insertDefinition(ctx, w.Tx, next)
			if err != nil {
				return err
			}
			answer.Definition, answer.Wrote = stored, true
		}
		for _, seriesChange := range change.Series {
			existing, has := series[seriesChange.Series]
			nextSeries := existing
			if !has {
				nextSeries = firstSeries(evalID, seriesChange.Series, len(series) == 0, true, "")
			}
			seriesChange.applyTo(&nextSeries, w.now)
			if has && sameSeries(existing, nextSeries) {
				continue
			}
			nextSeries.Version = existing.Version + 1
			stored, inserted, err := insertSeries(ctx, w.Tx, nextSeries)
			if err != nil {
				return err
			}
			if !inserted {
				return fmt.Errorf("version %d of series %s of %s was written by a write that holds no lock of the eval", nextSeries.Version, nextSeries.Series, evalID)
			}
			if has && existing.Listed != stored.Listed && !answer.Definition.Computed() {
				if err := relistDays(ctx, w.Tx, answer.Definition, stored); err != nil {
					return err
				}
			}
			series[stored.Series], answer.Wrote = stored, true
		}
		answer.Series = sortedSeries(series)
		return nil
	})
	if err != nil {
		return Registered{}, err
	}
	return answer, nil
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
// read folds the listed rows alone (DayWindow), so a row stored before a version that listed its
// series would otherwise stay out of every window, and one stored before a version that unlisted
// it in. A row that holds the listing already is left as it is.
//
// It runs under the eval's lock (Register), which every other writer of these rows takes before
// it reads the listing it writes (WriteReadings, fillLoggedZeros), so no row is written with the
// listing this write replaces once it has committed. It locks the rows before it updates them, in
// the byte order of their keys (repository, day), the order every statement that inserts or locks
// one eval's day rows takes them in (writeDays), where the update alone would take them in the
// order its plan reads them.
func relistDays(ctx context.Context, tx pgx.Tx, definition Definition, series Series) error {
	if _, err := tx.Exec(ctx, `
		with locked as (
			select repo, day from eval_days
			where project_key = $1 and eval_id = $2 and writer = $5 and series = $3 and listed <> $4
			order by repo collate "C", day
			for update
		)
		update eval_days d set listed = $4, updated_at = now()
		from locked
		where d.project_key = $1 and d.eval_id = $2 and d.writer = $5 and d.series = $3
		  and d.repo = locked.repo and d.day = locked.day
	`, definition.ProjectKey, definition.EvalID, series.Series, series.Listed, writerReadings); err != nil {
		return fmt.Errorf("list the day rows of series %s of %s as %t: %w", series.Series, definition.EvalID, series.Listed, err)
	}
	return nil
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
// an index entry cannot hold (maxKeyBytes), or whose links are not links, before any transaction
// opens, naming the field it refuses as the body spells it.
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
	for index, series := range change.Series {
		at := fmt.Sprintf("series[%d]", index)
		if err := validSeriesName(series.Series); err != nil {
			return fieldRefusal(CodeSeriesInvalid, at+".series", "series %q: %v", series.Series, err)
		}
		if reason := keyTooLong(at+".series", series.Series); reason != "" {
			return fieldRefusal(CodeKeyTooLong, at+".series", "%s", reason)
		}
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
