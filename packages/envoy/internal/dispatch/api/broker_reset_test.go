package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The leads of the runbook's reset after a Dispatch database restored or made again alone
// ("After a Dispatch database restored or made again alone").
const (
	resetSessionsLead    = "**End every browser session and revoke every personal token.**"
	resetCredentialsLead = "**End every person's credential at the broker.**"
	resetDirectoryLead   = "**Clear a refusal only for the directory's holder.**"
)

// brokerList is a shell function of the runbook's credentials block: a read of the broker's
// database that lists, by id and person or by login, one kind of person credential still live.
var brokerList = regexp.MustCompile(`^(signins|logins|pages)\(\) \{ psql "\$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At (?:-F ' ' )?-c "([^"]+)"; \}$`)

// resetSessions runs the reset's first block from the runbook: its read of the broker's greatest
// recorded generation on the broker's database, then its one transaction on Dispatch's database.
// It answers the number every session was raised to.
func (r *recoveryRig) resetSessions(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	block := runbookBlockAfter(t, resetSessionsLead)
	if len(block.statements) < 4 || !strings.HasPrefix(block.statements[0], "select coalesce(max(session_generation), 0) from person_replacement_generations") {
		t.Fatalf("the runbook's reset of sessions is %q; want the broker's read, then one transaction on Dispatch's database", block.statements)
	}
	var greatest int64
	if err := r.rig.Store.Pool.QueryRow(ctx, block.statements[0]).Scan(&greatest); err != nil {
		t.Fatalf("the reset's read of the broker's greatest generation: %v", err)
	}
	runStatements(t, r.dispatch.Pool, block.statements[1:], pgx.NamedArgs{"broker_greatest": greatest})
	var raised int64
	if err := r.dispatch.Pool.QueryRow(ctx, `select generation from session_generation_floors where login = '*'`).Scan(&raised); err != nil {
		t.Fatalf("the deployment's floor after the reset: %v", err)
	}
	return raised
}

// credentialsBlock is the reset's second block of the runbook: its lines, and the three reads of
// the broker's database it defines (signins, logins and pages), each a list of one kind of person
// credential still live, by id and person or by login.
type credentialsBlock struct {
	text  string
	lists map[string]string
}

// readCredentialsBlock reads that block from the runbook.
func readCredentialsBlock(t *testing.T) credentialsBlock {
	t.Helper()
	block := runbookBlockAfter(t, resetCredentialsLead)
	lists := map[string]string{}
	for _, command := range block.commands {
		if match := brokerList.FindStringSubmatch(command); match != nil {
			lists[match[1]] = match[2]
		}
	}
	if len(lists) != 3 {
		t.Fatalf("the runbook's credentials block defines the lists %v; want signins, logins and pages", slices.Sorted(maps.Keys(lists)))
	}
	text := strings.Join(block.lines, "\n")
	if !strings.Contains(text, `if left=$( { signins && logins && pages; } ); then`) {
		t.Fatalf("the runbook's credentials block ends with no read of all three lists; it reads:\n%s", text)
	}
	return credentialsBlock{text: text, lists: lists}
}

// rows runs the block's read name on the broker's database and answers each row's columns as
// psql -At prints them.
func (c credentialsBlock) rows(t *testing.T, r *recoveryRig, name string) [][]string {
	t.Helper()
	found, err := r.rig.Store.Pool.Query(context.Background(), c.lists[name])
	if err != nil {
		t.Fatalf("the credentials block's %s(): %v", name, err)
	}
	values, err := pgx.CollectRows(found, func(row pgx.CollectableRow) ([]string, error) {
		columns, err := row.Values()
		if err != nil {
			return nil, err
		}
		var texts []string
		for _, column := range columns {
			texts = append(texts, toText(column))
		}
		return texts, nil
	})
	if err != nil {
		t.Fatalf("the credentials block's %s(): %v", name, err)
	}
	return values
}

// live is what the block's last read lists: every person credential still live at the broker,
// which is empty when the block printed `no person credential is live at the broker`.
func (c credentialsBlock) live(t *testing.T, r *recoveryRig) []string {
	t.Helper()
	var left []string
	for _, name := range []string{"signins", "logins", "pages"} {
		for _, row := range c.rows(t, r, name) {
			left = append(left, name+": "+strings.Join(row, " "))
		}
	}
	return left
}

// endBrokerCredentials runs the reset's second block from the runbook: for each loop the block
// holds, the call it makes for every row of that loop's read - the broker's own routes, as
// Dispatch's ServiceAccount calls them, and, for a login with a page session, the store call
// `envoy-broker end-sessions` makes (store.DeleteBrowserSessionsForLogin). It answers what the
// block's last read lists still live (STILL LIVE), empty when every person credential at the
// broker is ended.
func (r *recoveryRig) endBrokerCredentials(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	block := readCredentialsBlock(t)
	if strings.Contains(block.text, `"$broker_url/v1/person-signins/$id/revoke"`) {
		for _, row := range block.rows(t, r, "signins") {
			if status, body := r.rig.UI(t, "POST", "/v1/person-signins/"+row[0]+"/revoke", map[string]any{"person": row[1]}); status != 200 {
				t.Fatalf("NOT REVOKED %s: %d %s", row[0], status, body)
			}
		}
	}
	if strings.Contains(block.text, `"$broker_url/v1/launcher-credentials/$id/revoke-by-approver"`) {
		for _, row := range block.rows(t, r, "logins") {
			if status, body := r.rig.UI(t, "POST", "/v1/launcher-credentials/"+row[0]+"/revoke-by-approver", map[string]any{"approver": row[1]}); status != 200 {
				t.Fatalf("NOT REVOKED %s: %d %s", row[0], status, body)
			}
		}
	}
	if strings.Contains(block.text, `envoy-broker end-sessions "$login"`) {
		for _, row := range block.rows(t, r, "pages") {
			if _, err := r.rig.Store.DeleteBrowserSessionsForLogin(ctx, row[0]); err != nil {
				t.Fatalf("NOT ENDED %s: %v", row[0], err)
			}
		}
	}
	return block.live(t, r)
}

// toText is a column of a list's row as psql -At prints it: a uuid in its hyphened form, any other
// value as fmt prints it.
func toText(value any) string {
	if id, ok := value.([16]byte); ok {
		return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
	}
	return fmt.Sprint(value)
}

// directoryUser is what the Workspace directory answers users.get with, of the fields the
// runbook's block reads.
type directoryUser struct {
	ID           string `json:"id"`
	PrimaryEmail string `json:"primaryEmail"`
}

// directoryAnswer is one response of the directory stand-in. A zero Status is 200, and a missing
// address is 404 as users.get answers an account the organization does not hold.
type directoryAnswer struct {
	User   directoryUser
	Status int
	// Body, when set, is the answer's bytes in place of User's JSON, for an answer that does not
	// parse.
	Body string
}

