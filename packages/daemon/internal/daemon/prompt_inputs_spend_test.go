package daemon

import (
	"sync"
	"testing"
)

const (
	promptInputsDefaultTokenCap = 1_850_000
	promptInputsMaxTokenCap     = 6_000_000
)

type promptInputsUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}

func (u promptInputsUsage) inputTotal() int { return u.Input + u.CacheRead + u.CacheWrite }

func (u *promptInputsUsage) add(v promptInputsUsage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
}

// A reservation includes the estimated input and the maximum output while the call runs.
// Once the call ends only its usage, reported or estimated, counts against the shared cap.
type promptInputsSpend struct {
	cap       int
	mu        sync.Mutex
	accounted int
	calls     int
	usage     promptInputsUsage
}

func (s *promptInputsSpend) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.cap-s.accounted {
		return false
	}
	s.accounted += n
	s.calls++
	return true
}

// settle replaces one reservation. A partial report stays in the reported totals but charges
// at least the input estimate and the output seen. It does not continue to hold the unused
// output budget. The streamed estimate counts generated deltas, not the JSON framing.
func (s *promptInputsSpend) settle(reserved, inputEstimate, streamedOutput int, usage promptInputsUsage, complete bool) int {
	charge := usage.inputTotal() + usage.Output
	if !complete {
		charge = max(inputEstimate, usage.inputTotal()) + max(streamedOutput, usage.Output)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounted += charge - reserved
	s.usage.add(usage)
	return charge
}

func TestPromptInputsSpendReleasesEveryEndedCall(t *testing.T) {
	t.Parallel()
	ledger := promptInputsSpend{cap: 300}
	if !ledger.reserve(150) || !ledger.reserve(150) || ledger.reserve(1) {
		t.Fatal("in-flight calls must hold their full reservations")
	}
	complete := promptInputsUsage{Input: 10, CacheRead: 12, CacheWrite: 18, Output: 7}
	if got := ledger.settle(150, 90, 35, complete, true); got != 47 {
		t.Fatalf("complete charge = %d, want 47 including both cache counts", got)
	}
	partial := promptInputsUsage{Input: 4, CacheRead: 20, Output: 3}
	if got := ledger.settle(150, 80, 16, partial, false); got != 96 {
		t.Fatalf("partial charge = %d, want estimated input plus streamed output (96)", got)
	}
	if ledger.accounted != 143 || ledger.calls != 2 || ledger.usage != (promptInputsUsage{Input: 14, CacheRead: 32, CacheWrite: 18, Output: 10}) {
		t.Fatalf("settled ledger: accounted %d, calls %d, usage %+v", ledger.accounted, ledger.calls, ledger.usage)
	}
	if !ledger.reserve(157) || ledger.reserve(1) {
		t.Fatal("settlement did not release exactly the unused reservation")
	}
	if got := ledger.settle(157, 70, 5, promptInputsUsage{CacheWrite: 80, Output: 9}, false); got != 89 {
		t.Fatalf("partial charge = %d, want the higher reported counts (89)", got)
	}
}
