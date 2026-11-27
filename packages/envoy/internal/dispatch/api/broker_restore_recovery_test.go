package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// recoveryRig is a Dispatch server and a real broker (ciphertest.NewRig) for the runbook's
// recovery: the pass the server runs is the one the loop runs, and the broker is the one the
// recovery's reads read.
type recoveryRig struct {
	rig         *ciphertest.Rig
	dispatch    *store.Store
	served      *server
	people      *store.PgPeopleStore
	revocations *store.BrokerRevocations
	broker      *cipher.Client
}

// recoveryIssuer is the sign-in issuer every account of these tests belongs to.
const recoveryIssuer = "https://accounts.google.com"

// newRecoveryRig builds the pair.
func newRecoveryRig(t *testing.T) *recoveryRig {
	t.Helper()
	rig := ciphertest.NewRig(t)
	dispatch := storetest.Open(t)
	deps, err := NewDeps(withDocumentService(t, DepsInput{
		Store: dispatch, Identity: headerIdentity(dispatch), ServerURL: "https://dispatch.example", Events: events.NewBroker(),
		AgentSecretsURL: rig.URL, AgentSecretsTokenFile: accountTokenFile(t, rig.UIToken),
	}))
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	return &recoveryRig{
		rig: rig, dispatch: dispatch,
		served:      &server{deps: deps, lifetime: t.Context()},
		people:      store.NewPgPeopleStore(dispatch.Pool, testSigningKey, nil, store.WithBrokerRevocations(recoveryIssuer)),
		revocations: store.NewBrokerRevocations(dispatch.Pool),
		broker:      cipher.New(rig.URL, accountTokenFile(t, rig.UIToken)),
	}
}

// refused reports whether the broker refuses subject under email.
func (r *recoveryRig) refused(t *testing.T, email, subject string) bool {
	t.Helper()
	var refused bool
	if err := r.rig.Store.Pool.QueryRow(context.Background(), `select exists (select 1 from replaced_person_subjects where person = $1 and subject = $2)`,
		email, subject).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	return refused
}

// signInMachine gives account a live sign-in of email on a machine at the broker, the credential a
// restore brings back: `agent-secrets login` writes one, and the revoke ends it.
func (r *recoveryRig) signInMachine(t *testing.T, email, account, thumbprint string) string {
	t.Helper()
	var id string
	if err := r.rig.Store.Pool.QueryRow(context.Background(), `insert into person_signins (person, thumbprint, machine_account, issuer, subject)
		values ($1, $2, 'machine', $3, $4) returning id::text`, email, thumbprint, recoveryIssuer, account).Scan(&id); err != nil {
		t.Fatalf("the sign-in of %s at %s on a machine: %v", account, email, err)
	}
	return id
}

// machineSignInLive reports whether the sign-in id is still live at the broker.
func (r *recoveryRig) machineSignInLive(t *testing.T, id string) bool {
	t.Helper()
	var live bool
	if err := r.rig.Store.Pool.QueryRow(context.Background(), `select revoked_at is null from person_signins where id = $1::uuid`, id).Scan(&live); err != nil {
		t.Fatal(err)
	}
	return live
}

// ageClaims puts every claim an hour back, so the next pass may claim the address again at once.
func (r *recoveryRig) ageClaims(t *testing.T) {
	t.Helper()
	if _, err := r.dispatch.Pool.Exec(context.Background(), `update broker_revocations set tried_at = tried_at - interval '1 hour' where tried_at is not null`); err != nil {
		t.Fatal(err)
	}
}

