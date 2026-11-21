package streams

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natscred"
)

// measureInbox is the inbox prefix the measurement job's grant subscribes (natsgrant.Measure).
const measureInbox = "_INBOX_envoy_measure"

// commandUsage is `envoy-infra streams`' usage.
const commandUsage = `usage:
  envoy-infra streams check --nats-url <urls> (--token-file <file> | --nkey-seed-file <file>) [settings]

check runs as the measurement job: the token file holds the platform token the hub's auth service
admits it by (user service); a static hub's measurement user connects by its nkey seed instead.
streams update and streams census are the writer hold's held commands, registered with it, which
write the hold journal's entries; update makes every stream check reports missing or below its
copies. Exit: 0 clean, 1 a failure, 2 UNKNOWN or a usage error.
settings: --replicas (ENVOY_NATS_REPLICAS, default 1), --stream-max-bytes (ENVOY_STREAM_MAX_BYTES),
          --session-max-age (ENVOY_SESSION_MAX_AGE)`

// Command runs `envoy-infra streams check` with args, the words after streams, and returns its exit
// code: the report's (Report.ExitCode), and 2 for a usage error. `streams update` and `streams
// census` are the writer hold's held commands, which run Update and Census inside a hold of their
// own workflow and write its journal's intent, done and stop entries; this command does not take
// them.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
		fmt.Fprintln(stderr, commandUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	word := args[0]
	flags := flag.NewFlagSet("envoy-infra streams "+word, flag.ContinueOnError)
	flags.SetOutput(stderr)
	natsURLs := flags.String("nats-url", "", "the hub's NATS URLs, comma-separated")
	tokenFile := flags.String("token-file", "", "the platform token the hub's auth service admits the measurement job by")
	seedFile := flags.String("nkey-seed-file", "", "a static hub's measurement user's nkey seed")
	replicas := flags.String("replicas", os.Getenv(bus.ReplicasEnvVar), "the deployment's replica count")
	maxBytes := flags.String("stream-max-bytes", os.Getenv(bus.StreamMaxBytesEnvVar), "the notification stream's byte limit")
	maxAge := flags.String("session-max-age", os.Getenv(bus.SessionMaxAgeEnvVar), "the session bucket's age")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "envoy-infra streams %s: "+format+"\n", append([]any{word}, a...)...)
		fmt.Fprintln(stderr, commandUsage)
		return 2
	}
	if word != "check" {
		return usage("the word is check; streams update and streams census are the writer hold's held commands")
	}
	settings, err := readSettings(*replicas, *maxBytes, *maxAge)
	if err != nil {
		return usage("%v", err)
	}
	switch {
	case *natsURLs == "":
		return usage("--nats-url is required")
	case *tokenFile == "" && *seedFile == "":
		return usage("--token-file or --nkey-seed-file is required")
	case *tokenFile != "" && *seedFile != "":
		return usage("name --token-file or --nkey-seed-file, not both")
	}
	conn, err := connect(*natsURLs, *tokenFile, *seedFile, measureInbox, "envoy-infra streams check")
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra streams check: connect: %v\n", err)
		return 2
	}
	defer conn.Close()
	return printReport(stdout, Check(ctx, conn, settings))
}

// readSettings reads the deployment's settings as each program reads them (bus.ParseNATSReplicas,
// bus.ParseStreamMaxBytes, bus.ParseSessionMaxAge).
func readSettings(replicas, maxBytes, maxAge string) (streamset.Settings, error) {
	var settings streamset.Settings
	var err error
	if settings.Replicas, err = bus.ParseNATSReplicas(replicas); err != nil {
		return settings, err
	}
	if settings.StreamMaxBytes, err = bus.ParseStreamMaxBytes(maxBytes); err != nil {
		return settings, err
	}
	if settings.SessionMaxAge, err = bus.ParseSessionMaxAge(maxAge); err != nil {
		return settings, err
	}
	return settings, nil
}

// connect connects to the hub at urls as the token file's identity (user service, the token as the
// password), or as the seed file's nkey user, asking on inbox.
func connect(urls, tokenFile, seedFile, inbox, name string) (*nats.Conn, error) {
	options := []nats.Option{nats.Name(name), nats.CustomInboxPrefix(inbox), nats.Timeout(10 * time.Second), nats.MaxReconnects(0)}
	if seedFile != "" {
		credential, err := nats.NkeyOptionFromSeed(seedFile)
		if err != nil {
			return nil, err
		}
		options = append(options, credential)
	} else {
		token, err := readToken(tokenFile)
		if err != nil {
			return nil, err
		}
		options = append(options, nats.UserInfo(natscred.ServiceUser, token))
	}
	return nats.Connect(urls, options...)
}

// readToken is the token path holds, trimmed; an empty file is refused naming the path.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s holds no token", path)
	}
	return token, nil
}

// printReport prints report's lines and returns its exit code.
func printReport(stdout io.Writer, report Report) int {
	for _, line := range report.Lines() {
		fmt.Fprintln(stdout, line)
	}
	return report.ExitCode()
}
