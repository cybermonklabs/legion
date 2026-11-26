package api

// This file is the registry of the contract's entries, the one place a contract number above
// LastNumberedContract is made. Each change to the contract after the numbered entries of
// version.go is an entry: one file of this package, `contract_<id>_<slug>.go`, which
// `scripts/daemon-contract-entry.sh <slug>` writes with the entry's text, its id and the rows it
// adds to the tables the daemon serves from, and one fixture,
// `packages/contracts/fixtures/daemon-api/contracts/<id>_<slug>.json`, which `go test
// ./internal/api -update` writes. No file holds an entry's number: each build gives it one, from
// the order of its entries, and derives every table from the frozen rows of the numbered entries
// and the rows of its entries in that order.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// LastNumberedContract is L, the last contract numbered by hand: the greatest numbered entry of
// version.go, and the greatest number of
// `packages/contracts/fixtures/daemon-api/released-contracts.json` (TestReleasedContractsGolden).
// It never moves. Each number at or below it names the numbered entry version.go gives it, or no
// entry, for good; each number above it is a new entry's, which every build gives it again.
const LastNumberedContract = 275

// contractOrder is the stamp: the ids of this build's entries in the order main's first-parent
// history added them, written as contractOrderForm and then the ids, comma-separated, with no
// space. The release's and the worker image's builds link it in with
// `-ldflags "-X github.com/sjawhar/legion/daemon/internal/api.contractOrder=<stamp>"`, from what
// `scripts/daemon-contract-order.sh --stamp <rev>` prints for the commit they build. It is empty
// in every other build — `go test`, a developer's own `go build` — which ranks its entries by id.
// The stamp `v1:` alone is a stamped build of no entries.
var contractOrder string

// contractOrderForm is the head of every stamp, which names the one form a stamp's ids are
// written in.
const contractOrderForm = "v1:"

// contractEntry is one entry, which its file hands to registerEntry from its init() function.
// Its id is the untyped constant the file declares, `<lowerCamel(slug)>Contract`, whose value is
// the UTC second the entry was made as 14 digits, and its slug the words of its file's name.
//
// Each other field is rows the entry adds to one table, which the registry appends to that
// table's frozen rows in the build's order. A row names the entry by its constant wherever the
// row holds a contract — a band's or a stand-in's contract, an Answer's Since, an
// EnumeratedField's Until, an EnumeratedValue's Since, a TypedSetting's Since and Until, a
// RemovedRoutes value, an AcceptedField's LeftAt — and the registry resolves each such value to
// the entry's number, so a row moves verbatim from a frozen table into an entry's file.
type contractEntry struct {
	id   int
	slug string
	// bands are rows of stateNarrowings: the keys the entry adds to the state, taken off a caller
	// whose strip is below it.
	bands []stateNarrowing
	// valueStrips are rows of valueStrips: a field's values the entry adds, held back from a
	// caller whose restore is below their arrival (fieldArrivals.heldBack).
	valueStrips []valueStrip
	// standIns are rows of standIns: the keys the entry removes from the state, written back for
	// a caller whose restore is below it.
	standIns []standIn
	// answers are whole rows of Answers, the answers the entry adds; answerRoutes are routes it
	// adds to an answer older than it; identityAnswers are answers shapeAnswer serves the same to
	// every contract (identityAnswer), each an answer no case of shapeAnswer names.
	answers         []Answer
	answerRoutes    []answerRoutes
	identityAnswers []AnswerID
	// enumeratedFields are rows of EnumeratedFields, the enumerated fields the entry adds: a row of
	// the state under issueKeyPath is a row of the record answer too, as a frozen row is
	// (withIssueRecordRows). enumeratedValues are values it adds to a field older than it.
	enumeratedFields []EnumeratedField
	enumeratedValues []enumeratedValues
	// typedSettings, removedRoutes and acceptedByName are rows of typedSettings, RemovedRoutes and
	// AcceptedByName.
	typedSettings  []TypedSetting
	removedRoutes  map[string]int
	acceptedByName []AcceptedField
}

