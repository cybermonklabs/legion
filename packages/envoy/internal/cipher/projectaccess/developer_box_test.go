package projectaccess_test

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestDeveloperBoxAsOfRefusesAProjectClosedToDeveloperBoxes: Dispatch's closed_to_developer_boxes
// reaches Answer from its own key, and an answer that carries none, an older Dispatch's, is a
// project open to them. DeveloperBoxAsOf passes a list of projects its person sees that no answer
// calls closed, a private one on its first day (DAYONE) and one an older Dispatch answers (OLD)
// among them, and refuses, naming it, the first that is closed: CLOSE, public and closing, and
// SWITCHED, private after its full switch. A project its person cannot see is refused as Visible
// refuses it, never named as closed. Visible itself is unchanged: it passes every one of them.
func TestDeveloperBoxAsOfRefusesAProjectClosedToDeveloperBoxes(t *testing.T) {
	const carol, alice = "carol@example.com", "alice@example.com"
	client := clientOf(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PUB":      {Visibility: "public", Owner: carol},
		"DAYONE":   {Visibility: "private", Owner: carol},
		"OLD":      {Visibility: "private", Owner: carol},
		"CLOSE":    {Visibility: "public", Owner: carol, ClosedToDeveloperBoxes: true},
		"SWITCHED": {Visibility: "private", Owner: carol, ClosedToDeveloperBoxes: true},
	}, map[string]string{carol: projectaccess.MemberFull}))

	for key, closed := range map[string]bool{"PUB": false, "DAYONE": false, "OLD": false, "CLOSE": true, "SWITCHED": true} {
		answer, err := client.Access(t.Context(), key, carol)
		if err != nil {
			t.Fatal(err)
		}
		if answer.ClosedToDeveloperBoxes != closed {
			t.Fatalf("Access(%s).ClosedToDeveloperBoxes = %v, want %v", key, answer.ClosedToDeveloperBoxes, closed)
		}
	}

	if asked, err := projectaccess.DeveloperBoxAsOf(t.Context(), client, carol, []string{"PUB", "DAYONE", "OLD"}); err != nil || asked.IsZero() {
		t.Fatalf("DeveloperBoxAsOf(carol, PUB DAYONE OLD) = %v, %v; want a pass and the time its oldest answer was asked", asked, err)
	}
	for _, c := range []struct {
		projects []string
		closed   string
	}{
		{[]string{"PUB", "CLOSE"}, "CLOSE"},
		{[]string{"SWITCHED", "PUB"}, "SWITCHED"},
	} {
		_, err := projectaccess.DeveloperBoxAsOf(t.Context(), client, carol, c.projects)
		var refused *projectaccess.ClosedError
		if !errors.As(err, &refused) || refused.Project != c.closed || !errors.Is(err, projectaccess.ErrClosed) {
			t.Fatalf("DeveloperBoxAsOf(carol, %v) = %v, want ErrClosed naming %s", c.projects, err, c.closed)
		}
	}
	_, err := projectaccess.DeveloperBoxAsOf(t.Context(), client, alice, []string{"SWITCHED"})
	var unseen *projectaccess.NotVisibleError
	if !errors.As(err, &unseen) || unseen.Project != "SWITCHED" || errors.Is(err, projectaccess.ErrClosed) {
		t.Fatalf("DeveloperBoxAsOf(alice, SWITCHED) = %v, want ErrNotVisible naming SWITCHED and not ErrClosed", err)
	}
	if err := projectaccess.Visible(t.Context(), client, carol, []string{"CLOSE", "SWITCHED"}); err != nil {
		t.Fatalf("Visible(carol, CLOSE SWITCHED) = %v, want a pass: a closed project is no question of sight", err)
	}
}
