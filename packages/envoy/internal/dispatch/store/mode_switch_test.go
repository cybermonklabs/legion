package store

import (
	"context"
	"crypto/sha256"
	"os"
	"slices"
	"strings"
	"testing"
)

// dispatchRunbook is the runbook page that gives the recording-again of a broker revocation settled
// by itself, which an operator runs by hand.
const dispatchRunbook = "../../../../../docs/site/src/content/docs/dispatch/running-dispatch.md"

// restoreRunbook is the runbook page that gives the statements a deployment moving between the
// Cognito pool and a domain runs at the cutover, and the reset after a Dispatch database restored or
// made again alone.
const restoreRunbook = "../../../../../docs/site/src/content/docs/dispatch/after-a-restore.md"

// sqlBlockAfter is the first SQL block the document at path gives after the line starting with
// lead (its leading spaces aside, since a list item indents its lines): a ```sql fence, or a psql
// heredoc from a line ending in <<'SQL' to the line SQL. It answers the block's statements, each
// ending at a line that ends in a semicolon, its lines trimmed and joined by newlines, without the
// semicolon.
func sqlBlockAfter(t *testing.T, path, lead string) []string {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(text), "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), lead) })
	if start < 0 {
		t.Fatalf("%s has no line starting %q", path, lead)
	}
	var statements, statement []string
	end := ""
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		switch {
		case end == "" && line == "```sql":
			end = "```"
		case end == "" && strings.HasSuffix(line, "<<'SQL'"):
			end = "SQL"
		case end != "" && line == end:
			if len(statement) > 0 {
				t.Fatalf("%s's block after %q ends inside a statement: %q", path, lead, statement)
			}
			return statements
		case end != "" && line != "":
			statement = append(statement, line)
			if strings.HasSuffix(line, ";") {
				statements = append(statements, strings.TrimSuffix(strings.Join(statement, "\n"), ";"))
				statement = nil
			}
		}
	}
	t.Fatalf("%s gives no SQL block after %q", path, lead)
	return nil
}

// modeSwitchStatements is the SQL block the runbook gives after the paragraph starting with lead.
func modeSwitchStatements(t *testing.T, lead string) []string {
	t.Helper()
	return sqlBlockAfter(t, restoreRunbook, lead)
}

// mintToken stores a personal token of owner recording subject ("" for none), as the mint does
// now, and answers its id.
func mintToken(t *testing.T, pool *Pool, owner, name, subject string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(owner + "/" + name))
	var id string
	if err := pool.QueryRow(context.Background(), `insert into agent_tokens (owner, name, token_hash, prefix, subject)
		values ($1, $2, $3, 'test', nullif($4, '')) returning id::text`, owner, name, hash[:], subject).Scan(&id); err != nil {
		t.Fatalf("mint the token %q of %s: %v", name, owner, err)
	}
	return id
}

