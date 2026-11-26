// Command dev-airlock-stands serves the airlock's stand-ins to the broker's local stack
// (scripts/dev-broker.sh and the airlock checks run beside it), so a check script drives the
// same stand-ins the Go tests do: the two HTTPS services S and T, which record every request
// they get and of which S answers a WebSocket upgrade at /socket; the stand-in GitHub and
// Google endpoints a github-app and a google-login secret are minted at, the GitHub one served
// a second time over TLS for a command behind the proxy; the stand-in
// Microsoft authority the azure kind reads a tenant's OpenID configuration from and exchanges
// at; a stand-in AWS service that verifies Signature Version 4 with the real test key and
// the real session token; the three the broker's model judge calls - a model endpoint, a
// Dispatch holding a few issues, and the Compute Engine metadata server the broker mints its
// own identity token at; the stand-in of Certificate Authority Service a broker pod asks for its
// proxy's intermediate; and the three of the anthropic kind - Anthropic's Admin API, a recording
// HTTPS service like S, and the two an anthropic-federation secret is minted at, IAM's
// generateIdToken and Anthropic's token exchange (anthropic.go). Every one of them but the last two
// is internal/cipher/proxyrules/proxyrulestest's, which is why one implementation answers both.
//
//	dev-airlock-stands [--dir <path>] [--address <stand>=<host:port> ...] [--host <stand>=<name> ...]
//	                   [--installation <owner>=<installation id> ...]
//	                   [--installation-permissions <owner>=<permission>:<level>,... ...]
//	                   [--google-lifetime <duration>]
//	                   [--aws-key <key id>=<secret key>] [--aws-session-token <token>]
//	                   [--records-address <host:port>]
//	                   [--clock-offset-file <path>]
//	dev-airlock-stands front --listen <host:port> [--backend <host:port> ...] [--backends-file <path>]
//	dev-airlock-stands enroll-pod --url <broker> --token-file <path> --pod-token-file <path>
//	                   --pod-uid <uid> --thumbprint <thumbprint> [--service <name>] [--session <id>]
//	dev-airlock-stands probe-private
//	dev-airlock-stands probe-bound
//	dev-airlock-stands load <load|tunnel|upload|gap|upgrade> [--proxy-file <path>] [--header-file <path>]
//	                   [--host <name>] ...
//
// The stack and its checks need five more small things of their own, and they are modes of this
// one command rather than commands of their own so that it builds one binary: `front`, one TCP
// address in front of several (front.go); `enroll-pod`, the two calls that enrol a pod session
// (enroll.go); and the three Go clients the checks drive, `probe-private` and `probe-bound`
// (probe.go) and `load` (load.go), which are here so that CI compiles and vets them. With no
// subcommand it serves the stands, which is what the rest of this comment is about.
//
// A stand is named s, t, github, github-tls, google, microsoft, aws, model, dispatch, metadata,
// ca, anthropic, anthropic-token or iam. Each binds 127.0.0.1 on a port the operating system picks
// unless --address names one, and each HTTPS stand's certificate names 127.0.0.1 and localhost
// unless --host adds names of its own - the host a kind binds a credential to, which the command's
// own TLS verification reads out of the certificate.
//
// The anthropic-token stand is Anthropic's token exchange, POST /v1/oauth/token: it answers an
// sk-ant-oat01- token of its own for an assertion the iam stand minted, with the expires_in a
// check sets while it runs at POST <records URL>/answers/anthropic, whose body is
// {"expires_in": <seconds>} (anthropic_answer_url; 3600 until a check calls it). The iam stand is
// generateIdToken, POST /v1/projects/-/serviceAccounts/{account}:generateIdToken, which takes this
// run's metadata access token as its bearer, the one the metadata stand answers on its
// access-token path, and answers an identity token for that account and the audience its body
// names. Each keeps what it minted: GET <records URL>/tokens/iam answers each identity token with
// its account and audience (iam_tokens_url), and GET <records URL>/tokens/anthropic each token
// with its expires_in (anthropic_tokens_url).
//
// The ca stand answers POST /v1/projects/{project}/locations/{location}/caPools/{pool}/certificates,
// the request BROKER_PROXY_TEST_CA_URL points a broker pod at, with an intermediate for the
// CSR's key signed by a test root of its own, under a policy: 7 days at most, starting 5 minutes
// before its issue, an authority of path length 0 for servers alone (serverAuth its one extended
// key usage), and the policy's names, the request's own until POST <ca URL>/policy, whose
// body is {"names": [...]}, gives it names, as an apply of the pool's issuance policy does. It
// dates each certificate by the clock --clock-offset-file moves, the file the brokers' clock
// reads. POST <ca URL>/fail makes it fail every request 503 until POST <ca URL>/answer.
//
// The model stand answers one call at a time and what it answers is set while it runs, since
// one check drives several cases against one stack: POST <records URL>/answers/model, whose
// body is {"verdict": "grant"|"deny", "reason": "…"} for a structured reply, {"text": "…"}
// for a reply that is no such object, {"obey_guidance": true} for a model that obeys every
// guidance in its prompt (proxyrulestest.ObeyedVerdict), and {"delay": "30s"} for an answer
// held back past the caller's own time bound, beside any of them. The report names that address
// (model_answer_url), and until a check calls it the model grants.
//
// The GitHub stand answers what each token request's body asked for - the repositories it names
// and the permissions it names - and serves GET /repos/{owner}/{repo} to a token it minted whose
// scope holds that repository, so a check spends a narrowed token as a caller does. The App's
// installation on each owner holds what --installation-permissions names for that owner, or the
// stand-in package's own map where it names nothing, and a token request naming a permission
// past what it holds is refused 422, as GitHub refuses it; only github-profile-check.sh passes
// the flag, so every other driver reads the stand's own map. Three routes answer by what the
// token holds, as GitHub answers them (proxyrulestest.GitHubStand): GET
// /repos/{owner}/{repo}/keys needs administration, GET /orgs/{org}/installations needs
// organization_administration, and POST /repos/{owner}/{repo}/statuses/{sha} needs statuses at
// write and answers 422 for any commit but the one the report names (github_commit_sha,
// proxyrulestest.GitHubCommitSHA). The github-tls stand is that same GitHub
// over TLS: a token minted at the github stand, where the broker's minter reaches it, is spent
// at github-tls, where a command reaches it through the proxy under a bound host's name. What it
// answers a token request is set while it runs by the same device as the model's: POST
// <records URL>/answers/github, whose body is {"mode": "<word>", "repository": "<name>"}, where
// the repository is the one the refuse-repository mode reads and every other mode leaves out.
// The six modes are ordinary, the answer each body asked for; refuse, the HTTP 422 that mints
// nothing, as GitHub answers a body naming a permission the installation does not hold;
// refuse-repository, that same 422 to a token request whose repositories name the one this body
// named and the ordinary answer to every other, which is how a check drives a caller that asks
// GitHub again with less of a narrowing to find the refused part; refuse-permissions, that 422
// to a token request whose body names any permissions and the ordinary answer to every other;
// widen, a token wider than the body asked for, which the broker must refuse to release; and
// fail-once, one HTTP 503 to the next token request, as a GitHub outage answers, before the
// ordinary answer again, which the broker's minter retries. A refuse-repository that names no
// repository is refused 400, as a mode outside the six is. The
// report names that address (github_answer_url), and until a check calls it the stand answers
// each token request what its body asked for, with metadata at read beside it, as GitHub grants
// that with every installation token.
//
// Every stand's requests are read back at the recording route this prints for it, GET
// <records URL>/records/<stand>, whose answer is the JSON array of what that stand was sent:
// each record's method, path, query, host, header and body. That is how a shell script asserts
// what a service received with no Go program of its own.
//
// It writes one JSON object to standard output naming every address, every recording route and
// every file it wrote - the authority's certificate and key, which
// BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE and a mounted BROKER_AIRLOCK_CA_CERT_FILE and
// BROKER_AIRLOCK_CA_KEY_FILE name, the ca stand's test root (proxy_root_file), and each HTTPS
// stand's own certificate - the AWS key pair the AWS stand verifies against, which the script
// signs its calls with, the GitHub App key the GitHub stand mints for, which the local stack's
// fake secret store holds as that App's stored value, and the two Dispatch issues the dispatch
// stand holds, one open and one done. It then logs "stands listening records=<records URL>" and
// serves until it is interrupted. Every key here is made at each start and is a stand-in's:
// nothing it prints opens anything.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

