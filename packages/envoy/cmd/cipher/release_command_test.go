package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// commandBroker is a fake broker for what a run sends and what a release says of itself: every
// request for one name is granted at once, its name released as "<NAME>-v1" unless answers names
// another value, and the broker records every call it receives, each POST /v1/requests body with
// the request object it verified, and each release's body. A release answers the target its
// grant's request signed as its locations, as the broker does, unless confirmsTarget says what to
// answer in its place.
//
// knows is the release body's vintage the broker reads. A broker from before a field decodes the
// release's body as the broker's readJSON (internal/cipher/api) does, refusing a field it does not
// declare, and writes that refusal as readJSON writes it, 400 INVALID_NARROWING with the decoder's
// own text. A release of REFUSED_APP is refused 400 INVALID_NARROWING on its own account, which
// names no unknown field.
type commandBroker struct {
	srv   *httptest.Server
	knows releaseBodyFields

	mu       sync.Mutex
	calls    int                            // every call the broker received, on any route
	requests []map[string]any               // each POST /v1/requests body, as the JSON object it was sent as
	objects  []record.RequestObject         // the request object each of those bodies signed, as the broker verified it
	bodies   []string                       // each release's body, "" for none
	values   map[string]string              // what a name is released as, where it is not "<NAME>-v1"
	leftOut  map[string]map[string][]string // the values answer's github_left_out, nil for none
	targets  map[string][]string            // grant id -> the locations its request signed
	// confirms says whether confirmed replaces the locations a release answers: nil confirmed
	// answers none, as a broker from before the target does.
	confirms  bool
	confirmed []string
}

// releaseBodyFields is which fields of the release body a fake broker reads.
type releaseBodyFields int

const (
	knowsEveryField releaseBodyFields = iota // command and reason
	knowsNoField                             // neither: a broker from before command
	knowsCommand                             // command alone: a broker from before reason
)

func newCommandBroker(t *testing.T, knows releaseBodyFields) *commandBroker {
	t.Helper()
	b := &commandBroker{knows: knows, targets: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		compact, _ := body["request"].(string)
		obj, err := record.VerifyRequestObject(compact, b.srv.URL, time.Minute, time.Now())
		b.mu.Lock()
		b.requests = append(b.requests, body)
		if err == nil {
			b.objects = append(b.objects, obj)
			if len(obj.Details) == 1 {
				b.targets[obj.Details[0].Identifier] = obj.Details[0].Locations
			}
		}
		b.mu.Unlock()
		if err != nil || len(obj.Details) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name := obj.Details[0].Identifier
		writeJSON(w, map[string]any{
			"request_id": "req-" + name, "state": "granted",
			"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
			"grant_id": name, "record_id": nil, "approver": nil, "lifetime_seconds": nil,
		})
	})
	mux.HandleFunc("POST /v1/grants/{id}/values", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		if len(raw) > 1<<20 {
			// The broker's own cap on a JSON body (maxJSONBodyBytes, internal/cipher/api).
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			writeJSON(w, map[string]string{"code": "REQUEST_TOO_LARGE", "error": "request body exceeds 1048576 bytes"})
			return
		}
		var every struct {
			Command []string `json:"command"`
			Reason  string   `json:"reason"`
		}
		var none struct{}
		var commandOnly struct {
			Command []string `json:"command"`
		}
		into := map[releaseBodyFields]any{knowsEveryField: &every, knowsNoField: &none, knowsCommand: &commandOnly}[b.knows]
		if len(raw) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(into); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]string{"code": "INVALID_NARROWING", "error": "body must be one valid JSON object: " + err.Error()})
				return
			}
		}
		name := r.PathValue("id")
		if name == "REFUSED_APP" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"code": "INVALID_NARROWING", "error": "the release cannot be read"})
			return
		}
		b.mu.Lock()
		value, named := b.values[name]
		leftOut := b.leftOut
		locations := b.targets[name]
		if b.confirms {
			locations = b.confirmed
		}
		b.mu.Unlock()
		if !named {
			value = name + "-v1"
		}
		answer := map[string]any{"values": map[string]string{name: value}, "expires_at": nil}
		if leftOut != nil {
			answer["github_left_out"] = leftOut
		}
		if len(locations) > 0 {
			answer["locations"] = locations
		}
		writeJSON(w, answer)
	})
	// The handler reads b.srv.URL, so b.srv is assigned before the server serves: Start, which
	// sets the URL, runs after the assignment and before the goroutine that serves.
	b.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls++
		b.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	b.srv.Start()
	t.Cleanup(b.srv.Close)
	return b
}

