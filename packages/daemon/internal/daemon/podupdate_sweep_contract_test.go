package daemon

// The sweep's contract with the one rule of whom an update round asks (supervise's
// Claim.OwesUpdateAsk): from the daemon's tick (supervision.reconcilePodUpdates) to the publish of
// each ask it enqueued (the outbox's updateAsk), the sweep asks a claim, and the ask reaches the
// claim's role, exactly where that rule owes it an ask, it has a session, its agent said it is
// ready and a machine of this daemon holds it, over every state, role and fact of a claim, in groups
// of every size and mix an issue's claims make, and over every field of a claim at every depth,
// alone, in pairs and all at once; and the tick that opens a round posts it to every claim with a
// session. The rig these tests drive the path with is podupdate_sweep_rig_test.go's, and the kinds
// of claim they build groups of, the pools and shapes of those groups, the fields of a claim they
// set, the sets of fields and the groups they set them in are podupdate_sweep_fields_test.go's. An
// ask that waited in the outbox while its claim moved on is podupdate_undelivered_ask_test.go's.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The sweep owes a claim an ask where the claim's own record owes it one (supervise's
// Claim.OwesUpdateAsk): a claim of the round with a session is owed its ask until an ask of the
// round reached its session or its role answered the round, and a failed claim, one that has left
// the round's pod and a claim in another round are owed none. This is the list before the sweep
// asks whether the claim's agent said it is ready (askRound's supervise.SaidReady), which drops a
// failed claim as it drops every claim that is not ready, so the sweep asks no failed claim
// whether the rule refuses it or not. With the rule's failed case taken out, this test shows it in
// the list itself, and the property and field tests show it only where the tick decides a group's
// round before openPodUpdate, since the round waits for no failed claim (waitingClaims), and so
// posts the round to no claim while the rule owes a failed claim an ask (sweepPath.posts). The
// control is the ready claim with none of these, which the sweep owes its ask.
//
// The negative control is the rule with its failed case taken out: the failed claim is then on
// the list, and its case fails. This test holds each case of the rule as the sweep reads it, and
// the next one (TestPodUpdateSweepOwesAnAskExactlyWhereTheRuleDoes) holds, over the facts of a
// claim it names, that the sweep refuses and owes no claim by a statement of its own that the
// rule does not make: a sweep that dropped failed claims on its own before it read the rule passes
// this test under that same mutant, and fails the next one.
func TestPodUpdateSweepOwesAnAskWhereTheClaimsOwnRecordOwesOne(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		set  func(*supervise.Claim)
		owed bool
	}{
		{name: "the control: a ready claim of the round with no ask and no answer is owed its ask", set: func(*supervise.Claim) {}, owed: true},
		{name: "a claim an ask of the round reached is owed none", set: func(c *supervise.Claim) { c.UpdateTaskedAt = &at }},
		{name: "a claim that answered the round is owed none", set: func(c *supervise.Claim) { c.UpdateSignalledAt = &at }},
		{name: "a claim that left the round's pod is owed none", set: func(c *supervise.Claim) { c.UpdateLeftRound = podUpdateRound }},
		{name: "a claim that failed is owed none", set: func(c *supervise.Claim) { c.State = supervise.StateFailed }},
		{name: "a claim in another round is owed none", set: func(c *supervise.Claim) { c.UpdateRound = "pod:" + podUpdateReplacementPod }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := supervise.Claim{Token: "legion-legion-implementer", Role: claim.RoleImplementer, Session: "ses_i",
				State: supervise.StateReady, UpdateRound: podUpdateRound}
			tc.set(&c)
			asking := unaskedClaims([]supervise.Claim{c}, podUpdateRound)
			if owed := len(asking) == 1 && asking[0].Token == c.Token; owed != tc.owed || len(asking) > 1 {
				t.Fatalf("the sweep owes an ask to %d claims of one %s claim in round %q, asked %v, answered %v, left %q; want the claim owed=%t",
					len(asking), c.State, c.UpdateRound, c.UpdateTaskedAt, c.UpdateSignalledAt, c.UpdateLeftRound, tc.owed)
			}
		})
	}
}

// sweepFact is one fact of a claim that sweepFacts sets or leaves out, and the facts a claim holds
// are the bits of the ones it holds, read from its own fields (factsOf).
type sweepFact int

const (
	// factSession is a session (Claim.Session).
	factSession sweepFact = 1 << iota
	// factAsked is an ask of the round that reached its session (Claim.UpdateTaskedAt).
	factAsked
	// factAnswered is its role's answer to the round (Claim.UpdateSignalledAt).
	factAnswered
	// factLeft is a leave of the round's pod (Claim.UpdateLeftRound).
	factLeft
	// factOtherRound is another round than the one swept (Claim.UpdateRound).
	factOtherRound
	// factLocator is a process the claim records (Claim.Locator).
	factLocator
	// sweepFactSets is how many sets of those facts there are, each of which sweepFacts builds a
	// claim of for every state and role.
	sweepFactSets
)

var sweepFactNames = []struct {
	fact sweepFact
	name string
}{
	{factSession, "session"}, {factAsked, "asked"}, {factAnswered, "answered"}, {factLeft, "left"},
	{factOtherRound, "other round"}, {factLocator, "locator"},
}

