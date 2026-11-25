package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// operatorCall is one command that reaches the daemon over the operator's bearer — each `legion
// claims` subcommand, each `legion project` subcommand, each `legion intake` subcommand, and
// `legion issue status <issue> <status>` from an operator shell: the flags every one of them takes
// — where the daemon is and the file holding the bearer — and the streams it reports on.
//
// The bearer is only ever a file's contents: no flag takes it as a value, so it is in no argv, and
// once read it goes into the request's Authorization header and nowhere else — no line these
// commands print carries it.
type operatorCall struct {
	cmd            *cobra.Command
	flags          *pflag.FlagSet
	tokenFile      *string
	configPath     *string
	port           *int
	stdout, stderr io.Writer
	// wait is the --wait flag, when the command takes one (withWait), and waited the bound the
	// whole call runs under: the flag's value, or defaultWait when the flag is left unset. until
	// is the moment that bound expires, fixed when check reads the call. Every request the command
	// makes is bounded by that one moment, so a command that reads the claims in a loop is bounded
	// by its --wait and not by --wait per request.
	wait   *time.Duration
	waited time.Duration
	until  time.Time
}

// defaultWait is --wait's default, the bound of a call that names none: a minute. The call reads
// it through programbound when the flag is left unset, so this package's tests can give it their
// own deadline: a stand-in daemon a loaded machine made slower than the minute would have the call
// refused, and the row would read the refusal.
var defaultWait = time.Minute

func init() {
	programbound.Register(&defaultWait)
}

// withWait gives the command a --wait flag bounding its whole call, which every `legion claims`
// subcommand takes: a command that does not answer is worse than one that refuses, and an
// operator waiting on a worker whose pod is coming up has waited minutes for a suspend with no
// answer at all (dispatch://INFRA-714 R10). The daemon's own work is bounded by the daemon's own
// deadlines: a call this bound ends says what it asked for and that the daemon goes on with it,
// and never cancels the decision the daemon is making.
func (c *operatorCall) withWait(usage string) {
	c.wait = c.flags.Duration("wait", defaultWait, usage)
}

// bound is ctx under the command's remaining --wait, and ctx itself for a command that takes no
// --wait. The deadline is the one check fixed, so every request shares it.
func (c *operatorCall) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.until.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, c.until)
}

// newOperatorCall gives cmd the three flags every operator command takes; tokenUsage is how
// --operator-token-file reads in its help.
func newOperatorCall(cmd *cobra.Command, tokenUsage string, stdout, stderr io.Writer) *operatorCall {
	flags := cmd.Flags()
	return &operatorCall{
		cmd:        cmd,
		flags:      flags,
		tokenFile:  flags.String("operator-token-file", "", tokenUsage),
		configPath: flags.String("config", "", "path to legion.yaml (default "+defaultConfigPath+")"),
		port:       flags.Int("port", 0, "port to reach, overriding the configured one"),
		stdout:     stdout,
		stderr:     stderr,
	}
}

// name is how the command's lines name it: `legion claims spawn`.
func (c *operatorCall) name() string { return c.cmd.CommandPath() }

// check refuses any required flag left out or empty, by name — a usage error, before anything is
// read or sent; the command's Args refuse a word that is not a flag. A false ok carries the exit
// code. A command that takes a --wait starts its bound here, before its first request, so the
// bound covers the whole call (bound).
func (c *operatorCall) check(required ...string) (code int, ok bool) {
	for _, name := range required {
		if c.flags.Lookup(name).Value.String() == "" {
			return refuse(c.cmd, "--%s is required", name), false
		}
	}
	if c.wait != nil && *c.wait > 0 {
		c.waited = *c.wait
		if !c.flags.Changed("wait") {
			c.waited = programbound.Of(&defaultWait)
		}
		c.until = time.Now().Add(c.waited)
	}
	return 0, true
}

// connect reads the bearer --operator-token-file names, when the command was given one, by
// config.ReadPrivateSecretPointer's rules — a file that cannot be read, holds nothing, or that its
// group or others can read is refused naming its path, never its contents — and finds the daemon
// the way `legion state` does (daemonAddress).
func (c *operatorCall) connect() (operator, bool) {
	var bearer string
	if *c.tokenFile != "" {
		read, err := config.ReadPrivateSecretPointer("--operator-token-file", *c.tokenFile)
		if err != nil {
			fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), err)
			return operator{}, false
		}
		bearer = read
	}
	address, err := daemonAddress(*c.configPath, *c.port)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), err)
		return operator{}, false
	}
	return operator{base: address, bearer: bearer}, true
}