const exitUsageError = 2

// The stands this command serves. s, t and anthropic are the services a call behind the airlock
// reaches; github-tls is the GitHub stand as a call behind the airlock reaches it, over TLS
// under a bound host's name; the other twelve stand in for the endpoints the broker itself calls.
const (
	standS              = "s"
	standT              = "t"
	standGitHub         = "github"
	standGitHubTLS      = "github-tls"
	standGoogle         = "google"
	standMicrosoft      = "microsoft"
	standAWS            = "aws"
	standModel          = "model"
	standDispatch       = "dispatch"
	standMetadata       = "metadata"
	standCA             = "ca"
	standAnthropic      = "anthropic"
	standAnthropicToken = "anthropic-token"
	standIAM            = "iam"
)

// stands is every stand's name, in the order the JSON lists them.
var stands = []string{
	standS, standT, standGitHub, standGitHubTLS, standGoogle, standMicrosoft, standAWS,
	standModel, standDispatch, standMetadata, standCA, standAnthropic, standAnthropicToken, standIAM,
}

// plainStands are the nine served over http rather than https. The broker mints a type's value
// with an HTTP client that trusts the host's own roots and nothing else, so a minter endpoint
// behind TLS would need a root in the pod's trust store - the GitHub, Google, Anthropic and IAM
// stands are four such endpoints - and the three the model judge calls and the certificate
// authority the proxy asks for its intermediate are reached by that same
// sort of client; the metadata server could be nothing else, since internal/googleidentity asks
// it over http and no other scheme. Every other stand is reached through the proxy, whose extra
// roots BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE names. That is why the GitHub stand is two: github,
// plain, where the broker mints a GitHub profile's token, and github-tls, where a command behind
// the proxy spends it, since the proxy refuses plain http to a bound host and decrypts nothing but
// a bound host's port 443. The anthropic kind's are two for the same reason: anthropic-token,
// plain, where the broker mints, and anthropic, the Admin API a command reaches through the proxy.
var plainStands = map[string]bool{
	standGitHub: true, standGoogle: true,
	standModel: true, standDispatch: true, standMetadata: true, standCA: true,
	standAnthropicToken: true, standIAM: true,
}

