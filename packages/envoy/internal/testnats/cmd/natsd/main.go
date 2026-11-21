// Command natsd runs one nats-server in this process, in the foreground, until it is signalled.
// It is the bus of packages/envoy/scripts/e2e-local.sh and packages/envoy/scripts/e2e-refill.sh,
// each of which builds it beside the listener it builds: an agent's machine has no container
// runtime, so neither script can run the NATS image. e2e-refill.sh also stops, kills and restarts
// it under a running listener, and a restart reads back the JetStream store the directory holds.
//
// With -callout-keys <dir> it runs the configuration testnats.StartWithCallout runs instead, on
// options of its own, so it keeps nats-server's short first PING as a deployed hub does: every
// client but the auth service's own user is sent to the auth service (packages/envoy/scripts/
// dev-stack.sh --callout runs that service in its brokers). The service's user, the issuer and the
// curve key are made the first time, as testnats.NewCalloutKeys makes them, and written to
// service.seed, issuer.seed and xkey.seed in dir, each readable by its owner alone, beside the
// server's configuration (nats.conf); a later start on the same dir keeps them, so a restarted
// server still answers to the brokers that read them.
//
// Behind the callout it also makes every protected stream of the declared set
// (internal/streamset, EnsureStreams) once it accepts connections, as an admin user of its own
// that only this process holds: the configuration names the admin's public key, made at each
// start, and its seed never leaves this process's memory. The auth service answers no client but
// the operator with a stream create or update (natsgrant.ForCallout), and the dev stack's listener
// and Dispatch run beside it with ENVOY_STREAMS_OPERATOR_MADE=1 and only read their streams, so the
// streams exist before any of them connects, as the operator's update makes them in a deployment.
// The admin's connection closes once they are made.
//
// It is a test fixture, not a product. Nothing deployed runs it: it listens on loopback only, has
// no users but the callout's and its own admin's, keeps its JetStream store wherever the caller says and never cleans
// it up, and lives under internal/testnats so nothing outside this module can import it. The
// server the tests run is the one testnats starts (nats.go); this is the same server for a shell
// script's sake.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
)

// readyTimeout bounds the wait for the server to accept connections, so a port another process
// holds ends this command instead of leaving its caller waiting.
const readyTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "natsd: %v\n", err)
		os.Exit(1)
	}
}

