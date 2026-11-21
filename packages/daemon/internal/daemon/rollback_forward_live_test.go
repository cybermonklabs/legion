//go:build e2e

// The two runs of the rollback proof that a build of this change makes, before and after the
// release before it runs on the same database (rollback_live_test.go, which says how
// scripts/e2e/rollback-notices.sh drives the four).
package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// rollbackLaunches is how many launches the seed writes of each claim after its first, each of
// which moves its generation and mints a new boot token, so the history of the tokens a claim's
// earlier launches minted holds rows for the rollback to meet.
const rollbackLaunches = 2

// rollbackSeededGeneration is the generation each claim stands at once the seed has launched it
// rollbackLaunches times after its first.
const rollbackSeededGeneration = 1 + rollbackLaunches

// The seed: a daemon of this change's release migrates the database the release before recorded the
// fixture in (TestARollbackFixtureIsRecordedByTheReleaseBeforeLive), so its migrations run over a
// claims table holding one claim in each state, reads each of those claims back, launches each
// twice more, and queues one of each notice that release adds beside a canary tick, through the
// outbox's own row constructor (record.NewOutboxRow) and enqueue, as every writer of those notices
// queues them. The tick is queued first, so a lane that stops at a row it cannot decode has run it
// already.
func TestARollbackSeedIsWrittenByTheNewerReleaseLive(t *testing.T) {
	p := newRollbackProof(t)
	ctx := context.Background()
	p.step("this release migrates the database the release before recorded", func() {
		before, err := p.st.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("read the schema version: %v", err)
		}
		applied, err := p.st.Migrate(ctx)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		version, err := p.st.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("read the schema version: %v", err)
		}
		p.note("from version %d, applied %d migrations; the schema is at version %d", before, applied, version)
		if applied == 0 {
			t.Fatalf("this release applied no migration over the release before's database")
		}
	})
	claims := map[claim.Token]supervise.Claim{}
	p.step("it reads back each claim the release before recorded, one in each state", func() {
		read, err := p.st.Claims(ctx)
		if err != nil {
			t.Fatalf("read the claims: %v", err)
		}
		for _, c := range read {
			claims[c.Token] = c
		}
		for _, c := range rollbackClaims {
			token := p.token(c)
			held, ok := claims[token]
			if !ok || held.State != c.state || held.Generation != 1 || held.Reach != (supervise.Reach{}) {
				t.Fatalf("%s reads back as %+v (found %t), want the %s claim at generation 1 with no reach record", token, held, ok, c.state)
			}
			p.note("%s: %s at generation %d, no reach record", token, held.State, held.Generation)
		}
	})
	p.step("two launches of each claim, and the boot tokens of the launches each left", func() {
		for _, c := range rollbackClaims {
			launched := claims[p.token(c)]
			for range rollbackLaunches {
				launched.Generation++
				launched.BootTokenHash = supervise.HashBootToken(fmt.Sprintf("%s-%d", launched.Token, launched.Generation))
				if err := p.st.PutClaim(ctx, launched); err != nil {
					t.Fatalf("launch %s at generation %d: %v", launched.Token, launched.Generation, err)
				}
			}
		}
		history := p.bootTokenHistory()
		p.note("the database keeps %d boot tokens of earlier launches", len(history))
		if len(history) != rollbackLaunches*len(rollbackClaims) {
			t.Fatalf("the database keeps %d boot tokens of earlier launches, want the %d the launches left", len(history), rollbackLaunches*len(rollbackClaims))
		}
	})
	p.step("a canary tick, then one of each notice of this change", func() {
		root, implementer := p.root(), p.token(rollbackClaim{role: claim.RoleImplementer})
		now := time.Now().UTC()
		queued := []struct {
			issue   string
			payload record.OutboxPayload
		}{
			{p.cfg.Project, record.ControllerNotice{Kind: record.TickNotice}},
			{p.cfg.Project, record.ControllerNotice{Kind: record.UnreachableClaimsNotice, Unreachable: []record.UnreachableClaim{
				{Claim: string(implementer), Issue: root, Role: claim.RoleImplementer, State: string(supervise.StateWorking), LastSeen: now.Add(-25 * time.Minute)},
			}}},
			{p.cfg.Project, record.ControllerNotice{Kind: record.EnvoyNoLiveSessionNotice,
				Reason: "Envoy's session list has held no live session for 20 minutes"}},
			{root, record.ControllerNotice{Kind: record.RelaunchReadyNotice, Role: claim.RoleImplementer, Claim: string(implementer),
				Reason: "the relaunch guard would now let it through"}},
			{root, record.ControllerNotice{Kind: record.RelaunchGuardHeldNotice, Role: claim.RoleImplementer, Claim: string(implementer),
				Reason: "the relaunch guard has refused it for 60 minutes; this is a case for a person"}},
			{root, record.ControllerNotice{Kind: record.RelaunchNotBackNotice, Role: claim.RoleImplementer, Claim: string(implementer),
				Reason: "it is still out of reach 20 minutes after its relaunch; this is a case for a person"}},
			{root, record.ControllerNotice{Kind: record.UnreachableExitNotice, Role: claim.RolePlanner, Claim: string(p.token(rollbackClaim{role: claim.RolePlanner})),
				Reason: "its process exits at its Envoy role claim; this is a case for a person"}},
			{root, record.ControllerNotice{Kind: record.WorkerLockedOutNotice, Role: claim.RoleImplementer, Claim: string(implementer),
				Reason: "generation 3 has been refused the worker stream for 5 minutes; generation 2 is still connected"}},
			{root, record.Notice{Kind: record.ClaimOutOfReachNotice, Role: claim.RoleImplementer, Claim: string(implementer),
				Reason: "its session has not registered with Envoy for more than 20 minutes; nothing is asked of you"}},
		}
		if err := p.st.Tx(ctx, func(tx pgx.Tx) error {
			for _, q := range queued {
				row, err := record.NewOutboxRow(q.issue, q.payload, now)
				if err != nil {
					return err
				}
				if err := p.records.Enqueue(ctx, tx, row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("queue the notices: %v", err)
		}
		for _, row := range p.rows(append([]string{rollbackCanaryKind}, rollbackNoticeKinds...)) {
			p.note("row %d: kind %s, notice %s", row.id, row.kind, row.noticeKind)
		}
	})
}

// The roll-forward: once the release before has run on the database and stopped, a daemon of this
// change's release starts on it and delivers each notice the rollback left, once: the controller's
// on the controller's topic and the architect's on the architect's, each under its own row's key,
// and each row finishes. Before it boots, it holds that each launch the release before made during
// the rollback kept the boot token of the launch it replaced, the seed's last
// (rollbackSeededGeneration): that release names claim_boot_tokens in no statement, so only the
// database's own trigger can have kept it, and without it a shim of that generation is a token no
// launch minted. It prints the claims of each state and the boot tokens of earlier launches as the
// database holds them after the release before ran its launches.
func TestARolledForwardDaemonDeliversTheNoticesTheRollbackLeftLive(t *testing.T) {
	p := newRollbackProof(t)
	var left []rollbackRow
	p.step("the rollback left each notice of this change in the outbox", func() {
		left = p.rows(rollbackNoticeKinds)
		for _, row := range left {
			p.note("row %d: kind %s, notice %s, %d attempts, last error %q", row.id, row.kind, row.noticeKind, row.attempts, row.lastError)
		}
		if len(left) != len(rollbackNoticeKinds) {
			t.Fatalf("the outbox holds %d of the %d notices the seed queued", len(left), len(rollbackNoticeKinds))
		}
	})
	p.step("each launch the release before made kept the boot token of the launch it replaced", func() {
		ctx := context.Background()
		moved := 0
		for _, c := range rollbackClaims {
			token := p.token(c)
			var generation int64
			if err := p.st.Pool().QueryRow(ctx, "select generation from claims where token = $1", string(token)).Scan(&generation); err != nil {
				t.Fatalf("read the generation of %s: %v", token, err)
			}
			if generation <= rollbackSeededGeneration {
				p.note("%s: at generation %d, which the release before did not launch", token, generation)
				continue
			}
			moved++
			var kept bool
			if err := p.st.Pool().QueryRow(ctx, `select exists (select 1 from claim_boot_tokens
				where claim_token = $1 and generation = $2 and boot_token_hash = $3)`, string(token), rollbackSeededGeneration,
				supervise.HashBootToken(fmt.Sprintf("%s-%d", token, rollbackSeededGeneration))).Scan(&kept); err != nil {
				t.Fatalf("read the boot tokens %s keeps: %v", token, err)
			}
			p.note("%s: launched to generation %d by the release before; generation %d's boot token kept: %t", token, generation, rollbackSeededGeneration, kept)
			if !kept {
				t.Errorf("%s was launched to generation %d by the release before, and the boot token of generation %d, which that launch replaced, is not kept",
					token, generation, rollbackSeededGeneration)
			}
		}
		// A rollback that launched nothing has shown nothing about what its launches keep.
		if moved == 0 {
			t.Fatalf("the release before launched none of the %d claims past generation %d, so this step saw no launch of its own", len(rollbackClaims), rollbackSeededGeneration)
		}
	})
	p.step("this release's daemon starts on the same database", func() {
		p.boot()
		p.note("version %d of the schema", p.d.state().Daemon.SchemaVersion)
	})
	p.step("it delivers each of them once and finishes its row", func() {
		if !testwait.Holds(120*time.Second, 200*time.Millisecond, func() bool { return len(p.rows(rollbackNoticeKinds)) == 0 }) {
			t.Fatalf("notices still in the outbox: %+v", p.rows(rollbackNoticeKinds))
		}
		architect := notify.RoleTopicPrefix + string(p.token(rollbackClaim{role: claim.RoleArchitect}))
		implementer := string(p.token(rollbackClaim{role: claim.RoleImplementer}))
		controller, toArchitect := p.published(notify.ControllerTopic(p.project)), p.published(architect)
		for _, row := range left {
			topic, published := notify.ControllerTopic(p.project), controller
			if row.noticeKind == string(record.ClaimOutOfReachNotice) {
				topic, published = architect, toArchitect
			}
			var delivered []publishedNotice
			for _, n := range published {
				if n.Key == record.OutboxKey(row.id) {
					delivered = append(delivered, n)
				}
			}
			if len(delivered) != 1 || delivered[0].Kind != row.noticeKind {
				t.Errorf("row %d (notice %s) reached %s as %+v, want its notice once", row.id, row.noticeKind, topic, delivered)
				continue
			}
			p.note("row %d: %s delivered on %s, naming claim %q", row.id, delivered[0].Kind, topic, delivered[0].Claim)
		}
		if !slices.ContainsFunc(toArchitect, func(n publishedNotice) bool {
			return n.Kind == string(record.ClaimOutOfReachNotice) && n.Claim == implementer
		}) {
			t.Errorf("the architect was told %+v, want the notice naming %s", toArchitect, implementer)
		}
	})
	p.step("its log names no decode failure", func() {
		failures := p.logLines("decode outbox row")
		for _, line := range failures {
			p.note("log: %s", line)
		}
		if len(failures) != 0 {
			t.Errorf("the daemon's log names %d decode failures", len(failures))
		}
	})
	p.step("the claims of each state, and the boot tokens of earlier launches the database holds", func() {
		p.d.state()
		p.noteClaims()
		for _, kept := range p.bootTokenHistory() {
			p.note("kept: %s", kept)
		}
	})
	p.step("it stops", func() { p.d.stop() })
}

// bootTokenHistory is every boot token of an earlier launch the database keeps for the project's
// claims, as `<claim> generation <n>`, oldest first.
func (p *rollbackProof) bootTokenHistory() []string {
	p.t.Helper()
	rows, err := p.st.Pool().Query(context.Background(), `select b.claim_token, b.generation from claim_boot_tokens b
		join claims c on c.token = b.claim_token where c.project = $1 order by b.claim_token, b.generation`, p.project)
	if err != nil {
		p.t.Fatalf("read the boot tokens of earlier launches: %v", err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var token string
		var generation int64
		if err := rows.Scan(&token, &generation); err != nil {
			p.t.Fatalf("read the boot tokens of earlier launches: %v", err)
		}
		kept = append(kept, fmt.Sprintf("%s generation %d", token, generation))
	}
	if err := rows.Err(); err != nil {
		p.t.Fatalf("read the boot tokens of earlier launches: %v", err)
	}
	return kept
}