// upgradePath is the one path S answers a WebSocket upgrade at, and the path a check drives
// an upgrade through the airlock on. It is a path of its own rather than every path of S, so
// that each of S's other routes still answers the service's own body and records the request.
const upgradePath = "/socket"

// The AWS key pair the AWS stand verifies a call against when --aws-key names none. The key id
// is in the shape AWS mints them, since the stand reads it out of each call's Credential.
const (
	defaultAWSKeyID     = "AKIA" + "STANDINKEY000000"
	defaultAWSSecretKey = standInAWSPhrase
	standInAWSPhrase    = "the-stand-in-aws-secret-access-key"
)

// standInGitHubAppID is the App id the stored github-app value carries. The GitHub stand reads
// no assertion, so nothing but the minter's own JWT header ever sees it.
const standInGitHubAppID = "123456"

// writeGitHubAppKey makes this run's stand-in GitHub App key and writes it as PEM, answering
// the path. The App JWT the github-app minter signs needs a real RSA key, and the key has to
// come from somewhere: it comes from here, beside the AWS pair, so the local stack's fake
// secret store is filled from this one report and no script generates a key of its own.
func writeGitHubAppKey(dir string) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate the stand-in GitHub App key: %w", err)
	}
	path := filepath.Join(dir, "github-app.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return "", fmt.Errorf("write the stand-in GitHub App key: %w", err)
	}
	return path, nil
}

// The two issues the dispatch stand holds: one being worked and one that is over, so a check
// drives both the issue a reader may act on and the issue it must not. Every other key is a
// 404, which is the third case and needs no fixture.
const (
	standInOpenIssue = "ACME-1353"
	standInDoneIssue = "ACME-1200"
)

// The one ask the dispatch stand holds: an answered ask on the open issue, which the judge reads
// as data for the model when a reason cites it. Its answering login is --ask-answered-by, since
// the login a local stack's approver signs in as is that stack's own.
const (
	standInAnsweredAsk = "4f3c2a10-5b6d-4e7f-8a9b-0c1d2e3f4a5b"
	standInAskQuestion = "Let the judge spend the judged token on this issue's own runs?"
	standInAskAnswer   = "Yes, for the runs this issue names, up to the seconds the key's judge tag gives the judge."
)

// defaultAskAnsweredBy is who answered that ask when no flag names a login. A local stack passes
// the login its own approver signs in as, and nothing here opens anything.
const defaultAskAnsweredBy = "approver@example.test"

// standInSpec is the open issue's spec document. It is written to the house outline, because a
// reader of a spec takes its Summary and Requirements sections out of it and shows those alone.
const standInSpec = `## Summary

The secrets broker asks one model whether a pending human-tier credential request should be
granted in the place of the person who decides it today.

## Requirements

- The judge decides a request for a key whose judge tag gives it seconds of it, with the key's
  guidance tag, and a person decides every other.
- The judge grants or denies, and a denial is final; a request it cannot decide goes to its
  person once.
- Every verdict is recorded against the request, with what the model was asked and answered.
`

// standInIssues is the fixture the dispatch stand answers from. The done issue carries no spec
// document, which is the other answer a spec read gives: an issue is judged on what Dispatch
// holds of it, and an issue with no spec is one of them.
func standInIssues() []proxyrulestest.DispatchIssue {
	return []proxyrulestest.DispatchIssue{
		{Key: standInOpenIssue, Title: "The secrets broker's model judge", Status: "in_progress", Spec: standInSpec},
		{Key: standInDoneIssue, Title: "The airlock's own local stack", Status: "done"},
	}
}

