package api

import (
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// viewerSees reports whether viewer sees project; no viewer is the server's own read.
func viewerSees(viewer *model.Viewer, project string) bool {
	return viewer == nil || viewer.All || slices.Contains(viewer.Sees, project)
}
