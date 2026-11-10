// Command dev-airlock-stands serves the airlock's stand-ins to the broker's local stack
// (scripts/dev-broker.sh and the airlock checks run beside it), so a check script drives the
// same stand-ins the Go tests do: the two HTTPS services S and T, which record every request
// they get and of which S answers a WebSocket upgrade at /socket; the stand-in GitHub and
// Google endpoints a github-app and a google-login secret are minted at; the stand-in
// Microsoft authority the azure kind reads a tenant's OpenID configuration from and exchanges
// at; a stand-in AWS service that verifies Signature Version 4 with the real test key and
// the real session token; and the three the broker's model judge calls - a model endpoint, a
// Dispatch holding a few issues, and the Compute Engine metadata server the broker mints its
// own identity token at. Every one of them is internal/broker/airlock/airlocktest's, which is
// why one implementation answers both.
//
//	dev-airlock-stands [--dir <path>] [--address <stand>=<host:port> ...] [--host <stand>=<name> ...]
//	                   [--github-owner <owner>=<installation id> ...] [--google-lifetime <duration>]
//	                   [--aws-key <key id>=<secret key>] [--aws-session-token <token>]
//	                   [--records-address <host:port>]
//	dev-airlock-stands front --listen <host:port> [--backend <host:port> ...] [--backends-file <path>]
//	dev-airlock-stands enroll-pod --url <broker> --token-file <path> --pod-token-file <path>
//	                   --pod-uid <uid> --thumbprint <thumbprint> [--service <name>] [--session <id>]
//	dev-airlock-stands probe-private
//	dev-airlock-stands probe-bound
//	dev-airlock-stands load <load|tunnel|upload|gap|upgrade> [--proxy <url>] [--host <name>] ...
//
// The stack and its checks need five more small things of their own, and they are modes of this
// one command rather than commands of their own so that it builds one binary: `front`, one TCP
// address in front of several (front.go); `enroll-pod`, the two calls that enrol a pod session
// (enroll.go); and the three Go clients the checks drive, `probe-private` and `probe-bound`
// (probe.go) and `load` (load.go), which are here so that CI compiles and vets them. With no
// subcommand it serves the stands, which is what the rest of this comment is about.
//
// A stand is named s, t, github, google, microsoft, aws, model, dispatch or metadata. Each
// binds 127.0.0.1 on a port the operating system picks unless --address names one, and each
// HTTPS stand's certificate names 127.0.0.1 and localhost unless --host adds names of its own
// - the name a rules file binds a secret to, which the command's own TLS verification reads
// out of the certificate.
//
// The model stand answers one call at a time and what it answers is set while it runs, since
// one check drives several cases against one stack: POST <records URL>/answers/model, whose
// body is {"verdict": "approve"|"refer", "reason": "…"} for a structured reply, {"text": "…"}
// for a reply that is no such object, and {"delay": "30s"} for an answer held back past the
// caller's own time bound. The report names that address (model_answer_url), and until a check
// calls it the model approves.
//
// Every stand's requests are read back at the recording route this prints for it, GET
// <records URL>/records/<stand>, whose answer is the JSON array of what that stand was sent:
// each record's method, path, query, host, header and body. That is how a shell script asserts
// what a service received with no Go program of its own.
//
// It writes one JSON object to standard output naming every address, every recording route and
// every file it wrote - the authority's certificate and key, which
// BROKER_AIRLOCK_CA_CERT_FILE, BROKER_AIRLOCK_CA_KEY_FILE and
// BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE name, and each HTTPS stand's own certificate - the AWS
// key pair the AWS stand verifies against, which the script signs its calls with, the GitHub
// App key the GitHub stand mints for, which the local stack's fake secret store holds as that
// App's stored value, and the two Dispatch issues the dispatch stand holds, one open and one
// done. It then logs "stands listening records=<records URL>" and serves until it is
// interrupted. Every key here is made at each start and is a stand-in's: nothing it prints
// opens anything.
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

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
)

const exitUsageError = 2