// standInAsks is the ask fixture: one ask on the open issue, answered by the login the flag
// names. Every other id is a 404, which is how a check drives an ask the reason cites and
// Dispatch does not hold.
func standInAsks(answeredBy string) []proxyrulestest.DispatchAsk {
	return []proxyrulestest.DispatchAsk{{
		ID: standInAnsweredAsk, IssueKey: standInOpenIssue, State: "answered",
		Question: standInAskQuestion, Options: []string{"Let it", "Leave it to me"},
		AnsweredBy: answeredBy, Selected: []string{"Let it"}, Text: standInAskAnswer,
	}}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// options is what the flags say: everything serve needs and nothing it reads from the
// environment.
type options struct {
	dir           string
	addresses     standFlags
	hosts         hostFlags
	installations map[string]int64
	// permissions is what --installation-permissions says each named owner's installation
	// holds, by owner; an owner it does not name holds the stand-in package's own map.
	permissions     map[string]map[string]string
	googleLifetime  time.Duration
	awsKeyID        string
	awsSecretKey    string
	awsSessionToken string
	recordsAddress  string
	askAnsweredBy   string
	// clockOffsetFile is the file the ca stand's certificates are dated by, --clock-offset-file.
	clockOffsetFile string
}

// run reads the subcommand, if there is one, and otherwise serves the stands. The subcommands
// are words rather than flags of the stands' own flag set: each has flags of its own, and a
// flag set that held every mode's flags would accept any mixture of them.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "front":
			return runFront(ctx, args[1:], stdout, stderr)
		case "enroll-pod":
			return runEnrollPod(ctx, args[1:], stdout, stderr)
		case "probe-private":
			return runProbePrivate(args[1:], stdout, stderr)
		case "probe-bound":
			return runProbeBound(args[1:], stdout, stderr)
		case "load":
			return runLoad(ctx, args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("dev-airlock-stands", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := options{addresses: standFlags{flagName: "--address"}, hosts: hostFlags{}, installations: map[string]int64{}, permissions: map[string]map[string]string{}}
	fs.StringVar(&o.dir, "dir", "", "write the authority's and the services' files into `<path>`, and with none a directory of this run's own")
	fs.Var(&o.addresses, "address", "bind `<stand>=<host:port>` at that address rather than 127.0.0.1 on a port the system picks (repeatable)")
	fs.Var(&o.hosts, "host", "add `<stand>=<name>`'s name or address to that stand's certificate (repeatable)")
	fs.Var(installationFlags{installations: o.installations}, "installation", "the GitHub stand answers `<owner>=<installation id>` as an installation of the App (repeatable)")
	fs.Var(permissionFlags{held: o.permissions}, "installation-permissions", "the App's installation on the owner of `<owner>=<permission>:<level>,...` holds those permissions, and nothing for an owner followed by nothing, in place of the stand's own map (repeatable)")
	fs.DurationVar(&o.googleLifetime, "google-lifetime", time.Hour, "the Google stand's answer says the access token lasts `<duration>`")
	fs.StringVar(&o.recordsAddress, "records-address", "127.0.0.1:0", "serve the recording routes at `<host:port>`")
	awsKey := fs.String("aws-key", "", "the AWS stand verifies each call against `<key id>=<secret key>`, and with none a pair of its own")
	fs.StringVar(&o.awsSessionToken, "aws-session-token", "", "the AWS stand requires `<token>` as the X-Amz-Security-Token of each call; with none it refuses a call that carries any, which is the pair that has no session token")
	fs.StringVar(&o.askAnsweredBy, "ask-answered-by", defaultAskAnsweredBy, "the dispatch stand's one answered ask reads `<login>` as the person who answered it")
	fs.StringVar(&o.clockOffsetFile, "clock-offset-file", "", "date the ca stand's certificates by the system's clock moved by the Go duration `<path>` holds, read again for each one: the file the brokers' BROKER_PROXY_TEST_CLOCK_OFFSET_FILE names")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	o.awsKeyID, o.awsSecretKey = defaultAWSKeyID, defaultAWSSecretKey
	if *awsKey != "" {
		keyID, secretKey, ok := strings.Cut(*awsKey, "=")
		if !ok || keyID == "" || secretKey == "" {
			fmt.Fprintln(stderr, "dev-airlock-stands: --aws-key wants <key id>=<secret key>")
			return exitUsageError
		}
		o.awsKeyID, o.awsSecretKey = keyID, secretKey
	}
	if o.googleLifetime <= 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands: --google-lifetime %s is no lifetime\n", o.googleLifetime)
		return exitUsageError
	}
	if len(o.installations) == 0 {
		// One owner when no --installation names any, so a github-app secret still mints: a
		// check that cares which owner says so itself.
		o.installations["acme-org"] = 7
	}
	for _, owner := range slices.Sorted(maps.Keys(o.permissions)) {
		if _, installed := o.installations[owner]; !installed {
			// A map for an owner the App is not installed on would answer nothing: a check
			// that named the wrong owner reads that here, not as a cut that never happened.
			fmt.Fprintf(stderr, "dev-airlock-stands: --installation-permissions names %s, on which no --installation installs the App\n", owner)
			return exitUsageError
		}
	}
	return serve(ctx, o, stdout, stderr)
}

// standFlags is a repeated flag naming one stand: --address.
type standFlags struct {
	flagName string
	values   map[string]string
}

func (f *standFlags) String() string { return "" }

// Set parses <stand>=<value>, refusing a stand this command does not serve and a stand named
// twice, whose second value would replace the first.
func (f *standFlags) Set(value string) error {
	stand, rest, ok := strings.Cut(value, "=")
	if !ok || rest == "" || !slices.Contains(stands, stand) {
		return fmt.Errorf("want <stand>=<value>, the stand one of %s", strings.Join(stands, ", "))
	}
	if _, twice := f.values[stand]; twice {
		return fmt.Errorf("%s names %s twice", f.flagName, stand)
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[stand] = rest
	return nil
}

// hostFlags is --host: the names each stand's certificate carries beyond its own, which one
// stand may be given several of.
type hostFlags struct {
	values map[string][]string
}

func (f *hostFlags) String() string { return "" }

func (f *hostFlags) Set(value string) error {
	stand, host, ok := strings.Cut(value, "=")
	if !ok || host == "" || !slices.Contains(stands, stand) {
		return fmt.Errorf("want <stand>=<name>, the stand one of %s", strings.Join(stands, ", "))
	}
	if f.values == nil {
		f.values = map[string][]string{}
	}
	f.values[stand] = append(f.values[stand], host)
	return nil
}

// installationFlags is --installation: the installation id the App has on each owner.
type installationFlags struct {
	installations map[string]int64
}

func (f installationFlags) String() string { return "" }

func (f installationFlags) Set(value string) error {
	owner, id, ok := strings.Cut(value, "=")
	if !ok || owner == "" {
		return errors.New("want <owner>=<installation id>")
	}
	installation, err := strconv.ParseInt(id, 10, 64)
	if err != nil || installation <= 0 {
		return fmt.Errorf("the installation id %q is no positive number", id)
	}
	if _, twice := f.installations[owner]; twice {
		return fmt.Errorf("--installation names %s twice", owner)
	}
	f.installations[owner] = installation
	return nil
}

// standReport is one stand as the JSON names it.
type standReport struct {
	// Address is the host:port it listens on, and URL that address as a caller reaches it.
	Address string `json:"address"`
	URL     string `json:"url"`
	// Records is the route the stand's requests are read back at.
	Records string `json:"records_url"`
	// CertFile is the stand's own certificate, written beside the authority's, and empty for
	// a stand served over plain http.
	CertFile string `json:"cert_file,omitempty"`
}

// report is the whole JSON object this command writes to standard output.
type report struct {
	// Dir is where every file below was written.
	Dir string `json:"dir"`
	// CACertFile and CAKeyFile are the authority behind every stand's certificate: the
	// certificate BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE names, and a self-signed pair the mounted
	// BROKER_AIRLOCK_CA_CERT_FILE and BROKER_AIRLOCK_CA_KEY_FILE can name.
	CACertFile string `json:"ca_cert_file"`
	CAKeyFile  string `json:"ca_key_file"`
	// ProxyRootFile is the test root the ca stand signs every intermediate under, the one root a
	// client trusts the proxy by when the brokers take their intermediates from that stand.
	ProxyRootFile string `json:"proxy_root_file"`
	// RecordsURL is where every stand's recording route is served.
	RecordsURL string `json:"records_url"`
	// ModelAnswerURL is where a check sets what the model stand answers next, POST
	// <records URL>/answers/model, whose body the command's own comment spells out.
	ModelAnswerURL string `json:"model_answer_url"`
	// GitHubAnswerURL is where a check sets what the GitHub stand answers each token request
	// from then on, POST <records URL>/answers/github, whose body the command's own comment
	// spells out.
	GitHubAnswerURL string `json:"github_answer_url"`
	// AnthropicAnswerURL is where a check sets the expires_in the anthropic-token stand answers
	// each exchange from then on, POST <records URL>/answers/anthropic. AnthropicTokensURL and
	// IAMTokensURL are where it reads every token the exchange answered and every identity token
	// the iam stand minted, with its account and audience.
	AnthropicAnswerURL string `json:"anthropic_answer_url"`
	AnthropicTokensURL string `json:"anthropic_tokens_url"`
	IAMTokensURL       string `json:"iam_tokens_url"`
	// DispatchOpenIssue and DispatchDoneIssue are the two issues the dispatch stand holds,
	// which is what a check names in a request's reason: the open one for a reason a judge
	// may act on, the done one for a reason it must not, and any other key for no issue at
	// all.
	DispatchOpenIssue string `json:"dispatch_open_issue"`
	DispatchDoneIssue string `json:"dispatch_done_issue"`
	// DispatchAnsweredAsk is the one ask the dispatch stand holds: an answered ask on
	// DispatchOpenIssue, which a reason may cite as dispatch://<open issue>/ask/<this id>, and
	// which the judge then reads as data for the model. DispatchAskAnsweredBy is the login it
	// reads as having answered it, which --ask-answered-by names. Any other ask id is 404.
	DispatchAnsweredAsk   string `json:"dispatch_answered_ask"`
	DispatchAskAnsweredBy string `json:"dispatch_ask_answered_by"`
	// AWSKeyID and AWSSecretKey are the pair the AWS stand verifies a call against, which a
	// script signs its calls with and the broker's fake secret store holds as the real key.
	AWSKeyID     string `json:"aws_key_id"`
	AWSSecretKey string `json:"aws_secret_key"`
	// GitHubAppID and GitHubAppKeyFile are the App the GitHub stand mints installation tokens
	// for: the id and the PEM private key this run made, which the broker's fake secret store
	// holds as a github-app secret's stored value. The stand verifies no assertion, so the key
	// is only ever what the minter signs its App JWT with.
	GitHubAppID      string `json:"github_app_id"`
	GitHubAppKeyFile string `json:"github_app_key_file"`
	// GitHubCommitSHA is the one commit every repository of the GitHub stand holds.
	GitHubCommitSHA string `json:"github_commit_sha"`
	// Stands is each stand by name.
	Stands map[string]standReport `json:"stands"`
}

// serve starts every stand, writes the report and serves until ctx ends. Anything it cannot
// start ends it, 1.
func serve(ctx context.Context, o options, stdout, stderr io.Writer) int {
	dir := o.dir
	if dir == "" {
		made, err := os.MkdirTemp("", "dev-airlock-stands-")
		if err != nil {
			fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
			return 1
		}
		dir = made
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	authority, err := proxyrulestest.NewAuthority("dev-airlock-stands authority")
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	certFile, keyFile, err := authority.WriteFiles(dir)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}

	services := map[string]*proxyrulestest.Service{}
	defer func() {
		for _, service := range services {
			service.Close()
		}
	}()
	appKeyFile, err := writeGitHubAppKey(dir)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	out := report{
		Dir: dir, CACertFile: certFile, CAKeyFile: keyFile,
		AWSKeyID: o.awsKeyID, AWSSecretKey: o.awsSecretKey,
		GitHubAppID: standInGitHubAppID, GitHubAppKeyFile: appKeyFile, GitHubCommitSHA: proxyrulestest.GitHubCommitSHA,
		DispatchOpenIssue: standInOpenIssue, DispatchDoneIssue: standInDoneIssue,
		DispatchAnsweredAsk: standInAnsweredAsk, DispatchAskAnsweredBy: o.askAnsweredBy,
		Stands: map[string]standReport{},
	}
	for _, name := range stands {
		hosts := append([]string{"127.0.0.1", "localhost"}, o.hosts.values[name]...)
		service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{
			Addr:      o.addresses.values[name],
			Hosts:     hosts,
			Authority: authority,
			Plain:     plainStands[name],
			Body:      "the " + name + " stand-in answered",
		})
		if err != nil {
			fmt.Fprintf(stderr, "dev-airlock-stands: the %s stand: %v\n", name, err)
			return 1
		}
		services[name] = service
		entry := standReport{Address: service.Addr(), URL: service.URL()}
		if pem := service.CertificatePEM(); pem != nil {
			entry.CertFile = filepath.Join(dir, name+".crt")
			if err := os.WriteFile(entry.CertFile, pem, 0o644); err != nil {
				fmt.Fprintf(stderr, "dev-airlock-stands: write the %s stand's certificate: %v\n", name, err)
				return 1
			}
		}
		out.Stands[name] = entry
	}
	// S answers an upgrade on one path of its own and its own body everywhere else, and T and
	// anthropic their own body and nothing more; the other ten answer as the service they stand in
	// for. Every one of them records what it was sent, which is what they are all for.
	services[standS].AnswerWith(proxyrulestest.NewUpgradeStand(proxyrulestest.UpgradeOptions{Path: upgradePath}).Answer)
	// The GitHub stand answers each token request what its body asked for, with metadata at read
	// beside it, until a check sets another answer, so a stack driven with no call to its answer
	// route mints as GitHub does. It mints a token of its own for every request, as GitHub does
	// (DistinctTokens): the broker revokes a token it minted and hands to nobody, and a stand that
	// answered one string for a scope would have that revoke end the token the airlock's proxy
	// holds for the same scope. Each installation holds what --installation-permissions names for
	// its owner, or the stand-in package's own map. One stand serves both GitHub stands, so a token
	// minted at the plain one is spent at the TLS one.
	github := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{
		Installations: o.installations, Permissions: o.permissions, DistinctTokens: true,
	})
	services[standGitHub].AnswerWith(github.Answer)
	services[standGitHubTLS].AnswerWith(github.Answer)
	services[standGoogle].AnswerWith(proxyrulestest.NewGoogleStand(proxyrulestest.GoogleOptions{Lifetime: o.googleLifetime}).Answer)
	// The Microsoft stand plays both hosts of its one certificate: the authority a federated
	// token is exchanged at, and the Resource Manager that access token is then spent at.
	services[standMicrosoft].AnswerWith(proxyrulestest.Answers(
		proxyrulestest.NewMicrosoftStand(proxyrulestest.MicrosoftOptions{}).Answer,
		proxyrulestest.NewAzureResourceStand(proxyrulestest.AzureResourceOptions{}).Answer,
	))
	services[standAWS].AnswerWith(proxyrulestest.NewAWSStand(proxyrulestest.AWSOptions{
		KeyID: o.awsKeyID, SecretKey: o.awsSecretKey, SessionToken: o.awsSessionToken,
	}).Answer)
	// The model stand grants until a check sets another answer, so a stack driven with no
	// call to the answer route judges a request through to a grant.
	firstAnswer, err := modelAnswerOf(modelAnswerRequest{Verdict: "grant"})
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: the model stand's first answer: %v\n", err)
		return 1
	}
	model := proxyrulestest.NewModelStand(proxyrulestest.ModelOptions{Answer: firstAnswer})
	services[standModel].AnswerWith(model.Answer)
	services[standDispatch].AnswerWith(proxyrulestest.NewDispatchStand(proxyrulestest.DispatchOptions{
		Issues: standInIssues(), Asks: standInAsks(o.askAnsweredBy),
	}).Answer)
	// The metadata stand answers the machine's access token too, this run's own, which the iam
	// stand takes as the one bearer of a generateIdToken call: the broker reads it there and spends
	// it at IAM to mint an anthropic-federation secret's identity token, as a pod spends its own.
	access := randomText(32)
	services[standMetadata].AnswerWith(proxyrulestest.Answers(
		metadataAccessAnswer(access),
		proxyrulestest.NewMetadataStand(proxyrulestest.MetadataOptions{}).Answer,
	))
	// The exchange answers an hour's token until a check sets another lifetime, so a stack driven
	// with no call to its answer route mints as a federation rule of an hour does.
	iam := &iamStand{access: access}
	exchange := &anthropicExchangeStand{iam: iam, expiresIn: defaultAnthropicLifetime}
	services[standIAM].AnswerWith(iam.Answer)
	services[standAnthropicToken].AnswerWith(exchange.Answer)
	// The ca stand: the certificate request a broker pod sends for its intermediate, answered
	// under a test root of its own and the policy's names, dated by the brokers' moved clock, and
	// told to fail and answer again at POST /fail and POST /answer and given other names at POST
	// /policy (proxyrulestest.CAStand).
	now := time.Now
	if o.clockOffsetFile != "" {
		clock, err := proxy.NewOffsetFileClock(o.clockOffsetFile)
		if err != nil {
			fmt.Fprintf(stderr, "dev-airlock-stands: --clock-offset-file: %v\n", err)
			return 1
		}
		now = clock.Now
	}
	ca, err := proxyrulestest.NewCAStand(proxyrulestest.CAOptions{Now: now})
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: the ca stand: %v\n", err)
		return 1
	}
	services[standCA].AnswerWith(ca.Answer)
	out.ProxyRootFile = filepath.Join(dir, "proxy-root.crt")
	if err := os.WriteFile(out.ProxyRootFile, ca.Root().CertificatePEM(), 0o644); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: write the proxy test root: %v\n", err)
		return 1
	}

	records := &http.Server{Handler: recordsHandler(services, model, github, iam, exchange), ReadHeaderTimeout: 30 * time.Second}
	recordsURL, stopRecords, err := serveRecords(records, o.recordsAddress)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	defer stopRecords()
	out.RecordsURL = recordsURL
	out.ModelAnswerURL, out.GitHubAnswerURL = recordsURL+modelAnswerPath, recordsURL+githubAnswerPath
	out.AnthropicAnswerURL = recordsURL + anthropicAnswerPath
	out.AnthropicTokensURL, out.IAMTokensURL = recordsURL+anthropicTokensPath, recordsURL+iamTokensPath
	for name, entry := range out.Stands {
		entry.Records = recordsURL + "/records/" + name
		out.Stands[name] = entry
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: write the report: %v\n", err)
		return 1
	}
	if flusher, ok := stdout.(interface{ Sync() error }); ok {
		_ = flusher.Sync()
	}
	fmt.Fprintf(stderr, "dev-airlock-stands: stands listening records=%s\n", recordsURL)
	<-ctx.Done()
	return 0
}