// answerRoutes is the routes an entry adds to answer, a row of Answers older than the entry: one
// more route that answers that schema.
type answerRoutes struct {
	answer AnswerID
	routes []string
}

// enumeratedValues is the values an entry adds to the field of answer at keyPath, a row of
// EnumeratedFields older than the entry. A field of the state under issueKeyPath takes them at its
// record's key path too, since the two are one field (withIssueRecordRows).
type enumeratedValues struct {
	answer  AnswerID
	keyPath string
	values  []EnumeratedValue
}

// fileName is the name of the entry's file, by which every refusal names the entry.
func (e contractEntry) fileName() string {
	return fmt.Sprintf("contract_%d_%s.go", e.id, e.slug)
}

// entryIDForm and entrySlugForm are the forms of an entry's id, the 14 digits `YYYYMMDDHHMMSS`
// of the UTC second its author made it, and of its slug, lowercase words joined by hyphens.
var (
	entryIDForm   = regexp.MustCompile(`^\d{14}$`)
	entrySlugForm = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// registeredEntries is every entry this package registers, in the order the init() functions of
// their files ran, which is no order the registry reads.
var registeredEntries []contractEntry

// registryBuilt is set when the registry is built, after which no entry can be numbered.
var registryBuilt bool

// registerEntry registers entry. Each entry's file calls it from its init() function, and from
// nowhere else: Go runs every package-level initializer of this package before any init()
// function, so a table an initializer built would hold no entry's row, and the registry builds
// every value it derives at its first use instead (registry). An entry registered after that
// would be numbered by no build, so it panics.
func registerEntry(entry contractEntry) {
	if registryBuilt {
		panic(fmt.Sprintf("api: %s registered its entry after the contract registry was built: an entry registers from its file's init() alone, and no package-level initializer reads a value the registry derives", entry.fileName()))
	}
	registeredEntries = append(registeredEntries, entry)
}

// contractRegistry is every value a build derives from its entries: each entry's number, this
// build's own contract, SupportedContracts and every table an entry adds to, each table its
// frozen rows and then the entries' rows in number order.
//
// The invariant this replaces a test's assertion with (R4 of dispatch://INFRA-1569): a new
// entry's number is L plus its rank in the build's order, so no source line holds it and no test
// can pin it. A test may compare one entry's number with another's, or bound it by
// DaemonAPIVersion(). No test requires an entry, or the number of entries, to be the newest.
//
// The registry compares no contract number with another: it numbers by rank and resolves by
// lookup, so every comparison of a contract stays in the code that serves it.
type contractRegistry struct {
	// stamped says the build was stamped (contractOrder), and entries is each entry in the
	// build's order — the stamp's, else the ids' — with its number.
	stamped bool
	entries []NumberedEntry
	// numbers is each entry's number by its id, version this build's own contract, and supported
	// SupportedContracts.
	numbers   map[int]int
	version   int
	supported []int
	// The tables, each its frozen rows and then the entries' rows, every entry's id resolved to
	// its number: stateNarrowings, valueStrips, standIns, Answers (with the routes entries add to
	// an older answer, and the state's row typing typedSettings), the identity answers of
	// shapeAnswer, EnumeratedFields (with the values entries add to an older field), the
	// arrivals of each of the state's enumerated fields by key path (arrivalsAt), typedSettings,
	// RemovedRoutes and AcceptedByName.
	stateNarrowings  []stateNarrowing
	valueStrips      []valueStrip
	standIns         []standIn
	answers          []Answer
	identityAnswers  map[AnswerID]bool
	enumeratedFields []EnumeratedField
	arrivals         map[string]fieldArrivals
	typedSettings    []TypedSetting
	removedRoutes    map[string]int
	acceptedByName   []AcceptedField
}

// NumberedEntry is one entry as this build numbers it: its id, its number and its slug, one line
// of `legion version --contracts`.
type NumberedEntry struct {
	ID     int
	Number int
	Slug   string
}

// buildContractRegistry is the registry of entries under stamp, which reads nothing else but
// the frozen tables: each entry numbered L plus its rank in the build's order (entryOrder), and
// each table its frozen rows and then the entries' rows in that order. It refuses an entry whose
// id or slug is of no entry's form, two entries of one id, naming both, a stamp that does not
// name exactly entries, naming each difference, and a row that adds to an answer or a field the
// tables do not hold.
func buildContractRegistry(entries []contractEntry, stamp string) (*contractRegistry, error) {
	byID := make(map[int]contractEntry, len(entries))
	for _, entry := range entries {
		if !entryIDForm.MatchString(strconv.Itoa(entry.id)) || !entrySlugForm.MatchString(entry.slug) {
			return nil, fmt.Errorf("%s is no entry's name: an entry's id is the 14 digits of the UTC second it was made and its slug lowercase words joined by hyphens, as scripts/daemon-contract-entry.sh writes them",
				entry.fileName())
		}
		if other, taken := byID[entry.id]; taken {
			return nil, fmt.Errorf("two contract entries share the id %d, %s and %s: make the later of them again with scripts/daemon-contract-entry.sh",
				entry.id, other.fileName(), entry.fileName())
		}
		byID[entry.id] = entry
	}
	order, stamped, err := entryOrder(byID, stamp)
	if err != nil {
		return nil, err
	}
	built := &contractRegistry{
		stamped:   stamped,
		entries:   make([]NumberedEntry, 0, len(order)),
		numbers:   make(map[int]int, len(order)),
		version:   LastNumberedContract + len(order),
		supported: slices.Clone(frozenContracts),
	}
	ordered := make([]contractEntry, 0, len(order))
	for rank, id := range order {
		number := LastNumberedContract + rank + 1
		built.numbers[id] = number
		built.entries = append(built.entries, NumberedEntry{ID: id, Number: number, Slug: byID[id].slug})
		built.supported = append(built.supported, number)
		ordered = append(ordered, byID[id])
	}
	if err := built.deriveTables(ordered); err != nil {
		return nil, err
	}
	return built, nil
}

// entryOrder is the ids of entries in the build's order, and whether a stamp gave it: the
// stamp's order where the stamp names exactly these entries, and the ids' own order where there
// is no stamp. A stamp of another form is refused, naming what it reads, and one of another set
// of entries is refused, naming each id it names that no entry has, each id it names twice and
// each entry it leaves out, and no entry it names as the build registers it.
func entryOrder(entries map[int]contractEntry, stamp string) ([]int, bool, error) {
	if stamp == "" {
		return slices.Sorted(maps.Keys(entries)), false, nil
	}
	listed, ofTheForm := strings.CutPrefix(stamp, contractOrderForm)
	if !ofTheForm {
		return nil, true, fmt.Errorf("the contract order stamp %q does not start %q, the one form this build reads", stamp, contractOrderForm)
	}
	var order []int
	var unregistered, twice []string
	named := map[int]bool{}
	if listed != "" {
		for _, word := range strings.Split(listed, ",") {
			if !entryIDForm.MatchString(word) {
				return nil, true, fmt.Errorf("the contract order stamp %q names %q, which is no entry id (14 digits)", stamp, word)
			}
			id, _ := strconv.Atoi(word)
			if named[id] {
				twice = append(twice, word)
				continue
			}
			named[id] = true
			if _, registered := entries[id]; !registered {
				unregistered = append(unregistered, word)
				continue
			}
			order = append(order, id)
		}
	}
	var missing []string
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		if !named[id] {
			missing = append(missing, entries[id].fileName())
		}
	}
	var differences []string
	if len(unregistered) > 0 {
		differences = append(differences, "it names "+strings.Join(unregistered, ", ")+", which this build registers no entry for")
	}
	if len(twice) > 0 {
		differences = append(differences, "it names "+strings.Join(twice, ", ")+" twice")
	}
	if len(missing) > 0 {
		differences = append(differences, "it leaves out "+strings.Join(missing, ", ")+", which this build registers")
	}
	if len(differences) > 0 {
		return nil, true, fmt.Errorf("the contract order stamp does not name exactly this build's entries: %s; stamp the build with what `scripts/daemon-contract-order.sh --stamp` prints for the commit it builds",
			strings.Join(differences, "; "))
	}
	return order, true, nil
}

