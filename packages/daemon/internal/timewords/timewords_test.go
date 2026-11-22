package timewords

import (
	"testing"
	"time"
)

// Each bound the daemon's sentences name reads as a person says it, and the unit is the largest
// one the bound is a whole number of, an hour reading as 60 minutes.
func TestWordsSaysADurationAsASentenceDoes(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Minute:  "5 minutes",
		20 * time.Minute: "20 minutes",
		60 * time.Minute: "60 minutes",
		6 * time.Hour:    "6 hours",
		2 * time.Hour:    "2 hours",
		time.Minute:      "1 minute",
		90 * time.Second: "90 seconds",
		time.Second:      "1 second",
		0:                "0 seconds",
	} {
		if got := Words(d); got != want {
			t.Errorf("Words(%s) = %q, want %q", d, got, want)
		}
	}
}
