// packages/envoy/internal/cipher/requests/reason_fan_out_test.go
// The review of cybermonklabs/legion#4779: a stored reason naming the most projects a reason can
// name is read inside the time Dispatch gives a list, its projects asked a few at a time and once
// for the list, and a project Dispatch cannot answer for leaves off only the rows naming it.
package requests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// dispatchListWindow is how long Dispatch waits for the broker's answer to a list: its broker
// client's defaultTimeout (internal/dispatch/cipher, client.go), which Pending and Grants use.
const dispatchListWindow = 5 * time.Second

// densestReason is a reason naming n distinct projects with the shortest keys the key pattern
// takes, two-rune prefixes ("A0-1") one space apart: 80 of them take 399 of a reason's 400 runes,
// the most projects a stored reason can name.
func densestReason(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%c%c-1", 'A'+rune(i/10), '0'+rune(i%10))
	}
	return strings.Join(keys, " ")
}

// fanOutLog is a projectaccess.Access that counts each access question it passes on, by project
// and person, and the most it held at once, and fails every question about the project fail.
type fanOutLog struct {
	inner projectaccess.Access
	fail  string
	mu    sync.Mutex
	asked map[string]int
	now   int
	most  int
}

func (f *fanOutLog) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	f.mu.Lock()
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[project+" for "+person]++
	f.now++
	f.most = max(f.most, f.now)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.now--
		f.mu.Unlock()
	}()
	if project == f.fail {
		return projectaccess.Answer{}, errors.New("dispatch did not answer for this project")
	}
	return f.inner.Access(ctx, project, person)
}

func (f *fanOutLog) MemberKind(ctx context.Context, person string) (string, error) {
	return f.inner.MemberKind(ctx, person)
}

func (f *fanOutLog) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return f.inner.States(ctx, projects)
}

// TestAListReadsAStoredReasonOfEightyProjectsWellInsideDispatchsWindow: two requests stored before
// the cap, from two pods that name no person, so every reader's list holds them (everyonesPod),
// each with a reason naming the same 80 projects, none of which Dispatch holds; beside them a
// third request whose reason names no issue. Dispatch holds each answer 100 ms, and the client
// holds none yet. An outsider's list asks the 80 questions maySeeAtOnce at a time, and each once
// for the list, though two rows name all 80, so it answers in well under half of Dispatch's 5 s:
// one at a time they took 8 s. Where Dispatch cannot answer for one of the 80, the two rows naming
// it are left off, asked about it once, and the third is listed: a project Dispatch could not be
// asked about hides the rows it is named by, and no other.
func TestAListReadsAStoredReasonOfEightyProjectsWellInsideDispatchsWindow(t *testing.T) {
	reason := densestReason(80)
	if n, runes := len(record.ReasonProjects(reason)), utf8.RuneCountInString(reason); n != 80 || runes > record.MaxReasonRunes {
		t.Fatalf("densestReason(80) names %d projects in %d runes; want 80 in at most %d", n, runes, record.MaxReasonRunes)
	}
	for _, c := range []struct {
		name string
		fail string
	}{
		{"Dispatch answers every project", ""},
		{"Dispatch cannot answer for one project", "H9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			var stored []string
			for _, name := range []string{"pod-eighty-a", "pod-eighty-b"} {
				pod, key := everyonesPod(t, m, name, "")
				req := storedBeforeTheCap(t, m, pod, key, reason, "SHARED_KEY")
				if req.RecordID == nil {
					t.Fatalf("the stored request of %s = %+v; want it pending", name, req)
				}
				stored = append(stored, *req.RecordID)
			}
			plainPod, plainKey := everyonesPod(t, m, "pod-eighty-plain", "")
			plain, err := m.Create(t.Context(), plainPod, signRequest(t, m, plainKey, "rotate the wiki key", "SHARED_KEY"), "", nil)
			if err != nil || plain.RecordID == nil {
				t.Fatalf("Create(SHARED_KEY, a reason naming no issue) = %+v, %v; want it pending", plain, err)
			}

			dispatch.SetStall(100 * time.Millisecond)
			log := &fanOutLog{inner: projectaccess.New(dispatch.URL, dispatch.Account, nil), fail: c.fail}
			m.Projects = log
			start := time.Now()
			listed := pendingRecords(t, m, outsider)
			took := time.Since(start)
			t.Logf("the list took %s, asked %d questions, at most %d at once", took.Round(time.Millisecond), len(log.asked), log.most)

			if !slices.Contains(listed, *plain.RecordID) {
				t.Errorf("PendingForApprover(%s) = %v; want the request %s, whose reason names no issue", outsider, listed, *plain.RecordID)
			}
			for _, id := range stored {
				if got, want := slices.Contains(listed, id), c.fail == ""; got != want {
					t.Errorf("PendingForApprover(%s) lists the 80-project request %s: %v; want %v", outsider, id, got, want)
				}
			}
			if took >= dispatchListWindow/2 {
				t.Errorf("the list took %s; want under %s, half the %s Dispatch gives it", took, dispatchListWindow/2, dispatchListWindow)
			}
			if len(log.asked) != 80 {
				t.Errorf("the list asked %d distinct questions; want 80, one for each project the reason names", len(log.asked))
			}
			for question, n := range log.asked {
				if n != 1 {
					t.Errorf("the list asked %q %d times; want once for the list, though two rows name it", question, n)
				}
			}
			if log.most < 2 || log.most > maySeeAtOnce {
				t.Errorf("the list held %d questions at once; want from 2 to maySeeAtOnce (%d)", log.most, maySeeAtOnce)
			}
		})
	}
}
