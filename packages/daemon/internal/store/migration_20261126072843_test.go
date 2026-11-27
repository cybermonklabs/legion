package store

import (
	"context"
	"reflect"
	"testing"
)

// signalSettingsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const signalSettingsVersion = 20261126072843

// signalSettingsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const signalSettingsRelease = "the release that keeps each project's settings of admission by signals and its record of changes"

// 20261126072843 adds signal_settings, signal_changes and signal_change_ids. The switch is 1 or 0
// and nothing else, a null among them, and so is each entry of a hold and of the switch, while a
// limit a person cleared and a value the daemon cannot compute are null. The provider's one part
// is its headroom: a limit or an entry of its refusals is refused. Its way back drops the three
// tables and deletes this version row, so a roll-forward runs the file again.
func TestTheSignalSettingsArriveWith20261126072843AndTheirFlagsAreOneOrZero(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, signalSettingsVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name in ('signal_settings', 'signal_changes', 'signal_change_ids')`
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the signal settings: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the settings' tables after the migration = %v, want the three", got)
	}
	const setting = `insert into signal_settings (project, setting, value, set_at, by_kind, by_session, reason)
		values ('LEGION', $1, $2, '2026-11-26T10:00:00Z', 'operator', '', 'the owner said so')`
	const entry = `insert into signal_changes (project, id, at, signal, part, what, value, absent, by_kind, by_session, reason)
		values ('LEGION', $1, '2026-11-26T10:00:00Z', $2, null, $3, $4, null, $5, $6, $7)`
	const providerValue = `insert into signal_changes (project, id, at, signal, part, what, value)
		values ('LEGION', $1, '2026-11-26T10:00:00Z', 'provider', $2, 'value', 0.4)`
	one, zero := 1.0, 0.0
	for _, valid := range []struct {
		name      string
		statement func() error
	}{
		{"the switch on", func() error { return exec(setting, "switch", one) }},
		{"the switch turned off", func() error {
			return exec(setting+" on conflict (project, setting) do update set value = excluded.value", "switch", zero)
		}},
		{"a limit cleared", func() error { return exec(setting, "idle", nil) }},
		{"a hold of 0", func() error { return exec(entry, 1, "idle", "holds", zero, nil, nil, nil) }},
		{"the switch's entry of 1", func() error { return exec(entry, 2, nil, "switch", one, "operator", "", "the owner said so") }},
		{"a cleared limit's entry", func() error { return exec(entry, 3, "queue", "limit", nil, "operator", "", "the owner said so") }},
		{"a limit of the provider's headroom", func() error { return exec(setting, "provider", 0.9) }},
		{"a value of the provider's headroom", func() error { return exec(providerValue, 6, "headroom") }},
	} {
		if err := valid.statement(); err != nil {
			t.Fatalf("%s was refused: %v", valid.name, err)
		}
	}
	for name, statement := range map[string]func() error{
		"a switch of no value":       func() error { return exec(setting, "switch", nil) },
		"a switch of 2":              func() error { return exec(setting, "switch", 2.0) },
		"a hold of no value":         func() error { return exec(entry, 4, "kills", "holds", nil, nil, nil, nil) },
		"the switch's entry of none": func() error { return exec(entry, 5, nil, "switch", nil, "operator", "", "the owner said so") },
		// The live daemon computes no series of the provider's refusals, so neither a limit nor an
		// entry of one is kept.
		"a limit of the provider's refusals": func() error { return exec(setting, "refusals", 2.0) },
		"a value of the provider's refusals": func() error { return exec(providerValue, 7, "refusals") },
	} {
		if err := statement(); err == nil {
			t.Errorf("%s was accepted, want it refused", name)
		}
	}

	if err := exec(wayBackOf(t, signalSettingsVersion, signalSettingsRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126072843`
	if got := append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the settings' tables and the version row = %v, want neither", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the settings' tables after the roll-forward = %v, want the three again", got)
	}
}
