package test

import (
	"testing"

	"example.com/stage4b-fixture/fixture"
)

func TestGreet(t *testing.T) {
	if got := fixture.Greet("stage4b"); got != "hello, stage4b" {
		t.Fatalf("Greet = %q", got)
	}
}
