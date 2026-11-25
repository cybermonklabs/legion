// Command calloutd runs the NATS auth service (internal/cipher/natscallout) for the server
// natsd -callout-keys runs, deciding a session's token by what a broker's introspection route
// answers over HTTP. The secrets broker runs the same service in its replicas (cmd/cipherd) with
// its own introspection in process; this stands in for it where a client's test holds the
// broker's answers itself: packages/envoy-client's src/__tests__/nats-credential.test.ts serves
// the token exchange and the introspection route, and so decides which token the service admits
// and when.
//
// It is a test fixture, not a product. It admits sessions alone and no service, reads the three
// seeds natsd wrote in the -keys directory, and lives under internal/testnats so nothing outside
// this module can import it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/redact"
)

// readyTimeout bounds the wait for the service's subscription to reach the server, so a server
// that never answers ends this command instead of leaving its caller waiting.
const readyTimeout = 10 * time.Second

// introspectTimeout bounds one introspection, under the service's own bound on a decision.
const introspectTimeout = 2 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "calloutd: %v\n", err)
		os.Exit(1)
	}
}

// run starts the service on the server -nats names with the keys in -keys, prints
// "calloutd: ready" once its subscription has reached the server, and returns once a signal has
// stopped it.
func run(args []string) error {
	flags := flag.NewFlagSet("calloutd", flag.ContinueOnError)
	keys := flags.String("keys", "", "the directory natsd -callout-keys keeps service.seed, issuer.seed and xkey.seed in")
	server := flags.String("nats", "", "the server's client URL")
	introspect := flags.String("introspect", "", "the broker's introspection route, which every session's token is asked of")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keys == "" || *server == "" || *introspect == "" {
		return errors.New("-keys, -nats and -introspect are each required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	service, err := natscallout.Start(ctx, natscallout.Settings{
		URLs:           []string{*server},
		SeedFile:       filepath.Join(*keys, "service.seed"),
		IssuerSeedFile: filepath.Join(*keys, "issuer.seed"),
		XKeySeedFile:   filepath.Join(*keys, "xkey.seed"),
	}, natscallout.Deps{
		Introspect: introspector(*introspect),
		ServiceToken: func(context.Context, string) (string, bool, time.Time, time.Time, error) {
			return "", false, time.Time{}, time.Time{}, errors.New("calloutd admits no service")
		},
		Log: slog.New(slog.NewTextHandler(os.Stdout, nil)),
	})
	if err != nil {
		return err
	}
	defer service.Close()
	ready, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := service.Ready(ready); err != nil {
		// The server by its host alone: -nats is unchecked, so its user information and query print
		// nowhere.
		return fmt.Errorf("the service did not reach %s within %s: %w", natscallout.HubHosts("-nats", []string{*server}), readyTimeout, err)
	}
	fmt.Println("calloutd: ready")
	<-ctx.Done()
	return nil
}

// introspector asks route what the broker knows of a token: an RFC 7662 form POST, answered by
// the broker's own wire (oauthwire.Introspection). A route that does not answer 200 with that
// shape is an error, which the service refuses the connection over as a broker it cannot reach.
func introspector(route string) func(context.Context, string) (oauthwire.Introspection, error) {
	client := &http.Client{Timeout: introspectTimeout}
	return func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		form := url.Values{"token": {token}}.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, route, strings.NewReader(form))
		if err != nil {
			return oauthwire.Introspection{}, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			return oauthwire.Introspection{}, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return oauthwire.Introspection{}, fmt.Errorf("the introspection route answered HTTP %d", response.StatusCode)
		}
		var answer oauthwire.Introspection
		read, err := redact.ReadAnswer(response, 64<<10)
		if err != nil {
			return oauthwire.Introspection{}, fmt.Errorf("read the introspection answer: %w", err)
		}
		if err := read.Decode(&answer); err != nil {
			return oauthwire.Introspection{}, fmt.Errorf("read the introspection answer: %w", err)
		}
		return answer, nil
	}
}