// serveRecords binds the recording routes and answers their URL and how to stop them.
func serveRecords(server *http.Server, addr string) (string, func(), error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("bind the recording routes: %w", err)
	}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), func() { _ = server.Close() }, nil
}

// recordsHandler is the things the recording server answers: GET /records/<stand>, the JSON
// array of every request that stand was sent, each record's method, path, query, host, header and
// body, which is what a check script reads a result back from; the model, GitHub and Anthropic
// stands' answer routes; and the two lists of what the iam and anthropic-token stands minted. They
// are served here because a check that drives one case after another needs one address to set
// and read them at and the recording server is the address it already has.
func recordsHandler(services map[string]*proxyrulestest.Service, model *proxyrulestest.ModelStand, github *proxyrulestest.GitHubStand, iam *iamStand, exchange *anthropicExchangeStand) http.Handler {
	mux := http.NewServeMux()
	for _, name := range slices.Sorted(maps.Keys(services)) {
		service := services[name]
		mux.HandleFunc("GET /records/"+name, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			encoder := json.NewEncoder(w)
			encoder.SetIndent("", "  ")
			_ = encoder.Encode(service.Requests())
		})
	}
	mux.HandleFunc("POST "+modelAnswerPath, answerRoute(modelAnswerOf, model.Answers,
		func(answer proxyrulestest.ModelAnswer) map[string]string {
			return map[string]string{"text": answer.Text, "delay": answer.Delay.String(), "obey_guidance": strconv.FormatBool(answer.ObeyGuidance)}
		}))
	mux.HandleFunc("POST "+githubAnswerPath, answerRoute(githubAnswerOf, github.Answers,
		func(answer proxyrulestest.GitHubAnswer) map[string]string {
			return map[string]string{"mode": string(answer.Mode), "repository": answer.Repository}
		}))
	mux.HandleFunc("POST "+anthropicAnswerPath, answerRoute(anthropicAnswerOf, exchange.Answers,
		func(expiresIn int64) map[string]string {
			return map[string]string{"expires_in": strconv.FormatInt(expiresIn, 10)}
		}))
	mux.HandleFunc("GET "+iamTokensPath, tokensRoute(iam.Minted))
	mux.HandleFunc("GET "+anthropicTokensPath, tokensRoute(exchange.Minted))
	return mux
}

