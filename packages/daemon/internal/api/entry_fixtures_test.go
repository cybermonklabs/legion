package api

//ci:reads packages/contracts/fixtures/daemon-api/** buildRecords reads each entry's fixture under contracts/, which holds no file until the first entry
//ci:reads packages/contracts/testdata/entry-fixture-examples/* TestTheCompositionOfTheExampleFixtures reads the examples' base, their entry fixtures and the record they compose

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// updateEntry names the one entry whose fixture `-update-entry=<id>` writes again, as `-update`
// writes the fixture of an entry that has none.
var updateEntry = flag.String("update-entry", "", "write again the fixture of the entry of this id, as -update writes the fixture of an entry that has none")

// entryRequests names a file of what entries change of the requests, which `-update` and
// `-update-entry` write into those entries' fixtures in place of what this build's request types
// read: a JSON object from an entry's id to its routes, in the form of a fixture's `routes`, made
// over the record of the entries before it (requestsWith). One build's types read the same fields
// at every number, so they give the requests of the last entry of the build's order alone: an
// earlier entry this file does not name takes the routes of its committed fixture, and one with no
// fixture is refused (writeEntryFixtures). scripts/daemon-contract-convert.sh writes the file from
// the records a branch made before dispatch://INFRA-1569 kept at each of its numbers.
var entryRequests = flag.String("entry-requests", "", "a file of what each entry, by id, changes of the requests, which -update and -update-entry write in place of what the request types read")

// rewritingFixtures is whether this run writes fixtures, with `-update` or `-update-entry`. The
// cases that read the records skip such a run, which reads the fixtures it is writing.
func rewritingFixtures() bool {
	return *updateGolden || *updateEntry != ""
}

// entryFixture is one entry fixture, `packages/contracts/fixtures/daemon-api/contracts/<id>_<slug>.json`:
// what one entry changes of the contract, with no number in it, since the entry's number is its
// rank in the build's order and moves with the order. The numbered entries' records are frozen
// (contractShapesFixture, requestShapesFixture), and the record of a contract above L is L's with
// the fixture of each entry at or below it applied in the build's order (composeEntries). The same
// form is the TypeScript type `EntryFixture` of `packages/contracts/src/daemon-api-records.ts`,
// whose composition is held with this one to the example fixtures
// (TestTheCompositionOfTheExampleFixtures). The file is the value marshalled with two spaces of
// indent, as every golden of this package is; every list is sorted (by answer, route, key path or
// value), and a key with nothing to say is left out:
//
//	{
//	  "answers": [{                          one element for each answer the entry changes
//	    "answer": "<AnswerID>",
//	    "change": "add" | "remove",          absent where the answer stands before and after
//	    "noBody": true | false,              on "add" the new answer's; else present only where the
//	                                         entry changes it, holding the value after
//	    "addKeyPaths": ["<key path>"],       key paths of the body the entry adds
//	    "removeKeyPaths": ["<key path>"],    and removes
//	    "addValues": {"<key path>": ["<value>"]},     values it adds to the domain at a key path
//	    "removeValues": {"<key path>": ["<value>"]}   and removes from it
//	  }],
//	  "routes": [{                           one element for each route a plugin calls whose request
//	    "route": "METHOD /path",             the entry changes
//	    "change": "add" | "remove",
//	    "addFieldPaths": ["<field path>"],
//	    "removeFieldPaths": ["<field path>"]
//	  }],
//	  "answerRows": [{                       the rows it adds to answers.json, with no since
//	    "id": "<AnswerID>", "routes": ["METHOD /path"], "noBody": false,
//	    "typedSettings": [{"key": "<settings key>"}]
//	  }],
//	  "enumeratedRows": [{                   the rows it adds to enumerated-fields.json, with no since
//	    "answer": "<AnswerID>", "keyPath": "<key path>", "values": ["<value>"]
//	  }]
//	}
//
// An added answer's record is its addKeyPaths and addValues alone, and a removed answer's element
// holds nothing but its change. A row of answerRows of an answer no row before it names is the
// whole row, and arrives at the entry's number; one of an answer a row before it names holds only
// the routes and the typed settings the entry adds to that row, and that row's own noBody. A typed
// setting is its key alone: it is typed from the entry on. A row of enumeratedRows names a field
// and the values the entry adds to it, each arriving at the entry's number; a field no row before
// it declares is a new row. answers.json and enumerated-fields.json hold the numbered entries' rows
// alone, so these two lists are where a reader with no Go toolchain finds an entry's rows.
type entryFixture struct {
	Answers        []answerChange       `json:"answers"`
	Routes         []routeChange        `json:"routes"`
	AnswerRows     []entryAnswerRow     `json:"answerRows"`
	EnumeratedRows []entryEnumeratedRow `json:"enumeratedRows"`
}

// answerChange is what one entry changes of one answer's record (entryFixture).
type answerChange struct {
	Answer         AnswerID            `json:"answer"`
	Change         string              `json:"change,omitempty"`
	NoBody         *bool               `json:"noBody,omitempty"`
	AddKeyPaths    []string            `json:"addKeyPaths,omitempty"`
	RemoveKeyPaths []string            `json:"removeKeyPaths,omitempty"`
	AddValues      map[string][]string `json:"addValues,omitempty"`
	RemoveValues   map[string][]string `json:"removeValues,omitempty"`
}

// routeChange is what one entry changes of one route's request record (entryFixture).
type routeChange struct {
	Route            string   `json:"route"`
	Change           string   `json:"change,omitempty"`
	AddFieldPaths    []string `json:"addFieldPaths,omitempty"`
	RemoveFieldPaths []string `json:"removeFieldPaths,omitempty"`
}

// entryAnswerRow is one row an entry adds to answers.json (entryFixture).
type entryAnswerRow struct {
	ID            AnswerID            `json:"id"`
	Routes        []string            `json:"routes"`
	NoBody        bool                `json:"noBody"`
	TypedSettings []entryTypedSetting `json:"typedSettings,omitempty"`
}

// entryTypedSetting is one settings key an entry's row of answers.json types from the entry on.
type entryTypedSetting struct {
	Key string `json:"key"`
}

// entryEnumeratedRow is one row an entry adds to enumerated-fields.json (entryFixture).
type entryEnumeratedRow struct {
	Answer  AnswerID `json:"answer"`
	KeyPath string   `json:"keyPath"`
	Values  []string `json:"values"`
}

// The words of a change: an answer or a route the entry adds, or one it removes.
const (
	changeAdd    = "add"
	changeRemove = "remove"
)

// contractRecords is everything of one contract an entry fixture changes: the record of each
// answer and of each route at Contract, and the rows of answers.json and enumerated-fields.json
// as they stand there. The example fixtures' base.json and expected.json are written in this form,
// as the TypeScript `ContractRecords` is.
type contractRecords struct {
	Contract         int               `json:"contract"`
	Records          []contractShape   `json:"records"`
	Requests         []requestShape    `json:"requests"`
	Answers          []Answer          `json:"answers"`
	EnumeratedFields []EnumeratedField `json:"enumeratedFields"`
}

// numberedFixture is one entry fixture at its place in the build's order: name is its file's name
// without `.json` (`<id>_<slug>`), number the contract it is.
type numberedFixture struct {
	name    string
	number  int
	fixture entryFixture
}

// answerRecord is one answer's record while the entries are applied to it.
type answerRecord struct {
	noBody   bool
	keyPaths map[string]bool
	domains  map[string]map[string]bool
}

