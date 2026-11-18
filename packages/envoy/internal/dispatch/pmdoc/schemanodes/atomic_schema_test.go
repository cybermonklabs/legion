package schemanodes

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// serverMain is where the server registers the schema channel. The test reads it, because what
// makes the reinstall safe is that the registration is a Wake and not a Handle.
const serverMain = "../../../../cmd/dispatch/main.go"

// TestTheInstalledSchemaIsOneAtomicValue proves the installed schema is one immutable value
// behind one pointer: it is reinstalled from the store while many goroutines read it, every
// reader sees a whole value, the reinstall runs on a goroutine of its own rather than in a
// notification handler, a read that fails leaves the installed value and is made again, a value
// that fails validation leaves the process on the schema it has and is read again after the retry
// interval, and a store above this build's version is installed at this build's own version, with
// the elements it reads and nothing else.
func TestTheInstalledSchemaIsOneAtomicValue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	// The installed schema is process-wide. The hook puts back the one this binary booted
	// with once the test is over.
	booted := decodeSchema(t, pmdoc.SchemaJSON())
	pmdoctest.UseBlockSchema(t, booted)

	requireTheRegistrationIsAWake(t)
	requireTheServerClosesItsSocketsOnASwitch(t)

	built := pmdoc.BuiltSchemaVersion()
	if built < 2 {
		t.Fatalf("this build's schema version is %d; the test needs two versions it can install", built)
	}
	addTheClaimType(t, ctx, pool)
	requireReadersSeeWholeValues(t, ctx, pool, built, booted)
	requireAFailedReadKeepsTheValueAndIsMadeAgain(t, ctx)
	requireARefusedValueKeepsTheValueAndIsReadAgain(t, ctx, pool, built)
	requireAStoreAboveThisBuildIsInstalledAtItsOwnVersion(t, ctx, pool, built)
}

// requireTheRegistrationIsAWake reads the registration the server makes. A reinstall reads the
// store, which blocks, and a Hub.Handle handler runs on the hub's own goroutine and must not
// block, so the schema channel is registered with Wake and this package registers no handler at
// all.
func requireTheRegistrationIsAWake(t *testing.T) {
	t.Helper()
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, ".", func(entry os.FileInfo) bool {
		return !strings.HasSuffix(entry.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("read this package's own source: %v", err)
	}
	wakes, handles := 0, 0
	for _, parsed := range packages {
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, named := call.Fun.(*ast.SelectorExpr)
			if !named {
				return true
			}
			switch selector.Sel.Name {
			case "Wake":
				wakes++
			case "Handle":
				handles++
			}
			return true
		})
	}
	if wakes != 1 || handles != 0 {
		t.Errorf("this package makes %d Wake and %d Handle registrations, want one Wake and no Handle: a reinstall reads the store and a handler must not block", wakes, handles)
	}

	server, err := os.ReadFile(serverMain)
	if err != nil {
		t.Fatalf("read %s: %v", serverMain, err)
	}
	source := string(server)
	if !strings.Contains(source, "schemanodes.RegisterWake(listenerHub)") {
		t.Errorf("%s does not register the schema channel through schemanodes.RegisterWake", serverMain)
	}
	for _, handler := range []string{"Handle(schemanodes.Channel", "Handle(\"" + Channel + "\""} {
		if strings.Contains(source, handler) {
			t.Errorf("%s registers the schema channel as a notification handler (%s); the reinstall would then run on the hub's goroutine", serverMain, handler)
		}
	}
}

