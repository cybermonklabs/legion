package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/eval"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// refuseSessionsDatabase refuses a database URL naming the database of the agent sessions of the
// machine the eval runs on, whose URL the file OMP_SESSION_SQL_DSN_FILE names holds: the eval
// creates and drops databases on the server it is given, and that one is never among them. A URL
// is that database when it is the file's text, or names the same host, port and database.
func refuseSessionsDatabase(databaseURL string) error {
	path := os.Getenv("OMP_SESSION_SQL_DSN_FILE")
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	sessions := strings.TrimSpace(string(data))
	if sessions == "" {
		return nil
	}
	if strings.TrimSpace(databaseURL) == sessions {
		return errors.New("the database URL is the agent sessions' database (OMP_SESSION_SQL_DSN_FILE): give the eval a test Postgres")
	}
	ours, errOurs := pgx.ParseConfig(databaseURL)
	theirs, errTheirs := pgx.ParseConfig(sessions)
	if errOurs == nil && errTheirs == nil && ours.Host == theirs.Host && ours.Port == theirs.Port && ours.Database == theirs.Database {
		return errors.New("the database URL names the agent sessions' database (OMP_SESSION_SQL_DSN_FILE): give the eval a test Postgres")
	}
	return nil
}

// databases are the eval's own databases on one Postgres server: a template holding the committed
// seed, and a copy of it for each try, so every try starts on the seed as committed. mu orders the
// tries' writes on the one admin connection, and on made, the databases close drops.
type databases struct {
	mu       sync.Mutex
	admin    *pgx.Conn
	base     *url.URL
	template string
	prefix   string
	made     []string
}

// newDatabases connects to the server databaseURL names, a postgres:// URL, makes the template,
// migrates it and writes the seed into it.
func newDatabases(ctx context.Context, databaseURL string, seed *eval.Seed) (*databases, error) {
	base, err := url.Parse(databaseURL)
	if err != nil || (base.Scheme != "postgres" && base.Scheme != "postgresql") {
		return nil, errors.New("the database URL is no postgres:// URL")
	}
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the database server: %w", err)
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	d := &databases{admin: admin, base: base, prefix: "going_eval_" + hex.EncodeToString(suffix)}
	d.template = d.prefix + "_seed"
	if err := d.create(ctx, d.template, ""); err != nil {
		d.close(ctx, false)
		return nil, err
	}
	database, err := store.Open(ctx, d.url(d.template))
	if err != nil {
		d.close(ctx, false)
		return nil, fmt.Errorf("open the seed's database: %w", err)
	}
	err = database.Migrate(ctx)
	if err == nil {
		_, err = eval.WriteSeed(ctx, database, seed)
	}
	database.Pool.Close()
	if err != nil {
		d.close(ctx, false)
		return nil, fmt.Errorf("write the seed's database: %w", err)
	}
	return d, nil
}

// url is the URL of the eval's database name: the URL the eval was given, naming that database.
func (d *databases) url(name string) string {
	u := *d.base
	u.Path, u.RawPath = "/"+name, ""
	return u.String()
}

// create makes the database name, a copy of template where template is not "", copied file by
// file rather than through the write-ahead log, and with JIT off, as production's database runs
// (testdb.WithoutJIT; docs/testing.md, "A database a Go test creates"). A copy does not take its
// template's settings, so the template and each try's copy are each set as they are made.
func (d *databases) create(ctx context.Context, name, template string) error {
	quoted := pgx.Identifier{name}.Sanitize()
	statement := "create database " + quoted + " strategy file_copy"
	if template != "" {
		statement += " template " + pgx.Identifier{template}.Sanitize()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.admin.Exec(ctx, statement); err != nil {
		return fmt.Errorf("create the database %s: %w", name, err)
	}
	d.made = append(d.made, name)
	if _, err := d.admin.Exec(ctx, testdb.WithoutJIT(quoted)); err != nil {
		return fmt.Errorf("turn JIT off on the database %s: %w", name, err)
	}
	return nil
}

// copyFor makes a database for one try, a copy of the seed's.
func (d *databases) copyFor(ctx context.Context, try string) (string, error) {
	name := d.prefix + "_" + try
	return name, d.create(ctx, name, d.template)
}

// drop drops one of the eval's databases.
func (d *databases) drop(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.admin.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
		return fmt.Errorf("drop the database %s: %w", name, err)
	}
	d.forget(name)
	return nil
}

// keep leaves one of the eval's databases on the server when the eval ends.
func (d *databases) keep(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forget(name)
}

// forget takes name off the databases close drops; d.mu is held.
func (d *databases) forget(name string) {
	for i, made := range d.made {
		if made == name {
			d.made = append(d.made[:i], d.made[i+1:]...)
			break
		}
	}
}

// close drops every database the eval made that it does not keep, none where keep, and closes
// the connection.
func (d *databases) close(ctx context.Context, keep bool) {
	d.mu.Lock()
	left := append([]string{}, d.made...)
	d.mu.Unlock()
	for _, name := range left {
		if !keep {
			_ = d.drop(ctx, name)
		}
	}
	_ = d.admin.Close(ctx)
}

// chatSettings are the chat model's settings the eval reads from its own environment and gives
// its server, under the server's own names (cmd/dispatch's DISPATCH_AZURE_CHAT_* and the keyless
// Azure sign-in), with the endpoint put through the eval's chat proxy.
type chatSettings struct {
	Endpoint, Deployment, APIVersion, Scope string
	TenantID, ClientID, TokenFile           string
}

