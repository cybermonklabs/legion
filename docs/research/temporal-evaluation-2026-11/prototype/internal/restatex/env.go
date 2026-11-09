package restatex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ServerReady is how long a driver waits for restate-server to answer. The server
// provisions its cluster on first start, which on a small machine takes a few seconds.
const ServerReady = 90 * time.Second

// ServiceReady is how long a driver waits for a service process's endpoint to answer.
const ServiceReady = 30 * time.Second

// EnvOptions is everything one Restate experiment needs before it can measure
// anything: where to work, which binaries to run, and which loopback addresses the
// server is to use.
type EnvOptions struct {
	// Experiment is the number, used to name the work directory and the cluster node
	// so two experiments never share a data directory.
	Experiment int
	// WorkDir is the scratch directory the experiment was given; the environment
	// makes its own directory under it.
	WorkDir string
	// ServerBinary is the restate-server executable the driver starts. Empty means
	// the driver starts nothing and measures a server already running at Ingress and
	// Admin.
	ServerBinary string
	// Ingress and Admin are the server's two loopback addresses.
	Ingress string
	Admin   string
}

// Env is one experiment's Restate: a work directory of its own, the server process if
// the driver started it, and clients for the server's two APIs.
type Env struct {
	// Dir is this experiment's own scratch directory, under the given -work-dir. The
	// server's data lives under it, so nothing is written outside -work-dir.
	Dir string
	// Server is the process the driver started, or nil when the experiment was
	// pointed at a server somebody else runs.
	Server *Server
	// Admin and Ingress address that server.
	Admin   *Admin
	Ingress *Ingress
	// Version is the server version the experiment measured, read from the admin API.
	Version string
	// Defaults are the server's own effective settings, read from the binary, and
	// empty when the experiment did not start the server itself.
	Defaults Defaults
	// StartedServer records which of the two cases this is, which is a fact worth
	// printing: a measurement against somebody else's server is a different claim.
	StartedServer bool
}

// StartEnv prepares one experiment's Restate and returns it ready to use. The caller
// must call Stop on every exit path.
func StartEnv(ctx context.Context, opts EnvOptions) (*Env, error) {
	if err := RequireLoopback("-restate-ingress", opts.Ingress); err != nil {
		return nil, err
	}
	if err := RequireLoopback("-restate-admin", opts.Admin); err != nil {
		return nil, err
	}
	dir := filepath.Join(opts.WorkDir, fmt.Sprintf("exp%d-restate", opts.Experiment))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	env := &Env{
		Dir:     dir,
		Admin:   NewAdmin(opts.Admin),
		Ingress: NewIngress(opts.Ingress),
	}

	if opts.ServerBinary != "" {
		fabric, err := FreePort()
		if err != nil {
			return nil, err
		}
		defaults, err := ReadDefaults(ctx, opts.ServerBinary, dir)
		if err != nil {
			return nil, err
		}
		env.Defaults = defaults
		server, err := StartServer(ctx, ServerOptions{
			Binary:  opts.ServerBinary,
			BaseDir: filepath.Join(dir, "server"),
			Ingress: opts.Ingress,
			Admin:   opts.Admin,
			Fabric:  fabric,
			// A node name that never changes for a data directory that is always
			// new, so a second run cannot inherit the first one's cluster identity.
			NodeName: fmt.Sprintf("exp%d", opts.Experiment),
			// One partition: a single-node experiment needs no more, and the cluster
			// is provisioned in a second rather than ten.
			Partitions: 1,
		}, ServerReady)
		if err != nil {
			return nil, err
		}
		env.Server = server
		env.StartedServer = true
	}

	version, err := env.Admin.Version(ctx)
	if err != nil {
		env.Stop()
		if opts.ServerBinary == "" {
			return nil, fmt.Errorf("no restate-server binary was given and no server answered on the admin address: pass -restate-server with the path of a restate-server executable, or start one yourself and pass its -restate-admin and -restate-ingress addresses: %w", err)
		}
		return nil, err
	}
	env.Version = version
	return env, nil
}

// Stop takes down whatever the environment started. It is safe to call more than once.
func (e *Env) Stop() {
	if e.Server != nil {
		e.Server.Stop()
	}
}
