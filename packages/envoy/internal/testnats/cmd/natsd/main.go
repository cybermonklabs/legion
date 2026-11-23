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
// It is a test fixture, not a product. Nothing deployed runs it: it listens on loopback only, has
// no users but the callout's, keeps its JetStream store wherever the caller says and never cleans
// it up, and lives under internal/testnats so nothing outside this module can import it. The
// server the tests run is the one testnats starts (nats.go); this is the same server for a shell
// script's sake.
package main

import (
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
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/testnats"
)

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
	if *calloutDir != "" {
		config, err := calloutConfig(*calloutDir)
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
	// The server's own readiness, asked every 100 ms, for as long as it takes: a JetStream start
	// on a loaded machine can take longer than any fixed wait allows, and the script that started
	// natsd waits for its readiness line under a deadline of its own. A listener the server cannot
	// open, a port another process holds among them, is logged as fatal, and nats-server's logger
	// (ConfigureLogger's) exits on a fatal line, so that ends this command at once, naming the port.
	for !srv.ReadyForConnections(100 * time.Millisecond) {
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

// calloutConfig returns the path of the callout server's configuration in dir, written from the
// keys dir holds: made and written there the first time, when dir holds none of the three seeds,
// and read back on every start after, so the keys a broker read stay the server's. A dir holding
// some of the seeds but not all is refused naming the first one missing.
func calloutConfig(dir string) (string, error) {
	keys, err := readCalloutKeys(dir)
	if errors.Is(err, fs.ErrNotExist) {
		keys, err = writeCalloutKeys(dir)
	}
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(path, []byte(testnats.CalloutConfig(keys)), 0o600); err != nil {
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
