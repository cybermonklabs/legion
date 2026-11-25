package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The registry's tests (contracts.go) build their registries through buildContractRegistry, the
// registry's own builder, from two test entries and never from the entries this package
// registers, so they hold the same at every count of entries: A, made later, and B, made
// earlier, so that the order of their ids and the order a stamp gives them can disagree, which is
// what tells the two orders apart. A test that serves through the shaping installs its registry
// in place of the package's own for that test alone (installContractRegistry).

// laterTestContract and earlierTestContract are the ids of A and B, the UTC seconds each was made
// as an entry's id is (scripts/daemon-contract-entry.sh), both later than any entry of this tree
// can be. strayTestContract is an id of the same form that no test registry registers.
const (
	laterTestContract   = 20991231235959
	earlierTestContract = 20990101000000
	strayTestContract   = 20990606060606
)

// laterEntry is A: one band, which takes `claimsToRefresh` off a caller whose strip is below A's
// number.
func laterEntry() contractEntry {
	return contractEntry{id: laterTestContract, slug: "later", bands: []stateNarrowing{
		{contract: laterTestContract, state: withoutClaimsToRefresh, dropped: "claimsToRefresh, the later test entry's key"},
	}}
}

// earlierEntry is B: one band, which takes `wakesWaiting` off a caller whose strip is below B's
// number.
func earlierEntry() contractEntry {
	return contractEntry{id: earlierTestContract, slug: "earlier", bands: []stateNarrowing{
		{contract: earlierTestContract, state: withoutWakesWaiting, dropped: "wakesWaiting, the earlier test entry's key"},
	}}
}

// stampOf is the stamp that names ids in that order: contractOrderForm, then the ids
// comma-separated.
func stampOf(ids ...int) string {
	words := make([]string, len(ids))
	for i, id := range ids {
		words[i] = strconv.Itoa(id)
	}
	return contractOrderForm + strings.Join(words, ",")
}

// testRegistry is the registry of entries under stamp, and fails the test where the builder
// refuses them.
func testRegistry(t *testing.T, stamp string, entries ...contractEntry) *contractRegistry {
	t.Helper()
	built, err := buildContractRegistry(entries, stamp)
	if err != nil {
		t.Fatalf("build a registry of %d test entries under the stamp %q: %v", len(entries), stamp, err)
	}
	return built
}

// installContractRegistry puts built in place of the package's own registry until the test ends,
// so that every accessor of contracts.go, and every line of the shaping that reads one, reads
// built; the registry in place before is put back after. No test of this package calls
// t.Parallel, so no other test reads the registry while one is installed.
func installContractRegistry(t *testing.T, built *contractRegistry) {
	t.Helper()
	before := registry()
	currentRegistry.Store(built)
	t.Cleanup(func() { currentRegistry.Store(before) })
}

// wantNumbered fails the test unless the installed registry holds the entries of want, numbered
// as want numbers them and listed in want's order, with this build's own contract L plus their
// count and SupportedContracts the frozen list followed by each of their numbers.
func wantNumbered(t *testing.T, want ...NumberedEntry) {
	t.Helper()
	if got := NumberedEntries(); !slices.Equal(got, want) {
		t.Errorf("the build's entries are %+v, want %+v", got, want)
	}
	for _, entry := range want {
		if got := entryNumber(entry.ID); got != entry.Number {
			t.Errorf("entryNumber(%d) = %d, want %d", entry.ID, got, entry.Number)
		}
	}
	version := LastNumberedContract + len(want)
	if got := DaemonAPIVersion(); got != version {
		t.Errorf("DaemonAPIVersion() = %d, want L (%d) plus %d entries, %d", got, LastNumberedContract, len(want), version)
	}
	supported := slices.Clone(frozenContracts)
	for number := LastNumberedContract + 1; number <= version; number++ {
		supported = append(supported, number)
	}
	if got := SupportedContracts(); !slices.Equal(got, supported) {
		t.Errorf("SupportedContracts() = %v, want the frozen list and then L+1 to %d: %v", got, version, supported)
	}
}

