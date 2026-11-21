package specgraph

import (
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// The nodes of TestStatementState beside TestCheckState's. T is checkStateTop, with its own check of
// the code CT, and L1 is checkStateLeaf under it, with its check C1, checkStateCheck. L2 is a second
// statement under T, with its check C2. P is a top statement that supports T, with its check CP: T
// has a check of its own, since a statement that other statements support has one (R5). Some rows
// give T a query of production as its only own check, and add Q, a top statement whose only check
// reads production.
var (
	statementLeafTwo = Statement{ID: "a-late-session-gets-its-messages", Kind: KindBehavior, Parent: checkStateTop.ID,
		Text: "A session that connects after a write gets the message of that write."}
	statementLeafTwoCheck = Check{ID: "a-late-session-reads-the-message", Serves: statementLeafTwo.ID,
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestALateSessionReadsTheMessage", Job: "go-checks"},
		Claim:          "A session that connects after a write reads the message of that write.",
		ProcedureText:  "The program writes one message and then connects one session.\nIt must find the message in the session.",
		FailingExample: "A listener that delivers only the messages that arrive after a session connects."}
	statementSupporter = Statement{ID: "the-store-keeps-each-message", Kind: KindBehavior, Serves: []string{checkStateTop.ID},
		Text: "The store keeps each message until its session reads it."}
	statementSupporterCheck = Check{ID: "the-store-returns-each-message", Serves: statementSupporter.ID,
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestTheStoreReturnsEachMessage", Job: "go-checks"},
		Claim:          "The store returns each message that it keeps to the session that reads it.",
		ProcedureText:  "The program stores two messages and reads the session.\nIt must find both messages.",
		FailingExample: "A store that returns only the newest message."}
	statementTopCheck = Check{ID: "a-sent-message-reaches-its-session", Serves: checkStateTop.ID,
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestASentMessageReachesItsSession", Job: "go-checks"},
		Claim:          "A message that a sender writes reaches the session that it is for.",
		ProcedureText:  "The program connects one session and writes one message to it.\nIt must find the message in the session.",
		FailingExample: "A listener that drops each message."}
	statementTopProduction = Check{ID: "production-delivers-each-message", Serves: checkStateTop.ID,
		Subject: SubjectProduction, Procedure: ProcedureQuery,
		Claim:          "Each message that production received in the last hour reached its session.",
		ProcedureText:  "The query reads each message that production received in the last hour.\nIt returns each message that did not reach its session.",
		FailingExample: "A copy of the message records with one message that did not reach its session."}
	statementFast = Statement{ID: "delivery-is-fast", Kind: KindProperty,
		Text: "Envoy delivers a message to a connected session in less than one second."}
	statementFastCheck = Check{ID: "production-delivery-takes-under-a-second", Serves: statementFast.ID,
		Subject: SubjectProduction, Procedure: ProcedureQuery,
		Claim:          "Each delivery of the last hour took less than one second.",
		ProcedureText:  "The query reads the time of each delivery of the last hour.\nIt returns each delivery that took one second or more.",
		FailingExample: "A copy of the delivery records with one delivery that took two seconds."}
	// S supports L1, and S2 supports S: supporters under T, for R1.
	statementLeafSupporter = Statement{ID: "the-log-keeps-each-stored-message", Kind: KindBehavior, Serves: []string{checkStateLeaf.ID},
		Text: "The log keeps each stored message until the listener starts again."}
	statementLeafSupporterCheck = Check{ID: "the-log-returns-each-stored-message", Serves: statementLeafSupporter.ID,
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestTheLogReturnsEachStoredMessage", Job: "go-checks"},
		Claim:          "The log returns each stored message after the listener starts again.",
		ProcedureText:  "The program stores one message, starts the listener again and reads the log.\nIt must find the message.",
		FailingExample: "A log that drops its messages when the listener starts."}
	statementSupporterOfSupporter = Statement{ID: "the-disk-keeps-each-write", Kind: KindBehavior, Serves: []string{statementLeafSupporter.ID},
		Text: "The disk keeps each write of the log until the log deletes it."}
	statementSupporterOfSupporterCheck = Check{ID: "the-disk-returns-each-write", Serves: statementSupporterOfSupporter.ID,
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestTheDiskReturnsEachWrite", Job: "go-checks"},
		Claim:          "The disk returns each write of the log after the listener starts again.",
		ProcedureText:  "The program writes one record of the log, starts the listener again and reads the disk.\nIt must find the record.",
		FailingExample: "A disk that loses its last write when the listener starts."}
)

// The state of each statement at one subject (R10), and the end of a wait on a statement (R29).
// The base is TestCheckState's at c1, subject the head c1 of branch b, with CT, L2, P and their
// checks added: each check passed at c1 and failed on a fault that author-b planted in the test of
// author-a, so every statement is met. Each row changes the base and names the whole state of each
// statement it is about, and WaitEnds of each statement in waits.
func TestStatementState(t *testing.T) {
	t.Parallel()
	T, L1, L2, P, Q := checkStateTop.ID, checkStateLeaf.ID, statementLeafTwo.ID, statementSupporter.ID, statementFast.ID
	C1, C2, CP, CT := checkStateCheck.ID, statementLeafTwoCheck.ID, statementSupporterCheck.ID, statementTopCheck.ID
	// held is the state of a statement whose files hold its approved version 1, which is current.
	held := func(id, state, reason, detail, own string) StatementState {
		return StatementState{ID: id, State: state, Reason: reason, Detail: detail, Own: own, HeldVersion: 1}
	}
	met := func(id string) StatementState { return held(id, "met", "checks_true", "", "met") }
	missing := func(id string) StatementState {
		return StatementState{ID: id, State: "not_built", Reason: "not_built", Detail: id, Own: "not_built"}
	}
	// mainWithL1False is row v-work 8's change: T's own check CT passes at c1, C1 fails, and the
	// subject is the head c1 of main.
	mainWithL1False := func(s *checkCase) {
		s.runOutcome(C1, OutcomeFail)
		s.Subject.Branch = "main"
	}
	for _, row := range []struct {
		name   string
		change func(s *checkCase)
		want   []StatementState
		waits  map[string]bool
	}{
		{name: "base: every check is true, so every statement is met", change: func(*checkCase) {},
			want:  []StatementState{met(T), met(L1), met(L2), met(P)},
			waits: map[string]bool{T: false, L1: false}},
		{name: "v-state 1: L1's check is false", change: func(s *checkCase) { s.runOutcome(C1, OutcomeFail) },
			want: []StatementState{held(L1, "not_met", "check_false", C1, "not_met"), held(T, "not_met", "check_false", C1, "met"), met(L2), met(P)}},
		{name: "v-state 9: L2 is approved and not in the files", change: func(s *checkCase) { s.drop(L2, C2) },
			want: []StatementState{missing(L2), held(T, "not_built", "not_built", L2, "met"), met(L1)}},
		{name: "v-state 10: P's check is false", change: func(s *checkCase) { s.runOutcome(CP, OutcomeFail) },
			want: []StatementState{held(P, "not_met", "check_false", CP, "not_met"), held(T, "not_met", "supporter_not_met", P, "met"), met(L1)}},
		{name: "v-state 14: L1 has a passing code check and a production check with no result; subject the head of main", change: func(s *checkCase) {
			s.addNodes(checkStateProduction)
			s.Subject.Branch = "main"
		}, want: []StatementState{met(L1), met(T)}, waits: map[string]bool{L1: true, T: true}},
		{name: "precedence: L1 unknown, L2 false", change: func(s *checkCase) {
			s.runOutcome(C1, OutcomeCouldNotRun)
			s.runOutcome(C2, OutcomeFail)
		}, want: []StatementState{held(T, "not_met", "check_false", C2, "met"), held(L1, "unknown", "check_unknown", C1, "unknown"), held(L2, "not_met", "check_false", C2, "not_met")}},
		{name: "precedence: L1 false, L2 not built", change: func(s *checkCase) {
			s.runOutcome(C1, OutcomeFail)
			s.drop(L2, C2)
		}, want: []StatementState{held(T, "not_built", "not_built", L2, "met"), held(L1, "not_met", "check_false", C1, "not_met"), missing(L2)}},
		{name: "v-two-specs: the files hold version 1 of L1, and version 2 is current", change: func(s *checkCase) {
			later := checkStateLeaf
			later.Text = "Envoy delivers each stored message to its session after Envoy starts again."
			s.approve(checkStateApproved.Add(time.Hour), later)
		}, want: []StatementState{
			{ID: L1, State: "not_built", Reason: "not_built", Detail: L1, Own: "not_built", HeldVersion: 1, EarlierText: true},
			held(T, "not_built", "not_built", L1, "met"), met(L2),
		}},
		{name: "v-two-specs: a revert takes L2 out after it read met", change: func(s *checkCase) {
			s.commit("c2", "c1", "docs/x.md")
			s.Subject.Commit = "c2"
			s.drop(L2, C2)
		}, want: []StatementState{missing(L2), held(T, "not_built", "not_built", L2, "met"), met(L1)}},
		{name: "B3: a top statement Q whose only check reads production; subject the head of a branch", change: func(s *checkCase) {
			s.addNodes(statementFast, statementFastCheck)
		}, want: []StatementState{held(Q, "met", "checks_true", "", "met"), met(T)}},
		{name: "B3: the same, subject c9 under deploy d2, its check true there", change: func(s *checkCase) {
			s.addNodes(statementFast, statementFastCheck)
			s.commit("c9", "c1")
			s.Subject = Point{Commit: "c9", Deploy: "d2"}
			s.Results = append(s.Results, productionPassed(statementFastCheck, "d2")...)
		}, want: []StatementState{met(Q), met(T)}, waits: map[string]bool{Q: false}},
		{name: "partly checked: the reviewer judged that L1's checks decide part of it", change: func(s *checkCase) {
			s.partlyChecked(L1)
		}, want: []StatementState{{ID: L1, State: "met", Reason: "checks_true", Own: "met", HeldVersion: 1, PartlyChecked: true}, met(T)}},
		{name: "partly checked and not met: no partly checked", change: func(s *checkCase) {
			s.partlyChecked(L1)
			s.runOutcome(C1, OutcomeFail)
		}, want: []StatementState{held(L1, "not_met", "check_false", C1, "not_met")}},
		{name: "R29 (v-work 8): T's own check is true at main, and L1 is not met", change: mainWithL1False,
			want: []StatementState{held(T, "not_met", "check_false", C1, "met")}, waits: map[string]bool{T: true, L1: false}},
		{name: "R29 (v-work 8): T's own check is unknown at main", change: func(s *checkCase) {
			mainWithL1False(s)
			s.runOutcome(CT, OutcomeCouldNotRun)
		}, want: []StatementState{held(T, "not_met", "check_false", C1, "unknown")}, waits: map[string]bool{T: false}},
		{name: "R29 (v-work 8): T is not in the files at main", change: func(s *checkCase) {
			mainWithL1False(s)
			s.drop(T)
		}, want: []StatementState{missing(T)}, waits: map[string]bool{T: false}},
		{name: "note 4: T has no own check and no supporter, and the files hold its text; subject the head of main", change: func(s *checkCase) {
			alone := statementSupporter
			alone.Serves = nil
			s.replace(alone)
			s.remove(CT)
			s.Subject.Branch = "main"
		}, want: []StatementState{met(T)}, waits: map[string]bool{T: true, "a-statement-the-spec-never-held": false}},
		// R29 with R10: "Only the checks that read the subject count." At the head of main, an own
		// check of production decides nothing, so it holds no wait: the wait reads the own checks that
		// read main, and here there is none.
		{name: "R29: T's only own check reads production; subject the head of main, where the wait ends", change: func(s *checkCase) {
			s.remove(CT)
			s.addNodes(statementTopProduction)
			s.Subject.Branch = "main"
		}, want: []StatementState{met(T)}, waits: map[string]bool{T: true}},
		{name: "R29: T's own code check is true and its own production check has no result; subject the head of main", change: func(s *checkCase) {
			s.addNodes(statementTopProduction)
			s.Subject.Branch = "main"
		}, want: []StatementState{met(T)}, waits: map[string]bool{T: true}},
		{name: "R29: T's own code check is false beside its own production check; subject the head of main, where the wait holds", change: func(s *checkCase) {
			s.addNodes(statementTopProduction)
			s.runOutcome(CT, OutcomeFail)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(T, "not_met", "check_false", CT, "not_met")}, waits: map[string]bool{T: false}},
		{name: "R29 at another branch: T's own check is true; subject the head of branch b", change: func(*checkCase) {},
			want: []StatementState{met(T)}, waits: map[string]bool{T: false}},
		{name: "L1's check is unknown", change: func(s *checkCase) { s.runOutcome(C1, OutcomeCouldNotRun) },
			want: []StatementState{held(L1, "unknown", "check_unknown", C1, "unknown"), held(T, "unknown", "check_unknown", C1, "met")}},
		{name: "P's check is unknown", change: func(s *checkCase) { s.runOutcome(CP, OutcomeCouldNotRun) },
			want: []StatementState{held(P, "unknown", "check_unknown", CP, "unknown"), held(T, "unknown", "supporter_unknown", P, "met")}},
		{name: "P is not built", change: func(s *checkCase) { s.drop(P, CP) },
			want: []StatementState{missing(P), held(T, "unknown", "supporter_not_built", P, "met")}},
		// R1: "A statement is true when the statements under it, the statements that support it, and
		// its own checks are true." v-state: "a failing check of a statement that supports this one
		// (not met)", and a failing result reads not met for "its statement and each statement above
		// it". So T follows the state of L1, which follows the state of its supporter S, at each
		// level. S supports L1 and S2 supports S.
		{name: "R1 (the acceptance run's S2): S's check is false, so L1, which S supports, is not met, and so is T above it", change: func(s *checkCase) {
			s.addCheck(statementLeafSupporterCheck, statementLeafSupporter)
			s.runOutcome(statementLeafSupporterCheck.ID, OutcomeFail)
		}, want: []StatementState{
			held(statementLeafSupporter.ID, "not_met", "check_false", statementLeafSupporterCheck.ID, "not_met"),
			held(L1, "not_met", "supporter_not_met", statementLeafSupporter.ID, "met"),
			held(T, "not_met", "supporter_not_met", statementLeafSupporter.ID, "met"), met(L2), met(P),
		}},
		{name: "R1: S2's check is false, so S, which S2 supports, is not met, and so are L1, which S supports, and T above it", change: func(s *checkCase) {
			s.addCheck(statementLeafSupporterCheck, statementLeafSupporter)
			s.addCheck(statementSupporterOfSupporterCheck, statementSupporterOfSupporter)
			s.runOutcome(statementSupporterOfSupporterCheck.ID, OutcomeFail)
		}, want: []StatementState{
			held(statementSupporterOfSupporter.ID, "not_met", "check_false", statementSupporterOfSupporterCheck.ID, "not_met"),
			held(statementLeafSupporter.ID, "not_met", "supporter_not_met", statementSupporterOfSupporter.ID, "met"),
			held(L1, "not_met", "supporter_not_met", statementLeafSupporter.ID, "met"),
			held(T, "not_met", "supporter_not_met", statementLeafSupporter.ID, "met"),
		}},
		{name: "R1: S's check is unknown, so L1 is unknown through S, and so is T above it", change: func(s *checkCase) {
			s.addCheck(statementLeafSupporterCheck, statementLeafSupporter)
			s.runOutcome(statementLeafSupporterCheck.ID, OutcomeCouldNotRun)
		}, want: []StatementState{
			held(L1, "unknown", "supporter_unknown", statementLeafSupporter.ID, "met"),
			held(T, "unknown", "supporter_unknown", statementLeafSupporter.ID, "met"), met(L2),
		}},
		// R10: "Not built: the files at that commit do not hold the approved text of the statement, or
		// of a statement under it." A supporter is neither, so a supporter that is not built makes L1
		// unknown, as it makes T unknown in the row above, and T above L1 unknown too.
		{name: "R1 with R10: S is not built, so L1 is unknown through S, and so is T above it", change: func(s *checkCase) {
			s.addCheck(statementLeafSupporterCheck, statementLeafSupporter)
			s.drop(statementLeafSupporter.ID, statementLeafSupporterCheck.ID)
		}, want: []StatementState{
			missing(statementLeafSupporter.ID),
			held(L1, "unknown", "supporter_not_built", statementLeafSupporter.ID, "met"),
			held(T, "unknown", "supporter_not_built", statementLeafSupporter.ID, "met"),
		}},
		// Validate refuses the loop of Q and T, and the loop refuses each of its statements. Q reads
		// refused (the derivation gives no clean state for a tree that the rules refuse). T is
		// refused too, and its own check is false: a refusal never hides a failure, so T reads not met.
		{name: "a loop of supporters: Q reads refused (loop), and T, refused too, reads not met by its own check", change: func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = []string{Q}
			s.replace(loop)
			fast := statementFast
			fast.Serves = []string{T}
			s.addNodes(fast, statementFastCheck)
			s.runOutcome(CT, OutcomeFail)
		}, want: []StatementState{held(Q, "unknown", "refused", "loop", "unknown"), held(T, "not_met", "check_false", CT, "not_met")}},
		// Each statement of a loop that the tree rules refuse is refused, whichever id the loop rule
		// names first, so a wait on each holds (R29), and no answer depends on the order of the ids.
		{name: "a loop of T and L1 under it (T serves L1): Validate names L1, and each of the two reads refused; subject the head of main", change: func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = []string{L1}
			s.replace(loop)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(T, "unknown", "refused", "loop", "unknown"), held(L1, "unknown", "refused", "loop", "unknown")},
			waits: map[string]bool{T: false, L1: false}},
		{name: "a loop of T and P (T serves P): Validate names T, and P, whose id sorts last, reads refused too; subject the head of main", change: func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = []string{P}
			s.replace(loop)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(T, "unknown", "refused", "loop", "unknown"), held(P, "unknown", "refused", "loop", "unknown")},
			waits: map[string]bool{T: false, P: false}},
		// R6: "Each leaf statement has one or more checks." R7: "silence is not a pass." A leaf that
		// the approved spec gives no check reads unknown when the files hold its text, and so does
		// each statement above it that it would otherwise leave met. Its own state for a wait is
		// unknown too (R10: "Met means that its checks pass"), and the pilot calls the derivation
		// with no reader that would refuse the files first.
		{name: "R6: L2, a leaf with no check, reads unknown, and so does T above it", change: func(s *checkCase) { s.remove(C2) },
			want: []StatementState{held(L2, "unknown", "no_check", L2, "unknown"), held(T, "unknown", "no_check", L2, "met"), met(L1)}},
		{name: "R6: P, a supporter with no check, reads unknown, and T reads its supporter unknown", change: func(s *checkCase) { s.remove(CP) },
			want: []StatementState{held(P, "unknown", "no_check", P, "unknown"), held(T, "unknown", "supporter_unknown", P, "met")}},
		{name: "R29 with R6: at main, a wait on L2 or P, leaves with no check, holds; a wait on T, whose own check is true, ends", change: func(s *checkCase) {
			s.remove(C2, CP)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(L2, "unknown", "no_check", L2, "unknown"), held(P, "unknown", "no_check", P, "unknown"), held(T, "unknown", "no_check", L2, "met")},
			waits: map[string]bool{L2: false, P: false, T: true}},
		{name: "precedence: L1 false, L2 a leaf with no check", change: func(s *checkCase) {
			s.runOutcome(C1, OutcomeFail)
			s.remove(C2)
		}, want: []StatementState{held(T, "not_met", "check_false", C1, "met"), held(L2, "unknown", "no_check", L2, "unknown")}},
		{name: "precedence: L2 a leaf with no check that the files do not hold", change: func(s *checkCase) {
			s.remove(C2)
			s.drop(L2)
		}, want: []StatementState{missing(L2), held(T, "not_built", "not_built", L2, "met")}},
		// R5: "A statement that other statements support still has checks of its own." Validate
		// pairs this rule with R6's (supported-without-check beside leaf-without-check), and so does
		// the derivation: T, which P supports, with statements under it and no own check, reads
		// unknown, no_check, as a leaf with no check does, and so does each statement above it; its
		// own state is unknown, so a wait on it holds. "note 4" is the tree that the rules take: no
		// supporter, and the wait ends.
		{name: "R5: T, which P supports, has statements under it and no own check; subject the head of main, where a wait on T holds", change: func(s *checkCase) {
			s.remove(CT)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(T, "unknown", "no_check", T, "unknown"), met(L1), met(L2), met(P)},
			waits: map[string]bool{T: false, L1: true}},
		{name: "R5: Q above T reads unknown, no_check, with the detail T", change: func(s *checkCase) {
			under := checkStateTop
			under.Parent = Q
			s.replace(under)
			s.addNodes(statementFast, statementFastCheck)
			s.remove(CT)
		}, want: []StatementState{held(Q, "unknown", "no_check", T, "met"), held(T, "unknown", "no_check", T, "unknown"), met(L1)}},
		// D1: the derivation gives no clean state for a tree that the rules refuse. A statement that
		// Validate refuses reads unknown, refused, with the rule as the detail, and each statement
		// above it reads unknown, statement_refused, with the detail that names it; its own state is
		// unknown, so a wait on it holds. A problem that names no check and no statement refuses each
		// check and statement of the tree: a file is no relation of The Spec, so the split of the tree
		// into files changes no state.
		{name: "D1: L1, whose text has no letter and no digit, reads refused, and T above it reads unknown; subject the head of main", change: func(s *checkCase) {
			bare := checkStateLeaf
			bare.Text = "-"
			s.replace(bare)
			s.Subject.Branch = "main"
		}, want: []StatementState{held(L1, "unknown", "refused", "no-text", "unknown"), held(T, "unknown", "statement_refused", L1, "met"), met(L2), met(P)},
			waits: map[string]bool{L1: false, T: true}},
		{name: "D1: a problem of a term in the file of the statements refuses each of them", change: func(s *checkCase) {
			s.files["spec.yaml"] = append(s.files["spec.yaml"], Term{ID: "message", Name: "message", Definition: "-"})
		}, want: []StatementState{held(T, "unknown", "refused", "bad-term", "unknown"), held(L1, "unknown", "refused", "bad-term", "unknown"),
			held(L2, "unknown", "refused", "bad-term", "unknown"), held(P, "unknown", "refused", "bad-term", "unknown")}},
		{name: "D1: a problem of a component in a file of its own refuses each statement of the tree", change: func(s *checkCase) {
			s.files["components.yaml"] = []specNode{Component{ID: "listener", Name: "the listener", Purpose: "-"}}
		}, want: []StatementState{held(T, "unknown", "refused", "bad-component", "unknown"), held(L1, "unknown", "refused", "bad-component", "unknown"),
			held(L2, "unknown", "refused", "bad-component", "unknown"), held(P, "unknown", "refused", "bad-component", "unknown")}},
		{name: "D1: a problem of a term refuses P and its check in a file of their own too", change: func(s *checkCase) {
			s.drop(P, CP)
			s.files["p.yaml"] = []specNode{statementSupporter, statementSupporterCheck}
			s.files["spec.yaml"] = append(s.files["spec.yaml"], Term{ID: "message", Name: "message", Definition: "-"})
		}, want: []StatementState{held(T, "unknown", "refused", "bad-term", "unknown"), held(P, "unknown", "refused", "bad-term", "unknown")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newStatementCase(t)
			row.change(s)
			state := derive(t, s.input())
			for _, want := range row.want {
				i := slices.IndexFunc(state.Statements, func(st StatementState) bool { return st.ID == want.ID })
				switch {
				case i < 0:
					t.Errorf("Derive holds no state of %s: %+v", want.ID, state.Statements)
				case state.Statements[i] != want:
					t.Errorf("the state of %s:\n got %+v\nwant %+v", want.ID, state.Statements[i], want)
				}
			}
			for id, ends := range row.waits {
				if got := state.WaitEnds(id); got != ends {
					t.Errorf("WaitEnds(%s) = %v, want %v", id, got, ends)
				}
			}
		})
	}
}