// The stands this command serves. s and t are the two services a call behind the airlock
// reaches; the other seven stand in for the endpoints the broker itself calls.
const (
	standS         = "s"
	standT         = "t"
	standGitHub    = "github"
	standGoogle    = "google"
	standMicrosoft = "microsoft"
	standAWS       = "aws"
	standModel     = "model"
	standDispatch  = "dispatch"
	standMetadata  = "metadata"
)

// stands is every stand's name, in the order the JSON lists them.
var stands = []string{
	standS, standT, standGitHub, standGoogle, standMicrosoft, standAWS,
	standModel, standDispatch, standMetadata,
}

// plainStands are the five served over http rather than https. The broker mints a kind's value
// with an HTTP client that trusts the host's own roots and nothing else, so a minter endpoint
// behind TLS would need a root in the pod's trust store, and the three the model judge calls
// are reached by that same sort of client; the metadata server could be nothing else, since
// internal/googleidentity asks it over http and no other scheme. Every other stand is reached
// through the proxy, whose extra roots BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE names.
var plainStands = map[string]bool{
	standGitHub: true, standGoogle: true,
	standModel: true, standDispatch: true, standMetadata: true,
}

// upgradePath is the one path S answers a WebSocket upgrade at, and the path a check drives
// an upgrade through the airlock on. It is a path of its own rather than every path of S, so
// that each of S's other routes still answers the service's own body and records the request.
const upgradePath = "/socket"

// The AWS key pair the AWS stand verifies a call against when --aws-key names none. The key id
// is in the shape AWS mints them, since the stand reads it out of each call's Credential.
const (
	defaultAWSKeyID     = "AKIASTANDINKEY000000"
	defaultAWSSecretKey = "the-stand-in-aws-secret-access-key"
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
	standInOpenIssue = "INFRA-1353"
	standInDoneIssue = "INFRA-1200"
)

// standInSpec is the open issue's spec document. It is written to the house outline, because a
// reader of a spec takes its Summary and Requirements sections out of it and shows those alone.
const standInSpec = `## Summary

The secrets broker asks one model whether a pending human-tier credential request should be
approved in the place of the person who decides it today.

## Requirements

- The judge decides a request the airlock rules give it seconds for, and refers every other.
- The judge never denies: a request it does not approve waits for its person exactly as it does
  today.
- Every verdict is recorded against the request, with what the model was asked and answered.
`

