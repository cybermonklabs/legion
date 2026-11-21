package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// testerR26SweepLead opens the runbook's paragraph before the block that ends every person's
// machine login recording no approving account (the move between the pool and a domain).
const testerR26SweepLead = "The secrets broker serves a machine login without asking Dispatch, and a machine login approved"

// testerR26PoolLogin makes a machine login as a pool-era approve made one, recording no approving
// account, and checks it is live.
func testerR26PoolLogin(t *testing.T, r *recoveryRig, email string) string {
	t.Helper()
	client := ciphertest.NewClient(t, r.rig.URL, strings.TrimPrefix(r.rig.URL, "http://"))
	client.UIToken, client.DPoP = r.rig.UIToken, cipherclient.NewDPoPTransport
	machine := client.DeviceLogin(t, ciphertest.NewSigningKey(t), email, "sweep-box")
	login := client.ApproveMachineLogin(t, email, machine.UserCode)
	var recordsNone bool
	if err := r.rig.Store.Pool.QueryRow(context.Background(), `select approver_subject is null from launcher_credentials where id = $1::uuid`, login).Scan(&recordsNone); err != nil {
		t.Fatal(err)
	}
	if !recordsNone || !testerR26LoginLive(t, r, login) {
		t.Fatalf("setup: the login records no approver %t and is live %t; want both", recordsNone, testerR26LoginLive(t, r, login))
	}
	return login
}

// testerR26LoginLive reads the login at the broker directly, not through any block's own list.
func testerR26LoginLive(t *testing.T, r *recoveryRig, login string) bool {
	t.Helper()
	var live bool
	if err := r.rig.Store.Pool.QueryRow(context.Background(), `select revoked_at is null from launcher_credentials where id = $1::uuid`, login).Scan(&live); err != nil {
		t.Fatal(err)
	}
	return live
}

// testerR26RunSweep runs the sweep block from the runbook through bash, psql and curl against the
// rig's broker: its URL placeholder becomes the rig's URL, and a kubectl stand-in prints the rig's
// account token. dropRevoke replaces the block's revoke call with a no-op, the F2 control.
func testerR26RunSweep(t *testing.T, r *recoveryRig, dropRevoke bool) (string, error) {
	t.Helper()
	block := runbookBlockAfter(t, testerR26SweepLead)
	text := strings.Join(block.lines, "\n")
	if !strings.Contains(text, "no machine login records no approving account") {
		t.Fatalf("the block after %q is not the sweep: %s", testerR26SweepLead, text)
	}
	text = strings.Replace(text, "'<the URL in DISPATCH_AGENT_SECRETS_URL>'", `"$TESTER_BROKER_URL"`, 1)
	if dropRevoke {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if strings.Contains(line, "/revoke-by-approver") {
				lines[i] = "  true # tester control: the revoke call replaced by a no-op"
			}
		}
		text = strings.Join(lines, "\n")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$TESTER_ACCOUNT_TOKEN\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "sweep.sh")
	if err := os.WriteFile(script, []byte(text+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", script)
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BROKER_PSQL_URL="+psqlURL(t, r.rig.Store.Pool),
		"TESTER_BROKER_URL="+r.rig.URL,
		"TESTER_ACCOUNT_TOKEN="+r.rig.UIToken)
	output, err := command.CombinedOutput()
	t.Logf("the sweep block printed (exit error %v):\n%s", err, output)
	return string(output), err
}

// TestTesterR26TheSweepBlockEndsEveryLoginRecordingNoApprover is F2's test: no test of the
// repository runs the pool-to-domain sweep. A pool-era login is live, the block runs, and the login
// is read revoked; TESTER_R26_DROP_REVOKE=1 is its red run: the block's revoke call is a no-op, so
// the login stays live, the block prints STILL LIVE and fails, and this test fails three ways.
func TestTesterR26TheSweepBlockEndsEveryLoginRecordingNoApprover(t *testing.T) {
	r := newRecoveryRig(t)
	login := testerR26PoolLogin(t, r, "sweep@example.com")
	output, err := testerR26RunSweep(t, r, os.Getenv("TESTER_R26_DROP_REVOKE") == "1")
	if err != nil {
		t.Errorf("the sweep block failed: %v", err)
	}
	if !strings.Contains(output, "no machine login records no approving account") {
		t.Errorf("the sweep block did not print its done line")
	}
	if testerR26LoginLive(t, r, login) {
		t.Errorf("after the sweep block, the login %s recording no approver is live; want it revoked", login)
	}
}