// R6: "Each leaf statement has one or more checks." R5: "A statement that other statements support
// still has checks of its own." The rules of the tree and the derivation ask for a check of one set
// of statements: Validate refuses a statement with leaf-without-check or supported-without-check
// exactly when Derive reads it unknown, no_check, with itself as the detail. Each row is the base of
// TestStatementState with checks taken out.
func TestTheTreeAndTheDerivationAskForACheckOfOneSetOfStatements(t *testing.T) {
	t.Parallel()
	T, L1, L2, P := checkStateTop.ID, checkStateLeaf.ID, statementLeafTwo.ID, statementSupporter.ID
	for _, row := range []struct {
		name   string
		change func(s *checkCase)
		want   []string // the statements of which the two ask a check, sorted
	}{
		{"the base", func(*checkCase) {}, nil},
		{"leaves with no check", func(s *checkCase) { s.remove(checkStateCheck.ID, statementLeafTwoCheck.ID) }, []string{L2, L1}},
		{"a supporter with no check, a leaf", func(s *checkCase) { s.remove(statementSupporterCheck.ID) }, []string{P}},
		{"a supported statement with statements under it and no check", func(s *checkCase) { s.remove(statementTopCheck.ID) }, []string{T}},
		{"a statement with statements under it, no supporter and no check", func(s *checkCase) {
			alone := statementSupporter
			alone.Serves = nil
			s.replace(alone)
			s.remove(statementTopCheck.ID)
		}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newStatementCase(t)
			row.change(s)
			var refused, derived []string
			for _, p := range Validate(s.parse(), Options{}) {
				if p.Rule == "leaf-without-check" || p.Rule == "supported-without-check" {
					refused = append(refused, p.Node)
				}
			}
			for _, st := range derive(t, s.input()).Statements {
				if st.Reason == "no_check" && st.Detail == st.ID {
					derived = append(derived, st.ID)
				}
			}
			slices.Sort(refused)
			slices.Sort(derived)
			if !slices.Equal(refused, row.want) {
				t.Errorf("Validate asks a check of %q, want %q", refused, row.want)
			}
			if !slices.Equal(derived, row.want) {
				t.Errorf("Derive reads no_check of %q, want %q", derived, row.want)
			}
		})
	}
}