// clearHoldersRefusals runs the reset's third block itself, through bash, psql and jq, against the
// broker's database: the runbook's step that makes the libpq URL, then the block. A stand-in for
// curl answers each address from directory. It returns the block's output and its status so tests
// hold both a 404 that leaves a refusal and a non-404 failure that makes the block fail. env, each
// NAME=value, goes after the block's own environment, so a test can give it another database.
func (r *recoveryRig) clearHoldersRefusals(t *testing.T, directory map[string]directoryAnswer, env ...string) (string, error) {
	t.Helper()
	for _, tool := range []string{"bash", "psql", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed, so the reset's directory block cannot be run here: %v", tool, err)
		}
	}
	dir := t.TempDir()
	answers := filepath.Join(dir, "directory")
	statuses := filepath.Join(dir, "status")
	if err := os.Mkdir(answers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statuses, 0o700); err != nil {
		t.Fatal(err)
	}
	for address, answer := range directory {
		raw := []byte(answer.Body)
		if answer.Body == "" {
			encoded, err := json.Marshal(answer.User)
			if err != nil {
				t.Fatal(err)
			}
			raw = encoded
		}
		if err := os.WriteFile(filepath.Join(answers, address), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		status := answer.Status
		if status == 0 {
			status = 200
		}
		if err := os.WriteFile(filepath.Join(statuses, address), []byte(strconv.Itoa(status)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in for users.get writes its JSON body and, when curl asked for it, the status
	// trailer curl's -w '%{http_code}' prints. A missing address is a 404 and curl exits 22 for
	// every non-200, as --fail-with-body does.
	curl := `#!/bin/sh
write=
for arg; do
  case "$arg" in *'%{http_code}'*) write=1;; esac
  url=$arg
done
address=${url#*/users/}
address=${address%%\?*}
status=404
body='{"error":{"code":404,"message":"Resource Not Found: userKey"}}'
if [ -f "$DIRECTORY_ANSWERS/$address" ]; then
  body=$(cat "$DIRECTORY_ANSWERS/$address")
  status=$(cat "$DIRECTORY_STATUSES/$address")
fi
printf '%s' "$body"
[ -z "$write" ] || printf '\n%s' "$status"
[ "$status" = 200 ] && exit 0
exit 22
`
	if err := os.WriteFile(filepath.Join(stubs, "curl"), []byte(curl), 0o700); err != nil {
		t.Fatal(err)
	}
	block := runbookBlockAfter(t, resetDirectoryLead)
	urlStep := runbookBlockAfter(t, brokerURLLead)
	script := filepath.Join(dir, "directory.sh")
	text := strings.Join(urlStep.lines, "\n") + "\n" + strings.Join(block.lines, "\n") + "\n"
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", script)
	command.Env = append(append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BROKER_DATABASE_URL="+psqlURL(t, r.rig.Store.Pool),
		"DIRECTORY_TOKEN=not-a-secret",
		"DIRECTORY_ANSWERS="+answers,
		"DIRECTORY_STATUSES="+statuses), env...)
	output, err := command.CombinedOutput()
	return string(output), err
}

// approveAs asks the broker to approve a new machine login of email, naming account as the
// approving Google account and generation as the session generation Dispatch's check read, as
// Dispatch's approve relay does, and answers the broker's error, nil when it approved.
func (r *recoveryRig) approveAs(t *testing.T, email, account string, generation int64) error {
	t.Helper()
	ctx := context.Background()
	client := brokertest.NewClient(t, r.rig.URL, strings.TrimPrefix(r.rig.URL, "http://"))
	client.UIToken, client.DPoP = r.rig.UIToken, brokerclient.NewDPoPTransport
	authorization := client.DeviceLogin(t, brokertest.NewSigningKey(t), email, "reset-box-"+account)
	lookedUp, err := r.broker.MachineLookup(ctx, json.RawMessage(`{"code":`+strconv.Quote(authorization.UserCode)+`}`))
	if err != nil {
		t.Fatalf("look up %s's machine login by its code: %v", email, err)
	}
	var record struct {
		RecordID string `json:"record_id"`
	}
	if err := json.Unmarshal(lookedUp, &record); err != nil || record.RecordID == "" {
		t.Fatalf("the lookup answered %s (%v), want a record", lookedUp, err)
	}
	code := authorization.UserCode
	_, err = r.broker.Approve(ctx, record.RecordID, agentsecrets.Decision{
		Approver: email, Code: &code, ApproverIssuer: recoveryIssuer, ApproverSubject: account, ApproverSessionGeneration: &generation,
	})
	return err
}

// pageSession gives account a live session on the broker's approval page under email, as the
// page's callback writes one.
func (r *recoveryRig) pageSession(t *testing.T, email, account string) {
	t.Helper()
	hash := sha256.Sum256([]byte(email + "/" + account))
	if _, err := r.rig.Store.Pool.Exec(context.Background(), `insert into browser_sessions (secret_hash, login, expires_at, issuer, subject)
		values ($1, $2, now() + interval '1 hour', $3, $4)`, hash[:], email, recoveryIssuer, account); err != nil {
		t.Fatalf("the page session of %s at %s: %v", account, email, err)
	}
}

// personalToken stores a live personal token of email recording account, as the mint does, and
// answers its id.
func (r *recoveryRig) personalToken(t *testing.T, email, account string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(email + "/" + account + "/token"))
	var id string
	if err := r.dispatch.Pool.QueryRow(context.Background(), `insert into agent_tokens (owner, name, token_hash, prefix, scopes, subject)
		values ($1, 'restored', $2, $3, $4, $5) returning id::text`,
		email, hash[:], strings.Repeat("r", agentTokenPrefixLength), []string{model.ScopePerson}, account).Scan(&id); err != nil {
		t.Fatalf("the personal token of %s at %s: %v", account, email, err)
	}
	return id
}

// TestTheResetEndsEveryCredentialAndClearsOnlyTheHoldersRefusal runs the runbook's reset after a
// Dispatch database restored or made again alone, every step from the runbook, against Dispatch's
// database and a real broker's (brokertest.NewRig), over the three histories round 21's review
// found the per-address recovery missed, and two the directory does not answer for:
//
//   - unapplied@ (F1): A held the address at the backup; after it, B took it, and the revocation of
//     A was still owed when the database was lost, so the broker never refused A. Restored,
//     Dispatch records A, A's personal token is live, and A holds a sign-in on a machine, a machine
//     login and an approval-page session at the broker. The directory names B.
//   - noaccount@ (F2): the database was made again, so Dispatch has no row; the broker refuses X.
//     The directory names H.
//   - takeback@ (F3): the take-back the lost database still owed leaves the broker refusing H,
//     Google's holder, and N, an account that held the address once; Dispatch, restored, records
//     the stale S. The directory names H.
//   - owedrow@: the restored database still owes X's revoke though the broker already applied it;
//     X took the address back after the backup, and the directory names X. The reset settles its
//     restored debt before clearing X, so the first pass after the reset cannot refuse X again.
//   - alias@: the directory answers users.get of the address with another user, whose
//     primaryEmail is not the address (users.get answers an alias), and that user's id is an
//     account the broker refuses here.
//   - outside@: the directory answers 404, as it does for someone from outside the domain and a
//     deleted user; the broker refuses O.
//
// After the reset's first block every session is raised, every personal token revoked and every
// restored debt settled (the store test holds the cookies), after its second no person credential is
// live at the broker and its read says so, and after its third the broker no longer refuses the
// directory's holder at takeback@ or owedrow@ and refuses every other account it refused. Then each
// holder signs in to Dispatch: the sign-in replaces the restored account at the raised generation,
// the loop pays that revocation, and the broker refuses A and S. The directory's owedrow@ holder
// can make a broker sign-in and approve before Dispatch starts and stays live after its first pass.
// Each holder's approve is approved; an approve by A, S, X, N and the alias's account is refused
// APPROVER_REPLACED.
//
// Controls, each a break of the runbook restored by checksum: the credentials block without its
// call that revokes a sign-in (STILL LIVE names it); the directory block without its primaryEmail
// comparison (the alias's account is cleared and approves); and the first block without its
// settlement (the owedrow@ holder is refused again and its sign-in ends).
func TestTheResetEndsEveryCredentialAndClearsOnlyTheHoldersRefusal(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const unapplied, noAccount, takeback, owed, kept, alias, outside = "unapplied@example.com", "noaccount@example.com", "takeback@example.com", "owedrow@example.com", "kept@example.com", "alias@example.com", "outside@example.com"

	// unapplied@: Dispatch, restored, records A; A's credentials at the broker are live.
	if err := r.people.SignInSubject(ctx, unapplied, "a-unapplied", "refresh-a", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	token := r.personalToken(t, unapplied, "a-unapplied")
	signIn := r.signInMachine(t, unapplied, "a-unapplied", strings.Repeat("u", 43))
	r.pageSession(t, unapplied, "a-unapplied")
	loginClient := brokertest.NewClient(t, r.rig.URL, strings.TrimPrefix(r.rig.URL, "http://"))
	loginClient.UIToken, loginClient.DPoP = r.rig.UIToken, brokerclient.NewDPoPTransport
	machine := loginClient.DeviceLogin(t, brokertest.NewSigningKey(t), unapplied, "unapplied-box")
	machineLogin := loginClient.ApproveMachineLogin(t, unapplied, machine.UserCode)

	// noaccount@: no Dispatch row; the broker refuses X.
	r.revoke(t, noAccount, "x-noaccount", "h-noaccount", 3)
	// takeback@: Dispatch records the stale S; the broker refuses H and N.
	if err := r.people.SignInSubject(ctx, takeback, "s-takeback", "refresh-s", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	r.revoke(t, takeback, "h-takeback", "s-takeback", 4)
	r.revoke(t, takeback, "n-takeback", "s-takeback", 5)
	// owedrow@: the broker applied X's revoke before the backup, but restored Dispatch still owes
	// it. X then took the address back without that history reaching the broker, so the directory
	// clears X. The first block must settle the restored row before Dispatch starts again.
	if err := r.people.SignInSubject(ctx, owed, "x-owed", "refresh-x", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := r.people.SignInSubject(ctx, owed, "y-owed", "refresh-y", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	owedRows, err := r.revocations.Owing(ctx, owed)
	if err != nil || len(owedRows) != 1 || owedRows[0].SessionGeneration == nil {
		t.Fatalf("the restored database owes %+v at %s, %v; want X's one revocation carrying a generation", owedRows, owed, err)
	}
	r.revoke(t, owed, owedRows[0].Subject, owedRows[0].HolderSubject, *owedRows[0].SessionGeneration)
	// kept@: broker T is refused already, while restored Dispatch still owes S's revoke naming T.
	// The directory names H, so it keeps T's refusal. A stale first pass would apply S's revoke
	// and delete T's refusal; the reset's settlement makes that impossible.
	if err := r.people.SignInSubject(ctx, kept, "s-kept", "refresh-s", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := r.people.SignInSubject(ctx, kept, "t-kept", "refresh-t", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	keptRows, err := r.revocations.Owing(ctx, kept)
	if err != nil || len(keptRows) != 1 || keptRows[0].SessionGeneration == nil {
		t.Fatalf("the restored database owes %+v at %s, %v; want S's one revocation carrying a generation", keptRows, kept, err)
	}
	r.revoke(t, kept, "t-kept", "u-kept", 1)
	// alias@ and outside@: refusals the directory gives no holder for.
	r.revoke(t, alias, "n-alias", "h-alias", 2)
	r.revoke(t, outside, "o-outside", "h-outside", 2)
	directory := map[string]directoryAnswer{
		unapplied: {User: directoryUser{ID: "b-unapplied", PrimaryEmail: unapplied}},
		noAccount: {User: directoryUser{ID: "h-noaccount", PrimaryEmail: noAccount}},
		takeback:  {User: directoryUser{ID: "h-takeback", PrimaryEmail: strings.ToUpper(takeback)}},
		owed:      {User: directoryUser{ID: "x-owed", PrimaryEmail: owed}},
		kept:      {User: directoryUser{ID: "h-kept", PrimaryEmail: kept}},
		alias:     {User: directoryUser{ID: "n-alias", PrimaryEmail: "someone-else@example.com"}},
	}
	if live := readCredentialsBlock(t).live(t, r); len(live) == 0 {
		t.Fatal("control: before the reset no person credential is live at the broker; the test would show nothing")
	}

	raised := r.resetSessions(t)
	if reset := strings.Join(runbookBlockAfter(t, resetSessionsLead).lines, "\n"); !strings.Contains(reset, "lock table session_generation_floors, user_sessions, agent_tokens, broker_revocations, deployment_resets in exclusive mode") {
		t.Errorf("the reset's transaction does not lock broker_revocations and deployment_resets beside its other rows:\n%s", reset)
	}
	var revoked bool
	if err := r.dispatch.Pool.QueryRow(ctx, `select revoked_at is not null from agent_tokens where id = $1::uuid`, token).Scan(&revoked); err != nil || !revoked {
		t.Errorf("after the reset A's personal token at %s is revoked %t, %v; want revoked", unapplied, revoked, err)
	}
	if owing, err := r.revocations.Owing(ctx, owed); err != nil || len(owing) != 0 {
		t.Errorf("after the reset's first block %s owes %+v, %v; want its restored revocation settled before the directory clears its holder", owed, owing, err)
	}
	if owing, err := r.revocations.Owing(ctx, kept); err != nil || len(owing) != 0 {
		t.Errorf("after the reset's first block %s owes %+v, %v; want its restored revocation settled before the directory keeps T's refusal", kept, owing, err)
	}
	if left := r.endBrokerCredentials(t); len(left) != 0 {
		t.Errorf("STILL LIVE after the reset's credentials block:\n%s", strings.Join(left, "\n"))
	}
	if r.machineSignInLive(t, signIn) {
		t.Errorf("after the reset A's sign-in on a machine at %s is live", unapplied)
	}
	var loginLive bool
	if err := r.rig.Store.Pool.QueryRow(ctx, `select revoked_at is null from launcher_credentials where id = $1::uuid`, machineLogin).Scan(&loginLive); err != nil || loginLive {
		t.Errorf("after the reset A's machine login at %s is live %t, %v; want it revoked", unapplied, loginLive, err)
	}
	var pageSessionsLive int
	if err := r.rig.Store.Pool.QueryRow(ctx, `select count(*) from browser_sessions where login = $1 and expires_at > now()`, unapplied).Scan(&pageSessionsLive); err != nil || pageSessionsLive != 0 {
		t.Errorf("after the reset A's approval-page sessions at %s are live %d (%v); want none", unapplied, pageSessionsLive, err)
	}
	output, err := r.clearHoldersRefusals(t, directory)
	if err != nil {
		t.Fatalf("the reset's directory block: %v\n%s", err, output)
	}
	for _, want := range []string{"HOLDER " + takeback + " h-takeback", "HOLDER " + owed + " x-owed", "HOLDER " + kept + " h-kept", "NO ACCOUNT " + alias, "NO ACCOUNT " + outside} {
		if !strings.Contains(output, want) {
			t.Errorf("the directory block printed:\n%s\nwant a line %q", output, want)
		}
	}
	for _, refusal := range []struct {
		email, account string
		want           bool
	}{
		{noAccount, "x-noaccount", true},
		{takeback, "h-takeback", false},
		{takeback, "n-takeback", true},
		{owed, "x-owed", false},
		{kept, "t-kept", true},
		{alias, "n-alias", true},
		{outside, "o-outside", true},
	} {
		if got := r.refused(t, refusal.email, refusal.account); got != refusal.want {
			t.Errorf("after the directory block the broker refuses %s at %s %t, want %t", refusal.account, refusal.email, got, refusal.want)
		}
	}
	// X can sign in and approve at the broker after the directory clears X and before Dispatch
	// starts. A restored row the first block did not settle would re-refuse X and end this sign-in.
	owedSignIn := r.signInMachine(t, owed, "x-owed", strings.Repeat("o", 43))
	owedGeneration, found, err := store.NewPgSessionStore(r.dispatch.Pool).CurrentSessionGeneration(ctx, owed)
	if err != nil || !found || owedGeneration < raised {
		t.Fatalf("%s's session generation after the reset is %d (found %t, %v); want at least the reset's %d", owed, owedGeneration, found, err, raised)
	}
	if err := r.approveAs(t, owed, "x-owed", owedGeneration); err != nil {
		t.Errorf("the directory's owedrow@ holder X approves after the reset: %v; want it approved", err)
	}
	// This is Dispatch's first pass, before anyone signs in again. A restored owed S→T row that
	// step 1 left owed would apply here and lift T's directory-kept refusal.
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	if !r.refused(t, kept, "t-kept") {
		t.Errorf("after the first pass, the broker does not refuse T at %s; want the directory-kept refusal unchanged", kept)
	}

	// Dispatch is started again, and each holder signs in only after that first pass.
	for _, holder := range []struct{ email, account string }{
		{unapplied, "b-unapplied"}, {noAccount, "h-noaccount"}, {takeback, "h-takeback"},
	} {
		if err := r.people.SignInSubject(ctx, holder.email, holder.account, "refresh-"+holder.account, model.MemberFull); err != nil {
			t.Fatalf("%s's sign-in at %s after the reset: %v", holder.account, holder.email, err)
		}
	}
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	for _, replaced := range []struct{ email, account string }{{unapplied, "a-unapplied"}, {takeback, "s-takeback"}} {
		if !r.refused(t, replaced.email, replaced.account) {
			t.Errorf("after the holder's sign-in and a pass, the broker does not refuse %s at %s; want it refused", replaced.account, replaced.email)
		}
	}
	if r.refused(t, owed, "x-owed") || !r.machineSignInLive(t, owedSignIn) {
		t.Errorf("after Dispatch's first pass, the directory's owedrow@ holder X is refused %t and its sign-in live %t; want not refused and live", r.refused(t, owed, "x-owed"), r.machineSignInLive(t, owedSignIn))
	}
	for _, holder := range []struct{ email, account string }{
		{unapplied, "b-unapplied"}, {noAccount, "h-noaccount"}, {takeback, "h-takeback"},
	} {
		generation, found, err := store.NewPgSessionStore(r.dispatch.Pool).CurrentSessionGeneration(ctx, holder.email)
		if err != nil || !found || generation < raised {
			t.Fatalf("%s's session generation after the sign-in is %d (found %t, %v); want at least the reset's %d", holder.email, generation, found, err, raised)
		}
		if err := r.approveAs(t, holder.email, holder.account, generation); err != nil {
			t.Errorf("the directory's holder %s approves at %s: %v; want it approved", holder.account, holder.email, err)
		}
	}
	for _, other := range []struct{ email, account string }{
		{unapplied, "a-unapplied"}, {noAccount, "x-noaccount"}, {takeback, "s-takeback"}, {takeback, "n-takeback"}, {alias, "n-alias"},
	} {
		err := r.approveAs(t, other.email, other.account, raised+1000)
		var refusal *agentsecrets.Error
		if !errors.As(err, &refusal) || refusal.Status != 403 || refusal.Code != "APPROVER_REPLACED" {
			t.Errorf("an approve by %s at %s answered %v (%+v); want it refused 403 APPROVER_REPLACED", other.account, other.email, err, refusal)
		}
	}
}

// TestTheResetDirectoryBlockReportsEveryUnusableStatus gives each non-200, non-404 directory
// answer a refusal to leave alone. The block must say that the read is unknown, name its status
// and fail, rather than call an unavailable directory account no account.
func TestTheResetDirectoryBlockReportsEveryUnusableStatus(t *testing.T) {
	r := newRecoveryRig(t)
	cases := []struct {
		email  string
		status int
	}{
		{"directory-401@example.com", 401},
		{"directory-403@example.com", 403},
		{"directory-429@example.com", 429},
		{"directory-503@example.com", 503},
	}
	for _, test := range cases {
		r.revoke(t, test.email, "refused-"+strconv.Itoa(test.status), "holder-"+strconv.Itoa(test.status), int64(test.status))
	}
	r.revoke(t, "directory-404@example.com", "refused-404", "holder-404", 404)

	directory := map[string]directoryAnswer{}
	for _, test := range cases {
		directory[test.email] = directoryAnswer{Status: test.status}
	}
	output, err := r.clearHoldersRefusals(t, directory)
	if err == nil {
		t.Fatalf("the directory block answered success:\n%s\nwant failure after its unavailable directory reads", output)
	}
	for _, test := range cases {
		want := "UNKNOWN " + test.email + ": the directory read failed (" + strconv.Itoa(test.status) + ")"
		if !strings.Contains(output, want) {
			t.Errorf("the directory block printed:\n%s\nwant %q", output, want)
		}
		if !r.refused(t, test.email, "refused-"+strconv.Itoa(test.status)) {
			t.Errorf("the directory block cleared the refusal after its %d read at %s; want it left", test.status, test.email)
		}
	}
	if !strings.Contains(output, "NO ACCOUNT directory-404@example.com") {
		t.Errorf("the directory block printed:\n%s\nwant its 404 as NO ACCOUNT", output)
	}
	if !r.refused(t, "directory-404@example.com", "refused-404") {
		t.Error("the directory block cleared the refusal after its 404; want it left")
	}
}

// TestTheResetDirectoryBlockFailsWhenTheBrokerRefusesAClear: the directory names the refused
// account as the address's holder, and the broker's database refuses the clear. The block must
// name the address NOT CLEARED and fail, since that holder stays refused, rather than end in
// success.
//
// Control in the runbook: the clear's psql without its failure arm lets the block exit 0.
func TestTheResetDirectoryBlockFailsWhenTheBrokerRefusesAClear(t *testing.T) {
	r := newRecoveryRig(t)
	const email = "clear-refused@example.com"
	r.revoke(t, email, "refused-holder", "other-account", 1)
	for _, statement := range []string{
		`create function refuse_this_tests_clear() returns trigger language plpgsql as $$ begin raise exception 'this test refuses the clear'; end $$`,
		`create trigger refuse_this_tests_clear before delete on replaced_person_subjects for each row execute function refuse_this_tests_clear()`,
	} {
		if _, err := r.rig.Store.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the broker's database refuse the clear: %v", err)
		}
	}
	output, err := r.clearHoldersRefusals(t, map[string]directoryAnswer{
		email: {User: directoryUser{ID: "refused-holder", PrimaryEmail: email}},
	})
	if err == nil {
		t.Fatalf("the directory block answered success:\n%s\nwant failure after the broker refused its clear", output)
	}
	if want := "NOT CLEARED " + email; !strings.Contains(output, want) {
		t.Errorf("the directory block printed:\n%s\nwant %q", output, want)
	}
	if !r.refused(t, email, "refused-holder") {
		t.Error("the broker does not refuse the holder after its refused clear; want the refusal left")
	}
}

// handClearLead opens the paragraph before the block that finds each refusal of the account an
// address records, which a broker restore brings back over a clear an operator made by hand.
const handClearLead = "A refusal an operator lifted by hand with the broker clear above"

// TestTheRefusalsOfAnAddressesRecordedAccountAreFound: the broker's clear writes no row, so after
// a broker restore nothing records which refusals an operator had lifted by hand. Each such
// refusal is of the account Dispatch records for the address now, so the runbook's block finds
// them by comparing people.sub with the broker's refused accounts. Here the restored broker
// refuses holder@'s recorded account, which the block names, and other@'s replaced account, which
// Dispatch no longer records, which it leaves out. With either database unreadable it says
// UNKNOWN and fails.
//
// Control in the runbook: the block listing every refusal, without Dispatch's side (other@'s
// refusal is named too).
func TestTheRefusalsOfAnAddressesRecordedAccountAreFound(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const holder, other = "holder-cleared@example.com", "other-refused@example.com"
	if err := r.people.SignInSubject(ctx, holder, "h-recorded", "refresh-h", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"o-replaced", "o-recorded"} {
		if err := r.people.SignInSubject(ctx, other, account, "refresh-"+account, model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	r.revoke(t, holder, "h-recorded", "h-someone", 1)
	r.revoke(t, other, "o-replaced", "o-recorded", 2)
	output, err := r.runResetBlock(t, handClearLead)
	if err != nil {
		t.Fatalf("the block failed: %v\n%s", err, output)
	}
	if want := "REFUSES ITS HOLDER " + holder + " h-recorded"; !strings.Contains(output, want) {
		t.Errorf("the block printed:\n%s\nwant %q", output, want)
	}
	if strings.Contains(output, other) {
		t.Errorf("the block printed:\n%s\nwant nothing of %s, whose refused account Dispatch no longer records", output, other)
	}
	for _, broken := range []struct{ env, want string }{
		{"DATABASE_URL=" + unreadableDatabase, "UNKNOWN: Dispatch's database could not be read, so no refusal was compared"},
		{"BROKER_DATABASE_URL=" + unreadableDatabase, "UNKNOWN: the broker database could not be read, so no refusal was compared"},
	} {
		output, err := r.runResetBlock(t, handClearLead, broken.env)
		if err == nil || !strings.Contains(output, broken.want) {
			t.Errorf("the block with %s answered %v:\n%s\nwant failure and %q", broken.env, err, output, broken.want)
		}
	}
}

// TestTheResetDirectoryBlockFailsOnAnUnreadableBrokerOrAnUnparseableAnswer holds the directory
// block's two other arms that make it fail: the broker's database cannot be read for the
// addresses it refuses, and the directory answers 200 with a body that does not parse. Each must
// print its UNKNOWN line, fail, and clear no refusal, rather than end in success having read
// nothing.
//
// Controls in the runbook: the read's `|| { ...; exit 1; }` dropped, and the parse's dropped; each
// block then exits 0.
func TestTheResetDirectoryBlockFailsOnAnUnreadableBrokerOrAnUnparseableAnswer(t *testing.T) {
	t.Run("the broker's database cannot be read", func(t *testing.T) {
		r := newRecoveryRig(t)
		const email = "broker-unread@example.com"
		r.revoke(t, email, "refused-unread", "holder-unread", 1)
		output, err := r.clearHoldersRefusals(t, map[string]directoryAnswer{
			email: {User: directoryUser{ID: "refused-unread", PrimaryEmail: email}},
		}, "BROKER_DATABASE_URL="+unreadableDatabase)
		if err == nil {
			t.Fatalf("the directory block answered success:\n%s\nwant failure when the broker's database cannot be read", output)
		}
		if want := "UNKNOWN: the broker database could not be read, so no refusal was cleared"; !strings.Contains(output, want) {
			t.Errorf("the directory block printed:\n%s\nwant %q", output, want)
		}
		if !r.refused(t, email, "refused-unread") {
			t.Error("the directory block cleared a refusal it could not read; want it left")
		}
	})
	t.Run("the directory's 200 answer does not parse", func(t *testing.T) {
		r := newRecoveryRig(t)
		const email = "directory-garbled@example.com"
		r.revoke(t, email, "refused-garbled", "holder-garbled", 1)
		output, err := r.clearHoldersRefusals(t, map[string]directoryAnswer{
			email: {Status: 200, Body: `{"id": "refused-garbled", "primaryEmail":`},
		})
		if err == nil {
			t.Fatalf("the directory block answered success:\n%s\nwant failure after a 200 answer that does not parse", output)
		}
		if want := "UNKNOWN " + email + ": the directory's 200 answer did not parse"; !strings.Contains(output, want) {
			t.Errorf("the directory block printed:\n%s\nwant %q", output, want)
		}
		if !r.refused(t, email, "refused-garbled") {
			t.Error("the directory block cleared the refusal after an answer that did not parse; want it left")
		}
	})
}

// resetCloseLead opens the paragraph before the block that closes the reset.
const resetCloseLead = "**End every person's credential at the broker again, and close the reset.**"

// closeReset runs the reset's close from the runbook (runResetBlock).
func (r *recoveryRig) closeReset(t *testing.T, env ...string) (string, error) {
	t.Helper()
	return r.runResetBlock(t, resetCloseLead, env...)
}

// runResetBlock runs the runbook's block after lead (resetBlockCommand) and answers the block's
// output and its status.
func (r *recoveryRig) runResetBlock(t *testing.T, lead string, env ...string) (string, error) {
	t.Helper()
	output, err := r.resetBlockCommand(t, lead, env...).CombinedOutput()
	return string(output), err
}

// resetBlockCommand is the command that runs the runbook's block after lead through bash and psql:
// the runbook's step that makes the broker's libpq URL, then the block, against Dispatch's database
// and the broker's. A stand-in kubectl prints a token, so a block that mints one runs as far as its
// first read. env, each NAME=value, goes after the block's own environment, so a test can give it a
// database that cannot be read. Nothing of the command calls t, so a test may run it off its own
// goroutine.
func (r *recoveryRig) resetBlockCommand(t *testing.T, lead string, env ...string) *exec.Cmd {
	t.Helper()
	for _, tool := range []string{"bash", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed, so the runbook's block cannot be run here: %v", tool, err)
		}
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubs, "kubectl"), []byte("#!/bin/sh\necho not-a-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	block := runbookBlockAfter(t, lead)
	urlStep := runbookBlockAfter(t, brokerURLLead)
	script := filepath.Join(dir, "block.sh")
	if err := os.WriteFile(script, []byte(strings.Join(urlStep.lines, "\n")+"\n"+strings.Join(block.lines, "\n")+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", script)
	command.Env = append(append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BROKER_DATABASE_URL="+psqlURL(t, r.rig.Store.Pool),
		"DATABASE_URL="+psqlURL(t, r.dispatch.Pool)), env...)
	return command
}

// unreadableDatabase is a database URL nothing answers at, for a block's failed read.
const unreadableDatabase = "postgres://nobody@127.0.0.1:1/nothing?connect_timeout=2"

// TestTheResetsBlocksChangeNothingWhenADatabaseCannotBeRead: step 1, the credentials block and the
// close each read a database before they write, and each must print its UNKNOWN line, fail, and
// change nothing when that read fails: step 1 raises nothing and records no start, and the close
// records no end, whether the broker's database cannot be read or Dispatch's cannot be reached.
//
// Controls in the runbook: each arm's `|| { ...; exit 1; }` (the credentials block's `else`)
// dropped. Step 1 then fails without its line, the credentials block exits 0 having read nothing,
// and the close records the reset's end over a broker it never read.
func TestTheResetsBlocksChangeNothingWhenADatabaseCannotBeRead(t *testing.T) {
	ctx := context.Background()
	floors := func(t *testing.T, r *recoveryRig) int {
		t.Helper()
		var n int
		if err := r.dispatch.Pool.QueryRow(ctx, `select count(*) from session_generation_floors where login = '*'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("step 1, the broker's database unreadable", func(t *testing.T) {
		r := newRecoveryRig(t)
		output, err := r.runResetBlock(t, resetSessionsLead, "BROKER_DATABASE_URL="+unreadableDatabase)
		if err == nil {
			t.Fatalf("step 1 answered success:\n%s\nwant failure when the broker's database cannot be read", output)
		}
		if want := "UNKNOWN: the broker database could not be read, so nothing was reset"; !strings.Contains(output, want) {
			t.Errorf("step 1 printed:\n%s\nwant %q", output, want)
		}
		if starts, n := r.resetMarks(t, "start"), floors(t, r); len(starts) != 0 || n != 0 {
			t.Errorf("after the failed step 1 deployment_resets holds the starts %v and the deployment floor %d rows; want nothing written", starts, n)
		}
	})
	t.Run("the credentials block, the broker's database unreadable", func(t *testing.T) {
		r := newRecoveryRig(t)
		output, err := r.runResetBlock(t, resetCredentialsLead, "BROKER_DATABASE_URL="+unreadableDatabase)
		if err == nil {
			t.Fatalf("the credentials block answered success:\n%s\nwant failure when the broker's database cannot be read", output)
		}
		if want := "UNKNOWN: the broker database could not be read, so what is left is unknown"; !strings.Contains(output, want) {
			t.Errorf("the credentials block printed:\n%s\nwant %q", output, want)
		}
	})
	for _, test := range []struct{ name, env, want string }{
		{"the close, the broker's database unreadable", "BROKER_DATABASE_URL=" + unreadableDatabase,
			"UNKNOWN: the broker database could not be read, so the reset is not closed"},
		{"the close, Dispatch's database unreadable", "DATABASE_URL=" + unreadableDatabase,
			"UNKNOWN: Dispatch's database could not be read or written, so the reset is not closed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newRecoveryRig(t)
			r.resetSessions(t)
			r.ageReset(t, resetWait+time.Minute)
			output, err := r.closeReset(t, test.env)
			if err == nil {
				t.Fatalf("the close answered success:\n%s\nwant failure", output)
			}
			if !strings.Contains(output, test.want) {
				t.Errorf("the close printed:\n%s\nwant %q", output, test.want)
			}
			if ends := r.resetMarks(t, "end"); len(ends) != 0 {
				t.Errorf("after the failed close deployment_resets holds the ends %v; want none", ends)
			}
		})
	}
}

// resetMarks answers the moments deployment_resets holds for mark, oldest first.
func (r *recoveryRig) resetMarks(t *testing.T, mark string) []time.Time {
	t.Helper()
	rows, err := r.dispatch.Pool.Query(context.Background(), `select at from deployment_resets where mark = $1 order by id`, mark)
	if err != nil {
		t.Fatal(err)
	}
	marks, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		t.Fatal(err)
	}
	return marks
}

// resetWait is how long after step 1 the runbook's close waits: an hour, the life of a Google ID
// token issued before an address moved, and ten minutes for the broker's two clocks against
// Dispatch's database's: the broker process's, which checks a token's expiry, and the broker
// database's, which stamps each credential's creation.
const resetWait = time.Hour + 10*time.Minute

// ageReset moves the reset this database records, and what the broker recorded since, back by by,
// as though they happened that much earlier: each mark, the done_at of every revocation a reset's
// first transaction settled, which is exactly that transaction's start mark, and the created_at of
// every person credential at the broker, so a credential made during the reset stays inside it. The
// tables refuse such a write, so their guards are off for it, each database in one transaction, as
// the made-again test turns broker_revocations' guard off.
func (r *recoveryRig) ageReset(t *testing.T, by time.Duration) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		database   runbookDatabase
		statements []string
	}{
		{r.dispatch.Pool, []string{
			`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
			`alter table deployment_resets disable trigger deployment_resets_guarded`,
			`update broker_revocations set done_at = done_at - make_interval(secs => @by) where done_at in (select at from deployment_resets where mark = 'start')`,
			`update deployment_resets set at = at - make_interval(secs => @by)`,
			`alter table deployment_resets enable trigger deployment_resets_guarded`,
			`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
		}},
		{r.rig.Store.Pool, []string{
			// person_signins' guard refuses any write of a row's recorded account, and this
			// write moves no account: its triggers are off for the transaction, as a restore's are.
			`set local session_replication_role = replica`,
			`update person_signins set created_at = created_at - make_interval(secs => @by)`,
			`update launcher_credentials set created_at = created_at - make_interval(secs => @by)`,
			`update browser_sessions set created_at = created_at - make_interval(secs => @by)`,
		}},
	} {
		tx, err := step.database.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range step.statements {
			if _, err := tx.Exec(ctx, statement, pgx.NamedArgs{"by": by.Seconds()}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("move the reset back by %s: %q: %v", by, statement, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// resetDeployment runs the whole reset from the runbook, in its order: step 1, the credentials
// block at once, the directory block with directory, then, with the reset moved back a minute
// past the close's wait in place of waiting, the credentials block again and the close. It answers
// the reset's start and end as deployment_resets records them.
func (r *recoveryRig) resetDeployment(t *testing.T, directory map[string]directoryAnswer) (start, end time.Time) {
	t.Helper()
	r.resetSessions(t)
	if left := r.endBrokerCredentials(t); len(left) != 0 {
		t.Fatalf("STILL LIVE after the reset's first credentials run: %v", left)
	}
	if output, err := r.clearHoldersRefusals(t, directory); err != nil {
		t.Fatalf("the reset's directory block: %v\n%s", err, output)
	}
	r.ageReset(t, resetWait+time.Minute)
	if left := r.endBrokerCredentials(t); len(left) != 0 {
		t.Fatalf("STILL LIVE after the reset's second credentials run: %v", left)
	}
	if output, err := r.closeReset(t); err != nil {
		t.Fatalf("the reset's close: %v\n%s", err, output)
	}
	starts, ends := r.resetMarks(t, "start"), r.resetMarks(t, "end")
	if len(starts) == 0 || len(ends) == 0 {
		t.Fatalf("after the reset deployment_resets holds starts %v and ends %v; want one of each", starts, ends)
	}
	return starts[len(starts)-1], ends[len(ends)-1]
}

// TestTheResetClosesOnlyAnHourAfterItsStartOnACleanRead: step 1 records the reset's start; the
// credentials block runs at once and ends a replaced account's sign-in; the close refuses before
// its wait has passed since the start, and after it while a sign-in made during the wait (a Google
// ID token issued before the address moved, traded before it expired) is live; the credentials
// block run again ends it, and the close then records the reset's end. A sign-in made after the
// wait can only be the address holder's, since every token issued before the move has expired by
// then: the close leaves it out of its read, so a person who signs in at the broker early keeps
// Dispatch down no longer, and the sign-in stays live.
//
// Controls in the runbook: the close without its wait (it closes at once), the close without its
// read of the broker (it closes over the live sign-in), the close's read without its cutoff
// (STILL LIVE names the holder's sign-in), and the first run waiting the hour (the steps' text).
func TestTheResetClosesOnlyAnHourAfterItsStartOnACleanRead(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "close@example.com"
	if err := r.people.SignInSubject(ctx, email, "a-close", "refresh-a", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	signIn := r.signInMachine(t, email, "a-close", strings.Repeat("c", 43))
	steps := runbookParagraph(t, "2. End every person's credential at the broker")
	if !strings.HasPrefix(steps, "2. End every person's credential at the broker, at once.") {
		t.Errorf("the reset's step 2 reads %q; want it run at once", steps)
	}
	if want := "4. No sooner than an hour and ten minutes after step 1, end every person's credential at the broker again, then close the reset"; !strings.Contains(steps, want) {
		t.Errorf("the reset's steps read %q; want %q", steps, want)
	}

	r.resetSessions(t)
	if starts := r.resetMarks(t, "start"); len(starts) != 1 {
		t.Fatalf("after step 1 deployment_resets holds the starts %v; want one", starts)
	}
	if left := r.endBrokerCredentials(t); len(left) != 0 || r.machineSignInLive(t, signIn) {
		t.Fatalf("the credentials block run at once leaves %v, and the replaced account's sign-in live %t; want nothing", left, r.machineSignInLive(t, signIn))
	}
	output, err := r.closeReset(t)
	if err == nil || !strings.Contains(output, "NOT CLOSED") || len(r.resetMarks(t, "end")) != 0 {
		t.Errorf("the close right after step 1 answered %v:\n%s\nends %v; want NOT CLOSED and no end recorded", err, output, r.resetMarks(t, "end"))
	}

	// A sign-in traded during the wait, then the wait passes.
	traded := r.signInMachine(t, email, "a-close", strings.Repeat("d", 43))
	r.ageReset(t, resetWait+time.Minute)
	// The holder signs in at the broker once the wait has passed, before the close.
	holders := r.signInMachine(t, email, "b-close", strings.Repeat("h", 43))
	output, err = r.closeReset(t)
	if err == nil || !strings.Contains(output, "STILL LIVE") || !strings.Contains(output, traded) || len(r.resetMarks(t, "end")) != 0 {
		t.Errorf("the close past the wait, with the sign-in %s made during it live, answered %v:\n%s\nends %v; want STILL LIVE naming it and no end recorded", traded, err, output, r.resetMarks(t, "end"))
	}
	if strings.Contains(output, holders) {
		t.Errorf("the close's read names %s, a sign-in made after the wait, which only the holder can make:\n%s\nwant it left out", holders, output)
	}
	if left := r.endBrokerCredentials(t); len(left) != 0 || r.machineSignInLive(t, traded) {
		t.Fatalf("the credentials block run again leaves %v, and the sign-in live %t; want nothing", left, r.machineSignInLive(t, traded))
	}
	holders = r.signInMachine(t, email, "b-close", strings.Repeat("i", 43))
	output, err = r.closeReset(t)
	if err != nil || !strings.Contains(output, "the reset ended at") || len(r.resetMarks(t, "end")) != 1 {
		t.Errorf("the close after the second run, with only the holder's later sign-in live, answered %v:\n%s\nends %v; want the reset ended and one end recorded", err, output, r.resetMarks(t, "end"))
	}
	if !r.machineSignInLive(t, holders) {
		t.Error("the holder's sign-in made after the wait is not live after the close; want it left alone")
	}
}

// TestTheResetCloseRecordsNoEndWhenStepOneRunsAgainDuringIt: the close reads the open reset's wait,
// then the broker, then records the end, and step 1 run again between the first read and the
// insert starts a newer reset, whose wait has not passed. The insert's own condition then writes
// nothing, and the close says NOT CLOSED rather than closing the newer reset. A stand-in psql runs
// step 1's start mark just before the close's last call, the insert.
//
// Control in the runbook: the close without its empty-insert arm (it prints `the reset ended at`
// with no moment and exits 0).
func TestTheResetCloseRecordsNoEndWhenStepOneRunsAgainDuringIt(t *testing.T) {
	realPsql, err := exec.LookPath("psql")
	if err != nil {
		t.Skipf("psql is not installed, so the close cannot be run here: %v", err)
	}
	r := newRecoveryRig(t)
	r.resetSessions(t)
	r.ageReset(t, resetWait+time.Minute)
	dir := t.TempDir()
	// The close calls psql five times: Dispatch's read of the wait, the broker's three lists, and
	// Dispatch's insert of the end. Before the fifth, step 1 runs again.
	stub := `#!/bin/sh
n=$(cat "$PSQL_CALLS" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$PSQL_CALLS"
if [ "$n" = 5 ]; then
  "$REAL_PSQL" "$DATABASE_URL" -v ON_ERROR_STOP=1 -qAt -c "insert into deployment_resets (mark) values ('start')" >/dev/null || exit 1
fi
exec "$REAL_PSQL" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := r.closeReset(t,
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_PSQL="+realPsql, "PSQL_CALLS="+filepath.Join(dir, "calls"))
	if err == nil || !strings.Contains(output, "NOT CLOSED: step 1 ran again since this close read the wait") {
		t.Errorf("the close with step 1 run again before its insert answered %v:\n%s\nwant NOT CLOSED for the newer reset", err, output)
	}
	if starts, ends := r.resetMarks(t, "start"), r.resetMarks(t, "end"); len(starts) != 2 || len(ends) != 0 {
		t.Errorf("deployment_resets holds the starts %v and the ends %v; want two starts and no end", starts, ends)
	}
}

// TestTheResetCloseWaitsForAStepOneStillOpen: step 1 run again while the close runs. Step 1's
// transaction, the runbook's own statements, has recorded its start and not committed when the
// close, run from the runbook, reads the wait, reads the broker and records the end. An end
// recorded then would take an id above that start's, which the close cannot see yet, so once step
// 1 commits the newest mark would be that end, and the newer reset would read as closed without
// its wait. Step 1 and the close both lock deployment_resets, so the close queues behind step 1,
// then finds its start the newest mark and says NOT CLOSED.
//
// Controls in the runbook: step 1's lock and the close's both dropped (the close answers `the
// reset ended at` while step 1 is open, and the newest mark is then its end). Either lock alone
// orders the two, so dropping one of them passes.
func TestTheResetCloseWaitsForAStepOneStillOpen(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	r.resetSessions(t)
	r.ageReset(t, resetWait+time.Minute)

	block := runbookBlockAfter(t, resetSessionsLead)
	transaction := block.statements[1:]
	if len(transaction) < 3 || transaction[0] != "begin" || transaction[len(transaction)-1] != "commit" {
		t.Fatalf("the runbook's reset of sessions is %q; want the broker's read, then one transaction on Dispatch's database", block.statements)
	}
	var greatest int64
	if err := r.rig.Store.Pool.QueryRow(ctx, block.statements[0]).Scan(&greatest); err != nil {
		t.Fatalf("the reset's read of the broker's greatest generation: %v", err)
	}
	stepOne, err := r.dispatch.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stepOne.Rollback(ctx)
	for _, statement := range transaction[1 : len(transaction)-1] {
		if _, err := stepOne.Exec(ctx, statement, pgx.NamedArgs{"broker_greatest": greatest}); err != nil {
			t.Fatalf("the runbook's %q: %v", statement, err)
		}
	}

	type answer struct {
		output string
		err    error
	}
	closing := r.resetBlockCommand(t, resetCloseLead)
	answered := make(chan answer, 1)
	go func() {
		output, err := closing.CombinedOutput()
		answered <- answer{string(output), err}
	}()
	early, answeredFirst := storetest.WaitForAnyQueuedBehind(t, stepOne, answered, 2*time.Minute)
	if answeredFirst {
		t.Errorf("the close answered while step 1 was open, without waiting for it:\n%s", early.output)
	}
	if err := stepOne.Commit(ctx); err != nil {
		t.Fatalf("step 1's commit: %v", err)
	}
	closed := early
	if !answeredFirst {
		select {
		case closed = <-answered:
		case <-time.After(2 * time.Minute):
			t.Fatal("the close did not answer within two minutes of step 1's commit")
		}
	}
	if closed.err == nil || !strings.Contains(closed.output, "NOT CLOSED: step 1 ran again since this close read the wait") {
		t.Errorf("the close beside step 1 answered %v:\n%s\nwant NOT CLOSED for the newer reset", closed.err, closed.output)
	}
	var newest string
	if err := r.dispatch.Pool.QueryRow(ctx, `select mark from deployment_resets order by id desc limit 1`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if starts, ends := r.resetMarks(t, "start"), r.resetMarks(t, "end"); len(starts) != 2 || len(ends) != 0 || newest != "start" {
		t.Errorf("deployment_resets holds the starts %v and the ends %v, the newest mark by id %q; want two starts, no end, and the newer start newest", starts, ends, newest)
	}
}

// TestTheResetCloseRefusesAWaitAnswerItCannotRead: the close's first read answers `none`, `early`
// or the wait's end as a UTC moment, and any other answer (psql printing a notice, a server that
// answers in another form) must stop the close before it reads the broker. A stand-in psql answers
// the first call with `not-a-moment`.
//
// Control in the runbook: the close's catch-all arm taking the answer as a moment (it fails on the
// broker's read instead, naming the broker).
func TestTheResetCloseRefusesAWaitAnswerItCannotRead(t *testing.T) {
	realPsql, err := exec.LookPath("psql")
	if err != nil {
		t.Skipf("psql is not installed, so the close cannot be run here: %v", err)
	}
	r := newRecoveryRig(t)
	r.resetSessions(t)
	r.ageReset(t, resetWait+time.Minute)
	dir := t.TempDir()
	stub := `#!/bin/sh
n=$(cat "$PSQL_CALLS" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$PSQL_CALLS"
if [ "$n" = 1 ]; then
  cat >/dev/null
  echo not-a-moment
  exit 0
fi
exec "$REAL_PSQL" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := r.closeReset(t,
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_PSQL="+realPsql, "PSQL_CALLS="+filepath.Join(dir, "calls"))
	if want := "UNKNOWN: Dispatch's database answered 'not-a-moment' for the reset's wait, so the reset is not closed"; err == nil || !strings.Contains(output, want) {
		t.Errorf("the close answered %v:\n%s\nwant failure and %q", err, output, want)
	}
	if ends := r.resetMarks(t, "end"); len(ends) != 0 {
		t.Errorf("deployment_resets holds the ends %v; want none", ends)
	}
}

// TestTheResetCloseChecksItsWaitBeforeItReadsTheBroker: the close decides the wait before it reads
// the broker, so a read inside the wait can never stand for a close recorded after it. Right after
// step 1, with the broker's database unreadable, it answers NOT CLOSED for the wait, and reads
// nothing. It also refuses to close when no reset is open.
//
// Control in the runbook: the close reading the broker first (it answers UNKNOWN for the broker).
func TestTheResetCloseChecksItsWaitBeforeItReadsTheBroker(t *testing.T) {
	r := newRecoveryRig(t)
	output, err := r.closeReset(t)
	if err == nil || !strings.Contains(output, "NOT CLOSED: no reset is open") {
		t.Errorf("the close with no reset recorded answered %v:\n%s\nwant NOT CLOSED: no reset is open", err, output)
	}
	r.resetSessions(t)
	output, err = r.closeReset(t, "BROKER_DATABASE_URL="+unreadableDatabase)
	if err == nil || !strings.Contains(output, "NOT CLOSED: an hour and ten minutes have not passed since step 1") {
		t.Errorf("the close right after step 1 with the broker unreadable answered %v:\n%s\nwant NOT CLOSED for the wait", err, output)
	}
	if strings.Contains(output, "the broker database could not be read") {
		t.Errorf("the close right after step 1 read the broker before it checked its wait:\n%s", output)
	}
	if ends := r.resetMarks(t, "end"); len(ends) != 0 {
		t.Errorf("deployment_resets holds the ends %v; want none", ends)
	}
}

// TestTheResetMarksRefuseEveryWriteButTheResetsOwn: deployment_resets takes a start at the
// inserting transaction's own now(), and an end only when the newest mark, by id, is a start at
// least the close's wait old. It refuses an insert of any other moment, an end with no open start
// or before the wait, an update (one that moves a mark to now() included), a delete and a truncate,
// each with SQLSTATE 23514, so no statement moves a bound the recovery reads.
//
// Controls in the migration: the guard's tg_op branch off (the update to now() succeeds), and its
// end arm off (each early or unopened end succeeds).
func TestTheResetMarksRefuseEveryWriteButTheResetsOwn(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	refused := func(t *testing.T, statement string) {
		t.Helper()
		_, err := r.dispatch.Pool.Exec(ctx, statement)
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Errorf("%s: %v; want SQLSTATE 23514", statement, err)
		}
	}
	refused(t, `insert into deployment_resets (mark) values ('end')`)
	if _, err := r.dispatch.Pool.Exec(ctx, `insert into deployment_resets (mark) values ('start')`); err != nil {
		t.Fatalf("the reset's own start: %v", err)
	}
	for _, statement := range []string{
		`insert into deployment_resets (mark) values ('end')`,
		`insert into deployment_resets (mark, at) values ('start', now() - interval '2 hours')`,
		`update deployment_resets set at = at - interval '2 hours'`,
		`update deployment_resets set at = now()`,
		`update deployment_resets set mark = 'end'`,
		`delete from deployment_resets`,
		`truncate deployment_resets`,
	} {
		refused(t, statement)
	}
	r.ageReset(t, resetWait-time.Minute)
	refused(t, `insert into deployment_resets (mark) values ('end')`)
	r.ageReset(t, 2*time.Minute)
	if _, err := r.dispatch.Pool.Exec(ctx, `insert into deployment_resets (mark) values ('end')`); err != nil {
		t.Errorf("an end a minute past the wait after the open start: %v; want it taken", err)
	}
	refused(t, `insert into deployment_resets (mark) values ('end')`)
	if starts, ends := r.resetMarks(t, "start"), r.resetMarks(t, "end"); len(starts) != 1 || len(ends) != 1 {
		t.Errorf("deployment_resets holds the starts %v and the ends %v after the refused writes; want one of each", starts, ends)
	}
}

// TestTheResetsBoundsFollowTheMarksOrderNotTheirClock: the migration exists because a clock can be
// set back between two resets, so every reader takes the last start, and the end that closes it,
// by id. Here reset 1 runs and closes, then the database's clock steps back and reset 2 starts with
// a moment before reset 1's.
//
//   - The read of the last reset counts reset 2 as open, so a backup taken after reset 1's end runs
//     the reset's blocks again.
//   - Record-again starts at reset 2's start, so a revocation Dispatch settled between reset 2's
//     start and reset 1's is told again.
//   - The close waits from reset 2's start, not from reset 1's.
//
// Control in the runbook: each reader picking the last mark by max(at) (each row fails).
func TestTheResetsBoundsFollowTheMarksOrderNotTheirClock(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "clock-back@example.com"
	for _, account := range []string{"a-clock", "b-clock"} {
		if err := r.people.SignInSubject(ctx, email, account, "refresh-"+account, model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	r.served.revokeOwedBrokerCredentials(ctx)
	r.resetDeployment(t, map[string]directoryAnswer{email: {User: directoryUser{ID: "b-clock", PrimaryEmail: email}}})
	// Reset 1 ran on a clock half an hour ahead: its start is forty-one minutes ago and its end half
	// an hour ahead. The clock is set right, and reset 2 starts; its start is moved to an hour and
	// eleven minutes ago, before reset 1's by the clock and past the close's wait.
	r.resetSessions(t)
	for _, statement := range []string{
		`alter table deployment_resets disable trigger deployment_resets_guarded`,
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update deployment_resets set at = at + interval '30 minutes' where id in (select id from deployment_resets order by id limit 2)`,
		`update deployment_resets set at = now() - interval '71 minutes' where id = (select max(id) from deployment_resets)`,
		// The revocation the loop paid before reset 1, written as settled between reset 2's start
		// and reset 1's by the clock.
		`update broker_revocations set done_at = now() - interval '1 hour' where email = '` + email + `'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
		`alter table deployment_resets enable trigger deployment_resets_guarded`,
	} {
		if _, err := r.dispatch.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set the clock back between the two resets, %q: %v", statement, err)
		}
	}
	ends := r.resetMarks(t, "end")
	end1 := ends[len(ends)-1]

	t.Run("the read counts reset 2 open", func(t *testing.T) {
		answer := queryRunbook(t, r.dispatch.Pool, runbookBlockAfter(t, repeatResetLead), pgx.NamedArgs{"backup": end1.Add(2 * time.Hour)})
		if len(answer) != 1 || answer[0] != "run the reset's blocks again" {
			t.Errorf("the read for a backup two hours after reset 1's end, with reset 2 open, answered %q; want the reset's blocks run again", answer)
		}
	})
	t.Run("record-again starts at reset 2's start", func(t *testing.T) {
		runStatements(t, r.dispatch.Pool, runbookBlockAfter(t, brokerRestoredLead).statements, pgx.NamedArgs{"backup": "-infinity"})
		if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 1 {
			t.Errorf("record-again owes %+v, %v; want the revocation settled after reset 2's start told again", owing, err)
		}
	})
	t.Run("the close waits from reset 2's start", func(t *testing.T) {
		if left := r.endBrokerCredentials(t); len(left) != 0 {
			t.Fatalf("STILL LIVE: %v", left)
		}
		output, err := r.closeReset(t)
		if err != nil || !strings.Contains(output, "the reset ended at") {
			t.Errorf("the close of reset 2, past its wait by its own mark and inside it by reset 1's, answered %v:\n%s\nwant the reset ended", err, output)
		}
	})
}

// TestTheResetCredentialsPageLoopGivesBrokerRunNoStdin runs the page-session loop itself. Its
// broker_run stand-in reads stdin, as kubectl run -i, docker run -i or ssh can. With stdin from
// /dev/null it ends every listed login; without the redirect its first run consumes the remaining
// logins from the loop.
func TestTheResetCredentialsPageLoopGivesBrokerRunNoStdin(t *testing.T) {
	for _, tool := range []string{"bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed, so the page-session loop cannot be run here: %v", tool, err)
		}
	}
	block := readCredentialsBlock(t)
	const runner = `broker_run() { '<the command that runs its arguments where your deployment runs a broker subcommand>' "$@"; }`
	const standIn = `broker_run() { cat >/dev/null; printf 'ENDED %s\n' "$3"; }`
	text := strings.Replace(block.text, runner, standIn, 1)
	if text == block.text {
		t.Fatalf("the runbook's page-session loop has no broker_run declaration %q:\n%s", runner, block.text)
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o700); err != nil {
		t.Fatal(err)
	}
	kubectl := "#!/bin/sh\nprintf token\n"
	if err := os.WriteFile(filepath.Join(stubs, "kubectl"), []byte(kubectl), 0o700); err != nil {
		t.Fatal(err)
	}
	psql := `#!/bin/sh
case "$*" in
  *"browser_sessions"*)
    if [ ! -e "$PAGES_SEEN" ]; then
      printf 'a@example.com\nb@example.com\nc@example.com\n'
      : >"$PAGES_SEEN"
    fi
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(stubs, "psql"), []byte(psql), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "credentials.sh")
	if err := os.WriteFile(script, []byte(text+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", script)
	command.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PAGES_SEEN="+filepath.Join(dir, "pages-seen"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the credentials block: %v\n%s", err, output)
	}
	for _, login := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if !strings.Contains(string(output), "ENDED "+login) {
			t.Errorf("a broker_run stand-in that reads stdin ended no %s:\n%s", login, output)
		}
	}
	if !strings.Contains(block.text, `broker_run envoy-broker end-sessions "$login" </dev/null`) {
		t.Errorf("the runbook's page-session loop is missing </dev/null:\n%s", block.text)
	}
}