// requireTheServerClosesItsSocketsOnASwitch reads the installer the server runs and fails unless
// its OnSwitch closes the document sockets (docs.Service.CloseDocumentSockets). The hook firing
// is proved below, on the reads that move the schema; this is what makes that firing a close of
// the sockets a browser admitted on the schema the process no longer serves.
func requireTheServerClosesItsSocketsOnASwitch(t *testing.T) {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, serverMain, nil, 0)
	if err != nil {
		t.Fatalf("read %s: %v", serverMain, err)
	}
	installers, closing := 0, 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		named, isNamed := literal.Type.(*ast.SelectorExpr)
		if !isNamed || named.Sel.Name != "Installer" {
			return true
		}
		if from, isIdent := named.X.(*ast.Ident); !isIdent || from.Name != "schemanodes" {
			return true
		}
		installers++
		for _, element := range literal.Elts {
			field, isField := element.(*ast.KeyValueExpr)
			if !isField {
				continue
			}
			if key, isIdent := field.Key.(*ast.Ident); !isIdent || key.Name != "OnSwitch" {
				continue
			}
			ast.Inspect(field.Value, func(inner ast.Node) bool {
				if call, isCall := inner.(*ast.CallExpr); isCall {
					if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && selector.Sel.Name == "CloseDocumentSockets" {
						closing++
					}
				}
				return true
			})
		}
		return true
	})
	if installers != 1 || closing != 1 {
		t.Errorf("%s builds %d schemanodes.Installer values, %d of them with an OnSwitch that calls CloseDocumentSockets; want one installer that closes the document sockets when the schema it serves moves",
			serverMain, installers, closing)
	}
}

// requireReadersSeeWholeValues flips the store between two schemas that must agree - the older
// version without the claim type, this build's version with it - while goroutines read the
// installed value through every reader of the package. A reader that sees a version and a type
// set that disagree has seen a half-built value, which is what the one pointer rules out. The
// race detector is on, so a map written after it was published fails the run too. manifest is
// this build's own, the schema the store holds at built.
func requireReadersSeeWholeValues(t *testing.T, ctx context.Context, pool *store.Pool, built int, manifest pmdoc.BlockSchema) {
	t.Helper()
	flip := func(version int, claimState string) {
		// One statement, so the store itself never answers half a change: the version every
		// type carries, whether the claim type is live and the elements read only from a later
		// version move together. An element whose since is above the version is in no store at
		// that version, since a manifest holding one is refused, so it leaves with the version
		// and comes back with it. A type or an attribute carries its since on its node and goes
		// gone. An item kind of an attribute's items carries it in the attribute's item_versions
		// - plan_task's needs takes an issue from version 6 - and leaves the attribute's items:
		// the statement writes each such attribute's items as this build's manifest gives them
		// at the version (itemsAsOf).
		if _, err := pool.Exec(ctx, `
			update graph_nodes
			set attributes = case
					when kind = $3 and coalesce((attributes->>'block_type')::boolean, false)
						then jsonb_set(attributes, '{version}', to_jsonb($1::int))
					when kind = $4 and $5::jsonb ? id
						then (attributes - 'items' - 'item_versions') || ($5::jsonb -> id)
					else attributes end,
				state = case
					when kind = $3 and id = 'claim' then $2::text
					when attributes ? 'since' then case when (attributes->>'since')::int > $1 then 'gone' else 'live' end
					else state end
			where kind in ($3, $4)
		`, version, claimState, KindSupertag, KindAttribute, itemsAsOf(t, manifest, version)); err != nil {
			t.Errorf("flip the store's schema to version %d: %v", version, err)
		}
	}
	flip(built-1, "gone")
	if err := InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the store's schema: %v", err)
	}

	reading, stopReading := context.WithCancel(ctx)
	var readers sync.WaitGroup
	var versionsSeen sync.Map
	tree, err := pmdoc.ParseRendering("Intro.\n")
	if err != nil {
		t.Fatalf("parse a document for the pointer read: %v", err)
	}
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for reading.Err() == nil {
				// The whole-value read: one load answers the version and the types, and
				// the two must agree.
				var value pmdoc.BlockSchema
				if err := json.Unmarshal(pmdoc.SchemaJSON(), &value); err != nil {
					t.Errorf("decode the installed schema: %v", err)
					return
				}
				holdsClaim := slices.ContainsFunc(value.Types, func(typ pmdoc.BlockTypeSchema) bool {
					return typ.Name == "claim"
				})
				if (value.Version == built) != holdsClaim {
					t.Errorf("a reader saw schema version %d with the claim type held %t: a half-built value", value.Version, holdsClaim)
					return
				}
				versionsSeen.Store(value.Version, struct{}{})
				// The readers that take no lock and load the pointer once each.
				_ = pmdoc.SchemaVersion()
				_ = pmdoc.LinkKinds()
				_ = pmdoc.HasLinkKinds()
				_ = pmdoc.IsTypedBlock("claim")
				_ = pmdoc.BlockPointers(tree)
			}
		}()
	}

	wake := make(chan struct{}, 1)
	installer := &Installer{Store: pool, Wake: wake, Retry: 10 * time.Millisecond}
	installing, stopInstalling := context.WithCancel(ctx)
	var reinstalls sync.WaitGroup
	reinstalls.Add(1)
	go func() {
		defer reinstalls.Done()
		installer.Run(installing)
	}()
	for round := range 40 {
		version := built - 1
		if round%2 == 0 {
			version = built
			flip(built, "live")
		} else {
			flip(built-1, "gone")
		}
		// What the hub's wake says: look again. A signal already buffered is folded into the
		// one there, as Hub.Wake folds its own.
		select {
		case wake <- struct{}{}:
		default:
		}
		// Each flip is installed before the next is written, so the readers read through a
		// reinstall every round however long one read of the store takes on a loaded machine;
		// a fixed pause between flips let the installer read only one of them there.
		deadline := time.Now().Add(10 * time.Second)
		for pmdoc.SchemaVersion() != version {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the installer never installed version %d within 10s of the flip that wrote it", round, version)
			}
			time.Sleep(time.Millisecond)
		}
	}
	stopInstalling()
	reinstalls.Wait()
	stopReading()
	readers.Wait()

	var seen []int
	versionsSeen.Range(func(version, _ any) bool {
		seen = append(seen, version.(int))
		return true
	})
	if len(seen) < 2 {
		t.Errorf("the readers saw the versions %v; the test proves nothing unless the schema was reinstalled under them", seen)
	}
	// The store as the cases after this one start from: this build's version, every element of
	// it live, and the claim type retired.
	flip(built, "gone")
	if err := InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the store's schema at this build's version: %v", err)
	}
}

