package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// probePrompt is the one message each probe call sends.
const probePrompt = "Reply with the one word: ok"

// routeNotFound is the reason Azure's gateway gives, in the header below, for a deployment it
// does not route to: a 404 with no body.
const (
	routeNotFound      = "route_not_found"
	modelErrorReasonHd = "ms-azureml-model-error-reason"
)

// probeCall is one probe call's outcome.
type probeCall struct {
	Status  int     `json:"status"`
	Reason  string  `json:"reason,omitempty"`
	Served  string  `json:"served,omitempty"`
	Error   string  `json:"error,omitempty"`
	Seconds float64 `json:"seconds"`
}

// ProbeRow is one model's probe: its calls, what answered, and whether it passes.
type ProbeRow struct {
	Model         string         `json:"model"`
	Family        string         `json:"family"`
	Role          string         `json:"role"`
	Calls         int            `json:"calls"`
	OK            int            `json:"ok"`
	RouteNotFound int            `json:"route_not_found"`
	Statuses      map[int]int    `json:"statuses"`
	Served        map[string]int `json:"served"`
	Errors        []string       `json:"errors,omitempty"`
	Latency       [2]float64     `json:"latency_p50_p95_seconds"`
	Pass          bool           `json:"pass"`
	Why           string         `json:"why,omitempty"`
}

// servedMatches is whether served names the model requested: the same id, or one that extends
// it (a dated version, a gateway's provider prefix), with case and the separators "." and "_"
// ignored.
func servedMatches(requested, served string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if _, rest, ok := strings.Cut(s, "/"); ok {
			s = rest
		}
		return strings.NewReplacer(".", "-", "_", "-").Replace(s)
	}
	r, s := norm(requested), norm(served)
	return r == s || strings.HasPrefix(s, r) || strings.HasPrefix(r, s)
}

// modelRefused is whether an error body says the provider does not serve the model.
func modelRefused(status int, body string) bool {
	if status == http.StatusNotFound {
		return true
	}
	lower := strings.ToLower(body)
	return status == http.StatusBadRequest && strings.Contains(lower, "model") &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "invalid") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "not supported"))
}

// prober makes the probe's calls.
type prober struct {
	http    *http.Client
	gateway string
	keyFile string
	foundry llm.FoundryConfig
	tokens  func(context.Context) (string, error)
}

