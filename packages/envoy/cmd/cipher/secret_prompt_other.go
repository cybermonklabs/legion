//go:build (!linux && !darwin) || (!amd64 && !arm64)

package main

import "time"

func readHiddenAtTerminal(int, time.Time, func(), func(error)) ([]byte, error) {
	return nil, errNoValuePrompt
}