// A new entry's number is L plus its rank in the build's order, and a build with no stamp ranks
// its entries by their ids, whatever order they registered in: B, made earlier, is L+1, A is
// L+2, and the build's own contract is L+2. An id the registry holds no entry for is numbered
// above every contract the build serves, which is where code naming an entry that this registry
// lacks finds it.
func TestAnEntrysNumberIsLPlusItsRank(t *testing.T) {
	for _, registered := range [][]contractEntry{{laterEntry(), earlierEntry()}, {earlierEntry(), laterEntry()}} {
		installContractRegistry(t, testRegistry(t, "", registered...))
		if ContractOrderStamped() {
			t.Error("a build with no stamp reads as stamped")
		}
		wantNumbered(t,
			NumberedEntry{ID: earlierTestContract, Number: LastNumberedContract + 1, Slug: "earlier"},
			NumberedEntry{ID: laterTestContract, Number: LastNumberedContract + 2, Slug: "later"})
	}
	if got, want := entryNumber(strayTestContract), DaemonAPIVersion()+1; got != want {
		t.Errorf("entryNumber of an id the registry holds no entry for = %d, want %d, above this build's own contract", got, want)
	}
}

// A stamped build numbers its entries in the stamp's order and in no other: stamped A then B, A
// is L+1 and B is L+2 though B's id is the earlier, and stamped B then A, the other way round.
func TestAStampNumbersTheEntriesInItsOrder(t *testing.T) {
	installContractRegistry(t, testRegistry(t, stampOf(laterTestContract, earlierTestContract), earlierEntry(), laterEntry()))
	if !ContractOrderStamped() {
		t.Error("a stamped build reads as unstamped")
	}
	wantNumbered(t,
		NumberedEntry{ID: laterTestContract, Number: LastNumberedContract + 1, Slug: "later"},
		NumberedEntry{ID: earlierTestContract, Number: LastNumberedContract + 2, Slug: "earlier"})

	installContractRegistry(t, testRegistry(t, stampOf(earlierTestContract, laterTestContract), laterEntry(), earlierEntry()))
	wantNumbered(t,
		NumberedEntry{ID: earlierTestContract, Number: LastNumberedContract + 1, Slug: "earlier"},
		NumberedEntry{ID: laterTestContract, Number: LastNumberedContract + 2, Slug: "later"})
}

// The stamp `v1:` names no entry and is a stamp all the same: a build of no entries that carries
// it is stamped, and one that carries none is not, which is what `legion version --contracts`
// tells apart (`order stamped`, `order id`). Either declares L as its own contract.
func TestAnEmptyStampIsStamped(t *testing.T) {
	installContractRegistry(t, testRegistry(t, contractOrderForm))
	if !ContractOrderStamped() {
		t.Errorf("a build stamped %q reads as unstamped", contractOrderForm)
	}
	wantNumbered(t)

	installContractRegistry(t, testRegistry(t, ""))
	if ContractOrderStamped() {
		t.Error("a build with no stamp reads as stamped")
	}
	wantNumbered(t)
}