// answers makes each release from here on answer values, by name, in place of "<NAME>-v1", and
// leftOut as its github_left_out, none for nil.
func (b *commandBroker) answers(values map[string]string, leftOut map[string]map[string][]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values, b.leftOut = values, leftOut
}

// confirmsTarget makes each release from here on answer locations in place of the target its
// grant's request signed, none for nil, as a broker that does not confirm that target does.
func (b *commandBroker) confirmsTarget(locations []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.confirms, b.confirmed = true, locations
}

// take returns the release bodies the broker recorded since the last take, each without its
// trailing line ending, and forgets them.
func (b *commandBroker) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	bodies := make([]string, len(b.bodies))
	for i, body := range b.bodies {
		bodies[i] = strings.TrimSpace(body)
	}
	b.bodies = nil
	return bodies
}

// takeRequests returns how many calls the broker received on any route, each POST /v1/requests
// body, and each request object those bodies signed, since the last takeRequests, and forgets
// them.
func (b *commandBroker) takeRequests() (calls int, bodies []map[string]any, objects []record.RequestObject) {
	b.mu.Lock()
	defer b.mu.Unlock()
	calls, bodies, objects = b.calls, b.requests, b.objects
	b.calls, b.requests, b.objects = 0, nil, nil
	return calls, bodies, objects
}

