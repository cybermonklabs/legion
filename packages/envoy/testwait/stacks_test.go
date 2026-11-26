package testwait

import (
	"strconv"
	"strings"
	"testing"
)

// A traceback at GOTRACEBACK=system or crash names gp and m before its state. The parser keeps the
// goroutine id and removes the machine-specific arguments and frame fields from its grouping key.
func TestReadTraceParsesSystemHeader(t *testing.T) {
	trace := "goroutine 7 gp=0x123 m=0 [select, 2 minutes]:\n" +
		"example.wait(0x1)\n\t/src/example.go:12 +0x5 fp=0x1 sp=0x2 pc=0x3\n" +
		"created by example.start in goroutine 1\n\t/src/start.go:3 +0x2"
	got := readTrace(trace)
	if got.id != 7 || got.creator != 1 {
		t.Fatalf("the system traceback read id %d and creator %d, want 7 and 1", got.id, got.creator)
	}
	if strings.Contains(got.key, "0x1") || strings.Contains(got.key, "fp=") || strings.Contains(got.key, "2 minutes") {
		t.Errorf("the stack key keeps machine-specific text:\n%s", got.key)
	}
}

// A dump of a test and many identical goroutines stays under stackBound after the waiting stack and
// says what it cut, rather than producing a package-sized alarm dump.
func TestDumpStacksStopsAtTheBound(t *testing.T) {
	traces := []string{"goroutine 5 [running]:\nexample.wait()\n\ttest.go:1\ntesting.tRunner()\n\ttesting.go:1\ncreated by testing.(*T).Run in goroutine 1\n\ttesting.go:1"}
	traces = append(traces, "goroutine 1 [chan receive]:\ntesting.runTests()\n\ttesting.go:1")
	for id := 6; id < 1000; id++ {
		traces = append(traces, "goroutine "+strconv.Itoa(id)+" [select]:\nexample.worker"+strconv.Itoa(id)+"()\n\ttest.go:2\ncreated by example.start in goroutine 5\n\ttest.go:1")
	}
	got := dumpStacks("TestWait", "as the wait gave up", traces)
	if !strings.Contains(got, "end of the goroutine stacks") || len(got) > stackBound+4096 {
		t.Errorf("the bounded dump is %d bytes and has end line %t", len(got), strings.Contains(got, "end of the goroutine stacks"))
	}
}
