package access

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheMadePrivateTriggerIsTheBodyGoRenders: record_project_made_private, the function of the
// projects table's trigger, is projectMadePrivateFunctionBody as it renders now, word for word
// (whitespace aside), so the rule it reads (PersonSeesProjectSQL) is written once, in Go; it sets
// nothing for its calls; and its trigger, projects_made_private_reached, is a constraint trigger
// deferred to commit, so the person locks it takes are the last its transaction takes.
//
// Control: a word of the rendered body changed (the listed sessions' insert dropped), and the test
// names the difference.
func TestTheMadePrivateTriggerIsTheBodyGoRenders(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var source string
	var settings []string
	if err := database.Pool.QueryRow(ctx, `
		select prosrc, coalesce(proconfig, '{}') from pg_proc where proname = 'record_project_made_private'`).Scan(&source, &settings); err != nil {
		t.Fatalf("read record_project_made_private: %v", err)
	}
	words := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	if held, rendered := words(source), words(projectMadePrivateFunctionBody()); held != rendered {
		t.Errorf("record_project_made_private's body differs from projectMadePrivateFunctionBody: a migration must replace the function\nheld:     %s\nrendered: %s", held, rendered)
	}
	if len(settings) > 0 {
		t.Errorf("record_project_made_private sets %v on each of its calls, want none", settings)
	}
	var deferrable, deferred bool
	if err := database.Pool.QueryRow(ctx, `
		select tgdeferrable, tginitdeferred from pg_trigger
		where tgname = 'projects_made_private_reached' and tgrelid = 'public.projects'::regclass`).Scan(&deferrable, &deferred); err != nil {
		t.Fatalf("read the trigger projects_made_private_reached: %v", err)
	}
	if !deferrable || !deferred {
		t.Errorf("projects_made_private_reached is deferrable=%t initially deferred=%t, want both: its person locks must be the last its transaction takes", deferrable, deferred)
	}
}
