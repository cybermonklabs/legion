package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// hookPrompts is every role message a hook firing wrote, oldest first, decoded as the outbox
// runner decodes one before it publishes it (record.HookPrompt).
func hookPrompts(t *testing.T, pool *pgxpool.Pool) []record.HookPrompt {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select payload from outbox where kind = 'hook_prompt' order by id")
	if err != nil {
		t.Fatalf("read the hook prompts: %v", err)
	}
	defer rows.Close()
	prompts := []record.HookPrompt{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a hook prompt: %v", err)
		}
		prompt := record.HookPrompt{}
		if err := json.Unmarshal(payload, &prompt); err != nil {
			t.Fatalf("decode the hook prompt %s: %v", payload, err)
		}
		prompts = append(prompts, prompt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the hook prompts: %v", err)
	}
	return prompts
}

// firingMessages is every message a firing posted on the issue, oldest first: the one line a
// reader of a fleet greps the firings out by (fireHook).
func firingMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	posted := []string{}
	for _, body := range messageBodies(t, pool) {
		if strings.HasPrefix(body, "Legion hook ") {
			posted = append(posted, body)
		}
	}
	return posted
}

// promptRoles is the roles the firing's role messages are addressed to, in the order they were
// written.
func promptRoles(prompts []record.HookPrompt) []string {
	roles := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		roles = append(roles, string(prompt.Role))
	}
	return roles
}

// firingPrompt is the words of one hook of these proofs, written out so that a message can be
// read against the hook that fired it rather than against the words Legion ships
// (config.DefaultWorkBackPrompt).
const (
	readTheThreads = "Read the review threads of this pull request again, and say what the round keeps missing."
	saySoFar       = "Say what you have done so far and what is in your way."
)