// composeEntries is the record of each entry's number, in the order of entries: base, the record
// of the contract before the first of them, with the fixture of each entry up to that one applied
// in turn. Each operation of a fixture has one condition, read against the record before the
// entry: an added key path, field path, domain value, answer, route or row is absent from it, and
// a removed one present. A fixture that breaks one is refused, naming the entry and the operation,
// because it was written over another set of entries than the ones before it here; so is a fixture
// that says something no operation can mean (an added answer with no noBody, a removed one that
// names a key path, an answer or a route named twice). The TypeScript `composeEntries` is this
// function, and the two are held to one set of example fixtures.
func composeEntries(base contractRecords, entries []numberedFixture) ([]contractRecords, error) {
	answers := make(map[AnswerID]answerRecord, len(base.Records))
	for _, record := range base.Records {
		domains := make(map[string]map[string]bool, len(record.Domains))
		for path, values := range record.Domains {
			domains[path] = setOf(values)
		}
		answers[record.Answer] = answerRecord{noBody: record.NoBody, keyPaths: setOf(record.KeyPaths), domains: domains}
	}
	routes := make(map[string]map[string]bool, len(base.Requests))
	for _, request := range base.Requests {
		routes[request.Route] = setOf(request.FieldPaths)
	}
	answerRows := cloneAnswers(base.Answers)
	enumeratedRows := cloneEnumeratedFields(base.EnumeratedFields)
	composed := make([]contractRecords, 0, len(entries))
	for _, entry := range entries {
		refused := func(format string, args ...any) error {
			return fmt.Errorf("entry %s (contract %d) %s. A fixture applies over the entries before it in the build's order, stamped or by id: an entry that changes what another added comes after it in both orders, so give it a later id (scripts/daemon-contract-entry.sh) and write its fixture again (go test ./internal/api -update-entry=<id>)",
				entry.name, entry.number, fmt.Sprintf(format, args...))
		}
		if err := applyAnswerChanges(entry.fixture.Answers, answers, refused); err != nil {
			return nil, err
		}
		if err := applyRouteChanges(entry.fixture.Routes, routes, refused); err != nil {
			return nil, err
		}
		var err error
		if answerRows, err = withAnswerRows(entry, answerRows, refused); err != nil {
			return nil, err
		}
		if enumeratedRows, err = withEnumeratedRows(entry, enumeratedRows, refused); err != nil {
			return nil, err
		}
		at := contractRecords{
			Contract:         entry.number,
			Records:          make([]contractShape, 0, len(answers)),
			Requests:         make([]requestShape, 0, len(routes)),
			Answers:          cloneAnswers(answerRows),
			EnumeratedFields: cloneEnumeratedFields(enumeratedRows),
		}
		for _, answer := range slices.Sorted(maps.Keys(answers)) {
			record := answers[answer]
			domains := make(map[string][]string, len(record.domains))
			for path, values := range record.domains {
				domains[path] = slices.Sorted(maps.Keys(values))
			}
			at.Records = append(at.Records, contractShape{Contract: entry.number, Answer: answer, NoBody: record.noBody,
				KeyPaths: slices.Sorted(maps.Keys(record.keyPaths)), Domains: domains})
		}
		for _, route := range slices.Sorted(maps.Keys(routes)) {
			at.Requests = append(at.Requests, requestShape{Contract: entry.number, Route: route,
				FieldPaths: slices.Sorted(maps.Keys(routes[route]))})
		}
		composed = append(composed, at)
	}
	return composed, nil
}

// setOf is list as a set, never nil, so a record with no key path composes as one with an empty
// set rather than a nil one.
func setOf(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, item := range list {
		set[item] = true
	}
	return set
}

// repeatedIn is the first word list holds twice, and false where it holds none: a fixture names
// each answer, route, key path and value once.
func repeatedIn(list []string) (string, bool) {
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		if seen[item] {
			return item, true
		}
		seen[item] = true
	}
	return "", false
}

// applyAnswerChanges is changes applied to answers, each condition read against the record before
// the entry, and refused names the broken one.
func applyAnswerChanges(changes []answerChange, answers map[AnswerID]answerRecord, refused func(string, ...any) error) error {
	named := make([]string, len(changes))
	for i, change := range changes {
		named[i] = string(change.Answer)
	}
	if answer, twice := repeatedIn(named); twice {
		return refused("names the %s answer twice", answer)
	}
	for _, change := range changes {
		before, held := answers[change.Answer]
		if path, twice := repeatedIn(slices.Concat(change.AddKeyPaths, change.RemoveKeyPaths)); twice {
			return refused("names `%s` of the %s answer twice", path, change.Answer)
		}
		var record answerRecord
		switch change.Change {
		case changeRemove:
			if !held {
				return refused("removes the %s answer, and the record before it holds no such answer", change.Answer)
			}
			if change.NoBody != nil || len(change.AddKeyPaths)+len(change.RemoveKeyPaths)+len(change.AddValues)+len(change.RemoveValues) > 0 {
				return refused("removes the %s answer and changes it too", change.Answer)
			}
			delete(answers, change.Answer)
			continue
		case changeAdd:
			if held {
				return refused("adds the %s answer, and the record before it holds that answer already", change.Answer)
			}
			if change.NoBody == nil {
				return refused("adds the %s answer and names no noBody for it", change.Answer)
			}
			if len(change.RemoveKeyPaths)+len(change.RemoveValues) > 0 {
				return refused("adds the %s answer and removes from it too", change.Answer)
			}
			record = answerRecord{noBody: *change.NoBody, keyPaths: map[string]bool{}, domains: map[string]map[string]bool{}}
		case "":
			if !held {
				return refused("changes the %s answer, and the record before it holds no such answer", change.Answer)
			}
			if change.NoBody != nil && *change.NoBody == before.noBody {
				return refused("sets noBody of the %s answer to %t, and the record before it holds %t already", change.Answer, *change.NoBody, before.noBody)
			}
			record = answerRecord{noBody: before.noBody, keyPaths: maps.Clone(before.keyPaths), domains: make(map[string]map[string]bool, len(before.domains))}
			if change.NoBody != nil {
				record.noBody = *change.NoBody
			}
			for path, values := range before.domains {
				record.domains[path] = maps.Clone(values)
			}
		default:
			return refused("names the change %q of the %s answer, which is neither %q nor %q", change.Change, change.Answer, changeAdd, changeRemove)
		}
		for _, path := range change.AddKeyPaths {
			if before.keyPaths[path] {
				return refused("adds `%s` to the %s answer, and the record before it holds that key path already", path, change.Answer)
			}
			record.keyPaths[path] = true
		}
		for _, path := range change.RemoveKeyPaths {
			if !before.keyPaths[path] {
				return refused("removes `%s` from the %s answer, and the record before it holds no such key path", path, change.Answer)
			}
			delete(record.keyPaths, path)
		}
		for _, path := range slices.Sorted(maps.Keys(change.AddValues)) {
			values := change.AddValues[path]
			if value, twice := repeatedIn(values); twice {
				return refused("names `%s` at `%s` of the %s answer twice", value, path, change.Answer)
			}
			held := record.domains[path]
			if held == nil {
				held = map[string]bool{}
			}
			for _, value := range values {
				if before.domains[path][value] {
					return refused("adds `%s` at `%s` of the %s answer, and the record before it admits that value already", value, path, change.Answer)
				}
				held[value] = true
			}
			record.domains[path] = held
		}
		for _, path := range slices.Sorted(maps.Keys(change.RemoveValues)) {
			for _, value := range change.RemoveValues[path] {
				if !before.domains[path][value] {
					return refused("removes `%s` at `%s` of the %s answer, and the record before it admits no such value", value, path, change.Answer)
				}
				delete(record.domains[path], value)
			}
			// A domain the entry empties is no domain: a record that holds none for a field
			// admits no value of it, as one holding an empty domain does.
			if len(record.domains[path]) == 0 {
				delete(record.domains, path)
			}
		}
		answers[change.Answer] = record
	}
	return nil
}

