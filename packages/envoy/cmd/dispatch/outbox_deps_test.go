package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestOutboxDepsTakesTheRoleModeFromTheEnv: the mode the outbox resolves a role route's holder in
// is DISPATCH_PROJECT_PRIVACY_REQUIRED, read through the settings table (outboxDepsTakeTheMode).
func TestOutboxDepsTakesTheRoleModeFromTheEnv(t *testing.T) {
	outboxDepsTakeTheMode(t)
}

// outboxDepsTakeTheMode hands DISPATCH_PROJECT_PRIVACY_REQUIRED to its reader through the
// settings table, as main does (resolveBootConfig, then outboxDeps): `true` gives the outbox's
// role reader roles.Required, unset or blank gives roles.AllPublic, and any other value refuses
// the start naming the variable. TestEverySettingReachesItsReader runs it as the row's case.
func outboxDepsTakeTheMode(t *testing.T) {
	t.Helper()
	for name, test := range map[string]struct {
		overrides map[string]string
		want      roles.Mode
	}{
		"true":  {map[string]string{"DISPATCH_PROJECT_PRIVACY_REQUIRED": " true "}, roles.Required},
		"unset": {nil, roles.AllPublic},
		"blank": {map[string]string{"DISPATCH_PROJECT_PRIVACY_REQUIRED": " "}, roles.AllPublic},
	} {
		deps := outboxDeps(resolveWith(t, test.overrides), nil, nil, nil, nil, nil, nil, nil)
		if got := deps.RoleClaims.Mode(); got != test.want {
			t.Errorf("DISPATCH_PROJECT_PRIVACY_REQUIRED %s: the outbox's role reader resolves in mode %d, want %d", name, got, test.want)
		}
	}
	for _, value := range []string{"yes", "1", "TRUE", "false"} {
		refusedWith(t, map[string]string{"DISPATCH_PROJECT_PRIVACY_REQUIRED": value}, "DISPATCH_PROJECT_PRIVACY_REQUIRED")
	}
}

// The sessions the rows below name: S, whose token lists no project; one whose token lists PRIV
// alone; and one whose token lists another project than PUB.
const (
	sessionS        = "ses_outbox_s"
	sessionPRIVOnly = "ses_outbox_priv_only"
	sessionOther    = "ses_outbox_other"
)

// outboxRig is a test hub's role and session buckets, as a listener writes them, and Dispatch's
// test database, with PUB public and PRIV private in its projects rows.
type outboxRig struct {
	natsURL  string
	database *store.Store
	roles    nats.KeyValue
	sessions nats.KeyValue
}

