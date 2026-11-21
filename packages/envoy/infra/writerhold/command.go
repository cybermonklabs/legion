package writerhold

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	compute "google.golang.org/api/compute/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/envoy/infra/internal/flagerr"
	"github.com/sjawhar/envoy/infra/seed"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// SigningKeySecret is the Secret Manager secret that holds the private half of the operator's hold
// signing key, which the operator's Google service account alone can read.
const SigningKeySecret = "operator-only-envoy-hold-signing"

// The words of `envoy-infra hold`.
const (
	actionStatus  = "status"
	actionPrepare = "prepare"
	actionSet     = "set"
	actionAbort   = "abort"
	actionRelease = "release"
	actionRestore = "restore"
	actionProve   = "prove"
	actionKeygen  = "keygen"
)

const usage = `usage: envoy-infra hold status|prepare|set|abort|release|restore|prove|keygen [options]

  status   --broker-url U --token-file F --verify-keys K [--hold ID]
  prepare  --workflow W --reason R --minters-report F   (and the writer's options below)
  set      (--hold ID | --workflow W --reason R) --minters-report F [--restored T=D]... [--snapshot T=S]...
  abort    --hold ID --reason R
  release  --hold ID --reason R
  restore  (--bootstrap | --hold ID) --reason R
  prove    --hold ID --restored T=D...
  keygen   --signing-key-secret projects/P/secrets/` + SigningKeySecret + `

  Every command but keygen: --broker-url U (the secrets broker), --token-file F (the operator's
  ServiceAccount token for the broker's NATS audience), --verify-keys K (BROKER_NATS_HOLD_SIGNING_KEYS).
  Every command that writes the hold: --signing-key-secret projects/P/secrets/` + SigningKeySecret + `.
  Every command that reaches the hub: --nats URL... and --probe-token-file F, and the hub's
  Kubernetes objects through --kube-namespace (envoy), --statefulset (envoy-nats) and --configmap
  (envoy-nats-config), with the in-cluster or KUBECONFIG configuration.

Exit codes: 0 done; 1 refused, naming what; 2 a usage error, or UNKNOWN: a reading that is not
whole, which is never a pass.
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// options are a run's flags.
type options struct {
	action                                      string
	brokerURL, tokenFile, verifyKeys, keySecret string
	holdID, workflow, reason, mintersReport     string
	bootstrap                                   bool
	restored, snapshots                         stringList
	nats                                        stringList
	probeTokenFile                              string
	namespace, statefulSet, configMap           string
}

// KeyStore is the Secret Manager store of the operator's hold signing key: keygen writes its first
// version, and every command that writes the hold reads the version it serves. seed.Store, which
// `envoy-infra seed` writes nkey seeds through, is one.
type KeyStore interface {
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	Current(ctx context.Context, name string) (version string, value []byte, err error)
}

// env is what a run reaches outside the process. Command gives the real one (realEnv); a test gives
// its own.
type env struct {
	// records opens the operator's client of the broker's hold routes for o, with the signing key
	// when signing.
	records func(ctx context.Context, o options, signing bool) (*holdrecord.Client, error)
	// keys opens the signing key's store.
	keys func(ctx context.Context) (KeyStore, error)
	// hub is the operator's reach of the hub for o: its Kubernetes objects, its NATS connections
	// and the probe.
	hub func(ctx context.Context, o options) (*Operator, error)
	now func() time.Time
}

// Command runs `envoy-infra hold` with args, the words after "hold", and returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, realEnv())
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	actions := []string{actionStatus, actionPrepare, actionSet, actionAbort, actionRelease, actionRestore, actionProve, actionKeygen}
	if len(args) == 0 || !contains(actions, args[0]) {
		fmt.Fprint(stderr, usage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return 0
		}
		return 2
	}
	o, code := parse(args, stderr)
	if code >= 0 {
		return code
	}
	err := dispatch(ctx, o, stdout, stderr, e)
	return report(stderr, o.action, err)
}

// parse reads args into options, or answers the exit code of a usage error (or of --help).
func parse(args []string, stderr io.Writer) (options, int) {
	o := options{action: args[0], namespace: DefaultNamespace, statefulSet: DefaultStatefulSet, configMap: DefaultConfigMap}
	flags := flag.NewFlagSet("envoy-infra hold "+o.action, flag.ContinueOnError)
	var parsed bytes.Buffer
	flags.SetOutput(&parsed)
	flags.StringVar(&o.brokerURL, "broker-url", "", "the secrets broker's base URL")
	flags.StringVar(&o.tokenFile, "token-file", "", "the operator's ServiceAccount token")
	flags.StringVar(&o.verifyKeys, "verify-keys", "", "BROKER_NATS_HOLD_SIGNING_KEYS")
	flags.StringVar(&o.keySecret, "signing-key-secret", "", "projects/<project>/secrets/"+SigningKeySecret)
	flags.StringVar(&o.holdID, "hold", "", "the hold's id")
	flags.StringVar(&o.workflow, "workflow", "", "the hold's workflow")
	flags.StringVar(&o.reason, "reason", "", "why, as the hold row records it")
	flags.StringVar(&o.mintersReport, "minters-report", "", "operator-token-minters.sh's --report file")
	flags.BoolVar(&o.bootstrap, "bootstrap", false, "write the first row, released at gen 1")
	flags.Var(&o.restored, "restored", "a restored target and its disk or volume, <target>=<disk>")
	flags.Var(&o.snapshots, "snapshot", "a restored target and its snapshot, <target>=<snapshot>")
	flags.Var(&o.nats, "nats", "a hub client URL")
	flags.StringVar(&o.probeTokenFile, "probe-token-file", "", "the hold probe's platform token")
	flags.StringVar(&o.namespace, "kube-namespace", o.namespace, "the hub's namespace")
	flags.StringVar(&o.statefulSet, "statefulset", o.statefulSet, "the hub's StatefulSet")
	flags.StringVar(&o.configMap, "configmap", o.configMap, "the hub's ConfigMap")
	if err := flags.Parse(args[1:]); err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stderr, parsed.String())
			return o, 0
		case strings.HasPrefix(err.Error(), "flag needs an argument: -"):
			fmt.Fprintf(stderr, "envoy-infra hold: %v\n%s", err, usage)
		default:
			if name := flagerr.Malformed(flags, err); name != "" {
				fmt.Fprintf(stderr, "envoy-infra hold: --%s: a malformed value (not echoed)\n%s", name, usage)
			} else {
				fmt.Fprintf(stderr, "envoy-infra hold: a flag this command does not take, or a malformed one (not echoed)\n%s", usage)
			}
		}
		return o, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "envoy-infra hold: word %d after \"hold\" is no flag and no flag's value (not echoed)\n%s", len(args)-flags.NArg()+1, usage)
		return o, 2
	}
	// A flag's value reaches the lines the command writes and the hold's records, so a value that
	// holds an nkey seed is refused naming the flag, never the value, as seed and buckets refuse one.
	var seeded []string
	flags.Visit(func(set *flag.Flag) {
		if bus.HoldsSeed(set.Value.String()) {
			seeded = append(seeded, "--"+set.Name)
		}
	})
	if len(seeded) > 0 {
		fmt.Fprintf(stderr, "envoy-infra hold: %s: an nkey seed, which is never an argument (not echoed)\n", strings.Join(seeded, ", "))
		return o, 2
	}
	if problem := o.usageProblem(); problem != "" {
		fmt.Fprintf(stderr, "envoy-infra hold %s: %s\n%s", o.action, problem, usage)
		return o, 2
	}
	return o, -1
}

// usageProblem is what the flags of a run lack or misuse for its action, or "".
func (o options) usageProblem() string {
	need := func(flagName, value string) string {
		if value == "" {
			return "--" + flagName + " is required"
		}
		return ""
	}
	var problems []string
	add := func(p string) {
		if p != "" {
			problems = append(problems, p)
		}
	}
	if o.action == actionKeygen {
		add(need("signing-key-secret", o.keySecret))
		return strings.Join(problems, "; ")
	}
	add(need("broker-url", o.brokerURL))
	add(need("token-file", o.tokenFile))
	add(need("verify-keys", o.verifyKeys))
	writes := o.action != actionStatus
	if writes {
		add(need("signing-key-secret", o.keySecret))
		add(need("reason", reasonFor(o)))
	}
	switch o.action {
	case actionPrepare:
		add(need("workflow", o.workflow))
		add(need("minters-report", o.mintersReport))
		if o.holdID != "" {
			add("--hold is not taken: prepare makes the hold's id")
		}
	case actionSet:
		if o.holdID == "" && o.workflow == "" {
			add("--hold, for a prepared hold, or --workflow, for a hold with no prepare step, is required")
		}
		if o.holdID != "" && o.workflow != "" {
			add("--hold and --workflow are not taken together: a prepared hold's workflow is in its record")
		}
		add(need("minters-report", o.mintersReport))
	case actionAbort, actionRelease:
		add(need("hold", o.holdID))
	case actionRestore:
		if o.bootstrap == (o.holdID != "") {
			add("exactly one of --bootstrap and --hold is required")
		}
	case actionProve:
		add(need("hold", o.holdID))
		if len(o.restored) == 0 {
			add("--restored is required: a prove names each target it proves")
		}
	}
	if o.action == actionSet || o.action == actionProve {
		if len(o.nats) == 0 {
			add("--nats is required: the hub's client URLs")
		}
		add(need("probe-token-file", o.probeTokenFile))
	}
	if o.workflow != "" && !contains(workflows(), o.workflow) {
		add(fmt.Sprintf("--workflow %q is not one of %s", o.workflow, strings.Join(workflows(), ", ")))
	}
	for _, pair := range append(append([]string{}, o.restored...), o.snapshots...) {
		if target, value, ok := strings.Cut(pair, "="); !ok || target == "" || value == "" {
			add(fmt.Sprintf("%q is not <target>=<value>", pair))
		}
	}
	return strings.Join(problems, "; ")
}

// reasonFor is the run's reason; prove takes none of its own, since its entry carries its disks.
func reasonFor(o options) string {
	if o.action == actionProve {
		return "prove"
	}
	return o.reason
}

// workflows are the hold's workflows, as holdrecord's table names them.
func workflows() []string {
	return []string{
		string(holdrecord.WorkflowCutover), string(holdrecord.WorkflowRollback), string(holdrecord.WorkflowSelectionRecovery),
		string(holdrecord.WorkflowBucketsCreate), string(holdrecord.WorkflowBucketsRecreate), string(holdrecord.WorkflowBucketsRestore),
		string(holdrecord.WorkflowStreamsUpdate), string(holdrecord.WorkflowHubRestore), string(holdrecord.WorkflowStreamsCensus),
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// report writes err's line and answers the exit code: 0 for none, 2 for an UNKNOWN reading (an
// *Unknown, or holdrecord.ErrUnknown, a row or record that does not verify), and 1 for every
// refusal.
func report(stderr io.Writer, action string, err error) int {
	if err == nil {
		return 0
	}
	var unknown *Unknown
	if errors.As(err, &unknown) || errors.Is(err, holdrecord.ErrUnknown) {
		line := err.Error()
		if !strings.HasPrefix(line, "UNKNOWN") {
			line = "UNKNOWN " + line
		}
		fmt.Fprintf(stderr, "envoy-infra hold %s: %s\n", action, line)
		return 2
	}
	fmt.Fprintf(stderr, "envoy-infra hold %s: %v\n", action, err)
	return 1
}

func dispatch(ctx context.Context, o options, stdout, stderr io.Writer, e env) error {
	if o.action == actionKeygen {
		store, err := e.keys(ctx)
		if err != nil {
			return err
		}
		return keygen(ctx, store, o.keySecret, stdout, stderr)
	}
	records, err := e.records(ctx, o, o.action != actionStatus)
	if err != nil {
		return err
	}
	switch o.action {
	case actionStatus:
		return status(ctx, records, o.holdID, stdout)
	case actionRestore:
		return restore(ctx, records, o, stdout)
	case actionPrepare:
		op, err := e.hub(ctx, o)
		if err != nil {
			return err
		}
		return prepare(ctx, records, op, o, e.now, stdout)
	case actionAbort:
		op, err := e.hub(ctx, o)
		if err != nil {
			return err
		}
		return abort(ctx, records, op, o, stdout)
	case actionRelease:
		op, err := e.hub(ctx, o)
		if err != nil {
			return err
		}
		return release(ctx, records, op, o, stdout)
	case actionSet:
		op, err := e.hub(ctx, o)
		if err != nil {
			return err
		}
		return set(ctx, records, op, o, e.now, stdout)
	case actionProve:
		op, err := e.hub(ctx, o)
		if err != nil {
			return err
		}
		return prove(ctx, records, op, o, stdout)
	}
	return fmt.Errorf("no action %q", o.action)
}

// status prints the hold row, verified, and, with a hold id, that hold's journal.
func status(ctx context.Context, records *holdrecord.Client, holdID string, stdout io.Writer) error {
	row, err := records.Row(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "row: gen %d, phase %s, hold %q, signed at %s\n", row.Gen, row.Phase, row.HoldID, row.SignedAt.Format(time.RFC3339Nano))
	if holdID == "" {
		holdID = row.HoldID
	}
	if holdID == "" {
		return nil
	}
	d, gen, err := records.Journal(ctx, holdID)
	if err != nil {
		return err
	}
	printJournal(stdout, holdID, d, gen)
	return nil
}

// printJournal prints a hold's journal: its workflow, completion and each entry.
func printJournal(stdout io.Writer, holdID string, d holdrecord.Done, gen uint64) {
	fmt.Fprintf(stdout, "hold %s: workflow %s, record gen %d, complete %t, hold_gen %d\n", holdID, d.Workflow, gen, d.Complete, d.HoldGen)
	for i, op := range d.Ops {
		fmt.Fprintf(stdout, "  %d. %s %s%s%s\n", i+1, op.Command, op.State, mapLine(" inputs", op.Inputs), mapLine(" outputs", op.Outputs))
	}
}

func mapLine(label string, m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return label + " " + strings.Join(parts, " ")
}

// restore writes a lost hold row back through POST /v1/nats-hold/restore: with --bootstrap the
// first row, released at gen 1 with no hold id, which lands only in an empty log; with --hold, the
// row held under that hold id, at the gen the route names (holdrecord.RestoreRow signs it afresh
// there), so a lost row comes back closed and the hold goes on.
func restore(ctx context.Context, records *holdrecord.Client, o options, stdout io.Writer) error {
	phase, holdID := natsgrant.OperatorPhaseHeld, o.holdID
	if o.bootstrap {
		phase, holdID = natsgrant.OperatorPhaseReleased, ""
	}
	row, err := records.RestoreRow(ctx, 1, phase, holdID, o.reason)
	if err != nil {
		var refused *holdrecord.RouteError
		if o.bootstrap && errors.As(err, &refused) && (refused.Code == "gen_mismatch" || refused.Code == "row_exists") {
			return fmt.Errorf("--bootstrap writes the first row only into an empty log, and the broker answered %s: %w", refused.Code, err)
		}
		return err
	}
	fmt.Fprintf(stdout, "restored the row: gen %d, phase %s, hold %q\n", row.Gen, row.Phase, row.HoldID)
	return nil
}

// keygen makes the operator's hold signing key: an Ed25519 pair, whose private half, the seed as
// unpadded base64url, it adds as the first version of the secret; it prints the public half alone,
// in the form BROKER_NATS_HOLD_SIGNING_KEYS takes (signedrecord.ParsePublicKeys). A secret that
// already holds a version is refused by the store, and nothing is written.
func keygen(ctx context.Context, store KeyStore, secret string, stdout, stderr io.Writer) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	seedBytes := private.Seed()
	value := []byte(base64.RawURLEncoding.EncodeToString(seedBytes))
	clear(seedBytes)
	defer clear(value)
	clear(private)
	version, err := store.Put(ctx, secret, value)
	if err != nil {
		return fmt.Errorf("add the signing key to %s: %w", secret, err)
	}
	fmt.Fprintf(stderr, "envoy-infra hold keygen: wrote version %s of %s; the public half follows on standard output\n", version, secret)
	fmt.Fprintln(stdout, base64.RawURLEncoding.EncodeToString(public))
	return nil
}

// signingKey reads the private half from store's served version of secret.
func signingKey(ctx context.Context, store KeyStore, secret string) (ed25519.PrivateKey, error) {
	_, value, err := store.Current(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("read the signing key from %s: %w", secret, err)
	}
	defer clear(value)
	seedBytes, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(value)))
	if err != nil || len(seedBytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s's served version is not the unpadded base64url of a %d-byte Ed25519 seed (not echoed)", secret, ed25519.SeedSize)
	}
	defer clear(seedBytes)
	return ed25519.NewKeyFromSeed(seedBytes), nil
}

// openRecords opens the operator's client of o's broker: the token read from o.tokenFile at each
// request, the verify keys parsed from o.verifyKeys, and, when signing, the private half from the
// key store.
func openRecords(ctx context.Context, o options, signing bool, keys func(context.Context) (KeyStore, error)) (*holdrecord.Client, error) {
	verify, err := signedrecord.ParsePublicKeys("--verify-keys", o.verifyKeys)
	if err != nil {
		return nil, err
	}
	cfg := holdrecord.Config{
		BrokerURL:  o.brokerURL,
		Token:      tokenFromFile(o.tokenFile),
		VerifyKeys: verify,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
	if signing {
		store, err := keys(ctx)
		if err != nil {
			return nil, err
		}
		if cfg.SigningKey, err = signingKey(ctx, store, o.keySecret); err != nil {
			return nil, err
		}
	}
	return holdrecord.New(cfg)
}

// tokenFromFile reads the token at path at each call: the kubelet rewrites a projected token in
// place.
func tokenFromFile(path string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the token file %s: %w", path, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("the token file %s is empty", path)
		}
		return token, nil
	}
}

// newHoldID is a fresh hold id: a nuid, one subject token with no '.', so the bucket barrier's key
// hold.barrier.<hold id> is one key.
func newHoldID() string { return nuid.Next() }

// openKeys opens Secret Manager, as `envoy-infra seed --store gcp-secret-manager` does.
func openKeys(ctx context.Context) (KeyStore, error) { return seed.OpenGCP(ctx, "", "") }

// realEnv is the env of a real run: the broker's routes with the operator's token and the hold
// key from Secret Manager, and the hub through the Kubernetes API (in the operator's Job, the
// in-cluster configuration; elsewhere KUBECONFIG's) and its NATS client URLs.
func realEnv() env {
	return env{
		records: func(ctx context.Context, o options, signing bool) (*holdrecord.Client, error) {
			return openRecords(ctx, o, signing, openKeys)
		},
		keys: openKeys,
		hub:  openOperator,
		now:  time.Now,
	}
}

// openOperator is the real Operator of o.
func openOperator(ctx context.Context, o options) (*Operator, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no Kubernetes configuration, in the cluster or in KUBECONFIG: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Compute API: %w", err)
	}
	urls := strings.Join(o.nats, ",")
	return &Operator{
		Hub: KubeHub{Client: client, Namespace: o.namespace, StatefulSet: o.statefulSet, ConfigMap: o.configMap,
			Forward: APIForwarder{Config: config}},
		Disks: ComputeDisks{Service: computeService},
		URLs:  o.nats,
		Connect: func(context.Context) (*nats.Conn, error) {
			credential, _ := natscred.Service(natscred.ServiceFile(o.tokenFile))
			return nats.Connect(urls, credential, nats.CustomInboxPrefix(natsgrant.OperatorInbox),
				nats.Name("envoy-infra hold"), nats.Timeout(10*time.Second), nats.MaxReconnects(-1))
		},
		Probe: func(_ context.Context, url string) error {
			credential, _ := natscred.Service(natscred.ServiceFile(o.probeTokenFile))
			nc, err := nats.Connect(url, credential, nats.Name("envoy-infra hold probe"), nats.NoReconnect(), nats.Timeout(10*time.Second))
			if err == nil {
				nc.Close()
			}
			return err
		},
	}, nil
}
