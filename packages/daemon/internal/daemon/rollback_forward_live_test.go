//go:build e2e

// The two runs of the rollback proof that a build of this change makes, before and after the
// release before it runs on the same database (rollback_live_test.go, which says how
// scripts/e2e/rollback-notices.sh drives the four).
package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
// twice more, and queues a canary tick through the outbox's own row constructor
// (record.NewOutboxRow) and enqueue, for the release before to deliver.
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
	p.step("a canary tick", func() {
		row, err := record.NewOutboxRow(p.cfg.Project, record.ControllerNotice{Kind: record.TickNotice}, time.Now().UTC())
		if err != nil {
			t.Fatalf("build the canary tick: %v", err)
		}
		if err := p.st.Tx(ctx, func(tx pgx.Tx) error { return p.records.Enqueue(ctx, tx, row) }); err != nil {
			t.Fatalf("queue the canary tick: %v", err)
		}
		for _, row := range p.rows([]string{rollbackCanaryKind}) {
			p.note("row %d: kind %s, notice %s", row.id, row.kind, row.noticeKind)
		}
	})
}

// The roll-forward: once the release before has run on the database and stopped, a daemon of this
// change's release starts on it with no decode failure. Before it boots, it holds that each launch
// the release before made during the rollback kept the boot token of the launch it replaced, the
// seed's last (rollbackSeededGeneration): that release names claim_boot_tokens in no statement, so
// only the database's own trigger can have kept it, and without it a shim of that generation is a
// token no launch minted. It prints the claims of each state and the boot tokens of earlier
// launches as the database holds them after the release before ran its launches.
func TestARolledForwardDaemonDeliversTheNoticesTheRollbackLeftLive(t *testing.T) {
	p := newRollbackProof(t)
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