// standInIssues is the fixture the dispatch stand answers from. The done issue carries no spec
// document, which is the other answer a spec read gives: an issue is judged on what Dispatch
// holds of it, and an issue with no spec is one of them.
func standInIssues() []airlocktest.DispatchIssue {
	return []airlocktest.DispatchIssue{
		{Key: standInOpenIssue, Title: "The secrets broker's model judge", Status: "in_progress", Spec: standInSpec},
		{Key: standInDoneIssue, Title: "The airlock's own local stack", Status: "done"},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// options is what the flags say: everything serve needs and nothing it reads from the
// environment.
type options struct {
	dir             string
	addresses       standFlags
	hosts           hostFlags
	installations   map[string]int64
	googleLifetime  time.Duration
	awsKeyID        string
	awsSecretKey    string
	awsSessionToken string
	recordsAddress  string
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
	o := options{addresses: standFlags{flagName: "--address"}, hosts: hostFlags{}, installations: map[string]int64{}}
	fs.StringVar(&o.dir, "dir", "", "write the authority's and the services' files into `<path>`, and with none a directory of this run's own")
	fs.Var(&o.addresses, "address", "bind `<stand>=<host:port>` at that address rather than 127.0.0.1 on a port the system picks (repeatable)")
	fs.Var(&o.hosts, "host", "add `<stand>=<name>`'s name or address to that stand's certificate (repeatable)")
	fs.Var(ownerFlags{installations: o.installations}, "github-owner", "the GitHub stand answers `<owner>=<installation id>` as an installation of the App (repeatable)")
	fs.DurationVar(&o.googleLifetime, "google-lifetime", time.Hour, "the Google stand's answer says the access token lasts `<duration>`")
	fs.StringVar(&o.recordsAddress, "records-address", "127.0.0.1:0", "serve the recording routes at `<host:port>`")
	awsKey := fs.String("aws-key", "", "the AWS stand verifies each call against `<key id>=<secret key>`, and with none a pair of its own")
	fs.StringVar(&o.awsSessionToken, "aws-session-token", "", "the AWS stand requires `<token>` as the X-Amz-Security-Token of each call; with none it refuses a call that carries any, which is the pair that has no session token")
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
		// One owner, so a github-app secret released with no --github-owner still mints: a
		// check that cares which owner says so itself.
		o.installations["acme-org"] = 7
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

// ownerFlags is --github-owner: the installation id the App has on each owner.
type ownerFlags struct {
	installations map[string]int64
}

func (f ownerFlags) String() string { return "" }

func (f ownerFlags) Set(value string) error {
	owner, id, ok := strings.Cut(value, "=")
	if !ok || owner == "" {
		return errors.New("want <owner>=<installation id>")
	}
	installation, err := strconv.ParseInt(id, 10, 64)
	if err != nil || installation <= 0 {
		return fmt.Errorf("the installation id %q is no positive number", id)
	}
	if _, twice := f.installations[owner]; twice {
		return fmt.Errorf("--github-owner names %s twice", owner)
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
	// CACertFile and CAKeyFile are the authority behind every stand's certificate: the pair
	// BROKER_AIRLOCK_CA_CERT_FILE and BROKER_AIRLOCK_CA_KEY_FILE name, and the certificate
	// BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE names.
	CACertFile string `json:"ca_cert_file"`
	CAKeyFile  string `json:"ca_key_file"`
	// RecordsURL is where every stand's recording route is served.
	RecordsURL string `json:"records_url"`
	// ModelAnswerURL is where a check sets what the model stand answers next, POST
	// <records URL>/answers/model, whose body the command's own comment spells out.
	ModelAnswerURL string `json:"model_answer_url"`
	// DispatchOpenIssue and DispatchDoneIssue are the two issues the dispatch stand holds,
	// which is what a check names in a request's reason: the open one for a reason a judge
	// may act on, the done one for a reason it must not, and any other key for no issue at
	// all.
	DispatchOpenIssue string `json:"dispatch_open_issue"`
	DispatchDoneIssue string `json:"dispatch_done_issue"`
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
	authority, err := airlocktest.NewAuthority("dev-airlock-stands authority")
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	certFile, keyFile, err := authority.WriteFiles(dir)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}

	services := map[string]*airlocktest.Service{}
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
		GitHubAppID: standInGitHubAppID, GitHubAppKeyFile: appKeyFile,
		DispatchOpenIssue: standInOpenIssue, DispatchDoneIssue: standInDoneIssue,
		Stands: map[string]standReport{},
	}
	for _, name := range stands {
		hosts := append([]string{"127.0.0.1", "localhost"}, o.hosts.values[name]...)
		service, err := airlocktest.NewService(airlocktest.ServiceOptions{
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
	// S answers an upgrade on one path of its own and its own body everywhere else, and T its
	// own body and nothing more; the other seven answer as the service they stand in for. Every
	// one of them records what it was sent, which is what they are all for.
	services[standS].AnswerWith(airlocktest.NewUpgradeStand(airlocktest.UpgradeOptions{Path: upgradePath}).Answer)
	services[standGitHub].AnswerWith(airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: o.installations}).Answer)
	services[standGoogle].AnswerWith(airlocktest.NewGoogleStand(airlocktest.GoogleOptions{Lifetime: o.googleLifetime}).Answer)
	// The Microsoft stand plays both hosts of its one certificate: the authority a federated
	// token is exchanged at, and the Resource Manager that access token is then spent at.
	services[standMicrosoft].AnswerWith(airlocktest.Answers(
		airlocktest.NewMicrosoftStand(airlocktest.MicrosoftOptions{}).Answer,
		airlocktest.NewAzureResourceStand(airlocktest.AzureResourceOptions{}).Answer,
	))
	services[standAWS].AnswerWith(airlocktest.NewAWSStand(airlocktest.AWSOptions{
		KeyID: o.awsKeyID, SecretKey: o.awsSecretKey, SessionToken: o.awsSessionToken,
	}).Answer)
	// The model stand approves until a check sets another answer, so a stack driven with no
	// call to the answer route judges a request through to a grant.
	firstAnswer, err := modelAnswerOf(modelAnswerRequest{Verdict: "approve"})
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: the model stand's first answer: %v\n", err)
		return 1
	}
	model := airlocktest.NewModelStand(airlocktest.ModelOptions{Answer: firstAnswer})
	services[standModel].AnswerWith(model.Answer)
	services[standDispatch].AnswerWith(airlocktest.NewDispatchStand(airlocktest.DispatchOptions{Issues: standInIssues()}).Answer)
	services[standMetadata].AnswerWith(airlocktest.NewMetadataStand(airlocktest.MetadataOptions{}).Answer)

	records := &http.Server{Handler: recordsHandler(services, model), ReadHeaderTimeout: 30 * time.Second}
	recordsURL, stopRecords, err := serveRecords(records, o.recordsAddress)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands: %v\n", err)
		return 1
	}
	defer stopRecords()
	out.RecordsURL, out.ModelAnswerURL = recordsURL, recordsURL+modelAnswerPath
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

// recordsHandler is the two things the recording server answers: GET /records/<stand>, the
// JSON array of every request that stand was sent, each record's method, path, query, host,
// header and body, which is what a check script reads a result back from; and the model stand's
// answer route, which is served here because a check that drives one case after another needs
// one address to set them at and the recording server is the address it already has.
func recordsHandler(services map[string]*airlocktest.Service, model *airlocktest.ModelStand) http.Handler {
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
	mux.HandleFunc("POST "+modelAnswerPath, func(w http.ResponseWriter, r *http.Request) {
		var body modelAnswerRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxModelAnswerBody)).Decode(&body); err != nil {
			http.Error(w, "the answer is no JSON object: "+err.Error(), http.StatusBadRequest)
			return
		}
		answer, err := modelAnswerOf(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		model.Answers(answer)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": answer.Text, "delay": answer.Delay.String()})
	})
	return mux
}

// modelAnswerPath is where a check sets what the model stand answers from there on, and
// maxModelAnswerBody what one such request is read of: a reply is a sentence or two, and a body
// past this is a mistake rather than an answer.
const (
	modelAnswerPath    = "/answers/model"
	maxModelAnswerBody = 1 << 20
)

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
}