// TestABrokerRestoredAloneIsRecoveredByTellingItAgainWhatWasSettledSince: the broker's database,
// restored alone to a moment before revocations Dispatch paid, holds those replaced accounts'
// credentials live again and no record refusing them. Nothing at Dispatch sees it: each
// revocation is settled, so the loop claims none and the owed gauges read zero. The runbook's
// other block records again every revocation settled at or after the broker's backup less an
// hour, and the loop then tells the broker each: it refuses the replaced account again and ends
// what it holds.
//
// Two addresses. At lostbroker@, account-b replaced account-a. At takeback@, account-t2 replaced
// account-t1 and then account-t1 took the address back, so both of its revocations were settled
// after the backup, and account-t1 holds it again: the block records account-t2's revocation
// again, naming account-t1 as its holder, and leaves account-t1's own alone, since the broker must
// not be told that the holder was replaced. account-t1 stays able to trade and account-t2 is
// refused.
//
// The moment the test gives the block is two minutes after the settlements, as an operator's is
// when the backup's record rounds it to the minute, names the backup's end, or comes from a clock
// ahead of Dispatch's. The hour's margin is what reaches back over that; the rows would otherwise
// be left out and the accounts would keep their credentials with nothing showing it.
//
// Control, in the test: before the block, a pass changes nothing and the owed count is zero - the
// state the recovery exists for. Control in the runbook, a break restored by checksum: the block
// without its exclusion of a revocation whose account holds its address again fails, since it
// would record that account as its own holder's replaced account.
func TestABrokerRestoredAloneIsRecoveredByTellingItAgainWhatWasSettledSince(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email, takeback = "lostbroker@example.com", "takeback@example.com"
	signIn := func(email, account string) {
		t.Helper()
		if err := r.people.SignInSubject(ctx, email, account, "refresh-"+account, "", model.MemberFull); err != nil {
			t.Fatalf("the sign-in of %s at %s: %v", account, email, err)
		}
	}
	pay := func() {
		t.Helper()
		r.ageClaims(t)
		r.served.revokeOwedBrokerCredentials(ctx)
	}
	signIn(email, "account-a")
	machine := r.signInMachine(t, email, "account-a", strings.Repeat("a", 43))
	signIn(email, "account-b")
	signIn(takeback, "account-t1")
	signIn(takeback, "account-t2")
	pay()
	t2Machine := r.signInMachine(t, takeback, "account-t2", strings.Repeat("t", 43))
	signIn(takeback, "account-t1")
	pay()
	for _, address := range []string{email, takeback} {
		if owing, err := r.revocations.Owing(ctx, address); err != nil || len(owing) != 0 {
			t.Fatalf("after the passes, %s owes %+v, %v; want every revocation settled", address, owing, err)
		}
	}
	if !r.refused(t, email, "account-a") || r.machineSignInLive(t, machine) || !r.refused(t, takeback, "account-t2") || r.refused(t, takeback, "account-t1") || r.machineSignInLive(t, t2Machine) {
		t.Fatalf("after the passes the broker refuses account-a %t (its sign-in live %t), account-t2 %t (its sign-in live %t) and account-t1 %t; want account-a and account-t2 refused and ended, account-t1 not refused",
			r.refused(t, email, "account-a"), r.machineSignInLive(t, machine), r.refused(t, takeback, "account-t2"), r.machineSignInLive(t, t2Machine), r.refused(t, takeback, "account-t1"))
	}
	// The moment the operator gives the block, as the backup's own record writes it: after the
	// settlements rather than before them, by rounding or by the other server's clock. Only the
	// hour's margin in the block reaches back over it.
	backup := time.Now().Add(2 * time.Minute)

	// The broker's database is restored to the backup: its record of the replacements is gone and
	// the replaced accounts' sign-ins are live again. A restore writes rows the guards refuse, so
	// they are off for these statements.
	for _, statement := range []string{
		`alter table replaced_person_subjects disable trigger replaced_person_subjects_guarded`,
		`alter table person_replacement_generations disable trigger person_replacement_generations_guarded`,
		`delete from replaced_person_subjects where person in ('` + email + `', '` + takeback + `')`,
		`delete from person_replacement_generations where person in ('` + email + `', '` + takeback + `')`,
		`update person_signins set revoked_at = null where id in ('` + machine + `'::uuid, '` + t2Machine + `'::uuid)`,
		`alter table replaced_person_subjects enable trigger replaced_person_subjects_guarded`,
		`alter table person_replacement_generations enable trigger person_replacement_generations_guarded`,
	} {
		if _, err := r.rig.Store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("restore the broker to the backup, %q: %v", statement, err)
		}
	}

	count, oldest, err := r.revocations.Owed(ctx)
	if err != nil || count != 0 || oldest != 0 {
		t.Fatalf("control: after the broker's restore Dispatch owes %d (oldest %s), %v; want nothing owed, which is why the gauges cannot see it", count, oldest, err)
	}
	pay()
	if r.refused(t, email, "account-a") || !r.machineSignInLive(t, machine) || r.refused(t, takeback, "account-t2") {
		t.Fatalf("control: a pass alone recovered the broker (refuses account-a %t, its sign-in live %t, refuses account-t2 %t); want it to change nothing",
			r.refused(t, email, "account-a"), r.machineSignInLive(t, machine), r.refused(t, takeback, "account-t2"))
	}

	block := runbookBlockAfter(t, brokerRestoredLead)
	runStatements(t, r.dispatch.Pool, block.statements, pgx.NamedArgs{"backup": backup})
	pay()
	for _, ended := range []struct {
		email, account, signIn string
	}{{email, "account-a", machine}, {takeback, "account-t2", t2Machine}} {
		if !r.refused(t, ended.email, ended.account) {
			t.Errorf("after the runbook's recovery of a broker restored alone, the broker does not refuse %s at %s; want it refused again", ended.account, ended.email)
		}
		if r.machineSignInLive(t, ended.signIn) {
			t.Errorf("after the runbook's recovery of a broker restored alone, %s's sign-in on a machine at %s is live; want it ended again", ended.account, ended.email)
		}
	}
	if r.refused(t, takeback, "account-t1") {
		t.Errorf("after the recovery the broker refuses account-t1, which holds %s again; want it to trade", takeback)
	}
	for _, address := range []string{email, takeback} {
		if owing, err := r.revocations.Owing(ctx, address); err != nil || len(owing) != 0 {
			t.Errorf("after the recovery and a pass, %s owes %+v, %v; want each revocation settled again", address, owing, err)
		}
	}
}