// tokenState is what the personal token id stores: the account it records ("" for none) and
// whether it is revoked. A revoked personal token is refused at admission, 401 TOKEN_REVOKED
// (api.personalTokenActor, held by api.TestAgentTokensArePrivateRevocableAndAttributeSessionWrites).
func tokenState(t *testing.T, pool *Pool, id string) (subject string, revoked bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select coalesce(subject, ''), revoked_at is not null from agent_tokens where id = $1::uuid`,
		id).Scan(&subject, &revoked); err != nil {
		t.Fatalf("read the token %s: %v", id, err)
	}
	return subject, revoked
}

// TestTheModeSwitchRunsOnTheGuardedSchema runs the runbook's cutover statements, in both
// directions, read from the runbook, against a database migrated through 20261118041916, each
// statement on its own as the runbook says: each passes, every session ends (the generation moves
// by one), the refresh tokens the step names are cleared, and every recorded Google account stays.
// On the return to a domain, a personal token minted under the pool, which records no account, is
// revoked, while a token recording the account the address kept keeps that account and stays
// unrevoked.
//
// Controls, in the test: the runbook's earlier switch, which cleared people.sub in the same
// statement as the session step, is refused with SQLSTATE 23514, and the refusal undoes its session
// step too, so a cookie issued before the switch would still act; and, on a database of its own,
// the return's step without its token statement leaves the pool-era token unrevoked.
func TestTheModeSwitchRunsOnTheGuardedSchema(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	exec := func(statement string, args ...any) error {
		_, err := database.Pool.Exec(ctx, statement, args...)
		return err
	}
	mustExec := func(statement string, args ...any) {
		t.Helper()
		if err := exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	type person struct {
		sub, refreshToken string
		generation        int64
	}
	read := func(email string) person {
		t.Helper()
		var got person
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(p.sub, ''), coalesce(p.refresh_token, ''), coalesce(s.generation, -1)
			from people p left join user_sessions s on s.login = p.email where p.email = $1
		`, email).Scan(&got.sub, &got.refreshToken, &got.generation); err != nil {
			t.Fatalf("read %s: %v", email, err)
		}
		return got
	}
	// seed writes a person as the deployment holds them before the cutover: an account (or none,
	// for ""), a refresh token and a session at generation.
	seed := func(email, sub, refreshToken string, generation int64) {
		t.Helper()
		if sub == "" {
			mustExec(`insert into people (email, refresh_token, confirmed_at) values ($1, $2, now())`, email, refreshToken)
		} else {
			mustExec(`insert into people (email, sub, refresh_token, confirmed_at) values ($1, $2, $3, now())`, email, sub, refreshToken)
		}
		mustExec(`insert into user_sessions (login, generation) values ($1, $2) on conflict (login) do update set generation = excluded.generation`, email, generation)
		if got := read(email).generation; got != generation {
			t.Fatalf("seed %s: generation %d, want %d", email, got, generation)
		}
	}
	// runStep runs, each on its own, the statements the runbook gives for direction, which start
	// as want does, one for one.
	runStep := func(direction string, statements []string, want ...string) {
		t.Helper()
		if len(statements) != len(want) {
			t.Fatalf("%s: the runbook gives %q, want %d statements starting %q", direction, statements, len(want), want)
		}
		for i, statement := range statements {
			if !strings.HasPrefix(statement, want[i]) {
				t.Fatalf("%s: the runbook's statement %d is %q, want one starting %q", direction, i+1, statement, want[i])
			}
			if err := exec(statement); err != nil {
				t.Fatalf("%s: the runbook's %q answered %v, want it to pass on the guarded schema", direction, statement, err)
			}
		}
	}
	const peopleStep, sessionStep, tokenStep = "update people set", "update user_sessions set", "update agent_tokens set"

	// From the Cognito pool to a domain. A sealed value (v1:) is a domain's, which the first
	// direction leaves in place.
	const sealed = "v1:sealed"
	seed("pool@x.example", "", "pool-token", 0)
	seed("sealed@x.example", "", sealed, 0)
	seed("back@x.example", "account-a", "pool-token-2", 2)
	// back@x.example recorded account-a in an earlier time in domain mode; under the pool its
	// person minted a token recording no account, and one recording account-a is from before.
	poolEra := mintToken(t, database.Pool, "back@x.example", "pool-era", "")
	recorded := mintToken(t, database.Pool, "back@x.example", "recording-a", "account-a")
	for name, id := range map[string]string{"pool-era": poolEra, "recording-a": recorded} {
		if _, revoked := tokenState(t, database.Pool, id); revoked {
			t.Fatalf("before the return, the token %s of back@x.example is revoked; want it live", name)
		}
	}
	runStep("pool to domain", modeSwitchStatements(t, "From the Cognito pool to a domain"), peopleStep, sessionStep, tokenStep)
	for email, want := range map[string]person{
		"pool@x.example":   {sub: "", refreshToken: "", generation: 1},
		"sealed@x.example": {sub: "", refreshToken: sealed, generation: 1},
		"back@x.example":   {sub: "account-a", refreshToken: "", generation: 3},
	} {
		if got := read(email); got != want {
			t.Errorf("pool to domain: %s is %+v, want %+v", email, got, want)
		}
	}
	if _, revoked := tokenState(t, database.Pool, poolEra); !revoked {
		t.Errorf("pool to domain: the token minted under the pool, recording no account, is not revoked")
	}
	if subject, revoked := tokenState(t, database.Pool, recorded); subject != "account-a" || revoked {
		t.Errorf("pool to domain: the token recording account-a records %q and is revoked %t; want account-a, not revoked", subject, revoked)
	}

	// From a domain back to the Cognito pool.
	seed("domain@x.example", "account-b", "v1:google", 5)
	runStep("domain to pool", modeSwitchStatements(t, "From a domain back to the Cognito pool"), peopleStep, sessionStep)
	for email, want := range map[string]person{
		"domain@x.example": {sub: "account-b", refreshToken: "", generation: 6},
		"back@x.example":   {sub: "account-a", refreshToken: "", generation: 4},
		"sealed@x.example": {sub: "", refreshToken: "", generation: 2},
	} {
		if got := read(email); got != want {
			t.Errorf("domain to pool: %s is %+v, want %+v", email, got, want)
		}
	}

	// Control: the earlier runbook's switch, one statement clearing sub with the session step.
	if err := exec(`update people set refresh_token = null, confirmed_at = null, sub = null; update user_sessions set generation = generation + 1`); !isSQLState(err, "23514") {
		t.Errorf("control: the earlier switch answered %v, want it refused with SQLSTATE 23514", err)
	}
	if got := read("domain@x.example").generation; got != 6 {
		t.Errorf("control: after the refused switch, the generation is %d, want 6: the refusal undoes its session step", got)
	}

	// Control: on a database of its own, the return's step without its token statement leaves the
	// token minted under the pool unrevoked.
	control := migratedEmptyStore(t)
	if _, err := control.Pool.Exec(ctx, `insert into people (email, sub, refresh_token, confirmed_at) values ('back@x.example', 'account-a', 'pool-token-2', now())`); err != nil {
		t.Fatalf("control: seed back@x.example: %v", err)
	}
	controlToken := mintToken(t, control.Pool, "back@x.example", "pool-era", "")
	for _, statement := range modeSwitchStatements(t, "From the Cognito pool to a domain") {
		if strings.HasPrefix(statement, tokenStep) {
			continue
		}
		if _, err := control.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("control: %q: %v", statement, err)
		}
	}
	if _, revoked := tokenState(t, control.Pool, controlToken); revoked {
		t.Errorf("control: without the token statement, the token minted under the pool is revoked; want it unrevoked, so the statement is what revokes it")
	}
}