// D1: the derivation gives no clean state for a tree that the rules refuse. For each problem that
// the schema or Validate finds in the files, Derive reads no check that the problem names as true,
// and no statement that it names, or that is above a check or a statement that it names, as met; a
// problem that names no check and no statement does so for each check and statement of the tree,
// whatever file holds it. The rule is read from the problems, so a rule that the schema or Validate
// gains is under the test too. Each row is the base of TestStatementState with one fault.
func TestNoTreeThatTheRulesRefuseReadsTrueOrMet(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		change func(s *checkCase)
	}{
		{"a program check that names no job", func(s *checkCase) { s.refuse(func(c *Check) { c.Test.Job = "" }) }},
		{"a program check that names no test", func(s *checkCase) { s.refuse(func(c *Check) { c.Test = nil }) }},
		{"a check whose claim has no letter and no digit", func(s *checkCase) { s.refuse(func(c *Check) { c.Claim = "-" }) }},
		{"a check of another procedure", func(s *checkCase) { s.refuse(func(c *Check) { c.Procedure = "script" }) }},
		{"a check of another subject", func(s *checkCase) { s.refuse(func(c *Check) { c.Subject = "staging" }) }},
		{"a check whose failing example has no letter and no digit", func(s *checkCase) { s.refuse(func(c *Check) { c.FailingExample = "-" }) }},
		{"a statement whose text has no letter and no digit", func(s *checkCase) {
			bare := checkStateLeaf
			bare.Text = "-"
			s.replace(bare)
		}},
		{"a statement of another kind", func(s *checkCase) {
			other := statementLeafTwo
			other.Kind = "rule"
			s.replace(other)
		}},
		{"a leaf with no check", func(s *checkCase) { s.remove(statementLeafTwoCheck.ID) }},
		{"a supported statement with no check of its own", func(s *checkCase) { s.remove(statementTopCheck.ID) }},
		{"a term with no definition in the file of the statements", func(s *checkCase) {
			s.files["spec.yaml"] = append(s.files["spec.yaml"], Term{ID: "message", Name: "message", Definition: "-"})
		}},
		{"a term with no definition in another file than a statement and its check", func(s *checkCase) {
			s.drop(statementSupporter.ID, statementSupporterCheck.ID)
			s.files["p.yaml"] = []specNode{statementSupporter, statementSupporterCheck}
			s.files["spec.yaml"] = append(s.files["spec.yaml"], Term{ID: "message", Name: "message", Definition: "-"})
		}},
		{"a component with no purpose in a file of its own", func(s *checkCase) {
			s.files["components.yaml"] = []specNode{Component{ID: "listener", Name: "the listener", Purpose: "-"}}
		}},
		{"a loop of a statement and the statement under it", func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = []string{checkStateLeaf.ID}
			s.replace(loop)
		}},
		{"a loop of supporters whose other member's id sorts after the one the rule names", func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = []string{statementSupporter.ID}
			s.replace(loop)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newStatementCase(t)
			row.change(s)
			files := s.parse()
			problems := append(schemaProblems(files), Validate(files, Options{})...)
			if len(problems) == 0 {
				t.Fatal("the schema and Validate find no problem, so the row tests nothing")
			}
			state := derive(t, s.input())
			for _, p := range problems {
				named := namedBy(files, p)
				for _, id := range withParents(files, named) {
					for _, c := range state.Checks {
						if c.ID == id && c.State == "true" {
							t.Errorf("%s: check %s reads %+v", p.Rule, id, c)
						}
					}
					for _, st := range state.Statements {
						if st.ID == id && st.State == "met" {
							t.Errorf("%s: statement %s reads %+v", p.Rule, id, st)
						}
					}
				}
				for _, st := range state.Statements {
					if slices.Contains(named, st.ID) && st.Own == "met" {
						t.Errorf("%s: statement %s reads %+v, so a wait on it ends", p.Rule, st.ID, st)
					}
				}
			}
		})
	}
}

