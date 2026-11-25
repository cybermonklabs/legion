package store

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// operatorDoc is a file that gives one of the operator's blocks, in a psql heredoc after the line
// starting with lead.
type operatorDoc struct{ path, lead string }

// lockAddressSessionsSQL is the statement each operator block runs first in its transaction: it
// locks the address's user_sessions row, so a sign-out at the same moment waits for the block,
// rather than moving the generation between a statement's read of it and the guard's own check.
const lockAddressSessionsSQL = "select generation from user_sessions where login = :'email' for update"

// operatorBlock is the block both docs give (sqlBlockAfter), which must say the same in both: begin,
// the lock of the address's user_sessions row (lockAddressSessionsSQL), the block's statements, the
// last starting with last, and commit. It answers the statements between begin and commit, the lock
// first, each with its psql variables (:'email', :'issuer', :'subject') turned into the named
// placeholders @email, @issuer and @subject.
func operatorBlock(t *testing.T, docs []operatorDoc, last string) []string {
	t.Helper()
	var block []string
	for _, doc := range docs {
		found := sqlBlockAfter(t, doc.path, doc.lead)
		if len(found) < 4 || found[0] != "begin" || found[1] != lockAddressSessionsSQL || found[len(found)-1] != "commit" ||
			!strings.HasPrefix(found[len(found)-2], last) {
			t.Fatalf("%s gives %q after %q, want begin, %q, the block's statements ending in one starting %q, and commit",
				doc.path, found, doc.lead, lockAddressSessionsSQL, last)
		}
		if block != nil && !slices.Equal(found, block) {
			t.Fatalf("%s gives %q, and %s gives %q: the two must say the same", doc.path, found, docs[0].path, block)
		}
		block = found
	}
	replacer := strings.NewReplacer(":'email'", "@email", ":'issuer'", "@issuer", ":'subject'", "@subject")
	sent := make([]string, 0, len(block)-2)
	for _, statement := range block[1 : len(block)-1] {
		sent = append(sent, replacer.Replace(statement))
	}
	return sent
}

