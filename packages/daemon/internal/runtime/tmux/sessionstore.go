package tmux

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// sessionLookupTimeout bounds a resume's look-up of its session in the session table: connecting
// to the database and reading the row.
const sessionLookupTimeout = 30 * time.Second

// sessionInDatabase is whether the session a resume of backend names is a row of the session table
// rather than a file: an Oh My Pi session under a session database (Options.SessionDSNFile), keyed
// by the path its file would have had, as a pod's launcher holds a resume (internal/launcher).
// Every other session, a Claude Code transcript under either store, is a file.
func (r *Runtime) sessionInDatabase(backend runtime.Backend) bool {
	return r.sessionDSNFile != "" && backend == runtime.BackendOmp
}

// sessionWritten is whether the session table holds the session file names (ompsessions.Written).
// An error is a look-up that could not say: a database it could not reach or read.
func (r *Runtime) sessionWritten(ctx context.Context, file string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionLookupTimeout)
	defer cancel()
	conn, err := ompsessions.Connect(ctx, r.sessionDSNFile)
	if err != nil {
		return false, fmt.Errorf("resume session %s: %w", file, err)
	}
	defer conn.Close(context.Background())
	_, ok, err := ompsessions.Written(ctx, conn, file)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// sessionByID is the path of the newest session the session table holds under the session id id
// (ompsessions.FindByID), the path its file would have had, for a resume that names its session by
// its id alone; "" when the table holds none, which the resume answers as a session found nowhere
// (runtime.ErrSessionFileMissing). An error is a look-up that could not say: a database it could
// not reach or read.
func (r *Runtime) sessionByID(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionLookupTimeout)
	defer cancel()
	conn, err := ompsessions.Connect(ctx, r.sessionDSNFile)
	if err != nil {
		return "", fmt.Errorf("resume session %s: %w", id, err)
	}
	defer conn.Close(context.Background())
	path, _, err := ompsessions.FindByID(ctx, conn, id)
	return path, err
}

// recordedSession is where the session a resume names is, "" when it is nowhere, with the folders
// searched for it and what the search skipped. Under a session database an Oh My Pi session is a
// row of the session table rather than a file (sessionInDatabase), so the row is the whole search:
// the session is at its recorded path or nowhere, and no folder is searched or skipped. Every
// other session is searched for as a file (sessionFileOf).
func (r *Runtime) recordedSession(ctx context.Context, file string, backend runtime.Backend, sessionsDir func() (string, error)) (found string, searched, skipped []string, err error) {
	if !r.sessionInDatabase(backend) {
		return sessionFileOf(file, sessionsDir)
	}
	written, err := r.sessionWritten(ctx, file)
	if err != nil || !written {
		return "", nil, nil, err
	}
	return file, nil, nil, nil
}

// sessionOfID is where the session a resume names by its id alone is (runtime.SpawnSpec's
// ResumeSession), with what the search skipped. Under a session database an Oh My Pi session is a
// row of the session table (sessionInDatabase), so the newest row of the id is the whole search
// (sessionByID): found is the path its file would have had, and no folder is searched or
// skipped. Every other session is the newest file of the id one folder below the pane's sessions
// directory, which sessionsDir resolves (sessionFileByID). A session the store holds nowhere is an
// error wrapping runtime.ErrSessionFileMissing, in the store's own sentence; any other error is a
// look-up that could not say, and wraps no runtime.ErrSessionFileMissing.
func (r *Runtime) sessionOfID(ctx context.Context, id string, backend runtime.Backend, sessionsDir func() (string, error)) (found string, skipped []string, err error) {
	harness := runtime.Backends[backend].Name
	if r.sessionInDatabase(backend) {
		found, err := r.sessionByID(ctx, id)
		if err == nil && found == "" {
			err = fmt.Errorf("%w: the session table holds no row of %s session %s", runtime.ErrSessionFileMissing, harness, id)
		}
		return found, nil, err
	}
	sessions, err := sessionsDir()
	if err != nil {
		return "", nil, err
	}
	found, skipped, err = sessionFileByID(sessions, id)
	if err == nil && found == "" {
		err = fmt.Errorf("%w: no folder of the pane's %s sessions directory %s holds a file of session %s",
			runtime.ErrSessionFileMissing, harness, sessions, id)
	}
	return found, skipped, err
}