// namedBy is each check and statement of files that the problem p names: the node it names, and,
// for a check, the statement it serves; for a loop, each statement of the cycle that its detail
// writes ("loop: a -[parent]-> b -[serves]-> a"); for a problem that names no check and no
// statement, each check and statement of files, whatever file holds it.
func namedBy(files Tree, p Problem) []string {
	var named []string
	switch c, held := files.CheckByID(p.Node); {
	case held:
		named = append(named, c.ID, c.Serves)
	case p.Rule == "loop":
		for _, word := range strings.Fields(strings.TrimPrefix(p.Detail, "loop: ")) {
			if !strings.HasPrefix(word, "-[") && !slices.Contains(named, word) {
				named = append(named, word)
			}
		}
	default:
		if _, held := files.StatementByID(p.Node); held {
			return []string{p.Node}
		}
		for _, st := range files.Statements {
			named = append(named, st.ID)
		}
		for _, c := range files.Checks {
			named = append(named, c.ID)
		}
	}
	return named
}

// withParents is ids and each statement of files above one of them.
func withParents(files Tree, ids []string) []string {
	named := slices.Clone(ids)
	for i := 0; i < len(named); i++ {
		if st, held := files.StatementByID(named[i]); held && st.Parent != "" && !slices.Contains(named, st.Parent) {
			named = append(named, st.Parent)
		}
	}
	return named
}

