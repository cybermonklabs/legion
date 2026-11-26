package memory

import (
	"testing"
	"time"
)

// TestHoldsMemoryWhileSampled holds 64 MiB, every page touched, for longer than two of
// race_memory.py's 5-second samples, so a sample is taken while it runs and names it.
func TestHoldsMemoryWhileSampled(t *testing.T) {
	block := make([]byte, 64<<20)
	for i := range block {
		block[i] = byte(i)
	}
	time.Sleep(12 * time.Second)
	if block[len(block)-1] != byte(len(block)-1) {
		t.Fatal("the block changed under the test")
	}
}
