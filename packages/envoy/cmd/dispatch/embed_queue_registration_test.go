package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The lines of a process's own log the row below reads, as the binary writes them.
const (
	// admissionRefusedLine is processes.Register's wait on a live peer whose key set this process
	// does not share: the process is in joinRegistry and has not registered.
	admissionRefusedLine = "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse"
	// registeredLine is joinRegistry's, once the process's row is written.
	registeredLine = `msg="dispatch: registered this server process"`
	// coveragePassLine is the embedding queue's boot coverage pass (embedqueue.Cover), which
	// names the identity the process looked up as active (activeIdentity) and how many other
	// identities' vectors it dropped (foreignVectorsDropped).
	coveragePassLine = `msg="dispatch embedqueue: coverage pass"`
)

// passesHold is how long the row below holds both processes in their registration, once both
// wait there, for the coverage passes of a build whose queue starts before registering: such a
// queue starts before the boot reaches the registry at all, and its pass is a few statements and
// one call to a fake on loopback, so it has logged by then or within this. A build that starts
// its queue after registering logs no pass in the hold, which then runs its whole length.
const passesHold = 5 * time.Second

// seededEmbeddedAt is when every seeded vector says it was embedded: long before the row runs,
// so a vector the row's processes dropped and embedded again is told from the one seeded.
var seededEmbeddedAt = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// retiredIdentity is the identity embedder_active records when the row starts: an embedder no
// process of the row is configured with, as one a fleet ran before every process carrying it
// left.
const retiredIdentity = "azure:retired.example/embed-v-4-0"

