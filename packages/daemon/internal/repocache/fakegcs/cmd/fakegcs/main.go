// Command fakegcs serves package fakegcs's double of Cloud Storage, IAM signBlob and the GCE
// metadata server on one address, for a process that is no Go test: the burst Job runs it in a
// container of its own beside the cache and the fetches, points the cache at it with
// STORAGE_EMULATOR_HOST and GCE_METADATA_HOST, and compiles it, with the package, from the tested
// head with the worker image's Go, as package fakegcs's documentation says.
//
// It listens on -listen (a loopback address and a port the kernel chooses, by default), keeps its
// objects under -root, which must exist or be creatable and which it never removes, answers as the
// service account -email, and prints the host:port it listens on as the first line of its standard
// output, so a script that started it on port 0 reads where to point its clients. It serves until
// SIGINT or SIGTERM, then stops and exits 0; a failure to start or to serve exits 1, with why on
// standard error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
)

// main reads the flags, starts the double and serves it until a signal or a failure.
func main() {
	listen := flag.String("listen", "127.0.0.1:0", "the address to listen on, host:port; port 0 lets the kernel choose one")
	root := flag.String("root", "", "the scratch directory to keep objects in (required)")
	email := flag.String("email", fakegcs.DefaultEmail, "the service account the metadata server answers as and signBlob signs for")
	flag.Parse()
	if err := run(*listen, *root, *email); err != nil {
		fmt.Fprintln(os.Stderr, "fakegcs:", err)
		os.Exit(1)
	}
}

// run serves the double on listen, over root, as email, until SIGINT or SIGTERM ends it (nil) or
// serving fails (its error).
func run(listen, root, email string) error {
	if root == "" {
		return errors.New("-root names no scratch directory to keep objects in")
	}
	if flag.NArg() > 0 {
		return fmt.Errorf("takes no arguments, not %q", flag.Args())
	}
	double, err := fakegcs.New(fakegcs.Options{Root: root, Email: email})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	running := double.Serve(listener)
	fmt.Println(running.Addr())
	signalled, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- running.Wait() }()
	select {
	case <-signalled.Done():
		return running.Close()
	case err := <-served:
		return fmt.Errorf("serve on %s: %w", running.Addr(), err)
	}
}