// A firing reaches each role the hook names, and says the same thing on the issue.
//
// The four cases hold the whole of what a firing writes: one role message for each role the hook
// names, addressed to that role and carrying the hook's own words; one message on the issue
// naming the hook, the move, the count and the roles it reached; the pull request named where the
// issue has one and left out where it has none; and the words of each message the hook's own,
// never Legion's shipped default.
func TestAFiringReachesEachRoleItNames(t *testing.T) {
	// The hook of the first three cases: one role named twice, which reaches that role once, and
	// an every of 1, so the first move back of the issue fires it.
	oneFiring := Hook{Name: "work-back", On: HookEventWorkBack, Every: 1,
		Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleImplementer},
		Prompt: saySoFar}

	t.Run("one role message for each role the hook names, and one message on the issue", func(t *testing.T) {
		pool := migratedPool(t)
		engine := workBackEngine(50, oneFiring)
		seedWorkBackIssue(t, pool, "h0")

		testerFails(t, pool, engine, "1")

		prompts := hookPrompts(t, pool)
		if want := []string{"implementer", "tester", "reviewer"}; !sameStrings(promptRoles(prompts), want) {
			t.Fatalf("the firing addressed %v, want %v: one message for each role the hook names, in the hook's own order, and the role it names twice reached once",
				promptRoles(prompts), want)
		}
		for _, prompt := range prompts {
			if prompt.Hook != "work-back" || prompt.From != phase.Testing || prompt.To != phase.Implementing ||
				prompt.Count != 1 || prompt.PullRequest != "sjawhar/legion#42" || prompt.Prompt != saySoFar {
				t.Fatalf("the %s's message = %+v, want the work-back hook's firing at 1 move back from testing to implementing, naming pull request sjawhar/legion#42 and carrying the hook's own words",
					prompt.Role, prompt)
			}
			if count := prompt.Counts["testing-implementing"]; count != 1 || len(prompt.Counts) != 1 {
				t.Fatalf("the %s's message carries the counts %v, want the one move back the issue has made, under testing-implementing", prompt.Role, prompt.Counts)
			}
		}
		want := "Legion hook work-back fired at 1 moves back: testing to implementing. Prompted: implementer, tester, reviewer."
		if posted := firingMessages(t, pool); !sameStrings(posted, []string{want}) {
			t.Fatalf("the firing posted %q on the issue, want the one message %q", posted, want)
		}
	})

	t.Run("an issue whose reviewer has never started", func(t *testing.T) {
		pool := migratedPool(t)
		engine := workBackEngine(50, oneFiring)
		seedWorkBackIssue(t, pool, "h0")

		// The move is the tester's, so no fact of this run writes the reviewer a phases row: the
		// reviewer has run no phase, holds no claim and no session, and the hook names it all the
		// same.
		testerFails(t, pool, engine, "1")

		var reviewerRows int
		if err := pool.QueryRow(context.Background(),
			"select count(*) from phases where issue = 'LEGION-208' and role = 'reviewer'").Scan(&reviewerRows); err != nil {
			t.Fatalf("count the reviewer's phase rows: %v", err)
		}
		if reviewerRows != 0 {
			t.Fatalf("the issue holds %d reviewer phase rows, want none: this case is of an issue whose reviewer has never started", reviewerRows)
		}
		prompts := hookPrompts(t, pool)
		if want := []string{"implementer", "tester", "reviewer"}; !sameStrings(promptRoles(prompts), want) {
			t.Fatalf("the firing addressed %v, want %v: the firing addresses the roles the hook names, and the outbox decides later what it can deliver",
				promptRoles(prompts), want)
		}
		want := "Legion hook work-back fired at 1 moves back: testing to implementing. Prompted: implementer, tester, reviewer."
		if posted := firingMessages(t, pool); !sameStrings(posted, []string{want}) {
			t.Fatalf("the firing posted %q on the issue, want the one message %q, naming the three roles the hook names", posted, want)
		}
	})

	t.Run("an issue with no pull request recorded", func(t *testing.T) {
		pool := migratedPool(t)
		engine := workBackEngine(50, oneFiring)
		// No pull request is recorded: a hook can fire on a move made before the implementer has
		// opened one.
		standsIn(t, pool, phase.Implementing, "in_progress")
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})

		architectMovesBack(t, pool, engine, phase.Planning)

		prompts := hookPrompts(t, pool)
		if len(prompts) != 3 {
			t.Fatalf("the firing wrote %d role messages, want the 3 roles the hook names", len(prompts))
		}
		for _, prompt := range prompts {
			if prompt.PullRequest != "" {
				t.Fatalf("the %s's message names pull request %q, want none: the issue has no pull request recorded", prompt.Role, prompt.PullRequest)
			}
			if message := prompt.Message("LEGION-208"); strings.Contains(message, "Pull request") {
				t.Fatalf("the %s reads %q, want no pull-request sentence in it", prompt.Role, message)
			}
		}
	})

	t.Run("the words of each message are its own hook's", func(t *testing.T) {
		pool := migratedPool(t)
		// Two hooks fire on the one move, each with words of its own, so a message is read
		// against the hook that wrote it and not against the words Legion ships.
		engine := workBackEngine(50,
			Hook{Name: "work-back", On: HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleImplementer}, Prompt: saySoFar},
			Hook{Name: "review-loop", On: HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleReviewer}, Prompt: readTheThreads})
		seedWorkBackIssue(t, pool, "h0")

		testerFails(t, pool, engine, "1")

		prompts := hookPrompts(t, pool)
		if len(prompts) != 2 {
			t.Fatalf("the two hooks wrote %d role messages, want one each: %+v", len(prompts), prompts)
		}
		for _, prompt := range prompts {
			words := map[string]string{"work-back": saySoFar, "review-loop": readTheThreads}[prompt.Hook]
			if words == "" {
				t.Fatalf("a message names hook %q, which no hook of this engine is called", prompt.Hook)
			}
			if prompt.Prompt != words {
				t.Fatalf("the %s's message from hook %s carries %q, want its own hook's words %q", prompt.Role, prompt.Hook, prompt.Prompt, words)
			}
			message := prompt.Message("LEGION-208")
			if !strings.HasSuffix(message, "\n\n"+words) {
				t.Fatalf("the %s reads %q, want it to end with a blank line and its own hook's words %q", prompt.Role, message, words)
			}
			if strings.Contains(message, config.DefaultWorkBackPrompt) {
				t.Fatalf("the %s reads %q, want the words of hook %s and not the ones Legion ships with no hooks configured", prompt.Role, message, prompt.Hook)
			}
		}
		if hooks := []string{prompts[0].Hook, prompts[1].Hook}; !slices.Contains(hooks, "work-back") || !slices.Contains(hooks, "review-loop") {
			t.Fatalf("the messages came from hooks %v, want one from each of the two hooks that fired", hooks)
		}
	})
}