// answerRoute is one of those three answer routes: it reads the request body as a Body, hands it
// to read, which answers the answer it names or what is wrong with it, sets that answer on its
// stand and answers the caller what reply makes of it. A body that is no JSON object, and one that
// names no answer, are each 400 and set nothing, so a check that misspelled an answer reads that
// rather than drive the next case against the answer still in place.
func answerRoute[Body, Answer any](read func(Body) (Answer, error), set func(Answer), reply func(Answer) map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body Body
		if err := json.NewDecoder(io.LimitReader(r.Body, maxAnswerBody)).Decode(&body); err != nil {
			http.Error(w, "the answer is no JSON object: "+err.Error(), http.StatusBadRequest)
			return
		}
		answer, err := read(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		set(answer)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply(answer))
	}
}

// modelAnswerPath, githubAnswerPath and anthropicAnswerPath are where a check sets what the
// model stand, the GitHub stand and the anthropic-token stand answer from there on, and
// maxAnswerBody what one such request is read of: a reply is a sentence or two and an answer a
// word and a repository name or a number, so a body past this is a mistake rather than an answer.
// iamTokensPath and anthropicTokensPath are where it reads what the iam and anthropic-token
// stands minted.
const (
	modelAnswerPath     = "/answers/model"
	githubAnswerPath    = "/answers/github"
	anthropicAnswerPath = "/answers/anthropic"
	iamTokensPath       = "/tokens/iam"
	anthropicTokensPath = "/tokens/anthropic"
	maxAnswerBody       = 1 << 20
)

