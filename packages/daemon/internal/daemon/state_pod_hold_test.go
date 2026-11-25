package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// servedHold is an issue's `podHold` as `GET /legion/v1/state` writes it. The test reads the
// wire through a type of its own, so it builds against a daemon that writes no such key and
// fails there on what the body carries.
type servedHold struct {
	Reason  string    `json:"reason"`
	FirstAt time.Time `json:"firstAt"`
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// servedHolds is the `podHold` of each issue of keys in the state server answers, keyed by issue
// and nil for an issue that carries none. An issue of keys the body does not name fails the test:
// an absent key is a hold the state does not show only where the issue it belongs to is there.
func servedHolds(t *testing.T, server *httptest.Server, keys ...string) map[string]*servedHold {
	t.Helper()
	response, err := (&http.Client{Timeout: 30 * time.Second}).Get(server.URL + "/legion/v1/state")
	if err != nil {
		t.Fatalf("GET the state: %v", err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read the state's body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the state = %d %s, want 200", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var body struct {
		Issues map[string]struct {
			PodHold *servedHold `json:"podHold"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	holds := map[string]*servedHold{}
	for _, key := range keys {
		issue, named := body.Issues[key]
		if !named {
			t.Fatalf("the state names no issue %s, so it cannot say whether that issue's pod is held", key)
		}
		holds[key] = issue.PodHold
	}
	return holds
}

// Each issue of `GET /legion/v1/state` names its pod's standing hold: the idle rule's word for it
// as idleHoldNames spells it, when its streak began, its last pass, and its detail cut to the
// first 200 characters. A pod with no hold row carries no hold; a
// pod whose hold the state showed carries none once the pod is released (RecordPodRelease ends
// its streak) and none once a pass ends the streak (EndPodHoldStreaks); and a pass under another
// reason leaves one standing hold, the new one, since RecordPodHold ends every other reason's
// streak in the transaction that records it.
//
// Each subtest that expects no hold first reads the hold shown, so each fails against a state
// that carries no hold at all, which is the state before this change.
func TestTheStateNamesAPodsStandingHold(t *testing.T) {
	cfg := testConfig(t)
	held, quiet := cfg.Project+"-1", cfg.Project+"-2"
	released, ended, moved := cfg.Project+"-3", cfg.Project+"-4", cfg.Project+"-5"
	keys := []string{held, quiet, released, ended, moved}
	run := previousRun{}
	for _, key := range keys {
		run.issues = append(run.issues, runningRoot(key, phase.Implementing))
	}
	putPreviousRun(t, cfg, run)

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	// pod_holds is keyed on the claim token's spelling of the project, which is what the idle
	// pass writes under and what the daemon's source reads under.
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	view := &source{
		store: st, records: record.NewStore(), project: cfg.Project, projectToken: project,
		runtime: "tmux", admissionCap: cfg.AdmissionCap, startedAt: time.Now(),
		deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
		reports:    func() map[claim.Token]capabilities.Report { return nil },
	}
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: view, StateTransactions: st.Pool(), Log: quietLogger(),
	}).Handler)
	t.Cleanup(server.Close)

	hold := func(t *testing.T, issue string, reason workflow.IdleHold, detail string, at time.Time) {
		t.Helper()
		if err := st.RecordPodHold(ctx, store.PodHold{Project: project, Issue: issue, Reason: reason.String(), Detail: detail, At: at}); err != nil {
			t.Fatalf("record a %s pass of %s: %v", reason, issue, err)
		}
	}
	shown := func(t *testing.T, issue string, reason workflow.IdleHold) {
		t.Helper()
		got := servedHolds(t, server, issue)[issue]
		if got == nil {
			t.Fatalf("the state carries no podHold for %s, whose pod is held for %s", issue, reason)
		}
		if got.Reason != reason.String() {
			t.Fatalf("the state names %s's hold %q, want %q", issue, got.Reason, reason.String())
		}
	}
	first := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)

	// Two passes of one streak, with a detail longer than the state shows: the start is the
	// first pass, the last pass the second, and the detail its first 200 characters, counted as
	// characters and not bytes (each `é` is two bytes).
	t.Run("held", func(t *testing.T) {
		detail := strings.Repeat("async-job,é", 25)
		last := first.Add(2 * time.Hour)
		hold(t, held, workflow.IdleHoldBackgroundWork, detail, first)
		hold(t, held, workflow.IdleHoldBackgroundWork, detail, last)

		holds := servedHolds(t, server, held, quiet)
		got := holds[held]
		if got == nil {
			t.Fatalf("the state carries no podHold for %s, whose pod is held for background_work", held)
		}
		// The 200 is the spec's own number, written here rather than read from the daemon, so the
		// test holds the daemon to it.
		want := servedHold{
			Reason: "background_work", FirstAt: first, At: last,
			Detail: string([]rune(detail)[:200]),
		}
		if got.Reason != want.Reason || !got.FirstAt.Equal(want.FirstAt) || !got.At.Equal(want.At) || got.Detail != want.Detail {
			t.Fatalf("the state names %s's hold %+v, want %+v", held, *got, want)
		}
		if holds[quiet] != nil {
			t.Fatalf("the state names a hold %+v for %s, whose pod has no hold row", *holds[quiet], quiet)
		}
	})

	t.Run("released", func(t *testing.T) {
		hold(t, released, workflow.IdleHoldImage, "", first)
		shown(t, released, workflow.IdleHoldImage)
		if _, _, err := st.RecordPodRelease(ctx, store.PodRelease{Project: project, Issue: released, At: first.Add(time.Minute)}); err != nil {
			t.Fatalf("record the release of %s: %v", released, err)
		}
		if got := servedHolds(t, server, released)[released]; got != nil {
			t.Fatalf("the state names a hold %+v for %s after its pod's release", *got, released)
		}
	})

	t.Run("ended", func(t *testing.T) {
		hold(t, ended, workflow.IdleHoldDelivery, "", first)
		shown(t, ended, workflow.IdleHoldDelivery)
		if err := st.EndPodHoldStreaks(ctx, project, ended); err != nil {
			t.Fatalf("end the hold streaks of %s: %v", ended, err)
		}
		if got := servedHolds(t, server, ended)[ended]; got != nil {
			t.Fatalf("the state names a hold %+v for %s after its streak ended", *got, ended)
		}
	})

	t.Run("another_reason", func(t *testing.T) {
		hold(t, moved, workflow.IdleHoldInTurn, "", first)
		shown(t, moved, workflow.IdleHoldInTurn)
		next := first.Add(30 * time.Minute)
		hold(t, moved, workflow.IdleHoldReminder, "", next)

		got := servedHolds(t, server, moved)[moved]
		want := servedHold{Reason: workflow.IdleHoldReminder.String(), FirstAt: next, At: next}
		if got == nil || got.Reason != want.Reason || !got.FirstAt.Equal(want.FirstAt) || !got.At.Equal(want.At) || got.Detail != "" {
			t.Fatalf("after a pass under another reason the state names %s's hold %+v, want the new one, %+v", moved, got, want)
		}
		var standing int
		if err := st.Pool().QueryRow(ctx, `select count(*) from pod_holds where project = $1 and issue = $2 and not ended`,
			project, moved).Scan(&standing); err != nil {
			t.Fatalf("count the standing holds of %s: %v", moved, err)
		}
		if standing != 1 {
			t.Fatalf("%s's pod has %d standing holds after a pass under another reason, want 1", moved, standing)
		}
	})
}
