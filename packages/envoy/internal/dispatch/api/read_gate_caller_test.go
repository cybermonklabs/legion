package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// tokenLookup reports whether statement is a personal token's read of its own row
// (personalTokenActor): one per verification of a `dsp_` credential.
func tokenLookup(statement string) bool {
	statement = strings.TrimSpace(statement)
	return strings.HasPrefix(statement, "select") && strings.Contains(statement, "from agent_tokens") &&
		strings.Contains(statement, "where token_hash = $1")
}

// personRecord reports whether statement is the test identity's record of the person a request
// names (identity.HeaderIdentity, store.PgPeopleStore.Record): one per verification of a person at
// the browser, as a cookie's verification is one read of its session in production.
func personRecord(statement string) bool {
	return strings.Contains(statement, "insert into people (email) values ($1) on conflict (email) do nothing")
}

// callerResolutions is what one request sent to resolve its caller: verified, the statements that
// verify its credential (personRecord, tokenLookup), and resolved, those that resolve its viewer
// (access.Resolve, viewerResolution). The shared token's verification compares the bearer in
// memory and sends nothing, so for it only the viewer's statements are seen.
type callerResolutions struct{ verified, resolved int }

func resolutionsIn(statements []string) callerResolutions {
	var counted callerResolutions
	for _, statement := range statements {
		switch {
		case strings.Contains(statement, viewerResolution):
			counted.resolved++
		case personRecord(statement), tokenLookup(statement):
			counted.verified++
		}
	}
	return counted
}

// readAs makes one GET of target as caller on a recording context and answers the statements it
// made, as read makes one as alice.
func (c *fragmentCensus) readAs(t *testing.T, seed privateSeed, caller privateCaller, target string) (int, []string) {
	t.Helper()
	c.mu.Lock()
	c.statements = nil
	c.mu.Unlock()
	response := seed.requestIn(t, context.WithValue(t.Context(), recordingKey{}, true), caller, http.MethodGet, target, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	return response.Code, slices.Clone(c.statements)
}

// gateWalkTarget is the path the walk reads a GET row at: its wildcards filled by its Probe with
// objects (censusObjects.fill), a deploy run by an id no run holds, since the row resolves its
// caller before it reads one, and the query censusQueries gives the row.
func gateWalkTarget(t *testing.T, objects censusObjects, name, pattern string) string {
	t.Helper()
	probe := readClasses[name].Probe
	for wildcardName, object := range probe {
		if object == probeDeliveryRun {
			pattern = strings.ReplaceAll(pattern, "{"+wildcardName+"}", "1")
		}
	}
	target := objects.fill(t, pattern, probe)
	if query, takes := censusQueries[name]; takes {
		target += "?" + query(objects).Encode()
	}
	return target
}

// TestEveryGETRowResolvesItsCallerOnce walks every GET row the server mounts and reads each as one
// caller of each kind - alice at the browser, carol's personal token that lists PRIV, the shared
// token, and bob, a restricted member - each at the objects of a project it sees, so the read runs
// the statements of its answer. Each request must verify its caller's credential at most once and
// resolve its viewer at most once: the read check resolves both in front of the handler and keeps
// them in the request's context (withinReadScope, withCallerResolved), and every layer and handler
// behind it reads them there. What is counted is the statements a resolution sends, on the
// request's own context (callerResolutions): a person's record by the test identity and a token's
// read of its row for the credential, and access.Resolve's one statement for the viewer. The
// shared token's credential sends none, so it is held to its viewer's alone.
//
// Control: a read check that resolves the viewer and does not keep the caller (readerOf alone)
// leaves each handler that asks for the caller again (requireAuthenticated, actingPerson,
// artifactReaderOf) to verify the credential a second or third time, and the walk fails naming
// each such row.
func TestEveryGETRowResolvesItsCallerOnce(t *testing.T) {
	census := &fragmentCensus{}
	fixture := seedFragmentCensus(t, census)
	callers := []struct {
		caller  privateCaller
		objects censusObjects
	}{
		{callerAlice, fixture.public},
		{callerCarolPrivToken, fixture.private},
		{callerSharedToken, fixture.public},
		{callerBob, fixture.private},
	}
	// spread counts, by caller and by count, the rows whose requests verified or resolved that
	// many times.
	spread := map[string]map[int]int{}
	tally := func(caller privateCaller, what string, count int) {
		key := string(caller) + " " + what
		if spread[key] == nil {
			spread[key] = map[int]int{}
		}
		spread[key][count]++
	}
	var over []string
	walked, verifiedTotal, resolvedTotal := 0, 0, 0
	for _, route := range fixture.server.routes() {
		if route.Method != http.MethodGet || slices.Contains(route.Reads, readclass.TestHook) {
			continue
		}
		name := route.Method + " " + route.Pattern
		var line []string
		for _, read := range callers {
			target := gateWalkTarget(t, read.objects, name, route.Pattern)
			status, statements := census.readAs(t, fixture.privateSeed, read.caller, target)
			counted := resolutionsIn(statements)
			walked++
			verifiedTotal += counted.verified
			resolvedTotal += counted.resolved
			tally(read.caller, "verified", counted.verified)
			tally(read.caller, "resolved", counted.resolved)
			line = append(line, fmt.Sprintf("%s %d: verified %d, resolved %d", read.caller, status, counted.verified, counted.resolved))
			if counted.verified > 1 || counted.resolved > 1 {
				over = append(over, fmt.Sprintf("%s as %s (GET %s, %d): verified its credential %d times and resolved its viewer %d times",
					name, read.caller, target, status, counted.verified, counted.resolved))
			}
		}
		t.Logf("%s: %s", name, strings.Join(line, "; "))
	}
	if walked == 0 {
		t.Fatal("the server mounts no GET row, so the walk read nothing")
	}
	for _, key := range slices.Sorted(maps.Keys(spread)) {
		var counts []string
		for _, count := range slices.Sorted(maps.Keys(spread[key])) {
			counts = append(counts, fmt.Sprintf("%d rows at %d", spread[key][count], count))
		}
		t.Logf("spread, %s: %s", key, strings.Join(counts, ", "))
	}
	t.Logf("%d reads walked: %d credential verifications and %d viewer resolutions in all", walked, verifiedTotal, resolvedTotal)
	if verifiedTotal == 0 || resolvedTotal == 0 {
		t.Fatalf("the walk counted %d verifications and %d resolutions, so the counters see nothing", verifiedTotal, resolvedTotal)
	}
	for _, row := range over {
		t.Error(row)
	}
}

// everyResolution counts, on any context, the statements a caller's resolution sends
// (callerResolutions): for a request a test cannot give a context of its own, such as a socket's
// upgrade, which a real listener serves.
type everyResolution struct{ verified, resolved atomic.Int64 }

func (e *everyResolution) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, viewerResolution):
		e.resolved.Add(1)
	case personRecord(data.SQL), tokenLookup(data.SQL):
		e.verified.Add(1)
	}
	return ctx
}