// call makes one probe call on m. A Foundry candidate that sets its effort is asked for it, as
// agentloop asks Chat Completions (`off` sent as `none`), so the probe finds a deployment that
// refuses the effort. A Claude probe asks for no thinking.
func (p *prober) call(ctx context.Context, m ModelEntry) probeCall {
	started := time.Now()
	var req *http.Request
	var err error
	switch m.Family {
	case llm.FamilyClaude:
		body, _ := json.Marshal(map[string]any{"model": m.ID, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": probePrompt}}})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.gateway, "/")+"/v1/messages", bytes.NewReader(body))
		if err == nil {
			key, kerr := os.ReadFile(p.keyFile)
			if kerr != nil {
				return probeCall{Error: "reading the gateway key file: " + kerr.Error()}
			}
			req.Header.Set("X-Api-Key", strings.TrimSpace(string(key)))
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	case llm.FamilyFoundry:
		request := map[string]any{"model": m.ID, "max_tokens": 64, "messages": []map[string]string{{"role": "user", "content": probePrompt}}}
		if effort := m.Effort; effort != "" {
			if effort == "off" {
				effort = "none"
			}
			request["reasoning_effort"] = effort
		}
		body, _ := json.Marshal(request)
		target := strings.TrimRight(p.foundry.Endpoint, "/") + "/chat/completions"
		if p.foundry.APIVersion != "" {
			target += "?api-version=" + url.QueryEscape(p.foundry.APIVersion)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err == nil {
			token, terr := p.tokens(ctx)
			if terr != nil {
				return probeCall{Error: "the Foundry sign-in: " + terr.Error()}
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
	default:
		return probeCall{Error: "family " + m.Family}
	}
	if err != nil {
		return probeCall{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return probeCall{Error: err.Error(), Seconds: time.Since(started).Seconds()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c := probeCall{Status: resp.StatusCode, Reason: resp.Header.Get(modelErrorReasonHd), Seconds: time.Since(started).Seconds()}
	var reply struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &reply) == nil {
		c.Served = reply.Model
	}
	if resp.StatusCode != http.StatusOK {
		c.Error = strings.TrimSpace(string(data))
		if len(c.Error) > 300 {
			c.Error = c.Error[:300]
		}
	}
	return c
}

// probeRow sums a model's calls into its row, named as the candidate is (ModelEntry.Name), and
// decides whether it passes.
func probeRow(m ModelEntry, role string, calls []probeCall) ProbeRow {
	row := ProbeRow{Model: m.Name(), Family: m.Family, Role: role, Calls: len(calls), Statuses: map[int]int{}, Served: map[string]int{}}
	var secs []float64
	var whys []string
	for _, c := range calls {
		row.Statuses[c.Status]++
		if c.Reason == routeNotFound {
			row.RouteNotFound++
		}
		if c.Status == http.StatusOK {
			row.OK++
			secs = append(secs, c.Seconds)
			if c.Served != "" {
				row.Served[c.Served]++
			}
		}
		if c.Error != "" && len(row.Errors) < 3 && !slices.Contains(row.Errors, c.Error) {
			row.Errors = append(row.Errors, c.Error)
		}
		if modelRefused(c.Status, c.Error) {
			whys = append(whys, fmt.Sprintf("status %d: the provider does not serve %q", c.Status, m.ID))
		}
	}
	for served := range row.Served {
		if !servedMatches(m.ID, served) {
			whys = append(whys, fmt.Sprintf("it served %q for %q", served, m.ID))
		}
	}
	if row.RouteNotFound > 0 {
		whys = append(whys, fmt.Sprintf("%d calls answered %s", row.RouteNotFound, routeNotFound))
	}
	if row.OK == 0 {
		whys = append(whys, "no call answered 200")
	}
	slices.Sort(secs)
	if len(secs) > 0 {
		row.Latency = [2]float64{percentile(secs, 0.5), percentile(secs, 0.95)}
	}
	slices.Sort(whys)
	whys = slices.Compact(whys)
	row.Pass = len(whys) == 0
	row.Why = strings.Join(whys, "; ")
	return row
}

func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("calls", 20, "the calls to each model")
	outPath := fs.String("out", "", "a file for the probe's rows as JSON (keep it under $TMPDIR)")
	mf := addModelFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *n < 1 || mf.concurrency < 1 {
		fmt.Fprintln(stderr, "writing-check harness probe: --calls and --concurrency must be above 0, and nothing else")
		return 2
	}
	if *outPath != "" {
		if err := OutsideRepository("--out", *outPath); err != nil {
			fmt.Fprintf(stderr, "writing-check harness probe: %v\n", err)
			return 2
		}
	}
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "writing-check harness probe: %v\n", err)
		return 1
	}
	p := &prober{http: &http.Client{Timeout: 120 * time.Second}, gateway: mf.gateway, keyFile: mf.keyFile, foundry: mf.foundryConfig()}
	models := cfg.AllModels()
	if slices.ContainsFunc(models, func(m ModelEntry) bool { return m.Family == llm.FamilyFoundry }) {
		if p.tokens, err = p.foundry.Tokens(); err != nil {
			fmt.Fprintf(stderr, "writing-check harness probe: %v\n", err)
			return 2
		}
	}
	if p.gateway == "" {
		fmt.Fprintln(stderr, "writing-check harness probe: no model gateway: give --gateway or set ANTHROPIC_BASE_URL")
		return 2
	}
	started := time.Now()
	calls := make([][]probeCall, len(models))
	slots := make(chan struct{}, mf.concurrency)
	var wg sync.WaitGroup
	for i, m := range models {
		calls[i] = make([]probeCall, *n)
		for k := range *n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					calls[i][k] = probeCall{Error: ctx.Err().Error()}
					return
				}
				calls[i][k] = p.call(ctx, m)
				<-slots
			}()
		}
	}
	wg.Wait()
	rows := make([]ProbeRow, len(models))
	pass := true
	for i, m := range models {
		rows[i] = probeRow(m, cfg.RoleOf(m.ID), calls[i])
		pass = pass && rows[i].Pass
	}
	fmt.Fprintf(stdout, "| Model | Family | Role | Calls | 200 | route_not_found | Statuses | Served ids | p50 / p95 s | Pass |\n| --- | --- | --- | ---: | ---: | ---: | --- | --- | --- | --- |\n")
	for _, r := range rows {
		var statuses, served []string
		for _, s := range slices.Sorted(maps.Keys(r.Statuses)) {
			statuses = append(statuses, fmt.Sprintf("%d×%d", s, r.Statuses[s]))
		}
		for _, s := range slices.Sorted(maps.Keys(r.Served)) {
			served = append(served, fmt.Sprintf("%s×%d", s, r.Served[s]))
		}
		verdict := "yes"
		if !r.Pass {
			verdict = "no: " + r.Why
		}
		fmt.Fprintf(stdout, "| %s | %s | %s | %d | %d | %d | %s | %s | %.2f / %.2f | %s |\n",
			r.Model, r.Family, r.Role, r.Calls, r.OK, r.RouteNotFound, strings.Join(statuses, ", "), strings.Join(served, ", "), r.Latency[0], r.Latency[1], verdict)
	}
	for _, pend := range cfg.Models.Pending {
		fmt.Fprintf(stdout, "| (%s) | %s | candidate | 0 | - | - | - | - | - | pending: no deployment id yet |\n", pend.Output, pend.Family)
	}
	fmt.Fprintf(stdout, "\nwall time: %s\n", time.Since(started).Round(time.Second))
	for _, r := range rows {
		for _, e := range r.Errors {
			fmt.Fprintf(stderr, "writing-check harness probe: %s: %s\n", r.Model, e)
		}
	}
	if *outPath != "" {
		if err := writeJSON(*outPath, map[string]any{"time": started.UTC().Format(time.RFC3339), "calls_each": *n, "rows": rows, "pending": cfg.Models.Pending}); err != nil {
			fmt.Fprintf(stderr, "writing-check harness probe: %v\n", err)
			return 1
		}
	}
	if !pass {
		fmt.Fprintln(stderr, "writing-check harness probe: a model failed the probe")
		return 1
	}
	return 0
}