// Derive gives the subject, the state of each check that CheckStates judges, and the state of each
// current approved statement in preorder: top statements by id, then the statements under each
// one by id, depth first. A statement in a loop of parents, which Validate refuses, is still listed
// once.
func TestDeriveListsEachStatementInPreorder(t *testing.T) {
	t.Parallel()
	s := newStatementCase(t)
	s.addNodes(statementFast, statementFastCheck)
	in := s.input()
	state := derive(t, in)
	if state.Subject != in.Subject {
		t.Errorf("Subject = %+v, want %+v", state.Subject, in.Subject)
	}
	if checks := checkStates(t, in); !slices.Equal(state.Checks, checks) {
		t.Errorf("Checks:\n got %+v\nwant %+v", state.Checks, checks)
	}
	want := []string{statementFast.ID, checkStateTop.ID, statementLeafTwo.ID, checkStateLeaf.ID, statementSupporter.ID}
	if got := statementIDs(state); !slices.Equal(got, want) {
		t.Errorf("the statements of Derive: %q, want %q", got, want)
	}

	loop := checkStateTop
	loop.Parent = checkStateLeaf.ID
	s.replace(loop)
	got := statementIDs(derive(t, s.input()))
	if sorted := slices.Sorted(slices.Values(got)); !slices.Equal(sorted, slices.Sorted(slices.Values(want))) {
		t.Errorf("with a loop of parents, the statements of Derive: %q, want each of %q once", got, want)
	}
}

