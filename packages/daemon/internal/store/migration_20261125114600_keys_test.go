package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A key's agent that ran on a claim before the upgrade keeps that claim's name, and so the session
// the claim holds, however many later versions of its plan leave the key out. A claim the release
// before made records no agent of its session, and a claim that records none gives its session to
// the next start that reaches it (supervise.Claim.SessionOfAnotherAgent), so where nothing keeps
// the name for the key, a new agent given that name resumes the key's conversation. Version 1 of
// LEGION-208's plan, `implementer:a reviewer`, written by the release before, ran key a's agent on
// the claim implementer. Version 2, `implementer reviewer`, is named from version 1's rows, which
// name key a, so its implementer takes implementer-2, and its write replaces those rows. Version 3,
// `planner implementer reviewer`, finds no row of key a, and its implementer is a new agent: it
// takes implementer-2 again, never implementer, since 20261125114600 recorded key a on the claim
// implementer, from version 1's rows (record.KeySessionsOf). Version 4, `implementer:a reviewer`,
// gives key a implementer again. The fault: version 3's implementer took implementer.
//
// A start the release before queued for a key's first step makes that step's claim when it runs,
// after the upgrade. The migration writes nothing into that start; the daemon reads its agent, the
// key, from the steps (claim_agent_from_steps, which the migration defines and fills the claims
// with), and the claim it makes records the key, as the claim of a start of this release does.
//
// The claim of a name only an agent with no key held, reviewer, records that agent, the agent of
// step 2 (`step:2:t`), and the steps give that agent to a queued start of such a claim, which names
// none, so the next start of another agent that reaches the claim starts a session of its own
// (20261125114600's header).
func TestAKeysClaimFromBeforeTheUpgradeKeepsItsNameAfterVersionsLeaveTheKeyOut(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, claimSessionAgentVersion-1)
	records := record.NewStore()
	owner, err := claim.AgentToken("legion", "LEGION-208", claim.Agent(claim.RoleArchitect))
	if err != nil {
		t.Fatal(err)
	}
	// put writes version of issue's plan at generation 1, task t with words as its steps, each
	// `role` or `role:key`, named as every writer names them (record.NameAgents): from the issue's
	// rows and, where withSessions, the names whose claims hold a key's session, which the release
	// before had no record of. It answers the names.
	put := func(issue string, version int, withSessions bool, words ...string) []claim.Agent {
		t.Helper()
		var names []claim.Agent
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			steps := make([]record.PlanStep, len(words))
			for i, word := range words {
				role, key, _ := strings.Cut(word, ":")
				steps[i] = record.PlanStep{Issue: issue, Generation: 1, Task: "t", Position: i + 1, Role: role, Type: "implementation", AgentKey: key}
			}
			earlier, err := records.PlanStepsOf(ctx, tx, issue)
			if err != nil {
				return err
			}
			var sessions record.KeySessions
			if withSessions {
				claims, err := records.IssueClaims(ctx, tx, "legion", issue)
				if err != nil {
					return err
				}
				sessions = record.KeySessionsOf(claims)
			}
			steps = record.NameAgents(earlier, nil, sessions, steps)
			v := record.PlanVersion{Issue: issue, Generation: 1, Version: version, ArtifactID: "plan-artifact",
				Source: record.PlanSourceOwner, GivenBy: owner, GivenAt: time.Date(2026, 11, 22, 0, version, 0, 0, time.UTC)}
			task := record.PlanTask{Issue: issue, Generation: 1, Task: "t", PlanVersion: version, BlockText: "Build the widget.", State: record.TaskWaiting}
			if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
				return err
			}
			for _, step := range steps {
				names = append(names, step.Agent)
			}
			return nil
		}); err != nil {
			t.Fatalf("write version %d of %s's plan %v: %v", version, issue, words, err)
		}
		return names
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	// The release before: its writes name no column it does not know.
	if got := put("LEGION-208", 1, false, "implementer:a", "reviewer"); !slices.Equal(got, []claim.Agent{"implementer", "reviewer"}) {
		t.Fatalf("the names of version 1 = %q, want implementer and reviewer", got)
	}
	if got := put("LEGION-209", 1, false, "implementer:b", "reviewer"); !slices.Equal(got, []claim.Agent{"implementer", "reviewer"}) {
		t.Fatalf("the names of LEGION-209's version 1 = %q, want implementer and reviewer", got)
	}
	const releaseBeforesClaim = `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $2, $3, 1, $4, '', 'idle', 0, 0, 0, 0)`
	exec(releaseBeforesClaim, "legion-legion-legion-208-implementer", "LEGION-208", "implementer", "ses_key_a")
	exec(releaseBeforesClaim, "legion-legion-legion-208-reviewer", "LEGION-208", "reviewer", "ses_reviewer")
	startOf := func(token string) string {
		return `{"op":"start","tree":"LEGION-209","role":"` + strings.TrimPrefix(token, "legion-legion-legion-209-") +
			`","claim":"` + token + `","generation":1,"phase":"implementing","entry":1,"task":"Build it."}`
	}
	for _, token := range []string{"legion-legion-legion-209-implementer", "legion-legion-legion-209-reviewer"} {
		exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
			values ('supervise', 'LEGION-209', $1, 0, now(), '', '')`, startOf(token))
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate to this release: %v", err)
	}

	t.Run("key a keeps implementer", func(t *testing.T) {
		if got := put("LEGION-208", 2, true, "implementer", "reviewer"); !slices.Equal(got, []claim.Agent{"implementer-2", "reviewer"}) {
			t.Fatalf("the names of version 2 = %q, want implementer-2 and reviewer: version 1's rows name key a's implementer", got)
		}
		if got := put("LEGION-208", 3, true, "planner", "implementer", "reviewer"); !slices.Equal(got, []claim.Agent{"planner", "implementer-2", "reviewer"}) {
			t.Fatalf("the names of version 3 = %q, want planner, implementer-2 and reviewer: its implementer is a new agent, and "+
				"the claim implementer holds key a's session, which that claim keeps for key a's agent", got)
		}
		if got := rowsAsText(t, st, `select concat(token, '=', session_agent) from claims where issue = 'LEGION-208' order by token`); !slices.Equal(got,
			[]string{"legion-legion-legion-208-implementer=key:a", "legion-legion-legion-208-reviewer=step:2:t"}) {
			t.Fatalf("the claims of LEGION-208 = %q, want the implementer's recording key a and the reviewer's step 2's agent", got)
		}
		if got := put("LEGION-208", 4, true, "implementer:a", "reviewer"); !slices.Equal(got, []claim.Agent{"implementer", "reviewer"}) {
			t.Fatalf("the names of version 4 = %q, want implementer and reviewer: key a comes back to the claim of its session", got)
		}
	})

	t.Run("the steps give key b's queued start its agent", func(t *testing.T) {
		for token, want := range map[string]string{"legion-legion-legion-209-implementer": "key:b", "legion-legion-legion-209-reviewer": "step:2:t"} {
			payloads := rowsAsText(t, st, `select payload::text from outbox where kind = 'supervise' and payload->>'claim' = '`+token+`'`)
			if len(payloads) != 1 {
				t.Fatalf("the starts of %s = %q, want one", token, payloads)
			}
			decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKind("supervise"), Payload: []byte(payloads[0])})
			if err != nil {
				t.Fatalf("decode the start of %s: %v", token, err)
			}
			if got := string(decoded.(record.SuperviseRequest).Agent); got != "" {
				t.Errorf("the start of %s names the agent %q, want none: the migration writes no queued row", token, got)
			}
			if got := rowsAsText(t, st, `select coalesce(claim_agent_from_steps('LEGION-209', '`+token+`'), '')`); !slices.Equal(got, []string{want}) {
				t.Errorf("the agent the steps give %s = %q, want %q, which the start that makes the claim records", token, got, want)
			}
		}
	})
}

// 20261125114600 records on a claim from before it, where no start the release before kept
// (step_starts) names the agent that ran on it last, the agent the issue's plan rows give its name
// last: the agent of the latest row of the name in record.Postgres.PlanStepsOf's order, leaving
// out the owner's rows and rows that hold no name, `key:<key>` where that row names a key and
// `step:<place>:<task>` where it names none. Where that row names a key, it is the key
// record.NameAgents keeps the name for, so the names the rows alone give an issue's agents are the
// names its claims alone give them (record.KeySessionsOf), and a write that deletes the rows takes
// no name from a key. On LEGION-208, key a's generation 1 row and key b's later generation 2 row
// both hold implementer: the claim records key b, as NameAgents gives key b the name and key a
// implementer-2. On LEGION-212, a row with no key holds implementer and key e's later row holds it
// too: the claim records key e, since an earlier row with no key takes no name from a later key.
// The claim reviewer, a name only step 2's agent with no key held, records that agent.
//
// On LEGION-209, key c's row holds implementer and a later row with no key holds it too: the rows
// the release before left for `implementer:c` and then `implementer`, as it gave the name to the
// agent with no key after key c's, on the claim key c's agent ran on. With no start kept to say
// which ran, the claim records that agent with no key, step 1's of generation 2. There the record
// and NameAgents part: NameAgents still holds the name for key c while key c's row stands, as a
// row with no key does not end a key's hold, so the rows name a later version's implementer
// implementer-2, and the claims alone, which keep no name for key c, name it implementer.
//
// The migration writes no queued row: a daemon of the release before reads each one as that
// release wrote it, whatever project it serves. A start that makes its claim, LEGION-213's, takes
// its agent where the daemon reads it, from the same rule the migration fills the claims by
// (claim_agent_from_steps), so key f; a start of a claim that stands takes the session that claim
// records.
//
// The controls, each left as it was: the tree's architect, whose owner's rows hold no name;
// LEGION-210's tester, whose key's row holds no name, as the first form of 20261124213820 left
// every row; LEGION-211's implementer, of an issue with no rows, since an agent is its issue's own;
// and the suspend of LEGION-209's implementer, which is no start. The way back then drops the
// column and takes the agent off each start, and the roll forward records each agent again.
func TestTheSessionAgentMigrationRecordsTheAgentTheRowsGiveAClaimsNameLast(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, claimSessionAgentVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	seedPlanSteps(t, st, "LEGION-208", 1, [2]string{"implementer", "a"}, [2]string{"reviewer", ""}, [2]string{"acceptance", "owner"})
	seedPlanSteps(t, st, "LEGION-208", 2, [2]string{"implementer", "b"})
	seedPlanSteps(t, st, "LEGION-209", 1, [2]string{"implementer", "c"})
	seedPlanSteps(t, st, "LEGION-209", 2, [2]string{"implementer", ""})
	seedPlanSteps(t, st, "LEGION-212", 1, [2]string{"implementer", ""})
	seedPlanSteps(t, st, "LEGION-212", 2, [2]string{"implementer", "e"})
	seedPlanSteps(t, st, "LEGION-210", 1, [2]string{"tester", "d"})
	seedPlanSteps(t, st, "LEGION-213", 1, [2]string{"implementer", "f"})
	exec(`update plan_steps set agent = role where agent_key <> 'owner' and issue <> 'LEGION-210'`)
	const releaseBeforesClaim = `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $2, $3, 1, 'ses', '', 'idle', 0, 0, 0, 0)`
	for _, c := range [][3]string{
		{"legion-legion-legion-208-architect", "LEGION-208", "architect"},
		{"legion-legion-legion-208-implementer", "LEGION-208", "implementer"},
		{"legion-legion-legion-208-reviewer", "LEGION-208", "reviewer"},
		{"legion-legion-legion-209-implementer", "LEGION-209", "implementer"},
		{"legion-legion-legion-210-tester", "LEGION-210", "tester"},
		{"legion-legion-legion-211-implementer", "LEGION-211", "implementer"},
		{"legion-legion-legion-212-implementer", "LEGION-212", "implementer"},
	} {
		exec(releaseBeforesClaim, c[0], c[1], c[2])
	}
	// The queued rows of the release before: a start whose claim the start itself makes,
	// LEGION-213's, and starts and a suspend of claims that stand.
	const keyStart = `{"op":"start","tree":"LEGION-213","role":"implementer","claim":"legion-legion-legion-213-implementer","generation":1,"phase":"implementing","entry":1,"task":"Build it."}`
	for _, row := range [][2]string{
		{"LEGION-213", keyStart},
		{"LEGION-208", `{"op":"start","tree":"LEGION-208","role":"implementer","claim":"legion-legion-legion-208-implementer","generation":1,"phase":"implementing","entry":1,"task":"Build it."}`},
		{"LEGION-209", `{"op":"start","tree":"LEGION-209","role":"implementer","claim":"legion-legion-legion-209-implementer","generation":1,"phase":"implementing","entry":1,"task":"Build it."}`},
		{"LEGION-209", `{"op":"suspend","tree":"LEGION-209","role":"implementer","claim":"legion-legion-legion-209-implementer","generation":1}`},
		{"LEGION-208", `{"op":"start","tree":"LEGION-208","role":"reviewer","claim":"legion-legion-legion-208-reviewer","generation":1,"phase":"reviewing","entry":1,"task":"Review it."}`},
	} {
		exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
			values ('supervise', $1, $2, 0, now(), '', '')`, row[0], row[1])
	}
	wantClaims := []string{
		"legion-legion-legion-208-architect=", "legion-legion-legion-208-implementer=key:b", "legion-legion-legion-208-reviewer=step:2:a",
		"legion-legion-legion-209-implementer=step:1:a", "legion-legion-legion-210-tester=", "legion-legion-legion-211-implementer=",
		"legion-legion-legion-212-implementer=key:e",
	}
	payloads := rowsAsText(t, st, `select payload::text from outbox order by id`)
	recorded := func(when string) {
		t.Helper()
		if got := rowsAsText(t, st, `select concat(token, '=', session_agent) from claims order by token`); !slices.Equal(got, wantClaims) {
			t.Fatalf("the claims %s = %q, want %q", when, got, wantClaims)
		}
		if got := rowsAsText(t, st, `select payload::text from outbox order by id`); !slices.Equal(got, payloads) {
			t.Fatalf("the queued supervise rows %s = %q, want each as the release before wrote it, %q", when, got, payloads)
		}
		if got := rowsAsText(t, st, `select coalesce(claim_agent_from_steps('LEGION-213', 'legion-legion-legion-213-implementer'), '')`); !slices.Equal(got, []string{"key:f"}) {
			t.Fatalf("the agent the steps give LEGION-213's start %s = %q, want key:f", when, got)
		}
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the agent of a claim's session: %v", err)
	}
	recorded("after the migration")
	records := record.NewStore()
	for issue, want := range map[string]struct {
		words            []string
		byRows, byClaims []claim.Agent
	}{
		"LEGION-208": {[]string{"implementer:a", "implementer:b"}, []claim.Agent{"implementer-2", "implementer"}, []claim.Agent{"implementer-2", "implementer"}},
		"LEGION-209": {[]string{"implementer"}, []claim.Agent{"implementer-2"}, []claim.Agent{"implementer"}},
		"LEGION-212": {[]string{"implementer:e", "implementer"}, []claim.Agent{"implementer", "implementer-2"}, []claim.Agent{"implementer", "implementer-2"}},
	} {
		steps := make([]record.PlanStep, len(want.words))
		for i, word := range want.words {
			role, key, _ := strings.Cut(word, ":")
			steps[i] = record.PlanStep{Issue: issue, Generation: 3, Task: "t", Position: i + 1, Role: role, AgentKey: key}
		}
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			earlier, err := records.PlanStepsOf(ctx, tx, issue)
			if err != nil {
				return err
			}
			claims, err := records.IssueClaims(ctx, tx, "legion", issue)
			if err != nil {
				return err
			}
			var byRows, byClaims []claim.Agent
			for _, step := range record.NameAgents(earlier, nil, nil, steps) {
				byRows = append(byRows, step.Agent)
			}
			for _, step := range record.NameAgents(nil, nil, record.KeySessionsOf(claims), steps) {
				byClaims = append(byClaims, step.Agent)
			}
			if !slices.Equal(byRows, want.byRows) || !slices.Equal(byClaims, want.byClaims) {
				t.Errorf("%s's agents %v are named %q from its rows and %q from its claims, want %q and %q",
					issue, want.words, byRows, byClaims, want.byRows, want.byClaims)
			}
			return nil
		}); err != nil {
			t.Fatalf("read %s's rows and claims: %v", issue, err)
		}
	}

	// A start this release queues names its step's agent, a key the release before does not
	// decode: the way back takes that key off it and leaves every other key as it was.
	const agentStart = `{"op":"start","tree":"LEGION-208","role":"implementer","claim":"legion-legion-legion-208-implementer","generation":1,"phase":"implementing","entry":2,"task":"Build it.","agent":"key:b"}`
	var agentRow int64
	if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('supervise', 'LEGION-208', $1, 0, now(), '', '') returning id`, agentStart).Scan(&agentRow); err != nil {
		t.Fatalf("queue a start that names its agent: %v", err)
	}
	exec(wayBackOf(t, claimSessionAgentVersion, claimSessionAgentRelease))
	var stripped bool
	if err := st.Pool().QueryRow(ctx, `select payload = $1::jsonb - 'agent' from outbox where id = $2`, agentStart, agentRow).Scan(&stripped); err != nil {
		t.Fatalf("read the start that named its agent after the way back: %v", err)
	}
	if !stripped {
		t.Fatalf("the start that named its agent after the way back = %s, want it without the key agent and with every other key", agentStart)
	}
	exec(`delete from outbox where id = $1`, agentRow)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	recorded("after the roll forward")
}

// A new agent with no key that a later version gives the name of a claim from before the upgrade
// starts in a session of its own, even where every row that named the claim's agent is gone. The
// release before ran LEGION-208's generation 1, `implementer:a reviewer`, with key a's agent on the
// claim implementer, and its generation 2, `implementer reviewer`, gave the name implementer to the
// agent with no key of step 1, which ran on that claim after key a's agent. After the upgrade a new
// owner's run deletes both plans (workflow's passOverEarlierOwners), and a later version of its
// plan, `planner implementer reviewer`, given after that deletion, is named from the rows that
// stand then (workflow's versionRefusal reads PlanStepsOf), which hold no row of the name: it gives
// implementer to a new agent with no key, the agent of step 2, and no row and no claim holds the
// name for anyone. (The run's first step, planFirstStep, reads the rows before it deletes them, so
// its own steps would still find key a's row; the test drives the later version.) That agent's start
// must find the claim's session another agent's (supervise.Claim.SessionOfAnotherAgent), so it
// starts a session of its own rather than the conversation the claim holds. The control: the agent
// of generation 2's step 1, which the rows gave the name last, finds the session its own.
func TestANewAgentGivenTheNameOfAClaimFromBeforeTheUpgradeStartsASessionOfItsOwn(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, claimSessionAgentVersion-1)
	seedPlanSteps(t, st, "LEGION-208", 1, [2]string{"implementer", "a"}, [2]string{"reviewer", ""})
	seedPlanSteps(t, st, "LEGION-208", 2, [2]string{"implementer", ""}, [2]string{"reviewer", ""})
	for _, statement := range []string{
		`update plan_steps set agent = role`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('legion-legion-legion-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 2,
			'ses_implementer', '', 'idle', 0, 0, 0, 0)`,
	} {
		if _, err := st.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the agent of a claim's session: %v", err)
	}

	records := record.NewStore()
	var named []record.PlanStep
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, generation := range []uint64{1, 2} {
			if err := records.DeletePlan(ctx, tx, "LEGION-208", generation); err != nil {
				return err
			}
		}
		earlier, err := records.PlanStepsOf(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		claims, err := records.IssueClaims(ctx, tx, "legion", "LEGION-208")
		if err != nil {
			return err
		}
		named = record.NameAgents(earlier, nil, record.KeySessionsOf(claims), []record.PlanStep{
			{Issue: "LEGION-208", Generation: 3, Task: "a", Position: 1, Role: "planner"},
			{Issue: "LEGION-208", Generation: 3, Task: "a", Position: 2, Role: "implementer"},
			{Issue: "LEGION-208", Generation: 3, Task: "a", Position: 3, Role: "reviewer"},
		})
		return nil
	}); err != nil {
		t.Fatalf("delete the earlier owner's plans and name the new one: %v", err)
	}
	if named[1].Agent != "implementer" {
		t.Fatalf("the new plan's step 2 is named %q, want implementer, the name no row or claim holds now", named[1].Agent)
	}
	held := supervise.Claim{SessionAgent: sessionAgentOfToken(t, st, "legion-legion-legion-208-implementer")}
	if newcomer := named[1].PlanAgent().ID(); !held.SessionOfAnotherAgent(newcomer) {
		t.Errorf("the claim implementer records the agent %q of its session, so the start of %s, a new agent with no key, "+
			"takes that session, want it to start a session of its own", held.SessionAgent, newcomer)
	}
	if lastNamed := plan.AgentOf("a", 1, plan.Step{Role: "implementer"}).ID(); held.SessionOfAnotherAgent(lastNamed) {
		t.Errorf("the claim implementer records the agent %q of its session, so the start of %s, the agent the rows gave the "+
			"name last, leaves its own session, want it to keep it", held.SessionAgent, lastNamed)
	}
}

