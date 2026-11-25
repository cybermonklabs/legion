// Package apifixture holds what the daemon API's fixtures (packages/contracts/fixtures/daemon-api)
// are made from where the tests of more than one package make them: the fixtures' path, the
// commands that write the state fixtures, the strip of a state's own contract marks, the loader of
// the legion.yaml files the goldens load, and the pause the pause routes' goldens answer.
// internal/api's goldens and internal/daemon's composition of the state fixtures read the same
// ones, so the last pause the last-pause fixture carries and the pause the pause routes' fixtures
// answer share the moments, the reason and the actors. Their ids differ: the store numbers the
// pause internal/daemon's seed writes (1 in state-last-pause.json), and the pause routes' goldens
// answer Pause's own (7 in project-resume.json). The legion.yaml files themselves stay in
// internal/api's state_golden_test.go, which internal/daemon's state fixtures read them from. Only
// tests import it.
package apifixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Path is the path of the daemon-api fixture name from the directory of a package under
// internal/, where go test runs that package's tests. Each fixture is the cross-language pin: Go
// writes the wire shape, and `packages/contracts/src/legion-api.test.ts` parses it through the
// zod schema the plugin reads with and holds the parse equal to the file, so a Go field with no
// zod line is named there. Go is the source of truth; the fixture is how the schema is held to it.
func Path(name string) string {
	return filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// StateFixturesRewrite is the two commands, run in packages/daemon, that write the state fixtures
// and every fixture internal/api writes from the populated one, in the order they must run:
// internal/daemon's composition of the state fixtures over their seeds, then internal/api's
// goldens.
const StateFixturesRewrite = "go test ./internal/daemon/ -run '^TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds$' -update, " +
	"then go test ./internal/api/ -run Golden -update"

// WithoutOwnContractMarks is body, the JSON of a state, with each mark of its `contract` that
// names own, the contract of the daemon that served it (api.DaemonAPIVersion, which the caller
// passes, since this package imports no api), taken out, and `contract` itself where that leaves
// it empty, every other key in its place and every other byte as it stands; and the marks it took
// out, by name. A state fixture holds no mark of the daemon's own contract, since every contract
// entry moves that number: the caller holds the marks taken out to the ones it expects instead.
func WithoutOwnContractMarks(t testing.TB, body []byte, own int) (json.RawMessage, map[string]int) {
	t.Helper()
	var state jsonObject
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	taken := map[string]int{}
	at := slices.IndexFunc(state, func(member jsonMember) bool { return member.key == "contract" })
	if at < 0 {
		return body, taken
	}
	var marks jsonObject
	if err := json.Unmarshal(state[at].value, &marks); err != nil {
		t.Fatalf("decode the state's contract mark: %v", err)
	}
	marks = slices.DeleteFunc(marks, func(member jsonMember) bool {
		var contract int
		if json.Unmarshal(member.value, &contract) != nil || contract != own {
			return false
		}
		taken[member.key] = contract
		return true
	})
	if len(marks) == 0 {
		state = slices.Delete(state, at, at+1)
	} else {
		encoded, err := json.Marshal(marks)
		if err != nil {
			t.Fatalf("encode the state's contract mark: %v", err)
		}
		state[at].value = encoded
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode the state: %v", err)
	}
	return encoded, taken
}

// jsonMember is one member of a JSON object: its key and its value as written.
type jsonMember struct {
	key   string
	value json.RawMessage
}

// jsonObject is a JSON object as its members, in the order they are written, so a member can be
// taken out of a golden and every other key stays where the writer put it.
type jsonObject []jsonMember

func (o *jsonObject) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return fmt.Errorf("a JSON object begins with {, not %v (%v)", open, err)
	}
	members := jsonObject{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, isKey := token.(string)
		if !isKey {
			return fmt.Errorf("a JSON object's key is a string, not %v", token)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		members = append(members, jsonMember{key: key, value: value})
	}
	*o = members
	return nil
}

func (o jsonObject) MarshalJSON() ([]byte, error) {
	var encoded bytes.Buffer
	encoded.WriteByte('{')
	for i, member := range o {
		if i > 0 {
			encoded.WriteByte(',')
		}
		key, err := json.Marshal(member.key)
		if err != nil {
			return nil, err
		}
		encoded.Write(key)
		encoded.WriteByte(':')
		encoded.Write(member.value)
	}
	encoded.WriteByte('}')
	return encoded.Bytes(), nil
}

// LoadSettings writes body as a legion.yaml under t.TempDir() and loads it as the daemon does,
// reading no environment, so the resolved values are the file's and the defaults alone.
func LoadSettings(t testing.TB, body string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// pauseSince is when the fixtures' pause began, and pauseEnded when its resume ended it.
var (
	pauseSince = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	pauseEnded = time.Date(2026, 11, 15, 9, 41, 30, 0, time.UTC)
)

// Pause is the fixtures' pause: the operator paused LEGION for a provider's outage, and, once
// ended, the controller resumed it from its session.
func Pause(ended bool) store.Pause {
	p := store.Pause{ID: 7, Project: "LEGION", Since: pauseSince, By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "the model provider is refusing every turn"}
	if ended {
		p.EndedAt, p.EndedBy = pauseEnded, store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}
	}
	return p
}
