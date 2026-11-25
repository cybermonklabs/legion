package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// kubeRefusalBearer is the bearer the Sandbox runtime carries to the Kubernetes API in this file's
// row, as the in-cluster ServiceAccount token is carried to the API server: it holds one of the
// made-up credentials oauth2calltest.RequireNone looks for (oauth2calltest.Token), so a text that
// holds it fails the row.
const kubeRefusalBearer = "kube-bearer-" + oauth2calltest.Token

// refusedSandboxGVR is the Sandbox resource as the runtime reads it (sandbox's sandboxGVR), for the
// control's own client.
var refusedSandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// refusingSandboxAPI is issueSandboxAPI with one request refused in the API server's place: a read
// of the Sandbox named refused, answered 403 Forbidden with a Status that echoes the request's
// Authorization in every field a server writes words into (the message, the details' name, group
// and kind, and a cause's type, message and field). Every other request is the issueSandboxAPI's,
// which serves the runtime's informers. The Authorization of every request is kept, so the row can
// say what the stand-in was sent.
type refusingSandboxAPI struct {
	*issueSandboxAPI
	refused string

	seen     sync.Mutex
	bearers  []string
	refusals int
}

func (a *refusingSandboxAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resource, name, ok := kubePath(r.URL.Path)
	refuse := ok && resource == "sandboxes" && name == a.refused && r.Method == http.MethodGet && r.URL.Query().Get("watch") != "true"
	a.seen.Lock()
	a.bearers = append(a.bearers, r.Header.Get("Authorization"))
	if refuse {
		a.refusals++
	}
	a.seen.Unlock()
	if !refuse {
		a.issueSandboxAPI.ServeHTTP(w, r)
		return
	}
	carried := r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
		Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "the request carried " + carried,
		Details: &metav1.StatusDetails{Name: carried, Group: carried, Kind: carried, Causes: []metav1.StatusCause{{
			Type: metav1.CauseType(carried), Message: carried, Field: carried,
		}}}})
}

// refusalCount is how many reads of the refused Sandbox the stand-in has refused.
func (a *refusingSandboxAPI) refusalCount() int {
	a.seen.Lock()
	defer a.seen.Unlock()
	return a.refusals
}

// requireSentBearer fails t unless the stand-in was sent a request and every request carried want:
// the row's control that whatever answered in the API server's place held the credential the row
// looks for.
func (a *refusingSandboxAPI) requireSentBearer(t *testing.T, want string) {
	t.Helper()
	a.seen.Lock()
	defer a.seen.Unlock()
	if len(a.bearers) == 0 {
		t.Fatal("the Kubernetes stand-in was sent no request")
	}
	for _, got := range a.bearers {
		if got != want {
			t.Fatalf("the Kubernetes stand-in was sent the Authorization %q, want %q on every request", got, want)
		}
	}
}

// kubeRefusalConfig is how the row's runtime reaches the stand-in at host: sandboxRESTConfig's JSON
// both ways, under kubeRefusalBearer.
func kubeRefusalConfig(host string) *rest.Config {
	config := sandboxRESTConfig(host)
	config.BearerToken = kubeRefusalBearer
	return config
}