// TestExecSendsItsCommandWithTheRelease: the exec form names the command it runs, the arguments
// after --, in the release's body, beside the --reason its own call gave, so the broker can log
// what the grant was spent on and why this call spent it; the command then runs with the value.
func TestExecSendsItsCommandWithTheRelease(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	for _, row := range []struct {
		name  string
		flags []string
		body  string
	}{
		{"alone", nil, `{"command":["sh","-c","echo \"ran $APP\""]}`},
		{"with its reason", []string{"--reason", "rotate the key"}, `{"command":["sh","-c","echo \"ran $APP\""],"reason":"rotate the key"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := append(append([]string{"APP"}, row.flags...), "--", "sh", "-c", `echo "ran $APP"`)
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			if exit != 0 || stdout != "ran APP-v1\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 running with the value", exit, stdout, stderr)
			}
			if bodies := b.take(); !reflect.DeepEqual(bodies, []string{row.body}) {
				t.Fatalf("release bodies = %q, want one release carrying %q", bodies, row.body)
			}
		})
	}
}

// TestExecReleasesFromAnOlderBrokerWithoutTheCommand: a broker from before the command field, or
// from before the reason field, refuses the whole body as INVALID_NARROWING naming the unknown
// field and releases nothing, so the exec form sends the same release once more without the
// command and the reason, and the command runs. Any other INVALID_NARROWING is the broker's answer
// to the release itself: it is not sent again, and nothing runs.
func TestExecReleasesFromAnOlderBrokerWithoutTheCommand(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	const command = `"command":["sh","-c","echo \"ran $APP\""]`
	for _, row := range []struct {
		name   string
		knows  releaseBodyFields
		flags  []string
		bodies []string
	}{
		{"alone", knowsNoField, nil, []string{`{` + command + `}`, ``}},
		{"with its reason", knowsNoField, []string{"--reason", "rotate the key"}, []string{`{` + command + `,"reason":"rotate the key"}`, ``}},
		{"with its reason, to a broker that reads the command", knowsCommand, []string{"--reason", "rotate the key"}, []string{`{` + command + `,"reason":"rotate the key"}`, ``}},
	} {
		t.Run(row.name, func(t *testing.T) {
			b := newCommandBroker(t, row.knows)
			args := append(append([]string{"APP"}, row.flags...), "--", "sh", "-c", `echo "ran $APP"`)
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			if exit != 0 || stdout != "ran APP-v1\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 running with the value", exit, stdout, stderr)
			}
			if bodies := b.take(); !reflect.DeepEqual(bodies, row.bodies) {
				t.Fatalf("release bodies = %q, want %q: the release, then the same without its command and reason", bodies, row.bodies)
			}
		})
	}

	t.Run("a refusal of the release itself", func(t *testing.T) {
		b := newCommandBroker(t, knowsEveryField)
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, "REFUSED_APP", "--", "sh", "-c", "echo ran-the-child")
		if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "the release cannot be read") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming the broker's refusal and nothing run", exit, stdout, stderr)
		}
		if bodies := b.take(); len(bodies) != 1 {
			t.Fatalf("release bodies = %q, want the one release the broker refused", bodies)
		}
	})
}

// TestExecReleasesACommandOverTheBodyCap: a command of 9,001 arguments, about 1.1 MB and well
// under the kernel's limit, runs as it did before the release named its command: the client
// sends the broker the first 32 arguments alone, each cut to 256 bytes on a character's
// boundary, which is all the broker records, so the body never reaches the broker's 1 MiB cap.
// The command itself gets every argument.
func TestExecReleasesACommandOverTheBodyCap(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	wide := strings.Repeat("é", 500)
	args := []string{"APP", "--", "sh", "-c", `echo "ran $APP $#"`, wide}
	for range 9000 {
		args = append(args, strings.Repeat("x", 120))
	}
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
	if exit != 0 || stdout != "ran APP-v1 9000\n" {
		t.Fatalf("exit %d, stdout %q, stderr %.300q; want exit 0 and the command run with all 9,000 of its arguments", exit, stdout, stderr)
	}
	bodies := b.take()
	if len(bodies) != 1 {
		t.Fatalf("the broker was sent %d releases, want 1", len(bodies))
	}
	var sent struct {
		Command []string `json:"command"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("decode the release body: %v", err)
	}
	if len(sent.Command) != 32 || sent.Command[3] != strings.Repeat("é", 128) || sent.Command[4] != strings.Repeat("x", 120) {
		t.Fatalf("the release named %d arguments (the 4th %d bytes), want 32, the 4th cut to 128 whole characters", len(sent.Command), len(sent.Command[min(3, len(sent.Command)-1)]))
	}
}

// TestLeftOutPermissionsAreNamedOnStderr: a release whose values answer carries github_left_out
// writes one line on stderr for each account it names, the accounts in order, each naming the
// profile, the account and every entry left out since the installation does not hold it. The
// command runs with the value the release carries, a GitHub profile's JSON object of
// installations exactly as it came, and no line on stderr holds any part of its token. A release
// that leaves nothing out writes no such line.
func TestLeftOutPermissionsAreNamedOnStderr(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	const token = "ghs_leftOutTokenNeverPrinted0123"
	release := `{"installations":{"acme-org":{"installation_id":7,"token":"` + token + `","expires_at":"2100-01-01T00:00:00Z"}}}`
	b.answers(map[string]string{"APP": release}, map[string]map[string][]string{
		"APP": {"example-org": {"members"}, "acme-org": {"issues", "statuses:write"}},
	})
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, "APP", "--", "sh", "-c", `printf %s "$APP"`)
	if exit != 0 || stdout != release {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and the child's APP the release as it came", exit, stdout, stderr)
	}
	want := "agent-secrets: the profile APP left out issues, statuses:write for acme-org, since the installation there does not hold them\n" +
		"agent-secrets: the profile APP left out members for example-org, since the installation there does not hold them\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want the lines %q", stderr, want)
	}
	if strings.Contains(stderr, token) {
		t.Fatalf("stderr = %q holds the token", stderr)
	}

	b.answers(nil, nil)
	stdout, stderr, exit = runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, "APP", "--", "sh", "-c", `printf %s "$APP"`)
	if exit != 0 || stdout != "APP-v1" || strings.Contains(stderr, "left out") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0, the value, and no left-out line", exit, stdout, stderr)
	}
}