// resetBlocksAgain runs the runbook's read of whether a broker backup taken at backup may predate
// the last reset's end and, where it answers `run the reset's blocks again`, the reset's
// credentials and directory blocks again, as the runbook's next paragraph says. It answers the
// read's answer.
func (r *recoveryRig) resetBlocksAgain(t *testing.T, backup any, directory map[string]directoryAnswer) string {
	t.Helper()
	answer := queryRunbook(t, r.dispatch.Pool, runbookBlockAfter(t, repeatResetLead), pgx.NamedArgs{"backup": backup})
	if len(answer) != 1 {
		t.Fatalf("the runbook's read of the last reset answered %q; want one line", answer)
	}
	if answer[0] != "run the reset's blocks again" {
		return answer[0]
	}
	const again = "run **End every person's credential at the broker** and **Clear a refusal only for the directory's holder** again, after the record-again block and before starting Dispatch"
	if paragraph := runbookParagraph(t, "For `run the reset's blocks again`"); !strings.Contains(paragraph, again) {
		t.Fatalf("the runbook's paragraph after the read is %q; want %q", paragraph, again)
	}
	if left := r.endBrokerCredentials(t); len(left) != 0 {
		t.Fatalf("STILL LIVE after the reset's credentials block run again: %v", left)
	}
	if output, err := r.clearHoldersRefusals(t, directory); err != nil {
		t.Fatalf("the reset's directory block run again: %v\n%s", err, output)
	}
	return answer[0]
}