// The derivation of a statement recurses into the statements under it and its supporters, and its
// loop guard (reaches) is what ends a loop of parent and support relations. This test derives each
// loop of the rows of TestStatementState, TestNoTreeThatTheRulesRefuseReadsTrueOrMet and
// TestDeriveListsEachStatementInPreorder under a stack cap of 16 MiB, so that a derivation with no
// guard fails in a fraction of a second, where Go's own limit of 1 GB would take far longer and that
// much memory. Exceeding the cap ends the whole test binary (runtime/debug.SetMaxStack). The test
// does not run in parallel, so no other test runs under the cap: each test that calls t.Parallel
// waits until the tests that do not have ended. It sets the previous limit back when it ends.
func TestTheDerivationEndsOnEachLoop(t *testing.T) {
	previous := debug.SetMaxStack(16 << 20)
	t.Cleanup(func() { debug.SetMaxStack(previous) })
	T, L1, P, Q := checkStateTop.ID, checkStateLeaf.ID, statementSupporter.ID, statementFast.ID
	serves := func(ids ...string) func(s *checkCase) {
		return func(s *checkCase) {
			loop := checkStateTop
			loop.Serves = ids
			s.replace(loop)
		}
	}
	for _, row := range []struct {
		name   string
		change func(s *checkCase)
	}{
		{"a loop of supporters: T serves Q, and Q serves T", func(s *checkCase) {
			serves(Q)(s)
			fast := statementFast
			fast.Serves = []string{T}
			s.addNodes(fast, statementFastCheck)
		}},
		{"T serves L1 under it", serves(L1)},
		{"T serves P, which serves T", serves(P)},
		{"a loop of parents: T under L1, which is under T", func(s *checkCase) {
			loop := checkStateTop
			loop.Parent = L1
			s.replace(loop)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newStatementCase(t)
			row.change(s)
			in := s.input()
			var want []string
			for _, st := range in.Files.Statements {
				want = append(want, st.ID)
			}
			slices.Sort(want)
			if got := slices.Sorted(slices.Values(statementIDs(derive(t, in)))); !slices.Equal(got, want) {
				t.Errorf("the statements of Derive: %q, want each of %q once", got, want)
			}
		})
	}
}

