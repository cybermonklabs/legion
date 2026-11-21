package specgraph

import "slices"

// StatementState is the state of one statement at the subject (R10): State "not_built", "met",
// "not_met" or "unknown", with the Reason code of the rule that decided it and Detail, the id that
// the reason names: the statement that is not built, the check that is false or unknown, the
// supporter, or the statement with no check where the tree rules ask for one (lacksCheck), and ""
// for "checks_true". HeldVersion is the approved version of the statement that the files hold, 0
// when they hold none, and EarlierText marks a version that is not the current one (R10: "Where
// the files hold the text of an earlier approval, the page says so"). PartlyChecked marks a met
// statement whose checks decide only part of it, as the reviewer of its level judged (R10). Own is
// the state over the statement's own checks that read the subject, which a wait on the statement
// reads (R29).
type StatementState struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	Detail        string `json:"detail"`
	Own           string `json:"own"`
	HeldVersion   int    `json:"held_version"`
	EarlierText   bool   `json:"earlier_text"`
	PartlyChecked bool   `json:"partly_checked"`
}

// The states of a statement (R10).
const (
	statementNotBuilt = "not_built"
	statementMet      = "met"
	statementNotMet   = "not_met"
	statementUnknown  = "unknown"
)

// The reason codes of a statement state, beside reasonNotBuilt and reasonRefused, each the rule of
// Derive that gives it.
const (
	reasonCheckFalse        = "check_false"
	reasonSupporterNotMet   = "supporter_not_met"
	reasonCheckUnknown      = "check_unknown"
	reasonSupporterUnknown  = "supporter_unknown"
	reasonSupporterNotBuilt = "supporter_not_built"
	reasonNoCheck           = "no_check"
	reasonStatementRefused  = "statement_refused"
	reasonChecksTrue        = "checks_true"
)

// statementJudge derives the state of each statement of the approved spec at one subject from the
// states of the checks that read the subject.
type statementJudge struct {
	approved   Approved
	holds      map[string]Holding        // which approved text of each node the files hold
	refused    map[string]string         // the rule that refuses each check and statement of the files
	checks     map[string]CheckState     // the state of each check that reads the subject, by id
	statements map[string]bool           // the id of each current statement
	children   map[string][]string       // the statements under each statement, sorted by id
	own        map[string][]string       // the checks of each statement, sorted by id
	supporters map[string][]string       // the statements that support each statement, sorted by id
	states     map[string]StatementState // each state derived so far, by id
}

// newStatementJudge is the judge of the statements of current, the current text of each node of
// approved, at a subject where the files hold holds, the tree rules refuse refused (refusals) and
// the checks read checks.
func newStatementJudge(current Tree, approved Approved, holds map[string]Holding, refused map[string]string, checks []CheckState) *statementJudge {
	j := &statementJudge{approved: approved, holds: holds, refused: refused, checks: make(map[string]CheckState, len(checks)),
		statements: make(map[string]bool, len(current.Statements)), children: map[string][]string{},
		own: map[string][]string{}, supporters: map[string][]string{}, states: map[string]StatementState{}}
	for _, c := range checks {
		j.checks[c.ID] = c
	}
	// The tree is sorted by id, so each list is too.
	for _, s := range current.Statements {
		j.statements[s.ID] = true
		if s.Parent != "" {
			j.children[s.Parent] = append(j.children[s.Parent], s.ID)
		}
		for _, supported := range s.Serves {
			j.supporters[supported] = append(j.supporters[supported], s.ID)
		}
	}
	for _, c := range current.Checks {
		j.own[c.Serves] = append(j.own[c.Serves], c.ID)
	}
	return j
}

// inPreorder is the id of each current statement in preorder: the top statements by id, then the
// statements under each one by id, depth first. A statement whose parent is no current statement is
// a top statement, and a statement in a loop of parents, which Validate refuses, comes after them,
// each statement once.
func (j *statementJudge) inPreorder(current Tree) []string {
	seen := make(map[string]bool, len(current.Statements))
	order := make([]string, 0, len(current.Statements))
	for _, s := range current.Statements {
		if !j.statements[s.Parent] {
			order = j.preorder(s.ID, seen, order)
		}
	}
	for _, s := range current.Statements {
		order = j.preorder(s.ID, seen, order)
	}
	return order
}

// preorder appends to order id and each statement under it that seen does not hold, in preorder,
// and adds each to seen.
func (j *statementJudge) preorder(id string, seen map[string]bool, order []string) []string {
	if seen[id] {
		return order
	}
	seen[id] = true
	order = append(order, id)
	for _, child := range j.children[id] {
		order = j.preorder(child, seen, order)
	}
	return order
}

// state is the state of the statement id, in the order of Derive.
func (j *statementJudge) state(id string) StatementState {
	if st, derived := j.states[id]; derived {
		return st
	}
	st := StatementState{ID: id, Own: j.ownState(id)}
	if h := j.holds[id]; h.Present {
		st.HeldVersion, st.EarlierText = h.Version, !h.Current
	}
	under := j.preorder(id, map[string]bool{}, nil)
	if i := slices.IndexFunc(under, j.notBuilt); i >= 0 {
		st.State, st.Reason, st.Detail = statementNotBuilt, reasonNotBuilt, under[i]
	} else {
		st.State, st.Reason, st.Detail = j.byChecks(id)
	}
	st.PartlyChecked = st.State == statementMet && *j.approved.Nodes[id].PartlyChecked // Input.check refuses one that is not set
	j.states[id] = st
	return st
}

