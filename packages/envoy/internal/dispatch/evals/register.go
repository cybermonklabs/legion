package evals

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

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
type Registration struct {
	Project string
	Slug    string
	Caller  model.Actor

	Name            Field[string]
	Question        Field[string]
	NumberRule      Field[string]
	Decision        Field[string]
	DocBlock        Field[string]
	FailureModes    Field[[]FailureMode]
	Unit            Field[string]
	Direction       Field[string]
	Target          Field[*float64]
	TargetInclusive Field[bool]
	TargetJudges    Field[[]string]
	OwnerRole       Field[string]
	Person          Field[string]
	IssueKey        Field[string]
	Kind            Field[string]
	Source          Field[string]
	State           Field[string]
	ReplacedBy      Field[string]
	// Links is the eval's whole list of links. Each link's RecordedAt is the server's: a link the
	// newest version already holds (the same relation, address and label) keeps the time it was
	// first stored, and any other is stored at this write's time.
	Links   Field[[]Link]
	DocText Field[string]

	Series []SeriesChange
}

// SeriesChange is one series a register write registers or changes, by name, with each field it
// sets, copied forward as Registration's are.
type SeriesChange struct {
	Series    string
	Unit      Field[string]
	Role      Field[string]
	IsPrimary Field[bool]
	MinN      Field[*int]
	Listed    Field[bool]
	Agg       Field[string]
	Links     Field[[]Link]
}

// Registered is what a register write left: the eval's newest definition, the newest version of
// each of its series (sorted by name), and whether the write stored a new version of the
// definition or of any series. A write that changes no field of the definition stores none of it,
// and one that changes no field of a series stores no version of that series; a write that stores
// neither wrote nothing.
type Registered struct {
	Definition Definition
	Series     []Series
	Wrote      bool
}

