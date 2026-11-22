package consumernames

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// backupPrinted is the line's end by which backup prints the walk's sha256.
var backupPrinted = regexp.MustCompile(`; backup sha256 ([0-9a-f]{64})\n`)

// backupSHA256Of is the sha256 a backup's output prints, failing t when it prints none.
func backupSHA256Of(t *testing.T, output string) string {
	t.Helper()
	match := backupPrinted.FindStringSubmatch(output)
	require.NotNil(t, match, "the backup printed no sha256: %q", output)
	return match[1]
}

// backupTree is every path under dir, dir itself among them, with what it is: a directory, or a
// file and its bytes.
func backupTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[path] = "a directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[path] = "a file holding " + string(data)
		return nil
	}))
	return tree
}

// consumer-names backup's flags: it requires --out, takes it alone of the words, and takes no
// --hold, since it runs outside a hold; each misuse is a usage error naming the flag, exit 2.
// Control: the same flags otherwise pass the usage check (a run whose env reaches nothing then
// fails past it, exit 1).
func TestTheBackupsFlags(t *testing.T) {
	refused := env{records: func(context.Context, options, bool) (*holdrecord.Client, error) {
		return nil, fmt.Errorf("this run reaches no broker")
	}}
	for _, row := range []struct {
		name    string
		args    []string
		code    int
		problem string
	}{
		{"backup with --out", []string{"backup", "--out", "/nonexistent/walk"}, 1, ""},
		{"backup without --out", []string{"backup"}, 2, "envoy-infra consumer-names backup: --out is required"},
		{"backup with --hold", []string{"backup", "--out", "/nonexistent/walk", "--hold", "h1"}, 2,
			"envoy-infra consumer-names backup: --hold is not backup's: backup runs outside a hold, with the row released"},
		{"--out on another word", []string{"convert", "--hold", "h1", "--out", "/nonexistent/walk"}, 2,
			"envoy-infra consumer-names convert: --out is backup's alone"},
	} {
		t.Run(row.name, func(t *testing.T) {
			code, _, stderr := createBucketRun(t, context.Background(), refused, row.args...)
			require.Equal(t, row.code, code, stderr)
			if row.problem == "" {
				require.NotContains(t, stderr, "usage:", "control: the flags did not pass the usage check")
				return
			}
			require.Contains(t, stderr, row.problem+"\n")
		})
	}
}

// consumer-names backup, with the row released, so under exactly the released grant (the stack's
// auth service answers the operator under the row's phase, natsgrant.Operator(released)), writes
// the selection bucket's walk into --out and prints its sha256. After the bucket is lost, create-
// bucket --after-loss --backup loads that walk in a selection-recovery hold: its selection record
// and its sentinel carry the sha256 the backup printed, and every record comes back. Control, in
// the same hold just before the load: with the backup's directory moved away, the load from --out
// is refused at its read of the backup and writes nothing to the hold's journal, so the walk the
// load then reads is the one backup wrote, and nothing else gives it one.
func TestTheReleasedBackupIsTheWalkTheRecoveryLoads(t *testing.T) {
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	records := createBucketBackedBucket(t, h)

	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase, "the backup is the released operator's")
	out := filepath.Join(t.TempDir(), "walk")
	code, stdout, stderr := h.run("backup", "--out", out)
	require.Equal(t, 0, code, stderr)
	sha := backupSHA256Of(t, stdout)
	manifest, walked, err := kvrecords.Read(out)
	require.NoError(t, err, "the backup's --out holds no walk")
	require.Equal(t, selectionStreamName, manifest.Stream)
	require.NotEmpty(t, walked, "the walk holds no record")

	id := createBucketLoseBucket(t, h)
	aside := out + ".aside"
	require.NoError(t, os.Rename(out, aside))
	code, _, stderr = h.run("create-bucket", "--hold", id, "--after-loss", "--backup", out)
	require.Equal(t, 1, code, "control: the load ran with no backup at --out\n%s", stderr)
	require.Contains(t, stderr, "--backup "+out+" holds neither a jsm.go snapshot nor a "+kvrecords.Format+" walk")
	require.Empty(t, createBucketEntries(h.journal(id), createBucketAfterLossCommand), "control: the refused load wrote to the journal")
	require.NoError(t, os.Rename(aside, out))

	code, stdout, stderr = h.run("create-bucket", "--hold", id, "--after-loss", "--backup", out)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "backup "+sha+"; postcheck passed")
	in, found := createBucketSelection(t, h, id)
	require.True(t, found, "the recovery wrote no selection record")
	require.Equal(t, sha, in.BackupSHA256, "the selection record's backup is not the one backup printed")
	require.Equal(t, createBucketFormatWalk, in.Format)
	_, present, sentinel, found := createBucketState(h)
	require.True(t, present && found, "the recovery left no bucket or no sentinel")
	require.Equal(t, sha, sentinel.state.BackupSHA256, "the sentinel's backup is not the one backup printed")
	nc, _ := h.operator()
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	for key, record := range records {
		got, _, err := store.Read(ctx, key)
		require.NoError(t, err, key)
		record.Key = key
		require.Equal(t, record, got, key)
	}
	h.closeOperators()
	require.NoError(t, h.release(id))
}