// runOperatorBlock runs statements in one transaction, as psql runs the block between its begin and
// its commit, binding @email, @issuer (the canonical issuer) and @subject wherever each names them.
// Before it commits, it checks that the transaction holds the lock of email's user_sessions row: a
// sign-out's write of that row from another transaction, given 100 ms, is refused with SQLSTATE
// 55P03 (lock_not_available), and rolled back.
func runOperatorBlock(t *testing.T, pool *Pool, statements []string, email, subject string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	args := pgx.NamedArgs{"email": email, "issuer": canonicalIssuer, "subject": subject}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, args); err != nil {
			t.Fatalf("the operator's %q: %v", statement, err)
		}
	}
	signOut, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signOut.Exec(ctx, `set local lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = signOut.Exec(ctx, `update user_sessions set generation = generation + 1 where login = $1`, email)
	if rollback := signOut.Rollback(ctx); rollback != nil {
		t.Fatal(rollback)
	}
	if !isSQLState(err, "55P03") {
		t.Fatalf("a sign-out of %s while the operator's block is open answered %v, want it held off with SQLSTATE 55P03: the block must lock the address's user_sessions row first", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the operator's commit: %v", err)
	}
}

// recordAgainDocs are the two files that give the operator's recording-again of a revocation
// settled by itself: this package's README and the runbook.
var recordAgainDocs = []operatorDoc{
	{"README.md", "- **A revocation settled by itself**"},
	{dispatchRunbook, "A revocation settled without the broker"},
}

// TestARevocationSettledByItselfIsPaidOnceRecordedAgain: a statement that settles an owed
// revocation by itself, which the guard lets through since it is the operator's
// clear-broker-revocation, leaves nothing for the server's loop to pay, so the broker never ends the
// replaced account's credentials. The operator's recording-again, run here from the README and the
// runbook, makes it owed under its next generation, naming the account the address records now as
// its holder: the loop claims it again, also after the address passed to a third account, and
// another address's revocation is left as it was. The block holds the address's user_sessions row
// locked until it commits (runOperatorBlock).
//
// Control, in the test: after the settlement and before the recording-again, nothing is owed and a
// claim takes nothing.
func TestARevocationSettledByItselfIsPaidOnceRecordedAgain(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "settled@example.com"
	signInAll(t, people, email, "member-1", "member-2")
	signInAll(t, people, "other@example.com", "member-1", "member-2")
	if _, err := database.Pool.Exec(ctx, `update broker_revocations set done_at = now() where email = $1`, email); err != nil {
		t.Fatalf("a statement settling the revocation by itself: %v", err)
	}
	if count, _, err := revocations.Owed(ctx); err != nil || count != 1 {
		t.Fatalf("control: after the settlement, Owed = %d, %v; want 1, the other address's alone", count, err)
	}
	if owed, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || owed[0].Email != "other@example.com" {
		t.Fatalf("control: after the settlement, a claim takes %+v, %v; want the other address's alone", owed, err)
	}

	// The address passes to a third account meanwhile: the recording-again names it as the holder.
	signInAll(t, people, email, "member-3")
	ageClaims(t, database, email)
	runOperatorBlock(t, database.Pool, operatorBlock(t, recordAgainDocs, "update broker_revocations set generation = generation + 1"), email, "member-1")
	rows := brokerRevocationsOf(t, database, email)
	if !slices.Contains(rows, owedRow("member-1", "member-3", 2)) {
		t.Fatalf("after the recording-again, the revocations of %s are %+v; want member-1's owed at generation 2, held by member-3", email, rows)
	}
	if got, want := sessionGenerationsOf(t, database, email)["member-1"], sessionGenerationOf(t, database.Pool, email); got != want {
		t.Fatalf("after the recording-again, member-1's revocation records session generation %d, want the address's %d, which member-3's sign-in left", got, want)
	}
	if owing, err := revocations.Owing(ctx, email); err != nil || len(owing) != 2 {
		t.Fatalf("after the recording-again, %s owes %+v, %v; want two: member-1's again and member-2's, which member-3's sign-in recorded", email, owing, err)
	}
}

// skippedRevocationDocs are the two files that give the operator's recording of the revocation an
// account written into people.sub by itself skipped: this package's README and the runbook.
var skippedRevocationDocs = []operatorDoc{
	{"README.md", "- **An account written into `people.sub` by itself**"},
	{dispatchRunbook, "The broker refuses an account it was told no longer holds an address until a later revocation"},
}

// TestAnAccountWrittenBackByItselfIsRecoveredByRecordingTheSkippedRevocation: member-a holds the
// address and member-b takes it, whose sign-in records member-a's revocation, still owed and unpaid.
// Then a statement writes member-a back into people.sub by itself: it records no revocation of
// member-b, whose broker credentials would stay live, and settles nothing of member-a, whose owed
// revocation the loop would pay first and so end what member-a holds. The operator's recovery, run
// here from the README and the runbook, settles member-a's revocation, as a sign-in that takes an
// address back does, and makes member-b's owed, naming member-a, the account people records now, as
// its holder, at the address's session generation; a claim takes it, so the server's loop pays it.
// The block holds the address's user_sessions row locked until it commits (runOperatorBlock).
//
// Controls, in the test: after the stray write and before the recovery, nothing of member-b is
// owed; and, at another address, the block without its settle leaves member-a's revocation owed
// beside member-b's.
func TestAnAccountWrittenBackByItselfIsRecoveredByRecordingTheSkippedRevocation(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	statements := operatorBlock(t, skippedRevocationDocs, "insert into broker_revocations ")
	// writtenBack signs member-a and then member-b in at email, leaving member-a's revocation owed,
	// and then writes member-a back into people.sub by itself.
	writtenBack := func(email string) {
		t.Helper()
		signInAll(t, people, email, "member-a", "member-b")
		owing, err := revocations.Owing(ctx, email)
		if err != nil || len(owing) != 1 || owing[0].Subject != "member-a" || owing[0].HolderSubject != "member-b" {
			t.Fatalf("before the stray write, %s owes %s, %v; want member-a's alone, which member-b took", email, owedText(owing...), err)
		}
		if _, err := database.Pool.Exec(ctx, `update people set sub = 'member-a' where email = $1`, email); err != nil {
			t.Fatalf("the statement writing member-a back by itself: %v", err)
		}
	}
	// owedSubjects is the replaced accounts email owes a revocation of, in the order a pass pays them.
	owedSubjects := func(email string) []string {
		t.Helper()
		owing, err := revocations.Owing(ctx, email)
		if err != nil {
			t.Fatalf("Owing(%s): %v", email, err)
		}
		var subjects []string
		for _, revocation := range owing {
			subjects = append(subjects, revocation.Subject)
		}
		return subjects
	}

	const email = "written-back@example.com"
	writtenBack(email)
	if got := owedSubjects(email); !slices.Equal(got, []string{"member-a"}) {
		t.Fatalf("control: after member-a was written back by itself, %s owes %v; want member-a's alone, member-b's skipped", email, got)
	}
	runOperatorBlock(t, database.Pool, statements, email, "member-b")
	want := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: "member-b", HolderSubject: "member-a", Generation: 1,
		SessionGeneration: new(sessionGenerationOf(t, database.Pool, email))}
	if owing, err := revocations.Owing(ctx, email); err != nil || len(owing) != 1 || !reflect.DeepEqual(owing[0], want) {
		t.Fatalf("after the recovery, %s owes %s, %v; want %s alone, member-a's settled", email, owedText(owing...), err, owedText(want))
	}
	var settled bool
	if err := database.Pool.QueryRow(ctx, `select done_at is not null from broker_revocations where email = $1 and subject = 'member-a'`, email).Scan(&settled); err != nil || !settled {
		t.Fatalf("after the recovery, member-a's revocation is settled %t, %v; want settled", settled, err)
	}
	ageClaims(t, database, email)
	if owed, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || !reflect.DeepEqual(owed[0], want) {
		t.Fatalf("the claim after the recovery: %s, %v; want %s", owedText(owed...), err, owedText(want))
	}

	const control = "written-back-control@example.com"
	writtenBack(control)
	unsettled := slices.DeleteFunc(slices.Clone(statements), func(statement string) bool {
		return strings.HasPrefix(statement, "update broker_revocations set done_at = now()")
	})
	if len(unsettled) != len(statements)-1 {
		t.Fatalf("the block %q holds no settle to leave out", statements)
	}
	runOperatorBlock(t, database.Pool, unsettled, control, "member-b")
	if got := owedSubjects(control); !slices.Equal(got, []string{"member-a", "member-b"}) {
		t.Errorf("control: the block without its settle leaves %s owing %v; want member-a's still owed before member-b's, so the settle is what ends it", control, got)
	}
}

// TestAnAccountWrittenBackOverARevocationPaidBeforeIsRecordedAgain runs the same recovery where
// the replaced account's revocation already has a row: member-a and member-b each held the address
// twice, so member-b's revocation from member-a's earlier take-back was recorded and paid. The
// recovery's record of member-b then meets that row, and its conflict arm makes it owed again
// under its next generation, naming member-a as its holder at the address's session generation, so
// the loop ends member-b's credentials, as a sign-in by member-a would have.
//
// Control, in the test: at another address with the same history, the block with its conflict arm
// made do nothing leaves member-b's paid row as it was and nothing of member-b owed, so the arm is
// what records it.
func TestAnAccountWrittenBackOverARevocationPaidBeforeIsRecordedAgain(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	statements := operatorBlock(t, skippedRevocationDocs, "insert into broker_revocations ")
	// paidThenWrittenBack gives email the history: member-a, member-b, member-a (whose sign-in
	// records member-b's revocation, which the loop then pays), member-b again, and a statement
	// writing member-a back into people.sub by itself.
	paidThenWrittenBack := func(email string) {
		t.Helper()
		signInAll(t, people, email, "member-a", "member-b", "member-a")
		if _, err := database.Pool.Exec(ctx, `update broker_revocations set done_at = now() where email = $1 and subject = 'member-b' and done_at is null`, email); err != nil {
			t.Fatalf("the payment of member-b's revocation at %s: %v", email, err)
		}
		signInAll(t, people, email, "member-b")
		if _, err := database.Pool.Exec(ctx, `update people set sub = 'member-a' where email = $1`, email); err != nil {
			t.Fatalf("the statement writing member-a back by itself at %s: %v", email, err)
		}
		rows := brokerRevocationsOf(t, database, email)
		paid := brokerRevocationRow{Issuer: canonicalIssuer, Subject: "member-b", HolderSubject: "member-a", Generation: 1, Done: true}
		if !slices.Contains(rows, paid) || !slices.Contains(rows, owedRow("member-a", "member-b", 2)) {
			t.Fatalf("before the recovery, the revocations of %s are %+v; want member-b's paid at generation 1 and member-a's owed at generation 2", email, rows)
		}
	}

	const email = "written-back-paid@example.com"
	paidThenWrittenBack(email)
	runOperatorBlock(t, database.Pool, statements, email, "member-b")
	want := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: "member-b", HolderSubject: "member-a", Generation: 2,
		SessionGeneration: new(sessionGenerationOf(t, database.Pool, email))}
	if owing, err := revocations.Owing(ctx, email); err != nil || len(owing) != 1 || !reflect.DeepEqual(owing[0], want) {
		t.Fatalf("after the recovery, %s owes %s, %v; want %s alone, member-a's settled", email, owedText(owing...), err, owedText(want))
	}

	const control = "written-back-paid-control@example.com"
	paidThenWrittenBack(control)
	const arm = " on conflict (email, issuer, subject) do update "
	inert := slices.Clone(statements)
	last := len(inert) - 1
	cut := strings.Index(inert[last], arm)
	if cut < 0 {
		t.Fatalf("the block's record %q has no conflict arm %q to leave out", inert[last], arm)
	}
	inert[last] = inert[last][:cut] + " on conflict (email, issuer, subject) do nothing"
	runOperatorBlock(t, database.Pool, inert, control, "member-b")
	if owing, err := revocations.Owing(ctx, control); err != nil || len(owing) != 0 {
		t.Errorf("control: the block with its conflict arm made do nothing leaves %s owing %s, %v; want nothing, so the arm is what records member-b again", control, owedText(owing...), err)
	}
}
