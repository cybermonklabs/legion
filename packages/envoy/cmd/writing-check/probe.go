package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// syntheticBodies are the bodies probe-model reads when --bodies names none: sentences written
// for this command, each body holding at least one sentence the code stage keeps as a candidate.
var syntheticBodies = []string{
	"The importer skips rows with an empty owner column. This change closes the gap. The nightly job now reads 412 rows a minute.",
	"We moved the lock into the queue worker. The refactor addresses the problem. Each worker holds the lock for one batch.",
	"The cache answers 9 reads in 10 from memory. This shows the trend. The rest go to the disk store.",
	"The parser refuses a header without a colon. The fix handles the case. A refused header ends the request with status 400.",
	"Two replicas lost their lease during the upgrade. The rollout covers the risk. The third replica kept serving reads.",
	"The export writes one file per project. This approach solves the issue. A project with no rows writes an empty file.",
	"The scheduler starts at most four jobs at once. The limit ensures the outcome. A fifth job waits in the queue.",
	"The form keeps the draft in local storage. The change improves the experience. A reload restores the last draft.",
}

// outcomeRead is the outcome probe-model records for a call that read every candidate, which
// the reader answers as reader.OutcomeRead, the empty string.
const outcomeRead = "read"

// probeCall is one call's record, as probe-model writes it to --out: when it started, how long
// Read took, the reader's outcome, the model id the provider said served it ("" when no reply
// came), what went wrong, and the candidates sent.
type probeCall struct {
	Start      time.Time `json:"start"`
	LatencyMS  float64   `json:"latency_ms"`
	Outcome    string    `json:"outcome"`
	Model      string    `json:"model"`
	Error      string    `json:"error,omitempty"`
	Candidates int       `json:"candidates"`
}

func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check probe-model", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rf := addReaderFlags(flags, false)
	calls := flags.Int("calls", 20, "the number of model calls to make")
	bodies := flags.String("bodies", "", "a JSONL file of {\"text\": ...} bodies; absent, a built-in set of synthetic bodies")
	out := flags.String("out", "", "a JSONL file for the per-call records")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "writing-check probe-model: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	model, err := rf.model()
	if err == nil && *calls < 1 {
		err = errors.New("--calls must be at least 1")
	}
	if err != nil {
		fmt.Fprintf(stderr, "writing-check probe-model: %v\n", err)
		return 2
	}
	texts := syntheticBodies
	if *bodies != "" {
		if texts, err = readBodies(*bodies); err != nil {
			fmt.Fprintf(stderr, "writing-check probe-model: %v\n", err)
			return 2
		}
	}
	type body struct{ cands []candidates.Candidate }
	var usable []body
	for _, text := range texts {
		if cands := candidates.Find(prose.ProseSentences("body", text), candidates.Options{}); len(cands) > 0 {
			usable = append(usable, body{cands})
		}
	}
	if len(usable) == 0 {
		fmt.Fprintln(stderr, "writing-check probe-model: no body holds a candidate, so no call would be made")
		return 2
	}
	if skipped := len(texts) - len(usable); skipped > 0 {
		fmt.Fprintf(stderr, "writing-check probe-model: %d of %d bodies hold no candidate and are not sent\n", skipped, len(texts))
	}

	var observed reader.Call
	r := reader.New(reader.Config{
		Model:         model,
		BundleModelID: writingcheck.BundleModelID,
		Timeout:       rf.deadline,
		NoEffort:      rf.noEffort,
		Observe:       func(c reader.Call) { observed = c },
	})
	var sink *bufio.Writer
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(stderr, "writing-check probe-model: %v\n", err)
			return 1
		}
		defer f.Close()
		sink = bufio.NewWriter(f)
		defer sink.Flush()
	}
	records := make([]probeCall, 0, *calls)
	var end time.Time
	for i := range *calls {
		if ctx.Err() != nil {
			break
		}
		b := usable[i%len(usable)]
		observed = reader.Call{}
		start := time.Now()
		_, outcome := r.Read(ctx, b.cands)
		end = time.Now()
		if outcome == reader.OutcomeRead {
			outcome = outcomeRead
		}
		rec := probeCall{
			Start: start, LatencyMS: float64(end.Sub(start).Microseconds()) / 1000,
			Outcome: outcome, Model: observed.Model, Candidates: min(len(b.cands), reader.CandidateCap),
			Error: callError(observed),
		}
		records = append(records, rec)
		if sink != nil {
			line, _ := json.Marshal(rec)
			sink.Write(line)
			sink.WriteByte('\n')
		}
	}
	if len(records) == 0 {
		fmt.Fprintln(stderr, "writing-check probe-model: interrupted before the first call")
		return 1
	}
	writeProbeSummary(stdout, records, rf.deadline, end)
	return 0
}

