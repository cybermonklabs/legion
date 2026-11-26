// packages/envoy/internal/cipher/requests/give_back_panic_test.go
// A give-back runs on a goroutine of its own, where net/http recovers no panic: one that panics
// leaves its value to lapse, logged, and the give-back goes on.
package requests

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAGiveBackThatPanicsLeavesItsValueToLapseAndGoesOn: a release hands two minted values back,
// FIRST_KEY's and SECOND_KEY's, and FIRST_KEY's GiveBack panics with its value in hand. SECOND_KEY's
// is still given back, and one ERROR line names FIRST_KEY with the goroutine's stack and holds
// neither value. Unrecovered, the panic ends the test binary, as it would the broker.
func TestAGiveBackThatPanicsLeavesItsValueToLapseAndGoesOn(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	const firstValue, secondValue = "ghs_the-first-minted-token", "ghs_the-second-minted-token"
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	var mu sync.Mutex
	var gaveBack []string
	m := &Machine{GiveBacks: map[string]GiveBack{"github-app": func(_ context.Context, name, value, _ string) {
		if name == "FIRST_KEY" {
			panic(fmt.Sprintf("the revoke of %s failed", value))
		}
		mu.Lock()
		gaveBack = append(gaveBack, name)
		mu.Unlock()
	}}}

	m.giveBack(t.Context(), []releasedName{
		{name: "FIRST_KEY", value: firstValue, secretType: "github-app"},
		{name: "SECOND_KEY", value: secondValue, secretType: "github-app"},
	})
	m.givingBack.Wait()

	if !slices.Equal(gaveBack, []string{"SECOND_KEY"}) {
		t.Errorf("the give-back gave back %v; want SECOND_KEY, after FIRST_KEY's panicked", gaveBack)
	}
	lines := linesOf(capture, giveBackPanickedMessage)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["name"] != "FIRST_KEY" || lines[0]["stack"] == "" ||
		lines[0]["error"] != "a panic of type string" {
		t.Fatalf("the log holds %v; want one ERROR line of %q naming FIRST_KEY, a panic of type string, with its stack", lines, giveBackPanickedMessage)
	}
	for _, record := range capture.Records() {
		line := fmt.Sprint(linesOf(capture, record.Message))
		if strings.Contains(line, firstValue) || strings.Contains(line, secondValue) {
			t.Fatalf("a log line holds a minted value: %s", line)
		}
	}
}