func (e *everyResolution) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestTheLiveSocketResolvesItsCallerOnceAtOpen: the live socket's upgrade resolves its caller
// once, in front of the handler (whileCallerResolves), which keeps that resolution as the caller
// it admits and hands it to the handler (serveLiveSocket), so opening the socket verifies the
// credential once and resolves the viewer once, for each kind of caller TestEveryGETRowResolvesItsCallerOnce
// walks. The heartbeat's own resolutions come later (StreamHeartbeat), and nothing else of the
// seeded server resolves a caller while the socket opens.
//
// Control: an upgrade that resolves the caller for the heartbeat on a request of its own and
// leaves the handler to resolve it again verifies the credential twice.
func TestTheLiveSocketResolvesItsCallerOnceAtOpen(t *testing.T) {
	counted := &everyResolution{}
	seed := seedPrivateProjectWith(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counted)})
	url := liveURLOf(t, seed.handler)
	for _, caller := range []privateCaller{callerAlice, callerCarolPrivToken, callerSharedToken, callerBob} {
		verified, resolved := counted.verified.Load(), counted.resolved.Load()
		seed.dialLive(t, url, seed.liveHeader(t, caller))
		verified, resolved = counted.verified.Load()-verified, counted.resolved.Load()-resolved
		// The shared token's verification compares the bearer in memory and sends nothing.
		wantVerified := int64(1)
		if caller == callerSharedToken {
			wantVerified = 0
		}
		if verified != wantVerified || resolved != 1 {
			t.Errorf("opening the live socket as %s verified its credential %d times and resolved its viewer %d times, want %d and 1",
				caller, verified, resolved, wantVerified)
		}
	}
}
