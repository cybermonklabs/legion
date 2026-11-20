package restatex

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
)

// reportEvery is how often a service process prints a reading of itself. The driver
// keeps the newest of each and reads it at the moment it cares about, which is how an
// experiment can say what the process held while an invocation was waiting.
const reportEvery = 300 * time.Millisecond

// inFlight is how many handler bodies this process is inside. Restate drives handlers
// over HTTP, so a handler that is not being invoked holds nothing in this process at
// all -- and showing that this count is zero while an invocation waits is the point of
// half of experiment 11.
var inFlight atomic.Int64

// EnterHandler records that this process is inside a handler body, and returns the
// function that records leaving it.
func EnterHandler() func() {
	inFlight.Add(1)
	return func() { inFlight.Add(-1) }
}

// Serve serves the given service definitions on listener, the loopback socket its driver
// handed over (StartService), until the context ends. It serves as the SDK's own Start
// does, unencrypted HTTP/2, on that socket instead of one it would bind itself, so the
// port stays the one the driver held. It prints the process's pid as its first field
// line, because the driver's whole measurement is which pid it killed, and then prints a
// reading of the process -- its goroutines and its in-flight handlers -- every few
// hundred milliseconds.
func Serve(ctx context.Context, listener net.Listener, definitions ...restate.ServiceDefinition) error {
	if err := RequireLoopback("-listen-fd", listener.Addr().String()); err != nil {
		listener.Close()
		return err
	}
	// The SDK's own chatter goes to stderr at warning level, so it never crowds the
	// field lines the driver reads from stdout.
	logger := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	endpoint := server.NewRestate().WithLogger(logger, true)
	for _, definition := range definitions {
		endpoint.Bind(definition)
	}
	handler, err := endpoint.Handler()
	if err != nil {
		listener.Close()
		return err
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{Handler: handler, Protocols: &protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	Field("pid", os.Getpid())
	go report(ctx)
	return httpServer.Serve(listener)
}

// report prints what this process holds, again and again, until the context ends.
func report(ctx context.Context) {
	ticker := time.NewTicker(reportEvery)
	defer ticker.Stop()
	for {
		Field("goroutines", runtime.NumGoroutine())
		Field("handlers-in-flight", inFlight.Load())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
