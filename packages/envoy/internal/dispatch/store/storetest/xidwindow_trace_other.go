//go:build !linux

package storetest

import (
	"errors"
	"time"
)

// xidTrace and xidTraceReading are the execution trace a build that judges gaps takes of the
// operation; this build judges none and takes none (XidJudgesGaps).
type xidTrace struct{}

type xidTraceReading struct{}

func startXidTrace() (*xidTrace, error) {
	return nil, errors.New("this build judges no gap, so it takes no execution trace")
}

func (*xidTrace) stop() {}

func (*xidTrace) read() (*xidTraceReading, error) { return &xidTraceReading{}, nil }

func (*xidTraceReading) goStates(int, time.Time, time.Time) (xidGoStates, bool) {
	return xidGoStates{}, false
}

func (*xidTraceReading) pastEnd(time.Time) time.Duration { return 0 }