// applyRouteChanges is changes applied to routes, each condition read against the record before
// the entry, and refused names the broken one.
func applyRouteChanges(changes []routeChange, routes map[string]map[string]bool, refused func(string, ...any) error) error {
	named := make([]string, len(changes))
	for i, change := range changes {
		named[i] = change.Route
	}
	if route, twice := repeatedIn(named); twice {
		return refused("names %s twice", route)
	}
	for _, change := range changes {
		before, held := routes[change.Route]
		if path, twice := repeatedIn(slices.Concat(change.AddFieldPaths, change.RemoveFieldPaths)); twice {
			return refused("names `%s` of %s twice", path, change.Route)
		}
		switch change.Change {
		case changeRemove:
			if !held {
				return refused("removes %s, and the record before it holds no such route", change.Route)
			}
			if len(change.AddFieldPaths)+len(change.RemoveFieldPaths) > 0 {
				return refused("removes %s and changes it too", change.Route)
			}
			delete(routes, change.Route)
			continue
		case changeAdd:
			if held {
				return refused("adds %s, and the record before it holds that route already", change.Route)
			}
			if len(change.RemoveFieldPaths) > 0 {
				return refused("adds %s and removes from it too", change.Route)
			}
		case "":
			if !held {
				return refused("changes %s, and the record before it holds no such route", change.Route)
			}
		default:
			return refused("names the change %q of %s, which is neither %q nor %q", change.Change, change.Route, changeAdd, changeRemove)
		}
		fieldPaths := maps.Clone(before)
		if fieldPaths == nil {
			fieldPaths = map[string]bool{}
		}
		for _, path := range change.AddFieldPaths {
			if before[path] {
				return refused("adds `%s` to %s, and the record before it holds that field path already", path, change.Route)
			}
			fieldPaths[path] = true
		}
		for _, path := range change.RemoveFieldPaths {
			if !before[path] {
				return refused("removes `%s` from %s, and the record before it holds no such field path", path, change.Route)
			}
			delete(fieldPaths, path)
		}
		routes[change.Route] = fieldPaths
	}
	return nil
}

// withAnswerRows is rows with entry's rows of answers.json added, each arriving at the entry's
// number, and refused names a row that adds what the row before it holds.
func withAnswerRows(entry numberedFixture, rows []Answer, refused func(string, ...any) error) ([]Answer, error) {
	after := cloneAnswers(rows)
	named := make([]string, len(entry.fixture.AnswerRows))
	for i, row := range entry.fixture.AnswerRows {
		named[i] = string(row.ID)
	}
	if id, twice := repeatedIn(named); twice {
		return nil, refused("adds two rows of the %s answer to answers.json", id)
	}
	for _, added := range entry.fixture.AnswerRows {
		typed := make([]TypedSetting, len(added.TypedSettings))
		for i, setting := range added.TypedSettings {
			typed[i] = TypedSetting{Key: setting.Key, Since: entry.number}
		}
		at := slices.IndexFunc(after, func(row Answer) bool { return row.ID == added.ID })
		if at < 0 {
			row := Answer{ID: added.ID, Routes: slices.Clone(added.Routes), Since: entry.number, NoBody: added.NoBody}
			if len(typed) > 0 {
				row.TypedSettings = typed
			}
			after = append(after, row)
			continue
		}
		before := after[at]
		if added.NoBody != before.NoBody {
			return nil, refused("adds to the %s row of answers.json with noBody %t, and that row holds %t", added.ID, added.NoBody, before.NoBody)
		}
		for _, route := range added.Routes {
			if slices.Contains(before.Routes, route) {
				return nil, refused("adds %s to the %s row of answers.json, and that row holds it already", route, added.ID)
			}
		}
		for _, setting := range typed {
			if slices.ContainsFunc(before.TypedSettings, func(held TypedSetting) bool { return held.Key == setting.Key && held.Until == 0 }) {
				return nil, refused("types `settings.%s` on the %s row of answers.json, and that row types it already", setting.Key, added.ID)
			}
		}
		after[at].Routes = append(before.Routes, added.Routes...)
		after[at].TypedSettings = append(before.TypedSettings, typed...)
	}
	return after, nil
}

