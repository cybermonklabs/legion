package slow

import (
	"testing"
	"time"
)

// TestSlow sleeps for 60 s, so go test kills it at a -timeout of 3s.
func TestSlow(t *testing.T) {
	t.Log("slow started")
	time.Sleep(60 * time.Second)
}
