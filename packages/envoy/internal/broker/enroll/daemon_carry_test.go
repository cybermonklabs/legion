// daemon_carry_test.go is a pod's carry under the daemon that enrolled it (plan E's t-e-54):
// every public Legion project's pods run as one account, legion-worker, and each
// project's daemon logs in as an account of its own, so a pod's grants follow the pair of the two
// (carryPrincipal, exchange.PodLaunchPrincipal) while its session name is held under its account
// alone (namePrincipal).
package enroll

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The two daemons of these tests, each its project's own account, whose pods both run as
// carryPodAccount, as every public project's pods run as legion-worker.
const (
	acmeDaemonAccount   = "system:serviceaccount:legion:legion-acme-daemon"
	globexDaemonAccount = "system:serviceaccount:legion:legion-globex-daemon"
)

// twoDaemons is a Service whose deployment binds carryPodAccount to the daemon service both
// daemons log in as, and each daemon's token login.
func twoDaemons(t *testing.T) (svc *Service, issuer *oidctest.Issuer, key *oidctest.Key, acme, globex Credential) {
	t.Helper()
	svc = newService(t)
	issuer, key = withPodVerifier(t, svc)
	svc.ServiceLogins = map[string]string{acmeDaemonAccount: "legion-daemon", globexDaemonAccount: "legion-daemon"}
	svc.ServicesByAccount = map[string]string{carryPodAccount: "legion-daemon"}
	rig := podRig{svc: svc}
	return svc, issuer, key, rig.login(t, acmeDaemonAccount, ""), rig.login(t, globexDaemonAccount, "")
}

// enrolPod enrols pod podUID of carryPodAccount under session, by the daemon whose login cred is.
func enrolPod(t *testing.T, svc *Service, issuer *oidctest.Issuer, key *oidctest.Key, cred Credential, session, podUID string) Enrollment {
	t.Helper()
	enr, err := svc.Create(context.Background(), cred, Enrollment{
		Kind: "pod", RuntimeID: podUID, Thumbprint: "tp-" + podUID, Slot: "implementer-g1", SessionID: str(session),
		PodToken: mintPodToken(t, issuer, key, carryPodAccount, podUID),
	})
	if err != nil {
		t.Fatalf("enrol pod %s under %s: %v", podUID, session, err)
	}
	return enr
}

// endLeavingDormant revokes enr as its own daemon does when it lets the role's process go, which
// leaves a named session's grant standing on the row for the session's next enrolment.
func endLeavingDormant(t *testing.T, svc *Service, cred Credential, enr Enrollment) {
	t.Helper()
	if err := svc.Revoke(context.Background(), cred, enr.ID.String(), "launcher"); err != nil {
		t.Fatalf("revoke %s: %v", enr.ID, err)
	}
	if left := readCarryState(t, svc, enr.ID); !left.Ended || !left.Dormant || left.LiveGrants != 1 {
		t.Fatalf("the end left %+v, want it ended, dormant and still holding its grant", left)
	}
}

// spellAccountAlone rewrites enr's carry principal to the account alone, the spelling every pod
// row a broker before migration 0033 wrote carries, and one an older replica still writes while
// the broker rolls.
func spellAccountAlone(t *testing.T, svc *Service, id uuid.UUID) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `update enrollments set carry_principal = 'account:' || subject where id = $1`, id); err != nil {
		t.Fatalf("spell %s's carry principal as the account alone: %v", id, err)
	}
}

// TestAPodOfAnotherDaemonCannotAdoptASessionsGrants: ACME's daemon enrols an ACME pod under
// session S and the session is granted a secret; the pod ends, leaving the grant for S's next
// enrolment. GLOBEX's daemon then enrols a GLOBEX pod, of the same account, under S, as a GLOBEX
// role that reported an ACME claim's session id would have it do: nothing moves, the ACME row still
// holds the grant and the GLOBEX row holds none. ACME's daemon's next enrolment of S takes it. The same
// holds of a row an older broker replica wrote under the account alone: its own daemon takes it,
// and the other daemon does not.
//
// The control: the account alone as a pod's carry principal (carryPrincipal's pod branch
// answering exchange.AccountPrincipal(*in.Subject)); GLOBEX's enrolment adopts the grant, and the test
// fails naming it (the pull request records the run).
func TestAPodOfAnotherDaemonCannotAdoptASessionsGrants(t *testing.T) {
	for _, spelling := range []struct {
		name   string
		legacy bool
	}{
		{"a row of this broker", false},
		{"a row an older broker replica wrote under the account alone", true},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			svc, issuer, key, acme, globex := twoDaemons(t)
			const session = "ses_acme"
			first := enrolPod(t, svc, issuer, key, acme, session, "acme-pod-1")
			grant := insertLiveGrant(t, svc, first.ID)
			endLeavingDormant(t, svc, acme, first)
			if spelling.legacy {
				spellAccountAlone(t, svc, first.ID)
			}

			taker := enrolPod(t, svc, issuer, key, globex, session, "globex-pod-1")
			if holder := grantHolder(t, svc, grant); holder != first.ID {
				t.Fatalf("ACME's grant %s hangs off %s after GLOBEX's daemon enrolled %s, want ACME's own row %s: a pod of another daemon takes no grant of the session it names",
					grant, holder, session, first.ID)
			}
			if took := readCarryState(t, svc, taker.ID); took.LiveGrants != 0 || took.Pending != 0 {
				t.Fatalf("GLOBEX's pod enrolled under ACME's session holds %+v, want nothing", took)
			}

			next := enrolPod(t, svc, issuer, key, acme, session, "acme-pod-2")
			if holder := grantHolder(t, svc, grant); holder != next.ID {
				t.Fatalf("ACME's grant %s hangs off %s after ACME's daemon enrolled %s again, want its new row %s", grant, holder, session, next.ID)
			}
		})
	}
}

