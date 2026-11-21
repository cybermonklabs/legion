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
//
// Every listener it has binds one loopback address of its own (testnats.ListenOn): the address -a
// names, which must be a loopback address, or else one testport.Claim picks and claims. A caller
// reads the address from the log, on the line that already named the port (`Listening for client
// connections on <address>:<port>`), and dials that, never 127.0.0.1. The claim is natsd's own,
// released when it exits, unless -claim-for names another process: a script that restarts natsd on
// its old port starts it the first time with `-claim-for $$`, so the claim is the script's and
// outlasts the restart, and passes the address it read back with -a at every restart, so no other
// stack's server takes the address and the port while this one is down. Off Linux the address is
// 127.0.0.1, which every server there shares.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
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

// run parses args as nats-server parses its own command line (-a for the address, -p for the
// client port, -store_dir for the JetStream store), -callout-keys and -claim-for, starts the server
// with JetStream and its log on stdout, and returns once a signal has shut it down.
func run(args []string) error {
	flags := flag.NewFlagSet("natsd", flag.ContinueOnError)
	calloutDir := flags.String("callout-keys", "", "run behind an auth callout whose keys are in (or are made in) this directory")
	claimFor := flags.Int("claim-for", 0, "with no -a, claim the address for this process id (a script's own, which outlasts a restart) rather than for natsd's own")
	help := func() { flags.PrintDefaults() }
	opts, err := natsserver.ConfigureOptions(flags, args, help, help, help)
	if err != nil {
		return err
	}
	if opts == nil {
		// -v, -h or -help_tls: ConfigureOptions has printed what was asked for.
		return nil
	}
	host, release, err := listenHost(opts.Host, *claimFor)
	if err != nil {
		return err
	}
	defer release()
	if *calloutDir != "" {
		config, err := calloutConfig(*calloutDir)
		if err != nil {
			return err
		}
		if err := opts.ProcessConfigFile(config); err != nil {
			return fmt.Errorf("read %s: %w", config, err)
		}
	}
	testnats.ListenOn(opts, host)
	opts.JetStream = true
	opts.NoSigs = true
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return err
	}
	srv.ConfigureLogger()
	// The signals are caught before the server starts, so one that comes as soon as the log names
	// the address (a script stops a stack it has only just started) still shuts the server down
	// and releases the claim, rather than killing the process with the claim left behind.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		return fmt.Errorf("no connections accepted on %s:%d within %s; is the port taken?", opts.Host, opts.Port, readyTimeout)
	}
	<-signals
	srv.Shutdown()
	srv.WaitForShutdown()
	return nil
}

// listenHost is the address natsd listens on, and what releases it: requested, the address -a
// gave, when it is a loopback address, released by nobody since the caller owns it; or, when -a
// gave none, an address testport.Claim claims for claimFor, else for this process. natsd releases
// only a claim of its own, on its way out; a claim for another process is that process's, and is
// free again once that process has exited (testport.Claim). Any other -a is refused: natsd
// listens on loopback only.
func listenHost(requested string, claimFor int) (string, func(), error) {
	if requested != "" {
		if ip := net.ParseIP(requested); ip == nil || !ip.IsLoopback() {
			return "", nil, fmt.Errorf("-a %s is no loopback address; natsd listens on loopback only", requested)
		}
		return requested, func() {}, nil
	}
	dir, owner := testport.ClaimDir(), os.Getpid()
	if claimFor < 0 {
		return "", nil, fmt.Errorf("-claim-for %d names no process", claimFor)
	}
	if claimFor != 0 {
		owner = claimFor
	}
	host, err := testport.Claim(dir, owner)
	if err != nil {
		return "", nil, err
	}
	if owner != os.Getpid() {
		return host, func() {}, nil
	}
	return host, func() {
		if err := testport.Release(dir, host, owner); err != nil {
			fmt.Fprintf(os.Stderr, "natsd: release the claim on %s: %v\n", host, err)
		}
	}, nil
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
