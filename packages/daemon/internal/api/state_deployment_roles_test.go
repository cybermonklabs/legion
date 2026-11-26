package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// servedRoles is what a state body serves of the roles a deployment defines: the issues by key, the
// admission's active issues, and the role of each stranded claim, waiting wake and pause stop by
// issue.
type servedRoles struct {
	Issues    map[string]map[string]any `json:"issues"`
	Admission struct {
		Active []string `json:"active"`
	} `json:"admission"`
	StrandedClaims []struct {
		Issue string `json:"issue"`
		Role  string `json:"role"`
	} `json:"strandedClaims"`
	WakesWaiting []struct {
		Issue string `json:"issue"`
		Role  string `json:"role"`
	} `json:"wakesWaiting"`
	Paused *struct {
		WaitsOn []struct {
			Issue string `json:"issue"`
			Role  string `json:"role"`
		} `json:"waitsOn"`
	} `json:"paused"`
}

// leftOutLine is one line the daemon writes for a claim a narrowing leaves out (leftAClaimOut).
type leftOutLine struct {
	Message        string `json:"msg"`
	PluginContract int    `json:"pluginContract"`
	Claim          string `json:"claim"`
}

// stateServedWithLog is GET /legion/v1/state asked by a caller that states contract in
// pluginContractHeader, the body decoded, the narrowing lines that call wrote and the lines of
// each claim left out. A pod's claims could not place a caller just below deploymentRolesContract:
// a pod's reckoning counts no claim at shapedAnswersContract or above (podContracts), since a
// build of that contract or later states its own.
func stateServedWithLog(t *testing.T, state State, contract int) (servedRoles, []narrowingLine, []leftOutLine) {
	t.Helper()
	var logged bytes.Buffer
	options := stateOptions(state, nil)
	options.Log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := stateStating(t, NewServer("127.0.0.1", 8437, options).Handler, "192.0.2.1:41000", contract)
	var served servedRoles
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	var narrowings []narrowingLine
	var left []leftOutLine
	for _, written := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if written == "" {
			continue
		}
		var line narrowingLine
		var out leftOutLine
		if err := json.Unmarshal([]byte(written), &line); err != nil {
			t.Fatalf("the daemon logged %q, which is not one JSON record: %v", written, err)
		}
		switch line.Message {
		case narrowedTheState:
			narrowings = append(narrowings, line)
		case leftAClaimOut:
			if err := json.Unmarshal([]byte(written), &out); err != nil {
				t.Fatal(err)
			}
			left = append(left, out)
		}
	}
	return served, narrowings, left
}

// deploymentRolesDropped is the `dropped` of the band of deploymentRolesContract's entry.
func deploymentRolesDropped() string {
	for _, row := range stateNarrowings() {
		if row.contract == entryNumber(deploymentRolesContract) {
			return row.dropped
		}
	}
	return ""
}