// testerR26LiveRows answers, for every broker table with a revoked_at column and an id, the ids
// live now: what a backup taken now holds live. A changed table with no id fails the restore.
func testerR26LiveRows(t *testing.T, r *recoveryRig) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rows, err := r.rig.Store.Pool.Query(ctx, `select c.table_name, exists (select 1 from information_schema.columns i where i.table_schema = c.table_schema and i.table_name = c.table_name and i.column_name = 'id')
		from information_schema.columns c join information_schema.tables t on t.table_schema = c.table_schema and t.table_name = c.table_name
		where c.table_schema = current_schema() and c.column_name = 'revoked_at' and t.table_type = 'BASE TABLE' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		name  string
		hasID bool
	}
	var tables []table
	for rows.Next() {
		var tb table
		if err := rows.Scan(&tb.name, &tb.hasID); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, tb)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tables) == 0 {
		t.Fatal("UNKNOWN: no broker table with a revoked_at column was read")
	}
	live := map[string][]string{}
	for _, tb := range tables {
		if !tb.hasID {
			var n int
			if err := r.rig.Store.Pool.QueryRow(ctx, `select count(*) from `+pgx.Identifier{tb.name}.Sanitize()+` where revoked_at is null`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			live["#"+tb.name] = []string{strings.Repeat("x", n)}
			continue
		}
		ids, err := r.rig.Store.Pool.Query(ctx, `select id::text from `+pgx.Identifier{tb.name}.Sanitize()+` where revoked_at is null`)
		if err != nil {
			t.Fatal(err)
		}
		for ids.Next() {
			var id string
			if err := ids.Scan(&id); err != nil {
				t.Fatal(err)
			}
			live[tb.name] = append(live[tb.name], id)
		}
		if err := ids.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

// testerR26Restore writes back the backup's live rows, as a restore of the broker's database
// does: triggers off, as pg_restore --disable-triggers runs. It answers how many rows it revived.
func testerR26Restore(t *testing.T, r *recoveryRig, backup map[string][]string) int {
	t.Helper()
	ctx := context.Background()
	now := testerR26LiveRows(t, r)
	for name, ids := range backup {
		if strings.HasPrefix(name, "#") && len(now[name][0]) != len(ids[0]) {
			t.Fatalf("UNKNOWN: %s has no id, and its live rows changed from %d to %d, so this restore cannot put it back", name[1:], len(ids[0]), len(now[name][0]))
		}
	}
	tx, err := r.rig.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	revived := 0
	for name, ids := range backup {
		if strings.HasPrefix(name, "#") {
			continue
		}
		tag, err := tx.Exec(ctx, `update `+pgx.Identifier{name}.Sanitize()+` set revoked_at = null where id::text = any($1) and revoked_at is not null`, ids)
		if err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		revived += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return revived
}

// TestABrokerRestoredFromBeforeThePoolSweepIsSweptAgain is F3's test, from the tester's round 26
// (TestTesterR26ABrokerRestoredFromBeforeThePoolSweepKeepsItsLoginLive there). A pool-era machine
// login is live; the broker is backed up; the pool-to-domain sweep ends the login; the broker's
// database alone is restored to the backup, which brings the login back; and the operator runs
// what the runbook gives for a broker restored alone: the record-again block with the backup's
// moment, the sweep again where the runbook says so, then Dispatch's pass. The sweep's revoke is
// the broker's alone, so Dispatch records no revocation for it, and the record-again block finds
// nothing to record again: only the sweep run again ends the login. Without the runbook's sentence
// the test runs no sweep and fails, naming the login (as it did at 762114d9c9). Its last step runs
// the sweep once more and shows that ends nothing new.
func TestABrokerRestoredFromBeforeThePoolSweepIsSweptAgain(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	login := testerR26PoolLogin(t, r, "pool-era@example.com")

	backupMoment := time.Now()
	backup := testerR26LiveRows(t, r)

	if _, err := testerR26RunSweep(t, r, false); err != nil {
		t.Fatalf("setup: the sweep block failed: %v", err)
	}
	if testerR26LoginLive(t, r, login) {
		t.Fatalf("setup: after the sweep the login %s is live; want it revoked", login)
	}

	revived := testerR26Restore(t, r, backup)
	if revived == 0 || !testerR26LoginLive(t, r, login) {
		t.Fatalf("setup: the restore revived %d rows and the login is live %t; want the login live again", revived, testerR26LoginLive(t, r, login))
	}
	t.Logf("the broker's restore revived %d rows the sweep had revoked", revived)

	count, _, err := r.revocations.Owed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Dispatch owes %d revocations after the broker's restore", count)

	block := runbookBlockAfter(t, brokerRestoredLead)
	runStatements(t, r.dispatch.Pool, block.statements, pgx.NamedArgs{"backup": backupMoment})
	if paragraph := runbookParagraph(t, poolSweepAgainLead); strings.Contains(paragraph, "run the sweep block of that move again after the record-again block and before starting Dispatch") {
		if _, err := testerR26RunSweep(t, r, false); err != nil {
			t.Errorf("the sweep block the runbook runs again after a broker restore failed: %v", err)
		}
	}
	r.ageClaims(t)
	r.served.revokeOwedBrokerCredentials(ctx)
	if testerR26LoginLive(t, r, login) {
		t.Errorf("after the runbook's recovery of a broker restored alone to a backup from before the pool-to-domain sweep, the pool-era machine login %s, recording no approving account, is live; want it ended, or the recovery to say to run the sweep again", login)
	}

	if _, err := testerR26RunSweep(t, r, false); err != nil {
		t.Errorf("the sweep block run again failed: %v", err)
	}
	if testerR26LoginLive(t, r, login) {
		t.Errorf("after the sweep block run again, the login %s is live; want it revoked", login)
	}
}
