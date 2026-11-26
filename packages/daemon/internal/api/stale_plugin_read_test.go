package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// failedInstallRead is the line the stale-caller refusal writes for a read of whether a claim's
// launch installs a plugin that failed (staleClaim).
const failedInstallRead = "api: the read of whether this claim's launch installs a plugin failed, so the call is not refused as one from a stale plugin"

// A read of whether a stale claim's launch installs a plugin that fails while its caller waits is
// the store's fault, and one line names it. A read that fails because the caller hung up says
// nothing of the store, and no line is written for it. The rule is asked in both cases: the read
// is counted, so the quiet case is one the read did reach.
func TestAFailedInstallReadIsLoggedOnlyWhileItsCallerWaits(t *testing.T) {
	logs := &bytes.Buffer{}
	var reads atomic.Int32
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		PromptAgents: testPromptAgents, PluginRelaunchable: true,
		NoPluginInstall: func(ctx context.Context, c supervise.Claim) (bool, error) {
			reads.Add(1)
			if err := ctx.Err(); err != nil {
				return false, fmt.Errorf("the role %s of the plan of %s: %w", c.Role, c.Issue, err)
			}
			return false, errors.New("the role " + string(c.Role) + " of the plan of " + c.Issue + ": connection refused")
		},
		State: fakeSource{}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		Log: slog.New(slog.NewJSONHandler(logs, nil)),
	}).Handler
	stale := DaemonAPIVersion() - 1

	for _, tc := range []struct {
		name   string
		issue  string
		hungUp bool
		lines  int
	}{
		{"while the caller waits", "LEGION-208", false, 1},
		{"once the caller hung up", "LEGION-209", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, boot := h.launch(tc.issue, claim.RoleTester)
			session := "ses_" + tc.issue
			current := h.registerContract(boot, session, DaemonAPIVersion(), true)
			h.makeStale(t, token, session, current.Secret, stale)
			body, err := json.Marshal(claim.ReadyRequest{ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation})
			if err != nil {
				t.Fatal(err)
			}
			ctx, hangUp := context.WithCancel(context.Background())
			defer hangUp()
			if tc.hungUp {
				hangUp()
			}
			request := httptest.NewRequest(http.MethodPost, "/legion/v1/claims/ready", bytes.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			before, logged := reads.Load(), logs.Len()

			h.handler.ServeHTTP(httptest.NewRecorder(), request)

			if reads.Load() == before {
				t.Fatalf("the ready of %s read nothing of whether its launch installs a plugin: this case proves nothing", tc.issue)
			}
			if got := strings.Count(logs.String()[logged:], failedInstallRead); got != tc.lines {
				t.Fatalf("the ready of %s wrote %d lines of a failed read, want %d; it logged:\n%s", tc.issue, got, tc.lines, logs.String()[logged:])
			}
		})
	}
}