// A stamp names exactly the entries its build registers, or the build is refused, naming each
// difference and nothing else: an entry the stamp leaves out, an id it names that the build
// registers no entry for, an id it names twice. A stamp of another form is refused too, naming
// what it reads. This build's own registry, which the package's other tests read, is built.
func TestAStampOfAnotherSetOfEntriesIsRefused(t *testing.T) {
	later, earlier, stray := strconv.Itoa(laterTestContract), strconv.Itoa(earlierTestContract), strconv.Itoa(strayTestContract)
	for _, tc := range []struct {
		name            string
		stamp           string
		naming, sparing []string
	}{
		{"an entry left out", stampOf(laterTestContract), []string{earlier}, []string{later}},
		{"an id of no entry", stampOf(laterTestContract, earlierTestContract, strayTestContract), []string{stray}, []string{later, earlier}},
		{"one left out and one of no entry", stampOf(laterTestContract, strayTestContract), []string{earlier, stray}, []string{later}},
		{"no entry named", contractOrderForm, []string{later, earlier}, nil},
		{"an id twice", stampOf(laterTestContract, earlierTestContract, laterTestContract), []string{later}, []string{earlier}},
		{"another form", "v2:" + later + "," + earlier, []string{"v2:"}, nil},
		{"no form", later + "," + earlier, []string{later + "," + earlier}, nil},
		{"a word that is no id", contractOrderForm + later + ",," + earlier, []string{`""`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildContractRegistry([]contractEntry{laterEntry(), earlierEntry()}, tc.stamp)
			if err == nil {
				t.Fatalf("a build of A (%s) and B (%s) stamped %q was built, want it refused", later, earlier, tc.stamp)
			}
			for _, named := range tc.naming {
				if !strings.Contains(err.Error(), named) {
					t.Errorf("the refusal %q does not name %s", err, named)
				}
			}
			for _, spared := range tc.sparing {
				if strings.Contains(err.Error(), spared) {
					t.Errorf("the refusal %q names %s, which the stamp names as the build registers it", err, spared)
				}
			}
		})
	}
	if err := ContractRegistryError(); err != nil {
		t.Errorf("this build's own registry is refused: %v", err)
	}
}

// R6: a daemon whose build carries A and then B, in the merge order its stamp gives, serves a
// plugin of the older build that carried A alone at that build's number, L+1, the shape of A and
// not of B: A's key, and not B's. It is served through the state route as every caller is, with
// the number stated in its header. A registry that ignored the stamp and numbered by id would
// give that number to B, and serve the caller B's key in place of A's. A caller at the daemon's
// own number is served both keys, and one at L neither, and each body's mark names the contract
// it was shaped to beside the daemon's own.
func TestACallerOfAnOlderBuildIsServedTheEntriesItsBuildCarries(t *testing.T) {
	installContractRegistry(t, testRegistry(t, stampOf(laterTestContract, earlierTestContract), laterEntry(), earlierEntry()))
	handler := stateServer(t, populatedState(t), nil)
	for _, tc := range []struct {
		contract           int
		carriesA, carriesB bool
	}{
		{LastNumberedContract, false, false},
		{LastNumberedContract + 1, true, false},
		{LastNumberedContract + 2, true, true},
	} {
		body := stateStating(t, handler, "127.0.0.1:40000", tc.contract)
		_, carriesA := claimsToRefreshServed(t, body)
		_, carriesB := wakesServed(t, body)
		if carriesA != tc.carriesA || carriesB != tc.carriesB {
			t.Errorf("a caller at %d is served A's claimsToRefresh: %t and B's wakesWaiting: %t, want %t and %t",
				tc.contract, carriesA, carriesB, tc.carriesA, tc.carriesB)
		}
		var marked struct {
			Contract ContractView `json:"contract"`
		}
		if err := json.Unmarshal(body, &marked); err != nil {
			t.Fatalf("decode the state served at %d: %v", tc.contract, err)
		}
		if want := (ContractView{Daemon: LastNumberedContract + 2, Caller: tc.contract}); marked.Contract != want {
			t.Errorf("the state served at %d is marked %+v, want %+v", tc.contract, marked.Contract, want)
		}
	}
}

// everyTableAnswer is the answer the entry of everyTableEntry adds.
const everyTableAnswer AnswerID = "every-table"