// deriveTables fills every table of r from the frozen rows and the rows of ordered, the entries
// in number order. Every new row comes in before any route or value an entry adds to an older
// row, so an entry may add to a row of an entry before it, and every contract value of every row
// that is an entry's id is then resolved to that entry's number. The frozen tables are copied
// before anything is written, so they stay as the numbered entries wrote them.
func (r *contractRegistry) deriveTables(ordered []contractEntry) error {
	r.stateNarrowings = slices.Clone(frozenStateNarrowings)
	r.valueStrips = frozenValueStrips()
	r.standIns = slices.Clone(frozenStandIns)
	r.answers = slices.Clone(frozenAnswers)
	r.identityAnswers = make(map[AnswerID]bool, len(frozenIdentityAnswers))
	for _, id := range frozenIdentityAnswers {
		r.identityAnswers[id] = true
	}
	fields := enumeratedFields()
	r.typedSettings = slices.Clone(frozenTypedSettings)
	r.removedRoutes = maps.Clone(frozenRemovedRoutes)
	r.acceptedByName = slices.Clone(frozenAcceptedByName)
	for _, entry := range ordered {
		r.stateNarrowings = append(r.stateNarrowings, entry.bands...)
		r.valueStrips = append(r.valueStrips, entry.valueStrips...)
		r.standIns = append(r.standIns, entry.standIns...)
		r.answers = append(r.answers, entry.answers...)
		for _, id := range entry.identityAnswers {
			r.identityAnswers[id] = true
		}
		fields = append(fields, withIssueRecordRows(entry.enumeratedFields)...)
		r.typedSettings = append(r.typedSettings, entry.typedSettings...)
		maps.Copy(r.removedRoutes, entry.removedRoutes)
		r.acceptedByName = append(r.acceptedByName, entry.acceptedByName...)
	}
	for _, entry := range ordered {
		for _, added := range entry.answerRoutes {
			at := slices.IndexFunc(r.answers, func(answer Answer) bool { return answer.ID == added.answer })
			if at < 0 {
				return fmt.Errorf("%s adds routes to the %s answer, which Answers has no row for", entry.fileName(), added.answer)
			}
			r.answers[at].Routes = append(slices.Clip(r.answers[at].Routes), added.routes...)
		}
		for _, added := range entry.enumeratedValues {
			if !withValuesAdded(fields, added) {
				return fmt.Errorf("%s adds values to `%s` of the %s answer, which EnumeratedFields has no row for", entry.fileName(), added.keyPath, added.answer)
			}
		}
	}

	resolved := func(contract int) int {
		if number, ofAnEntry := r.numbers[contract]; ofAnEntry {
			return number
		}
		return contract
	}
	for place := range r.stateNarrowings {
		r.stateNarrowings[place].contract = resolved(r.stateNarrowings[place].contract)
	}
	for place := range r.standIns {
		r.standIns[place].contract = resolved(r.standIns[place].contract)
	}
	for place := range fields {
		values := slices.Clone(fields[place].Values)
		for at := range values {
			values[at].Since = resolved(values[at].Since)
		}
		fields[place].Values = values
		fields[place].Until = resolved(fields[place].Until)
	}
	for place := range r.typedSettings {
		r.typedSettings[place].Since = resolved(r.typedSettings[place].Since)
		r.typedSettings[place].Until = resolved(r.typedSettings[place].Until)
	}
	for route, contract := range r.removedRoutes {
		r.removedRoutes[route] = resolved(contract)
	}
	for place := range r.acceptedByName {
		r.acceptedByName[place].LeftAt = resolved(r.acceptedByName[place].LeftAt)
	}
	for place := range r.answers {
		r.answers[place].Since = resolved(r.answers[place].Since)
		if r.answers[place].ID == AnswerState {
			r.answers[place].TypedSettings = r.typedSettings
		}
	}

	r.enumeratedFields = fields
	r.arrivals = map[string]fieldArrivals{}
	for _, field := range fields {
		if field.Answer == AnswerState {
			r.arrivals[field.KeyPath] = arrivalsOf(field.Values)
		}
	}
	return nil
}

