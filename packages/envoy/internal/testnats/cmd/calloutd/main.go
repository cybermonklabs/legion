// Command calloutd runs the NATS auth service (internal/broker/natscallout) for the server
// natsd -callout-keys runs, deciding a session's token by what a broker's introspection route
// answers over HTTP. The secrets broker runs the same service in its replicas (cmd/broker) with
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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/broker/natscallout"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The wait for the service's subscription to reach the server has no bound of its own, as natsd's
// wait for its server has none: a server that is slow to answer, as on a loaded machine, is waited
// for for as long as it takes, and the caller's own wait for the "calloutd: ready" line, which is
// bounded by its test's deadline alone, decides when that is too long. A signal ends the wait.
//
// An introspection has no bound of its own either: the service hands each one the context of its
// decision, whose deadline is the request's budget (natscallout's deliver), so a route that does
// not answer is refused at that budget as the deployed service refuses it.

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
	if err := service.Ready(ctx); err != nil {
		// Only the signal ends Ready before the subscription reaches the server.
		return fmt.Errorf("stopped before the service reached %s: %w", *server, err)
	}
	fmt.Println("calloutd: ready")
	<-ctx.Done()
	return nil
}

// introspector asks route what the broker knows of a token: an RFC 7662 form POST, answered by
// the broker's own wire (oauthwire.Introspection). A route that does not answer 200 with that
// shape is an error, which the service refuses the connection over as a broker it cannot reach.
func introspector(route string) func(context.Context, string) (oauthwire.Introspection, error) {
	client := &http.Client{}
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
		if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&answer); err != nil {
			return oauthwire.Introspection{}, fmt.Errorf("read the introspection answer: %w", err)
		}
		return answer, nil
	}
}
