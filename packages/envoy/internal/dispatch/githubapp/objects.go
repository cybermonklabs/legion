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

import (
	"bytes"
	"encoding/json"
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

// put writes m into object under name, its value as the map mapped builds of it, and nothing
// where m holds nothing.
func (m *objectMember[T]) put(object map[string]any, name string, mapped func(*T) map[string]any) {
	switch m.held {
	case heldNull:
		object[name] = nil
	case heldObject:
		object[name] = mapped(&m.value)
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

// objectsOf is each listed object as the map mapped builds of it, nil for an item GitHub listed as
// null and nil for no list, as encoding/json's own decode of the list into maps answered both.
func objectsOf[T any](listed []*T, mapped func(*T) map[string]any) []map[string]any {
	if listed == nil {
		return nil
	}
	objects := make([]map[string]any, len(listed))
	for i, item := range listed {
		if item != nil {
			objects[i] = mapped(item)
		}
	}
	return objects
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

func (r *listedRun) object() map[string]any {
	object := make(map[string]any, 10)
	r.ID.put(object, "id")
	r.Name.put(object, "name")
	r.HeadSHA.put(object, "head_sha")
	r.Status.put(object, "status")
	r.Conclusion.put(object, "conclusion")
	r.HTMLURL.put(object, "html_url")
	r.StartedAt.put(object, "started_at")
	r.CompletedAt.put(object, "completed_at")
	r.App.put(object, "app", (*listedApp).object)
	r.Output.put(object, "output", (*listedOutput).object)
	return object
}

// listedSuite is one check suite of GET /repos/{owner}/{repo}/commits/{sha}/check-suites, the
// members contracts.GithubCIObservation reads of a suite: its id, head_sha, status, conclusion,
// updated_at and app.id.
type listedSuite struct {
	ID         scalar                  `json:"id"`
	HeadSHA    scalar                  `json:"head_sha"`
	Status     scalar                  `json:"status"`
	Conclusion scalar                  `json:"conclusion"`
	UpdatedAt  scalar                  `json:"updated_at"`
	App        objectMember[listedApp] `json:"app"`
}

func (s *listedSuite) object() map[string]any {
	object := make(map[string]any, 6)
	s.ID.put(object, "id")
	s.HeadSHA.put(object, "head_sha")
	s.Status.put(object, "status")
	s.Conclusion.put(object, "conclusion")
	s.UpdatedAt.put(object, "updated_at")
	s.App.put(object, "app", (*listedApp).object)
	return object
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

func (s *listedStatus) object() map[string]any {
	object := make(map[string]any, 5)
	s.Context.put(object, "context")
	s.State.put(object, "state")
	s.CreatedAt.put(object, "created_at")
	s.TargetURL.put(object, "target_url")
	s.Description.put(object, "description")
	return object
}

// listedApp is the App a check run or a check suite names: its id, which the review App's rule
// and the record read, and its slug, which a CI result names its actor by.
type listedApp struct {
	ID   scalar `json:"id"`
	Slug scalar `json:"slug"`
}

func (a *listedApp) object() map[string]any {
	object := make(map[string]any, 2)
	a.ID.put(object, "id")
	a.Slug.put(object, "slug")
	return object
}

// listedOutput is a check run's output: its title and summary, which a CI result reports.
type listedOutput struct {
	Title   scalar `json:"title"`
	Summary scalar `json:"summary"`
}

func (o *listedOutput) object() map[string]any {
	object := make(map[string]any, 2)
	o.Title.put(object, "title")
	o.Summary.put(object, "summary")
	return object
}