// withEnumeratedRows is rows with entry's rows of enumerated-fields.json added, each value arriving
// at the entry's number and each row's values sorted by value, and refused names a row that adds a
// value the row before it holds.
func withEnumeratedRows(entry numberedFixture, rows []EnumeratedField, refused func(string, ...any) error) ([]EnumeratedField, error) {
	after := cloneEnumeratedFields(rows)
	named := make([]string, len(entry.fixture.EnumeratedRows))
	for i, row := range entry.fixture.EnumeratedRows {
		named[i] = string(row.Answer) + " " + row.KeyPath
	}
	if field, twice := repeatedIn(named); twice {
		return nil, refused("adds two rows of %s to enumerated-fields.json", field)
	}
	for _, added := range entry.fixture.EnumeratedRows {
		at := slices.IndexFunc(after, func(row EnumeratedField) bool { return row.Answer == added.Answer && row.KeyPath == added.KeyPath })
		row := EnumeratedField{Answer: added.Answer, KeyPath: added.KeyPath}
		if at >= 0 {
			row = after[at]
		}
		for _, value := range added.Values {
			if slices.ContainsFunc(row.Values, func(held EnumeratedValue) bool { return held.Value == value }) {
				return nil, refused("adds `%s` to `%s` of the %s answer in enumerated-fields.json, and that row holds it already", value, added.KeyPath, added.Answer)
			}
			row.Values = append(row.Values, EnumeratedValue{Value: value, Since: entry.number})
		}
		slices.SortFunc(row.Values, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
		if at >= 0 {
			after[at] = row
		} else {
			after = append(after, row)
		}
	}
	return after, nil
}

// cloneAnswers is answers with every list copied, so a composition that appends to a row's lists
// moves no row the base or an earlier contract holds.
func cloneAnswers(answers []Answer) []Answer {
	cloned := make([]Answer, len(answers))
	for i, answer := range answers {
		answer.Routes = slices.Clone(answer.Routes)
		answer.TypedSettings = slices.Clone(answer.TypedSettings)
		cloned[i] = answer
	}
	return cloned
}

// cloneEnumeratedFields is fields with every list of values copied, as cloneAnswers.
func cloneEnumeratedFields(fields []EnumeratedField) []EnumeratedField {
	cloned := make([]EnumeratedField, len(fields))
	for i, field := range fields {
		field.Values = slices.Clone(field.Values)
		cloned[i] = field
	}
	return cloned
}

// entryFixtureOf is the fixture an entry's `-update` writes: what at, the contract of that entry as
// the daemon serves it (each answer's record and each route's, and the rows of answers.json and
// enumerated-fields.json as the registry holds them up to that entry), changes of before, the
// record the entries ahead of it compose. A record holding no domain for a field and one holding
// an empty domain admit the same values, so only values make a change of a domain. A row change
// the fixture has no way to write is refused, naming the row: a row of answers.json or
// enumerated-fields.json that the entry removes or edits, a row that arrives at another number than
// the entry's, and a typed setting from another contract than the entry's or with an end.
func entryFixtureOf(before, at contractRecords) (entryFixture, error) {
	fixture := entryFixture{Answers: []answerChange{}, Routes: []routeChange{}, AnswerRows: []entryAnswerRow{}, EnumeratedRows: []entryEnumeratedRow{}}
	beforeAnswers, atAnswers := map[AnswerID]contractShape{}, map[AnswerID]contractShape{}
	for _, record := range before.Records {
		beforeAnswers[record.Answer] = record
	}
	for _, record := range at.Records {
		atAnswers[record.Answer] = record
	}
	for _, answer := range keysOfEither(beforeAnswers, atAnswers) {
		was, held := beforeAnswers[answer]
		is, served := atAnswers[answer]
		change := answerChange{Answer: answer}
		switch {
		case !served:
			change.Change = changeRemove
			fixture.Answers = append(fixture.Answers, change)
			continue
		case !held:
			change.Change = changeAdd
			change.NoBody = &is.NoBody
		case was.NoBody != is.NoBody:
			change.NoBody = &is.NoBody
		}
		change.AddKeyPaths, change.RemoveKeyPaths = setDifferences(was.KeyPaths, is.KeyPaths)
		for _, path := range keysOfEither(was.Domains, is.Domains) {
			added, removed := setDifferences(was.Domains[path], is.Domains[path])
			if len(added) > 0 {
				if change.AddValues == nil {
					change.AddValues = map[string][]string{}
				}
				change.AddValues[path] = added
			}
			if len(removed) > 0 {
				if change.RemoveValues == nil {
					change.RemoveValues = map[string][]string{}
				}
				change.RemoveValues[path] = removed
			}
		}
		if change.Change != "" || change.NoBody != nil || change.AddKeyPaths != nil || change.RemoveKeyPaths != nil ||
			change.AddValues != nil || change.RemoveValues != nil {
			fixture.Answers = append(fixture.Answers, change)
		}
	}
	beforeRoutes, atRoutes := map[string][]string{}, map[string][]string{}
	for _, request := range before.Requests {
		beforeRoutes[request.Route] = request.FieldPaths
	}
	for _, request := range at.Requests {
		atRoutes[request.Route] = request.FieldPaths
	}
	for _, route := range keysOfEither(beforeRoutes, atRoutes) {
		was, held := beforeRoutes[route]
		is, served := atRoutes[route]
		change := routeChange{Route: route}
		switch {
		case !served:
			change.Change = changeRemove
			fixture.Routes = append(fixture.Routes, change)
			continue
		case !held:
			change.Change = changeAdd
		}
		change.AddFieldPaths, change.RemoveFieldPaths = setDifferences(was, is)
		if change.Change != "" || change.AddFieldPaths != nil || change.RemoveFieldPaths != nil {
			fixture.Routes = append(fixture.Routes, change)
		}
	}
	rows, err := answerRowsOf(before.Answers, at.Answers, at.Contract)
	if err != nil {
		return entryFixture{}, err
	}
	fixture.AnswerRows = rows
	fields, err := enumeratedRowsOf(before.EnumeratedFields, at.EnumeratedFields, at.Contract)
	if err != nil {
		return entryFixture{}, err
	}
	fixture.EnumeratedRows = fields
	return fixture, nil
}

// keysOfEither is every key of a or b, sorted.
func keysOfEither[K cmp.Ordered, V any](a, b map[K]V) []K {
	keys := slices.Collect(maps.Keys(a))
	for key := range b {
		if _, held := a[key]; !held {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// setDifferences is what is holds that was does not, and what was holds that is does not, each
// sorted, and nil where it is empty.
func setDifferences(was, is []string) (added, removed []string) {
	for _, item := range is {
		if !slices.Contains(was, item) {
			added = append(added, item)
		}
	}
	for _, item := range was {
		if !slices.Contains(is, item) {
			removed = append(removed, item)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// answerRowsOf is the rows of answers.json an entry at number adds to before, as at holds them
// (entryFixtureOf).
func answerRowsOf(before, at []Answer, number int) ([]entryAnswerRow, error) {
	rows := []entryAnswerRow{}
	for _, was := range before {
		if !slices.ContainsFunc(at, func(row Answer) bool { return row.ID == was.ID }) {
			return nil, fmt.Errorf("the entry at %d removes the %s row of answers.json, which an entry fixture cannot write: an entry adds rows", number, was.ID)
		}
	}
	for _, is := range at {
		position := slices.IndexFunc(before, func(row Answer) bool { return row.ID == is.ID })
		var was Answer
		if position < 0 {
			if is.Since != number {
				return nil, fmt.Errorf("the %s row of answers.json arrives at %d and the entry that adds it is %d: a row an entry adds arrives at the entry", is.ID, is.Since, number)
			}
			was = Answer{ID: is.ID, NoBody: is.NoBody}
		} else {
			was = before[position]
		}
		if was.Since != 0 && (was.Since != is.Since || was.NoBody != is.NoBody || !slices.Equal(was.Routes, is.Routes[:min(len(was.Routes), len(is.Routes))])) {
			return nil, fmt.Errorf("the entry at %d edits the %s row of answers.json, which an entry fixture cannot write: an entry adds routes and typed settings to a row and edits none it holds", number, is.ID)
		}
		row := entryAnswerRow{ID: is.ID, Routes: slices.Clone(is.Routes[len(was.Routes):]), NoBody: is.NoBody}
		if !slices.Equal(was.TypedSettings, is.TypedSettings[:min(len(was.TypedSettings), len(is.TypedSettings))]) {
			return nil, fmt.Errorf("the entry at %d edits a typed setting of the %s row of answers.json, which an entry fixture cannot write: an entry adds a typed setting and edits none", number, is.ID)
		}
		for _, setting := range is.TypedSettings[len(was.TypedSettings):] {
			if setting.Since != number || setting.Until != 0 {
				return nil, fmt.Errorf("the entry at %d types `settings.%s` on the %s row of answers.json from %d to %d, which an entry fixture cannot write: a typed setting an entry adds is typed from the entry on",
					number, setting.Key, is.ID, setting.Since, setting.Until)
			}
			row.TypedSettings = append(row.TypedSettings, entryTypedSetting{Key: setting.Key})
		}
		if was.Since == 0 || len(row.Routes) > 0 || len(row.TypedSettings) > 0 {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// enumeratedRowsOf is the rows of enumerated-fields.json an entry at number adds to before, as at
// holds them (entryFixtureOf).
func enumeratedRowsOf(before, at []EnumeratedField, number int) ([]entryEnumeratedRow, error) {
	rows := []entryEnumeratedRow{}
	for _, was := range before {
		if !slices.ContainsFunc(at, func(row EnumeratedField) bool { return row.Answer == was.Answer && row.KeyPath == was.KeyPath }) {
			return nil, fmt.Errorf("the entry at %d removes the row of `%s` of the %s answer from enumerated-fields.json, which an entry fixture cannot write: an entry adds rows", number, was.KeyPath, was.Answer)
		}
	}
	for _, is := range at {
		var was EnumeratedField
		if position := slices.IndexFunc(before, func(row EnumeratedField) bool { return row.Answer == is.Answer && row.KeyPath == is.KeyPath }); position >= 0 {
			was = before[position]
		}
		row := entryEnumeratedRow{Answer: is.Answer, KeyPath: is.KeyPath, Values: []string{}}
		for _, value := range was.Values {
			if !slices.Contains(is.Values, value) {
				return nil, fmt.Errorf("the entry at %d removes or moves `%s` of `%s` of the %s answer in enumerated-fields.json, which an entry fixture cannot write: an entry adds values", number, value.Value, is.KeyPath, is.Answer)
			}
		}
		for _, value := range is.Values {
			if slices.Contains(was.Values, value) {
				continue
			}
			if value.Since != number {
				return nil, fmt.Errorf("`%s` of `%s` of the %s answer arrives at %d and the entry that adds it is %d: a value an entry adds arrives at the entry", value.Value, is.KeyPath, is.Answer, value.Since, number)
			}
			row.Values = append(row.Values, value.Value)
		}
		if len(row.Values) > 0 {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// packageEntryFixtures is the directory of the entry fixtures of the entries this package
// registers, `contracts/` beside the records. It holds entry fixtures and nothing else; a test
// registry's entries read theirs from a directory of test data (pairEntryFixtures), so no count of
// entries includes them.
var packageEntryFixtures = goldenPath("contracts")

// entryFixturePath is the fixture of entry in dir, `<id>_<slug>.json`.
func entryFixturePath(dir string, entry NumberedEntry) string {
	return filepath.Join(dir, fmt.Sprintf("%d_%s.json", entry.ID, entry.Slug))
}

// readEntryFixture is the fixture at path, decoded strictly, with its bytes, and false where no
// file is there.
func readEntryFixture(t *testing.T, path string) (entryFixture, []byte, bool) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return entryFixture{}, nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var fixture entryFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("%s is no entry fixture: %v", path, err)
	}
	return fixture, encoded, true
}

// encodedEntryFixture is fixture as its file holds it: marshalled with two spaces of indent and a
// newline at the end, as golden writes every fixture of this package.
func encodedEntryFixture(t *testing.T, fixture entryFixture) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("marshal an entry fixture: %v", err)
	}
	return append(encoded, '\n')
}

// lastNumberedRecords is L's record: the frozen records of contractShapesFixture and
// requestShapesFixture at L, and the numbered entries' rows of answers.json and
// enumerated-fields.json, which every entry's fixture is composed over.
func lastNumberedRecords(t *testing.T) contractRecords {
	t.Helper()
	shapes, written := readContractShapes(t)
	if !written {
		t.Fatal(frozenRecordsMissing())
	}
	requests, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is not written", requestShapesFixture)
	}
	base := contractRecords{Contract: LastNumberedContract, Answers: cloneAnswers(frozenAnswers), EnumeratedFields: cloneEnumeratedFields(enumeratedFields())}
	for _, record := range shapes {
		if record.Contract == LastNumberedContract {
			base.Records = append(base.Records, record)
		}
	}
	for _, record := range requests {
		if record.Contract == LastNumberedContract {
			base.Requests = append(base.Requests, record)
		}
	}
	if len(base.Records) == 0 || len(base.Requests) == 0 {
		t.Fatalf("%s or %s holds no record of L (%d), which every entry's record is composed over", contractShapesFixture, requestShapesFixture, LastNumberedContract)
	}
	return base
}

// composedBuild is the record of each entry's number in the build's order (NumberedEntries, of
// the registry in use): L's record with the fixture of each entry up to that one applied, each
// fixture read from dir (packageEntryFixtures for the entries this package registers). It returns
// the error of an entry with no fixture and of a fixture that breaks a condition of the
// composition, each naming the entry.
func composedBuild(t *testing.T, dir string) ([]contractRecords, error) {
	t.Helper()
	entries := NumberedEntries()
	numbered := make([]numberedFixture, len(entries))
	for i, entry := range entries {
		path := entryFixturePath(dir, entry)
		fixture, _, present := readEntryFixture(t, path)
		if !present {
			return nil, fmt.Errorf("entry %d_%s has no fixture %s: run `go test ./internal/api -update`, which writes it", entry.ID, entry.Slug, path)
		}
		numbered[i] = numberedFixture{name: fmt.Sprintf("%d_%s", entry.ID, entry.Slug), number: entry.Number, fixture: fixture}
	}
	return composeEntries(lastNumberedRecords(t), numbered)
}

// buildRecords is composedBuild, and fails the test on its error.
func buildRecords(t *testing.T, dir string) []contractRecords {
	t.Helper()
	composed, err := composedBuild(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	return composed
}

// shapeRecordsOfBuild is every record of the answers this build serves: the frozen records of
// contractShapesFixture, then the records of each entry's number, its fixture read from dir
// (buildRecords).
func shapeRecordsOfBuild(t *testing.T, dir string) []contractShape {
	t.Helper()
	frozen, written := readContractShapes(t)
	if !written {
		t.Fatal(frozenRecordsMissing())
	}
	records := slices.Clone(frozen)
	for _, contract := range buildRecords(t, dir) {
		records = append(records, contract.Records...)
	}
	return records
}

// requestRecordsOfBuild is every record of the requests this build reads: the frozen records of
// requestShapesFixture, then the records of each entry's number, its fixture read from dir
// (buildRecords).
func requestRecordsOfBuild(t *testing.T, dir string) []requestShape {
	t.Helper()
	frozen, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is missing: it holds the frozen records of the numbered entries, which no flag writes", requestShapesFixture)
	}
	records := slices.Clone(frozen)
	for _, contract := range buildRecords(t, dir) {
		records = append(records, contract.Requests...)
	}
	return records
}

// servedRecordsAt is contract as this daemon serves it: each answer's record taken from served
// (servedShapes), each route's as its request type reads it there (requestShapesAt), and the rows
// of answers.json and enumerated-fields.json as rows holds them.
func servedRecordsAt(contract int, served map[shapeKey]answerShape, walked map[string][]string, answers []Answer, fields []EnumeratedField) contractRecords {
	at := contractRecords{Contract: contract, Records: []contractShape{}, Answers: answers, EnumeratedFields: fields,
		Requests: requestShapesAt(walked, AcceptedByName(), contract)}
	for _, record := range shapeRecords(served, EnumeratedFields()) {
		if record.Contract == contract {
			at.Records = append(at.Records, record)
		}
	}
	return at
}

// The fixture of each entry, in the build's order. `-update` writes the fixture of each entry
// that has none, and `-update-entry=<id>` writes that entry's again: what the daemon serves at the
// entry's number less the record the entries before it compose (entryFixtureOf), with the rows of
// answers.json and enumerated-fields.json as the registry holds them up to that entry. Its routes
// are those `-entry-requests` gives it (entryRequests); else, for the last entry of the build's
// order, what the request types read; else those of its committed fixture. An earlier entry with
// neither is refused, naming it: the types read the fields of every entry after it too, so they
// would give it a later entry's request changes. No flag writes a frozen record or another entry's
// fixture: the frozen records are what the readers of the numbered entries need, and another
// entry's fixture is what its own contract means. A run with neither flag holds each fixture to
// the form `-update` writes, and the rows the fixtures compose to the registry's own tables, so an
// entry whose rows moved after its fixture was written is red here until its fixture is written
// again.
func TestEntryFixtures(t *testing.T) {
	entries := NumberedEntries()
	for _, entry := range entries {
		for _, retired := range testRowsByEntry[entry.ID].retiredValues {
			before := domainsOf(EnumeratedFields(), retired.answer, entry.Number-1)
			if !slices.Contains(before[retired.keyPath], retired.value) {
				t.Fatalf("entry %d cannot retire %q at %s of %s: the preceding contract does not admit it",
					entry.ID, retired.value, retired.keyPath, retired.answer)
			}
		}
	}
	if !rewritingFixtures() {
		composed := buildRecords(t, packageEntryFixtures)
		for _, entry := range entries {
			path := entryFixturePath(packageEntryFixtures, entry)
			fixture, encoded, _ := readEntryFixture(t, path)
			if want := encodedEntryFixture(t, fixture); !bytes.Equal(encoded, want) {
				t.Errorf("%s is not in the form `-update` writes it (sorted, indented by two spaces):\n got: %s\nwant: %s", path, encoded, want)
			}
		}
		if len(composed) == 0 {
			return
		}
		newest := composed[len(composed)-1]
		if got, want := jsonOf(t, newest.Answers), jsonOf(t, Answers()); got != want {
			t.Errorf("answers.json and the entry fixtures' answerRows compose\n%s\nand this build's Answers() is\n%s\nrun `go test ./internal/api -update-entry=<id>` for the entry whose rows moved", got, want)
		}
		if got, want := jsonOf(t, sortedFields(newest.EnumeratedFields)), jsonOf(t, sortedFields(EnumeratedFields())); got != want {
			t.Errorf("enumerated-fields.json and the entry fixtures' enumeratedRows compose\n%s\nand this build's EnumeratedFields() is\n%s\nrun `go test ./internal/api -update-entry=<id>` for the entry whose rows moved", got, want)
		}
		return
	}
	writes := func(entry NumberedEntry, present bool) bool {
		return (*updateGolden && !present) || strconv.Itoa(entry.ID) == *updateEntry
	}
	if *updateEntry != "" && !slices.ContainsFunc(entries, func(entry NumberedEntry) bool { return strconv.Itoa(entry.ID) == *updateEntry }) {
		t.Fatalf("-update-entry=%s names no entry this package registers", *updateEntry)
	}
	if !slices.ContainsFunc(entries, func(entry NumberedEntry) bool {
		_, _, present := readEntryFixture(t, entryFixturePath(packageEntryFixtures, entry))
		return writes(entry, present)
	}) {
		// Every entry has its fixture and none is named, so nothing is written, and the bodies
		// of every contract are not served for nothing.
		return
	}
	served := servedShapes(t, callerKinds[0])
	walked := walkedRequests(t)
	requests := requestsOfEntries(t, entries)
	if err := writeEntryFixtures(t, packageEntryFixtures, entries, registeredEntries, lastNumberedRecords(t), served, walked, requests, writes); err != nil {
		t.Fatal(err)
	}
}

// writeEntryFixtures writes into dir the fixture of each entry of entries, in the build's order,
// that writes says to write, composing each over base and the fixtures of the entries before it.
// registered holds each entry's declaration, served and walked are what the daemon serves and what
// its request types read, and requests the routes `-entry-requests` gives each entry it names. It
// returns the error of an entry whose fixture cannot be written or composed, naming the entry, and
// of an entry before the last whose routes neither requests nor its committed fixture gives.
func writeEntryFixtures(t *testing.T, dir string, entries []NumberedEntry, registered []contractEntry, base contractRecords,
	served map[shapeKey]answerShape, walked map[string][]string, requests map[int][]routeChange, writes func(NumberedEntry, bool) bool) error {
	t.Helper()
	byID := map[int]contractEntry{}
	for _, entry := range registered {
		byID[entry.id] = entry
	}
	before := base
	var ahead []contractEntry
	var ids []int
	for i, entry := range entries {
		name := fmt.Sprintf("%d_%s", entry.ID, entry.Slug)
		path := entryFixturePath(dir, entry)
		ahead, ids = append(ahead, byID[entry.ID]), append(ids, entry.ID)
		fixture, _, present := readEntryFixture(t, path)
		if writes(entry, present) {
			// The rows of answers.json and enumerated-fields.json as the registry holds them up to
			// this entry: a registry of this entry and the ones ahead of it, stamped in the build's
			// order, which numbers each of them as the build does.
			upTo, err := buildContractRegistry(ahead, stampOf(ids...))
			if err != nil {
				return fmt.Errorf("build the registry of the entries up to %s: %v", name, err)
			}
			at := servedRecordsAt(entry.Number, served, walked, upTo.answers, upTo.enumeratedFields)
			// One build's request types read every entry's fields at every number, so they give the
			// requests of the last entry of the build's order alone; an earlier entry takes its
			// routes from -entry-requests or from its committed fixture, never from the types.
			changes, given := requests[entry.ID]
			switch {
			case given:
				at.Requests = requestsWith(before.Requests, changes, entry.Number)
			case i == len(entries)-1:
				// The last entry's requests are what the types read, as servedRecordsAt took them.
			case present:
				at.Requests = requestsWith(before.Requests, fixture.Routes, entry.Number)
			default:
				return fmt.Errorf("entry %s is not last in the build's order, and the request types read the fields of every entry after it too, so they do not give its requests: "+
					"give its routes with `-entry-requests=<file>` (an object of each entry's routes by its id, in the form of a fixture's `routes`), or restore its committed fixture %s, whose routes are kept", name, path)
			}
			fixture, err = entryFixtureOf(before, at)
			if err != nil {
				return fmt.Errorf("entry %s: %v", name, err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("create the directory of entry fixtures: %v", err)
			}
			if err := os.WriteFile(path, encodedEntryFixture(t, fixture), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		} else if !present {
			return fmt.Errorf("entry %s has no fixture %s, and -update-entry writes one entry after it: run `go test ./internal/api -update` first", name, path)
		}
		composed, err := composeEntries(before, []numberedFixture{{name: name, number: entry.Number, fixture: fixture}})
		if err != nil {
			return err
		}
		before = composed[0]
	}
	return nil
}

// requestsOfEntries is the routes of each entry, by id, of the file `-entry-requests` names
// (entryRequests), and nil where the flag names none. An id of no entry of entries is refused, as
// `-update-entry` refuses one: the file is another branch's.
func requestsOfEntries(t *testing.T, entries []NumberedEntry) map[int][]routeChange {
	t.Helper()
	if *entryRequests == "" {
		return nil
	}
	encoded, err := os.ReadFile(*entryRequests)
	if err != nil {
		t.Fatalf("read -entry-requests: %v", err)
	}
	var requests map[int][]routeChange
	if err := json.Unmarshal(encoded, &requests); err != nil {
		t.Fatalf("-entry-requests=%s is no object of each entry's routes by its id: %v", *entryRequests, err)
	}
	for _, id := range slices.Sorted(maps.Keys(requests)) {
		if !slices.ContainsFunc(entries, func(entry NumberedEntry) bool { return entry.ID == id }) {
			t.Fatalf("-entry-requests=%s names entry %d, which this package does not register", *entryRequests, id)
		}
	}
	return requests
}

// requestsWith is the requests of before, at contract, with changes made as the records they were
// read from made them: a removed route goes, and every other route they name holds before's field
// paths less those removed and with those added. Those records stood over the branch's own base,
// not over before, so a change that before already holds (a field main added too) changes nothing
// here, and entryFixtureOf writes only what the entry changes of before.
func requestsWith(before []requestShape, changes []routeChange, contract int) []requestShape {
	routes := make(map[string]map[string]bool, len(before))
	for _, request := range before {
		routes[request.Route] = setOf(request.FieldPaths)
	}
	for _, change := range changes {
		if change.Change == changeRemove {
			delete(routes, change.Route)
			continue
		}
		paths, held := routes[change.Route]
		if !held {
			paths = map[string]bool{}
			routes[change.Route] = paths
		}
		for _, path := range change.RemoveFieldPaths {
			delete(paths, path)
		}
		for _, path := range change.AddFieldPaths {
			paths[path] = true
		}
	}
	requests := make([]requestShape, 0, len(routes))
	for _, route := range slices.Sorted(maps.Keys(routes)) {
		requests = append(requests, requestShape{Contract: contract, Route: route, FieldPaths: slices.Sorted(maps.Keys(routes[route]))})
	}
	return requests
}

// jsonOf is value as JSON, for a comparison whose failure prints both sides.
func jsonOf(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return string(encoded)
}

// sortedFields is fields by answer, then by key path. The registry places an entry's rows after
// the frozen ones, and the derived rows of the record answer where its derivation does; the
// composition places each entry's rows after the rows before it. The two orders say the same
// table, which is what the comparison holds.
func sortedFields(fields []EnumeratedField) []EnumeratedField {
	sorted := slices.Clone(fields)
	slices.SortFunc(sorted, func(a, b EnumeratedField) int {
		if a.Answer != b.Answer {
			return strings.Compare(string(a.Answer), string(b.Answer))
		}
		return strings.Compare(a.KeyPath, b.KeyPath)
	})
	return sorted
}

// retiredEnumValue is a value a released contract admitted and a later entry no longer admits.
// Its declaration stays in the historic table; only the answer domain changes.
type retiredEnumValue struct {
	answer         AnswerID
	keyPath, value string
}

func retiredEnumValueAt(answer AnswerID, keyPath, value string, contract int) bool {
	row := retiredEnumValue{answer: answer, keyPath: keyPath, value: value}
	for _, entry := range registry().entries {
		if entry.Number > contract {
			break
		}
		if slices.Contains(testRowsByEntry[entry.ID].retiredValues, row) {
			return true
		}
	}
	return false
}

// entryTestRows is what one entry adds to the tables of this package's tests that a contract
// change grows, as the entry's file adds its rows to the tables the daemon serves from. They are
// written in the entry's own `contract_<id>_<slug>_test.go`, which registers them from an init
// function:
//
//	func init() {
//		registerEntryTestRows(entryTestRows{entry: probeContract, requests: …, answers: …, states: …})
//	}
//
// so two entries add their rows in two files and edit no shared line. Each table is its frozen
// rows, then the entries' rows in the build's order (entriesTestRows).
type entryTestRows struct {
	// entry is the entry's declaration, its id (`probeContract`).
	entry int
	// retiredValues leave the entry's answer domains without removing historical declarations.
	retiredValues []retiredEnumValue
	// requests is each route the entry adds that decodes a body a plugin sends, with the type
	// its handler decodes the body into: rows of pluginRequests.
	requests map[string]reflect.Type
	// answers is the fixture of each answer the entry adds, and the roots and bodies it adds to
	// an older answer's fixture, which carry a key the entry adds to that answer: rows of
	// answerFixtures.
	answers func(t *testing.T) map[AnswerID]answerFixture
	// states is each state body the entry adds to stateFixtures, the bodies the records of the
	// state and of the issue record are taken from, which carries a key the entry adds to the
	// state.
	states func(t *testing.T) []State
	// carry writes into narrowing_test.go's narrowingState the keys the entry adds to the state,
	// so a band the entry adds finds something to take there and writes its own line, as every
	// frozen band does (TestANarrowedBodySaysSo); nil for an entry that adds no band.
	carry func(State) State
}

// testRowsByEntry is every entry's test rows, by the entry's id.
var testRowsByEntry = map[int]entryTestRows{}

// registerEntryTestRows records one entry's test rows. An entry registers them once, so a second
// registration of one id is a mistake of the source, refused before any test runs.
func registerEntryTestRows(rows entryTestRows) {
	if _, twice := testRowsByEntry[rows.entry]; twice {
		panic(fmt.Sprintf("the test rows of entry %d are registered twice", rows.entry))
	}
	testRowsByEntry[rows.entry] = rows
}

// entriesTestRows is the test rows of the build's entries, in the build's order (NumberedEntries):
// the order in which each table takes them after its frozen rows.
func entriesTestRows() []entryTestRows {
	var rows []entryTestRows
	for _, entry := range NumberedEntries() {
		if registered, held := testRowsByEntry[entry.ID]; held {
			rows = append(rows, registered)
		}
	}
	return rows
}

// Each registration of test rows names an entry this package registers, and each row of
// pluginRequests it adds is a route no frozen row and no other entry's row holds: a route's type
// is the type its handler decodes, which an entry's row adds and does not move. A registration of
// no entry would add its rows to no table, and a second row of one route would hide the first.
func TestEveryEntrysTestRowsAddToTheTables(t *testing.T) {
	registered := map[int]bool{}
	for _, entry := range NumberedEntries() {
		registered[entry.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(testRowsByEntry)) {
		if !registered[id] {
			t.Errorf("test rows are registered for entry %d, which is no entry this package registers", id)
		}
	}
	held := maps.Clone(frozenPluginRequests)
	for _, rows := range entriesTestRows() {
		for _, route := range slices.Sorted(maps.Keys(rows.requests)) {
			if _, twice := held[route]; twice {
				t.Errorf("entry %d adds %s to pluginRequests, which holds that route already", rows.entry, route)
			}
			held[route] = rows.requests[route]
		}
	}
}

// entryFixtureExamples is the directory of the example fixtures both compositions are held to:
// test data of the contracts package, beside its fixtures and never among the entry fixtures of
// `fixtures/daemon-api/contracts/`, so no count of entries includes them.
var entryFixtureExamples = filepath.Join("..", "..", "..", "contracts", "testdata", "entry-fixture-examples")

// The composition, held to the example fixtures it shares with the TypeScript schema tests
// (`packages/contracts/src/legion-api.test.ts`, "the example entry fixtures compose the one record
// expected of them"): base.json stands for L's record, each `<id>_<slug>.json` is an entry fixture,
// and expected.json is the one record the newest of them composes, which both languages reach or
// are red. The examples make every operation a fixture holds, and the last removes what the first
// and the second add, so their order matters: in the reverse order the composition is refused,
// naming the entry and the operation, as the shape gate refuses a fixture an order puts before the
// entry whose key it changes.
func TestTheCompositionOfTheExampleFixtures(t *testing.T) {
	dir := entryFixtureExamples
	read := func(name string) contractRecords {
		encoded, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var records contractRecords
		if err := json.Unmarshal(encoded, &records); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return records
	}
	base := read("base.json")
	listed, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var examples []numberedFixture
	for _, file := range listed {
		if !entryFixtureName.MatchString(file.Name()) {
			continue
		}
		fixture, _, _ := readEntryFixture(t, filepath.Join(dir, file.Name()))
		examples = append(examples, numberedFixture{name: strings.TrimSuffix(file.Name(), ".json"), fixture: fixture})
	}
	if len(examples) < 2 {
		t.Fatalf("%s holds %d example entry fixtures; an order of fewer than two proves nothing", dir, len(examples))
	}
	numbered := func(order []numberedFixture) []numberedFixture {
		ranked := slices.Clone(order)
		for rank := range ranked {
			ranked[rank].number = base.Contract + rank + 1
		}
		return ranked
	}
	composed, err := composeEntries(base, numbered(examples))
	if err != nil {
		t.Fatalf("the examples in id order: %v", err)
	}
	if got, want := comparableJSON(t, composed[len(composed)-1]), comparableJSON(t, read("expected.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("the examples compose\n%s\nand expected.json is\n%s", jsonOf(t, got), jsonOf(t, want))
	}

	reversed := slices.Clone(examples)
	slices.Reverse(reversed)
	reversed = numbered(reversed)
	_, err = composeEntries(base, reversed)
	want := fmt.Sprintf("entry %s (contract %d) removes the note answer, and the record before it holds no such answer", reversed[0].name, base.Contract+1)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("the examples in the reverse order composed with %v, want the refusal %q", err, want)
	}
	t.Logf("the reverse order is refused: %v", err)
}

// pairAddTestContract and pairRemoveTestContract are the ids of the wrongly ordered pair of check
// c-stamped-gate: two test entries whose fixtures do not commute, the second removing the key path
// the first adds (pairKeyPath). Each is an id of the form an entry's id takes, later than any entry
// of this tree can be, and no other test registry registers either.
const (
	pairAddTestContract    = 20990303030303
	pairRemoveTestContract = 20990404040404
)

// pairKeyPath is the state's key path the pair's first entry adds and its second removes.
const pairKeyPath = "issues.*.pairNote"

// pairEntryFixtures is the directory of the pair's fixtures: this package's test data, never among
// the entry fixtures of `contracts/` (packageEntryFixtures), so no count of entries includes them.
var pairEntryFixtures = filepath.Join("testdata", "entry-fixtures")

// pairEntries is the pair, as the entries of a test registry: rows of no table, with all they
// change in their fixtures.
func pairEntries() []contractEntry {
	return []contractEntry{{id: pairAddTestContract, slug: "pair-add"}, {id: pairRemoveTestContract, slug: "pair-remove"}}
}

// A fixture applies over the entries before it in the build's order, so the gate composes a
// registry's entries in the order its stamp gives them, and fails closed on an order that puts an
// entry before the entry whose key it changes. The pair, stamped in the order that adds the key
// first, composes: the record of the first entry's number holds the key and the second's does not.
// Stamped in the order that removes it first, the composition is refused, naming the second entry
// and its removal. The negative control of c-stamped-gate runs the shape gate itself
// (shapeGate) over this registry, stamped in that order.
func TestTheGateComposesATestRegistrysEntriesInItsStampedOrder(t *testing.T) {
	t.Run("stamped in the order that adds the key first", func(t *testing.T) {
		installContractRegistry(t, testRegistry(t, stampOf(pairAddTestContract, pairRemoveTestContract), pairEntries()...))
		composed, err := composedBuild(t, pairEntryFixtures)
		if err != nil {
			t.Fatalf("the pair in the order that adds the key first: %v", err)
		}
		if len(composed) != 2 {
			t.Fatalf("the pair composed %d records, want one for each of its two entries", len(composed))
		}
		for i, holds := range []bool{true, false} {
			at := slices.IndexFunc(composed[i].Records, func(record contractShape) bool { return record.Answer == AnswerState })
			if got := at >= 0 && slices.Contains(composed[i].Records[at].KeyPaths, pairKeyPath); got != holds {
				t.Errorf("the state's record of contract %d holds `%s`: %t, want %t", composed[i].Contract, pairKeyPath, got, holds)
			}
		}
	})
	t.Run("stamped in the order that removes the key first", func(t *testing.T) {
		installContractRegistry(t, testRegistry(t, stampOf(pairRemoveTestContract, pairAddTestContract), pairEntries()...))
		_, err := composedBuild(t, pairEntryFixtures)
		want := fmt.Sprintf("entry %d_pair-remove (contract %d) removes `%s` from the state answer, and the record before it holds no such key path",
			pairRemoveTestContract, LastNumberedContract+1, pairKeyPath)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the pair in the order that removes the key first composed with %v, want the refusal %q", err, want)
		}
		t.Logf("the order that removes the key first is refused: %v", err)
	})
}

// comparableJSON is value marshalled and read back as plain JSON values, so that an empty map and
// an absent one, or a nil list and an empty one where the form leaves it out, compare as the
// files they are written as.
func comparableJSON(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var plain any
	if err := json.Unmarshal(encoded, &plain); err != nil {
		t.Fatalf("decode %T: %v", value, err)
	}
	return plain
}

// requestsEarlierTestContract and requestsLaterTestContract are the ids of two test entries, the
// later of which adds a field to a request (requestsTestRoute, requestsLaterField). Each is an id
// of the form an entry's id takes, later than any entry of this tree can be, and no other test
// registry registers either.
const (
	requestsEarlierTestContract = 20990505050505
	requestsLaterTestContract   = 20990606060606
)

// requestsTestRoute is the route whose request the later entry of the two adds requestsLaterField
// to.
const (
	requestsTestRoute  = "POST /test/requests"
	requestsLaterField = "laterNote"
)

// One build's request types read every field at every number, so they give the requests of the
// last entry of the build's order alone: an earlier entry's requests read from them would take
// the fields of every entry after it. Written in one run, the earlier of two entries, the later
// of which adds a field to a request, is refused, naming it, and no fixture of it is written;
// given its routes with `-entry-requests`, it holds none of the later entry's field, which the
// later entry's fixture adds.
func TestAnEarlierEntrysFixtureTakesNoRequestFieldOfALaterEntry(t *testing.T) {
	base := lastNumberedRecords(t)
	base.Records = []contractShape{}
	base.Requests = []requestShape{{Contract: LastNumberedContract, Route: requestsTestRoute, FieldPaths: []string{"title"}}}
	walked := map[string][]string{requestsTestRoute: {requestsLaterField, "title"}}
	entries := []contractEntry{{id: requestsEarlierTestContract, slug: "requests-earlier"}, {id: requestsLaterTestContract, slug: "requests-later"}}
	installContractRegistry(t, testRegistry(t, stampOf(requestsEarlierTestContract, requestsLaterTestContract), entries...))
	missing := func(_ NumberedEntry, present bool) bool { return !present }
	addsTheLaterField := func(t *testing.T, path string) bool {
		t.Helper()
		fixture, _, present := readEntryFixture(t, path)
		if !present {
			t.Fatalf("%s is not written", path)
		}
		return slices.ContainsFunc(fixture.Routes, func(change routeChange) bool {
			return change.Route == requestsTestRoute && slices.Contains(change.AddFieldPaths, requestsLaterField)
		})
	}
	earlier, later := NumberedEntries()[0], NumberedEntries()[1]

	t.Run("read from the request types", func(t *testing.T) {
		dir := t.TempDir()
		err := writeEntryFixtures(t, dir, NumberedEntries(), entries, base, map[shapeKey]answerShape{}, walked, nil, missing)
		if err == nil {
			if addsTheLaterField(t, entryFixturePath(dir, earlier)) {
				t.Fatalf("the fixture of entry %d_requests-earlier adds %s to %s, which only the later entry adds", earlier.ID, requestsLaterField, requestsTestRoute)
			}
			t.Fatalf("the earlier entry's requests were read from the request types, want a refusal naming entry %d_requests-earlier", earlier.ID)
		}
		want := fmt.Sprintf("entry %d_requests-earlier is not last in the build's order", earlier.ID)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the write was refused with %v, want the refusal %q", err, want)
		}
		if _, _, present := readEntryFixture(t, entryFixturePath(dir, earlier)); present {
			t.Errorf("the refused write left a fixture of entry %d_requests-earlier", earlier.ID)
		}
		t.Logf("the earlier entry is refused: %v", err)
	})
	t.Run("given with -entry-requests", func(t *testing.T) {
		dir := t.TempDir()
		requests := map[int][]routeChange{earlier.ID: {}}
		if err := writeEntryFixtures(t, dir, NumberedEntries(), entries, base, map[shapeKey]answerShape{}, walked, requests, missing); err != nil {
			t.Fatalf("write the two fixtures: %v", err)
		}
		if addsTheLaterField(t, entryFixturePath(dir, earlier)) {
			t.Errorf("the fixture of entry %d_requests-earlier adds %s to %s, which only the later entry adds", earlier.ID, requestsLaterField, requestsTestRoute)
		}
		if !addsTheLaterField(t, entryFixturePath(dir, later)) {
			t.Errorf("the fixture of entry %d_requests-later does not add %s to %s", later.ID, requestsLaterField, requestsTestRoute)
		}
	})
}