// TestAPodAndItsTradeStillShareOneSessionName: a pod's grants follow a pair, but its session name
// is still held under its account, the principal its own token exchange claims the name under
// (oauth.Provider.claimSession binds exchange.Set.Pod's Principal), so the pod's enrolment under S
// and its trade under S never refuse each other. A pod enrolled under a login that proved no
// account carries nothing, and its name is held under its account all the same.
//
// The control: bind the pair in bindSessionName (in.carry for in.name). The name is then held by
// the pair, or by no principal under the login that proved no account, and not by the pod's
// account, so the test fails at the name's holder, naming S, before it reaches the trade (the pull
// request records the run).
func TestAPodAndItsTradeStillShareOneSessionName(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: carryPodAccount, Project: "LEGION", Person: "ada@example.com"}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	binder := oauthstore.New(svc.Store.Pool)
	for _, row := range []struct {
		name      string
		cred      Credential
		wantCarry string
	}{
		{"under its daemon's token login", daemonLogin(t, svc), exchange.PodLaunchPrincipal(carryPodAccount, exchange.AccountPrincipal(carryDaemonAccount))},
		{"under a login that proved no account", mintCredential(t, svc, nil, str("legion-daemon"), "cluster"), ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			session, uid := "ses-"+uuid.NewString()[:8], "pod-"+uuid.NewString()[:8]
			enr, err := svc.Create(ctx, row.cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
				PodToken: mintPodToken(t, issuer, key, carryPodAccount, uid)})
			if err != nil {
				t.Fatalf("the pod's enrolment: %v", err)
			}
			if got := carryPrincipalOf(t, svc, enr.ID); got != row.wantCarry {
				t.Fatalf("the enrolment's grants follow %q, want %q", got, row.wantCarry)
			}
			claims, err := set.Pod(map[string]any{"sub": carryPodAccount, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": uid}}}, session, nil)
			if err != nil {
				t.Fatalf("the exchange's reading of the pod's token: %v", err)
			}
			if got := sessionNameHolder(t, svc, session); got != claims.Principal {
				t.Fatalf("%s is held by %q after the enrolment, want the pod's account %q, the principal its trade claims it under", session, got, claims.Principal)
			}
			if _, err := binder.BindSession(ctx, session, claims.Principal, false, time.Now().Add(time.Hour)); err != nil {
				if errors.Is(err, oauthstore.ErrSessionHeld) {
					t.Fatalf("the pod's trade under %s, the name it enrolled, was refused as another's: %v", session, err)
				}
				t.Fatalf("the pod's trade under %s: %v", session, err)
			}
		})
	}
}

