package legionbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The daemon stand-in: INFRA's Legion daemon as the server's Legion state intake reads it whole,
// GET /legion/v1/state (legion.StatePath), answering a state that puts DefaultCards of the loaded
// INFRA issues on the Legion board. The answer is legion.State encoded as JSON, the members of the
// daemon's own document the whole read decodes (legion/state.go): each issue's key, phase, tree,
// slot and claims, the admission block, the settings and the daemon block. The read decodes the
// document loosely and asks for the route's observed view (`?view=observed`), which carries only
// those members, so the stand-in answers the same document whatever the query says, and a
// document of these members alone is read as the daemon's is.
//
// The intake judges a whole read good only where the state is the setting's project's and could
// be a starting point for state events (legion's fetchWhole): the daemon block names the project,
// a daemon.stateSeq above 0 and a daemon.stateId, and the settings carry a dispatch_url. A read
// that misses one is a failed read: the board's header names the failure, and the drive's
// catch-up, which waits for a good read (cmd/wait.go), fails at its bound. The stand-in publishes
// no state event, so the good read the intake makes as it starts is the one read of the stand-in
// a server makes, and its observations stand for the whole drive.
//
// The cards are the open issues the loaded observations hold a working phase for, newest first,
// each answered with the phase, the slot and the claims those observations hold, so the server's
// first read of the stand-in moves none of them. That read writes `null` for every observed issue
// the state does not carry (legion.State.Observations), and from then on legion_current holds the
// cards' issues alone. Every admitted issue without a slot stands in the admission block's waiting
// line, so the board puts it on a card of the Queue column rather than counting it on its root's;
// every issue with a slot is active, and every card's root is a live tree.

// DefaultCards is how many cards the stand-in puts on the board: R8's 1,000.
const DefaultCards = 1000

// standInPhases are the phases a card of the stand-in carries: those the loaded observations write
// as a role's stretch begins (infra.go's rolePhases), each of which the board puts on a column.
var standInPhases = []string{legion.PhaseAdmitted, legion.PhasePlanning, legion.PhaseImplementing, legion.PhaseTesting,
	legion.PhaseReviewing, legion.PhaseMerging}

// standInSlots is the admission cap the stand-in reports: the slot indexes the loaded
// observations give (copyObservations' number mod 8).
const standInSlots = 8

// The stand-in's daemon block and settings: what makes a whole read of it a good one (the comment
// above). standInStateSeq is 1, the boot event a daemon that runs a workflow commits first, and
// standInStateID one fixed store, so every arm's server takes the same starting point.
// standInDispatchURL is the Dispatch the daemon would publish its state events to: a name under
// .invalid (RFC 2606), since the stand-in publishes none and the read only checks one is named.
const (
	standInStateSeq    = 1
	standInStateID     = "5577b3c1-7e2a-4d0f-9b6e-1c8a2f4d5577"
	standInDispatchURL = "http://dispatch.invalid"
)

// StandInState is the state the stand-in answers for project: up to cards open issues whose newest
// observed phase is a working one, newest first, each with the slot and the claims its newest
// observations hold, the admission block over them, and the daemon block and settings of a daemon
// of project that runs a workflow (the comments above say what each holds).
func StandInState(ctx context.Context, pool *store.Pool, project string, cards int) (legion.State, error) {
	current, err := legion.ReadCurrent(ctx, pool, project)
	if err != nil {
		return legion.State{}, err
	}
	fields := map[string]map[string]json.RawMessage{}
	for _, one := range current.Newest {
		if one.IssueKey == legion.AdmissionKey {
			continue
		}
		if fields[one.IssueKey] == nil {
			fields[one.IssueKey] = map[string]json.RawMessage{}
		}
		fields[one.IssueKey][one.Field] = one.Value
	}
	rows, err := pool.Query(ctx, `
		select key, number, coalesce(parent_key, '')
		from issues
		where project_key = $1 and closed_at is null and key = any($2)
		order by number desc`, project, mapsKeys(fields))
	if err != nil {
		return legion.State{}, fmt.Errorf("read the open issues the observations name: %w", err)
	}
	type candidate struct {
		key, parent string
		number      int
	}
	var open []candidate
	for rows.Next() {
		var one candidate
		if err := rows.Scan(&one.key, &one.number, &one.parent); err != nil {
			rows.Close()
			return legion.State{}, fmt.Errorf("read the open issues the observations name: %w", err)
		}
		open = append(open, one)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return legion.State{}, fmt.Errorf("read the open issues the observations name: %w", err)
	}

	state := legion.State{
		Issues:    map[string]legion.Issue{},
		Admission: &legion.Admission{Cap: new(standInSlots)},
		Settings:  map[string]json.RawMessage{"dispatch_url": json.RawMessage(strconv.Quote(standInDispatchURL))},
		Daemon:    &legion.Daemon{Project: project, StateSeq: new(int64(standInStateSeq)), StateID: standInStateID},
	}
	trees := map[string]bool{}
	var waiting []candidate
	for _, one := range open {
		if len(state.Issues) == cards {
			break
		}
		observed := fields[one.key]
		phase := legion.JSONString(observed[legion.FieldPhase])
		if !slices.Contains(standInPhases, phase) {
			continue
		}
		root := one.key
		if one.parent != "" {
			root = one.parent
		}
		issue := legion.Issue{Key: one.key, Phase: phase, Tree: new(root), Workers: map[string]legion.Worker{}}
		if slot, err := strconv.Atoi(string(observed[legion.FieldSlot])); err == nil {
			issue.Slot = &legion.Slot{Index: slot}
			state.Admission.Active = append(state.Admission.Active, one.key)
		} else if phase == legion.PhaseAdmitted {
			waiting = append(waiting, one)
		}
		for field, value := range observed {
			role, isClaim := strings.CutPrefix(field, legion.ClaimFieldPrefix)
			claim := legion.JSONString(value)
			if !isClaim || claim == "" {
				continue
			}
			if role == "architect" {
				issue.Architect = &legion.Claim{State: claim}
				continue
			}
			issue.Workers[role] = legion.Worker{Claim: legion.Claim{State: claim}}
		}
		state.Issues[one.key] = issue
		if !trees[root] {
			trees[root] = true
			state.Admission.Trees = append(state.Admission.Trees, root)
		}
	}
	// The waiting line in Dispatch's rank order, which the loaded issues' ranks follow by number.
	slices.SortFunc(waiting, func(a, b candidate) int { return a.number - b.number })
	for _, one := range waiting {
		state.Admission.Waiting = append(state.Admission.Waiting, one.key)
	}
	slices.Sort(state.Admission.Active)
	slices.Sort(state.Admission.Trees)
	return state, nil
}

// mapsKeys is the keys of fields, sorted.
func mapsKeys(fields map[string]map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// StandIn is the stand-in's handler: GET /legion/v1/state, whatever its query, answers state's
// JSON, as the daemon's route does, with no credential asked; every other request is 404.
func StandIn(state legion.State) (http.Handler, error) {
	body, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode the stand-in's state: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+legion.StatePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	return mux, nil
}