// A caller older than deploymentRolesContract's entry sees an issue at step as at work: for a
// caller at each contract the daemon serves below it, LEGION-240, at step, is in `issues` with the
// phase implementing and in `admission.active`, and names no start that waits for its pod;
// LEGION-241, held from step, is held from implementing for a caller at recordFieldsContract or
// above and names no heldFrom below it, where withoutRecordFields already clears it, and keeps its
// checker's worker; neither names a stepRole; no stranded claim names the checker; and one line
// names the row. At the contract, LEGION-240 is at step and names the checker's start as waiting
// for its pod, both name the checker, and the checker's stranded claim is served.
func TestAnOlderCallerSeesAStepIssueAsAtWork(t *testing.T) {
	var contracts []int
	for _, row := range stateNarrowings() {
		if below := row.contract - 1; below >= supervise.PodUpdateContract-1 && !slices.Contains(contracts, below) {
			contracts = append(contracts, below)
		}
	}
	added := entryNumber(deploymentRolesContract)
	contracts = append(contracts, added)
	for _, contract := range contracts {
		t.Run(fmt.Sprint(contract), func(t *testing.T) {
			served, narrowings, _ := stateServedWithLog(t, populatedState(t), contract)
			older := contract < added
			step, held := served.Issues["LEGION-240"], served.Issues["LEGION-241"]
			if step == nil || held == nil {
				t.Fatalf("the issues served = %v, want LEGION-240 and LEGION-241 among them", issueKeys(served.Issues))
			}
			if !slices.Contains(served.Admission.Active, "LEGION-240") {
				t.Fatalf("admission.active = %v, want LEGION-240 in it", served.Admission.Active)
			}
			wantPhase, wantRole := "step", "checker"
			if older {
				wantPhase, wantRole = "implementing", ""
			}
			if step["phase"] != wantPhase {
				t.Errorf("LEGION-240 is served at %v, want %s", step["phase"], wantPhase)
			}
			for key, issue := range map[string]map[string]any{"LEGION-240": step, "LEGION-241": held} {
				if role, _ := issue["stepRole"].(string); role != wantRole {
					t.Errorf("%s is served the stepRole %q, want %q", key, role, wantRole)
				}
			}
			heldFrom, named := held["heldFrom"]
			switch {
			case contract < recordFieldsContract && named:
				t.Errorf("LEGION-241 names heldFrom %v below recordFieldsContract, want none", heldFrom)
			case contract >= recordFieldsContract && heldFrom != map[bool]string{true: "implementing", false: "step"}[older]:
				t.Errorf("LEGION-241 is served heldFrom %v, want %s", heldFrom, map[bool]string{true: "implementing", false: "step"}[older])
			}
			waiting, named := step["startWaitsForPod"]
			switch {
			case older && named:
				t.Errorf("LEGION-240 names startWaitsForPod %v below deploymentRolesContract, want none", waiting)
			case !older && fmt.Sprint(waiting) != "[checker]":
				t.Errorf("LEGION-240 is served startWaitsForPod %v, want [checker]", waiting)
			}
			if workers, _ := held["workers"].(map[string]any); workers["checker"] == nil {
				t.Errorf("LEGION-241 is served the workers %v, want its checker's among them", held["workers"])
			}
			checkers := 0
			for _, c := range served.StrandedClaims {
				if c.Role == "checker" {
					checkers++
				}
			}
			if want := map[bool]int{true: 0, false: 1}[older]; checkers != want {
				t.Errorf("the stranded claims served name the checker %d times, want %d", checkers, want)
			}
			lines := 0
			for _, line := range narrowings {
				if line.Dropped == deploymentRolesDropped() {
					lines++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[older]; lines != want {
				t.Errorf("the daemon wrote %d lines naming the row of deploymentRolesContract, want %d", lines, want)
			}
		})
	}
}

// A caller older than deploymentRolesContract's entry reads a pending task's phase as a strict list
// without step, so the checker's task, queued for step on an issue at step, is served queued for
// implementing below the contract and for step at it.
func TestAnOlderCallerSeesATaskPendingAtStepAsAtImplementing(t *testing.T) {
	state := populatedState(t)
	issues := maps.Clone(state.Issues)
	atStep := issues["LEGION-240"]
	atStep.StartWaitsForPod = nil
	atStep.Workers = map[claim.Role]PhaseView{"checker": {Claim: ClaimView{Session: "ses_checker", State: "working",
		PendingTask: &PendingTaskView{Phase: phase.Step, QueuedAt: time.Date(2026, 9, 22, 9, 16, 2, 0, time.UTC)}}}}
	issues["LEGION-240"] = atStep
	state.Issues = issues
	added := entryNumber(deploymentRolesContract)
	for _, contract := range []int{added - 1, added} {
		served, _, _ := stateServedWithLog(t, state, contract)
		want := map[bool]string{true: "implementing", false: "step"}[contract < added]
		workers, _ := served.Issues["LEGION-240"]["workers"].(map[string]any)
		checker, _ := workers["checker"].(map[string]any)
		view, _ := checker["claim"].(map[string]any)
		task, _ := view["pendingTask"].(map[string]any)
		if task["phase"] != want {
			t.Errorf("at contract %d the checker's pending task is served %v, want it at %s", contract, task, want)
		}
	}
}

// issueKeys is the keys of m, sorted.
func issueKeys(m map[string]map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Each claim the row leaves out for an older caller is logged, one line each, naming the caller's
// plugin contract and the claim: the checker's stranded claim on LEGION-241, its waiting wake and
// the open pause's wait on its stop are three lines, and none of the three is served.
func TestEachClaimLeftOutForAnOlderCallerIsLogged(t *testing.T) {
	state := populatedState(t)
	state.WakesWaiting = append(slices.Clone(state.WakesWaiting), WakeWaiting{Claim: "legion-legion-legion-241-checker", Issue: "LEGION-241", Role: "checker",
		Kind: "launch", DueAt: time.Date(2026, 9, 22, 9, 18, 20, 0, time.UTC)})
	paused := *state.Paused
	paused.WaitsOn = append(slices.Clone(paused.WaitsOn), PauseStopView{Claim: "legion-legion-legion-241-checker", Issue: "LEGION-241", Role: "checker",
		AskedAt: time.Date(2026, 9, 22, 9, 18, 30, 0, time.UTC)})
	state.Paused = &paused
	contract := entryNumber(deploymentRolesContract) - 1

	served, _, left := stateServedWithLog(t, state, contract)

	want := []leftOutLine{
		{Message: leftAClaimOut, PluginContract: contract, Claim: "checker of LEGION-241"},
		{Message: leftAClaimOut, PluginContract: contract, Claim: "checker of LEGION-241"},
		{Message: leftAClaimOut, PluginContract: contract, Claim: "checker of LEGION-241"},
	}
	if !slices.Equal(left, want) {
		t.Fatalf("the left-out lines = %+v, want %+v", left, want)
	}
	for _, wake := range served.WakesWaiting {
		if wake.Role == "checker" {
			t.Fatalf("the waiting wakes served name the checker on %s, want none for a caller below the contract", wake.Issue)
		}
	}
	if served.Paused == nil {
		t.Fatal("the state served carries no `paused`, want the open pause for a caller above pauseContract")
	}
	for _, stop := range served.Paused.WaitsOn {
		if stop.Role == "checker" {
			t.Fatalf("the open pause served waits on the checker's stop on %s, want none for a caller below the contract", stop.Issue)
		}
	}
}

// servedWithoutDeploymentRoles changes want, a state body as decoded JSON, to what the band of
// deploymentRolesContract serves a caller below it (withoutDeploymentRoles and
// withoutDeploymentRoleClaims): each phase, heldFrom and pending task's phase of step served as
// implementing, no stepRole, no startWaitsForPod, and no stranded claim, waiting wake or pause
// stop of a role outside Legion's six. The tests that build a narrowed body from the full one
// apply it as they take out the keys of every other row.
func servedWithoutDeploymentRoles(want map[string]any) {
	issues, _ := want["issues"].(map[string]any)
	for _, issue := range issues {
		issue := issue.(map[string]any)
		if issue["phase"] == "step" {
			issue["phase"] = "implementing"
		}
		if issue["heldFrom"] == "step" {
			issue["heldFrom"] = "implementing"
		}
		delete(issue, "stepRole")
		delete(issue, "startWaitsForPod")
		views := []any{issue["architect"]}
		if workers, ok := issue["workers"].(map[string]any); ok {
			for _, worker := range workers {
				if worker, ok := worker.(map[string]any); ok {
					views = append(views, worker["claim"])
				}
			}
		}
		for _, view := range views {
			if view, ok := view.(map[string]any); ok {
				if task, ok := view["pendingTask"].(map[string]any); ok && task["phase"] == "step" {
					task["phase"] = "implementing"
				}
			}
		}
	}
	keepBuiltin := func(entries []any) []any {
		kept := []any{}
		for _, entry := range entries {
			if role, _ := entry.(map[string]any)["role"].(string); claim.IsBuiltin(claim.Role(role)) {
				kept = append(kept, entry)
			}
		}
		return kept
	}
	for _, key := range []string{"strandedClaims", "wakesWaiting"} {
		if entries, ok := want[key].([]any); ok {
			want[key] = keepBuiltin(entries)
		}
	}
	if paused, ok := want["paused"].(map[string]any); ok {
		if stops, ok := paused["waitsOn"].([]any); ok {
			paused["waitsOn"] = keepBuiltin(stops)
		}
	}
}

// withoutDeploymentRoleKeys is body, a state body, as the band of deploymentRolesContract serves
// it (servedWithoutDeploymentRoles), written back as JSON.
func withoutDeploymentRoleKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var state map[string]any
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	servedWithoutDeploymentRoles(state)
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