// TestAKubernetesRefusalOfAnIssueSuspensionReachesTheOutboxWithNoCredential: an issue_suspend row
// (record.IssueSuspend, the durable close of an issue) is run by the outbox (RunOnce, runRow,
// execute, suspendIssue) over the real Sandbox runtime (sandbox.New, Runtime.SuspendIssue), whose
// Kubernetes API is a stand-in that refuses the runtime's read of the issue's Sandbox with a 403
// Status echoing the bearer the read carried. The store authorizes the close (store.IssueSuspension:
// a root that lingers at the row's generations, no role stop before the row, no stored claim), so
// the runtime reads the Sandbox, and the refusal comes back as the row's error. settle logs it
// ("outbox row failed", through a handler built as `legion start` builds it, redact.HandlerOptions)
// and retries the row (retry, retryAt), whose last_error the store keeps
// (record.Store.RetryOutbox). Neither the stored error nor the log holds the bearer.
//
// Control: the same refusal read by a client-go client of the same configuration with no runtime
// between them holds the bearer, and the stand-in was sent the bearer on every request; so the row's
// texts would hold it had the runtime passed the API server's words on. Captures
// kubeerror.daemon/outbox-retry (the row's stored last_error) and kubeerror.daemon/outbox-log (the
// outbox's whole log).
func TestAKubernetesRefusalOfAnIssueSuspensionReachesTheOutboxWithNoCredential(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	lingerUntil := now.Add(time.Hour)
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LastDispatchSeq: 1, LingerUntil: &lingerUntil}
	putOutboxIssue(t, pool, records, issue)
	const name = "legion-legion-legion-208"
	api := &refusingSandboxAPI{issueSandboxAPI: newIssueSandboxAPI(issueSandbox(name, "sandbox-uid", issue.Tree, issue.Key)), refused: name}
	server := httptest.NewServer(api)
	// Cleanups run last first: the runtime and the supervisor stop before the server closes, since
	// a server closed while the runtime's watches are open waits for them.
	t.Cleanup(func() { cancel(); server.Close() })
	config := kubeRefusalConfig(server.URL)

	// Control: the stand-in's refusal, as client-go makes an error of it, holds the bearer.
	raw, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	_, controlErr := raw.Resource(refusedSandboxGVR).Namespace(sandboxNamespace).Get(ctx, name, metav1.GetOptions{})
	if controlErr == nil || !strings.Contains(controlErr.Error(), oauth2calltest.Token) {
		t.Fatalf("client-go's error of the stand-in's refusal is %v, want one that holds %q: the row could not see a credential", controlErr, oauth2calltest.Token)
	}
	controlled := api.refusalCount()

	rt, err := sandbox.New(ctx, config, issueSandboxOptions(t, st, quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps.Runtime = rt
	t.Cleanup(sup.stop)
	logs := &syncBuffer{}
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		log: slog.New(slog.NewJSONHandler(logs, redact.HandlerOptions(nil))), now: func() time.Time { return now }}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: 1, TreeGeneration: 1}, now))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	api.requireSentBearer(t, "Bearer "+kubeRefusalBearer)
	if refused := api.refusalCount() - controlled; refused < 1 {
		t.Fatalf("the runtime's read of Sandbox %s was refused %d times, want at least once: the row never met the refusal", name, refused)
	}
	var attempts int
	var lastError string
	if err := pool.QueryRow(ctx, "select attempts, last_error from outbox where kind = $1", string(record.OutboxKindIssueSuspend)).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("read the issue_suspend row after its attempt: %v", err)
	}
	// want is the row's error as the head's runtime keeps it: the outbox's suspension of the
	// issue, the runtime's read of its Sandbox, and the projection of the API server's answer.
	const want = "suspend issue LEGION-208: read Sandbox: kubernetes: get sandboxes " + name + ": the API server answered 403 Forbidden"

	t.Run("outbox-retry", func(t *testing.T) {
		oauth2calltest.RequireNone(t, "kubeerror.daemon/outbox-retry", lastError)
		if attempts != 1 {
			t.Errorf("the issue_suspend row is on attempt %d, want 1: retried once, after the refusal", attempts)
		}
		if lastError != want {
			t.Errorf("the issue_suspend row's last error is %q, want %q", lastError, want)
		}
	})
	t.Run("outbox-log", func(t *testing.T) {
		logged := logs.String()
		oauth2calltest.RequireNone(t, "kubeerror.daemon/outbox-log", logged)
		failed := 0
		for _, line := range strings.Split(strings.TrimSuffix(logged, "\n"), "\n") {
			var entry struct {
				Msg   string `json:"msg"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("decode the outbox's log line %q: %v", line, err)
			}
			if entry.Msg != "outbox row failed" {
				continue
			}
			failed++
			if entry.Error != want {
				t.Errorf("the outbox's failure line carries the error %q, want %q", entry.Error, want)
			}
		}
		if failed != 1 {
			t.Errorf("the outbox logged %d failure lines, want one:\n%s", failed, logged)
		}
	})
}
