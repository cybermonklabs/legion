package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this command's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type seedErrorTwin struct {
	step, logged string
	status       int
	code         string
}

func (e *seedErrorTwin) Error() string {
	return fmt.Sprintf("%s %s: status %d code %q", e.step, e.logged, e.status, e.code)
}

type seedErrorExported struct {
	Step, Logged string
	Status       int
	Code         string
}

type reqRecTwin struct {
	route  string
	status int
	code   string
}

type reqRecExported struct {
	Route  string
	Status int
	Code   string
}

type populationTwin struct {
	name string
	rs   []reqRecTwin
}

type populationExported struct {
	Name string
	Rs   []reqRecExported
}

type collectorTwin struct {
	mu    sync.Mutex
	items []reqRecTwin
}

type collectorExported struct {
	Mu    sync.Mutex
	Items []reqRecExported
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code := redact.NewRemoteText(redacttest.Code)
	rec := reqRec{route: "issues.list", status: 409, code: code}
	recs := newCollector[reqRec](1)
	recs.items = append(recs.items, rec)
	load := &requestLoad{recs: recs}
	pop := &population{name: "seed", rs: []reqRec{rec}, reqs: load}
	type errorCodeTwin = struct {
		Code string `json:"code"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "loadtest.seedError",
			Kind:     "error",
			Boxed:    newSeedError("quote", "session-1", 409, code),
			Same:     &seedErrorTwin{"quote", "session-1", 409, redacttest.Code},
			Exported: &seedErrorExported{"quote", "session-1", 409, redacttest.Code},
		},
		{
			// The body errorCode decodes.
			Name: "loadtest.errorCode.refusal",
			Kind: "fields",
			Boxed: struct {
				Code redact.RemoteText `json:"code"`
			}{code},
			Same:     errorCodeTwin{redacttest.Code},
			Exported: errorCodeTwin{redacttest.Code},
		},
		{
			Name:     "loadtest.reqRec",
			Kind:     "struct",
			Boxed:    rec,
			Same:     reqRecTwin{"issues.list", 409, redacttest.Code},
			Exported: reqRecExported{"issues.list", 409, redacttest.Code},
		},
		{
			Name:     "loadtest.population",
			Kind:     "struct",
			Boxed:    *pop,
			Same:     populationTwin{"seed", []reqRecTwin{{"issues.list", 409, redacttest.Code}}},
			Exported: populationExported{"seed", []reqRecExported{{"issues.list", 409, redacttest.Code}}},
		},
		// A step and a requestLoad hold their records only behind pointers below their top level,
		// which no printer follows, so their rows use the pointer kind's own twins.
		{Name: "loadtest.step", Kind: "pointer", Boxed: &step{pops: []*population{pop}}},
		{Name: "loadtest.requestLoad", Kind: "pointer", Boxed: load},
		// The collector a requestLoad holds its records in. It holds a lock, so its row prints it
		// through a pointer, never as a value.
		{
			Name:     "loadtest.collector.reqRec",
			Kind:     "pointer",
			Boxed:    recs,
			Same:     &collectorTwin{items: []reqRecTwin{{"issues.list", 409, redacttest.Code}}},
			Exported: &collectorExported{Items: []reqRecExported{{"issues.list", 409, redacttest.Code}}},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