// byChecks is the state, reason and detail of the statement id, whose files hold its current text
// and the current text of each statement under it. It follows its own checks that read the
// subject, the state of each statement under it, and the state of each statement that supports it
// (R1: "A statement is true when the statements under it, the statements that support it, and its
// own checks are true"; R10: "Only the checks that read the subject count"), at each level: one of
// them not met, not met; else one of them unknown, unknown; else met. The search order is id
// itself, its own checks by id, the statements under it by id, then its supporters by id, and the
// first of them that decides gives the reason and the detail:
//   - id lacks a check that the tree rules ask for (lacksCheck): no_check, detail id (R7: "silence
//     is not a pass"); the tree rules refuse it otherwise (refusals): refused, detail the rule;
//   - an own check: check_false or check_unknown, detail the check;
//   - a statement under id gives its own reason and detail, so the detail names the check, the
//     supporter or the statement deep under id that decided; one that the tree rules refuse gives
//     statement_refused, detail that statement;
//   - a supporter: supporter_not_met, supporter_unknown or supporter_not_built, detail it.
//
// A statement under id or a supporter of id that reaches id in its turn is in a loop of parent and
// support relations, which Validate refuses: it reads unknown, statement_refused or
// supporter_unknown, so that no state depends on the order of the derivation.
func (j *statementJudge) byChecks(id string) (state, reason, detail string) {
	var notMet, unknown finding
	rule, refused := j.refused[id]
	switch {
	case j.lacksCheck(id):
		unknown.keep(reasonNoCheck, id)
	case refused:
		unknown.keep(reasonRefused, rule)
	}
	for _, c := range j.own[id] {
		switch cs, reads := j.checks[c]; {
		case !reads:
		case cs.State == checkFalse:
			notMet.keep(reasonCheckFalse, c)
		case cs.State == checkUnknown:
			unknown.keep(reasonCheckUnknown, c)
		}
	}
	for _, s := range j.children[id] {
		if j.reaches(s, id) {
			unknown.keep(reasonStatementRefused, s)
			continue
		}
		under := j.state(s)
		reason, detail := under.Reason, under.Detail
		if reason == reasonRefused {
			reason, detail = reasonStatementRefused, s
		}
		switch under.State {
		case statementNotMet:
			notMet.keep(reason, detail)
		case statementUnknown:
			unknown.keep(reason, detail)
		}
	}
	for _, p := range j.supporters[id] {
		supporter := statementUnknown
		if !j.reaches(p, id) {
			supporter = j.state(p).State
		}
		switch supporter {
		case statementNotMet:
			notMet.keep(reasonSupporterNotMet, p)
		case statementUnknown:
			unknown.keep(reasonSupporterUnknown, p)
		case statementNotBuilt:
			unknown.keep(reasonSupporterNotBuilt, p)
		}
	}
	switch {
	case notMet.reason != "":
		return statementNotMet, notMet.reason, notMet.detail
	case unknown.reason != "":
		return statementUnknown, unknown.reason, unknown.detail
	}
	return statementMet, reasonChecksTrue, ""
}

// finding is the first reason of one state that the search order meets, and the id it names.
type finding struct {
	reason, detail string
}

// keep sets the finding unless it has one.
func (f *finding) keep(reason, detail string) {
	if f.reason == "" {
		f.reason, f.detail = reason, detail
	}
}

// reaches is whether the state of statement follows the state of target: target is under it or
// supports it, directly or through other statements that are under or support those in their turn.
func (j *statementJudge) reaches(statement, target string) bool {
	seen := map[string]bool{statement: true}
	for next := []string{statement}; len(next) > 0; {
		s := next[len(next)-1]
		next = next[:len(next)-1]
		for _, deps := range [2][]string{j.children[s], j.supporters[s]} {
			for _, d := range deps {
				if d == target {
					return true
				}
				if !seen[d] {
					seen[d] = true
					next = append(next, d)
				}
			}
		}
	}
	return false
}

// lacksCheck is whether the approved spec gives the statement id no check where the tree rules ask
// for one, by the pair of rules of Validate: a leaf (leaf-without-check, R6: "Each leaf statement
// has one or more checks"), or a statement that other statements serve (supported-without-check,
// R5: "A statement that other statements support still has checks of its own"). A statement with
// statements under it, no supporter and no check of its own is neither: the rules take it. A
// statement that serves itself is in a loop, which Validate refuses too, and lacks a check here.
func (j *statementJudge) lacksCheck(id string) bool {
	return len(j.own[id]) == 0 && (len(j.children[id]) == 0 || len(j.supporters[id]) > 0)
}

// ownState is the state of the statement id over those of its own checks that read the subject
// (R29, with R10: "Only the checks that read the subject count"): not built when the files do not
// hold its current text; else not met when one of them is false, also when the tree rules refuse
// it, since a refusal never hides a failure; else unknown when it lacks a check that the tree
// rules ask for (lacksCheck; R10: "Met means that its checks pass"), the tree rules refuse it
// (refusals), or one of them is unknown; and met when all are true, also when a statement with
// statements under it and no supporter has no own check, or none of its checks reads the subject.
func (j *statementJudge) ownState(id string) string {
	if j.notBuilt(id) {
		return statementNotBuilt
	}
	state := statementMet
	if _, refused := j.refused[id]; refused || j.lacksCheck(id) {
		state = statementUnknown
	}
	for _, c := range j.own[id] {
		switch j.checks[c].State { // a check that does not read the subject has no state here
		case checkFalse:
			return statementNotMet
		case checkUnknown:
			state = statementUnknown
		}
	}
	return state
}

// notBuilt is whether the files do not hold the current approved text of the statement id.
func (j *statementJudge) notBuilt(id string) bool {
	h := j.holds[id]
	return !h.Present || !h.Current
}