// everyTableEntry is an entry with one row of every table an entry adds to, each row naming the
// entry by its id wherever the row holds a contract.
func everyTableEntry() contractEntry {
	return contractEntry{
		id: laterTestContract, slug: "every-table",
		bands: []stateNarrowing{
			{contract: laterTestContract, state: withoutClaimsToRefresh, dropped: "the every-table entry's band"}},
		valueStrips:     []valueStrip{{state: withoutUnadmittedHeldWords, dropped: "the every-table entry's strip"}},
		standIns:        []standIn{{contract: laterTestContract, state: withMaxFixAttempts}},
		answers:         []Answer{{ID: everyTableAnswer, Routes: []string{"POST /legion/v1/every-table"}, Since: laterTestContract}},
		answerRoutes:    []answerRoutes{{answer: AnswerEmptyObject, routes: []string{"POST /legion/v1/every-table/empty"}}},
		identityAnswers: []AnswerID{everyTableAnswer},
		enumeratedFields: []EnumeratedField{{Answer: AnswerState, KeyPath: issueKeyPath + "everyTable",
			Values: []EnumeratedValue{{Value: "every-table", Since: laterTestContract}}}},
		enumeratedValues: []enumeratedValues{{answer: AnswerState, keyPath: issueKeyPath + "holdReason",
			values: []EnumeratedValue{{Value: "every-table", Since: laterTestContract}}}},
		typedSettings: []TypedSetting{{Key: "every_table", Since: laterTestContract}},
		removedRoutes: map[string]int{"POST /legion/v1/every-table/removed": laterTestContract},
		acceptedByName: []AcceptedField{{Route: "POST /legion/v1/handoff/complete", Field: "everyTable",
			LeftAt: laterTestContract, Ignoring: "the every-table entry's field"}},
	}
}

// fieldAt is the row of fields of answer at keyPath, and false where fields holds none.
func fieldAt(fields []EnumeratedField, answer AnswerID, keyPath string) (EnumeratedField, bool) {
	at := slices.IndexFunc(fields, func(field EnumeratedField) bool { return field.Answer == answer && field.KeyPath == keyPath })
	if at < 0 {
		return EnumeratedField{}, false
	}
	return fields[at], true
}

// answerAt is the row of answers for id, and false where answers holds none.
func answerAt(answers []Answer, id AnswerID) (Answer, bool) {
	at := slices.IndexFunc(answers, func(answer Answer) bool { return answer.ID == id })
	if at < 0 {
		return Answer{}, false
	}
	return answers[at], true
}