// run parses args as nats-server parses its own command line (-p for the client port, -store_dir
// for the JetStream store) and -callout-keys, starts the server with JetStream on 127.0.0.1 and
// its log on stdout, and returns once a signal has shut it down.
func run(args []string) error {
	flags := flag.NewFlagSet("natsd", flag.ContinueOnError)
	calloutDir := flags.String("callout-keys", "", "run behind an auth callout whose keys are in (or are made in) this directory")
	help := func() { flags.PrintDefaults() }
	opts, err := natsserver.ConfigureOptions(flags, args, help, help, help)
	if err != nil {
		return err
	}
	if opts == nil {
		// -v, -h or -help_tls: ConfigureOptions has printed what was asked for.
		return nil
	}
	var admin nkeys.KeyPair
	if *calloutDir != "" {
		if admin, err = nkeys.CreateUser(); err != nil {
			return err
		}
		adminKey, err := admin.PublicKey()
		if err != nil {
			return err
		}
		config, err := calloutConfig(*calloutDir, adminKey)
		if err != nil {
			return err
		}
		if err := opts.ProcessConfigFile(config); err != nil {
			return fmt.Errorf("read %s: %w", config, err)
		}
	}
	opts.Host = "127.0.0.1"
	opts.JetStream = true
	opts.NoSigs = true
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return err
	}
	srv.ConfigureLogger()
	go srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		return fmt.Errorf("no connections accepted on %s:%d within %s; is the port taken?", opts.Host, opts.Port, readyTimeout)
	}
	if admin != nil {
		if err := makeStreams(srv.ClientURL(), admin); err != nil {
			srv.Shutdown()
			srv.WaitForShutdown()
			return err
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	srv.Shutdown()
	srv.WaitForShutdown()
	return nil
}

// calloutSeeds are the files -callout-keys keeps the three keys in.
var calloutSeeds = [3]string{"service.seed", "issuer.seed", "xkey.seed"}

// adminUser is the admin user's grant behind the callout: the JetStream API, which making the
// declared streams calls, and the reply inboxes its answers come to.
func adminUser(nkey string) testnats.StaticUser {
	return testnats.StaticUser{NKey: nkey, Publish: []string{"$JS.API.>"}, Subscribe: []string{"_INBOX.>"}}
}

// makeStreamsWait bounds making the declared streams.
const makeStreamsWait = time.Minute

// makeStreams makes every protected stream of the declared set on the server at url as admin, at
// one copy with no byte limit and the session bucket's own age, the settings the dev stack's
// programs run with, and closes its connection.
func makeStreams(url string, admin nkeys.KeyPair) error {
	adminKey, err := admin.PublicKey()
	if err != nil {
		return err
	}
	conn, err := nats.Connect(url, nats.Nkey(adminKey, admin.Sign), nats.Name("natsd-admin"))
	if err != nil {
		return fmt.Errorf("connect as the admin user: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), makeStreamsWait)
	defer cancel()
	if err := streamset.EnsureStreams(ctx, conn, streamset.Settings{}); err != nil {
		return fmt.Errorf("make the declared streams as the admin user: %w", err)
	}
	fmt.Println("natsd: made the declared streams as its admin user")
	return nil
}

// calloutConfig returns the path of the callout server's configuration in dir, written from the
// keys dir holds and adminKey, the admin user's public key: the keys are made and written there
// the first time, when dir holds none of the three seeds, and read back on every start after, so
// the keys a broker read stay the server's. A dir holding some of the seeds but not all is refused
// naming the first one missing.
func calloutConfig(dir, adminKey string) (string, error) {
	keys, err := readCalloutKeys(dir)
	if errors.Is(err, fs.ErrNotExist) {
		keys, err = writeCalloutKeys(dir)
	}
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(path, []byte(testnats.CalloutConfig(keys, adminUser(adminKey))), 0o600); err != nil {
		return "", fmt.Errorf("write the callout configuration: %w", err)
	}
	return path, nil
}

// readCalloutKeys reads the three seeds from dir, and answers fs.ErrNotExist only when none of
// them is there.
func readCalloutKeys(dir string) (testnats.CalloutKeys, error) {
	var keys testnats.CalloutKeys
	targets := [3][2]*string{
		{&keys.ServiceSeed, &keys.ServicePublic},
		{&keys.IssuerSeed, &keys.IssuerPublic},
		{&keys.XKeySeed, &keys.XKeyPublic},
	}
	var missing []string
	for index, name := range calloutSeeds {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return keys, fmt.Errorf("read %s: %w", name, err)
		}
		pair, err := nkeys.FromSeed(raw)
		if err != nil {
			return keys, fmt.Errorf("%s holds no seed: %w", name, err)
		}
		public, err := pair.PublicKey()
		if err != nil {
			return keys, fmt.Errorf("%s: %w", name, err)
		}
		*targets[index][0], *targets[index][1] = string(raw), public
	}
	switch len(missing) {
	case 0:
		return keys, nil
	case len(calloutSeeds):
		return keys, fs.ErrNotExist
	}
	return keys, fmt.Errorf("%s holds some of the callout's seeds but not %s", dir, missing[0])
}

// writeCalloutKeys makes the three keys and writes their seeds to dir, each readable by its owner
// alone.
func writeCalloutKeys(dir string) (testnats.CalloutKeys, error) {
	keys, err := testnats.MakeCalloutKeys()
	if err != nil {
		return keys, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return keys, err
	}
	for index, seed := range [3]string{keys.ServiceSeed, keys.IssuerSeed, keys.XKeySeed} {
		if err := os.WriteFile(filepath.Join(dir, calloutSeeds[index]), []byte(seed), 0o600); err != nil {
			return keys, fmt.Errorf("write %s: %w", calloutSeeds[index], err)
		}
	}
	return keys, nil
}