func newOutboxRig(t *testing.T) *outboxRig {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee, visibility) values
			('PUB', 'Public', 'alice', 'public'),
			('PRIV', 'Private', 'alice', 'private')`); err != nil {
		t.Fatalf("seed PUB public and PRIV private: %v", err)
	}
	_, url := testnats.Start(t)
	conn := testnats.Connect(t, url)
	// The buckets as a listener opens them, which is where the claims and sessions the outbox
	// reads are written.
	registry, err := envoystore.Open(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionTTL(2*time.Minute))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	rig := &outboxRig{natsURL: url, database: database}
	if rig.roles, err = js.KeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	if rig.sessions, err = js.KeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	return rig
}

// deps is the role reader of the outbox's Deps as main builds them (outboxDeps) for a process
// started with environment, reading each project's privacy through pool.
func (r *outboxRig) deps(t *testing.T, environment map[string]string, pool *store.Pool) (roles.ClaimStore, roles.SessionLookup) {
	t.Helper()
	client, err := bus.Connect([]string{r.natsURL})
	if err != nil {
		t.Fatalf("connect the outbox's bus client: %v", err)
	}
	t.Cleanup(client.Close)
	deps := outboxDeps(resolveWith(t, environment), pool, r.database.Pool, client, events.NewBroker(), nil, nil, nil)
	return deps.RoleClaims, deps.Sessions
}

// claim writes holder's claim of role under project, on the key a listener writes it under
// (contracts.RoleClaimKey).
func (r *outboxRig) claim(t *testing.T, project, role, holder string) {
	t.Helper()
	record, err := json.Marshal(envoystore.RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode %s's claim of %s: %v", holder, role, err)
	}
	if _, err := r.roles.Put(contracts.RoleClaimKey(project, role), record); err != nil {
		t.Fatalf("store %s's claim of %s under %s: %v", holder, role, project, err)
	}
}

// register writes holder's session record, registered now, a full member's whose token lists
// projects.
func (r *outboxRig) register(t *testing.T, holder string, projects ...string) {
	t.Helper()
	r.registerAs(t, holder, false, projects...)
}

// registerAs writes holder's session record, registered now, its token listing projects and its
// person restricted or a full member: a record a listener of this build writes, naming its key and
// the listener (session.SessionEntry.BoundTo), which the required mode reads a scope from.
func (r *outboxRig) registerAs(t *testing.T, holder string, restricted bool, projects ...string) {
	t.Helper()
	record, err := json.Marshal(session.SessionEntry{SessionID: holder, MachineID: "test-machine", UpdatedAt: time.Now().UnixMilli(), Projects: append([]string{}, projects...), Restricted: new(restricted)})
	if err != nil {
		t.Fatalf("encode %s's record: %v", holder, err)
	}
	if _, err := r.sessions.Put(holder, record); err != nil {
		t.Fatalf("store %s's record: %v", holder, err)
	}
}

// setVisibility commits project's visibility in Dispatch's projects row.
func (r *outboxRig) setVisibility(t *testing.T, project, visibility string) {
	t.Helper()
	if _, err := r.database.Pool.Exec(t.Context(), `update projects set visibility = $2 where key = $1`, project, visibility); err != nil {
		t.Fatalf("make %s %s: %v", project, visibility, err)
	}
}

// privacyAnswer is a ClaimStore whose Private answers private for project, whatever Dispatch's
// rows hold: the controls' broken privacy read.
type privacyAnswer struct {
	roles.ClaimStore
	project string
	private bool
}

func (c privacyAnswer) Private(ctx context.Context, project string) (bool, error) {
	if project == c.project {
		return c.private, nil
	}
	return c.ClaimStore.Private(ctx, project)
}

// modeAnswer is a ClaimStore that resolves in mode, whatever the deployment set: the controls'
// holder check left out.
type modeAnswer struct {
	roles.ClaimStore
	mode roles.Mode
}

func (c modeAnswer) Mode() roles.Mode { return c.mode }

// fallbackRows are the rows of the role fallback from (P, reviewer) to the claim of reviewer under
// no project, which S holds on its bare key: an event of PRIV, private in Dispatch's rows, is
// resolved to no holder, so the outbox keeps its copy as for a role nobody holds and S receives
// nothing; so is an event of GONE, a key no projects row holds, which counts as private; the same
// event of PUB reaches S, so the fallback works where it should. Each returns what it found wrong.
func fallbackRows(ctx context.Context, claims roles.ClaimStore, sessions roles.SessionLookup) []string {
	var wrong []string
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PRIV", "reviewer"); err != nil || resolved.State != roles.Unclaimed {
		wrong = append(wrong, fmt.Sprintf("an event of PRIV routed to reviewer resolved holder %q (state %v), err %v; want no holder: PRIV is private, so the claim of reviewer under no project is not PRIV's", resolved.Holder, resolved.State, err))
	}
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "GONE", "reviewer"); err != nil || resolved.State != roles.Unclaimed {
		wrong = append(wrong, fmt.Sprintf("an event of GONE routed to reviewer resolved holder %q (state %v), err %v; want no holder: no projects row holds GONE, so it counts as private", resolved.Holder, resolved.State, err))
	}
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PUB", "reviewer"); err != nil || resolved.State != roles.Live || resolved.Holder != sessionS {
		wrong = append(wrong, fmt.Sprintf("an event of PUB routed to reviewer resolved holder %q (state %v), err %v; want %s, the holder of reviewer under no project", resolved.Holder, resolved.State, err, sessionS))
	}
	return wrong
}

// requiredRows are the rows of the required mode, each a holder whose token does not see the
// event's project: the outbox publishes no copy to it and keeps the event as for a role nobody
// holds. A restricted member's holder of planner under no project whose token lists PRIV alone,
// for an event of PUB; a stale claim of tester under PUB by a restricted member's session whose
// token lists another project, made in the all-public mode; and a claim of auditor under PRIV by
// S, a full member's session whose token lists no project, made while PRIV was public and held
// across PRIV's switch to private. Each returns what it found wrong.
func requiredRows(t *testing.T, rig *outboxRig, claims roles.ClaimStore, sessions roles.SessionLookup) []string {
	t.Helper()
	ctx := t.Context()
	var wrong []string
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PUB", "planner"); err != nil || resolved.State != roles.Unclaimed {
		wrong = append(wrong, fmt.Sprintf("an event of PUB routed to planner resolved holder %q (state %v), err %v; want no holder: %s is a restricted member whose token lists PRIV alone", resolved.Holder, resolved.State, err, sessionPRIVOnly))
	}
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PUB", "tester"); err != nil || resolved.State != roles.Unclaimed {
		wrong = append(wrong, fmt.Sprintf("an event of PUB routed to tester resolved holder %q (state %v), err %v; want no holder: %s is a restricted member whose token lists another project than PUB", resolved.Holder, resolved.State, err, sessionOther))
	}
	rig.setVisibility(t, "PRIV", "public")
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PRIV", "auditor"); err != nil || resolved.Holder != sessionS {
		t.Fatalf("while PRIV is public an event of PRIV routed to auditor resolved holder %q (state %v), err %v; want %s, a full member's session", resolved.Holder, resolved.State, err, sessionS)
	}
	rig.setVisibility(t, "PRIV", "private")
	if resolved, err := roles.ResolveHolder(ctx, claims, sessions, "PRIV", "auditor"); err != nil || resolved.State != roles.Unclaimed {
		wrong = append(wrong, fmt.Sprintf("after PRIV's switch to private an event of PRIV routed to auditor resolved holder %q (state %v), err %v; want no holder: %s's token lists no project", resolved.Holder, resolved.State, err, sessionS))
	}
	return wrong
}

// TestTheOutboxWithholdsAPrivateProjectsRoleFromTheNoProjectHolder: the outbox resolves a role
// route's holder through the role reader main builds (outboxDeps), which reads each project's
// privacy from Dispatch's own committed rows. The claim of a free-form role under no project is
// not a private project's (fallbackRows), in either mode; a read of those rows that fails holds
// the event rather than send it to that claim's holder; and in the required mode a holder whose
// token does not see the event's project is not taken (requiredRows). Each part's control breaks
// the rule it holds and sees the rows name the holder that would then receive the event.
func TestTheOutboxWithholdsAPrivateProjectsRoleFromTheNoProjectHolder(t *testing.T) {
	rig := newOutboxRig(t)
	rig.claim(t, contracts.NoProject, "reviewer", sessionS)
	rig.register(t, sessionS)
	for name, environment := range map[string]map[string]string{
		"all-public": nil,
		"required":   {"DISPATCH_PROJECT_PRIVACY_REQUIRED": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			claims, sessions := rig.deps(t, environment, rig.database.Pool)
			for _, wrong := range fallbackRows(t.Context(), claims, sessions) {
				t.Error(wrong)
			}
			// Control: a privacy read that answers PRIV public, whatever Dispatch's rows hold.
			// PRIV's event then reaches S, and the rows name it.
			wrong := strings.Join(fallbackRows(t.Context(), privacyAnswer{ClaimStore: claims, project: "PRIV", private: false}, sessions), "\n")
			if !strings.Contains(wrong, "PRIV routed to reviewer resolved holder \""+sessionS+"\"") {
				t.Errorf("control: with PRIV read as public the rows found %q, want them to name %s receiving PRIV's event", wrong, sessionS)
			}
		})
	}

	t.Run("a privacy read that fails holds the event", func(t *testing.T) {
		closed, err := pgxpool.NewWithConfig(t.Context(), rig.database.Pool.Config().Copy())
		if err != nil {
			t.Fatalf("open a pool to close: %v", err)
		}
		closed.Close()
		claims, sessions := rig.deps(t, nil, store.NewPool(closed))
		resolved, err := roles.ResolveHolder(t.Context(), claims, sessions, "PRIV", "reviewer")
		var readErr *roles.ReadError
		if !errors.As(err, &readErr) || readErr.Step != roles.PrivacyRead || resolved.Holder != "" {
			t.Errorf("with Dispatch's rows unreadable an event of PRIV routed to reviewer resolved holder %q (state %v), err %v; want it held at the privacy read, sent to nobody", resolved.Holder, resolved.State, err)
		}
	})

	t.Run("required mode reads the holder", func(t *testing.T) {
		rig.claim(t, contracts.NoProject, "planner", sessionPRIVOnly)
		rig.registerAs(t, sessionPRIVOnly, true, "PRIV")
		rig.claim(t, "PUB", "tester", sessionOther)
		rig.registerAs(t, sessionOther, true, "OPS")
		rig.claim(t, "PRIV", "auditor", sessionS)
		claims, sessions := rig.deps(t, map[string]string{"DISPATCH_PROJECT_PRIVACY_REQUIRED": "true"}, rig.database.Pool)
		for _, wrong := range requiredRows(t, rig, claims, sessions) {
			t.Error(wrong)
		}
		// Control: today's resolution, the holder unread. Each holder is then taken, its copy
		// goes to its agent subject under the event's project and its relay withholds it, and
		// the rows name every one of them.
		wrong := strings.Join(requiredRows(t, rig, modeAnswer{ClaimStore: claims, mode: roles.AllPublic}, sessions), "\n")
		for _, holder := range []string{sessionPRIVOnly, sessionOther, sessionS} {
			if !strings.Contains(wrong, "resolved holder \""+holder+"\"") {
				t.Errorf("control: with the holder unread the rows found %q, want them to name %s", wrong, holder)
			}
		}
		// The member rule: the same planner holder as a full member's session listing PRIV is
		// taken for PUB's event, which a fresh read says is public; and a holder record with no
		// restricted claim is not, as a restricted member's.
		rig.register(t, sessionPRIVOnly, "PRIV")
		if resolved, err := roles.ResolveHolder(t.Context(), claims, sessions, "PUB", "planner"); err != nil || resolved.Holder != sessionPRIVOnly {
			t.Errorf("a full member's session listing PRIV, holding planner, resolved PUB's event to %q (state %v), err %v; want it taken", resolved.Holder, resolved.State, err)
		}
		record, err := json.Marshal(session.SessionEntry{SessionID: sessionPRIVOnly, MachineID: "test-machine", UpdatedAt: time.Now().UnixMilli(), Projects: []string{"PRIV"}})
		if err != nil {
			t.Fatalf("encode a record without the restricted claim: %v", err)
		}
		if _, err := rig.sessions.Put(sessionPRIVOnly, record); err != nil {
			t.Fatalf("store a record without the restricted claim: %v", err)
		}
		if resolved, err := roles.ResolveHolder(t.Context(), claims, sessions, "PUB", "planner"); err != nil || resolved.State != roles.Unclaimed {
			t.Errorf("a record without the restricted claim, listing PRIV, resolved PUB's event to %q (state %v), err %v; want no holder", resolved.Holder, resolved.State, err)
		}
	})
}