// requireAFailedReadKeepsTheValueAndIsMadeAgain holds a read that fails to leaving the installed
// value where it is, and to being made again after the retry interval: not before it, or a store
// that is down would be read as fast as it fails, and the server's installer, which sets no
// interval of its own, waits RetryInterval.
func requireAFailedReadKeepsTheValueAndIsMadeAgain(t *testing.T, ctx context.Context) {
	t.Helper()
	if interval := (&Installer{}).retryInterval(); interval != RetryInterval {
		t.Errorf("an installer given no retry interval reads a failed store again after %v, want RetryInterval (%v): the server's installer sets none", interval, RetryInterval)
	}
	before := pmdoc.SchemaJSON()
	const retry = 20 * time.Millisecond
	unreachable := &unreachableStore{}
	installer := &Installer{Store: unreachable, Retry: retry}
	running, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	installer.Run(running)
	requireReadAgainAfter(t, "a store that cannot be read", unreachable.reads, retry)
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("a read that failed changed the installed schema")
	}
}

// requireARefusedValueKeepsTheValueAndIsReadAgain holds a store schema that fails validation to
// installing nothing, with the process still serving the schema it had, and so to running no
// switch: the document sockets the server holds were admitted on the schema it still serves. The
// installer reads such a store again after its retry interval, as it reads one that failed, and
// not before: the next read can find the store a migration has corrected.
func requireARefusedValueKeepsTheValueAndIsReadAgain(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	before := pmdoc.SchemaJSON()
	setStoreVersion(t, ctx, pool, 0)
	switches := 0
	const retry = 20 * time.Millisecond
	counted := &countedStore{pool: pool}
	installer := &Installer{Store: counted, Retry: retry, OnSwitch: func() { switches++ }}
	running, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	installer.Run(running)
	requireReadAgainAfter(t, "a store whose schema fails validation", counted.reads, retry)
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("a schema that fails validation was installed")
	}
	if switches != 0 {
		t.Errorf("a schema that fails validation ran OnSwitch %d times, want none: the process serves the schema it served, so no document socket is owed a close", switches)
	}
	setStoreVersion(t, ctx, pool, built)
}