// Register writes the next version of one eval's definition, and of the series it names, in one
// transaction, copying forward every field the write does not set (Registration). Two writes of
// one eval at once both land, in turn, as the next two versions: each takes the eval's lock
// (lockEval) before it reads the version it writes after.
func (s *Store) Register(ctx context.Context, change Registration) (Registered, error) {
	if err := validateRegistration(change); err != nil {
		return Registered{}, err
	}
	evalID := EvalID(change.Project, change.Slug)
	var answer Registered
	err := s.inWrite(ctx, change.Project, func(ctx context.Context, w *writeTx) error {
		if err := lockEval(ctx, w.Tx, evalID); err != nil {
			return err
		}
		current, found, err := newestDefinition(ctx, w.Tx, evalID)
		if err != nil {
			return err
		}
		next := current
		if !found {
			next = firstDefinition(change.Project, change.Slug, change.Caller)
		}
		change.applyTo(&next, w.now)
		next.WrittenVia, next.WrittenBy = ViaRegister, change.Caller.Recorded()
		switch {
		case change.DocText.Set:
			held, err := s.holdsMethod(ctx, w, current.DocBlock, change.DocText.Value)
			if err != nil {
				return err
			}
			// A method the eval's section holds already is no change: its block and the
			// definition stand as they are.
			if !held {
				next.DocBlock = uuid.NewString()
				if err := s.writeMethod(ctx, w, next.Slug, current.DocBlock, next.DocBlock, change.DocText.Value, change.Caller); err != nil {
					return err
				}
			}
		case !found && next.DocBlock == "":
			next.DocBlock = uuid.NewString()
			if err := s.writeMethod(ctx, w, next.Slug, "", next.DocBlock, registrationSentence(ViaRegister, change.Caller, w.now), RegistryActor); err != nil {
				return err
			}
		}
		answer.Definition = current
		if !found || !sameDefinition(current, next) {
			next.Version = current.Version + 1
			stored, _, err := insertDefinition(ctx, w.Tx, next, false)
			if err != nil {
				return err
			}
			answer.Definition, answer.Wrote = stored, true
		}
		series, err := currentSeries(ctx, w.Tx, evalID)
		if err != nil {
			return err
		}
		for _, seriesChange := range change.Series {
			existing, has := series[seriesChange.Series]
			nextSeries := existing
			if !has {
				nextSeries = firstSeries(evalID, seriesChange.Series, len(series) == 0, true, "")
			}
			seriesChange.applyTo(&nextSeries, existing.Links, w.now)
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

func (change Registration) applyTo(next *Definition, now time.Time) {
	change.Name.applyTo(&next.Name)
	change.Question.applyTo(&next.Question)
	change.NumberRule.applyTo(&next.NumberRule)
	change.Decision.applyTo(&next.Decision)
	change.DocBlock.applyTo(&next.DocBlock)
	change.FailureModes.applyTo(&next.FailureModes)
	change.Unit.applyTo(&next.Unit)
	change.Direction.applyTo(&next.Direction)
	change.Target.applyTo(&next.Target)
	change.TargetInclusive.applyTo(&next.TargetInclusive)
	change.TargetJudges.applyTo(&next.TargetJudges)
	change.OwnerRole.applyTo(&next.OwnerRole)
	change.Person.applyTo(&next.Person)
	change.IssueKey.applyTo(&next.IssueKey)
	change.Kind.applyTo(&next.Kind)
	change.Source.applyTo(&next.Source)
	change.State.applyTo(&next.State)
	change.ReplacedBy.applyTo(&next.ReplacedBy)
	if change.Links.Set {
		next.Links = stampLinks(change.Links.Value, next.Links, now)
	}
	next.FailureModes = nonNil(next.FailureModes)
	next.TargetJudges = nonNil(next.TargetJudges)
}

func (change SeriesChange) applyTo(next *Series, previous []Link, now time.Time) {
	change.Unit.applyTo(&next.Unit)
	change.Role.applyTo(&next.Role)
	change.IsPrimary.applyTo(&next.IsPrimary)
	change.MinN.applyTo(&next.MinN)
	change.Listed.applyTo(&next.Listed)
	change.Agg.applyTo(&next.Agg)
	if change.Links.Set {
		next.Links = stampLinks(change.Links.Value, previous, now)
	}
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
// version's own number, time, writer and way of writing.
func sameDefinition(a, b Definition) bool {
	return a.EvalID == b.EvalID && a.Name == b.Name && a.Question == b.Question && a.NumberRule == b.NumberRule &&
		a.Decision == b.Decision && a.DocBlock == b.DocBlock && slices.Equal(a.FailureModes, b.FailureModes) &&
		a.Unit == b.Unit && a.Direction == b.Direction && sameTarget(a.Target, b.Target) &&
		a.TargetInclusive == b.TargetInclusive && slices.Equal(a.TargetJudges, b.TargetJudges) &&
		a.OwnerRole == b.OwnerRole && a.Person == b.Person && a.IssueKey == b.IssueKey && a.Kind == b.Kind &&
		a.Source == b.Source && a.State == b.State && a.ReplacedBy == b.ReplacedBy && sameLinks(a.Links, b.Links)
}

// sameSeries reports whether two versions of a series are the same but for the version's own
// number and time.
func sameSeries(a, b Series) bool {
	return a.Unit == b.Unit && a.Role == b.Role && a.IsPrimary == b.IsPrimary && sameMinN(a.MinN, b.MinN) &&
		a.Listed == b.Listed && a.Agg == b.Agg && sameLinks(a.Links, b.Links)
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

// validateRegistration refuses a register write whose fields the tables would refuse, or whose
// links are not links, before any transaction opens.
func validateRegistration(change Registration) error {
	if !slugPattern.MatchString(change.Slug) {
		return refusal(CodeSlugInvalid, "slug %q is not lower-case letters, digits and hyphens starting with a letter or a digit", change.Slug)
	}
	if change.DocText.Set && change.DocBlock.Set {
		return refusal(CodeFieldInvalid, "a register write sets doc_text, whose block it then names in doc_block, or doc_block alone, not both")
	}
	if change.DocText.Set && strings.TrimSpace(change.DocText.Value) == "" {
		return refusal(CodeDocTextEmpty, "doc_text holds no text, and a method block holds at least one paragraph")
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
			return refusal(CodeFieldInvalid, "%s %q is not one of %s", field.name, field.value.Value, strings.Join(field.set, ", "))
		}
	}
	if change.Source.Set && !validSource(change.Source.Value) {
		return refusal(CodeFieldInvalid, "source %q is neither %s nor %s<function>", change.Source.Value, SourceLogged, SourceComputedPrefix)
	}
	if change.TargetJudges.Set {
		for _, judge := range change.TargetJudges.Value {
			series, stat, found := strings.Cut(judge, ".")
			if !found || validSeriesName(series) != nil || stat == "" {
				return refusal(CodeFieldInvalid, "target judge %q is not <series>.<stat>", judge)
			}
		}
	}
	if change.Links.Set {
		if err := validateLinks(change.Links.Value, "links"); err != nil {
			return err
		}
	}
	for _, series := range change.Series {
		if err := validSeriesName(series.Series); err != nil {
			return refusal(CodeSeriesInvalid, "series %q: %v", series.Series, err)
		}
		if series.Role.Set && !slices.Contains(roles, series.Role.Value) {
			return refusal(CodeFieldInvalid, "series %s: role %q is not one of %s", series.Series, series.Role.Value, strings.Join(roles, ", "))
		}
		if series.Agg.Set && !slices.Contains(aggs, series.Agg.Value) {
			return refusal(CodeFieldInvalid, "series %s: agg %q is not one of %s", series.Series, series.Agg.Value, strings.Join(aggs, ", "))
		}
		if series.MinN.Set && series.MinN.Value != nil && *series.MinN.Value < 0 {
			return refusal(CodeFieldInvalid, "series %s: min_n %d is below 0", series.Series, *series.MinN.Value)
		}
		if series.Links.Set {
			if err := validateLinks(series.Links.Value, "series "+series.Series+" links"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validSource reports whether source is one eval_versions' check allows: logged, or computed:
// followed by the function's name, which holds no whitespace.
func validSource(source string) bool {
	if source == SourceLogged {
		return true
	}
	function, computed := strings.CutPrefix(source, SourceComputedPrefix)
	return computed && function != "" && !strings.ContainsFunc(function, unicode.IsSpace)
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
// absolute URL with a scheme and a host. where names the list in the refusal.
func validateLinks(links []Link, where string) error {
	for index, link := range links {
		if code, reason := linkProblem(link.Rel, link.Href); code != "" {
			return refusal(code, "%s[%d]: %s", where, index, reason)
		}
	}
	return nil
}

// linkProblem is the code and reason that refuse a link with relation rel and address href, or
// "" for a link the registry stores. The address is only parsed: nothing reads its far side.
func linkProblem(rel, href string) (string, string) {
	if strings.TrimSpace(rel) == "" {
		return CodeLinkRelMissing, "a link carries its relation in rel"
	}
	parsed, err := url.Parse(href)
	if href == "" || err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return CodeLinkHrefNotAbsolute, fmt.Sprintf("href %q is not an absolute URL with a scheme and a host", href)
	}
	return "", ""
}
