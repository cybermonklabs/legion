package killsame_test

import (
	"testing"
	"time"

	"example.com/acme/gotest/killsame"
)

func TestSame(t *testing.T) {
	t.Parallel()
	close(killsame.ExternalStarted)
	time.Sleep(5 * time.Second)
}
