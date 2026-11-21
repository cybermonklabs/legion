// Command jetstream-account-move moves a hub's JetStream streams and buckets out of the global
// account, $G, into the account a private render keeps JetStream in (core.JetStreamAccount,
// JETSTREAM, in packages/envoy/infra/core), keeping every message, sequence and durable consumer,
// and reads the result. One subcommand is one step, run as a one-off Job by an operator user of the
// account the step reads:
//
//	jetstream-account-move fence    --record <file>              (in $G, before the layout)
//	jetstream-account-move snapshot --record <file> --dir <dir>  (in $G)
//	jetstream-account-move layout   --record <file>              (in JETSTREAM, after the layout's apply)
//	jetstream-account-move restore  --record <file> --dir <dir>  (in JETSTREAM)
//	jetstream-account-move verify   --record <file>              (wherever the streams are)
//	jetstream-account-move switch   --record <file>              (as a client in ENVOY, after the broker names it)
//	jetstream-account-move rollback --record <file> --dir <dir>  (in $G, after the merge base's render is back)
//	jetstream-account-move probe    [--remover-seed-file <file>] (as a listener, after the switch)
//
// each with the connection flags --servers, --nkey-seed-file or --token-file, and --inbox-prefix.
//
// fence seals every stream of the account, the notification stream, each KV bucket and
// ENVOY_MEASURE among them, after which nats-server refuses every publish and KV write to it with
// an error ("invalid operation on sealed stream"), and writes the record: each stream's first and
// last sequence, message count, per-subject counts, identity (bus.StreamIDMetadataKey) and durable
// consumers. snapshot takes each stream's JetStream snapshot, consumers included. restore restores
// each into the account it runs in with the seal lifted, and verifies it against the record.
// Nothing between the fence and the switch accepts a write: the seal refuses it until the layout,
// and after the layout $G has no JetStream, so every write is refused with an error, never accepted
// and lost (Dispatch's outbox retries each until it is stored).
//
// rollback is the way back before the switch: nats-server refuses to lift a seal
// (server/stream.go:2578-2580, "stream configuration update can not unseal a sealed stream"), so
// once the merge base's render is back and $G's streams come back from their store still sealed,
// it deletes each and restores it from the move's own snapshot with the seal lifted, and verifies
// it against the record as restore does.
//
// Every line it prints names a stream and what the step found. A step's last line is its verdict:
// the step's own word (FENCED, SNAPSHOT TAKEN, LAYOUT APPLIED, MOVED, VERIFIED, SWITCHED, ROLLED
// BACK, GATE OPEN), exit 0; GATE SHUT, exit 1, when a stream differs from the record or a probe found
// a stored answer; UNKNOWN, exit 3, when a read or a write it needed could not be made. A usage
// error exits 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/natscred"
)

// The verdict exit codes.
const (
	exitOK      = 0
	exitShut    = 1
	exitUsage   = 2
	exitUnknown = 3
)

// requestTimeout bounds each JetStream API request but a restore's last, which the server answers
// once it has written the whole stream (restoreTimeout).
const requestTimeout = 30 * time.Second

// restoreTimeout bounds the last request of a restore: the server writes the stream before it
// answers.
const restoreTimeout = 10 * time.Minute

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// connection is how a step reaches the hub: its servers and its credential.
type connection struct {
	servers     string
	seedFile    string
	tokenFile   string
	inboxPrefix string
}

func (c *connection) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.servers, "servers", "", "the hub's NATS URLs, comma-separated")
	fs.StringVar(&c.seedFile, "nkey-seed-file", "", "a file holding the nkey seed of the user this step connects as")
	fs.StringVar(&c.tokenFile, "token-file", "", "a file holding the platform token the auth service admits this step by")
	fs.StringVar(&c.inboxPrefix, "inbox-prefix", "", "the reply prefix the credential's grant subscribes to; nats.go's _INBOX when empty")
}

// dial connects as c says, refusing a missing or doubled credential before it dials.
func (c connection) dial(name string) (*nats.Conn, error) {
	if c.servers == "" {
		return nil, usageError("--servers is required")
	}
	options := []nats.Option{nats.Name(name), nats.NoReconnect(), nats.Timeout(10 * time.Second)}
	switch {
	case c.seedFile != "" && c.tokenFile != "":
		return nil, usageError("--nkey-seed-file and --token-file are both set; a step connects as one identity")
	case c.seedFile != "":
		option, err := seedOption(c.seedFile)
		if err != nil {
			return nil, err
		}
		options = append(options, option)
	case c.tokenFile != "":
		option, _ := natscred.Service(natscred.ServiceFile(c.tokenFile))
		options = append(options, option)
	default:
		return nil, usageError("--nkey-seed-file or --token-file is required")
	}
	if c.inboxPrefix != "" {
		options = append(options, nats.CustomInboxPrefix(c.inboxPrefix))
	}
	return nats.Connect(c.servers, options...)
}