// requireReadAgainAfter holds reads, the moments an installer read a store it could not install
// from, to a second read and more, each at least retry after the one before.
func requireReadAgainAfter(t *testing.T, what string, reads []time.Time, retry time.Duration) {
	t.Helper()
	if len(reads) < 2 {
		t.Errorf("%s was read %d times in 300ms at a %v retry, want it read again", what, len(reads), retry)
	}
	for index := 1; index < len(reads); index++ {
		if gap := reads[index].Sub(reads[index-1]); gap < retry {
			t.Errorf("read %d of %s came %v after the read before it, want at least the retry interval %v (%d reads in 300ms): a retry that does not wait reads that store as fast as it fails",
				index+1, what, gap, retry, len(reads))
			break
		}
	}
}

// requireAStoreAboveThisBuildIsInstalledAtItsOwnVersion moves the store above this build: one
// migration, in one statement each, raises every block type's version to one past this build's,
// adds a typed block and an attribute of callout that version introduces, and brings back the
// claim type, which declares no since and so is read from version 1. The install keeps this
// build's version and the elements it reads - the claim type among them - and leaves out the two
// the newer version introduces; the process is ready throughout, since nothing it serves is
// refused. The read that brought the claim type in moved what the process serves, so it runs
// OnSwitch once, the hook the server closes its document sockets on; a second read of the same
// store moves nothing and runs it not at all, and the store put back as it was runs it once more.
func requireAStoreAboveThisBuildIsInstalledAtItsOwnVersion(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	before := pmdoc.SchemaJSON()
	const writer = `{"kind": "system", "id": "schemanodes-test-newer"}`
	newer := built + 1
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'future', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 98, 'since', $2::int, 'creatable_since', $2::int),
				now(), $1::jsonb, 0),
			('attribute', 'callout/tone', 'attribute', 'project', 'live',
				jsonb_build_object('name', 'tone', 'store_name', 'tone', 'position', 2, 'kind', 'string',
					'since', $2::int, 'creatable_since', $2::int),
				now(), $1::jsonb, 0)`, []any{writer, newer}},
		{`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		values ('attribute', 'callout/tone', 'attribute_of', 'supertag', 'callout', now(), $1::jsonb, 0, null, true)`, []any{writer}},
		{`update graph_nodes
		set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int)),
			state = case when id = 'claim' then 'live' else state end
		where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)`, []any{newer}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("move the store to version %d: %v", newer, err)
		}
	}
	switches := 0
	installer := &Installer{Store: pool, OnSwitch: func() { switches++ }}
	if !installer.reload(ctx) {
		t.Fatalf("the read of a store at version %d, one above this build, was reported as a read to make again: this build installs it at its own version", newer)
	}
	installed := decodeSchema(t, pmdoc.SchemaJSON())
	if installed.Version != built || pmdoc.SchemaVersion() != built {
		t.Errorf("a store at version %d installed version %d (SchemaVersion %d), want this build's own %d", newer, installed.Version, pmdoc.SchemaVersion(), built)
	}
	if pmdoc.IsTypedBlock("future") {
		t.Errorf("a store at version %d installed the typed block future, which that version introduces: this build reads only what its own version reads", newer)
	}
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("a store at version %d did not install the claim type, which declares no since and so is read from version 1", newer)
	}
	for _, typ := range installed.Types {
		if typ.Name == "callout" && typ.Attributes.Get("tone").Kind != "" {
			t.Errorf("a store at version %d installed callout's tone, which that version introduces", newer)
		}
	}
	if switches != 1 {
		t.Errorf("the read that installed the claim type ran OnSwitch %d times, want once: what the process serves moved, and the server closes its document sockets there", switches)
	}
	if _, err := pmdoc.ParseForWrite(":::future{#f1}\nBody.\n:::\n", nil); err == nil {
		t.Errorf("a write of the typed block future was accepted by a build that installed a store above its own version without it")
	}
	away := switches
	installer.reload(ctx)
	if switches != away {
		t.Errorf("a second read of the same store ran OnSwitch %d more times, want none: nothing moved, so no document socket is owed a close", switches-away)
	}

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`delete from graph_tuples where from_kind = 'attribute' and from_id = 'callout/tone'`, nil},
		{`delete from graph_nodes where (kind, id) in (('supertag', 'future'), ('attribute', 'callout/tone'))`, nil},
		{`update graph_nodes
		set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int)),
			state = case when id = 'claim' then 'gone' else state end
		where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)`, []any{built}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("put the store back at version %d: %v", built, err)
		}
	}
	installer.reload(ctx)
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("the store put back at version %d installed\n%s\nwant the schema served before it moved\n%s", built, pmdoc.SchemaJSON(), before)
	}
	if switches != 2 {
		t.Errorf("the store put back at version %d left OnSwitch run %d times in all, want twice: once for the claim type coming, once for it going", built, switches)
	}
}

// addTheClaimType writes a block type the store alone declares, with one block[] attribute, so
// the test can flip the store between two schemas that differ in their version and their types
// at once.
func addTheClaimType(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	const writer = `{"kind": "system", "id": "schemanodes-test"}`
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'claim', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 99),
				now(), $1::jsonb, 0),
			('attribute', 'claim/supports', 'attribute', 'project', 'live',
				jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0, 'kind', 'block[]',
					'targets', jsonb_build_array('requirement')),
				now(), $1::jsonb, 0)
	`, writer, pmdoc.BuiltSchemaVersion()); err != nil {
		t.Fatalf("write the claim type: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		values ('attribute', 'claim/supports', 'attribute_of', 'supertag', 'claim', now(), $1::jsonb, 0, null, true)
	`, writer); err != nil {
		t.Fatalf("tie the claim type's attribute to it: %v", err)
	}
}

// itemsAsOf is what each attribute of manifest whose items carry versions of their own holds of
// them at version, as a build of that version reads them (pmdoc.Install's asOfVersion): the item
// kinds whose since is at or below version, in the manifest's order, with the versions the
// manifest gives them, and neither key where no item is left. It is one JSON object keyed by the
// attribute's node id, `<type>/<store name>`, so one statement writes every such attribute's
// items. At this build's version it is the manifest's own items, which is what the store's
// migrations wrote.
func itemsAsOf(t *testing.T, manifest pmdoc.BlockSchema, version int) string {
	t.Helper()
	held := make(map[string]map[string]any)
	for _, typ := range manifest.Types {
		for _, attribute := range typ.Attributes {
			if len(attribute.ItemVersions) == 0 {
				continue
			}
			var items []pmdoc.BlockItemKind
			versions := make(map[string]pmdoc.BlockElementVersions)
			for _, item := range attribute.Items {
				given, gives := attribute.ItemVersions[string(item)]
				if max(given.Since, 1) > version {
					continue
				}
				items = append(items, item)
				if gives {
					versions[string(item)] = given
				}
			}
			fields := make(map[string]any)
			if len(items) > 0 {
				fields["items"] = items
				if len(versions) > 0 {
					fields["item_versions"] = versions
				}
			}
			held[typ.Name+"/"+StoreName(typ.Name, attribute.Name)] = fields
		}
	}
	encoded, err := json.Marshal(held)
	if err != nil {
		t.Fatalf("encode the items of the manifest as of version %d: %v", version, err)
	}
	return string(encoded)
}

// setStoreVersion moves the version every block type in the store carries.
func setStoreVersion(t *testing.T, ctx context.Context, pool *store.Pool, version int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = $2 and coalesce((attributes->>'block_type')::boolean, false)
	`, version, KindSupertag); err != nil {
		t.Fatalf("set the store's schema version to %d: %v", version, err)
	}
}

func decodeSchema(t *testing.T, body []byte) pmdoc.BlockSchema {
	t.Helper()
	var value pmdoc.BlockSchema
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode a block schema: %v", err)
	}
	return value
}

// unreachableStore is a store no read reaches, which records when each read of it was made. Run
// reads it on the test's own goroutine, so the record needs no lock.
type unreachableStore struct{ reads []time.Time }

var errUnreachable = errors.New("the store is unreachable")

func (s *unreachableStore) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	s.reads = append(s.reads, time.Now())
	return nil, errUnreachable
}

// countedStore is the store's pool, which records when each read of it began. Run reads it on the
// test's own goroutine, so the record needs no lock.
type countedStore struct {
	pool  *store.Pool
	reads []time.Time
}

func (s *countedStore) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	s.reads = append(s.reads, time.Now())
	return s.pool.BeginTx(ctx, options)
}
