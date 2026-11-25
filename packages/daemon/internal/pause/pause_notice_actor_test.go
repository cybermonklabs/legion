package pause

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main tells the controller of the operator's pause and resume alone (pause/pause.go:
// tellsController at d46ec4e0: "The operator's act does and the controller's own does not";
// record/outbox.go at d46ec4e0: "the controller's own pause or resume tells it nothing"), so
// every project-paused and project-resumed notice names `by: {kind: operator}` and no session.
// INFRA-3686's R2: "The fields are those of the kind, the issue, the act, the actor, the time …";
// the change's own wrong fact is its defect. The branch's controller prompt, which main did not
// have for these kinds, says a project-paused notice's `by` is the operator or "a controller
// session, which `session` names", and that a project-resumed notice names "who resumed it
// (`by`, the operator or a controller session, as for a pause)". Here the operator pauses and the
// registered controller's session resumes, then the controller's session pauses and the operator
// resumes: every notice the acts send names the operator, and the controller session's acts send
// none.
func TestTheControllersPromptNamesNoPauseActThatTellsItNothing(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	registerController(t, r.st)
	if _, err := r.svc.Pause(ctx, operator, "the model provider is down"); err != nil {
		t.Fatalf("pause as the operator: %v", err)
	}
	r.clock.Advance(10 * time.Minute)
	if _, _, err := r.svc.Resume(ctx, controller); err != nil {
		t.Fatalf("resume as the controller's session: %v", err)
	}
	r.clock.Advance(10 * time.Minute)
	if _, err := r.svc.Pause(ctx, controller, "the controller's own words"); err != nil {
		t.Fatalf("pause as the controller's session: %v", err)
	}
	r.clock.Advance(10 * time.Minute)
	if _, _, err := r.svc.Resume(ctx, operator); err != nil {
		t.Fatalf("resume as the operator: %v", err)
	}
	_, notices := controllerNotices(t, r.st)
	told := map[record.NoticeKind][]string{}
	for _, notice := range notices {
		if notice.Pause == nil {
			continue
		}
		told[notice.Kind] = append(told[notice.Kind], notice.Pause.By.Kind)
		if notice.Pause.By.Kind != record.PauseByOperator {
			return
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "controller-root.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "A `project-paused` notice") {
			continue
		}
		for _, claimed := range []string{"`kind: controller` is a controller session", "the operator or a controller session"} {
			if strings.Contains(line, claimed) {
				t.Errorf("roles/controller-root.md says %q of these notices, but a controller session's pause and resume send none, and "+
					"every notice the four acts sent names the operator: %v", claimed, told)
			}
		}
	}
}