// githubAnswerRequest is the body POST <records URL>/answers/github takes.
type githubAnswerRequest struct {
	// Mode is which of the GitHub stand's six answers the token requests from then on get:
	// ordinary, refuse, refuse-repository, refuse-permissions or widen for every one, or
	// fail-once for the next one alone, after which the stand answers as ordinary
	// (proxyrulestest.GitHubTokenAnswer).
	Mode string `json:"mode"`
	// Repository is the repository the refuse-repository mode refuses a token request for. It is
	// the one mode that reads it, and every other leaves it out.
	Repository string `json:"repository"`
}

// githubAnswerOf is one such request as the answer it sets, and what is wrong with a request
// that sets none: a mode outside the six is named back with the six, and a refuse-repository
// that names no repository is named back with what it is missing, since a check that misspelled
// a mode or left that name out must read it rather than drive its next case against the answer
// still in place.
func githubAnswerOf(body githubAnswerRequest) (proxyrulestest.GitHubAnswer, error) {
	answer := proxyrulestest.GitHubAnswer{
		Mode:       proxyrulestest.GitHubTokenAnswer(body.Mode),
		Repository: body.Repository,
	}
	switch answer.Mode {
	case proxyrulestest.GitHubTokensOrdinary, proxyrulestest.GitHubTokensRefused,
		proxyrulestest.GitHubTokensRefusedPermissions, proxyrulestest.GitHubTokensWider,
		proxyrulestest.GitHubTokensFailOnce:
		return answer, nil
	case proxyrulestest.GitHubTokensRefusedRepository:
		if answer.Repository == "" {
			return proxyrulestest.GitHubAnswer{}, fmt.Errorf(
				"the mode %s names no repository to refuse: name it in the answer's repository field",
				proxyrulestest.GitHubTokensRefusedRepository)
		}
		return answer, nil
	default:
		return proxyrulestest.GitHubAnswer{}, fmt.Errorf(
			"the mode %q is none of the GitHub stand's six answers: %s, %s, %s, %s, %s and %s",
			body.Mode, proxyrulestest.GitHubTokensOrdinary, proxyrulestest.GitHubTokensRefused,
			proxyrulestest.GitHubTokensRefusedRepository, proxyrulestest.GitHubTokensRefusedPermissions,
			proxyrulestest.GitHubTokensWider, proxyrulestest.GitHubTokensFailOnce)
	}
}

