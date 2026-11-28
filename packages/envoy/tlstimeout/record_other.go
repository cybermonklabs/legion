//go:build !unix

package tlstimeout

import "context"

// RecordContext writes nothing and returns on a system that is not unix, such as windows, which
// agent-secrets is built for too: the record's rate limit keeps its stamp under flock, opened with
// O_NOFOLLOW and held to its owner's uid, and such a system has none of them, and a record that
// could not be limited could flood a log in the burst it measures (record_unix.go).
func (r *Recorder) RecordContext(context.Context, string, Event) {}