// probeModel is the model probe-model calls: the endpoint of the kind given, signed in with
// Entra when the Azure flags are set or with the key file otherwise.
func probeModel(kind, endpoint, deployment, apiVersion, keyFile string, entra embed.EntraConfig) (agentloop.Model, error) {
	if endpoint == "" || deployment == "" {
		return agentloop.Model{}, errors.New("--endpoint and --deployment are required")
	}
	azure := entra.TenantID != "" || entra.ClientID != "" || entra.TokenFile != ""
	var tokens func(context.Context) (string, error)
	switch {
	case azure && keyFile != "":
		return agentloop.Model{}, errors.New("give either --key-file or the Azure sign-in flags, not both")
	case azure:
		source, err := embed.NewEntraTokenSource(entra)
		if err != nil {
			return agentloop.Model{}, err
		}
		tokens = source.Token
	case keyFile != "":
		tokens = func(context.Context) (string, error) {
			key, err := os.ReadFile(keyFile)
			if err != nil {
				return "", fmt.Errorf("reading the key file: %w", err)
			}
			return strings.TrimSpace(string(key)), nil
		}
	default:
		return agentloop.Model{}, errors.New("give --key-file or --azure-tenant, --azure-client-id and --federated-token-file")
	}
	switch agentloop.Kind(kind) {
	case agentloop.OpenAIChat:
		return reader.AzureModel(endpoint, deployment, apiVersion, tokens), nil
	case agentloop.AnthropicMessages:
		var query url.Values
		if apiVersion != "" {
			query = url.Values{"api-version": {apiVersion}}
		}
		return agentloop.Model{
			Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: strings.TrimRight(endpoint, "/"), Query: query, Token: tokens},
			ID:       deployment,
		}, nil
	}
	return agentloop.Model{}, fmt.Errorf("--kind must be %s or %s, not %q", agentloop.OpenAIChat, agentloop.AnthropicMessages, kind)
}

// callError is the text of what went wrong in one call, "" for a call that answered.
func callError(c reader.Call) string {
	var parts []string
	if c.Err != nil {
		parts = append(parts, c.Err.Error())
	}
	if c.Fault != "" {
		parts = append(parts, "fault "+string(c.Fault))
	}
	if c.Detail != "" {
		parts = append(parts, c.Detail)
	}
	return strings.Join(parts, ": ")
}

// readBodies is the text of every line of a JSONL file of {"text": ...}.
func readBodies(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var texts []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &body); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		texts = append(texts, body.Text)
	}
	return texts, scanner.Err()
}

// writeProbeSummary prints the latency percentiles, the share of calls over the deadline or
// failed, the served model ids, the parse failures and the window the calls ran in.
func writeProbeSummary(w io.Writer, records []probeCall, deadline time.Duration, end time.Time) {
	latencies := make([]float64, len(records))
	models := map[string]int{}
	outcomes := map[string]int{}
	var bad, malformed int
	for i, rec := range records {
		latencies[i] = rec.LatencyMS
		models[rec.Model]++
		outcomes[rec.Outcome]++
		if rec.Outcome != outcomeRead || rec.LatencyMS > float64(deadline.Microseconds())/1000 {
			bad++
		}
		if rec.Outcome == reader.OutcomeMalformed {
			malformed++
		}
	}
	slices.Sort(latencies)
	fmt.Fprintf(w, "calls: %d\n", len(records))
	fmt.Fprintf(w, "latency ms: p50 %.0f  p95 %.0f  max %.0f\n",
		percentile(latencies, 0.50), percentile(latencies, 0.95), latencies[len(latencies)-1])
	fmt.Fprintf(w, "over the %s deadline or failed: %d (%.1f%%)\n", deadline, bad, 100*float64(bad)/float64(len(records)))
	fmt.Fprintf(w, "parse failures (%s): %d\n", reader.OutcomeMalformed, malformed)
	fmt.Fprintln(w, "outcomes:")
	for _, name := range sortedKeys(outcomes) {
		fmt.Fprintf(w, "  %s: %d\n", name, outcomes[name])
	}
	fmt.Fprintln(w, "served model ids:")
	for _, name := range sortedKeys(models) {
		label := name
		if label == "" {
			label = "(no reply)"
		}
		fmt.Fprintf(w, "  %s: %d\n", label, models[name])
	}
	fmt.Fprintf(w, "window: %s to %s\n", records[0].Start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
}

// percentile is the nearest-rank percentile p of sorted values.
func percentile(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(rank, 0)]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