// modelAnswerRequest is the body POST <records URL>/answers/model takes.
type modelAnswerRequest struct {
	// Verdict and Reason are the structured reply: the answer's text is the JSON object
	// {"verdict": …, "reason": …}, which is the shape a caller asking for a structured reply
	// reads back out of a Messages API reply. A verdict with no reason is given one.
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	// Text is the whole of the answer's text, for an answer that is no such object at all:
	// the way a check drives a reply its caller cannot read.
	Text string `json:"text"`
	// Delay holds the answer back for that Go duration, which is how a check spends a
	// caller's own time bound: "30s" against a caller bounded at 20 seconds is an answer it
	// never sees, so the text beside it is never read.
	Delay string `json:"delay"`
	// ObeyGuidance answers each call as a model that obeys every guidance in its prompt would
	// (proxyrulestest.ObeyedVerdict), in place of a fixed verdict or text: the stand-in a check
	// of the judge's per-key isolation runs against.
	ObeyGuidance bool `json:"obey_guidance"`
}

// modelAnswerOf is one such request as the answer it sets, and what is wrong with a request
// that sets none.
func modelAnswerOf(body modelAnswerRequest) (proxyrulestest.ModelAnswer, error) {
	var answer proxyrulestest.ModelAnswer
	switch {
	case body.ObeyGuidance && (body.Verdict != "" || body.Text != ""):
		return answer, errors.New("the answer both obeys guidance and names a verdict or a text, and it is one or the other")
	case body.Verdict != "" && body.Text != "":
		return answer, errors.New("the answer names both a verdict and a text, and it is one or the other")
	case body.Verdict == "" && body.Text == "" && body.Delay == "" && !body.ObeyGuidance:
		return answer, errors.New("the answer names no verdict, no text, no delay and no obey_guidance")
	case body.ObeyGuidance:
		answer.ObeyGuidance = true
	case body.Verdict != "":
		reason := body.Reason
		if reason == "" {
			reason = "the stand-in model answered " + body.Verdict
		}
		encoded, err := json.Marshal(map[string]string{"verdict": body.Verdict, "reason": reason})
		if err != nil {
			return proxyrulestest.ModelAnswer{}, fmt.Errorf("the verdict could not be encoded: %w", err)
		}
		answer.Text = string(encoded)
	default:
		answer.Text = body.Text
	}
	if body.Delay != "" {
		delay, err := time.ParseDuration(body.Delay)
		if err != nil || delay < 0 {
			return proxyrulestest.ModelAnswer{}, fmt.Errorf("the delay %q is no duration to hold an answer back by", body.Delay)
		}
		answer.Delay = delay
	}
	return answer, nil
}
