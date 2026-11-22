package consumernames

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// Deps are what CommandOn reaches outside the process, in place of the broker, Secret Manager,
// the hub and the family databases its flags name: a test of another package runs the commands on
// its own stack with them (writerhold/holdtest), as writerhold.StreamsCommandOn runs the streams
// words. The hold's flags are still required and checked, and their values are not read.
type Deps struct {
	// Records opens the operator's client of the broker's hold routes, signing when signing (every
	// command but status).
	Records func(ctx context.Context, signing bool) (*holdrecord.Client, error)
	// Connect opens the operator's NATS connection to the hub, its replies under
	// natsgrant.OperatorInbox.
	Connect func(ctx context.Context) (*nats.Conn, error)
	// Database opens the Postgres store of family: "dispatch", or "daemon/<project key>". Nil is a
	// run with no family store, which refuses a command that needs one, naming the family. The
	// store is the caller's: the run does not close it.
	Database func(ctx context.Context, family string) (Database, error)
	// Operator is the operator's reach of the clustered hub: its Kubernetes objects and each
	// declared server's monitoring port, which the held re-read, convert's meta barrier and restore
	// --finish read. Nil is a run with no such reach, which refuses every held command, since each
	// runs the held re-read (run.operatorHub), and leaves status UNKNOWN on a daemon store that
	// answers SQLSTATE 3D000.
	Operator func(ctx context.Context) (*writerhold.Operator, error)
	// Now is the clock; time.Now when nil (run.now).
	Now func() time.Time
}

// CommandOn is Command with deps in place of the broker, Secret Manager, the hub and the
// databases its flags name.
func CommandOn(ctx context.Context, args []string, stdout, stderr io.Writer, deps Deps) int {
	e := env{
		records: func(ctx context.Context, _ options, signing bool) (*holdrecord.Client, error) {
			return deps.Records(ctx, signing)
		},
		connect: func(ctx context.Context, _ options) (*nats.Conn, error) { return deps.Connect(ctx) },
		database: func(ctx context.Context, _ options, family string) (Database, func(), error) {
			if deps.Database == nil {
				return nil, nil, fmt.Errorf("this run has no store of %s", family)
			}
			db, err := deps.Database(ctx, family)
			return db, nil, err
		},
		now: deps.Now,
	}
	if deps.Operator != nil {
		e.hub = func(ctx context.Context, _ options) (*writerhold.Operator, error) { return deps.Operator(ctx) }
	}
	return runCommand(ctx, args, stdout, stderr, e)
}
