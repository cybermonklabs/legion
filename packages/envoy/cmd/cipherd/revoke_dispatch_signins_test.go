package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The rows every run of the subcommand reads: two Dispatch sign-ins of one person, one approved
// with a scope and one with none, a third whose credential has expired with its tokens still
// live, and a machine login, which is no sign-in. Each holds one access token and one refresh
// token of its own.
const seedDispatchSignIns = `
insert into credential_requests (id, body, kind, approver, code, expires_at, dispatch_scopes) values
	('rec-sign-in-person', 'body', 'launcher_credential', 'ada@example.com', 'BCDF-GHJK', now() + interval '15 minutes', '{person}'),
	('rec-sign-in-none',   'body', 'launcher_credential', 'ada@example.com', 'BCDF-GHJL', now() + interval '15 minutes', '{}'),
	('rec-sign-in-old',    'body', 'launcher_credential', 'ada@example.com', 'BCDF-GHJM', now() + interval '15 minutes', '{person}'),
	('rec-machine',        'body', 'launcher_credential', 'ada@example.com', 'BCDF-GHJN', now() + interval '15 minutes', null);
insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, record_id, expires_at, dispatch_scopes) values
	('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d11', 'ada@example.com', 'example-host', 'thumbprint-sign-in-person', '{}', 'rec-sign-in-person', now() + interval '7 days',  '{person}'),
	('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d12', 'ada@example.com', 'example-host', 'thumbprint-sign-in-none',   '{}', 'rec-sign-in-none',   now() + interval '7 days',  '{}'),
	('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d13', 'ada@example.com', 'example-host', 'thumbprint-sign-in-old',    '{}', 'rec-sign-in-old',    now() - interval '1 hour',  '{person}'),
	('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d14', 'ada@example.com', 'example-host', 'thumbprint-machine',        '{}', 'rec-machine',        now() + interval '7 days',  null);
insert into oauth_access_tokens (signature, request_id, credential_id, request, active, expires_at)
	select 'access-' || c.key_thumbprint, 'request-' || c.key_thumbprint, c.id, '{}', true, now() + interval '15 minutes' from launcher_credentials c;
insert into oauth_refresh_tokens (signature, request_id, credential_id, request, active, expires_at)
	select 'refresh-' || c.key_thumbprint, 'request-' || c.key_thumbprint, c.id, '{}', true, now() + interval '7 days' from launcher_credentials c;
`