// TestARunningSessionKeepsItsGrantsAcrossTheCarryMigration: a session's pod rows written before
// migration 0033, carrying the account alone, one dormant and one live and each holding a grant,
// are rewritten by the migration to the pair its own daemon's enrolment spells, in SQL spelled as
// exchange.PodLaunchPrincipal spells it. An older broker replica's adopt, which matches a pod's
// predecessors on the account alone (carry_principal = 'account:' || subject), then finds none of
// the session's rows: that is what the rewrite is for, since while the broker rolls it keeps an
// older replica from handing those rows to another daemon's pod enrolled under the session's name.
// The next enrolment of the session by its own daemon then takes both grants. That half is a
// regression guard and no proof of the rewrite: a new replica's adopt also takes an account-alone
// row whose login proved the enrolling login's account, so it passes without the rewrite.
//
// The control: skip the rewrite (run nothing for 0033); the rows keep the account alone, and the
// test fails naming them and the count of them the older replica's match finds, while the
// adoption half still passes (the pull request records the run).
func TestARunningSessionKeepsItsGrantsAcrossTheCarryMigration(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	cred := daemonLogin(t, svc)
	const session = "ses_running"
	// Two rows of one session as a broker before 0033 left them: the grants are written after both
	// rows exist, so neither row took the other's at its creation.
	dormant := enrolPod(t, svc, issuer, key, cred, session, "pod-before-1")
	live := enrolPod(t, svc, issuer, key, cred, session, "pod-before-2")
	dormantGrant, liveGrant := insertLiveGrant(t, svc, dormant.ID), insertLiveGrant(t, svc, live.ID)
	if _, err := svc.Store.Pool.Exec(ctx, `update enrollments set revoked_at = now(), dormant_since = now() where id = $1`, dormant.ID); err != nil {
		t.Fatalf("end the first row dormant: %v", err)
	}
	spellAccountAlone(t, svc, dormant.ID)
	spellAccountAlone(t, svc, live.ID)

	rewrite, err := os.ReadFile("../store/migrations/0033_pod_carry_launcher.up.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, string(rewrite)); err != nil {
		t.Fatalf("run the migration's rewrite: %v", err)
	}
	want := exchange.PodLaunchPrincipal(carryPodAccount, exchange.AccountPrincipal(carryDaemonAccount))
	for _, id := range []uuid.UUID{dormant.ID, live.ID} {
		if got := carryPrincipalOf(t, svc, id); got != want {
			t.Errorf("row %s carries %q after the migration, want %q, the spelling its daemon's next enrolment writes", id, got, want)
		}
	}
	// The match an older replica's adopt makes for a pod enrolling under the session, the account
	// alone, finds none of the session's rows once they carry the pair.
	var olderReplicaFinds int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments
		where kind = 'pod' and session_id = $1 and carry_principal = 'account:' || subject`, session).Scan(&olderReplicaFinds); err != nil {
		t.Fatalf("count the session's rows an older replica's adopt matches: %v", err)
	}
	if olderReplicaFinds != 0 {
		t.Errorf("an older replica's adopt, matching the account alone, finds %d of %s's rows after the migration, want none: while the broker rolls, it would hand them to another daemon's pod enrolled under %s",
			olderReplicaFinds, session, session)
	}

	next := enrolPod(t, svc, issuer, key, cred, session, "pod-after")
	for _, grant := range []uuid.UUID{dormantGrant, liveGrant} {
		if holder := grantHolder(t, svc, grant); holder != next.ID {
			t.Errorf("grant %s hangs off %s after the session's next enrolment, want %s: a running session keeps its grants across the migration", grant, holder, next.ID)
		}
	}
}

// TestTheCarryMigrationTakesItsRowsInAdoptsOrder: migration 0033 locks the rows it rewrites in id
// order, the order adopt locks a session's predecessors in, so it cannot deadlock with an older
// broker replica's adoption of the same rows while the broker rolls, and fail the boot. An
// adoption holds the lower of two rows of one session; the rewrite waits on it, holding nothing;
// the adoption then takes the higher row, as adopt's `order by e.id for update` does, and commits;
// the rewrite completes and rewrites both.
//
// The control: the rewrite's `order by e.id desc`; it takes the higher row first and waits on the
// lower, the adoption waits on the higher, Postgres ends one of the two as a deadlock, and the
// test fails naming it (the pull request records the run).
func TestTheCarryMigrationTakesItsRowsInAdoptsOrder(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	cred := daemonLogin(t, svc)
	const session = "ses_rolling"
	for _, pod := range []string{"pod-roll-1", "pod-roll-2"} {
		spellAccountAlone(t, svc, enrolPod(t, svc, issuer, key, cred, session, pod).ID)
	}
	rows, err := svc.Store.Pool.Query(ctx, `select id from enrollments where session_id = $1 order by id`, session)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) != 2 {
		t.Fatalf("the session's rows are %v (%v), want two", ids, rows.Err())
	}
	rewrite, err := os.ReadFile("../store/migrations/0033_pod_carry_launcher.up.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}

	adoption, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adoption.Rollback(ctx) }()
	if _, err := adoption.Exec(ctx, `select 1 from enrollments where id = $1 for update`, ids[0]); err != nil {
		t.Fatalf("the adoption's lock of the lower row: %v", err)
	}
	migrated := make(chan error, 1)
	go func() {
		_, err := svc.Store.Pool.Exec(ctx, string(rewrite))
		migrated <- err
	}()
	storetest.AwaitLockWaiters(t, svc.Store.Pool, adoption, 1)
	if _, err := adoption.Exec(ctx, `select 1 from enrollments where id = $1 for update`, ids[1]); err != nil {
		t.Fatalf("the adoption's lock of the higher row while the rewrite waits: %v", err)
	}
	if err := adoption.Commit(ctx); err != nil {
		t.Fatalf("the adoption's commit: %v", err)
	}
	select {
	case err := <-migrated:
		if err != nil {
			t.Fatalf("the rewrite beside the adoption: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rewrite did not complete within 30s once the adoption committed")
	}
	want := exchange.PodLaunchPrincipal(carryPodAccount, exchange.AccountPrincipal(carryDaemonAccount))
	for _, id := range ids {
		if got := carryPrincipalOf(t, svc, id); got != want {
			t.Errorf("row %s carries %q after the rewrite, want %q", id, got, want)
		}
	}
}