// statementIDs is the id of each statement state, in order.
func statementIDs(s State) []string {
	var ids []string
	for _, st := range s.Statements {
		ids = append(ids, st.ID)
	}
	return ids
}

// newStatementCase is the base of TestStatementState: TestCheckState's base with CT, L2, P and
// their checks, each with a pass at c1 and a caught failing example, and the tie of C.
func newStatementCase(t *testing.T) *checkCase {
	t.Helper()
	s := newCheckCase(t)
	s.addCheck(statementTopCheck)
	s.addCheck(statementLeafTwoCheck, statementLeafTwo)
	s.addCheck(statementSupporterCheck, statementSupporter)
	if problems := Validate(s.parse(), Options{}); len(problems) != 0 {
		t.Fatalf("the base is not a valid tree: %+v", problems)
	}
	return s
}

// addNodes approves each node with the base's nodes and writes it at the end of spec.yaml.
func (s *checkCase) addNodes(nodes ...specNode) {
	s.t.Helper()
	for _, n := range nodes {
		s.approve(checkStateApproved, n)
		s.files["spec.yaml"] = append(slices.Clone(s.files["spec.yaml"]), n)
	}
}

// addCheck adds the program check c, after the nodes before it, with the tie of C, a pass of its
// job at c1 and a failing example at c1 on which its test failed.
func (s *checkCase) addCheck(c Check, before ...specNode) {
	s.t.Helper()
	s.addNodes(append(before, c)...)
	s.tieLikeC(c.ID)
	run := Result{Check: c.ID, Kind: ResultRun, Outcome: OutcomePass, Commit: "c1", Job: c.Test.Job, Attempt: 1, At: checkStateRun}
	fault := run
	fault.Kind, fault.Outcome, fault.At = ResultFailingExample, OutcomeFail, checkStateRun.Add(5*time.Minute)
	fault.FaultAuthor, fault.TestAuthors = "author-b", []string{"author-a"}
	s.Results = append(s.Results, run, fault)
}