// Two processes that start at once with different embedders, while no live process carries an
// embedder identity, agree on one active identity and drop none of its vectors (INFRA-557).
// Each process's queue embeds and drops only while its identity is the active one, which it
// looks up over the live registry rows and, when no live row carries an identity, takes as its
// own (embedqueue, active.go). A queue that looked before its process registered was in no live
// row, so two of them each found its own identity active and dropped the other's vectors, and a
// process the registration then refused exited having dropped them. main now starts the queue
// once the process has registered.
//
// The test holds both processes in their registration together, as a live peer whose signing
// keys they do not share holds a deploy's processes (processes.Register's wait): a server_processes
// row of a key neither holds, carrying no embedder identity, stamped an hour ahead so it stays
// live without a heartbeat. Each process logs that it waits on that peer, and the test holds them
// both there until both have run their coverage pass or passesHold has passed, whichever comes
// first, then deletes the peer's row, as its own deregistration does, and both register. So a
// build whose queue starts before registering looks, and drops, while neither process is live;
// this one's queue cannot look before its process's row is live.
//
// The database holds three vectors of each process's identity, windows of no comment, which no
// step of the coverage pass but the identity steps touches, and embedder_active records an
// identity neither process carries. Then:
//   - each process ran its coverage pass after it registered, which is the order main.go has;
//   - the identity each process's pass names active is one a process carries, and every seeded
//     vector of it is the one seeded: neither process dropped the vectors of an identity a
//     process found active. The process whose identity is active drops the other identity's
//     vectors as its pass exists to, so those are not read;
//   - a process whose pass names the other identity active dropped nothing;
//   - both passes name one active identity, and embedder_active records it.
//
// Against main.go with the queue started before joinRegistry, as it was, every one of these
// fails: each process logs its pass before its registration, each pass names its own identity
// active, and each dropped the three vectors of the other's, which the failure names.
func TestTwoProcessesStartingAtOnceWithDifferentEmbeddersAgreeOnOneAndKeepItsVectors(t *testing.T) {
	database := storetest.Open(t)
	fake := startAzureEmbeddingsFake(t)
	ctx := context.Background()

	if _, err := database.Pool.Exec(ctx, `insert into embedder_active (identity) values ($1)`, retiredIdentity); err != nil {
		t.Fatalf("record the retired identity active: %v", err)
	}
	seeded := map[string][]string{}
	for _, deployment := range []string{"embed-x", "embed-y"} {
		identity := fake.identity(t, deployment)
		seeded[identity] = seedWindowVectors(t, database, identity, 3)
	}
	var peer string
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, signing_keys, heartbeat_at)
		values (gen_random_uuid(), array['a-key-neither-process-holds'], now() + interval '1 hour')
		returning instance::text`).Scan(&peer); err != nil {
		t.Fatalf("register the refusing peer: %v", err)
	}

	url := databaseURLOf(t, database)
	named := map[string]*dispatchProcess{
		"A": startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey, Env: fake.settings("embed-x")}),
		"B": startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey, Env: fake.settings("embed-y")}),
	}
	names := []string{"A", "B"}

	// Both wait on the peer. Each tries again every HeartbeatInterval and gives up at the try
	// LiveWindow and one HeartbeatInterval after its first refusal (its line's time), so the peer
	// leaves by LiveWindow less one HeartbeatInterval after the earlier refusal: the try after
	// that admits, with one more left before either gives up.
	var releaseBy time.Time
	for _, name := range names {
		process := named[name]
		process.waitForLog(t, 2*time.Minute, admissionRefusedLine)
		refused, err := time.Parse(time.RFC3339Nano, logAttributes(process.logLines(admissionRefusedLine)[0])["time"])
		if err != nil {
			t.Fatalf("read when %s was first refused: %v", name, err)
		}
		if last := refused.Add(processes.LiveWindow - processes.HeartbeatInterval); releaseBy.IsZero() || last.Before(releaseBy) {
			releaseBy = last
		}
	}
	if held := time.Now().Add(passesHold); held.Before(releaseBy) {
		releaseBy = held
	}
	for time.Now().Before(releaseBy) && !(named["A"].logged(coveragePassLine) && named["B"].logged(coveragePassLine)) {
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, peer); err != nil {
		t.Fatalf("deregister the refusing peer: %v", err)
	}

	passes := map[string]map[string]string{}
	for _, name := range names {
		process := named[name]
		process.waitForLog(t, 30*time.Second, registeredLine)
		process.waitForLog(t, 30*time.Second, coveragePassLine)
		pass, registered := process.logLines(coveragePassLine)[0], process.logLines(registeredLine)[0]
		output := process.Output.String()
		if strings.Index(output, coveragePassLine) < strings.Index(output, registeredLine) {
			t.Errorf("%s ran its coverage pass before it registered, while no live row carried its identity:\n%s\n%s",
				name, pass, registered)
		}
		passes[name] = logAttributes(pass)
		t.Logf("%s's coverage pass: identity=%s activeIdentity=%s foreignVectorsDropped=%s identityStepsSkipped=%s",
			name, passes[name]["identity"], passes[name]["activeIdentity"], passes[name]["foreignVectorsDropped"],
			passes[name]["identityStepsSkipped"])
	}

	for _, name := range names {
		pass := passes[name]
		active, own := pass["activeIdentity"], pass["identity"]
		if seeded[active] == nil {
			t.Errorf("%s's coverage pass found %q active, which neither process carries", name, active)
		} else if dropped := droppedVectors(t, database, seeded[active], active); len(dropped) > 0 {
			t.Errorf("%s's coverage pass found %s active, and %d of its %d seeded vectors are no longer the ones seeded "+
				"(A's pass dropped %s vectors, B's %s): %s", name, active, len(dropped), len(seeded[active]),
				passes["A"]["foreignVectorsDropped"], passes["B"]["foreignVectorsDropped"], strings.Join(dropped, ", "))
		}
		if active != own && pass["foreignVectorsDropped"] != "0" {
			t.Errorf("%s's coverage pass found %s active, not its own %s, and dropped %s vectors",
				name, active, own, pass["foreignVectorsDropped"])
		}
	}
	var recorded string
	if err := database.Pool.QueryRow(ctx, `select identity from embedder_active`).Scan(&recorded); err != nil {
		t.Fatalf("read the active identity: %v", err)
	}
	if passes["A"]["activeIdentity"] != passes["B"]["activeIdentity"] || passes["A"]["activeIdentity"] != recorded {
		t.Errorf("A's coverage pass found %s active and B's %s, and embedder_active records %s; want one identity",
			passes["A"]["activeIdentity"], passes["B"]["activeIdentity"], recorded)
	}
}

// logAttributes is every key=value of one slog text line (logAttribute), a quoted value unquoted,
// and the first of a key that repeats.
func logAttributes(line string) map[string]string {
	attributes := map[string]string{}
	for _, match := range logAttribute.FindAllStringSubmatch(line, -1) {
		value := match[2]
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		if _, ok := attributes[match[1]]; !ok {
			attributes[match[1]] = value
		}
	}
	return attributes
}

// seedWindowVectors writes count comment windows holding a vector identity made, embedded at
// their current text at seededEmbeddedAt, and returns their ids. The windows belong to no
// comment: the coverage pass builds and repairs no window, so only its identity steps touch them,
// and the queue has nothing to embed in them while they hold their vector.
func seedWindowVectors(t *testing.T, database *store.Store, identity string, count int) []string {
	t.Helper()
	vector := make([]float32, embed.Dimension)
	vector[0] = 1
	rows, err := database.Pool.Query(context.Background(), `
		insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, embedded_at, covers)
		select 'comment_block', 'comment/' || gen_random_uuid()::text || '#b1', 'a window ' || $1 || ' embedded',
		       md5($1 || n::text), md5($1 || n::text), $1, $2::vector, $3, array['b1']
		from generate_series(1, $4) n
		returning id`, identity, embed.Literal(vector), seededEmbeddedAt, count)
	if err != nil {
		t.Fatalf("seed %d vectors of %s: %v", count, identity, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("seed %d vectors of %s: %v", count, identity, err)
	}
	if len(ids) != count {
		t.Fatalf("seed %d vectors of %s: wrote %d", count, identity, len(ids))
	}
	return ids
}

// droppedVectors is each of ids whose row no longer holds the vector identity made at
// seededEmbeddedAt: a vector dropped, whether or not it has been embedded again since.
func droppedVectors(t *testing.T, database *store.Store, ids []string, identity string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id from embeddings
		where kind = 'comment_block' and id = any($1)
		  and (embedding is null or model is distinct from $2 or embedded_at is distinct from $3)
		order by id`, ids, identity, seededEmbeddedAt)
	if err != nil {
		t.Fatalf("read the seeded vectors of %s: %v", identity, err)
	}
	dropped, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the seeded vectors of %s: %v", identity, err)
	}
	return dropped
}