// The backup's grant: as a static user granted exactly natsgrant.Operator(released), the backup
// succeeds, and the only subjects it publishes are the selection bucket's STREAM.INFO and
// STREAM.MSG.GET, kvrecords.Write's two calls. Control: for each of the two, the same grant with
// the entry that admits it removed fails the backup, which ends on the server's refusal, reported
// on its connection and named in its error, never on a timeout alone, and writes nothing at --out
// or beside it; the bucket's $JS.API.STREAM.MSG.GET.KV_envoy_session_consumers is one of them.
func TestTheBackupRunsOnTheReleasedGrant(t *testing.T) {
	ctx := context.Background()
	whole := natsgrant.Operator(natsgrant.OperatorPhaseReleased)
	run := func(t *testing.T, grant natsgrant.Permissions) (*grantsRig, int, string, []string, string) {
		t.Helper()
		g := grantsNewRig(t, 1, grant)
		store := createBucketMakeBucket(t, g.hub.admin, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1, Hold: "the-cutover"})
		_, err := store.Write(ctx, "ses_grant", selection.Record{Start: 1}, 0)
		require.NoError(t, err)
		out := filepath.Join(t.TempDir(), "walk")
		stop := g.hub.watch(t)
		code, stdout, stderr := createBucketRun(t, ctx, g.env(), "backup", "--out", out)
		return g, code, stdout + stderr, stop(), out
	}

	_, code, output, published, out := run(t, whole)
	require.Equal(t, 0, code, "the backup under exactly Operator(released)\n%s", output)
	backupSHA256Of(t, output)
	require.DirExists(t, out)
	msgGet := "$JS.API.STREAM.MSG.GET." + selectionStreamName
	require.Equal(t, []string{"$JS.API.STREAM.INFO." + selectionStreamName, msgGet}, published,
		"the subjects the backup published")
	for _, subject := range published {
		require.Contains(t, whole.Publish, subject, "Operator(released) names no entry of %s alone", subject)
		less := whole
		less.Publish = slices.DeleteFunc(slices.Clone(whole.Publish), func(entry string) bool { return grantsAdmits(entry, subject) })
		g, code, output, _, out := run(t, less)
		require.NotEqual(t, 0, code, "control: the backup succeeded without %s\n%s", subject, output)
		require.True(t, grantsNamesOneOf(strings.Join(g.refusals(), "\n"), []string{subject}),
			"control: the server reported no refused publish of %s: %q\n%s", subject, g.refusals(), output)
		require.True(t, grantsNamesOneOf(output, []string{subject}), "control: the backup without %s names no refused publish of it\n%s", subject, output)
		require.Equal(t, map[string]string{filepath.Dir(out): "a directory"}, backupTree(t, filepath.Dir(out)),
			"control: the refused backup wrote at --out or beside it")
		t.Logf("control: the backup without %s exits %d, naming the server's refusal", subject, code)
	}
}

// The backup runs outside a hold: with the row prepare, and then held, under a selection-recovery
// hold, it refuses naming the phase it found, writing nothing at --out and nothing to the hold's
// journal. The bucket it would walk is present and the operator's grant in either phase holds the
// walk's two reads (natsgrant.Operator: each phase's list holds released's), so the phase check is
// all that stands between such a run and a copy.
func TestTheBackupRefusesARowThatIsNotReleased(t *testing.T) {
	h := newStack(t)
	createBucketBackedBucket(t, h)
	id := h.prepared(holdrecord.WorkflowSelectionRecovery)
	for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		if phase == natsgrant.OperatorPhaseHeld {
			h.held(id)
		}
		require.Equal(t, phase, h.row().Phase)
		before := len(h.journal(id).Ops)
		parent := t.TempDir()
		out := filepath.Join(parent, "walk")
		code, _, stderr := h.run("backup", "--out", out)
		require.Equal(t, 1, code, "the backup with the row %s\n%s", phase, stderr)
		require.Contains(t, stderr, fmt.Sprintf("backup runs while the hold row is released, and it is %s (hold %q)", phase, id))
		require.Equal(t, map[string]string{parent: "a directory"}, backupTree(t, parent), "the refused backup wrote at --out or beside it")
		require.Len(t, h.journal(id).Ops, before, "the refused backup wrote to the hold's journal")
	}
}

// The backup writes its copy into a new directory: an --out that exists already, an empty
// directory, a directory holding a file or a file, is refused, naming it, before the run reads the
// hold row or reaches the hub, and --out and its parent are left as they were: no file of the copy
// lands in --out, and no staged directory of kvrecords.Write's is left beside it.
func TestTheBackupRefusesAnOutThatExists(t *testing.T) {
	h := newStack(t)
	createBucketBackedBucket(t, h)
	var reached atomic.Int32
	e := h.e
	records, connect := e.records, e.connect
	e.records = func(ctx context.Context, o options, signing bool) (*holdrecord.Client, error) {
		reached.Add(1)
		return records(ctx, o, signing)
	}
	e.connect = func(ctx context.Context, o options) (*nats.Conn, error) {
		reached.Add(1)
		return connect(ctx, o)
	}
	for _, row := range []struct {
		name string
		make func(t *testing.T, out string)
	}{
		{"an empty directory", func(t *testing.T, out string) { require.NoError(t, os.Mkdir(out, 0o700)) }},
		{"a directory holding a file", func(t *testing.T, out string) {
			require.NoError(t, os.Mkdir(out, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(out, "kept"), []byte("kept"), 0o600))
		}},
		{"a file", func(t *testing.T, out string) { require.NoError(t, os.WriteFile(out, []byte("kept"), 0o600)) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			parent := t.TempDir()
			out := filepath.Join(parent, "walk")
			row.make(t, out)
			before := backupTree(t, parent)
			reached.Store(0)
			code, _, stderr := createBucketRun(t, context.Background(), e, "backup", "--out", out)
			require.Equal(t, 1, code, stderr)
			require.Equal(t, before, backupTree(t, parent), "the refused backup changed --out or wrote beside it")
			require.Zero(t, reached.Load(), "the backup read the hold row or reached the hub before it refused --out")
			require.Contains(t, stderr, "--out "+out+" exists already")
		})
	}
}
