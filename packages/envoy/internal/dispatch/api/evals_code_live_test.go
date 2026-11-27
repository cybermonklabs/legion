package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival"
	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// codeShare is the share of series of the eval id over the window of a body of the evals list
// read, and whether the body holds the series.
func codeShare(t *testing.T, body json.RawMessage, id, series string) (*float64, bool) {
	t.Helper()
	var list evalListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode the evals body: %v", err)
	}
	for _, item := range list.Items {
		if item.ID != id {
			continue
		}
		for _, values := range item.Series {
			if values.Series.Series == series {
				return values.Window["share"], true
			}
		}
	}
	return nil, false
}

// TestAMeasurementReachesTheOpenEvalsView (k6): a page holds INFRA's evals over the 28 days from
// a branch's first day, and another page OTHR's, both answered and quiet, after the eval day
// refresher has run through today. Its code evals read "no data": no day is measured, and no build
// has written its groups' series yet. The code survival run then measures the seven days that wait
// on INFRA's default repository, served by the GitHub double; its results transaction writes the
// group series and the day rows, and the drain after its commit puts their changes in the log.
// INFRA's page gets one delta, with no reload, that upserts both code evals, lands on the route's
// body and reads the agents' survival share; OTHR's page is sent nothing.
func TestAMeasurementReachesTheOpenEvalsView(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "INFRA", "OTHR")
	ctx := t.Context()
	const repo = "acme/widgets"
	if _, err := database.Pool.Exec(ctx, `update projects set default_repo = $1 where key = 'INFRA'`, repo); err != nil {
		t.Fatalf("give INFRA its default repository: %v", err)
	}
	groups := []delivery.CodeGroup{{Name: "agents", BranchPrefixes: []string{"agent/"}}, {Name: "others"}}
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{Project: "INFRA", CodeGroups: &groups})

	today := measures.Midnight(time.Now())
	first := today.AddDate(0, 0, -45)
	history := codesurvivaltest.NewHistory(t)
	var lines []string
	for n := range 20 {
		at := first.AddDate(0, 0, n).Add(10 * time.Hour)
		for i := range 4 {
			lines = append(lines, fmt.Sprintf("day %02d line %d of the measured branch, long enough to be a line", n, i))
		}
		if n%3 == 2 {
			lines = lines[2:]
		}
		history.Repo.Write("src/file.txt", strings.Join(lines, "\n")+"\n")
		head := fmt.Sprintf("feature/%d", n+1)
		if n%2 == 0 {
			head = fmt.Sprintf("agent/%d", n+1)
		}
		history.Land(codesurvivaltest.Pull{Number: n + 1, HeadRef: head, Author: "someone", Title: fmt.Sprintf("change %d", n+1)}, at)
	}
	double := codesurvivaltest.New(t)
	double.Serve(repo, history.Branch())

	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	refreshEvalDays(t, registry, database, today.Add(13*time.Hour))
	rig := hubOn(t, handler, database, deps, newStillHubClock(), nil)
	window := evalsDays(first, first.AddDate(0, 0, 28))
	frame, otherFrame := evalsLiveView("INFRA", window, ""), evalsLiveView("OTHR", window, "")
	conn, other := newFakeLiveConn(), newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("e", frame))
	rig.hub.Subscribe(other, subscribeFrame("o", otherFrame))
	held := conn.snapshot(t, "e").Body
	other.snapshot(t, "o")
	rig.quiesce(t, frame)
	rig.quiesce(t, otherFrame)
	rig.requireNothingSent(t, conn, "the snapshot, before the measurement")
	// No build has written the project's group series yet, so the page holds no agents' series, or
	// one with no data.
	if share, found := codeShare(t, held, "INFRA/code-survival-30d", "survival:agents"); share != nil {
		t.Fatalf("before the measurement the page reads the agents' survival share %g (series held %t), want no data", *share, found)
	}

	run := &codesurvival.Run{Pool: database.Pool, Client: double.Client(t), Attributor: deps.Attributor, Build: testServerBuild,
		Now: func() time.Time { return first.AddDate(0, 0, 39).Add(time.Hour) }}
	run.Pass(ctx)

	rig.quiesce(t, frame)
	delta := conn.delta(t, "e")
	upserted := liveUpsertIDs(t, delta, "items")
	for _, id := range []string{"INFRA/code-survival-30d", "INFRA/post-merge-rework-30d"} {
		if !slices.Contains(upserted, id) {
			t.Errorf("the measurement's delta upserts %v, want %s among them", upserted, id)
		}
	}
	held = applyLiveDelta(t, evalsView, held, delta)
	route := getBody(t, handler, evalsTarget(t, "INFRA", held, ""))
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, held)), liveComparable(t, mustEncodeLive(t, evalsView, route)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the route's body:\nheld: %v\nGET:  %v", got, want)
	}
	share, found := codeShare(t, held, "INFRA/code-survival-30d", "survival:agents")
	if !found || share == nil {
		t.Fatalf("after the measurement the page reads the agents' survival share %v (series held %t), want a number", share, found)
	}
	rig.quiesce(t, otherFrame)
	rig.requireNothingSent(t, other, "the measurement of INFRA's repository, on OTHR's page")
	rig.requireNothingSent(t, conn, "the measurement's one delta")
	t.Logf("the measurement's delta of seq %s upserts %v; the page reads the agents' survival share %g with no reload", delta.Seq, upserted, *share)
}
