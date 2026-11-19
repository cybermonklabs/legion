//go:build memory || multiprocess

package main

import "github.com/sjawhar/envoy/internal/dispatch/docs"

// liveWritePausePath is the test hook route of the pause of the document's live writes at point,
// which the memory and the multiprocess drives both arm, wait on and release.
func liveWritePausePath(artifactID string, point docs.LiveWritePausePoint) string {
	return "/api/v1/artifacts/" + artifactID + "/_test/live-write-pauses/" + string(point)
}
