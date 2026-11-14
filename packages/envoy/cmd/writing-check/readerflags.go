package main

import (
	"context"
	"errors"
	"flag"
	"sync"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// readerFlags are the flags that say how a command reaches the model reader: the endpoint, its
// sign-in and the reader's bounds, as probe-model takes them. The batch commands (score, regress,
// reading) add the bundle's model id and how many writes are read at once.
type readerFlags struct {
	kind, endpoint, deployment, apiVersion string
	tenant, clientID, federated, scope     string
	keyFile                                string
	deadline                               time.Duration
	noEffort                               bool

	modelID     string
	concurrency int
}

// addReaderFlags registers the reader flags on flags; batch adds --model-id and --concurrency.
func addReaderFlags(flags *flag.FlagSet, batch bool) *readerFlags {
	f := &readerFlags{}
	flags.StringVar(&f.kind, "kind", "", "the endpoint's protocol: openai-chat (an Azure AI Foundry deployment) or anthropic-messages (a model gateway)")
	flags.StringVar(&f.endpoint, "endpoint", "", "the endpoint's base URL")
	flags.StringVar(&f.deployment, "deployment", "", "the model id or deployment name")
	flags.StringVar(&f.apiVersion, "api-version", "", "the api-version query parameter, for an Azure deployment")
	flags.StringVar(&f.tenant, "azure-tenant", "", "the Entra tenant id, for keyless Azure sign-in")
	flags.StringVar(&f.clientID, "azure-client-id", "", "the Entra client id, for keyless Azure sign-in")
	flags.StringVar(&f.federated, "federated-token-file", "", "the federated token file Entra trusts for the client id")
	flags.StringVar(&f.scope, "scope", embed.DefaultEntraScope, "the Entra token scope")
	flags.StringVar(&f.keyFile, "key-file", "", "a file holding the endpoint's key (sent as the protocol expects; never printed)")
	flags.DurationVar(&f.deadline, "deadline", reader.DefaultTimeout, "the reader's time bound for one call")
	flags.BoolVar(&f.noEffort, "no-effort", false, "send no reasoning effort (openai-chat only), for a deployment that refuses reasoning_effort")
	f.modelID = writingcheck.BundleModelID
	f.concurrency = 1
	if batch {
		flags.StringVar(&f.modelID, "model-id", writingcheck.BundleModelID, "the model id the bundle pins; a reply served by another model is model_mismatch (empty pins none)")
		flags.IntVar(&f.concurrency, "concurrency", 4, "the writes read at once")
	}
	return f
}

// model is the model the flags name.
func (f *readerFlags) model() (agentloop.Model, error) {
	model, err := probeModel(f.kind, f.endpoint, f.deployment, f.apiVersion, f.keyFile,
		embed.EntraConfig{TenantID: f.tenant, ClientID: f.clientID, TokenFile: f.federated, Scope: f.scope})
	if err != nil {
		return agentloop.Model{}, err
	}
	if f.noEffort && model.Endpoint.Kind != agentloop.OpenAIChat {
		return agentloop.Model{}, errors.New("--no-effort is for openai-chat alone: on anthropic-messages no effort turns thinking on")
	}
	if f.concurrency < 1 {
		return agentloop.Model{}, errors.New("--concurrency must be at least 1")
	}
	return model, nil
}

// observedReader is a reader with the last call it made, for a command that records each call's
// served model and fault. One goroutine uses it at a time: each worker of eachWrite holds its
// own, since a reader's Observe is not told which Read a call belongs to.
type observedReader struct {
	r    *reader.Reader
	last reader.Call
}

func (f *readerFlags) newReader(model agentloop.Model) *observedReader {
	o := &observedReader{}
	o.r = reader.New(reader.Config{
		Model: model, BundleModelID: f.modelID, Timeout: f.deadline, NoEffort: f.noEffort,
		Observe: func(c reader.Call) { o.last = c },
	})
	return o
}

// Read reads cands with the reader, forgetting the call before it. It makes observedReader a
// writingcheck.Reader.
func (o *observedReader) Read(ctx context.Context, cands []candidates.Candidate) ([]reader.Verdict, string) {
	o.last = reader.Call{}
	return o.r.Read(ctx, cands)
}

func (o *observedReader) Calls() int64 { return o.r.Calls() }

// eachWrite calls work for every index below n, from workers goroutines at once. Each worker
// first calls newWorker for the state it alone uses, such as its own reader.
func eachWrite[W any](ctx context.Context, n, workers int, newWorker func() W, work func(w W, i int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, max(n, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newWorker()
			for i := range jobs {
				work(w, i)
			}
		}()
	}
	for i := range n {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