// TestABrokerRestoredBeforeTheResetIsRecoveredByRepeatingTheResetCredentialBlocks covers two
// restores in sequence. Restored Dispatch still records A, because B's replacement was owed when
// Dispatch's database was lost. The reset ends A's broker sign-in, but a broker backup taken before
// the reset brings it back. No revocation names A yet, so record-again has nothing to write. The
// runbook's read of the last reset says the backup predates its end, so the reset's credentials
// and directory blocks run again after record-again: A's sign-in is ended before Dispatch starts,
// then B's sign-in pays the revocation that refuses A. With no reset recorded the read says so.
//
// Control in the runbook: the read answering anything but `run the reset's blocks again` for a
// backup from before the reset leaves A's sign-in live before Dispatch starts.
func TestABrokerRestoredBeforeTheResetIsRecoveredByRepeatingTheResetCredentialBlocks(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "reset-then-broker@example.com"
	directory := map[string]directoryAnswer{email: {User: directoryUser{ID: "b-reset", PrimaryEmail: email}}}
	if answer := r.resetBlocksAgain(t, time.Now(), directory); answer != "no reset ran" {
		t.Errorf("the read of the last reset on a deployment that never reset answered %q; want no reset ran", answer)
	}
	if err := r.people.SignInSubject(ctx, email, "a-reset", "refresh-a", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	signIn := r.signInMachine(t, email, "a-reset", strings.Repeat("z", 43))

	start, _ := r.resetDeployment(t, directory)
	if r.machineSignInLive(t, signIn) {
		t.Fatal("the reset left A's sign-in live")
	}
	backup := start.Add(-time.Minute)

	// Restore the broker to the backup before the reset: A's sign-in the reset ended is live again.
	if _, err := r.rig.Store.Pool.Exec(ctx, `update person_signins set revoked_at = null where id = $1::uuid`, signIn); err != nil {
		t.Fatalf("restore A's broker sign-in from before the reset: %v", err)
	}
	block := runbookBlockAfter(t, brokerRestoredLead)
	runStatements(t, r.dispatch.Pool, block.statements, pgx.NamedArgs{"backup": backup})
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 0 {
		t.Fatalf("the broker-alone record-again with no revocation naming A owes %+v, %v; want nothing", owing, err)
	}
	if answer := r.resetBlocksAgain(t, backup, directory); answer != "run the reset's blocks again" {
		t.Errorf("the read of the last reset for a backup from before it answered %q; want the reset's blocks run again", answer)
	}
	if r.machineSignInLive(t, signIn) {
		t.Errorf("after the broker-alone recovery, A's sign-in is live; want it ended before Dispatch starts")
	}
	if err := r.people.SignInSubject(ctx, email, "b-reset", "refresh-b", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	if r.machineSignInLive(t, signIn) || !r.refused(t, email, "a-reset") {
		t.Errorf("after B signs in and Dispatch pays the revocation, A's sign-in is live %t and the broker refuses A %t; want ended and refused", r.machineSignInLive(t, signIn), r.refused(t, email, "a-reset"))
	}
}

// TestABrokerBackupWrittenWithinTheHourAfterTheResetRepeatsItsBlocks is the history of the TN
// deep review's m1. Restored Dispatch owes the revocation of A, which B replaced and the broker
// never applied, and A holds a sign-in at the broker. The reset settles that revocation unpaid and
// ends A's sign-in. A broker backup taken just before the reset holds the sign-in live, and its
// moment is written late, half an hour after the reset's end, as a backup's end or a rounded
// moment can be. Restored to it, the broker serves A's sign-in again and no revocation names A. The
// read of the last reset takes the same hour's margin as record-again, so it says the backup may
// predate the reset's end, and the credentials block run again ends the sign-in. A backup written
// two hours after the end is read as holding the reset, and asks for nothing.
//
// Control in the runbook: the read without its hour (the backup's moment compared with the end as
// written) answers that the backup holds the reset, and A's sign-in stays live.
func TestABrokerBackupWrittenWithinTheHourAfterTheResetRepeatsItsBlocks(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "late-backup@example.com"
	for _, account := range []string{"a-late", "b-late"} {
		if err := r.people.SignInSubject(ctx, email, account, "refresh-"+account, "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	signIn := r.signInMachine(t, email, "a-late", strings.Repeat("l", 43))
	directory := map[string]directoryAnswer{email: {User: directoryUser{ID: "b-late", PrimaryEmail: email}}}
	_, end := r.resetDeployment(t, directory)
	if r.machineSignInLive(t, signIn) {
		t.Fatal("the reset left A's sign-in live")
	}

	if answer := r.resetBlocksAgain(t, end.Add(2*time.Hour), directory); answer != "the backup holds the last reset" {
		t.Errorf("the read for a backup written two hours after the reset's end answered %q; want the backup holds the last reset", answer)
	}
	backup := end.Add(30 * time.Minute)
	if _, err := r.rig.Store.Pool.Exec(ctx, `update person_signins set revoked_at = null where id = $1::uuid`, signIn); err != nil {
		t.Fatalf("restore A's broker sign-in from just before the reset: %v", err)
	}
	runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": backup})
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 0 {
		t.Errorf("record-again for a backup from just before the reset owes %+v, %v; want nothing, since the reset settled the one revocation", owing, err)
	}
	if answer := r.resetBlocksAgain(t, backup, directory); answer != "run the reset's blocks again" {
		t.Errorf("the read for a backup written half an hour after the reset's end answered %q; want the reset's blocks run again", answer)
	}
	if r.machineSignInLive(t, signIn) {
		t.Error("after the broker-alone recovery, A's sign-in the backup brought back is live; want it ended before Dispatch starts")
	}
}

// TestABrokerBackupTakenDuringTheResetRepeatsItsBlocks: a broker backup taken while the reset runs,
// after its first credentials run and past the close's wait since its start, but before its second
// run, holds a sign-in A traded during the wait with a Google ID token issued before B replaced it.
// The second run ends it and the reset closes. Restored to that backup, the broker serves the
// sign-in again. The read compares the backup with the reset's end, not its start, so it says the
// backup may predate the end, and the credentials block run again ends the sign-in.
//
// Control in the runbook: the read comparing the backup with the last reset's start answers that
// the backup holds the reset, and the traded sign-in stays live.
func TestABrokerBackupTakenDuringTheResetRepeatsItsBlocks(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "during-reset@example.com"
	for _, account := range []string{"a-during", "b-during"} {
		if err := r.people.SignInSubject(ctx, email, account, "refresh-"+account, "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	directory := map[string]directoryAnswer{email: {User: directoryUser{ID: "b-during", PrimaryEmail: email}}}
	r.resetSessions(t)
	if left := r.endBrokerCredentials(t); len(left) != 0 {
		t.Fatalf("STILL LIVE after the reset's first credentials run: %v", left)
	}
	if output, err := r.clearHoldersRefusals(t, directory); err != nil {
		t.Fatalf("the reset's directory block: %v\n%s", err, output)
	}
	traded := r.signInMachine(t, email, "a-during", strings.Repeat("u", 43))
	r.ageReset(t, resetWait+time.Minute)
	backup := time.Now()
	if left := r.endBrokerCredentials(t); len(left) != 0 || r.machineSignInLive(t, traded) {
		t.Fatalf("the reset's second credentials run leaves %v and the traded sign-in live %t; want nothing", left, r.machineSignInLive(t, traded))
	}
	if output, err := r.closeReset(t); err != nil {
		t.Fatalf("the reset's close: %v\n%s", err, output)
	}

	if _, err := r.rig.Store.Pool.Exec(ctx, `update person_signins set revoked_at = null where id = $1::uuid`, traded); err != nil {
		t.Fatalf("restore the sign-in traded during the reset: %v", err)
	}
	runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": backup})
	if answer := r.resetBlocksAgain(t, backup, directory); answer != "run the reset's blocks again" {
		t.Errorf("the read for a backup taken during the reset answered %q; want the reset's blocks run again", answer)
	}
	if r.machineSignInLive(t, traded) {
		t.Error("after the broker-alone recovery, the sign-in traded during the reset is live; want it ended before Dispatch starts")
	}
}

// TestABrokerBackupAfterAnUnclosedResetRepeatsItsBlocks is the same-family review's R27-1 row: a
// reset was started and never closed, so it is still running, and a broker backup of any moment
// may hold what its broker steps ended or have yet to end. The read answers `run the reset's blocks
// again` for a backup taken two hours after the start.
//
// Control in the runbook: an unclosed reset counted as closed long ago (`'-infinity'` for its end)
// answers `the backup holds the last reset`.
func TestABrokerBackupAfterAnUnclosedResetRepeatsItsBlocks(t *testing.T) {
	r := newRecoveryRig(t)
	r.resetSessions(t)
	starts := r.resetMarks(t, "start")
	if len(starts) != 1 || len(r.resetMarks(t, "end")) != 0 {
		t.Fatalf("after step 1 deployment_resets holds starts %v and ends %v; want one start and no end", starts, r.resetMarks(t, "end"))
	}
	if answer := r.resetBlocksAgain(t, starts[0].Add(2*time.Hour), nil); answer != "run the reset's blocks again" {
		t.Errorf("the read for a backup two hours after an unclosed reset's start answered %q; want the reset's blocks run again", answer)
	}
}

// TestABrokerRestoredBeforeTheResetLeavesTheDirectorysHolderCleared: the reset settles a
// revocation restored Dispatch owed without paying it, and its directory block clears the refusal
// of the account the directory names. Here that account, X, took the address back from Y before
// Dispatch's database was lost, so the restored database still records Y and owes "X replaced,
// holder Y". A broker backup from before the reset brings X's refusal back, and the runbook's
// record-again block then runs. Were it to record that settled row again, it would name Y, whom
// the restored `people` row records, as the holder; Dispatch's first pass would refuse X at the
// broker again and end the broker sign-in X made after the reset's directory block cleared it. The
// block records again only what Dispatch settled after the last reset's start, and the reset
// settled that row at its start, so it records that row again not at all, and the reset's blocks,
// run again, decide X's refusal.
//
// Control in the runbook: the block's bound as the backup's moment less an hour alone records the
// row again, and the pass refuses X and ends X's sign-in.
func TestABrokerRestoredBeforeTheResetLeavesTheDirectorysHolderCleared(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "holder-after-reset@example.com"
	// The restored database: X, then Y, and the revocation of X still owed.
	if err := r.people.SignInSubject(ctx, email, "x-owed", "refresh-x", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := r.people.SignInSubject(ctx, email, "y-owed", "refresh-y", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	// The broker, before the reset: it applied that revocation before the loss, so it refuses X.
	r.revoke(t, email, "x-owed", "y-owed", 2)
	directory := map[string]directoryAnswer{email: {User: directoryUser{ID: "x-owed", PrimaryEmail: email}}}
	start, _ := r.resetDeployment(t, directory)
	if r.refused(t, email, "x-owed") {
		t.Fatal("the reset's directory block left X refused; want X cleared")
	}
	backup := start.Add(-time.Minute)

	// The broker is restored to its backup from before the reset: X is refused again.
	r.revoke(t, email, "x-owed", "y-owed", 3)
	runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": backup})
	owing, err := r.revocations.Owing(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if len(owing) != 0 {
		t.Errorf("the broker-alone record-again after a reset owes %+v; want nothing, since the reset settled that revocation", owing)
	}
	if answer := r.resetBlocksAgain(t, backup, directory); answer != "run the reset's blocks again" {
		t.Errorf("the read for a backup from before the reset answered %q; want the reset's blocks run again", answer)
	}

	// X signs in at the broker, then Dispatch starts and its loop runs a pass.
	signIn := r.signInMachine(t, email, "x-owed", strings.Repeat("x", 43))
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	if r.refused(t, email, "x-owed") || !r.machineSignInLive(t, signIn) {
		t.Errorf("after Dispatch's first pass the broker refuses X %t and X's broker sign-in is live %t; want X not refused and its sign-in live", r.refused(t, email, "x-owed"), r.machineSignInLive(t, signIn))
	}
}

// TestAResetsSettledRevocationIsNotRecordedAgainByALaterBrokerRestore holds the same-family
// review's R25-1 histories (its round-25 probe), each through the whole documented procedure: the
// reset, a broker restored alone to a backup from before it, record-again, the read of the last
// reset and its blocks, and Dispatch's first pass.
//
//   - owedrow: Y replaced X, the broker applied it, restored Dispatch still owes it, and the
//     directory names X, who has not signed in to Dispatch since the reset. After the first pass X
//     is not refused and approves.
//   - kept: T replaced S, restored Dispatch owes S's revocation naming T, the broker already
//     refuses T, and the directory names H, so it keeps T's refusal. After the first pass T is
//     still refused.
//
// Control in the runbook: record-again's bound as the backup's moment less an hour alone records
// the reset-settled row again; the pass then refuses X (its approve APPROVER_REPLACED) and lifts
// T's refusal.
func TestAResetsSettledRevocationIsNotRecordedAgainByALaterBrokerRestore(t *testing.T) {
	ctx := context.Background()
	t.Run("owedrow", func(t *testing.T) {
		r := newRecoveryRig(t)
		const owed = "owedrow-restore@example.com"
		for _, account := range []string{"x-owed", "y-owed"} {
			if err := r.people.SignInSubject(ctx, owed, account, "refresh-"+account, "", model.MemberFull); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := r.revocations.Owing(ctx, owed)
		if err != nil || len(rows) != 1 || rows[0].SessionGeneration == nil {
			t.Fatalf("setup: owes %+v, %v; want one revocation with its generation", rows, err)
		}
		generation := *rows[0].SessionGeneration
		r.revoke(t, owed, "x-owed", "y-owed", generation) // the broker applied it before Dispatch's loss
		directory := map[string]directoryAnswer{owed: {User: directoryUser{ID: "x-owed", PrimaryEmail: owed}}}
		start, _ := r.resetDeployment(t, directory)
		if r.refused(t, owed, "x-owed") {
			t.Fatal("setup: after the reset the broker still refuses X")
		}
		backup := start.Add(-time.Minute)
		r.revoke(t, owed, "x-owed", "y-owed", generation) // the restored broker refuses X again
		runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": backup})
		if owing, err := r.revocations.Owing(ctx, owed); err != nil || len(owing) != 0 {
			t.Errorf("record-again after the reset owes %+v, %v; want nothing, since the reset settled the one revocation", owing, err)
		}
		r.resetBlocksAgain(t, backup, directory)
		r.ageClaims(t)
		r.served.revokeOwedBrokerCredentials(ctx)
		current, _, err := store.NewPgSessionStore(r.dispatch.Pool).CurrentSessionGeneration(ctx, owed)
		if err != nil {
			t.Fatal(err)
		}
		if r.refused(t, owed, "x-owed") {
			t.Error("after Dispatch's first pass the broker refuses X, the directory's holder; want X not refused")
		}
		if err := r.approveAs(t, owed, "x-owed", current); err != nil {
			t.Errorf("after Dispatch's first pass X's approve answered %v; want it approved", err)
		}
	})
	t.Run("kept", func(t *testing.T) {
		r := newRecoveryRig(t)
		const kept = "kept-restore@example.com"
		for _, account := range []string{"s-kept", "t-kept"} {
			if err := r.people.SignInSubject(ctx, kept, account, "refresh-"+account, "", model.MemberFull); err != nil {
				t.Fatal(err)
			}
		}
		r.revoke(t, kept, "t-kept", "u-kept", 1)
		directory := map[string]directoryAnswer{kept: {User: directoryUser{ID: "h-kept", PrimaryEmail: kept}}}
		start, _ := r.resetDeployment(t, directory)
		if !r.refused(t, kept, "t-kept") {
			t.Fatal("setup: after the reset the broker does not refuse T")
		}
		backup := start.Add(-time.Minute)
		runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": backup})
		if owing, err := r.revocations.Owing(ctx, kept); err != nil || len(owing) != 0 {
			t.Errorf("record-again after the reset owes %+v, %v; want nothing, since the reset settled the one revocation", owing, err)
		}
		r.resetBlocksAgain(t, backup, directory)
		r.ageClaims(t)
		r.served.revokeOwedBrokerCredentials(ctx)
		if !r.refused(t, kept, "t-kept") {
			t.Error("after Dispatch's first pass the broker no longer refuses T, whose refusal the directory kept; want T refused")
		}
	})
}

// TestABrokerDatabaseMadeAgainIsToldEveryRevocationSettled: a broker database made again, rather
// than restored, has forgotten every refusal and every replacement, and has no backup moment to
// give the block. The runbook says to give it `-infinity`, which records again every revocation
// Dispatch ever settled, whenever it was; the loop then tells the broker each, and the accounts
// those revocations named are refused again.
//
// Control, in the test: the same empty broker with a restore's own kind of moment, now(), records
// nothing again, since every settlement here is older.
func TestABrokerDatabaseMadeAgainIsToldEveryRevocationSettled(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "madeagain@example.com"
	if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	machine := r.signInMachine(t, email, "account-a", strings.Repeat("m", 43))
	if err := r.people.SignInSubject(ctx, email, "account-b", "refresh-b", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	r.served.revokeOwedBrokerCredentials(ctx)
	if !r.refused(t, email, "account-a") {
		t.Fatalf("after the pass the broker does not refuse account-a at %s", email)
	}
	// The broker's database is made again: every row of its record of accounts is gone, and the
	// credential it held is back. A database made again is empty, so the guards are off for the
	// statements that stand for it here.
	for _, statement := range []string{
		`alter table replaced_person_subjects disable trigger replaced_person_subjects_guarded`,
		`alter table person_replacement_generations disable trigger person_replacement_generations_guarded`,
		`delete from replaced_person_subjects`,
		`delete from person_replacement_generations`,
		`update person_signins set revoked_at = null where id = '` + machine + `'::uuid`,
		`alter table replaced_person_subjects enable trigger replaced_person_subjects_guarded`,
		`alter table person_replacement_generations enable trigger person_replacement_generations_guarded`,
	} {
		if _, err := r.rig.Store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("make the broker's database again, %q: %v", statement, err)
		}
	}
	// The settlement is three days older than the moment a restore would be given, so that
	// moment records nothing again: this is the case the page sends to -infinity. The guard
	// leaves done_at alone on a settled row only while it does not move, so it is off for this.
	for _, statement := range []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set done_at = now() - interval '3 days' where email = '` + email + `' and done_at is not null`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	} {
		if _, err := r.dispatch.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	block := runbookBlockAfter(t, brokerRestoredLead)
	runStatements(t, r.dispatch.Pool, block.statements, pgx.NamedArgs{"backup": time.Now()})
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 0 {
		t.Fatalf("control: with a restore's own moment the block recorded %+v again, %v; want nothing, which is why a database made again is given -infinity", owing, err)
	}
	if paragraph := runbookParagraph(t, madeAgainLead); !strings.Contains(paragraph, "Give the block `-infinity`") {
		t.Fatalf("the runbook's paragraph for a broker database made again reads %q; want it to give -infinity for the backup's moment", paragraph)
	}
	runStatements(t, r.dispatch.Pool, block.statements, pgx.NamedArgs{"backup": "-infinity"})
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	if !r.refused(t, email, "account-a") {
		t.Errorf("after the block with -infinity and a pass, the broker does not refuse account-a at %s; want every settled revocation told again", email)
	}
	if r.machineSignInLive(t, machine) {
		t.Errorf("after the block with -infinity and a pass, account-a's sign-in on a machine at %s is live; want it ended again", email)
	}
}

// TestTheBrokerRestoredBlockHoldsEachAddressesSessionRow: the block reads each address's session
// generation in a subquery and the guard on broker_revocations reads it again, in a VOLATILE
// plpgsql function with a snapshot of its own. A sign-out that commits between the two moves the
// generation, and the guard refuses the whole statement with 23514, so one sign-out anywhere in
// the set would throw away the recovery of every address in it. The block locks each of those
// session rows first (select ... for update), as the single-row blocks do, so such a sign-out
// waits for the commit instead.
//
// Control: drop the `for update` from the runbook's lock statement, and the sign-out no longer
// waits.
func TestTheBrokerRestoredBlockHoldsEachAddressesSessionRow(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "lockedrows@example.com"
	if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := r.people.SignInSubject(ctx, email, "account-b", "refresh-b", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	r.served.revokeOwedBrokerCredentials(ctx)
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 0 {
		t.Fatalf("after the pass, %s owes %+v, %v; want the revocation settled", email, owing, err)
	}
	block := runbookBlockAfter(t, brokerRestoredLead)
	if len(block.statements) != 5 || block.statements[0] != "begin" || block.statements[4] != "commit" ||
		!strings.HasPrefix(block.statements[1], "with locked as (select r.email, pg_advisory_xact_lock(") ||
		!strings.Contains(block.statements[1], "\nupdate machine_login_revocations m ") ||
		!strings.HasPrefix(block.statements[2], "select u.generation from user_sessions u ") ||
		!strings.HasPrefix(block.statements[3], "update broker_revocations ") {
		t.Fatalf("the runbook's block for a broker restored alone is %q; want begin, one statement that takes the members' machine-login approval locks and owes again the revocations it locked, a read of the addresses' session rows, the record-again, and commit", block.statements)
	}
	tx, err := r.dispatch.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	args := pgx.NamedArgs{"backup": time.Now().Add(-time.Hour)}
	for _, statement := range block.statements[1:3] {
		if _, err := tx.Exec(ctx, statement, args); err != nil {
			t.Fatalf("the block's approval locks and lock of the addresses' session rows: %v", err)
		}
	}
	// A sign-out at one of those addresses, from another connection: it must wait for the commit.
	signedOut := make(chan error, 1)
	go func() {
		signedOut <- store.NewPgSessionStore(r.dispatch.Pool).RevokeSessions(context.Background(), email)
	}()
	select {
	case err := <-signedOut:
		t.Fatalf("the sign-out at %s finished (%v) while the block held its session row; want it waiting for the commit", email, err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, block.statements[3], args); err != nil {
		t.Fatalf("the block's record-again: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the block's commit: %v", err)
	}
	select {
	case err := <-signedOut:
		if err != nil {
			t.Errorf("the sign-out at %s after the commit: %v; want it to go through", email, err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("the sign-out at %s did not finish within 10 s of the commit", email)
	}
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 1 {
		t.Errorf("after the block, %s owes %+v, %v; want the revocation recorded again", email, owing, err)
	}
}

// TestTheBrokerRestoredBlockTakesTheApprovalLockBeforeTheSessionRow: an owner's Reset or removal of
// a restricted member holds the member's machine-login approval lock (access.LockForAccessWrite,
// where access.RemovalOwes reads true) when ending their account writes their user_sessions row
// (people_account_ends_sessions). The block takes the approval locks first and the session rows
// after, the same order, so while such a write holds the approval lock, the block waits for it
// holding no session row: the write's session row is granted, it commits, and the block then
// commits too.
//
// Control: run the block's session-row read before its approval locks (the order before this
// change), and the block holds the row while it waits for the approval lock the write holds, the
// write waits for the row, and PostgreSQL fails one of them (40P01), and the row fails.
func TestTheBrokerRestoredBlockTakesTheApprovalLockBeforeTheSessionRow(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "member@other.example"
	if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if err := r.people.SignInSubject(ctx, email, "account-b", "refresh-b", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	r.served.revokeOwedBrokerCredentials(ctx)
	storetest.RecordMachineLoginRevocation(t, r.dispatch, email)
	machineLogins := store.NewMachineLoginRevocations(r.dispatch.Pool)
	claimed, err := machineLogins.Claim(ctx, time.Minute, 5)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %+v, %v; want the member's machine-login revocation", claimed, err)
	}
	if settled, err := machineLogins.Settle(ctx, claimed[0], 0); err != nil || !settled {
		t.Fatalf("Settle = %t, %v; want it settled", settled, err)
	}
	block := runbookBlockAfter(t, brokerRestoredLead)
	args := pgx.NamedArgs{"backup": time.Now().Add(-time.Hour)}

	// The owner's write: the member's approval lock first, as LockForAccessWrite takes it.
	write, err := r.dispatch.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback(ctx)
	if err := store.LockMachineLoginApprovals(ctx, write, email); err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- runBlockIn(r.dispatch.Pool, block.statements, args) }()
	testwait.Eventually(t, "the block to wait on a lock", func() bool {
		var waiting int
		if err := r.dispatch.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting > 0
	})
	// Ending the member's account writes their session row, as people_account_ends_sessions does.
	if tag, err := write.Exec(ctx, `update user_sessions set generation = generation + 1 where login = $1`, email); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("the owner's write of the member's session row while the block waits: %d rows, %v; want the row granted, the block holding no session row while it waits for the approval lock", tag.RowsAffected(), err)
	}
	if err := write.Commit(ctx); err != nil {
		t.Fatalf("commit the owner's write: %v", err)
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("the block after the owner's write committed: %v; want it to commit", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the block did not finish after the owner's commit")
	}
}

// runBlockIn runs a runbook block's statements, begin and commit included, on one connection of
// pool, and answers the first error.
func runBlockIn(pool *store.Pool, statements []string, args pgx.NamedArgs) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, statement := range statements[1 : len(statements)-1] {
		if _, err := tx.Exec(ctx, statement, args); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// TestABrokerRestoredAloneOwesAgainTheMachineLoginRevocationsSettledSince: the block for a broker
// restored alone owes again each revocation of a restricted member's machine logins that Dispatch
// settled at or after the backup's moment less an hour, at its next generation and with its
// recorded_at set to the block's own moment, so the pass again revokes every machine login the
// member holds and settles it only once a claim comes machineLoginSettleAfter after the block. One
// settled before then, which the restored broker's backup already holds revoked, stays settled.
//
// Control: drop the block's machine_login_revocations statement, and the revocation settled since
// the backup stays settled, named.
func TestABrokerRestoredAloneOwesAgainTheMachineLoginRevocationsSettledSince(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	revocations := store.NewMachineLoginRevocations(database.Pool)
	const since, before = "since@other.example", "before@other.example"
	for _, email := range []string{since, before} {
		storetest.RecordMachineLoginRevocation(t, database, email)
	}
	claimed, err := revocations.Claim(ctx, time.Minute, 5)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("Claim = %+v, %v; want both revocations", claimed, err)
	}
	generations := map[string]int64{}
	for _, owed := range claimed {
		if settled, err := revocations.Settle(ctx, owed, 0); err != nil || !settled {
			t.Fatalf("Settle(%s) = %t, %v; want it settled", owed.Email, settled, err)
		}
		generations[owed.Email] = owed.Generation
	}
	// The one settled three hours ago is older than the backup's moment less its hour.
	if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set done_at = now() - interval '3 hours' where email = $1`, before); err != nil {
		t.Fatal(err)
	}
	var blockStart time.Time
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&blockStart); err != nil {
		t.Fatal(err)
	}

	runStatements(t, database.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": time.Now().Add(-30 * time.Minute)})

	if owed, generation, err := revocations.Owing(ctx, since); err != nil || !owed || generation != generations[since]+1 {
		t.Errorf("the revocation settled since the backup is owed %t at generation %d (%v) after the block, want owed at %d", owed, generation, err, generations[since]+1)
	}
	var after time.Time
	if err := database.Pool.QueryRow(ctx, `select recorded_at from machine_login_revocations where email = $1`, since).Scan(&after); err != nil || after.Before(blockStart) {
		t.Errorf("the revocation owed again records %s (%v), want the block's own moment, at or after %s", after, err, blockStart)
	}
	if owed, _, err := revocations.Owing(ctx, before); err != nil || owed {
		t.Errorf("the revocation settled before the backup's hour is owed %t (%v) after the block, want it settled still", owed, err)
	}
}