// withValuesAdded adds added's values to the row of fields it names, and to that row's record
// row where the row is an issue's field of the state, each list sorted by value again as the type
// requires, copying each list before it writes it. It reports whether fields holds the row added
// names.
func withValuesAdded(fields []EnumeratedField, added enumeratedValues) bool {
	recordPath, ofAnIssue := strings.CutPrefix(added.keyPath, issueKeyPath)
	ofAnIssue = ofAnIssue && added.answer == AnswerState
	held := false
	for place, field := range fields {
		atTheRow := field.Answer == added.answer && field.KeyPath == added.keyPath
		atTheRecord := ofAnIssue && field.Answer == AnswerIssueRecord && field.KeyPath == recordPath
		if !atTheRow && !atTheRecord {
			continue
		}
		held = held || atTheRow
		values := append(slices.Clone(field.Values), added.values...)
		slices.SortFunc(values, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
		fields[place].Values = values
	}
	return held
}

// registryOnce builds this build's registry once, registryError is why it could not, and
// currentRegistry is the registry every accessor reads: the one built, or the one a test installed
// in its place (contracts_test.go's installContractRegistry).
var (
	registryOnce    sync.Once
	registryError   error
	currentRegistry atomic.Pointer[contractRegistry]
)

// buildRegistry builds this build's registry from every registered entry and the stamp, once,
// at the first read of any value it derives: after every init() function of this package has
// run, since no package-level initializer reads one (registerEntry).
func buildRegistry() {
	registryOnce.Do(func() {
		registryBuilt = true
		built, err := buildContractRegistry(registeredEntries, contractOrder)
		if err != nil {
			registryError = err
			return
		}
		currentRegistry.Store(built)
	})
}

// registry is the registry every value of this file is read from. A build whose registry could
// not be built panics here, naming why; `legion` refuses such a build before it runs any
// command (ContractRegistryError), so no daemon meets it.
func registry() *contractRegistry {
	buildRegistry()
	built := currentRegistry.Load()
	if built == nil {
		panic("api: " + registryError.Error())
	}
	return built
}

// ContractRegistryError is why this build's registry cannot be built, and nil where it can: a
// stamp that does not name exactly the entries this build registers, naming each difference, or
// two entries of one id, naming both. `legion` reads it before it runs any command, and refuses
// to run a build it is set for.
func ContractRegistryError() error {
	buildRegistry()
	return registryError
}

// DaemonAPIVersion is the contract this daemon speaks with the Legion plugin, over the surfaces
// version.go's header names: L plus the number of entries this build registers. The installed
// plugin declares the number it was built against as `legion.daemonApiVersion` in its packed
// `package.json`, and the boot gate (`internal/daemon/bootgate.go`) refuses to start unless the
// two are equal.
func DaemonAPIVersion() int { return registry().version }

// entryNumber is the number this build gives the entry whose id is id, which code that compares
// a contract with an entry's reads, as `entryNumber(<x>Contract)`. An id the registry holds no
// entry for — the id of an entry of this tree under a registry a test built without it — is
// numbered one above this build's own contract: no caller this build serves is at or above it,
// so the build serves as if the change had not arrived.
func entryNumber(id int) int {
	built := registry()
	if number, ofAnEntry := built.numbers[id]; ofAnEntry {
		return number
	}
	return built.version + 1
}

// NumberedEntries is each entry of this build in the build's order, with its number.
func NumberedEntries() []NumberedEntry { return slices.Clone(registry().entries) }

// ContractOrderStamped says whether this build carries a stamp (contractOrder), which numbers its
// entries in main's merge order, rather than ranking them by id.
func ContractOrderStamped() bool { return registry().stamped }
