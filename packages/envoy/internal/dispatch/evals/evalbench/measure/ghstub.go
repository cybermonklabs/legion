package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// The GitHub stand-in of check k17 (dispatch://INFRA-3206, R15): the first paint of the Evals tab
// makes no request to GitHub. The bench's server runs with no GitHub App, so it could make none and
// a record of its requests would prove nothing; with the stand-in (run.sh's EVALBENCH_GITHUB_STUB=1)
// the server runs with a throwaway App whose API base is this server, which answers the App's
// installation lookup and its token mint, a request that reads a repository's history reaching a 404
// as it would reach an App with no access to it, and writes each request it is sent, whatever its
// answer, as one JSON line (githubRequest) to the log `measure paint` reads (checkGitHubStub).
//
//	measure ghstub -log <file> -port-file <file> [-listen 127.0.0.1:0]
//
// It listens on a loopback port the kernel chooses and writes the port to -port-file once it is
// listening, so the caller starts the server it stands in for only then, and it stops, with the log
// complete, at SIGTERM or SIGINT.

// githubRequest is one request the stand-in was sent: when it received it, its method, its path
// with its query, and what it asked to accept (the diff media type of a patch read, say).
type githubRequest struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Accept string    `json:"accept"`
}

var (
	// installationRoute is GET /repos/{owner}/{repo}/installation, the lookup of the installation
	// that covers a repository, and tokenRoute POST /app/installations/{id}/access_tokens, the
	// mint of its token: the two calls every read of a repository starts with.
	installationRoute = regexp.MustCompile(`^/repos/[^/]+/[^/]+/installation$`)
	tokenRoute        = regexp.MustCompile(`^/app/installations/[0-9]+/access_tokens$`)
)

// githubPermissions is what the stand-in's installation and its tokens hold: the App reads
// contents, pull requests and actions, so no read of a repository is refused for a missing grant
// before it reaches the stand-in's 404.
var githubPermissions = map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}

// githubStub is the stand-in's handler and its log.
type githubStub struct {
	mu  sync.Mutex
	log io.Writer
	now func() time.Time
}

