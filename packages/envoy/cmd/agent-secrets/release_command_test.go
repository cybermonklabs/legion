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

	"github.com/sjawhar/envoy/internal/broker/record"
)

// commandBroker is a fake broker for what a release says of itself: every request is granted at
// once, its one name released as "<NAME>-v1", and each release records the body it was sent and
// echoes any github_app it was sent, as a broker that applied it does.
//
// knows is the release body's vintage the broker reads. A broker from before a field decodes the
// release's body as the broker's readJSON (internal/broker/api) does, refusing a field it does not
// declare, and writes that refusal as readJSON writes it, 400 INVALID_NARROWING with the decoder's
// own text. A narrowing to the owner "nobody" is refused 400 INVALID_NARROWING on its own
// account, which names no unknown field.
type commandBroker struct {
	srv   *httptest.Server
	knows releaseBodyFields

	mu     sync.Mutex
	bodies []string // each release's body, "" for none
}

// releaseBodyFields is which fields of the release body a fake broker reads.
type releaseBodyFields int

const (
	knowsEveryField releaseBodyFields = iota // github_app, command and reason
	knowsNarrowing                           // github_app alone: a broker from before command
	knowsCommand                             // github_app and command: a broker from before reason
)

func newCommandBroker(t *testing.T, knows releaseBodyFields) *commandBroker {
	t.Helper()
	b := &commandBroker{knows: knows}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Request string `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		obj, err := record.VerifyRequestObject(body.Request, b.srv.URL, time.Minute, time.Now())
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
			// The broker's own cap on a JSON body (maxJSONBodyBytes, internal/broker/api).
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			writeJSON(w, map[string]string{"code": "REQUEST_TOO_LARGE", "error": "request body exceeds 1048576 bytes"})
			return
		}
		var every struct {
			GitHubApp *GitHubNarrowing `json:"github_app"`
			Command   []string         `json:"command"`
			Reason    string           `json:"reason"`
		}
		var narrowingOnly struct {
			GitHubApp *GitHubNarrowing `json:"github_app"`
		}
		var commandToo struct {
			GitHubApp *GitHubNarrowing `json:"github_app"`
			Command   []string         `json:"command"`
		}
		var narrowing **GitHubNarrowing
		var into any
		switch b.knows {
		case knowsNarrowing:
			into, narrowing = &narrowingOnly, &narrowingOnly.GitHubApp
		case knowsCommand:
			into, narrowing = &commandToo, &commandToo.GitHubApp
		default:
			into, narrowing = &every, &every.GitHubApp
		}
		if len(raw) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(into); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]string{"code": "INVALID_NARROWING", "error": "body must be one valid JSON object: " + err.Error()})
				return
			}
		}
		sent := struct{ GitHubApp *GitHubNarrowing }{*narrowing}
		if sent.GitHubApp != nil && sent.GitHubApp.Owner == "nobody" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"code": "INVALID_NARROWING", "error": "github_app.owner names no installation"})
			return
		}
		name := r.PathValue("id")
		answer := map[string]any{"values": map[string]string{name: name + "-v1"}, "expires_at": nil}
		if sent.GitHubApp != nil {
			answer["github_app"] = sent.GitHubApp
		}
		writeJSON(w, answer)
	})
	// The handler reads b.srv.URL, so b.srv is assigned before the server serves: Start, which
	// sets the URL, runs after the assignment and before the goroutine that serves.
	b.srv = httptest.NewUnstartedServer(mux)
	b.srv.Start()
	t.Cleanup(b.srv.Close)
	return b
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

// TestExecSendsItsCommandWithTheRelease: the exec form names the command it runs, the arguments
// after --, in the release's body, beside the narrowing its --github-* flags ask for and the
// --reason its own call gave, so the broker can log what the grant was spent on and why this call
// spent it; the command then runs with the value.
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
		{"beside a narrowing", []string{"--github-owner", "ada"}, `{"github_app":{"owner":"ada"},"command":["sh","-c","echo \"ran $APP\""]}`},
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
// command and the reason, its narrowing kept, and the command runs. Any other INVALID_NARROWING
// is the broker's answer to the release itself: it is not sent again, and nothing runs.
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
		{"alone", knowsNarrowing, nil, []string{`{` + command + `}`, ``}},
		{"beside a narrowing", knowsNarrowing, []string{"--github-owner", "ada"}, []string{`{"github_app":{"owner":"ada"},` + command + `}`, `{"github_app":{"owner":"ada"}}`}},
		{"with its reason", knowsNarrowing, []string{"--reason", "rotate the key"}, []string{`{` + command + `,"reason":"rotate the key"}`, ``}},
		{"with its reason, to a broker that reads the command", knowsCommand, []string{"--github-owner", "ada", "--reason", "rotate the key"},
			[]string{`{"github_app":{"owner":"ada"},` + command + `,"reason":"rotate the key"}`, `{"github_app":{"owner":"ada"}}`}},
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

	t.Run("a refusal of the narrowing", func(t *testing.T) {
		b := newCommandBroker(t, knowsEveryField)
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, "APP", "--github-owner", "nobody", "--", "sh", "-c", "echo ran-the-child")
		if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "names no installation") {
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
