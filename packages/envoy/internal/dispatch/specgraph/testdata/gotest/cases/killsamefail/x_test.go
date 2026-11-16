package killsamefail_test

import (
	"testing"
	"time"

	"example.com/acme/gotest/killsamefail"
)

func TestSame(t *testing.T) {
	t.Parallel()
	close(killsamefail.ExternalStarted)
	time.Sleep(5 * time.Second)
}