// azureEmbeddingsFake is an Azure AI Foundry embeddings endpoint on loopback over TLS, which
// answers every deployment's every call with one unit vector per text, and the file of its
// certificate, which a process trusts through SSL_CERT_FILE as it trusts a real endpoint's
// authority: the process's embedder is the one DISPATCH_EMBEDDER=azure builds, unchanged.
type azureEmbeddingsFake struct {
	endpoint string
	roots    string
}

// startAzureEmbeddingsFake serves the fake until the test ends.
func startAzureEmbeddingsFake(t *testing.T) *azureEmbeddingsFake {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/models/embeddings" || json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, `{"error":{"code":"BadRequest"}}`, http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		answer := struct {
			Data []item `json:"data"`
		}{}
		for index := range request.Input {
			vector := make([]float32, embed.Dimension)
			vector[0] = 1
			answer.Data = append(answer.Data, item{Index: index, Embedding: vector})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	roots := filepath.Join(t.TempDir(), "azure-fake.pem")
	if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write the fake's certificate: %v", err)
	}
	return &azureEmbeddingsFake{endpoint: server.URL + "/models", roots: roots}
}

// settings is the environment that gives a process the fake's deployment as its embedder.
func (f *azureEmbeddingsFake) settings(deployment string) map[string]string {
	return map[string]string{
		"DISPATCH_EMBEDDER":                      "azure",
		"DISPATCH_AZURE_EMBED_ENDPOINT":          f.endpoint,
		"DISPATCH_AZURE_EMBED_DEPLOYMENT":        deployment,
		"DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE": "1000000",
		"DISPATCH_AZURE_EMBED_KEY":               fakeAzureEmbedKey,
		"SSL_CERT_FILE":                          f.roots,
	}
}

// fakeAzureEmbedKey is the key every process of the fake is given; the fake checks none.
const fakeAzureEmbedKey = "azure-fake-key"

// identity is the identity a process given settings(deployment) registers and embeds as, read
// from the embedder that setting builds (azureEmbedderFor), as the process builds it.
func (f *azureEmbeddingsFake) identity(t *testing.T, deployment string) string {
	t.Helper()
	return azureEmbedderFor(t, f.settings(deployment)).Identity()
}