// sessionAgentOfToken is the agent whose session the stored claim token holds (claims.session_agent).
func sessionAgentOfToken(t *testing.T, st *Store, token string) plan.AgentID {
	t.Helper()
	got := rowsAsText(t, st, `select session_agent from claims where token = '`+token+`'`)
	if len(got) != 1 {
		t.Fatalf("the claims %s = %q, want one", token, got)
	}
	return plan.AgentID(got[0])
}

// A claim from before the upgrade records the agent that ran on it last, where the start the release
// before kept for it (step_starts: the claim's token, the place, the moment and the start's record,
// which names its generation) shows which, rather than the agent the plan's rows name last, which
// may not have started. On each issue the release before ran generation 1, `implementer:c`, with
// key c's agent on the claim implementer, and generation 2, `implementer`, which gave the name
// implementer to the agent with no key of step 1.
//
// On LEGION-301 that agent had not started at the upgrade: the claim's latest start is
// generation 1's, so the claim records key c. Key c, named again by a later version, resumes its
// own session, and the agent with no key starts a session of its own. The fault: the claim
// recorded the agent with no key (`step:1:a`), so key c's return started a fresh session and the
// agent with no key would have taken key c's conversation.
//
// The controls: on LEGION-302 the latest start is generation 2's, so the claim records the agent
// with no key; on LEGION-303 no start was kept, so the claim records the agent the rows name last,
// the agent with no key too; on LEGION-304 the claim's latest start is at a place whose row now
// holds another name, so that start names no agent the rows still show, and the claim records the
// agent the rows name last rather than an older start's.
func TestTheSessionAgentMigrationRecordsTheAgentThatRanLastOnAClaim(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, claimSessionAgentVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	started := func(issue string, place int, generation int, at string) {
		t.Helper()
		token := "legion-legion-" + strings.ToLower(issue) + "-implementer"
		exec(`insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
			values ($1, 'a', 'step', $2::int, 1, $3::text, $4::text::timestamptz, 'ended', jsonb_build_object('task', 'a', 'step', $2::int, 'kind', 'step', 'agent', $3::text, 'generation', $5::int))`,
			issue, place, token, at, generation)
	}
	for _, issue := range []string{"LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304"} {
		first := [][2]string{{"implementer", "c"}}
		if issue == "LEGION-304" {
			first = append(first, [2]string{"reviewer", ""})
		}
		seedPlanSteps(t, st, issue, 1, first...)
		seedPlanSteps(t, st, issue, 2, [2]string{"implementer", ""})
		exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $2, 'implementer', 2, 'ses', '', 'idle', 0, 0, 0, 0)`,
			"legion-legion-"+strings.ToLower(issue)+"-implementer", issue)
	}
	exec(`update plan_steps set agent = role`)
	started("LEGION-301", 1, 1, "2026-11-20T10:00:00Z")
	started("LEGION-302", 1, 2, "2026-11-20T10:00:00Z")
	started("LEGION-304", 1, 1, "2026-11-20T10:00:00Z")
	started("LEGION-304", 2, 1, "2026-11-20T11:00:00Z")

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the agent of a claim's session: %v", err)
	}
	want := []string{
		"legion-legion-legion-301-implementer=key:c", "legion-legion-legion-302-implementer=step:1:a",
		"legion-legion-legion-303-implementer=step:1:a", "legion-legion-legion-304-implementer=step:1:a",
	}
	if got := rowsAsText(t, st, `select concat(token, '=', session_agent) from claims order by token`); !slices.Equal(got, want) {
		t.Fatalf("the claims after the migration = %q, want %q", got, want)
	}
}
