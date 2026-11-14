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
	"strconv"
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
// notification handler, a read that fails leaves the installed value and is made again, and a
// value this build cannot install - one that fails validation, one above its own version -
// leaves the process on the schema it has.
func TestTheInstalledSchemaIsOneAtomicValue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	// The installed schema is process-wide. The hook puts back the one this binary booted
	// with once the test is over.
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))

	requireTheRegistrationIsAWake(t)
	requireTheServerClosesItsSocketsOnASwitch(t)

	built := pmdoc.BuiltSchemaVersion()
	if built < 2 {
		t.Fatalf("this build's schema version is %d; the test needs two versions it can install", built)
	}
	addTheClaimType(t, ctx, pool)
	requireReadersSeeWholeValues(t, ctx, pool, built)
	requireAFailedReadKeepsTheValueAndIsMadeAgain(t, ctx)
	requireARefusedValueKeepsTheValue(t, ctx, pool, built)
	requireASchemaAboveThisBuildIsNotInstalled(t, ctx, pool, built)
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
// race detector is on, so a map written after it was published fails the run too.
func requireReadersSeeWholeValues(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	flip := func(version int, claimState string) {
		// One statement, so the store itself never answers half a change: the version every
		// type carries and whether the claim type is live move together.
		if _, err := pool.Exec(ctx, `
			update graph_nodes
			set attributes = case when coalesce((attributes->>'block_type')::boolean, false)
					then jsonb_set(attributes, '{version}', to_jsonb($1::int))
					else attributes end,
				state = case when id = 'claim' then $2::text else state end
			where kind = $3
		`, version, claimState, KindSupertag); err != nil {
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
		if round%2 == 0 {
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
		time.Sleep(time.Millisecond)
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
	reads := unreachable.reads
	if len(reads) < 2 {
		t.Errorf("a store that cannot be read was read %d times in 300ms at a %v retry, want it read again", len(reads), retry)
	}
	for index := 1; index < len(reads); index++ {
		if gap := reads[index].Sub(reads[index-1]); gap < retry {
			t.Errorf("read %d of a store that cannot be read came %v after the read before it, want at least the retry interval %v (%d reads in 300ms): a retry that does not wait reads a store that is down as fast as it fails",
				index+1, gap, retry, len(reads))
			break
		}
	}
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("a read that failed changed the installed schema")
	}
	if reason := pmdoc.NotReadyReason(); reason != "" {
		t.Errorf("a read that failed made the process not ready (%q); only a schema above this build's does", reason)
	}
}

// requireARefusedValueKeepsTheValue holds a store schema that fails validation to installing
// nothing, with the process still serving the schema it had and still ready, and so to running
// no switch: the document sockets the server holds were admitted on the schema it still serves.
func requireARefusedValueKeepsTheValue(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	before := pmdoc.SchemaJSON()
	setStoreVersion(t, ctx, pool, 0)
	switches := 0
	installer := &Installer{Store: pool, OnSwitch: func() { switches++ }}
	if !installer.reload(ctx) {
		t.Errorf("a schema the install refused was reported as a read that failed; it is an answer, and no retry is owed")
	}
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("a schema that fails validation was installed")
	}
	if reason := pmdoc.NotReadyReason(); reason != "" {
		t.Errorf("a schema that fails validation made the process not ready (%q)", reason)
	}
	if switches != 0 {
		t.Errorf("a schema that fails validation ran OnSwitch %d times, want none: the process serves the schema it served, so no document socket is owed a close", switches)
	}
	setStoreVersion(t, ctx, pool, built)
}

// requireASchemaAboveThisBuildIsNotInstalled holds a store schema above the version this build
// was made for to installing nothing and to making the process report itself not ready, naming
// the version. A store back at a version it can install makes it ready again. Each of those two
// reads moves what the process serves, so each runs OnSwitch once, the hook the server closes
// its document sockets on; a second read of the same store moves nothing and runs it not at all.
func requireASchemaAboveThisBuildIsNotInstalled(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	before := pmdoc.SchemaJSON()
	setStoreVersion(t, ctx, pool, built+1)
	switches := 0
	installer := &Installer{Store: pool, OnSwitch: func() { switches++ }}
	installer.reload(ctx)
	if string(pmdoc.SchemaJSON()) != string(before) {
		t.Errorf("a schema above this build's version was installed")
	}
	reason := pmdoc.NotReadyReason()
	if !strings.Contains(reason, strconv.Itoa(built+1)) || !strings.Contains(reason, strconv.Itoa(built)) {
		t.Errorf("the process is not ready because %q, want a reason naming the store's version %d and this build's %d", reason, built+1, built)
	}
	if switches != 1 {
		t.Errorf("the read of a schema above this build's version ran OnSwitch %d times, want once: the process serves no schema now, and the server closes its document sockets there", switches)
	}
	away := switches
	installer.reload(ctx)
	if switches != away {
		t.Errorf("a second read of the same schema above this build's version ran OnSwitch %d more times, want none: nothing moved, so no document socket is owed a close", switches-away)
	}
	setStoreVersion(t, ctx, pool, built)
	installer.reload(ctx)
	if reason := pmdoc.NotReadyReason(); reason != "" {
		t.Errorf("the process is still not ready (%q) after the store came back to a version it can install", reason)
	}
	if pmdoc.SchemaVersion() != built {
		t.Errorf("the installed schema is version %d, want the store's %d", pmdoc.SchemaVersion(), built)
	}
	if switches != 2 {
		t.Errorf("the store's return to version %d left OnSwitch run %d times in all, want twice: once for the read that left the process serving no schema, once for the read that installed one again", built, switches)
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

func (s *unreachableStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	s.reads = append(s.reads, time.Now())
	return nil, errUnreachable
}

func (s *unreachableStore) QueryRow(context.Context, string, ...any) pgx.Row {
	s.reads = append(s.reads, time.Now())
	return unreachableRow{}
}

type unreachableRow struct{}

func (unreachableRow) Scan(...any) error { return errUnreachable }