// runOutcome sets the outcome of each run of the check id.
func (s *checkCase) runOutcome(id string, outcome Outcome) {
	for i := range s.Results {
		if s.Results[i].Check == id && s.Results[i].Kind == ResultRun {
			s.Results[i].Outcome = outcome
		}
	}
}

// drop takes the nodes with the ids out of the files; the approved spec keeps them.
func (s *checkCase) drop(ids ...string) {
	for name, nodes := range s.files {
		s.files[name] = slices.DeleteFunc(slices.Clone(nodes), func(n specNode) bool { return slices.Contains(ids, specNodeID(n)) })
	}
}

// remove takes the nodes with the ids out of the files and out of the approved spec. A result of a
// check that it takes out keeps the digest of the text that ran, as a stored result does.
func (s *checkCase) remove(ids ...string) {
	s.t.Helper()
	s.drop(ids...)
	for _, id := range ids {
		if versions := s.Approved.Nodes[id].Versions; len(versions) > 0 {
			if text, problems := versionText(id, versions[len(versions)-1]); len(problems) == 0 && len(text.Checks) == 1 {
				for i := range s.Results {
					if s.Results[i].Check == id && s.Results[i].CheckDigest == "" {
						s.Results[i].CheckDigest = text.Checks[0].Digest()
					}
				}
			}
		}
		delete(s.Approved.Nodes, id)
	}
}

// partlyChecked marks the statement id as partly checked in the approved spec.
func (s *checkCase) partlyChecked(id string) {
	node := s.Approved.Nodes[id]
	node.PartlyChecked = new(true)
	s.Approved.Nodes[id] = node
}

// productionPassed is a pass of the production check c under deploy and a failing example on
// which it failed.
func productionPassed(c Check, deploy string) []Result {
	fault := Result{Check: c.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(5 * time.Minute)}
	return []Result{productionRun(c, deploy, OutcomePass, checkStateRun), fault}
}