// send makes one request of the daemon and reports its answer: a 2xx is handed to print, and
// anything else is the daemon's refusal, printed with its status and sentence, and the command
// fails.
func (c *operatorCall) send(ctx context.Context, op operator, method, path string, body any, print func(io.Writer, []byte) error) int {
	_, answer, ok := c.request(ctx, op, method, path, body)
	if !ok {
		return 1
	}
	return c.printAnswer(op, path, answer, print)
}

// request makes one request of the daemon and answers its 2xx status and body, the data the
// daemon served (Answer.StreamTo). A request that got no answer, or an answer outside 2xx — the
// daemon's refusal, with its status and sentence (refusal) — is reported and answers false.
//
// It runs under the command's --wait, which bounds the whole call rather than this one request
// (bound). What it asks for may wait on the runtime — a suspend or stop waits out the worker's
// stop grace, a resume that and a launch — so a bound that runs out is reported as what it is: the
// command refuses with a reason, and the daemon goes on with what it began under its own
// deadlines, which this bound never cancels.
func (c *operatorCall) request(ctx context.Context, op operator, method, path string, body any) (int, []byte, bool) {
	bounded, cancel := c.bound(ctx)
	defer cancel()
	answer, err := op.do(bounded, method, path, body)
	if err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			fmt.Fprintf(c.stderr, "%s: %s %s was not answered within the --wait bound of %s; the daemon goes on with it under its own deadlines. Read `legion claims list` to see where the claim stood, and ask again\n",
				c.name(), method, op.shown(path), c.waited)
			return 0, nil, false
		}
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), err)
		return 0, nil, false
	}
	if answer.Status()/100 != 2 {
		fmt.Fprintf(c.stderr, "%s: %s\n", c.name(), refusal(answer))
		return 0, nil, false
	}
	var served bytes.Buffer
	_, _ = answer.StreamTo(&served)
	return answer.Status(), served.Bytes(), true
}

// printAnswer hands a 2xx answer path served to print; one it cannot read fails the command.
func (c *operatorCall) printAnswer(op operator, path string, answer []byte, print func(io.Writer, []byte) error) int {
	if err := print(c.stdout, answer); err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer %s served: %v\n", c.name(), op.shown(path), err)
		return 1
	}
	return 0
}

// operator is a daemon's API root, and the operator bearer that opens its operator routes — ""
// for a request that authenticates otherwise (a grant in its body).
type operator struct {
	base   string
	bearer string
}

// errDaemonURL refuses a request of a daemon whose API root does not parse as an absolute URL with
// a host (urljoin.ErrBase). url.Parse's error, which http.NewRequest's carries, quotes the URL
// whole, its user information and query included, so the refusal names the URL by what it is.
var errDaemonURL = errors.New("the daemon's URL is not an absolute URL with a host (it is not printed)")

// urlOf is the URL a request of route asks: route joined onto the API root's path, the root's query
// kept ahead of route's own, its user information, host and port kept and its fragment, which no
// request sends, dropped (urljoin.Join). route appended to the root's text would sit inside the
// root's query or fragment, and the daemon would be asked another path. A root that does not parse
// is errDaemonURL; route is the CLI's own, a fixed route whose elements url.PathEscape wrote, which
// Join never refuses.
func (op operator) urlOf(route string) (*url.URL, error) {
	asked, err := urljoin.Join(op.base, route)
	if err != nil {
		return nil, errDaemonURL
	}
	return asked, nil
}

// shown is the URL a request of route asks (urlOf) as a line names it: through the URL rule
// (redact.URLCredentials), which removes its user information and its query, where the root can
// carry a credential — as do names it in the error of an answer it could not read, and
// oauth2call.Unanswered in the error of a request that got none. A root that does not parse is
// named by what it is.
func (op operator) shown(route string) string {
	asked, err := op.urlOf(route)
	if err != nil {
		return "the daemon's URL (it does not parse, and is not printed)"
	}
	return redact.URLCredentials(asked.String())
}