func (f sweepFact) String() string {
	var names []string
	for _, n := range sweepFactNames {
		if f&n.fact != 0 {
			names = append(names, n.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, "|")
}

// factsOf is the facts c holds, read from its own fields: what the tests select claims of sweepFacts
// by (sweepKindsOf), and what sweepFacts holds each claim it builds to.
func factsOf(c supervise.Claim) sweepFact {
	var f sweepFact
	if c.Session != "" {
		f |= factSession
	}
	if c.UpdateTaskedAt != nil {
		f |= factAsked
	}
	if c.UpdateSignalledAt != nil {
		f |= factAnswered
	}
	if c.UpdateLeftRound == podUpdateRound {
		f |= factLeft
	}
	if c.UpdateRound != podUpdateRound {
		f |= factOtherRound
	}
	if c.Locator != nil {
		f |= factLocator
	}
	return f
}

// sweepFacts is the claims the sweep tests below hold the sweep to the rule over: every state a
// claim can be in (supervise.States), every role a claim can be on (claim.Roles, and the
// controller's, which is none of them), and each set of the facts a claim holds or not (sweepFact):
// a session, a delivered ask, an answer, a leave of the round's pod, another round and a process
// locator, which is 64 claims for each state and role. Each claim carries token(role) and records
// the round's opening at at, as the one write of a round on a claim does (supervise's
// openPodUpdate), so the sweep's ask has a bound to state (roundBound); the tests give at as the
// rig's held clock, so no round they build has reached its bound. A state or a role added to those
// lists joins the tests without an edit. A claim that does not hold the facts it was built of, read
// from its fields (factsOf), fails the test, naming the facts set on the wrong fields.
func sweepFacts(t *testing.T, at time.Time, token func(claim.Role) claim.Token) []supervise.Claim {
	t.Helper()
	var claims []supervise.Claim
	for _, state := range supervise.States() {
		for _, role := range sweepRoleList {
			for facts := range sweepFactSets {
				opened := at
				c := supervise.Claim{Token: token(role), Role: role, State: state, UpdateRound: podUpdateRound,
					UpdateOpenedAt: &opened}
				if facts&factSession != 0 {
					c.Session = "ses_i"
				}
				if facts&factAsked != 0 {
					c.UpdateTaskedAt = &at
				}
				if facts&factAnswered != 0 {
					c.UpdateSignalledAt = &at
				}
				if facts&factLeft != 0 {
					c.UpdateLeftRound = podUpdateRound
				}
				if facts&factOtherRound != 0 {
					c.UpdateRound = "pod:" + podUpdateReplacementPod
				}
				if facts&factLocator != 0 {
					c.Locator = &runtime.Locator{Claim: c.Token, Incarnation: "inc-1"}
				}
				if got := factsOf(c); got != facts {
					t.Fatalf("sweepFacts built the %s %s claim of the facts %s, and its fields hold %s: the facts %s are set on the wrong fields",
						state, role, facts, got, facts^got)
				}
				claims = append(claims, c)
			}
		}
	}
	return claims
}

// sweepRoleList is every role a claim can be on: claim.Roles, and the controller's.
var sweepRoleList = append(slices.Clone(claim.Roles), claim.RoleController)

// sweepRoleCount is how many roles sweepFacts tries, which is also the most claims a group of one
// issue holds: one of each role, the controller's included.
var sweepRoleCount = len(sweepRoleList)

// sweepRoleIndex is the index of role in sweepRoleList.
func sweepRoleIndex(role claim.Role) int { return slices.Index(sweepRoleList, role) }

// sweepPropertyShapes is each group the property test puts a claim to the sweep in: alone; at
// each place of a group of two and of three, beside claims of every mix of the two kinds, owed an
// ask and refused one; and in a group of each size from four to one claim of every role, beside
// claims of which every count from none to all is owed and the rest refused, each group holding a
// claim the sweep asks: the first owed claim beside the one tried is one, and beside no owed claim
// the claim tried is one. So every size of group an issue's claims make is tried, every count of
// owed claims at each size, and in every group of four or more the sweep asks a claim, so a
// statement of its own there that reads the claims beside a claim changes what an ask reaches.
func sweepPropertyShapes() []sweepShape {
	shapes := []sweepShape{{name: "alone"}}
	for size := 2; size <= 3; size++ {
		for mix := range 1 << (size - 1) {
			others := make([]sweepKind, size-1)
			for k := range others {
				if others[k] = sweepRefused; mix&(1<<k) != 0 {
					others[k] = sweepOwedOne
				}
			}
			for place := range size {
				shapes = append(shapes, sweepShape{name: fmt.Sprintf("at %d of %d, beside claims %v", place+1, size, others), others: others, place: place})
			}
		}
	}
	for size := 4; size <= sweepRoleCount; size++ {
		shapes = append(shapes, sweepShape{name: fmt.Sprintf("in a group of %d, a claim the sweep asks beside %d claims refused one", size, size-1),
			others: slices.Repeat([]sweepKind{sweepRefused}, size-1), place: -1, askedOnly: true})
		for owed := 1; owed < size; owed++ {
			others := append(append([]sweepKind{sweepAsked}, slices.Repeat([]sweepKind{sweepOwedOne}, owed-1)...),
				slices.Repeat([]sweepKind{sweepRefused}, size-1-owed)...)
			shapes = append(shapes, sweepShape{name: fmt.Sprintf("in a group of %d, beside %d claims owed an ask, one the sweep asks among them, and %d refused one", size, owed, size-1-owed),
				others: others, place: -1})
		}
	}
	return shapes
}

// The sweep's whole path, from the daemon's tick to the publish (sweepRig.hold), asks a claim, and
// the ask reaches its role, exactly where the claim has a session, its own record owes it an ask
// (supervise's Claim.OwesUpdateAsk), its agent said it is ready and a machine of this daemon holds
// it, over every mix of the facts of a claim sweepFacts names, in groups of every size an issue's
// claims make (sweepPropertyShapes): every claim of sweepFacts alone, and at each place of a group
// of two and of three, beside claims of every mix of the two kinds, one the rule owes an ask and
// one it refuses; and each kind of claim of every state and role (sweepKindsOf) in a group of each
// size from four to one claim of every role, beside claims of every count owed and the rest
// refused, with a claim the sweep asks in every such group. The claims beside it are taken in turn
// (sweepPools), and each of them holds one more field of the claim, a field at any depth taken in
// turn that keeps it of its kind (claimLeaves). Every claim of every group is held to the rule, the
// claims beside it included, so a group that holds two or more claims the sweep owes an ask fails
// the test where any one of them is not asked or not reached.
//
// With the test above it holds the one rule of whom a round asks, which is neither a failed claim
// nor one that has left the round's pod: that test fails when the rule loses a case, and this one
// fails when the sweep refuses or owes a claim by a statement of its own that the rule does not
// make. That statement can stand anywhere from the tick to the publish (sweepPath), and it can read
// the claim or the claims beside it: a filter of its own on one state, on one role or on a locator
// present or absent, its own failed filter put back while the rule's failed case is taken out, its
// left-pod check taken out, a filter on a pending task that holds only in a group of one size or
// only beside a given count of owed claims, a claim refused while another claim of its group is
// owed an ask too, only the first owed claim of a group asked, or a claim refused in askRound
// beside a claim of one state and role in a group of one size. The test after it holds the same
// over every field of a claim. None of them fails for a statement of the sweep's own that agrees
// with the rule in every group, such as that failed filter put back with the rule whole: it changes
// no claim's answer, and it shows here the first time the rule changes and the two disagree.
//
// The negative control is the sweep's own failed filter put back with the rule's failed case taken
// out: this test then fails on each failed claim with a session that the rule owes an ask, and the
// test above passes. The test also fails when it saw no claim owed an ask or no claim refused one,
// no group in which the sweep asks two claims, a size of group in which it asks no claim, or a
// group of four or more in which it asks none, since such a set shows nothing.
func TestPodUpdateSweepOwesAnAskExactlyWhereTheRuleDoes(t *testing.T) {
	r := newSweepRig(t)
	at := podUpdateStart
	facts := sweepFacts(t, at, r.token)
	leaves, unset := claimLeaves(sweepSliceLength(t))
	if len(unset) > 0 || len(leaves) == 0 {
		t.Fatalf("the claim holds fields this test cannot set (%v), or none it can (%d), so it cannot vary the claims beside each claim", unset, len(leaves))
	}
	pools := newSweepPools(facts, r.asks)
	next := 0
	// vary sets one more field on a claim beside the claim tried: the next field in turn that the
	// claim leaves empty and whose value keeps it of its kind, owed an ask or refused one.
	vary := func(other *supervise.Claim) {
		owed := sweepOwed(*other)
		for range leaves {
			leaf := leaves[next%len(leaves)]
			next++
			if !leaf.empty(*other) {
				continue
			}
			c := *other
			if leaf.set(&c, at); sweepOwed(c) == owed {
				*other = c
				return
			}
		}
	}
	shapes := sweepPropertyShapes()
	checked, owed, refused, groups, twoAsked, unasked := 0, 0, 0, 0, 0, 0
	askedBySize := make([]int, sweepRoleCount+1)
	buf := make([]supervise.Claim, 0, sweepRoleCount)
	every, kinds := make([]int, len(facts)), sweepKindsOf(t, facts)
	for i := range every {
		every[i] = i
	}
	askedKinds := slices.DeleteFunc(slices.Clone(kinds), func(i int) bool { return !r.asks(facts[i]) })
	for _, s := range shapes {
		wrong, seen, example := 0, 0, ""
		tried := every
		if len(s.others) >= 3 {
			tried = kinds
		}
		if s.askedOnly {
			tried = askedKinds
		}
		for n, i := range tried {
			group := pools.group(buf, i, s.at(n), s.others, vary)
			asked, misses := r.hold(group)
			groups++
			seen += len(group)
			owed += asked
			refused += len(group) - asked
			askedBySize[len(group)] += asked
			if asked >= 2 {
				twoAsked++
			}
			if asked == 0 && len(group) >= 4 {
				unasked++
			}
			wrong += len(misses)
			if example == "" && len(misses) > 0 {
				example = misses[0]
			}
		}
		checked += seen
		if wrong > 0 {
			t.Errorf("%s: the sweep disagrees with the rule on %d of %d claims, for example %s", s.name, wrong, seen, example)
		}
	}
	var noAsk []int
	for size := 1; size <= sweepRoleCount; size++ {
		if askedBySize[size] == 0 {
			noAsk = append(noAsk, size)
		}
	}
	if owed == 0 || refused == 0 || twoAsked == 0 || len(noAsk) > 0 || unasked > 0 {
		t.Fatalf("checked %d claims of %d groups, %d owed an ask and %d refused one, %d groups in which the sweep asks two claims or more, groups of the sizes %v in which it asks none, and %d groups of four or more in which it asks none: a set with no owed or no refused claim, no group of two asked claims, a size of group with no ask, or a larger group with no ask shows nothing",
			checked, groups, owed, refused, twoAsked, noAsk, unasked)
	}
	t.Logf("checked %d claims of %d groups in %d shapes, of every size from 1 to %d: %d owed an ask, %d refused one; in %d groups the sweep asks two claims or more, and in every group of four or more it asks one",
		checked, groups, len(shapes), sweepRoleCount, owed, refused, twoAsked)
}

// The sweep's whole path, from the daemon's tick to the publish (sweepRig.hold), asks a claim, and
// the ask reaches its role, exactly where the claim has a session, its own record owes it an ask
// (which it owes neither a failed claim nor one that has left the round's pod), its agent said it
// is ready and a machine of this daemon holds it, whatever else the claim holds. Each exported
// field of supervise.Claim at every depth below it — through structs, pointers, and each slice at
// each of its elements up to launch_failure_limit's shipped value (sweepSliceLength), so each of
// the four budgets, each field of a pending task, of the handshake and of a turn's outcome, with
// one, two and three turn outcomes — is set in turn (claimLeaves), one field at a time: each kind
// of claim of every state and role (sweepKindsOf) that leaves that field empty is put to the sweep
// again with the field set, alone, beside a claim of another role and beside a claim of every other
// role (sweepFieldShapes), each of which holds the field too and is the next claim of its role in
// turn (sweepPools). A field the machine's view reads from the machine's own state rather than the
// claim's record — a compaction in flight, a process the runtime reported gone — is set by putting
// the claim's machine in that state (sweepMachineStates), on each claim whose machine takes it.
//
// Each state of the machine's view beside the claim (sweepViewStates) is set by putting every
// machine of a group in it, on each kind of claim, in the same three groups.
//
// Then fields are set together, in sets that put every pair of fields together in one set
// (sweepFieldBlocks), and in one set every field at once, on each kind of claim of every state and
// role, each claim alone and in groups of one claim of every role that hold the same set. In the
// plain build the groups are sweepKindGroups', which put each claim beside a claim of every other
// role in every state, its own state included (the arrangement in which every claim of a group
// has one state and one kind); under -race they are sweepRaceKindGroups', the same-state group of
// each kind the sweep asks and each asked claim beside the ready claim of every other role. So a
// filter of the sweep's own on a field the test above does not vary, on two of them together on
// any kind of claim, on two of them together beside a claim of another state, or on any number of
// them set together — a pending task, a task's text or the moment a turn confirmed it, the round's
// told mark, the claim's deaths or prompt failures, the claim's issue, a claim with more than one
// or two turn outcomes, a claim being compacted or whose process is gone, a pending task on a claim
// whose round was told — fails here, wherever it stands from the tick to the publish, on the
// claims with a session the rule owes an ask: a sweep that skipped every claim holding a task would
// leave a working role unasked until the round's bound.
//
// The plain build alone also sets each pair of fields alone on each kind of claim, on each such
// kind with a session at the tick that opens the round, and beside the ready claim of each other
// role, one role at a time, on one kind the sweep asks, taken in turn for each pair; and it puts
// each state of the machine's view at the round's opening, and beside each field of the claim, in
// an open round and at the opening. daemon-go runs this test plain by name for those
// (raceEnabled), and the race build runs none of them.
//
// Last, each field and each set is set on each claim of every state and role with a session at the
// tick that opens the round, alone and in the groups of every role above: the tick reads the group
// live there and must post the round to each of them (sweepRig.open). A field added to the claim
// joins the test without an edit, and a field of a type this test cannot set fails it.
//
// The test leaves out the fields every claim of sweepFacts sets (sweepFieldsEveryClaimSets: the
// token, the role, the state, the round and the round's opening), each named with why, and it
// fails when one of them is no field of the claim or is empty on some claim, and when a field it
// does not name is tried on no claim, or held by no machine of a claim it was set on.
//
// Each field is set to one value. So this test does not fail for a filter of the sweep's own on one
// value of a field other than the one it sets: a filter on a claim an operator suspended
// (supervise.SuspendedByOperator) passes it, since SuspendedBy is set to "x" here. Nor does it fail
// for a filter that agrees with the rule in every group, as the test above says.
//
// The negative control is a filter of the sweep's own on a pending task (Claim.Pending), anywhere
// from the tick to the publish: this test then fails with each field of Pending set, and the test
// above fails too, on the groups beside whose claims a pending task is set. A filter on a pending
// task together with the round's told mark fails the sets of fields alone. The test also fails
// when it tried no field or no set of fields, left a pair of fields set together on no kind of
// claim that leaves both empty, or saw no owed or no refused claim, since such a set shows nothing.
func TestPodUpdateSweepOwesAnAskExactlyWhereTheRuleDoesWhateverElseTheClaimHolds(t *testing.T) {
	r := newSweepRig(t)
	at := podUpdateStart
	facts := sweepFacts(t, at, r.token)
	kinds := sweepKindsOf(t, facts)
	leaves, unset := claimLeaves(sweepSliceLength(t))
	if len(unset) > 0 {
		t.Fatalf("the fields %v cannot be set, so this test cannot try them", unset)
	}
	top := map[string]bool{}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(supervise.Claim{})) {
		top[f.Name] = true
	}
	for name := range sweepFieldsEveryClaimSets {
		if !top[name] {
			t.Fatalf("%s is named as a field every claim of sweepFacts sets, and the claim has no such field", name)
		}
	}
	base := facts[slices.IndexFunc(facts, func(c supervise.Claim) bool {
		return c.State == supervise.StateReady && c.Role == claim.RoleImplementer && factsOf(c) == factSession
	})]
	if wrong := r.path.machineStatesRead(base, leaves, at); len(wrong) > 0 {
		t.Fatalf("the machine's view reads fields this test does not set as a machine holds them: %s", strings.Join(wrong, "; "))
	}
	if wrong := r.path.viewStatesRead(base); len(wrong) > 0 {
		t.Fatalf("the machine's view holds state beside the claim that this test does not put a machine in: %s", strings.Join(wrong, "; "))
	}
	checked, owed, refused, fields := 0, 0, 0, 0
	// sweepTrial is one phase's account of the groups it puts to the path, and try is the one way
	// each phase puts a group there: run is the tick it is put to, r.hold at a tick that finds the
	// round open or r.open at the tick that opens it. try adds the group's claims to the test's
	// totals and to the phase's own, counts the claims the sweep treated otherwise than the rule,
	// and keeps the phase's first such claim, with the shape the group was tried in.
	type sweepTrial struct {
		wrong, seen int
		example     string
	}
	try := func(p *sweepTrial, run func([]supervise.Claim) (int, []string), shape string, group []supervise.Claim) {
		asked, misses := run(group)
		checked += len(group)
		p.seen += len(group)
		owed += asked
		refused += len(group) - asked
		p.wrong += len(misses)
		if p.example == "" && len(misses) > 0 {
			p.example = shape + ", " + misses[0]
		}
	}
	buf := make([]supervise.Claim, 0, sweepRoleCount)
	others := make([]supervise.Claim, len(facts))
	for _, leaf := range leaves {
		for j, c := range facts {
			if others[j] = c; leaf.empty(c) {
				leaf.set(&others[j], at)
			}
		}
		pools := newSweepPools(others, r.asks)
		tried, held := 0, 0
		var trial sweepTrial
		for n, i := range kinds {
			c := facts[i]
			if !leaf.empty(c) {
				continue
			}
			tried++
			for _, s := range sweepFieldShapes {
				group := pools.group(buf, i, s.at(n), s.others, nil)
				try(&trial, r.hold, s.name, group)
				if !leaf.empty(group[s.at(n)]) {
					held++
				}
			}
		}
		_, everyClaimSets := sweepFieldsEveryClaimSets[leaf.top]
		switch {
		case everyClaimSets && tried > 0:
			t.Errorf("%s is named as a field every claim of sweepFacts sets, and %d claims leave %s empty: try it, and take it off the list", leaf.top, tried, leaf.name)
		case !everyClaimSets && tried == 0:
			t.Errorf("no claim of sweepFacts leaves %s empty, so this test tried it on no claim: name %s among the fields every claim sets, with why the sweep's tests still hold the sweep over it", leaf.name, leaf.top)
		case tried > 0 && held == 0:
			t.Errorf("no machine of the %d claims %s was set on held it, so this test tried it on no claim", tried, leaf.name)
		case tried > 0:
			fields++
		}
		if trial.wrong > 0 {
			t.Errorf("with %s set, the sweep disagrees with the rule on %d claims of %d groups, for example %s; want the sweep to ask a claim, and the ask to reach it, exactly where the rule owes it an ask, it has a session, its agent said it is ready and a machine holds it",
				leaf.name, trial.wrong, tried*len(sweepFieldShapes), trial.example)
		}
	}

	// Each state of the machine's view beside the claim (sweepViewStates), on each kind of claim,
	// alone, beside a claim of another role and beside a claim of every other role, with every
	// machine of the group put in that state. took is, for each state, the kinds of claim whose
	// machine takes it alone, which the plain build's phase below sets each field on.
	views := 0
	pools := newSweepPools(facts, r.asks)
	took := make([][]int, len(sweepViewStates))
	for v := range sweepViewStates {
		state := &sweepViewStates[v]
		r.path.view, r.path.taken = state, 0
		var trial sweepTrial
		for n, i := range kinds {
			for si, s := range sweepFieldShapes {
				before := r.path.taken
				try(&trial, r.hold, s.name, pools.group(buf, i, s.at(n), s.others, nil))
				if si == 0 && r.path.taken > before {
					took[v] = append(took[v], i)
				}
			}
		}
		r.path.view = nil
		if r.path.taken == 0 {
			t.Errorf("no machine of the %d claims put %s took that state, so this test tried it on no claim", trial.seen, state.what)
		} else {
			views++
		}
		if trial.wrong > 0 {
			t.Errorf("with each machine %s (%s), the sweep disagrees with the rule on %d of %d claims, for example %s",
				state.what, state.field, trial.wrong, trial.seen, trial.example)
		}
	}

	// The plain build alone (raceEnabled) holds each state of the machine's view at the tick that
	// opens the round, on each kind of claim in the same groups, and fails where no machine took the
	// state there. It holds each state with each field of the claim set beside it, on the kinds of
	// claim whose machine takes that state alone (took): alone at a tick that finds the round open,
	// on each such kind the sweep asks and on one other such kind taken in turn, and alone at the
	// tick that opens the round, on each such kind. It fails where no machine of those tries held the
	// state and the field at once, in the open round or at the opening, unless
	// sweepViewFieldsUntaken names that pair with why, and where a machine held a pair that list
	// names. So a filter of the sweep's own on the view's state at the opening's live read, or on a
	// view state together with a field of the claim (a wake the pacing holds on a claim whose round
	// was told, a prompt in flight on a claim with a death counted, either on one role's claim),
	// fails here. A field the machine's view reads from the machine's own state
	// (sweepMachineStates) is not set beside a view state: a test puts a machine in one state at a
	// time (sweepPath.hold).
	viewOpenings, viewFields, viewTurn := 0, 0, 0
	eligible := make([]int, 0, len(kinds))
	if !raceEnabled {
		for v := range sweepViewStates {
			state := &sweepViewStates[v]
			r.path.view, r.path.taken = state, 0
			var trial sweepTrial
			for n, i := range kinds {
				for _, s := range sweepFieldShapes {
					group := pools.group(buf, i, s.at(n), s.others, nil)
					for j := range group {
						group[j].UpdateRound, group[j].UpdateOpenedAt = "", nil
					}
					try(&trial, r.open, "at the tick that opens the round, "+s.name, group)
					viewOpenings++
				}
			}
			if r.path.taken == 0 {
				t.Errorf("no machine of the %d claims put %s at the tick that opens the round took that state, so this test tried it at the opening on no claim", trial.seen, state.what)
			}
			for _, leaf := range leaves {
				if slices.ContainsFunc(sweepMachineStates, func(s sweepMachineState) bool { return s.field == leaf.top }) {
					continue
				}
				eligible = eligible[:0]
				for _, i := range took[v] {
					if leaf.empty(facts[i]) {
						eligible = append(eligible, i)
					}
				}
				if len(eligible) == 0 {
					continue
				}
				// held is whether the try of group, which began with before machines taken, put a
				// machine in the state whose claim still holds the field: a machine that takes no
				// state holds the claim as the request left it.
				held := func(before int, group []supervise.Claim) bool {
					return r.path.taken > before && !leaf.empty(group[0])
				}
				heldOpen, heldOpening := false, false
				other := eligible[viewTurn%len(eligible)]
				viewTurn++
				for _, i := range eligible {
					c := facts[i]
					leaf.set(&c, at)
					if r.asks(facts[i]) || i == other {
						before, group := r.path.taken, append(buf[:0], c)
						try(&trial, r.hold, "with "+leaf.name+" set, alone", group)
						heldOpen = heldOpen || held(before, group)
						viewFields++
					}
					c.UpdateRound, c.UpdateOpenedAt = "", nil
					before, group := r.path.taken, append(buf[:0], c)
					try(&trial, r.open, "with "+leaf.name+" set, at the tick that opens the round", group)
					heldOpening = heldOpening || held(before, group)
					viewFields++
				}
				for _, where := range []struct {
					name string
					held bool
				}{{"in an open round", heldOpen}, {"at the tick that opens the round", heldOpening}} {
					why, named := sweepViewFieldsUntaken[sweepViewField{state.field, leaf.name, where.name}]
					switch {
					case !where.held && !named:
						t.Errorf("no machine put %s with %s set %s held both, so this test tried that pair on no claim: drive it, or name it in sweepViewFieldsUntaken with why", state.what, leaf.name, where.name)
					case where.held && named:
						t.Errorf("a machine put %s with %s set %s held both, and sweepViewFieldsUntaken names that pair (%s): take it off the list", state.what, leaf.name, where.name, why)
					}
				}
			}
			r.path.view = nil
			if trial.wrong > 0 {
				t.Errorf("with each machine %s (%s), at the round's opening or with a field of the claim set beside the state, the sweep disagrees with the rule on %d of %d claims, for example %s",
					state.what, state.field, trial.wrong, trial.seen, trial.example)
			}
		}
	}

	blocks := sweepFieldBlocks(sweepFlips(facts, kinds, leaves, at))
	if missing := sweepUncoveredPairs(facts, kinds, leaves, blocks); len(missing) > 0 {
		t.Fatalf("%d pairs of fields that a kind of claim leaves empty are set together on it in no set of fields, for example %s", len(missing), missing[0])
	}
	groups := sweepKindGroups(t, facts, kinds, sweepKinds)
	if raceEnabled {
		groups = sweepRaceKindGroups(t, facts, kinds, sweepKinds, r.asks)
	} else if missing := sweepUnmetNeighbours(facts, kinds, groups); len(missing) > 0 {
		t.Fatalf("%d claims of a role and a state stand beside a kind of claim in no group of every role, for example %s", len(missing), missing[0])
	}
	sets, setsWrong := 0, 0
	for _, block := range blocks {
		with := func(c supervise.Claim) supervise.Claim {
			for _, l := range block {
				if leaves[l].empty(c) {
					leaves[l].set(&c, at)
				}
			}
			return c
		}
		var trial sweepTrial
		for _, i := range kinds {
			try(&trial, r.hold, "alone", append(buf[:0], with(facts[i])))
		}
		for _, g := range groups {
			group := buf[:0]
			for _, i := range g {
				group = append(group, with(facts[i]))
			}
			try(&trial, r.hold, "in a group of every role", group)
		}
		sets++
		if trial.wrong > 0 {
			if setsWrong++; setsWrong <= 10 {
				t.Errorf("with %s set together, the sweep disagrees with the rule on %d of %d claims, for example %s",
					sweepLeafNames(leaves, block), trial.wrong, trial.seen, trial.example)
			}
		}
	}
	if setsWrong > 10 {
		t.Errorf("and with %d more sets of fields", setsWrong-10)
	}

	// Each pair of fields alone: the two set on each kind of claim that leaves both empty, and no
	// other field, so a filter on a pair that holds only while some third field is empty — a field
	// of the pair's set above — fails here, and so does a filter that owes a claim the rule refuses
	// where a pair is set on it. The pair alone is also put to the tick that opens the round on each
	// such kind whose claim has a session with the pair set, the claims the opening posts the round
	// to, whatever its state; and beside the ready claim of each other role, one role at a time, on
	// one kind the sweep asks, taken in turn for each pair (where that kind is of the role beside, the
	// next asked kind of another role takes its place). So such a filter fails here too where it
	// stands at the opening's live read, or reads which role's claim stands beside the claim. A
	// filter that reads the claim beside only on a kind other than the one its pair took in turn
	// passes. The plain build alone sets them, and daemon-go runs this test plain by name for that
	// (raceEnabled); under -race the test holds the groups, the opening and the machine's states,
	// where the ask and its publish run beside the machines' own work, at a cost the race step's
	// bound holds.
	pairKinds := kinds
	if raceEnabled {
		pairKinds = nil
	}
	ready := make([]int, sweepRoleCount)
	for role := range ready {
		ready[role] = slices.IndexFunc(facts, func(c supervise.Claim) bool {
			return c.State == supervise.StateReady && sweepRoleIndex(c.Role) == role && factsOf(c) == factSession
		})
		if ready[role] < 0 {
			t.Fatalf("no claim of sweepFacts is a ready %s claim with a session alone, so no asked claim can stand beside one", sweepRoleList[role])
		}
	}
	pairs, pairsBeside, pairsOpened, pairsWrong, pairTurn := 0, 0, 0, 0, 0
	beside := make([]int, 0, len(kinds))
	for a := range leaves {
		for b := a + 1; b < len(leaves); b++ {
			var trial sweepTrial
			beside = beside[:0]
			for _, i := range pairKinds {
				c := facts[i]
				if !leaves[a].empty(c) || !leaves[b].empty(c) {
					continue
				}
				leaves[a].set(&c, at)
				leaves[b].set(&c, at)
				try(&trial, r.hold, "alone", append(buf[:0], c))
				if r.asks(facts[i]) {
					beside = append(beside, i)
				}
				if c.Session == "" {
					continue
				}
				c.UpdateRound, c.UpdateOpenedAt = "", nil
				try(&trial, r.open, "at the tick that opens the round", append(buf[:0], c))
				pairsOpened++
			}
			if len(beside) > 0 {
				turn := pairTurn % len(beside)
				pairTurn++
				for role := range sweepRoleCount {
					k := -1
					for j := range beside {
						if next := beside[(turn+j)%len(beside)]; sweepRoleIndex(facts[next].Role) != role {
							k = next
							break
						}
					}
					if k < 0 {
						continue
					}
					c := facts[k]
					leaves[a].set(&c, at)
					leaves[b].set(&c, at)
					try(&trial, r.hold, "beside the ready "+string(sweepRoleList[role])+" claim", append(buf[:0], c, facts[ready[role]]))
					pairsBeside++
				}
			}
			if trial.seen > 0 {
				pairs++
			}
			if trial.wrong > 0 {
				if pairsWrong++; pairsWrong <= 10 {
					t.Errorf("with %s and %s set and no other field, the sweep disagrees with the rule on %d of %d claims, for example %s",
						leaves[a].name, leaves[b].name, trial.wrong, trial.seen, trial.example)
				}
			}
		}
	}
	if pairsWrong > 10 {
		t.Errorf("and with %d more pairs of fields", pairsWrong-10)
	}

	opening := sweepKindsOfFacts(facts, factSession, factSession|factLocator)
	openingGroups := sweepKindGroups(t, facts, opening, []sweepFact{factSession, factSession | factLocator})
	if raceEnabled {
		openingGroups = sweepRaceKindGroups(t, facts, opening, []sweepFact{factSession, factSession | factLocator}, r.asks)
	} else if missing := sweepUnmetNeighbours(facts, opening, openingGroups); len(missing) > 0 {
		t.Fatalf("at the round's opening, %d claims of a role and a state stand beside a claim with a session in no group of every role, for example %s", len(missing), missing[0])
	}
	opened, openedWrong := 0, 0
	openAt := func(label string, set func(*supervise.Claim)) {
		build := func(i int) supervise.Claim {
			c := facts[i]
			c.UpdateRound, c.UpdateOpenedAt = "", nil
			set(&c)
			return c
		}
		var trial sweepTrial
		for _, i := range opening {
			try(&trial, r.open, "alone", append(buf[:0], build(i)))
		}
		for _, g := range openingGroups {
			group := buf[:0]
			for _, i := range g {
				group = append(group, build(i))
			}
			try(&trial, r.open, "in a group of every role", group)
		}
		opened++
		if trial.wrong > 0 {
			if openedWrong++; openedWrong <= 10 {
				t.Errorf("at the tick that opens the round, %s, the sweep disagrees with the rule on %d of %d claims, for example %s; want the round posted to each claim with a session that has not retired, and no ask, since no claim records the round yet",
					label, trial.wrong, trial.seen, trial.example)
			}
		}
	}
	openAt("with no field set", func(*supervise.Claim) {})
	opens := func(l claimLeaf) bool {
		_, everyClaimSets := sweepFieldsEveryClaimSets[l.top]
		return !everyClaimSets
	}
	for _, leaf := range leaves {
		if !opens(leaf) {
			continue
		}
		openAt("with "+leaf.name+" set", func(c *supervise.Claim) {
			if leaf.empty(*c) {
				leaf.set(c, at)
			}
		})
	}
	for _, block := range blocks {
		block = slices.DeleteFunc(slices.Clone(block), func(l int) bool { return !opens(leaves[l]) })
		if len(block) < 2 {
			continue
		}
		openAt("with "+sweepLeafNames(leaves, block)+" set together", func(c *supervise.Claim) {
			for _, l := range block {
				if leaves[l].empty(*c) {
					leaves[l].set(c, at)
				}
			}
		})
	}
	if openedWrong > 10 {
		t.Errorf("and at the opening with %d more fields or sets of fields", openedWrong-10)
	}
	plainEmpty := !raceEnabled && (pairs == 0 || pairsBeside == 0 || pairsOpened == 0 || viewOpenings == 0 || viewFields == 0)
	if fields == 0 || views == 0 || sets == 0 || plainEmpty || opened < 2 || owed == 0 || refused == 0 {
		t.Fatalf("checked %d claims over %d fields, %d states of the machine's view (%d at the opening, %d with a field), %d sets of fields, %d pairs (%d beside another role's ready claim, %d at the opening) and %d openings, %d owed and %d refused: a set with no field, no view state, no set of fields, no pair or view state beside a field or at the opening in the plain build, or no opening tried, or no owed or no refused claim, shows nothing",
			checked, fields, views, viewOpenings, viewFields, sets, pairs, pairsBeside, pairsOpened, opened, owed, refused)
	}
	grouped := "in groups that put each kind of claim beside every other role in every state"
	if raceEnabled {
		grouped = "in same-state groups and beside each role's ready claim for the kinds the sweep asks"
	}
	t.Logf("checked %d claims over %d fields of a claim at every depth, %d states of the machine's view beside the claim (%d groups at the round's opening, %d claims with a field set beside the state), %d sets of fields that put every pair together and all of them at once, %s, %d pairs of fields alone (%d of them beside another role's ready claim, and %d claims at the round's opening), and the round's opening with each field and each set: %d owed an ask, %d refused one",
		checked, fields, views, viewOpenings, viewFields, sets, grouped, pairs, pairsBeside, pairsOpened, owed, refused)
}

// The sweep's ask reaches a ready claim the rule owes an ask whatever else the claim holds, through
// the rig's real outbox: for the rig's ready implementer in the round, the control with nothing
// more set and then each exported field of the claim at every depth that the claim leaves empty,
// set in turn (claimLeaves), the daemon's tick (sweepPath) queues one row of the round's ask, and
// the role-message lane's run of that row delivers one ask to the implementer's topic, exactly
// where the claim's own record owes it one (supervise's Claim.OwesUpdateAsk, which owes a failed
// claim and one that has left the round's pod nothing) and its agent said it is ready; and the
// tick posts the round to the claim once. A field the machine's view reads from the machine's own
// state is set by putting the machine in that state (sweepMachineStates). Through every case the
// round's ask to another role, the tester's, waits in the outbox as a row the lane has not run yet,
// so the tick asks each case beside an ask of the round still queued: askRound skips a claim whose
// own ask of the round is queued (the store's QueuedUpdateAsks), and an ask queued for one role
// holds back no other role's. Each case's rows are run and finished before the next case, so no
// row of the implementer's that an earlier case queued stands in its way. The acceptance test of
// this change wrote it as the red of a filter placed in askRound: a sweep that skipped every claim
// holding a pending task there passed every other test of the sweep, and failed this one. The same
// filter placed anywhere from the tick to the outbox's publish fails it the same way, and so does
// a sweep that skips every claim while any ask of the round is queued.
//
// The negative control is that filter: this test then fails with each field of Pending set, as
// "the tick queued 0 rows and the lane delivered 0 asks". The test also fails when it tried no
// field, or saw no owed or no refused case.
func TestPodUpdateTheSweepsAskReachesEveryOwedReadyClaimWhateverElseItHolds(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	opened := podUpdateStart
	base := rig.claimOf(claim.RoleImplementer)
	base.UpdateRound, base.UpdateOpenedAt, base.UpdateReasons = podUpdateRound, &opened, podUpdateDrift
	path := newSweepPath(rig)
	leaves, unset := claimLeaves(sweepSliceLength(t))
	if len(unset) > 0 {
		t.Fatalf("the fields %v cannot be set, so this test cannot try them", unset)
	}
	fields, owedSeen, refusedSeen := 0, 0, 0
	// asks is whom the tick asks: a claim the rule owes an ask whose agent said it is ready. The
	// rig's machine holds the implementer, and it has a session.
	asks := func(c supervise.Claim) bool {
		owed, _ := c.OwesUpdateAsk(podUpdateRound)
		return owed && supervise.SaidReady(c.State)
	}
	try := func(label string, c supervise.Claim) {
		t.Helper()
		heard := rig.deliveredTo(claim.RoleImplementer)
		other := record.UpdateAsk{Role: claim.RoleTester, Round: podUpdateRound, Pod: podUpdatePod,
			Text: supervise.UpdateTask(podUpdateRound, podUpdateDrift, podUpdateStart.Add(podUpdateGrace))}
		if written, err := rig.s.enqueue(rig.ctx, podUpdateIssue, other); err != nil || !written {
			t.Fatalf("%s: queue the tester's ask of the round: written=%t error=%v", label, written, err)
		}
		group := []supervise.Claim{c}
		path.hold(group)
		defer path.end()
		c = group[0]
		owed, why := c.OwesUpdateAsk(podUpdateRound)
		want := asks(c)
		delivered := want
		path.sweep(group, podUpdatePod, podUpdateRound)
		if wrong := path.posts(group, podUpdateRound, false); len(wrong) > 0 {
			t.Errorf("%s: %s", label, strings.Join(wrong, "; "))
		}
		// A machine held deciding is read by the tick as one that has not answered (roundGroup's
		// mask), and its decision ends before the lane runs the row, which publishes under the
		// claim's lock only where the rule still owes the claim an ask as the decision left it
		// (sweepPath.deciding).
		path.letGo()
		if asked, reaches, ok := path.deciding(c, asks); ok {
			want, delivered = asked, reaches
		}
		queued, others := 0, 0
		for _, row := range rig.rows(record.OutboxKindUpdateAsk) {
			var ask record.UpdateAsk
			if err := json.Unmarshal(row.Payload, &ask); err != nil {
				t.Fatalf("%s: decode update ask %d: %v", label, row.ID, err)
			}
			switch {
			case ask == other:
				others++
			case ask.Role == claim.RoleImplementer && ask.Round == podUpdateRound:
				queued++
			default:
				t.Fatalf("%s: an ask of round %s went to %s, want only the implementer's of round %s beside the tester's queued one", label, ask.Round, ask.Role, podUpdateRound)
			}
		}
		if others != 1 {
			t.Fatalf("%s: the outbox holds %d rows of the tester's ask, want the one this case queued", label, others)
		}
		rig.runLane()
		got := rig.deliveredTo(claim.RoleImplementer) - heard
		if (queued == 1) != want || queued > 1 || (got == 1) != delivered || got > 1 {
			t.Errorf("%s: the tick queued %d rows and the lane delivered %d asks to the ready implementer, state %s; the rule says owed=%t (%v); want a row exactly where the rule owes one and the agent said it is ready (%t), and an ask delivered where the rule still owes it at the publish (%t)",
				label, queued, got, c.State, owed, why, want, delivered)
		}
		if delivered {
			owedSeen++
		} else {
			refusedSeen++
		}
	}
	try("the control, no field set", base)
	for _, leaf := range leaves {
		if leaf.top == "UpdateRound" || !leaf.empty(base) {
			continue
		}
		c := base
		leaf.set(&c, opened)
		fields++
		try("with "+leaf.name+" set", c)
	}
	views := 0
	for v := range sweepViewStates {
		state := &sweepViewStates[v]
		path.view, path.taken = state, 0
		try("the control with its machine "+state.what, base)
		path.view = nil
		if path.taken == 0 {
			t.Errorf("the ready implementer's machine did not take the state of its view %s (%s), so this test tried it on no claim", state.what, state.field)
		} else {
			views++
		}
	}
	if fields == 0 || views == 0 || owedSeen == 0 || refusedSeen == 0 {
		t.Fatalf("tried %d fields and %d states of the machine's view, %d owed and %d refused: a test with no field or no view state tried, or no owed or no refused case, shows nothing", fields, views, owedSeen, refusedSeen)
	}
	t.Logf("tried the control, %d fields of a claim at every depth and %d states of the machine's view beside the claim, from the tick to the publish: %d owed an ask, %d refused one", fields, views, owedSeen, refusedSeen)
}