// decisionSettingNames are the decision model's settings the eval reads from its own environment
// and gives its server under the same names (cmd/dispatch's DISPATCH_DECISION_MODEL, its endpoint
// and its token file), each where it is set. With them, a run's agent asks the decision model Jev
// what each turn's words ask, and the no-plan guard skips the check of a turn Jev reads as Ask;
// with none, the server names no decision model, and the report counts every turn the guard
// decided on as one with no line from Jev. The server reaches the endpoint directly, not through
// the chat proxy, trusting it by the system's authorities the proxy's bundle holds.
var decisionSettingNames = []string{"DISPATCH_DECISION_MODEL", "DISPATCH_DECISION_MODEL_ENDPOINT", "DISPATCH_DECISION_MODEL_TOKEN_FILE"}

// decisionSettings is each decision model setting set in the eval's environment, as NAME=value.
func decisionSettings() []string {
	var settings []string
	for _, name := range decisionSettingNames {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			settings = append(settings, name+"="+value)
		}
	}
	return settings
}

// server is one try's Dispatch server: its process, its origin, its log and the shared token its
// agents present.
type server struct {
	command *exec.Cmd
	origin  string
	log     *os.File
	exited  chan struct{}
}

// startServer starts the Dispatch server binary on the database databaseURL names, with its agent
// process on the seed's clock, now (seedClock), the chat model behind the proxy, the decision
// model the eval's environment names (decisionSettings), the fake Envoy, a person named by
// personHeader and a log in dir, and waits until it serves.
func startServer(ctx context.Context, binary, dir, databaseURL, agentToken string, chat chatSettings, proxy *chatProxy,
	envoyURL string, tokensPerMinute int, now time.Time) (*server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hold a port for the server: %w", err)
	}
	socket, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		return nil, fmt.Errorf("hold a port for the server: %w", err)
	}
	defer socket.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	for _, sub := range []string{"home", "web", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	envoyIdentity := filepath.Join(dir, "envoy-identity")
	if err := os.WriteFile(envoyIdentity, []byte("going-eval-fake-envoy-identity\n"), 0o600); err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		return nil, err
	}
	command := exec.Command(binary)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"),
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=" + agentToken,
		"DISPATCH_IDENTITY=header:" + personHeader,
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_FD=3",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(port),
		"DISPATCH_SERVER_URL=" + origin,
		"DISPATCH_WEB_DIST=" + filepath.Join(dir, "web"),
		"ENVOY_URL=" + envoyURL,
		"ENVOY_ACCOUNT_TOKEN_FILE=" + envoyIdentity,
		"DISPATCH_ANSWERER=azure",
		"DISPATCH_AZURE_CHAT_ENDPOINT=" + proxy.endpoint(),
		"DISPATCH_AZURE_CHAT_DEPLOYMENT=" + chat.Deployment,
		"DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE=" + strconv.Itoa(tokensPerMinute),
		"DISPATCH_AZURE_TENANT_ID=" + chat.TenantID,
		"DISPATCH_AZURE_CLIENT_ID=" + chat.ClientID,
		"DISPATCH_AZURE_FEDERATED_TOKEN_FILE=" + chat.TokenFile,
		"SSL_CERT_FILE=" + proxy.bundle,
	}
	command.Env = append(command.Env, seedClock(now)...)
	command.Env = append(command.Env, decisionSettings()...)
	if chat.APIVersion != "" {
		command.Env = append(command.Env, "DISPATCH_AZURE_CHAT_API_VERSION="+chat.APIVersion)
	}
	if chat.Scope != "" {
		command.Env = append(command.Env, "DISPATCH_AZURE_CHAT_SCOPE="+chat.Scope)
	}
	command.ExtraFiles = []*os.File{socket}
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("start the server: %w", err)
	}
	s := &server{command: command, origin: origin, log: log, exited: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(s.exited)
	}()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/healthz", nil)
		if err != nil {
			s.stop()
			return nil, err
		}
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return s, nil
			}
		}
		select {
		case <-s.exited:
			s.stop()
			return nil, fmt.Errorf("the server exited while it started: see %s", log.Name())
		case <-ctx.Done():
			s.stop()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			s.stop()
			return nil, fmt.Errorf("the server did not serve within 3 minutes: see %s", log.Name())
		}
	}
}

// stop stops the server as a deploy does, with SIGTERM, and kills its process group (its agent
// with it) if it has not ended within a minute.
func (s *server) stop() {
	_ = s.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(time.Minute):
		_ = syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL)
		<-s.exited
	}
	_ = syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL)
	_ = s.log.Close()
}

// seedClock is the settings that put a try's agent on the seed's clock: the server's test hooks,
// under which alone it takes DISPATCH_TEST_AGENT_NOW, and now, the seed's now, as the time each
// run's model is told and every relative time the agent gives it is written from. The seed writes
// its times for that day, so they read as of that day, as the judge, built with the seed's now,
// reads them; the server's own leases and timeouts stay on the clock.
func seedClock(now time.Time) []string {
	return []string{"DISPATCH_TEST_HOOKS=1", "DISPATCH_TEST_AGENT_NOW=" + now.UTC().Format(time.RFC3339Nano)}
}

// buildServer builds cmd/dispatch into dir, from the envoy module the working directory is in.
func buildServer(ctx context.Context, dir string) (string, error) {
	binary := filepath.Join(dir, "envoy-dispatch")
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/dispatch")
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("build the server (go build ./cmd/dispatch, from packages/envoy): %w", err)
	}
	return binary, nil
}
