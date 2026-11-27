package githubapp

// objects.go holds the objects of a commit's check runs, check suites and commit statuses as the
// conditional reads of them decode GitHub's answer (CommitCheckRuns, CommitCheckSuites,
// CommitStatuses). Their readers take each object as a map, in the shape encoding/json decodes a
// JSON object into: contracts.GithubCIObservation and contracts.GithubStatusObservation map the
// webhook's own objects in that shape, and cireconcile's query builds its objects in it, so one
// run read any of those ways is one observation; the CI results' read of a check run (the API's
// ciRunOf) reads the same map. An interface value in a destination of redact.Answer.Decode would
// carry GitHub's text out of the answer whole, so the answer is decoded typed - each object's
// members that its readers read, and no other - and each object is then built into that map,
// every member as encoding/json's own map of the object held it (scalar, objectMember).
//
// A listing's own type is its one member table: each member is named once, in its json tag, which
// is where encoding/json reads it from and where objectOf reads it to build the map. A member
// added to a listing is therefore in the map its readers take, and one left out of the map is
// impossible.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// held is what one member of a listed object held: nothing (GitHub left it out, or sent a kind no
// reader takes a value of there), null, a string, a number, a bool or an object.
type held uint8

const (
	heldNothing held = iota
	heldNull
	heldString
	heldNumber
	heldBool
	heldObject
)

// member is one member of a listed object: a value that writes what it decoded into the map of
// the object, under the name its own json tag gives. Every member of every listing below is one
// of the two (scalar, objectMember), which objectOf reads each listing's members as.
type member interface {
	put(object map[string]any, name string)
}

// scalar is one member of a listed object that its readers read as a JSON scalar, held as
// encoding/json holds that member in a map of the object: a string, a float64, a bool or null,
// whichever GitHub sent, since a reader takes a value of more than one kind of some members
// (contracts.GithubPRNumber reads an id sent as a string of digits, and contracts'
// nestedNumberString prints an id of any kind). An object or an array is held as nothing: no
// reader takes a value of one where it reads a scalar but nestedNumberString, which printed Go's
// rendering of the map or the slice as a suite's or an App's id, and GitHub sends neither there. A
// scalar keeps the value it decoded, never the member's bytes.
type scalar struct {
	held    held
	text    string
	number  float64
	boolean bool
}

// UnmarshalJSON decodes one member, which encoding/json hands it whole and valid, by the kind its
// first byte names. A number a float64 cannot hold (1e400) is refused, as encoding/json refused it
// in the map.
func (s *scalar) UnmarshalJSON(data []byte) error {
	*s = scalar{}
	switch leadingByte(data) {
	case 'n':
		s.held = heldNull
	case '"':
		s.held = heldString
		return json.Unmarshal(data, &s.text)
	case 't', 'f':
		s.held = heldBool
		return json.Unmarshal(data, &s.boolean)
	case '{', '[':
	default:
		s.held = heldNumber
		return json.Unmarshal(data, &s.number)
	}
	return nil
}

// put writes s into object under name, as encoding/json's map of the object held it, and nothing
// where s holds nothing.
func (s *scalar) put(object map[string]any, name string) {
	switch s.held {
	case heldNull:
		object[name] = nil
	case heldString:
		object[name] = s.text
	case heldNumber:
		object[name] = s.number
	case heldBool:
		object[name] = s.boolean
	}
}

// objectMember is one member of a listed object that its readers read as an object, T the members
// they read of it: held as an object where GitHub sent one, as null where it sent null, and as
// nothing for any other kind, of which no reader takes a value.
type objectMember[T any] struct {
	held  held
	value T
}

// UnmarshalJSON decodes one member, which encoding/json hands it whole and valid.
func (m *objectMember[T]) UnmarshalJSON(data []byte) error {
	*m = objectMember[T]{}
	switch leadingByte(data) {
	case 'n':
		m.held = heldNull
	case '{':
		m.held = heldObject
		return json.Unmarshal(data, &m.value)
	}
	return nil
}

// put writes m into object under name, its value as the map of its own members (objectOf), and
// nothing where m holds nothing.
func (m *objectMember[T]) put(object map[string]any, name string) {
	switch m.held {
	case heldNull:
		object[name] = nil
	case heldObject:
		object[name] = objectOf(&m.value)
	}
}

// leadingByte is the first byte of a JSON value past any white space before it, which names the
// value's kind, and 0 for no value.
func leadingByte(data []byte) byte {
	data = bytes.TrimLeft(data, " \t\r\n")
	if len(data) == 0 {
		return 0
	}
	return data[0]
}

