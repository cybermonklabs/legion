// Command natsprobe tries one subject on a NATS hub whose connections the auth service admits
// (internal/cipher/natscallout), as a session, as a service or with no credential at all, and
// says what the hub answered:
//
//	natsprobe --servers <url> (--subscribe <subject> | --publish <subject>)
//	          [--service-token-file <path> | --service-google-audience <audience> | --no-credential]
//	          [--wait 3s]
//
// With no credential flag it connects as a session: it trades this machine's identity for one of
// the broker's access tokens, as every Go client of the broker does (AGENT_SECRETS_URL,
// AGENT_SECRETS_MACHINE_IDENTITY and its variables, AGENT_SECRETS_SESSION, AGENT_SECRETS_PROJECTS;
// internal/cipherclient), under a key it makes for this run, and presents it with
// natscred.Session. With a service flag it presents that platform token with natscred.Service, and
// with --no-credential nothing, as a client holding no credential does.
//
// It prints `connected as <name>`, the name the hub reports the connection by for a session
// (session/<id>) and the account its token names for a service, then one of: `served <subject>`,
// exit 0; `refused: <the server's text>`, exit 3, when the hub refused the subscribe or publish;
// `not admitted: <the server's text>`, exit 4, when the hub refused the connection, its
// Authorization Violation (nats.ErrAuthorization). A usage error exits 2 and any other failure 1,
// a connection that fails any other way, such as a hub that is not listening, included; a
// session's credential tried on a hub with no auth service, which sends no nonce to sign, is such
// a failure, and names the flags of the credentials that hub takes. It prints no token.
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

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/natscred"
)

const (
	exitServed      = 0
	exitFailed      = 1
	exitUsage       = 2
	exitRefused     = 3
	exitNotAdmitted = 4
)

// connectTimeout bounds the connection, the trade a session's signature waits for included.
const connectTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("natsprobe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	servers := flags.String("servers", "", "the hub's NATS URLs, comma separated")
	subscribe := flags.String("subscribe", "", "the subject to subscribe to")
	publish := flags.String("publish", "", "the subject to publish to")
	tokenFile := flags.String("service-token-file", "", "connect as a service presenting the platform token in this file")
	googleAudience := flags.String("service-google-audience", "", "connect as a service presenting this VM's Google identity token for this audience")
	none := flags.Bool("no-credential", false, "connect presenting no credential")
	wait := flags.Duration("wait", 3*time.Second, "how long to wait for the hub to refuse the subscribe or publish")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitServed
		}
		return exitUsage
	}
	usage := func(message string) int {
		fmt.Fprintf(stderr, "natsprobe: %s\n", message)
		return exitUsage
	}
	switch {
	case *servers == "":
		return usage("--servers is required")
	case (*subscribe == "") == (*publish == ""):
		return usage("name one of --subscribe and --publish")
	case countSet(*tokenFile != "", *googleAudience != "", *none) > 1:
		return usage("name at most one of --service-token-file, --service-google-audience and --no-credential")
	case *wait <= 0:
		return usage("--wait must be positive")
	}

	option, name, err := credential(getenv, *tokenFile, *googleAudience, *none)
	if err != nil {
		fmt.Fprintf(stderr, "natsprobe: %v\n", err)
		return exitFailed
	}
	refusals := make(chan error, 8)
	nc, err := nats.Connect(*servers, option, nats.Name("natsprobe"), nats.Timeout(connectTimeout), nats.NoReconnect(),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case refusals <- err:
			default:
			}
		}))
	if err != nil {
		// Only the hub's own refusal of the CONNECT is an answer of the hub's (nats.go,
		// sendConnect: an Authorization Violation is ErrAuthorization); any other failure, a hub
		// that is not listening included, says nothing of what the hub admits.
		if errors.Is(err, nats.ErrAuthorization) {
			fmt.Fprintf(stdout, "not admitted: %s\n", serverText(err))
			return exitNotAdmitted
		}
		// nats.go wraps the signature callback's error ("error signing nonce: %w", nats.go:3087-3090)
		// and Connect returns it: the probe never sent a CONNECT, so the hub refused nothing, and
		// which credential such a hub takes is the probe's to say, by its flags.
		if errors.Is(err, cipherclient.ErrNoNonce) {
			fmt.Fprintf(stderr, "natsprobe: %v; name the credential it does take: --no-credential, --service-token-file or --service-google-audience\n", err)
			return exitFailed
		}
		fmt.Fprintf(stderr, "natsprobe: connect: %v\n", err)
		return exitFailed
	}
	defer nc.Close()
	fmt.Fprintf(stdout, "connected as %s\n", name)

	subject := *subscribe
	if subject != "" {
		_, err = nc.SubscribeSync(subject)
	} else {
		subject = *publish
		err = nc.Publish(subject, []byte("natsprobe"))
	}
	if err == nil {
		err = nc.Flush()
	}
	if err != nil {
		fmt.Fprintf(stderr, "natsprobe: %v\n", err)
		return exitFailed
	}
	select {
	case refused := <-refusals:
		fmt.Fprintf(stdout, "refused: %s\n", serverText(refused))
		return exitRefused
	case <-time.After(*wait):
		fmt.Fprintf(stdout, "served %s\n", subject)
		return exitServed
	}
}

// credential is the option the probe connects with and the name it says it connects as: none for
// none, a service's when a token file or a Google audience is named, and otherwise a session's,
// traded from this machine's identity.
func credential(getenv func(string) string, tokenFile, googleAudience string, none bool) (nats.Option, string, error) {
	if none {
		return func(*nats.Options) error { return nil }, "no credential", nil
	}
	if tokenFile != "" || googleAudience != "" {
		reader := natscred.ServiceFile(tokenFile)
		if googleAudience != "" {
			reader = natscred.ServiceGoogle(googleAudience)
		}
		token, err := reader.Read()
		if err != nil {
			return nil, "", err
		}
		account, err := natscred.Identity(token)
		if err != nil {
			return nil, "", err
		}
		option, _ := natscred.Service(reader)
		return option, "service " + account, nil
	}
	subject, err := cipherclient.MachineIdentity(getenv)
	if err != nil {
		return nil, "", err
	}
	if subject == nil {
		return nil, "", fmt.Errorf("%s names no identity, so this machine has no broker token to connect as a session with; name a service token or --no-credential instead", cipherclient.IdentityVar)
	}
	session := cipherclient.TradeSession(getenv, "")
	if session == "" {
		return nil, "", fmt.Errorf("%s names no session to connect as", cipherclient.SessionVar)
	}
	projects, err := cipherclient.TradeProjects(getenv)
	if err != nil {
		return nil, "", err
	}
	key, err := proof.NewKey()
	if err != nil {
		return nil, "", err
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(getenv(cipherclient.BrokerURLVar)), Key: key, Subject: subject, Session: session,
		Projects: projects, NoLauncherCredential: true, HTTP: cipherclient.HTTPClient(),
	})
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if _, err := client.Token(ctx); err != nil {
		return nil, "", fmt.Errorf("trade for session %s: %w", session, err)
	}
	option, _ := natscred.Session(client)
	return option, "session/" + session, nil
}

// countSet is how many of set are true.
func countSet(set ...bool) int {
	count := 0
	for _, each := range set {
		if each {
			count++
		}
	}
	return count
}

// serverText is the server's own text of err, without nats.go's prefix: `Permissions Violation for
// Subscription to "<subject>"` or `Authorization Violation`.
func serverText(err error) string {
	text := strings.TrimPrefix(err.Error(), "nats: ")
	text = strings.TrimPrefix(text, "permissions violation: ")
	return strings.Trim(text, "'")
}