// TestTheBinaryRevokeDispatchSignInsEndsEverySignInAndItsTokens runs the REAL compiled broker's
// subcommand against the real Postgres, with BROKER_DATABASE_URL as its whole environment beside
// PATH: the database here takes the URL's own password, so the subcommand needs no other broker
// setting. It has the broker revoke every launcher credential whose dispatch_scopes is not null,
// the one approved with no scope and the expired one among them, and end each one's OAuth tokens,
// which is what the step is for, each audit row naming the broker's own actor
// (enroll.DispatchSignInsActor) since no person decided it; the machine login on the same database
// keeps its row and its live tokens. A second run finds nothing left and says so.
//
// Controls, in the test: the machine login's row and tokens, which fail the run where the
// subcommand revokes by any rule wider than "dispatch_scopes is not null"; the sign-ins' tokens,
// which stay active where a revoke writes revoked_at itself instead of going through enroll's
// endCredential; and a run with no BROKER_DATABASE_URL, which must refuse rather than report a
// clean sweep of a database it never opened.
func TestTheBinaryRevokeDispatchSignInsEndsEverySignInAndItsTokens(t *testing.T) {
	ctx := context.Background()
	binary := buildBroker(t)
	databaseURL := storetest.URL(t)
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Pool.Exec(ctx, seedDispatchSignIns); err != nil {
		t.Fatalf("seed three Dispatch sign-ins, a machine login and their tokens: %v", err)
	}

	out := runRevokeDispatchSignIns(t, binary, databaseURL, 0)
	t.Logf("broker %s printed: %s", revokeDispatchSignInsCommand, strings.TrimSpace(out))
	if want := revokeDispatchSignInsCommand + ": revoked 3 Dispatch sign-ins\n"; out != want {
		t.Fatalf("broker %s printed %q, want %q", revokeDispatchSignInsCommand, out, want)
	}
	if strings.Contains(out, "broker listening") {
		t.Fatalf("the subcommand bound a listener: %s", out)
	}

	for _, row := range []struct {
		thumbprint string
		revoked    bool
	}{
		{"thumbprint-sign-in-person", true},
		{"thumbprint-sign-in-none", true},
		{"thumbprint-sign-in-old", true},
		{"thumbprint-machine", false},
	} {
		var revoked, accessLive, refreshLive bool
		if err := st.Pool.QueryRow(ctx, `select c.revoked_at is not null,
				(select a.active from oauth_access_tokens a where a.credential_id = c.id),
				(select r.active from oauth_refresh_tokens r where r.credential_id = c.id)
			from launcher_credentials c where c.key_thumbprint = $1`, row.thumbprint).Scan(&revoked, &accessLive, &refreshLive); err != nil {
			t.Fatalf("read %s after the run: %v", row.thumbprint, err)
		}
		if revoked != row.revoked || accessLive == row.revoked || refreshLive == row.revoked {
			t.Errorf("%s after the run: revoked %t, its access token live %t, its refresh token live %t; want revoked %t with its tokens %t",
				row.thumbprint, revoked, accessLive, refreshLive, row.revoked, !row.revoked)
		}
	}
	var audited int
	if err := st.Pool.QueryRow(ctx, `select count(*) from audit where kind = 'launcher_credential.revoked' and actor = $1`,
		enroll.DispatchSignInsActor).Scan(&audited); err != nil || audited != 3 {
		t.Errorf("%d launcher_credential.revoked audit rows name %s (%v), want 3, one per sign-in, the broker's own revoke", audited, enroll.DispatchSignInsActor, err)
	}

	again := runRevokeDispatchSignIns(t, binary, databaseURL, 0)
	if want := revokeDispatchSignInsCommand + ": revoked 0 Dispatch sign-ins\n"; again != want {
		t.Fatalf("a second run printed %q, want %q: the first revoked them all", again, want)
	}

	cmd := exec.Command(binary, revokeDispatchSignInsCommand, "extra")
	cmd.Env = []string{"BROKER_DATABASE_URL=" + databaseURL, "PATH=" + os.Getenv("PATH")}
	output, err := cmd.CombinedOutput()
	if exit := exitCodeOf(t, err); exit != 2 || !strings.Contains(string(output), "usage: broker "+revokeDispatchSignInsCommand+", with no argument") {
		t.Errorf("broker %s extra = exit %d: %s; want 2 and the usage line", revokeDispatchSignInsCommand, exit, output)
	}

	cmd = exec.Command(binary, revokeDispatchSignInsCommand)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err = cmd.CombinedOutput()
	if exit := exitCodeOf(t, err); exit != 1 || !strings.Contains(string(output), "BROKER_DATABASE_URL is required") {
		t.Errorf("broker %s with no database setting = exit %d: %s; want 1 naming BROKER_DATABASE_URL", revokeDispatchSignInsCommand, exit, output)
	}
}

// runRevokeDispatchSignIns runs the broker binary's revoke-dispatch-signins with
// BROKER_DATABASE_URL and PATH as its whole environment — no sign-in, secret store or public URL
// setting — requires the exit code wantExit, and answers everything it printed.
func runRevokeDispatchSignIns(t *testing.T, binary, databaseURL string, wantExit int) string {
	t.Helper()
	cmd := exec.Command(binary, revokeDispatchSignInsCommand)
	cmd.Env = []string{"BROKER_DATABASE_URL=" + databaseURL, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if exit := exitCodeOf(t, err); exit != wantExit {
		t.Fatalf("broker %s exited %d, want %d:\n%s", revokeDispatchSignInsCommand, exit, wantExit, out)
	}
	return string(out)
}

// exitCodeOf is the exit code a finished command's error carries, 0 for one that succeeded; any
// other failure to run the binary fails t.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	}
	t.Fatalf("run the broker: %v", err)
	return 0
}
