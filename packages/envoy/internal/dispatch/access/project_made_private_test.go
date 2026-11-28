package access

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheMadePrivateTriggerIsTheBodyGoRenders: record_project_made_private, the function of the
// projects table's trigger, is projectMadePrivateFunctionBody as it renders now, word for word
// (whitespace aside), so what it records at a switch is written once, in Go; it runs as its owner
// (security definer), since it records reached projects of sessions the writer does not see, with
// search_path pinned to pg_catalog, public (plan D5); and its trigger,
// projects_made_private_reached, is a constraint trigger deferred to commit, so it reads the
// listener record as the switch's transaction leaves it.
//
// Control: a word of the rendered body changed (the listed sessions' insert dropped), and the test
// names the difference.
func TestTheMadePrivateTriggerIsTheBodyGoRenders(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var source string
	var definer bool
	var settings []string
	if err := database.Pool.QueryRow(ctx, `
		select prosrc, prosecdef, coalesce(proconfig, '{}') from pg_proc where proname = 'record_project_made_private'`).Scan(&source, &definer, &settings); err != nil {
		t.Fatalf("read record_project_made_private: %v", err)
	}
	words := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	if held, rendered := words(source), words(projectMadePrivateFunctionBody()); held != rendered {
		t.Errorf("record_project_made_private's body differs from projectMadePrivateFunctionBody: a migration must replace the function\nheld:     %s\nrendered: %s", held, rendered)
	}
	if !definer || !slices.Equal(settings, []string{"search_path=pg_catalog, public"}) {
		t.Errorf("record_project_made_private runs as its owner %t and sets %v on each of its calls, want security definer with search_path=pg_catalog, public alone", definer, settings)
	}
	var deferrable, deferred bool
	if err := database.Pool.QueryRow(ctx, `
		select tgdeferrable, tginitdeferred from pg_trigger
		where tgname = 'projects_made_private_reached' and tgrelid = 'public.projects'::regclass`).Scan(&deferrable, &deferred); err != nil {
		t.Fatalf("read the trigger projects_made_private_reached: %v", err)
	}
	if !deferrable || !deferred {
		t.Errorf("projects_made_private_reached is deferrable=%t initially deferred=%t, want both: it must read the listener record as the switch's transaction leaves it", deferrable, deferred)
	}
}