// Each table an entry adds to holds a registered entry's row after the build, after every
// frozen row and with the entry's id resolved to its number: a band, a value strip, a stand-in,
// a whole answer, a route added to an older answer, an identity answer, a new enumerated field
// (at the record's key path too, where it is an issue's), a value added to an older field (at
// both its key paths, and in the arrivals its value strip reads), a typed setting (on the state's
// row of Answers too), a removed route and a field accepted by name. The frozen tables are left
// as they were.
func TestEveryTableHoldsTheRowsOfARegisteredEntry(t *testing.T) {
	frozenEmptyRoutes := len(frozenAnswers[slices.IndexFunc(frozenAnswers, func(a Answer) bool { return a.ID == AnswerEmptyObject })].Routes)
	frozenHoldReasons, frozenFields := len(holdReasons), enumeratedFields()
	installContractRegistry(t, testRegistry(t, "", everyTableEntry()))
	number := LastNumberedContract + 1

	bands := stateNarrowings()
	if len(bands) != len(frozenStateNarrowings)+1 {
		t.Fatalf("stateNarrowings() holds %d rows, want the %d frozen and the entry's", len(bands), len(frozenStateNarrowings))
	}
	for place, band := range frozenStateNarrowings {
		if bands[place].contract != band.contract || bands[place].dropped != band.dropped {
			t.Errorf("row %d of stateNarrowings() is {%d %q}, want the frozen row {%d %q}",
				place, bands[place].contract, bands[place].dropped, band.contract, band.dropped)
		}
	}
	if band := bands[len(bands)-1]; band.contract != number || band.dropped != "the every-table entry's band" {
		t.Errorf("the last band is {%d %q}, want the entry's at %d", band.contract, band.dropped, number)
	}
	strips := valueStrips()
	if frozen := len(frozenValueStrips()); len(strips) != frozen+1 || strips[len(strips)-1].dropped != "the every-table entry's strip" {
		t.Errorf("valueStrips() holds %d rows, the last dropping %q, want the %d frozen and then the entry's",
			len(strips), strips[len(strips)-1].dropped, frozen)
	}
	standing := standIns()
	if len(standing) != len(frozenStandIns)+1 || standing[len(standing)-1].contract != number {
		t.Errorf("standIns() holds %d rows, the last at %d, want the %d frozen and then the entry's at %d",
			len(standing), standing[len(standing)-1].contract, len(frozenStandIns), number)
	}

	answers := Answers()
	if added, ok := answerAt(answers, everyTableAnswer); !ok || added.Since != number || !slices.Equal(added.Routes, []string{"POST /legion/v1/every-table"}) {
		t.Errorf("Answers() holds the entry's answer as %+v (held: %t), want it since %d", added, ok, number)
	}
	if empty, _ := answerAt(answers, AnswerEmptyObject); len(empty.Routes) != frozenEmptyRoutes+1 || empty.Routes[len(empty.Routes)-1] != "POST /legion/v1/every-table/empty" {
		t.Errorf("the empty object's routes are %v, want its %d frozen ones and then the entry's", empty.Routes, frozenEmptyRoutes)
	}
	if !identityAnswer(everyTableAnswer) {
		t.Error("the entry's identity answer is no identity answer of the registry")
	}
	body := struct{ Word string }{"every-table"}
	if shaped, _ := shapeAnswer(everyTableAnswer, body, ServingFloor, ServingFloor); shaped != any(body) {
		t.Errorf("shapeAnswer served the entry's identity answer as %v, want the body it was given", shaped)
	}

	fields := EnumeratedFields()
	for place, field := range frozenFields {
		if fields[place].Answer != field.Answer || fields[place].KeyPath != field.KeyPath {
			t.Errorf("row %d of EnumeratedFields() is `%s` of %s, want the frozen row `%s` of %s",
				place, fields[place].KeyPath, fields[place].Answer, field.KeyPath, field.Answer)
		}
	}
	entryValue := []EnumeratedValue{{Value: "every-table", Since: number}}
	for _, at := range []struct {
		answer  AnswerID
		keyPath string
	}{{AnswerState, issueKeyPath + "everyTable"}, {AnswerIssueRecord, "everyTable"}} {
		if field, ok := fieldAt(fields, at.answer, at.keyPath); !ok || !slices.Equal(field.Values, entryValue) {
			t.Errorf("EnumeratedFields() holds `%s` of %s as %+v (held: %t), want %+v", at.keyPath, at.answer, field.Values, ok, entryValue)
		}
	}
	for _, at := range []struct {
		answer  AnswerID
		keyPath string
	}{{AnswerState, issueKeyPath + "holdReason"}, {AnswerIssueRecord, "holdReason"}} {
		field, _ := fieldAt(fields, at.answer, at.keyPath)
		sorted := slices.IsSortedFunc(field.Values, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
		if len(field.Values) != frozenHoldReasons+1 || !slices.Contains(field.Values, entryValue[0]) || !sorted {
			t.Errorf("`%s` of %s holds %+v, want its %d frozen values and the entry's %+v, sorted by value",
				at.keyPath, at.answer, field.Values, frozenHoldReasons, entryValue[0])
		}
	}
	if arrivals := arrivalsAt(issueKeyPath + "holdReason"); !arrivals.heldBack("every-table", number-1) || arrivals.heldBack("every-table", number) {
		t.Errorf("the holdReason strip holds the entry's value back from %d: %t and from %d: %t, want true and false",
			number-1, arrivals.heldBack("every-table", number-1), number, arrivals.heldBack("every-table", number))
	}
	if len(holdReasons) != frozenHoldReasons {
		t.Errorf("the frozen holdReasons holds %d values after the build, want its %d", len(holdReasons), frozenHoldReasons)
	}

	settings, wantSetting := typedSettings(), TypedSetting{Key: "every_table", Since: number}
	if !slices.Contains(settings, wantSetting) {
		t.Errorf("typedSettings() = %+v, want it to hold %+v", settings, wantSetting)
	}
	if state, _ := answerAt(answers, AnswerState); !slices.Equal(state.TypedSettings, settings) {
		t.Errorf("the state's row of Answers() types %+v, want typedSettings() %+v", state.TypedSettings, settings)
	}
	removed := RemovedRoutes()
	if removed["POST /legion/v1/every-table/removed"] != number || removed["POST /legion/v1/children/{op}"] != childRouteRemovedContract {
		t.Errorf("RemovedRoutes() = %v, want the frozen route at %d and the entry's at %d", removed, childRouteRemovedContract, number)
	}
	accepted := AcceptedByName()
	if len(accepted) != len(frozenAcceptedByName)+1 || accepted[len(accepted)-1].Field != "everyTable" || accepted[len(accepted)-1].LeftAt != number {
		t.Errorf("AcceptedByName() = %+v, want the frozen rows and then the entry's, left at %d", accepted, number)
	}

	if empty, _ := answerAt(frozenAnswers, AnswerEmptyObject); len(empty.Routes) != frozenEmptyRoutes {
		t.Errorf("the frozen empty object's routes are %v after the build, want its %d", empty.Routes, frozenEmptyRoutes)
	}
}

// A row of a frozen table holds the number of a numbered entry, at or below L, and never a new
// entry's constant, whose value is an id far above L: a row that names a new entry lives in that
// entry's file (contract_<id>_<slug>.go), where the registry resolves it to the entry's number.
// The frozen tables are SupportedContracts' list, the bands, the stand-ins, the answers, the
// enumerated fields' values (which carry the arrivals the value strips read), the typed settings,
// the removed routes and the fields accepted by name.
func TestNoFrozenRowNamesAnEntry(t *testing.T) {
	atOrBelowL := func(table string, row any, contract int) {
		t.Helper()
		if contract > LastNumberedContract {
			t.Errorf("the frozen row %v of %s holds %d, above L (%d): a row that names a new entry belongs in that entry's file, contract_<id>_<slug>.go",
				row, table, contract, LastNumberedContract)
		}
	}
	for _, contract := range frozenContracts {
		atOrBelowL("SupportedContracts' frozen list", contract, contract)
	}
	for _, band := range frozenStateNarrowings {
		atOrBelowL("stateNarrowings", band.dropped, band.contract)
	}
	for place, row := range frozenStandIns {
		atOrBelowL("the stand-ins", place, row.contract)
	}
	for _, answer := range frozenAnswers {
		atOrBelowL("Answers", answer.ID, answer.Since)
	}
	for _, field := range enumeratedFields() {
		for _, value := range field.Values {
			atOrBelowL("EnumeratedFields", fmt.Sprintf("`%s` of `%s` of %s", value.Value, field.KeyPath, field.Answer), value.Since)
		}
	}
	for _, setting := range frozenTypedSettings {
		atOrBelowL("typedSettings", setting, setting.Since)
		atOrBelowL("typedSettings", setting, setting.Until)
	}
	for route, contract := range frozenRemovedRoutes {
		atOrBelowL("RemovedRoutes", route, contract)
	}
	for _, row := range frozenAcceptedByName {
		atOrBelowL("AcceptedByName", row.Route+" "+row.Field, row.LeftAt)
	}
}

// No two entries share an id, as no two store migrations share a version (embed_test.go): the
// entries this package registers have one id each, and the builder refuses two entries of one
// id, naming both.
func TestNoTwoEntriesShareAnId(t *testing.T) {
	seen := map[int]contractEntry{}
	for _, entry := range registeredEntries {
		if other, taken := seen[entry.id]; taken {
			t.Errorf("%s and %s share the id %d: make the later of them again with scripts/daemon-contract-entry.sh",
				other.fileName(), entry.fileName(), entry.id)
		}
		seen[entry.id] = entry
	}
	again := laterEntry()
	again.slug = "again"
	_, err := buildContractRegistry([]contractEntry{laterEntry(), earlierEntry(), again}, "")
	if err == nil {
		t.Fatalf("a build of %s and %s was built, want it refused", laterEntry().fileName(), again.fileName())
	}
	for _, named := range []string{laterEntry().fileName(), again.fileName()} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
}

// entryFixtureName is the name of one entry fixture: the entry's 14-digit id, then its slug.
var entryFixtureName = regexp.MustCompile(`^[0-9]{14}_.+\.json$`)

// Each entry this package registers has one fixture, `contracts/<id>_<slug>.json` beside the
// records, and each fixture is a registered entry's, so the count of fixtures is the count of
// entries, which is the count a reader with no Go toolchain adds to L
// (`scripts/daemon-contract.sh`, `daemonContract()` of packages/contracts, `legion-pins`). The
// directory holds entry fixtures and nothing else: each member is a regular file whose name is
// an entry fixture's, and a subdirectory, a symlink, a submodule or a file of another name is
// refused by name, as every reader that counts the fixtures refuses it rather than count it. An
// entry's id is 14 digits. A tree with no entry holds no such directory, which counts as no
// fixture. A run with -update writes the fixture of each entry that has none, so it counts
// nothing; the run after it does.
func TestEveryEntryHasOneFixture(t *testing.T) {
	if *updateGolden {
		t.Skip("the entry fixtures are being written; run the suite again without -update")
	}
	dir := goldenPath("contracts")
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		t.Fatalf("%s is a %s, not a directory: it holds the entry fixtures", dir, kindOf(info.Mode()))
	}
	listed, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read %s: %v", dir, err)
	}
	fixtures := map[string]bool{}
	for _, member := range listed {
		path := filepath.Join(dir, member.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("lstat %s: %v", path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s is a %s, which no entry fixture is: the directory holds regular files named <id>_<slug>.json and nothing else", path, kindOf(info.Mode()))
			continue
		}
		if !entryFixtureName.MatchString(member.Name()) {
			t.Errorf("%s is no entry fixture's name (<14-digit id>_<slug>.json): the directory holds entry fixtures and nothing else", path)
			continue
		}
		fixtures[member.Name()] = true
	}
	registered := map[string]bool{}
	for _, entry := range registeredEntries {
		if !entryIDForm.MatchString(strconv.Itoa(entry.id)) {
			t.Errorf("%s has the id %d, which is no entry id: an entry's id is the 14 digits of the UTC second it was made", entry.fileName(), entry.id)
		}
		name := fmt.Sprintf("%d_%s.json", entry.id, entry.slug)
		registered[name] = true
		if !fixtures[name] {
			t.Errorf("%s has no fixture %s in %s: run `go test ./internal/api -update`", entry.fileName(), name, dir)
		}
	}
	for name := range fixtures {
		if !registered[name] {
			t.Errorf("%s in %s is the fixture of no entry this package registers", name, dir)
		}
	}
	if len(fixtures) != len(registeredEntries) {
		t.Errorf("%s holds %d entry fixtures, want one for each of the %d entries this package registers", dir, len(fixtures), len(registeredEntries))
	}
}

// kindOf is what a member of the entry fixture directory that is no regular file is, as a
// refusal names it.
func kindOf(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	default:
		return "file of mode " + mode.String()
	}
}