// objectsOf is each listed object as the map of its members, nil for an item GitHub listed as null
// and nil for no list, as encoding/json's own decode of the list into maps answered both.
func objectsOf[T any](listed []*T) []map[string]any {
	if listed == nil {
		return nil
	}
	objects := make([]map[string]any, len(listed))
	for i, item := range listed {
		if item != nil {
			objects[i] = objectOf(item)
		}
	}
	return objects
}

// objectOf is one listed object as the map its readers take: each member the listing declares,
// under the name that member's json tag gives, written as the member writes itself (scalar.put,
// objectMember.put), and nothing for a member that holds nothing. It reads exported fields alone:
// membersOf lists no other, and the read's own IsExported guard is the one the gate's reflection
// census reads (exported_fields_only).
func objectOf[T any](listed *T) map[string]any {
	listing := reflect.TypeFor[T]()
	members := membersOf(listing)
	object := make(map[string]any, len(members))
	held := reflect.ValueOf(listed).Elem()
	for _, m := range members {
		if at := m.at; listing.Field(at).IsExported() {
			held.Field(at).Addr().Interface().(member).put(object, m.name)
		}
	}
	return object
}

// listedMember is one member of a listing's table: where the member sits in the listing's type and
// the name its json tag gives.
type listedMember struct {
	at   int
	name string
}

// memberTables holds each listing's member table, read from its type once (reflect.Type to
// []listedMember).
var memberTables sync.Map

// memberType is the interface every member of a listing implements, through a pointer to it.
var memberType = reflect.TypeFor[member]()

// membersOf is the member table of listed, read from the type's own fields. A field that is not an
// exported member under a json name is this package's own mistake, not an answer's, and panics:
// such a field would reach the map under no name, or not at all, and each listing's own test reads
// every listing through here.
func membersOf(listed reflect.Type) []listedMember {
	if table, ok := memberTables.Load(listed); ok {
		return table.([]listedMember)
	}
	table := make([]listedMember, 0, listed.NumField())
	for at := range listed.NumField() {
		field := listed.Field(at)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "" || !reflect.PointerTo(field.Type).Implements(memberType) {
			panic("githubapp: " + listed.String() + "'s " + field.Name + " is no exported member of a listed object under a json name")
		}
		table = append(table, listedMember{at: at, name: name})
	}
	memberTables.Store(listed, table)
	return table
}

// listedRun is one check run of GET /repos/{owner}/{repo}/commits/{sha}/check-runs, the members
// its readers read: contracts.GithubCIObservation reads its id, name, head_sha, status,
// conclusion, html_url, started_at, completed_at and app.id, and the CI results' read of a run
// its id, name, status, conclusion, html_url, completed_at, output.title, output.summary and
// app.slug.
type listedRun struct {
	ID          scalar                     `json:"id"`
	Name        scalar                     `json:"name"`
	HeadSHA     scalar                     `json:"head_sha"`
	Status      scalar                     `json:"status"`
	Conclusion  scalar                     `json:"conclusion"`
	HTMLURL     scalar                     `json:"html_url"`
	StartedAt   scalar                     `json:"started_at"`
	CompletedAt scalar                     `json:"completed_at"`
	App         objectMember[listedApp]    `json:"app"`
	Output      objectMember[listedOutput] `json:"output"`
}

// listedSuite is one check suite of GET /repos/{owner}/{repo}/commits/{sha}/check-suites, the
// members its readers read: contracts.GithubCIObservation reads its id, head_sha, status,
// conclusion, updated_at and app.id, and the CI reconcile's verify read (cireconcile's
// runningSuite) its id, status and latest_check_runs_count, which tells a suite still running
// with runs in it from one another App leaves queued with none.
type listedSuite struct {
	ID                   scalar                  `json:"id"`
	HeadSHA              scalar                  `json:"head_sha"`
	Status               scalar                  `json:"status"`
	Conclusion           scalar                  `json:"conclusion"`
	UpdatedAt            scalar                  `json:"updated_at"`
	LatestCheckRunsCount scalar                  `json:"latest_check_runs_count"`
	App                  objectMember[listedApp] `json:"app"`
}

// listedStatus is one commit status of GET /repos/{owner}/{repo}/commits/{sha}/status, the
// members contracts.GithubStatusObservation reads: its context, state, created_at, target_url and
// description.
type listedStatus struct {
	Context     scalar `json:"context"`
	State       scalar `json:"state"`
	CreatedAt   scalar `json:"created_at"`
	TargetURL   scalar `json:"target_url"`
	Description scalar `json:"description"`
}

// listedApp is the App a check run or a check suite names: its id, which the review App's rule
// and the record read, and its slug, which a CI result names its actor by.
type listedApp struct {
	ID   scalar `json:"id"`
	Slug scalar `json:"slug"`
}

// listedOutput is a check run's output: its title and summary, which a CI result reports.
type listedOutput struct {
	Title   scalar `json:"title"`
	Summary scalar `json:"summary"`
}