// ServeHTTP records the request and answers it: the installation lookup, the token mint and the
// list of the App's installations (empty, so no read that walks every installation reads a
// repository), and 404 to every other route.
func (s *githubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.record(r); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && installationRoute.MatchString(r.URL.Path):
		s.answer(w, http.StatusOK, map[string]any{"id": 1, "app_slug": "evalbench", "account": map[string]string{"login": "evalbench"}, "permissions": githubPermissions})
	case r.Method == http.MethodPost && tokenRoute.MatchString(r.URL.Path):
		s.answer(w, http.StatusCreated, map[string]any{"token": "ghs_evalbench", "expires_at": s.now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": githubPermissions})
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		s.answer(w, http.StatusOK, []any{})
	default:
		s.answer(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// record writes the request's line to the log, one write for the whole line, before the request is
// answered, so a request the paint's window ends on is in the log whatever it answers.
func (s *githubStub) record(r *http.Request) error {
	line, err := json.Marshal(githubRequest{Time: s.now().UTC(), Method: r.Method, Path: r.URL.RequestURI(), Accept: r.Header.Get("Accept")})
	if err != nil {
		return fmt.Errorf("encode the request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write the request log: %w", err)
	}
	return nil
}

func (s *githubStub) answer(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// loopbackListen is the address the stand-in may listen on: a loopback host, so no machine but this
// one reaches it, and the App key its caller hands the server it stands in for is never sent past it.
func loopbackListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("-listen %q is no host:port: %w", address, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-listen %q is no loopback address: the stand-in answers the App's calls, and listens on this machine alone", address)
	}
	return nil
}

// serveGitHubStub serves the stand-in on listener, recording to log, until ctx ends, and answers
// once every request in flight has been answered and the server is closed.
func serveGitHubStub(ctx context.Context, listener net.Listener, log io.Writer) error {
	server := &http.Server{Handler: &githubStub{log: log, now: time.Now}, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("stop the stand-in: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// writePortFile writes the port listener holds to path, whole or not at all: a reader that finds
// the file has the port.
func writePortFile(path string, listener net.Listener) error {
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(port+"\n"), 0o644); err != nil {
		return fmt.Errorf("write the port: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish the port: %w", err)
	}
	return nil
}

// ghstub is the `measure ghstub` command: it answers the process's exit status.
func ghstub(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("ghstub", flag.ContinueOnError)
	flags.SetOutput(stderr)
	logPath := flags.String("log", "", "the file each request is written to, one JSON line each (required); it is emptied at the start")
	portPath := flags.String("port-file", "", "the file the port is written to once the stand-in listens (required)")
	listen := flags.String("listen", "127.0.0.1:0", "the loopback address to listen on; port 0 is a port the kernel chooses")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *logPath == "" || *portPath == "" {
		fmt.Fprintln(stderr, "usage: measure ghstub -log <file> -port-file <file> [-listen 127.0.0.1:0]")
		return 2
	}
	if err := loopbackListen(*listen); err != nil {
		fmt.Fprintf(stderr, "measure ghstub: %v\n", err)
		return 2
	}
	log, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "measure ghstub: open the log: %v\n", err)
		return 1
	}
	defer log.Close()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "measure ghstub: listen on %s: %v\n", *listen, err)
		return 1
	}
	defer listener.Close()
	// The port is published once the signal is handled, so a stop that follows it is never lost.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := writePortFile(*portPath, listener); err != nil {
		fmt.Fprintf(stderr, "measure ghstub: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "measure ghstub: answering the App's installation and token calls on %s, logging to %s\n", listener.Addr(), *logPath)
	if err := serveGitHubStub(ctx, listener, log); err != nil {
		fmt.Fprintf(stderr, "measure ghstub: %v\n", err)
		return 1
	}
	return 0
}

// readGitHubLog reads every request the stand-in logged to path, in the order it received them.
func readGitHubLog(path string) ([]githubRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read the GitHub stand-in's log: %w", err)
	}
	defer file.Close()
	var requests []githubRequest
	lines := bufio.NewScanner(file)
	lines.Buffer(nil, 1<<20)
	for number := 1; lines.Scan(); number++ {
		var request githubRequest
		if err := json.Unmarshal(lines.Bytes(), &request); err != nil {
			return nil, fmt.Errorf("read line %d of %s: %w", number, filepath.Base(path), err)
		}
		requests = append(requests, request)
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read the GitHub stand-in's log: %w", err)
	}
	return requests, nil
}

// checkGitHubStub is check k17: a read of the paint's page makes no request to GitHub. Each request
// the stand-in received between the paint's first request and the answer of its last is a FAIL
// line, naming the request and how long after the paint began it came, and a RESULT line counts the
// requests inside the paint's window and those outside it, which the server's own loops made before
// the paint began or after it ended, for information. It answers the two counts.
func checkGitHubStub(r *report, requests []githubRequest, measured []measuredCase) (inside, outside int) {
	if len(measured) == 0 {
		r.fail("k17: no read of the paint was measured, so no window to look for a request to GitHub in")
		return 0, 0
	}
	from, to := measured[0].t0, measured[len(measured)-1].t1
	for _, request := range requests {
		if request.Time.Before(from) || request.Time.After(to) {
			outside++
			continue
		}
		inside++
		r.fail("k17 the server made a request to GitHub during the paint: %s %s (Accept %q) at %s, %s after the paint's first request",
			request.Method, request.Path, request.Accept, request.Time.UTC().Format(time.RFC3339Nano), request.Time.Sub(from).Round(time.Millisecond))
	}
	r.result("k17 github_requests_in_the_paint=%d github_requests_outside_the_paint=%d window=%s..%s window_ms=%d", inside, outside,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), to.Sub(from).Milliseconds())
	return inside, outside
}
