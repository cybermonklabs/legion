package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// specGateExemptionVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const specGateExemptionVersion = 20261122104755

// beforeSpecGateExemption is the version the test migrates through before it seeds the rows the
// release before writes.
const beforeSpecGateExemption = specGateExemptionVersion - 1

// specGateExemptionRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const specGateExemptionRelease = "the release that opens the design gate of a change request that proposes nothing"

// 20261122104755 gives each design gate the rule it started under (design_gates.rule), the
// version that opened it with no person (design_gates.exempt_version), the version of the
// Verification document its change request was read at (design_gates.verification_version) and
// whether that read found a changed check no person approved (design_gates.checks_unapproved),
// keeps the daemon's last reading of each issue's ending condition (spec_endings), lets the outbox
// queue the re-read of that condition (spec_ending_read), and is read by the two notice kinds of
// this release. A gate the release before wrote reads back under the rule of a person's approval
// with no exemption, read at no Verification version and holding no check, and the release
// before's own gate write, which names none of the columns, still lands.
//
// The way back, the last paragraph of the migration's header (migrations.WayBack), deletes the
// rows the release before cannot decode — the queued re-reads and the queued notices of the two
// kinds, the architect's and the controller's — and leaves every other row; it drops what the
// migration added and deletes the migration's version row, so the roll forward runs it again.
func TestTheGateRuleAndTheEndingComeFromTheMigrationAndGoWithTheWayBack(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, beforeSpecGateExemption)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	root := fmt.Sprintf("%s-%d", "ACME", 41)
	queue := func(kind, payload string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, $2, $3::jsonb, 0, now(), '')`,
			kind, root, payload)
		return err
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'ACME', 'a root', 'merging', 1, 'in_progress', 'A', 1)`, root)
	// The release before's gate write names these columns and no other (record's PutGate).
	const releaseBeforesGate = `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened) values ($1, $2, $3, $4, null, false)`
	exec(releaseBeforesGate, root, "spec", "the-spec", 3)
	if err := queue("spec_ending_read", `{"head":"h1"}`); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a spec_ending_read row before the migration = %v, want the kind check to refuse it", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the spec gate exemption: %v", err)
	}

	const gates = `select row(step, rule, exempt_version, verification_version, checks_unapproved)::text from design_gates order by step`
	if got, want := rowsAsText(t, store, gates), []string{"(spec,approval,,0,f)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the gate the release before wrote = %v, want %v: the rule of a person's approval, no exemption, no Verification version, no check held", got, want)
	}
	exec(releaseBeforesGate, root, "criteria", "the-criteria", 1)
	exec(`update design_gates set rule = 'change_request', exempt_version = 3, verification_version = 2, checks_unapproved = true where step = 'spec'`)
	if got, want := rowsAsText(t, store, gates), []string{"(criteria,approval,,0,f)", "(spec,change_request,3,2,t)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the gates after this release's write = %v, want %v", got, want)
	}
	for _, refused := range []struct{ why, statement string }{
		{"a rule that is none", `update design_gates set rule = 'person' where step = 'spec'`},
		{"an exemption of version 0", `update design_gates set exempt_version = 0 where step = 'spec'`},
		{"a Verification version below 0", `update design_gates set verification_version = -1 where step = 'spec'`},
		{"an ending of a state Dispatch never answers", `insert into spec_endings (issue, head, state) values ('` + root + `', 'h1', 'maybe')`},
		{"an ending at no head", `insert into spec_endings (issue, head, state) values ('` + root + `', '', 'ready')`},
	} {
		if _, err := store.Pool().Exec(ctx, refused.statement); err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}
	exec(`insert into spec_endings (issue, head, state, told) values ($1, 'h1', 'waiting', '{no_conclusion}')`, root)
	for _, row := range []struct{ kind, payload string }{
		{"spec_ending_read", `{"head":"h1"}`},
		{"notice", `{"kind":"spec-ending-waits","reason":"no_conclusion"}`},
		{"notice", `{"kind":"spec-request-stale"}`},
		{"controller_notice", `{"kind":"spec-request-stale"}`},
		{"notice", `{"kind":"design-approved","version":3}`},
		{"role_message", `{"role":"r","body":"b"}`},
	} {
		if err := queue(row.kind, row.payload); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", row.kind, err)
		}
	}
	t.Logf("the migration gave the gate its rule and its exemption, holds the ending and takes the re-read")

	exec(wayBackOf(t, specGateExemptionVersion, specGateExemptionRelease))
	const left = `select row(kind, payload->>'kind')::text from outbox order by id`
	if got, want := rowsAsText(t, store, left), []string{"(notice,design-approved)", "(role_message,)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the way back = %v, want %v: the re-read and the two new notice kinds gone, every other row kept", got, want)
	}
	if got := rowsAsText(t, store, `select column_name::text from information_schema.columns where table_name = 'design_gates' and column_name in ('rule', 'exempt_version', 'verification_version', 'checks_unapproved')`); len(got) != 0 {
		t.Fatalf("the gate columns the way back leaves = %v, want none", got)
	}
	if got := rowsAsText(t, store, `select coalesce(to_regclass('public.spec_endings')::text, '')`); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("spec_endings after the way back = %v, want it dropped", got)
	}
	if got := rowsAsText(t, store, fmt.Sprintf(`select version::text from schema_version where version = %d`, specGateExemptionVersion)); len(got) != 0 {
		t.Fatalf("the version row after the way back = %v, want it deleted", got)
	}
	if err := queue("spec_ending_read", `{"head":"h1"}`); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a spec_ending_read row after the way back = %v, want the kind check to refuse it again", err)
	}
	other := fmt.Sprintf("%s-%d", "ACME", 42)
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'ACME', 'another root', 'admitted', 1, 'in_progress', 'B', 1)`, other)
	exec(releaseBeforesGate, other, "spec", "another-spec", 4)
	t.Logf("the way back deleted the rows the release before cannot decode and took back what the migration added")

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the roll forward applied %d migrations, want this one again", applied)
	}
	if got, want := rowsAsText(t, store, gates), []string{"(criteria,approval,,0,f)", "(spec,approval,,0,f)", "(spec,approval,,0,f)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the gates after the roll forward = %v, want %v: every gate under the rule of a person's approval", got, want)
	}
}