// shownRoot is the API root as a line names it, as shown names a request's URL: through the URL
// rule (redact.URLCredentials), and by what it is when it does not parse, since then the rule,
// which reads a URL from its "://", may find none to read and would leave the root as written.
func (op operator) shownRoot() string {
	if _, err := op.urlOf("/"); err != nil {
		return "the daemon's URL (it does not parse, and is not printed)"
	}
	return redact.URLCredentials(op.base)
}

// daemonClient is the client every request of the CLI to a daemon's API is made on (do, get):
// http.DefaultClient's, following no redirect (redirects.Refused). An operator request carries the
// operator bearer, which a followed redirect would hand the host the Location names, and the
// daemon answers none: a 3xx is its answer, refused by its status.
var daemonClient = redirects.Refused(http.DefaultClient)

// do is one request to the daemon, at route joined onto its API root (urlOf): body as JSON when it
// is not nil, the bearer as its Authorization when op carries one. It answers the daemon's answer,
// read through oauth2call.ReadAnswer with the record of the request, so a refusal prints with the
// bearer and any credential the request carried redacted (Answer.Words, refusal), and a caller
// decodes what it decides on as it came (Answer.Decode). Only a request that was not made, got no
// answer, or whose answer could not be read, is an error: an API root that does not parse is
// errDaemonURL, a joined URL url.Parse refuses again (url.URL.String can write text it refuses,
// such as an IPv6 zone escape) and a request that got no answer name their failure and the URL
// through oauth2call.Unanswered, and an answer that could not be read names the URL through the
// URL rule.
func (op operator) do(ctx context.Context, method, path string, body any) (oauth2call.Answer, error) {
	asked, err := op.urlOf(path)
	if err != nil {
		return oauth2call.Answer{}, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return oauth2call.Answer{}, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, asked.String(), reader)
	if err != nil {
		return oauth2call.Answer{}, oauth2call.Unanswered(err, nil, "")
	}
	if op.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+op.bearer)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := daemonClient.Do(request)
	if err != nil {
		return oauth2call.Answer{}, oauth2call.Unanswered(err, request, "")
	}
	defer response.Body.Close()
	answer, err := oauth2call.ReadAnswer(response, 64<<20)
	if err != nil {
		return oauth2call.Answer{}, fmt.Errorf("read the answer to %s %s: %w", method, redact.URLCredentials(request.URL.String()), err)
	}
	return answer, nil
}

// daemonAddress is the API root of the daemon a command reaches: the one LEGION_DAEMON_URL names —
// the daemon names it on every pane, and a pane has no legion.yaml — unless --config or --port
// says otherwise, and otherwise the configured bind and port (stateAddress).
func daemonAddress(configPath string, port int) (string, error) {
	if _, pane := os.LookupEnv("LEGION_DAEMON_URL"); pane && configPath == "" && port == 0 {
		return daemonURL(), nil
	}
	address, err := stateAddress(configPath, port)
	if err != nil {
		return "", err
	}
	return "http://" + address, nil
}

// daemonURL is the API root LEGION_DAEMON_URL names, or loopback on LEGION_DAEMON_PORT (13370 when
// unset) while it names none.
func daemonURL() string {
	if endpoint := os.Getenv("LEGION_DAEMON_URL"); endpoint != "" {
		return endpoint
	}
	port := os.Getenv("LEGION_DAEMON_PORT")
	if port == "" {
		port = "13370"
	}
	return "http://127.0.0.1:" + port
}

// refusal is an answer outside 2xx as the operator reads it: the status, and the sentence the
// daemon's `{"error"}` body carries — or the body itself when it carries none, so nothing the
// daemon said is lost. Whether the body carries a sentence is decided on the body as it came
// (Answer.Decode), since the rule's reading of a whole body is not readable where its sentence
// names a URL beside the JSON quote that ends it; the sentence and the body each print as the rule
// reads them.
func refusal(answer oauth2call.Answer) string {
	status := answer.Status()
	message := strings.TrimSpace(answer.Words().String())
	var refused oauth2call.CodedRefusal
	if answer.Decode(&refused) == nil && !refused.Error.IsEmpty() {
		message = refused.Error.String()
	}
	answered := fmt.Sprintf("the daemon answered %d %s", status, http.StatusText(status))
	if message == "" {
		return answered
	}
	return answered + ": " + message
}