// modelAnswerOf is one such request as the answer it sets, and what is wrong with a request
// that sets none.
func modelAnswerOf(body modelAnswerRequest) (airlocktest.ModelAnswer, error) {
	var answer airlocktest.ModelAnswer
	switch {
	case body.Verdict != "" && body.Text != "":
		return answer, errors.New("the answer names both a verdict and a text, and it is one or the other")
	case body.Verdict == "" && body.Text == "" && body.Delay == "":
		return answer, errors.New("the answer names no verdict, no text and no delay")
	case body.Verdict != "":
		reason := body.Reason
		if reason == "" {
			reason = "the stand-in model answered " + body.Verdict
		}
		encoded, err := json.Marshal(map[string]string{"verdict": body.Verdict, "reason": reason})
		if err != nil {
			return airlocktest.ModelAnswer{}, fmt.Errorf("the verdict could not be encoded: %w", err)
		}
		answer.Text = string(encoded)
	default:
		answer.Text = body.Text
	}
	if body.Delay != "" {
		delay, err := time.ParseDuration(body.Delay)
		if err != nil || delay < 0 {
			return airlocktest.ModelAnswer{}, fmt.Errorf("the delay %q is no duration to hold an answer back by", body.Delay)
		}
		answer.Delay = delay
	}
	return answer, nil
}