// seedOption is the nkey user credential the file at path holds. No error quotes the file's
// content.
func seedOption(path string) (nats.Option, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the nkey seed file %s: %w", path, err)
	}
	user, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(raw))))
	if err != nil {
		return nil, fmt.Errorf("the file %s holds no nkey user seed", path)
	}
	public, err := user.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("the file %s holds no nkey user seed", path)
	}
	return nats.Nkey(public, user.Sign), nil
}

// usage is a usage error's message.
type usage string

func (u usage) Error() string { return string(u) }

func usageError(format string, args ...any) error { return usage(fmt.Sprintf(format, args...)) }

// out prints a step's lines and ends it with its verdict.
type out struct{ w io.Writer }

func (o out) line(format string, args ...any) { fmt.Fprintf(o.w, format+"\n", args...) }

// verdict prints the step's last line and returns its exit code: word when nothing shut or was
// unknown, GATE SHUT when any line shut it, UNKNOWN otherwise.
func (o out) verdict(word string, shut, unknown int) int {
	switch {
	case shut > 0:
		o.line("GATE SHUT")
		return exitShut
	case unknown > 0:
		o.line("UNKNOWN")
		return exitUnknown
	}
	o.line("%s", word)
	return exitOK
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: jetstream-account-move fence|snapshot|layout|restore|verify|switch|rollback|probe [flags]")
		return exitUsage
	}
	step, args := args[0], args[1:]
	fs := flag.NewFlagSet("jetstream-account-move "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connection
	conn.flags(fs)
	record := fs.String("record", "", "the fence record: written by fence, read by every later step")
	dir := fs.String("dir", "", "the directory the snapshots are written to and restored from")
	var probe probeOptions
	probe.flags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	o := out{stdout}
	code, err := runStep(ctx, step, conn, *record, *dir, probe, o)
	var u usage
	switch {
	case errors.As(err, &u):
		fmt.Fprintf(stderr, "jetstream-account-move %s: %s\n", step, u)
		return exitUsage
	case err != nil:
		o.line("UNKNOWN %s: %v", step, err)
		o.line("UNKNOWN")
		return exitUnknown
	}
	return code
}

// runStep runs one step. An error is a step that could not start: a usage error, a connection
// refused, a record it could not read or write.
func runStep(ctx context.Context, step string, conn connection, record, dir string, probe probeOptions, o out) (int, error) {
	needs := func(names ...string) error {
		for _, name := range names {
			if (name == "record" && record == "") || (name == "dir" && dir == "") {
				return usageError("--%s is required", name)
			}
		}
		return nil
	}
	var steps = map[string]struct {
		needs []string
		run   func(*nats.Conn) (int, error)
	}{
		"fence":    {[]string{"record"}, func(nc *nats.Conn) (int, error) { return fence(ctx, nc, record, o) }},
		"snapshot": {[]string{"record", "dir"}, func(nc *nats.Conn) (int, error) { return snapshot(ctx, nc, record, dir, o) }},
		"layout":   {[]string{"record"}, func(nc *nats.Conn) (int, error) { return layout(ctx, nc, record, o) }},
		"restore":  {[]string{"record", "dir"}, func(nc *nats.Conn) (int, error) { return restore(ctx, nc, record, dir, false, o) }},
		"verify":   {[]string{"record"}, func(nc *nats.Conn) (int, error) { return verifyStep(ctx, nc, record, o) }},
		"switch":   {[]string{"record"}, func(nc *nats.Conn) (int, error) { return switched(ctx, nc, record, o) }},
		"rollback": {[]string{"record", "dir"}, func(nc *nats.Conn) (int, error) { return restore(ctx, nc, record, dir, true, o) }},
		"probe":    {nil, func(nc *nats.Conn) (int, error) { return probe.run(ctx, nc, conn, o) }},
	}
	s, ok := steps[step]
	if !ok {
		return exitUsage, usageError("no step %q: fence, snapshot, layout, restore, verify, switch, rollback or probe", step)
	}
	if err := needs(s.needs...); err != nil {
		return exitUsage, err
	}
	nc, err := conn.dial("jetstream-account-move " + step)
	if err != nil {
		return exitUnknown, err
	}
	defer nc.Close()
	return s.run(nc)
}
