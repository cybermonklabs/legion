package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TestRigLaunch is the label's own code for the skill-scenario rig's launches
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh launch): the system prompt, the pane's
// environment and the gh files of one claim's launch, each made as this label's daemon and its
// tmux runtime make them, and the publish of each notice of the request to the run's Envoy
// listener, as the outbox's notice executor publishes it. With RIG_TEXTS_REQUEST and
// RIG_TEXTS_ANSWER it reads the launch request (scored-runs-contracts.ts, LaunchRequest) and
// writes the launch answer; with neither it runs its own cases. It reads those two variables and
// sets none, so it runs beside the package's other tests.
func TestRigLaunch(t *testing.T) {
	t.Parallel()
	request, answer := os.Getenv("RIG_TEXTS_REQUEST"), os.Getenv("RIG_TEXTS_ANSWER")
	if (request == "") != (answer == "") {
		t.Fatalf("RIG_TEXTS_REQUEST is %q and RIG_TEXTS_ANSWER is %q: give both or neither", request, answer)
	}
	if request != "" {
		data, err := os.ReadFile(request)
		if err != nil {
			t.Fatal(err)
		}
		req, err := decodeRigLaunchRequest(data)
		if err != nil {
			t.Fatalf("the launch request %s: %v", request, err)
		}
		encoded, err := json.MarshalIndent(rigLaunch(t, req), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(answer, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	// An owner's launch, with a planted line in the deployment's instructions and one notice:
	// the prompt file holds the planted line once, and the listener is asked once to keep the
	// notice for the architect's role topic.
	t.Run("an owner's launch", func(t *testing.T) {
		t.Parallel()
		listener := newLaunchListener(t)
		req := rigOwnLaunch(t, listener.URL)
		got := rigLaunch(t, req)

		prompt, err := os.ReadFile(got.SystemPromptFile)
		if err != nil {
			t.Fatal(err)
		}
		if held := strings.Count(string(prompt), rigPlantedInstructions); held != 1 {
			t.Errorf("the prompt file %s holds the planted instructions %d times, want once", got.SystemPromptFile, held)
		}
		token, err := claim.NewToken("rig", req.Claim.Issue, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		topic := notify.RoleTopicPrefix + string(token)
		if publishes := listener.publishes(); len(publishes) != 1 || publishes[0].Topic != topic || !publishes[0].Keep || publishes[0].Bearer != "Bearer rig-envoy-token" {
			t.Errorf("the listener received %+v, want one publish on %s, kept, with the run's Envoy bearer", publishes, topic)
		}
		if len(got.Published) != 1 || got.Published[0].Topic != topic || got.Published[0].Key != record.OutboxKey(req.Notices[0].Row) {
			t.Errorf("the answer's publishes = %+v, want one on %s under row %d's key", got.Published, topic, req.Notices[0].Row)
		}
		if got.Env["LEGION_PROJECT"] != "rig" || got.Env["LEGION_ROLE"] != string(claim.RoleArchitect) || got.Env["LEGION_BOOT_TOKEN_FILE"] != req.BootTokenFile {
			t.Errorf("the pane's environment = %v, want the claim's project, role and the rig's boot token file", got.Env)
		}
	})

	// The plan guidance reaches a launch at a label whose daemon has it, and this label's has none:
	// a request that names it is refused, naming the field, and the same request without it is read.
	t.Run("a launch request with planGuidanceFile is refused, naming it", func(t *testing.T) {
		t.Parallel()
		encoded, err := json.Marshal(rigOwnLaunch(t, "http://127.0.0.1:1"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRigLaunchRequest(encoded); err != nil {
			t.Fatalf("the request without planGuidanceFile is refused: %v", err)
		}
		guided := append(bytes.TrimSuffix(encoded, []byte("}")), []byte(`,"planGuidanceFile":"/rig/guidance.md"}`)...)
		if _, err := decodeRigLaunchRequest(guided); err == nil || !strings.Contains(err.Error(), "planGuidanceFile") {
			t.Errorf("the request with planGuidanceFile is answered %v, want a refusal naming planGuidanceFile", err)
		}
	})
}

// rigPlantedInstructions is the planted line of the own case's deployment instructions.
const rigPlantedInstructions = "rig-planted-instructions-5d1e: a line of this deployment's own instructions."

// rigOwnLaunch is the own case's launch request: the root architect of RIG-1, whose listener is
// envoyURL, with one catch-up notice, every file under a directory of the test's own.
func rigOwnLaunch(t *testing.T, envoyURL string) rigLaunchRequest {
	t.Helper()
	dir := t.TempDir()
	file := func(name, text string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	payload, err := json.Marshal(record.Notice{Kind: "catch-up", CatchUp: &record.CatchUp{Generation: 1, Launch: 1, Gates: []record.CatchUpGate{},
		Issues: []record.CatchUpIssue{{Key: "RIG-1", Title: "The rig's root", Phase: phase.Admitted, Status: "in_progress"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return rigLaunchRequest{
		Project: "RIG", StateDir: filepath.Join(dir, "state"), Workspace: filepath.Join(dir, "workspace"),
		DaemonURL: "http://127.0.0.1:1", EnvoyURL: envoyURL, EnvoyTokenFile: file("envoy-token", "rig-envoy-token\n", 0o600),
		NatsURLs: []string{"nats://127.0.0.1:4222"}, BootTokenFile: file("boot-token", "rig-boot-token", 0o600),
		GHToken: "rig-gh-token", GHPath: file("gh", "#!/bin/sh\nexit 0\n", 0o755), Path: "/usr/bin:/bin",
		Claim:      rigLaunchClaim{Tree: "RIG-1", Issue: "RIG-1", Role: string(claim.RoleArchitect)},
		Repository: "rig-owner/rig-repo", DefaultBranch: "trunk",
		InstructionsFile: file("instructions.md", rigPlantedInstructions+"\n", 0o600),
		Notices:          []rigLaunchNotice{{Issue: "RIG-1", Kind: "catch-up", Row: 17, Payload: payload}},
	}
}

// rigLaunchRequest is texts/launch-request.json (LaunchRequest in
// packages/pi-legion/scripts/skill-scenarios/scored-runs-contracts.ts), less planGuidanceFile,
// which this label has no plan guidance to take.
type rigLaunchRequest struct {
	Project          string            `json:"project"`
	StateDir         string            `json:"stateDir"`
	Workspace        string            `json:"workspace"`
	DaemonURL        string            `json:"daemonUrl"`
	EnvoyURL         string            `json:"envoyUrl"`
	EnvoyTokenFile   string            `json:"envoyTokenFile"`
	NatsURLs         []string          `json:"natsUrls"`
	BootTokenFile    string            `json:"bootTokenFile"`
	GHToken          string            `json:"ghToken"`
	GHPath           string            `json:"ghPath"`
	Path             string            `json:"path"`
	Claim            rigLaunchClaim    `json:"claim"`
	Repository       string            `json:"repository"`
	DefaultBranch    string            `json:"defaultBranch"`
	InstructionsFile string            `json:"instructionsFile"`
	Notices          []rigLaunchNotice `json:"notices"`
}

type rigLaunchClaim struct {
	Tree  string `json:"tree"`
	Issue string `json:"issue"`
	Role  string `json:"role"`
}

type rigLaunchNotice struct {
	Issue   string          `json:"issue"`
	Kind    string          `json:"kind"`
	Row     int64           `json:"row"`
	Payload json.RawMessage `json:"payload"`
}

// rigLaunchAnswer is texts/launch-answer.json (LaunchAnswer).
type rigLaunchAnswer struct {
	SystemPromptFile string             `json:"systemPromptFile"`
	Env              map[string]string  `json:"env"`
	Published        []rigLaunchPublish `json:"published"`
}

type rigLaunchPublish struct {
	Topic   string `json:"topic"`
	Message string `json:"message"`
	Key     string `json:"key"`
}

// decodeRigLaunchRequest reads a launch request, refusing a field this test does not handle by its
// name, and a path the test binary would read from its own package directory.
func decodeRigLaunchRequest(data []byte) (rigLaunchRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req rigLaunchRequest
	if err := decoder.Decode(&req); err != nil {
		return rigLaunchRequest{}, err
	}
	if decoder.More() {
		return rigLaunchRequest{}, errors.New("the request holds more than one JSON value")
	}
	for name, path := range map[string]string{"stateDir": req.StateDir, "workspace": req.Workspace, "envoyTokenFile": req.EnvoyTokenFile,
		"bootTokenFile": req.BootTokenFile, "ghPath": req.GHPath, "instructionsFile": req.InstructionsFile} {
		if !filepath.IsAbs(path) {
			return rigLaunchRequest{}, fmt.Errorf("%s is %q, want an absolute path", name, path)
		}
	}
	return req, nil
}

// rigLaunch makes req's launch as this label makes a launch of its claim:
//
//   - the spawn spec from a specs built as boot builds it, of the request's project, state
//     directory, deployment instructions, repository and default branch, the design-gate steps of
//     a deployment that leaves `gates.design` unset, the run's Envoy bearer as the launch secret,
//     and the repository's owner as the one owner the role's App reaches; it has no owner reader,
//     so the claim is addressed to its tree's architect;
//   - the system prompt composed and written as the tmux runtime writes it (runtime.ComposePrompt,
//     runtime.WriteSystemPrompt);
//   - the claim's gh files (ghconfig.WriteBundle, under runtime.GHConfigDir), its launch secret's
//     file, and the pane's variables (tmux.PaneVariables), with the rig's boot token file and the
//     PATH workerbin.Path makes;
//   - each notice published to the request's listener as the notice executor publishes it, to the
//     claim's role topic with noticeSummary and Notice.Published, asking the listener to keep it,
//     since the launch comes after the publish.
func rigLaunch(t *testing.T, req rigLaunchRequest) rigLaunchAnswer {
	t.Helper()
	ctx := context.Background()
	project, err := claim.ProjectToken(req.Project)
	if err != nil {
		t.Fatal(err)
	}
	role := claim.Role(req.Claim.Role)
	token, err := claim.NewToken(project, req.Claim.Issue, role)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := ghrepo.Parse("the launch request's repository", req.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(req.GHPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("ghPath %s is no executable file: %v", req.GHPath, err)
	}
	if err := os.MkdirAll(req.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composer, err := prompts.New(req.StateDir)
	if err != nil {
		t.Fatalf("compose the label's prompts: %v", err)
	}
	envoyToken, err := config.ReadSecretPointer("envoyTokenFile", req.EnvoyTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	owner := strings.ToLower(repo.Owner())
	s := specs{
		stateDir: req.StateDir, project: project, prompts: composer, instructions: req.InstructionsFile,
		designSteps: config.Gates{}.Steps(), secrets: map[string]string{"ENVOY_TOKEN": envoyToken},
		repository: func(context.Context, string) (ghrepo.Repository, string, error) { return repo, req.DefaultBranch, nil },
		appOwners:  func(claim.Role) []string { return []string{owner} },
	}
	c := supervise.Claim{Token: token, Project: project, Tree: req.Claim.Tree, Issue: req.Claim.Issue, Role: role, Generation: 1}
	spec, err := s.SpawnSpec(ctx, c)
	if err != nil {
		t.Fatalf("the spawn spec of %s: %v", token, err)
	}
	spec.Claim, spec.Project, spec.Tree, spec.Issue, spec.Role, spec.Generation = c.Token, c.Project, c.Tree, c.Issue, c.Role, c.Generation

	text, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	promptFile, err := runtime.WriteSystemPrompt(req.StateDir, string(token), text)
	if err != nil {
		t.Fatal(err)
	}
	files := []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: req.BootTokenFile}}
	names := make([]string, 0, len(spec.Secrets))
	for name := range spec.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		files = append(files, runtime.SecretFile{Variable: name, Value: spec.Secrets[name],
			Path: filepath.Join(runtime.SecretsDir(req.StateDir), string(token)+"-"+strings.ToLower(name))})
	}
	if err := runtime.WriteSecretFiles(req.StateDir, files[1:]); err != nil {
		t.Fatal(err)
	}
	ghRoot := runtime.GHConfigDir(req.StateDir, token)
	bundle := ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: ghconfig.Render(req.GHToken, "", time.Time{})}}
	if _, err := ghconfig.WriteBundle(ghRoot, bundle); err != nil {
		t.Fatalf("write the claim's gh files: %v", err)
	}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL,
		EnvoyURL: req.EnvoyURL, NATSURLs: req.NatsURLs, GHOwnersDir: ghRoot, GHDefault: owner, GH: req.GHPath}, files)
	if err != nil {
		t.Fatal(err)
	}
	answer := rigLaunchAnswer{SystemPromptFile: promptFile, Env: map[string]string{"PATH": workerbin.Path(req.Path, req.StateDir)},
		Published: []rigLaunchPublish{}}
	for _, variable := range variables {
		name, value, _ := strings.Cut(variable, "=")
		answer.Env[name] = value
	}

	if len(req.Notices) > 0 && role != claim.RoleArchitect {
		t.Fatalf("the launch request gives %d notices to the %s: a notice reaches an architect alone", len(req.Notices), role)
	}
	publisher := notify.New(req.EnvoyURL, envoyToken)
	for _, queued := range req.Notices {
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: queued.Row, Kind: record.Notice{}.OutboxKind(), Issue: queued.Issue, Payload: queued.Payload})
		if err != nil {
			t.Fatalf("notice row %d: %v", queued.Row, err)
		}
		notice, ok := decoded.(record.Notice)
		if !ok || string(notice.Kind) != queued.Kind {
			t.Fatalf("notice row %d holds %+v, want a notice of kind %s", queued.Row, decoded, queued.Kind)
		}
		published, key := notice.Published(queued.Row)
		topic, message := notify.RoleTopicPrefix+string(token), noticeSummary(notice.Kind, queued.Issue)
		if _, err := publisher.PublishKept(ctx, topic, message, published, key); err != nil {
			t.Fatalf("publish notice row %d: %v", queued.Row, err)
		}
		answer.Published = append(answer.Published, rigLaunchPublish{Topic: topic, Message: message, Key: key})
	}
	return answer
}

// launchListener stands in for an Envoy listener's publish route, keeping each message it is given.
type launchListener struct {
	*httptest.Server
	mu   sync.Mutex
	seen []launchListenerPublish
}

type launchListenerPublish struct {
	Topic  string
	Keep   bool
	Bearer string
}

func newLaunchListener(t *testing.T) *launchListener {
	t.Helper()
	l := &launchListener{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Topic string `json:"topic"`
			Keep  *bool  `json:"keep"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "not a publish", http.StatusBadRequest)
			return
		}
		l.mu.Lock()
		l.seen = append(l.seen, launchListenerPublish{Topic: body.Topic, Keep: body.Keep != nil && *body.Keep, Bearer: r.Header.Get("Authorization")})
		l.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *launchListener) publishes() []launchListenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}
