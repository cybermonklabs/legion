package buckets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// noProof is a writer proof that fails t if any part of it runs: the Runner every kubectl, ssh and
// sh call goes through, and an HTTP client every --connz read goes through.
func noProof(t *testing.T) (Runner, *http.Client) {
	t.Helper()
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Errorf("the writer proof ran %s %s", name, strings.Join(args, " "))
		return nil, errors.New("no proof may run")
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("the writer proof read %s", request.URL)
		return nil, errors.New("no proof may run")
	})}
	return run, client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// runCreate runs the command with a writer proof that fails t if it runs (noProof).
func runCreate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	run, client := noProof(t)
	var stdout, stderr strings.Builder
	code := command(context.Background(), args, &stdout, &stderr, run, client)
	return code, stdout.String(), stderr.String()
}

// streamOf reads bucket's stream, failing t when it cannot.
func streamOf(t *testing.T, js natsgo.JetStreamContext, bucket string) *natsgo.StreamInfo {
	t.Helper()
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read KV_%s: %v", bucket, err)
	}
	return info
}

// assertUnchanged fails t unless bucket's stream is the one before: the same stream, configuration
// and messages.
func assertUnchanged(t *testing.T, js natsgo.JetStreamContext, bucket string, before *natsgo.StreamInfo) {
	t.Helper()
	after := streamOf(t, js, bucket)
	if !after.Created.Equal(before.Created) || !reflect.DeepEqual(after.Config, before.Config) || after.State.LastSeq != before.State.LastSeq {
		t.Fatalf("KV_%s after the run: %+v\nwant it as it was: %+v", bucket, after, before)
	}
}

// A create of an absent bucket takes no writer proof: no kubectl, ssh, sh or connz read runs (the
// proof fails the test if any part of it runs), it prints no best-effort warning, and it makes the
// bucket with its own configuration, bus.CopiedBucketConfig: the known-session bucket's 30 days, on
// file storage, at one copy, the stream nats.go makes of that configuration. A proof flag beside it
// is a usage error, since the run would check nothing with it. At b20f45e30a (#4296's head) the
// create with no proof flag was a usage error, exit 2 ("name every place a writer of the deployment
// runs"), and one with an inventory took the proof.
func TestACreateOfAnAbsentBucketTakesNoWriterProof(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, js := server(t)
	code, stdout, stderr := runCreate(t, "create", "--nats", uri, "--bucket", bus.KnownSessionBucket)
	if code != 0 || stdout != "created "+bus.KnownSessionBucket+" (1 copies)\n" || stderr != "" {
		t.Fatalf("create of the absent %s = %d, want 0, %q and nothing on standard error\nstdout: %s\nstderr: %s", bus.KnownSessionBucket, code, "created "+bus.KnownSessionBucket+" (1 copies)", stdout, stderr)
	}
	info := streamOf(t, js, bus.KnownSessionBucket)
	if info.Config.MaxAge != bus.KnownSessionBucketMaxAge || info.Config.Storage != natsgo.FileStorage || info.Config.Replicas != 1 || info.Config.MaxMsgsPerSubject != 1 {
		t.Fatalf("KV_%s after create: %+v; want file storage, one copy, one value a key and max age %s", bus.KnownSessionBucket, info.Config, bus.KnownSessionBucketMaxAge)
	}

	for _, flags := range [][]string{
		{"--host", "box-1"},
		{"--kube-context", "prod"},
		{"--this-machine"},
		{"--connz", "http://127.0.0.1:8222"},
		{"--first-deployment"},
		{"--ssh", "ssm-ssh"},
	} {
		code, stdout, stderr := runCreate(t, append([]string{"create", "--nats", uri, "--bucket", bus.SessionBucket}, flags...)...)
		if want := "create takes no writer proof, so " + flags[0] + " would check nothing"; code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("create with %s = %d, want the usage error %q\n%s%s", flags[0], code, want, stdout, stderr)
		}
	}
	if _, err := js.StreamInfo("KV_" + bus.SessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("a create refused as a usage error made KV_%s: %v", bus.SessionBucket, err)
	}
}

// create can run again. A bucket there at the copies asked for with its own configuration is
// present: the run says so on a line naming it, exits 0, takes no proof, and sends the bucket's
// stream one read and nothing else, so the stream is the one it was, keys and all. The second run
// of the same create, after the first made the bucket, is that run. At b20f45e30a create with no
// proof flag was a usage error, exit 2, and one with an inventory printed "every bucket named
// exists" with no line saying any bucket is present.
func TestACreateOfABucketThereWithItsOwnConfigurationIsPresent(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	_, uri := testnats.Start(t)
	conn, js := jetStream(t, uri)
	requests := watchStreamRequests(t, conn)
	args := []string{"create", "--nats", uri, "--bucket", bus.KnownSessionBucket, "--bucket", bus.RoleClaimBucket}
	if code, stdout, stderr := runCreate(t, args...); code != 0 || strings.Count(stdout, "created ") != 2 {
		t.Fatalf("the first create = %d, want both buckets created\n%s%s", code, stdout, stderr)
	}
	kv, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role-claim bucket: %v", err)
	}
	if _, err := kv.Put("role-1", []byte("held")); err != nil {
		t.Fatalf("write a key: %v", err)
	}
	before := map[string]*natsgo.StreamInfo{}
	for _, bucket := range []string{bus.KnownSessionBucket, bus.RoleClaimBucket} {
		before[bucket] = streamOf(t, js, bucket)
	}
	requests.take(t)

	code, stdout, stderr := runCreate(t, args...)
	want := bus.KnownSessionBucket + " present at 1 copies with its own configuration: left as it is\n" +
		bus.RoleClaimBucket + " present at 1 copies with its own configuration: left as it is\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("the create again = %d, want 0 and\n%s\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
	if got, want := requests.take(t), []string{"INFO.KV_" + bus.KnownSessionBucket, "INFO.KV_" + bus.RoleClaimBucket}; !slices.Equal(got, want) {
		t.Fatalf("the create again sent %v to the buckets' streams, want %v", got, want)
	}
	for bucket, info := range before {
		assertUnchanged(t, js, bucket, info)
	}
	if entry, err := kv.Get("role-1"); err != nil || string(entry.Value()) != "held" {
		t.Fatalf("the key after the create again: %v, %v", entry, err)
	}
}

// A bucket there at other copies than the run asks for refuses the run, exit 1, naming the copies
// it has and the copies asked for, and nothing is created: not the absent bucket named beside it,
// which the run would otherwise make at the copies asked for, and no change to the bucket that is
// there. No proof runs. At b20f45e30a create with no proof flag was a usage error, exit 2, naming
// no copies; with an inventory and every bucket there it exited 0.
func TestACreateOfABucketThereAtOtherCopiesNamesThem(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, js := server(t)
	makeBuckets(t, js, bus.KnownSessionBucket)
	before := streamOf(t, js, bus.KnownSessionBucket)
	code, stdout, stderr := runCreate(t, "create", "--nats", uri, "--replicas", "3", "--bucket", bus.KnownSessionBucket, "--bucket", bus.RoleClaimBucket)
	if want := "differs: KV_" + bus.KnownSessionBucket + ": replicas 1, want 3"; code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "nothing is created") || stdout != "" {
		t.Fatalf("create at 3 copies of a bucket at 1 = %d, want 1 naming %q and nothing created\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
	assertUnchanged(t, js, bus.KnownSessionBucket, before)
	if _, err := js.StreamInfo("KV_" + bus.RoleClaimBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the refused create made KV_%s: %v", bus.RoleClaimBucket, err)
	}
}

// A bucket there at the copies asked for with another configuration than its own refuses the run,
// exit 1, naming each difference: another history and maximum age at once, each named; another
// storage; the discard policy and direct get of a bucket nats.go made before nats-server 2.7.2
// and 2.9; a subject beside the bucket's own; a stream it sources; a republish of its subjects;
// and a mirror of another stream in place of its subjects. The bucket is left as it is. The session
// bucket's maximum age is the one exception: the listeners set it from ENVOY_SESSION_MAX_AGE on the
// running bucket, so another age there is its own and the bucket is present. No proof runs. At
// b20f45e30a create with no proof flag was a usage error, exit 2, naming no field; at 6e52228ad6
// the subjects, source, republish and mirror rows were present, exit 0, since the comparison left
// those four out.
func TestACreateOfABucketThereWithAnotherConfigurationNamesIt(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	for _, c := range []struct {
		name   string
		bucket string
		change func(*natsgo.KeyValueConfig)
		update func(*natsgo.StreamConfig)
		code   int
		says   []string
		// stream, when set, makes the bucket's stream in place of nats.go's create of the bucket.
		stream func(natsgo.JetStreamContext, string) error
	}{
		{"another history and max age", bus.KnownSessionBucket, func(config *natsgo.KeyValueConfig) { config.History, config.TTL = 2, time.Hour }, nil,
			1, []string{"history 2, want 1", fmt.Sprintf("max age 1h0m0s, want %s", bus.KnownSessionBucketMaxAge)}, nil},
		{"another storage", bus.CIStateBucket, func(config *natsgo.KeyValueConfig) { config.Storage = natsgo.MemoryStorage }, nil,
			1, []string{"storage Memory, want File"}, nil},
		{"an old discard policy and no direct get", bus.InterestBucket, nil, func(config *natsgo.StreamConfig) { config.Discard, config.AllowDirect = natsgo.DiscardOld, false },
			1, []string{"discard policy DiscardOld, want DiscardNew", "direct get false, want true"}, nil},
		{"the session bucket at the age ENVOY_SESSION_MAX_AGE set", bus.SessionBucket, func(config *natsgo.KeyValueConfig) { config.TTL = 2 * time.Hour }, nil,
			0, []string{bus.SessionBucket + " present at 1 copies"}, nil},
		{"a subject beside its own", bus.RoleClaimBucket, nil, func(config *natsgo.StreamConfig) { config.Subjects = append(config.Subjects, "envoy_extra.>") },
			1, []string{"subjects $KV." + bus.RoleClaimBucket + ".>,envoy_extra.>, want $KV." + bus.RoleClaimBucket + ".>"}, nil},
		{"a stream it sources", bus.KnownSessionBucket, nil, func(config *natsgo.StreamConfig) { config.Sources = []*natsgo.StreamSource{{Name: "envoy_origin"}} },
			1, []string{"sources envoy_origin, want none"}, nil},
		{"a republish of its subjects", bus.CIStateBucket, nil, func(config *natsgo.StreamConfig) {
			config.RePublish = &natsgo.RePublish{Source: "$KV." + bus.CIStateBucket + ".>", Destination: "envoy_republished.>"}
		}, 1, []string{"republish $KV." + bus.CIStateBucket + ".> to envoy_republished.>, want none"}, nil},
		{"a mirror in place of its subjects", bus.InterestBucket, nil, nil,
			1, []string{"mirror envoy_origin, want none", "subjects none, want $KV." + bus.InterestBucket + ".>"},
			func(js natsgo.JetStreamContext, bucket string) error {
				if _, err := js.AddStream(&natsgo.StreamConfig{Name: "envoy_origin", Subjects: []string{"envoy_origin.>"}}); err != nil {
					return err
				}
				stream, _ := bus.CopiedBucketStreamConfig(bucket, 1, "2.15.1")
				stream.Subjects, stream.Mirror = nil, &natsgo.StreamSource{Name: "envoy_origin"}
				_, err := js.AddStream(&stream)
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			uri, js := server(t)
			config, _ := bus.CopiedBucketConfig(c.bucket, 1)
			if c.change != nil {
				c.change(&config)
			}
			if c.stream != nil {
				if err := c.stream(js, c.bucket); err != nil {
					t.Fatalf("make the bucket's stream: %v", err)
				}
			} else if _, err := js.CreateKeyValue(&config); err != nil {
				t.Fatalf("create the bucket: %v", err)
			}
			if c.update != nil {
				stream := streamOf(t, js, c.bucket).Config
				c.update(&stream)
				if _, err := js.UpdateStream(&stream); err != nil {
					t.Fatalf("update the bucket's stream: %v", err)
				}
			}
			before := streamOf(t, js, c.bucket)
			code, stdout, stderr := runCreate(t, "create", "--nats", uri, "--bucket", c.bucket)
			if code != c.code {
				t.Fatalf("create = %d, want %d\nstdout: %s\nstderr: %s", code, c.code, stdout, stderr)
			}
			for _, says := range c.says {
				if !strings.Contains(stdout+stderr, says) {
					t.Errorf("create does not name %q\nstdout: %s\nstderr: %s", says, stdout, stderr)
				}
			}
			assertUnchanged(t, js, c.bucket, before)
		})
	}
}

// syncBuffer is a writer a run writes to on its own goroutine while the test reads it.
type syncBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// awaitText waits until b holds text, failing t after 30 s.
func awaitText(t *testing.T, b *syncBuffer, text string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(b.String(), text); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the run wrote no %q in 30 s:\n%s", text, b.String())
		}
	}
}

// gkeUsers is the authorization of a hub whose gke user holds the in-cluster gke listener's grant
// (core.ListenerHostPermissions) with every publish subject holding one of without taken out, and
// whose watcher, which a client presenting no credential becomes, may do anything. It fails t when
// without names a text no subject of the grant holds, so a grant the test means to narrow is
// narrowed.
func gkeUsers(t *testing.T, public, watcher string, without ...string) string {
	t.Helper()
	grant := core.ListenerHostPermissions("gke")
	for _, text := range without {
		before := len(grant.Publish.Allow)
		grant.Publish.Allow = slices.DeleteFunc(slices.Clone(grant.Publish.Allow), func(subject string) bool { return strings.Contains(subject, text) })
		if len(grant.Publish.Allow) == before {
			t.Fatalf("no subject of the gke grant holds %q", text)
		}
	}
	everything := core.Rules{Allow: []string{">"}}
	config, err := core.Authorization{Users: []core.User{
		{Name: "gke", NKey: public, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true},
		{Name: "watcher", NKey: watcher, Publish: &everything, Subscribe: &everything},
	}, NoAuthUser: "watcher"}.Render(nil)
	if err != nil {
		t.Fatalf("render the hub's users: %v", err)
	}
	return config
}

// shortenRefusalWait sets refusalWait and refusalRetry until t ends.
func shortenRefusalWait(t *testing.T, wait, retry time.Duration) {
	savedWait, savedRetry := refusalWait, refusalRetry
	refusalWait, refusalRetry = wait, retry
	t.Cleanup(func() { refusalWait, refusalRetry = savedWait, savedRetry })
}

// A create the hub refuses under its user's grant is sent again until the grant arrives, as a
// Job's run right after the apply that grants it is refused until the hub reloads its auth.conf.
// Here the gke user starts with the gke listener's grant less every subject of the known-session
// bucket, so its read of KV_envoy_known_sessions is refused; once the run says so, the hub reloads
// with the read granted and the create not, so its create is refused; once the run says that, the
// hub reloads with the whole grant, and the run creates the bucket and exits 0. The run is the
// Job's command, create --inbox-prefix _INBOX., so its replies come on the prefix the gke user
// receives. At b20f45e30a (#4296's head) the command had no --inbox-prefix, a usage error, and its
// first refused read ended a break-glass run, exit 1.
func TestACreateRefusedUnderItsGrantIsSentAgainUntilTheGrantArrives(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	shortenRefusalWait(t, time.Minute, 200*time.Millisecond)
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	stream := "KV_" + bus.KnownSessionBucket
	server, uri := testnats.StartConfig(t, gkeUsers(t, public, watcher, stream, "$KV."+bus.KnownSessionBucket+"."), []string{"-js"}, true)
	_, js := jetStream(t, uri)
	t.Setenv("NATS_NKEY_SEED", seed)

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		run, client := noProof(t)
		done <- command(context.Background(), []string{"create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--nats", uri}, stdout, stderr, run, client)
	}()
	awaitText(t, stderr, "read KV_"+bus.KnownSessionBucket+": refused under this user's grant")
	if err := server.Reload(t, gkeUsers(t, public, watcher, "$JS.API.STREAM.CREATE."+stream)); err != nil {
		t.Fatalf("reload with the read granted: %v", err)
	}
	awaitText(t, stderr, "create KV_"+bus.KnownSessionBucket+": refused under this user's grant")
	if err := server.Reload(t, gkeUsers(t, public, watcher)); err != nil {
		t.Fatalf("reload with the whole grant: %v", err)
	}
	select {
	case code := <-done:
		if want := "created " + bus.KnownSessionBucket + " (1 copies)\n"; code != 0 || stdout.String() != want {
			t.Fatalf("the run = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
		}
	case <-time.After(time.Minute):
		t.Fatalf("the run had not ended a minute after the whole grant\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr.String(), `Permissions Violation for Publish to "$JS.API.STREAM.INFO.`+stream+`"`) {
		t.Errorf("the first refusal's line does not name the server's refusal:\n%s", stderr)
	}
	if info := streamOf(t, js, bus.KnownSessionBucket); info.Config.MaxAge != bus.KnownSessionBucketMaxAge {
		t.Fatalf("%s after the run: %+v", stream, info.Config)
	}
}

// A refusal that lasts past refusalWait fails the run, exit 1, naming the refusal, and the run
// ends within that bound; nothing is created. The bound is a second here and 180 s in a release.
func TestACreateRefusedPastItsWaitFails(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	shortenRefusalWait(t, time.Second, 200*time.Millisecond)
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	stream := "KV_" + bus.KnownSessionBucket
	_, uri := testnats.StartConfig(t, gkeUsers(t, public, watcher, stream, "$KV."+bus.KnownSessionBucket+"."), []string{"-js"}, true)
	_, js := jetStream(t, uri)
	t.Setenv("NATS_NKEY_SEED", seed)
	started := time.Now()
	code, stdout, stderr := runCreate(t, "create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--nats", uri)
	took := time.Since(started)
	if code != 1 || !strings.Contains(stderr, "envoy-infra buckets: read "+bus.KnownSessionBucket+": ") || !strings.Contains(stderr, `Permissions Violation for Publish to "$JS.API.STREAM.INFO.`+stream+`"`) || stdout != "" {
		t.Fatalf("a create refused past its wait = %d, want 1 naming the refusal\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if took < 600*time.Millisecond || took > 10*time.Second {
		t.Fatalf("the refused run took %s, want about its 1 s wait", took)
	}
	if _, err := js.StreamInfo(stream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the refused run made %s: %v", stream, err)
	}
}

// The bound a refused request is sent again within is 180 s, sent again every 5 s: a hub's
// configuration reloader takes a new auth.conf within that.
func TestARefusedRequestIsSentAgainForUpTo180Seconds(t *testing.T) {
	if refusalWait != 180*time.Second || refusalRetry != 5*time.Second {
		t.Fatalf("refusalWait %s and refusalRetry %s, want 3m0s and 5s", refusalWait, refusalRetry)
	}
}

// A user granted exactly the in-cluster gke listener's permissions, which subscribes _INBOX.> and
// no other reply prefix, runs create with --inbox-prefix _INBOX. (or _INBOX, without its dot): it
// makes the absent known-session bucket, and a second run finds it present. The grant is not
// widened. The control is the same user with the default prefix, _INBOX_envoy_buckets, whose
// subscription that grant refuses, so its read draws no answer and the run fails with nothing
// made. --inbox-prefix beside --break-glass, which takes _INBOX. already, and a prefix that is no
// subject's are usage errors. At b20f45e30a the command took no --inbox-prefix, a usage error.
func TestAListenersUserRunsCreateOnTheInboxPrefixItNames(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, seed := listenerUserServer(t)
	_, js := jetStream(t, uri)
	t.Setenv("NATS_NKEY_SEED", seed)
	stream := "KV_" + bus.KnownSessionBucket

	code, stdout, stderr := runCreate(t, "create", "--bucket", bus.KnownSessionBucket, "--nats", uri)
	if code != 1 || !strings.Contains(stderr, "read "+bus.KnownSessionBucket+": ") || stdout != "" {
		t.Fatalf("control: create as gke on the default prefix = %d, want 1 failing its read\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := js.StreamInfo(stream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("control: the failed run made %s: %v", stream, err)
	}
	t.Logf("control: create as gke on the default prefix = %d: %s", code, strings.TrimSpace(stderr))

	code, stdout, stderr = runCreate(t, "create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--nats", uri)
	if want := "created " + bus.KnownSessionBucket + " (1 copies)\n"; code != 0 || stdout != want {
		t.Fatalf("create as gke with --inbox-prefix _INBOX. = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
	code, stdout, stderr = runCreate(t, "create", "--inbox-prefix", "_INBOX", "--bucket", bus.KnownSessionBucket, "--nats", uri)
	if want := bus.KnownSessionBucket + " present at 1 copies with its own configuration: left as it is\n"; code != 0 || stdout != want {
		t.Fatalf("create again as gke with --inbox-prefix _INBOX = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}

	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--break-glass", "--operator", "job", "--inbox-prefix", "_INBOX."}, "--break-glass takes its replies on _INBOX. already"},
		{[]string{"--inbox-prefix", ""}, "--inbox-prefix is no inbox prefix"},
		{[]string{"--inbox-prefix", "_INBOX.*"}, "--inbox-prefix is no inbox prefix"},
		{[]string{"--inbox-prefix", "_INBOX.>"}, "--inbox-prefix is no inbox prefix"},
		{[]string{"--inbox-prefix", "_INBOX..x"}, "--inbox-prefix is no inbox prefix"},
		{[]string{"--inbox-prefix", "_INBOX x"}, "--inbox-prefix is no inbox prefix"},
	} {
		code, stdout, stderr := runCreate(t, append([]string{"create", "--bucket", bus.SessionBucket, "--nats", uri}, c.args...)...)
		if code != 2 || !strings.Contains(stderr, c.says) || stdout != "" {
			t.Errorf("create %q = %d, want the usage error %q\nstdout: %s\nstderr: %s", c.args, code, c.says, stdout, stderr)
		}
	}
}

// --inbox-prefix _INBOX., the Job's argument, reaches nats.go as its own default inbox, with no
// custom prefix set: nats.go@v1.55.0 refuses a custom prefix that ends in "." (nats.go:1757,
// "nats: invalid custom prefix"), which the control shows by setting that prefix through the bus,
// so inboxOption cuts the dot and sets none for _INBOX. Each prefix is applied to a real connection
// and the inbox it makes is read: _INBOX. and _INBOX make _INBOX.<id>, the subjects a listener's
// user subscribes (_INBOX.>), and the default makes _INBOX_envoy_buckets.<id>.
func TestTheInboxPrefixReachesNatsGo(t *testing.T) {
	uri, _ := server(t)
	if _, err := bus.Connect([]string{uri}, bus.WithInboxPrefix("_INBOX.")); err == nil || !strings.Contains(err.Error(), "nats: invalid custom prefix") {
		t.Fatalf("control: bus.Connect with the custom prefix _INBOX. = %v, want nats.go's refusal \"nats: invalid custom prefix\"", err)
	}
	for _, c := range []struct {
		flag, inbox string
		options     int
	}{
		{"_INBOX.", natsgo.InboxPrefix, 0},
		{"_INBOX", natsgo.InboxPrefix, 0},
		{InboxPrefix, InboxPrefix + ".", 1},
	} {
		options, ok := inboxOption(c.flag)
		if !ok || len(options) != c.options {
			t.Fatalf("inboxOption(%q) = %d options, %t; want %d, true", c.flag, len(options), ok, c.options)
		}
		client, err := bus.Connect([]string{uri}, options...)
		if err != nil {
			t.Fatalf("connect with --inbox-prefix %s: %v", c.flag, err)
		}
		if inbox := client.Conn.NewInbox(); !strings.HasPrefix(inbox, c.inbox) || strings.Count(inbox, ".") != strings.Count(c.inbox, ".") {
			t.Errorf("--inbox-prefix %s makes the inbox %s, want %s<id>", c.flag, inbox, c.inbox)
		}
		client.Close()
	}
}

// The Job's exact command - create --inbox-prefix _INBOX. --bucket envoy_known_sessions --replicas 3
// - on a three-server hub, as a user that may publish only what the in-cluster gke listener may and
// subscribe only _INBOX.>: the first run creates the bucket at three copies, having had every
// reply, and the second says it is present and exits 0. The control is the same command without
// --inbox-prefix, whose replies under _INBOX_envoy_buckets that user may not receive.
func TestTheJobsCreateRunsAsAUserSubscribingOnlyInboxes(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	grant := core.ListenerHostPermissions("gke")
	inboxes := core.Rules{Allow: []string{natsgo.InboxPrefix + ">"}}
	everything := core.Rules{Allow: []string{">"}}
	config, err := core.Authorization{Users: []core.User{
		{Name: "gke", NKey: public, Publish: &grant.Publish, Subscribe: &inboxes, HasSeed: true},
		{Name: "watcher", NKey: watcher, Publish: &everything, Subscribe: &everything},
	}, NoAuthUser: "watcher"}.Render(nil)
	if err != nil {
		t.Fatalf("render the hub's users: %v", err)
	}
	cluster := testnats.StartClusterConfig(t, 3, config)
	uri := cluster.Servers[0].URL()
	_, js := jetStream(t, uri)
	t.Setenv("NATS_NKEY_SEED", seed)
	job := []string{"create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--replicas", "3", "--nats", uri}

	if code, stdout, stderr := runCreate(t, slices.Delete(slices.Clone(job), 1, 3)...); code != 1 || stdout != "" {
		t.Fatalf("control: the command without --inbox-prefix = %d, want 1 with nothing made\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	code, stdout, stderr := runCreate(t, job...)
	if want := "created " + bus.KnownSessionBucket + " (3 copies)\n"; code != 0 || stdout != want {
		t.Fatalf("the Job's command = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
	if info := streamOf(t, js, bus.KnownSessionBucket); info.Config.Replicas != 3 || info.Config.MaxAge != bus.KnownSessionBucketMaxAge {
		t.Fatalf("KV_%s after the Job's command: %+v; want three copies and max age %s", bus.KnownSessionBucket, info.Config, bus.KnownSessionBucketMaxAge)
	}
	code, stdout, stderr = runCreate(t, job...)
	if want := bus.KnownSessionBucket + " present at 3 copies with its own configuration: left as it is\n"; code != 0 || stdout != want {
		t.Fatalf("the Job's command again = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
}

// An interrupt in a create's refusal wait ends the run at once: exit 1 naming the interrupt, the
// cause main's signal context carries, and no STREAM.CREATE reaches the hub, though the hub grants
// the create right after the interrupt, before the wait would send it again. The gke user's grant
// holds every subject of the known-session bucket but its create, so the run's read finds the
// bucket absent and its create is refused; once the run says so, the run's context is cancelled
// and the hub reloads with the whole grant. The wait is the release's, every 5 s within 180 s. At
// 6e52228ad6 the run took no context: it slept the wait out, sent the create again, which the hub
// then granted, and exited 0 having made the bucket.
func TestACreateInterruptedInItsRefusalWaitCreatesNothing(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	stream := "KV_" + bus.KnownSessionBucket
	server, uri := testnats.StartConfig(t, gkeUsers(t, public, watcher, "$JS.API.STREAM.CREATE."+stream), []string{"-js"}, true)
	conn, js := jetStream(t, uri)
	requests := watchStreamRequests(t, conn)
	t.Setenv("NATS_NKEY_SEED", seed)

	ctx, interrupt := context.WithCancelCause(context.Background())
	defer interrupt(nil)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		run, client := noProof(t)
		done <- command(ctx, []string{"create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--nats", uri}, stdout, stderr, run, client)
	}()
	awaitText(t, stderr, "create "+stream+": refused under this user's grant")
	interrupted := time.Now()
	interrupt(errors.New("interrupt signal received"))
	if err := server.Reload(t, gkeUsers(t, public, watcher)); err != nil {
		t.Fatalf("reload with the whole grant: %v", err)
	}
	select {
	case code := <-done:
		took := time.Since(interrupted)
		want := "envoy-infra buckets create: stopped: interrupt signal received; it starts no create after this"
		if code != 1 || !strings.Contains(stderr.String(), want) || stdout.String() != "" || took > 2*time.Second {
			t.Fatalf("the interrupted run = %d after %s, want 1 within 2 s naming %q\nstdout: %s\nstderr: %s", code, took.Round(time.Millisecond), want, stdout, stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the run had not ended 30 s after the interrupt\nstderr: %s", stderr)
	}
	if got := requests.take(t); slices.Contains(got, "CREATE."+stream) {
		t.Fatalf("the interrupted run sent %v, want no create", got)
	}
	if _, err := js.StreamInfo(stream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the interrupted run made %s: %v", stream, err)
	}
}

// heldJetStream is the JetStream context a create is given, which holds the first request of one
// kind the create sends through it until the test lets it go (let), and then sends it on: holds
// names the account's JetStream info ("INFO", AccountInfo, the first request of a bucket's create,
// which bus.CreateMissingCopiedBucket sends before its STREAM.CREATE) or the stream's create
// ("STREAM.CREATE", AddStream). reached is closed when the held request arrives; every other
// request goes to the server at once. creates counts the STREAM.CREATEs sent through it, held or
// not.
type heldJetStream struct {
	natsgo.JetStreamContext
	holds            string
	reached, release chan struct{}
	held, released   sync.Once
	creates          atomic.Int32
}

func holdFirst(js natsgo.JetStreamContext, holds string) *heldJetStream {
	return &heldJetStream{JetStreamContext: js, holds: holds, reached: make(chan struct{}), release: make(chan struct{})}
}

// hold waits for the release when kind is the first request of the kind h holds.
func (h *heldJetStream) hold(kind string) {
	first := false
	if kind == h.holds {
		h.held.Do(func() { first = true })
	}
	if first {
		close(h.reached)
		<-h.release
	}
}

// let lets the held request go, once however often it is called.
func (h *heldJetStream) let() { h.released.Do(func() { close(h.release) }) }

func (h *heldJetStream) AccountInfo(opts ...natsgo.JSOpt) (*natsgo.AccountInfo, error) {
	h.hold("INFO")
	return h.JetStreamContext.AccountInfo(opts...)
}

func (h *heldJetStream) AddStream(cfg *natsgo.StreamConfig, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	h.creates.Add(1)
	h.hold("STREAM.CREATE")
	return h.JetStreamContext.AddStream(cfg, opts...)
}

// heldCreate is a create run (createBuckets) of the known-session bucket on a server of the test's
// own, through a connection made as the run makes its own and a JetStream context that holds the
// first request of one kind (heldJetStream): uri is the server's, js the test's own context on it,
// interrupt ends the run's context with a cause, and done answers the run's exit code.
type heldCreate struct {
	uri            string
	js             natsgo.JetStreamContext
	held           *heldJetStream
	interrupt      context.CancelCauseFunc
	stdout, stderr *syncBuffer
	done           chan int
}

// startHeldCreate starts a heldCreate whose context holds the first request of the kind holds, and
// returns once that request has arrived. The hold is let go when the test ends, whatever it did.
func startHeldCreate(t *testing.T, holds string) *heldCreate {
	t.Helper()
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, js := server(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect as the run does: %v", err)
	}
	t.Cleanup(client.Close)
	ctx, interrupt := context.WithCancelCause(context.Background())
	run := &heldCreate{uri: uri, js: js, held: holdFirst(client.JS(), holds), interrupt: interrupt, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1)}
	t.Cleanup(func() {
		interrupt(nil)
		run.held.let()
	})
	go func() {
		run.done <- createBuckets(ctx, run.stdout, run.stderr, client.Conn, run.held, []string{bus.KnownSessionBucket}, 1, func(string) {})
	}()
	select {
	case <-run.held.reached:
	case <-time.After(30 * time.Second):
		t.Fatalf("the create sent no %s in 30 s\nstdout: %s\nstderr: %s", holds, run.stdout, run.stderr)
	}
	return run
}

// createInBus is the function a bucket's create runs in, as a goroutine's trace names it: bus's
// addBucketStream, on the goroutine bus.Bounded starts, which runs on to its own end once the run
// stops waiting for it.
const createInBus = "github.com/sjawhar/envoy/internal/bus.addBucketStream("

// createGoroutines is the id of each goroutine running a bucket's create (createInBus).
func createGoroutines() []uint64 { return goroutinetest.Running(createInBus) }

// awaitCreatesEnded waits until each goroutine of ids has returned (goroutinetest.Live), so what
// a create the run stopped waiting for sends has been sent, failing t after 30 s.
func awaitCreatesEnded(t *testing.T, ids []uint64) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); slices.ContainsFunc(ids, goroutinetest.Live); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("a bucket's create (goroutines %v) still ran 30 s after its held request was let go", ids)
		}
	}
}

// An interrupt while a create's request is in flight ends the run at once, and the run's last line
// says what the hub may still do with that request (stopped). The JetStream context the run is
// given holds one request of the create until the test lets it go, after the interrupt
// (startHeldCreate). The test then waits for the create the run stopped waiting for to end on its
// own goroutine (awaitCreatesEnded) before it reads what that create sent, and runs create again:
//
//   - held at the account's JetStream info, the create's first request, no STREAM.CREATE follows:
//     the create reads its context before it (bus's addBucketStream), so the bucket stays absent
//     and create run again makes it. With that read replaced by a 3 s sleep, the STREAM.CREATE goes
//     out once the sleep ends and makes the bucket, after the run stopped; at 69cde74210 this row
//     waited a fixed 2 s for it instead, and passed that change. With the run's context not passed
//     to the create (bus.CreateMissingCopiedBucket(context.Background(), ...)), the run waits the
//     held request out and has not ended 5 s after the interrupt.
//   - held at the STREAM.CREATE, the create already sent lands after the run has stopped, as the
//     line says ("one it had sent may still land"), and create run again finds the bucket present.
//
// Before the interrupt each row takes the goroutines that run the create (createGoroutines), and
// fails when there are none, so the wait for them to end is not a wait for nothing. No test of the
// package runs in parallel with another, so a create running then is the test's own. The control
// is TestACreateWhoseAccountInfoIsHeldAndLetGoGoesOn.
func TestACreateInterruptedWithItsRequestInFlightSaysWhatMayLand(t *testing.T) {
	const stoppedLine = "envoy-infra buckets create: stopped: interrupt signal received; it starts no create after this, but one it had sent may still land: run create again to see what the hub holds\n"
	for _, c := range []struct {
		name, holds string
		creates     int32
		lands       bool
		again       string
	}{
		{"interrupted while its account info is held", "INFO", 0, false, "created " + bus.KnownSessionBucket + " (1 copies)\n"},
		{"interrupted while its STREAM.CREATE is held", "STREAM.CREATE", 1, true, bus.KnownSessionBucket + " present at 1 copies with its own configuration: left as it is\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := startHeldCreate(t, c.holds)
			creates := createGoroutines()
			if len(creates) == 0 {
				t.Fatalf("no goroutine runs %s while the create's %s is held, so the wait for the create to end would wait for nothing", createInBus, c.holds)
			}
			run.interrupt(errors.New("interrupt signal received"))
			var code int
			select {
			case code = <-run.done:
			case <-time.After(5 * time.Second):
				t.Fatalf("the run had not ended 5 s after the interrupt, with its %s held: it waited the request out", c.holds)
			}
			run.held.let()
			awaitCreatesEnded(t, creates)
			if code != 1 || run.stdout.String() != "" || run.stderr.String() != stoppedLine {
				t.Fatalf("the run = %d, want 1\nstdout: %q\nstderr: %q\nwant:   %q", code, run.stdout, run.stderr, stoppedLine)
			}
			if creates := run.held.creates.Load(); creates != c.creates {
				t.Fatalf("the create sent %d STREAM.CREATE once it had ended, want %d", creates, c.creates)
			}
			if _, err := run.js.StreamInfo("KV_" + bus.KnownSessionBucket); (err == nil) != c.lands {
				t.Fatalf("KV_%s once the stopped run's create had ended: %v, want it there: %t", bus.KnownSessionBucket, err, c.lands)
			}
			if code, out, errOut := runCreate(t, "create", "--nats", run.uri, "--bucket", bus.KnownSessionBucket); code != 0 || out != c.again {
				t.Fatalf("create run again after the stopped run = %d, want 0 and %q\nstdout: %s\nstderr: %s", code, c.again, out, errOut)
			}
		})
	}
}

// A create whose account info is held and let go while the run is live goes on and makes the
// bucket, and the wait for the create to end sees its STREAM.CREATE: the hold is on the create's
// path, so the first row of TestACreateInterruptedWithItsRequestInFlightSaysWhatMayLand holds the
// request the create sends before its STREAM.CREATE, and a STREAM.CREATE sent after that request
// is answered is counted there.
func TestACreateWhoseAccountInfoIsHeldAndLetGoGoesOn(t *testing.T) {
	run := startHeldCreate(t, "INFO")
	creates := createGoroutines()
	run.held.let()
	var code int
	select {
	case code = <-run.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the run had not ended 30 s after its held account info was let go\nstdout: %s\nstderr: %s", run.stdout, run.stderr)
	}
	awaitCreatesEnded(t, creates)
	if want := "created " + bus.KnownSessionBucket + " (1 copies)\n"; code != 0 || run.stdout.String() != want || run.stderr.String() != "" {
		t.Fatalf("the run = %d, want 0 and %q\nstdout: %q\nstderr: %q", code, want, run.stdout, run.stderr)
	}
	if creates := run.held.creates.Load(); creates != 1 {
		t.Fatalf("the create sent %d STREAM.CREATE, want 1", creates)
	}
}

// A connection that reports no server version refuses the run before any read, with a line of its
// own, and nothing is read or created. ConnectedServerVersion answers "" while a connection is not
// connected, and the run reads it once, before its reads. Here the version is read off a
// connection that has been closed and the reads go through one to the same server that is
// connected: the state of a run whose connection came back between the version's read and the
// first stream read. At 65a488fe53 the present bucket was judged against a stream made for a
// server too old for a bucket, and refused naming its discard policy as a difference
// ("differs: KV_envoy_known_sessions: discard policy DiscardNew, want DiscardOld"), and
// the absent bucket's create failed as a server's before 2.6.2 ("key-value requires at least server
// version 2.6.2"). The control reads the version off the connected connection: the present bucket
// is left as it is, exit 0.
func TestACreateOnAConnectionReportingNoServerVersionIsRefusedForThat(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	const refusal = "envoy-infra buckets create: refused: the connection reports no server version, as one that is not connected does, and each bucket is judged against the stream its create makes on the hub's version; nothing is read or created: run create again\n"
	for _, c := range []struct {
		name            string
		present, closed bool
		code            int
		stdout, stderr  string
	}{
		{"a present bucket", true, true, 1, "", refusal},
		{"an absent bucket", false, true, 1, "", refusal},
		{"control: a present bucket, the version read off the connected connection", true, false, 0, bus.KnownSessionBucket + " present at 1 copies with its own configuration: left as it is\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			uri, js := server(t)
			if c.present {
				makeBuckets(t, js, bus.KnownSessionBucket)
			}
			conn, reads := jetStream(t, uri)
			versions := conn
			if c.closed {
				versions = testnats.Connect(t, uri)
				versions.Close()
				if version := versions.ConnectedServerVersion(); version != "" {
					t.Fatalf("a closed connection reports server version %q, want none: this row holds nothing", version)
				}
			}
			requests := watchStreamRequests(t, conn)
			var stdout, stderr strings.Builder
			code := createBuckets(t.Context(), &stdout, &stderr, versions, reads, []string{bus.KnownSessionBucket}, 1, func(string) {})
			if code != c.code || stdout.String() != c.stdout || stderr.String() != c.stderr {
				t.Fatalf("the run = %d, want %d\nstdout: %q\nwant:   %q\nstderr: %q\nwant:   %q", code, c.code, stdout.String(), c.stdout, stderr.String(), c.stderr)
			}
			if got := requests.take(t); c.closed && len(got) != 0 {
				t.Fatalf("the refused run sent %v to the bucket's stream, want nothing", got)
			}
			if _, err := js.StreamInfo("KV_" + bus.KnownSessionBucket); !c.present && !errors.Is(err, natsgo.ErrStreamNotFound) {
				t.Fatalf("the refused run made KV_%s: %v", bus.KnownSessionBucket, err)
			}
		})
	}
}

// heldDial is a connection's dialer whose dials fail while it is held, so a reconnect that has
// failed one dial waits its ReconnectWait before the next, until a ForceReconnect wakes it. A dial
// that blocked instead would hold the connection's lock, which ConnectedServerVersion takes too.
type heldDial struct{ held atomic.Bool }

func (d *heldDial) Dial(network, address string) (net.Conn, error) {
	if d.held.Load() {
		return nil, errors.New("this dial is held by the test")
	}
	return net.Dial(network, address)
}

// reconnectingJetStream is the JetStream context a create is given on conn, a connection dialled
// through dial. Once the request at names is answered - the create's lookup of the bucket
// ("lookup", KeyValue) or the account's JetStream info ("account info", AccountInfo) - it holds
// dial and drops the connection (ForceReconnect), so the connection is reconnecting, and reports no
// server version, until the create's STREAM.CREATE, which lets dial go and wakes the reconnect
// first. With no dial (the control) nothing is dropped. version is what the connection reported
// when that request returned, and creates counts the STREAM.CREATEs sent through it.
type reconnectingJetStream struct {
	natsgo.JetStreamContext
	conn    *natsgo.Conn
	dial    *heldDial
	at      string
	version atomic.Pointer[string]
	creates atomic.Int32
}

// answered drops the connection when request is the one r drops it after, and notes the version
// the connection then reports.
func (r *reconnectingJetStream) answered(request string) error {
	if request != r.at {
		return nil
	}
	if r.dial != nil {
		r.dial.held.Store(true)
		if dropped := r.conn.ForceReconnect(); dropped != nil {
			return dropped
		}
	}
	version := r.conn.ConnectedServerVersion()
	r.version.Store(&version)
	return nil
}

func (r *reconnectingJetStream) KeyValue(bucket string) (natsgo.KeyValue, error) {
	kv, err := r.JetStreamContext.KeyValue(bucket)
	if dropped := r.answered("lookup"); dropped != nil {
		return nil, dropped
	}
	return kv, err
}

func (r *reconnectingJetStream) AccountInfo(opts ...natsgo.JSOpt) (*natsgo.AccountInfo, error) {
	info, err := r.JetStreamContext.AccountInfo(opts...)
	if dropped := r.answered("account info"); dropped != nil {
		return nil, dropped
	}
	return info, err
}

func (r *reconnectingJetStream) AddStream(cfg *natsgo.StreamConfig, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	r.creates.Add(1)
	if r.at == "stream create" {
		// The create lands and its answer is lost: the stream is made, the connection is dropped
		// and held, and the send answers as one the server left unanswered, which the bus sends
		// again (sentAgain). The send after it meets a connection that is reconnecting.
		info, err := r.JetStreamContext.AddStream(cfg, opts...)
		if err != nil {
			return info, err
		}
		if dropped := r.answered("stream create"); dropped != nil {
			return nil, dropped
		}
		return nil, natsgo.ErrTimeout
	}
	if r.dial != nil {
		r.dial.held.Store(false)
		_ = r.conn.ForceReconnect()
	}
	return r.JetStreamContext.AddStream(cfg, opts...)
}

// reconnectingRun runs a create (createBuckets) of the known-session bucket at replicas copies on a
// server of the test's own, through a connection that drops once the request at is answered and
// reconnects only at the create's STREAM.CREATE, or never drops when drops is false
// (reconnectingJetStream). It returns the run's exit code and output, the context, and the test's
// own JetStream context on the server.
func reconnectingRun(t *testing.T, at string, drops bool, replicas int) (code int, stdout, stderr string, reconnecting *reconnectingJetStream, js natsgo.JetStreamContext) {
	t.Helper()
	uri, js := server(t)
	dial := &heldDial{}
	conn, err := natsgo.Connect(uri, natsgo.SetCustomDialer(dial), natsgo.ReconnectWait(time.Hour), natsgo.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	t.Cleanup(func() { dial.held.Store(false) })
	connJS, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	reconnecting = &reconnectingJetStream{JetStreamContext: connJS, conn: conn, at: at}
	if drops {
		reconnecting.dial = dial
	}
	var out, errOut strings.Builder
	code = createBuckets(t.Context(), &out, &errOut, conn, reconnecting, []string{bus.KnownSessionBucket}, replicas, func(string) {})
	if version := reconnecting.version.Load(); version == nil || (*version == "") != drops {
		t.Fatalf("the connection reported server version %v when its %s returned, want none: %t: this row holds nothing", version, at, drops)
	}
	return code, out.String(), errOut.String(), reconnecting, js
}

// A create whose connection drops after its account info, and reconnects only later, sends no
// STREAM.CREATE and is refused for the server version it cannot read. The create reads the
// server's version a second time after the account info (bus's addBucketStream), as nats.go does,
// for the stream's discard policy; a connection that is reconnecting reports none, which nats.go
// reads as a server too old for a discard policy of new. The run's own read, before its first read
// of a stream, found the version; the connection is dropped once the account info is answered and
// its reconnect held until a STREAM.CREATE (reconnectingJetStream). At 69cde74210 that create sent a
// STREAM.CREATE of discard old once the connection was back, the run exited 0 ("created
// envoy_known_sessions (1 copies)"), and the server held a bucket every present-bucket check of
// create then refuses for its discard policy. The control is the same run with no drop, which
// makes the bucket with a discard policy of new.
func TestACreateWhoseConnectionReconnectsAfterItsAccountInfoSendsNoCreate(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	const refusal = "envoy-infra buckets create: refused: the connection reports no server version, as one that is not connected does, and each bucket is judged against the stream its create makes on the hub's version; KV_" + bus.KnownSessionBucket + " is not created: run create again\n"
	for _, c := range []struct {
		name           string
		drops          bool
		code           int
		stdout, stderr string
		creates        int32
	}{
		{"dropped after its account info, its reconnect held", true, 1, "", refusal, 0},
		{"control: not dropped", false, 0, "created " + bus.KnownSessionBucket + " (1 copies)\n", "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr, reconnecting, js := reconnectingRun(t, "account info", c.drops, 1)
			if code != c.code || stdout != c.stdout || stderr != c.stderr {
				t.Fatalf("the run = %d, want %d\nstdout: %q\nwant:   %q\nstderr: %q\nwant:   %q", code, c.code, stdout, c.stdout, stderr, c.stderr)
			}
			if creates := reconnecting.creates.Load(); creates != c.creates {
				t.Fatalf("the create sent %d STREAM.CREATE, want %d", creates, c.creates)
			}
			info, err := js.StreamInfo("KV_" + bus.KnownSessionBucket)
			switch {
			case c.drops && !errors.Is(err, natsgo.ErrStreamNotFound):
				t.Fatalf("the refused run made KV_%s: %v", bus.KnownSessionBucket, err)
			case !c.drops && (err != nil || info.Config.Discard != natsgo.DiscardNew):
				t.Fatalf("control: KV_%s = %v, %v, want it made with a discard policy of new", bus.KnownSessionBucket, info, err)
			}
		})
	}
}

// A create whose connection drops after its lookup of the bucket, before the create's first read
// of the server version, is refused for that version with create's own line, as one that drops
// after the account info is. That first read (bus's addBucketStream) answers nats.go's own error for
// no version, "key-value requires at least server version 2.6.2", which the bus names with the
// copies asked for, so at 6928405d47 the run printed "create envoy_known_sessions: ...:
// ENVOY_NATS_REPLICAS=3: nats: key-value requires at least server version 2.6.2", which tells the
// operator neither what happened nor to run create again. The run is at 3 copies, as that report
// was. The control of the harness is TestACreateWhoseConnectionReconnectsAfterItsAccountInfoSendsNoCreate's.
func TestACreateWhoseConnectionReconnectsAfterItsLookupIsRefusedForNoServerVersion(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	const refusal = "envoy-infra buckets create: refused: the connection reports no server version, as one that is not connected does, and each bucket is judged against the stream its create makes on the hub's version; KV_" + bus.KnownSessionBucket + " is not created: run create again\n"
	code, stdout, stderr, reconnecting, js := reconnectingRun(t, "lookup", true, 3)
	if code != 1 || stdout != "" || stderr != refusal {
		t.Fatalf("the run = %d, want 1\nstdout: %q\nstderr: %q\nwant:   %q", code, stdout, stderr, refusal)
	}
	if creates := reconnecting.creates.Load(); creates != 0 {
		t.Fatalf("the create sent %d STREAM.CREATE, want none", creates)
	}
	if _, err := js.StreamInfo("KV_" + bus.KnownSessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the refused run made KV_%s: %v", bus.KnownSessionBucket, err)
	}
}

// A create whose STREAM.CREATE lands and whose answer is lost says the bucket may be there, not
// that it is not created. A create the server leaves unanswered is sent again (bus's sentAgain),
// and that send ends at its first read of the server version, having sent nothing, when the
// connection is reconnecting: the first send still made the stream. The bus marks the failure of
// a create that sent a STREAM.CREATE (bus.ErrStreamCreateSent), and the run's line says so. Here
// the fake answers the first STREAM.CREATE as unanswered after it has made the stream, and drops
// the connection with it. At 570d918b3b the run printed "KV_envoy_known_sessions is not created"
// while the hub held the bucket.
func TestACreateWhoseSentStreamCreateLosesItsAnswerSaysTheBucketMayBeThere(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	const refusal = "envoy-infra buckets create: refused: the connection reports no server version, as one that is not connected does, and each bucket is judged against the stream its create makes on the hub's version; KV_" + bus.KnownSessionBucket + " may have been created: a create of it was sent and its answer was never read: run create again\n"
	code, stdout, stderr, reconnecting, js := reconnectingRun(t, "stream create", true, 1)
	if code != 1 || stdout != "" || stderr != refusal {
		t.Fatalf("the run = %d, want 1\nstdout: %q\nstderr: %q\nwant:   %q", code, stdout, stderr, refusal)
	}
	if creates := reconnecting.creates.Load(); creates != 1 {
		t.Fatalf("the create sent %d STREAM.CREATE, want the one whose answer was lost", creates)
	}
	if _, err := js.StreamInfo("KV_" + bus.KnownSessionBucket); err != nil {
		t.Fatalf("the hub holds no KV_%s, so the run's line is right and this test proves nothing: %v", bus.KnownSessionBucket, err)
	}
}

// A create whose STREAM.CREATE the server answers with a refusal says what the server answered,
// and not that the bucket may have been created: an answered refusal made no stream. The hub's
// account here holds at most one stream (an account limit), and holds one already, so the server
// answers the bucket's create that the account has reached its maximum number of streams. The run
// fails naming that answer, and the hub holds no bucket. At 19efccd870 the bus marked every failed
// create that had sent a STREAM.CREATE with bus.ErrStreamCreateSent, a refused one included, so
// the run's line said the create "may have landed" of a bucket the hub does not hold.
func TestACreateWhoseStreamCreateIsRefusedSaysNothingMayHaveLanded(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	const config = "accounts {\n  LIMITED {\n    jetstream { max_streams: 1 }\n    users = [ { user: limited, password: limited } ]\n  }\n}\nno_auth_user: limited\n"
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, true)
	conn, js := jetStream(t, uri)
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "FILLER", Subjects: []string{"filler.>"}}); err != nil {
		t.Fatalf("make the account's one stream: %v", err)
	}
	var out, errOut strings.Builder
	code := createBuckets(t.Context(), &out, &errOut, conn, js, []string{bus.KnownSessionBucket}, 1, func(string) {})
	stdout, stderr := out.String(), errOut.String()
	if code != 1 || stdout != "" || !strings.Contains(stderr, "maximum number of streams reached") {
		t.Fatalf("the run = %d, want 1 naming the server's refusal\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "may have landed") || strings.Contains(stderr, "may have been created") {
		t.Fatalf("the run says a create the server refused may have landed:\nstderr: %q", stderr)
	}
	if _, err := js.StreamInfo("KV_" + bus.KnownSessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the hub holds KV_%s after the refused create (%v), so the refusal this test reads never happened", bus.KnownSessionBucket, err)
	}
}

// A request refused through a whole wait is reported once: by the run's one line naming the
// refusal on standard error, and by no line of the connection's, where the bus logs each refused
// message at ERROR ("envoy nats async error", options' async error callback). The read of the
// known-session bucket is refused for the whole of a 2 s wait, sent again every 100 ms, so about 20
// times, and the run fails naming the refusal. The control is a client of the same user without
// bus.WithPublishRefusalsUnlogged, whose one refused read the recorder holds at ERROR, so the count
// below sees such a line where one is written. At 6e52228ad6 the run left one such line for each
// refused send; at the release's 180 s wait, every 5 s, that is a line for each of 36 sends.
func TestARefusalWaitLogsTheRefusalOnce(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	shortenRefusalWait(t, 2*time.Second, 100*time.Millisecond)
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	stream := "KV_" + bus.KnownSessionBucket
	_, uri := testnats.StartConfig(t, gkeUsers(t, public, watcher, stream, "$KV."+bus.KnownSessionBucket+"."), []string{"-js"}, true)
	t.Setenv("NATS_NKEY_SEED", seed)
	logs := recordResends(t)
	refusal := `Permissions Violation for Publish to "$JS.API.STREAM.INFO.` + stream + `"`
	logged := func() int {
		count := 0
		for _, record := range logs.Records() {
			record.Attrs(func(attr slog.Attr) bool {
				if record.Level >= slog.LevelError && attr.Key == "error" && strings.Contains(attr.Value.String(), refusal) {
					count++
				}
				return true
			})
		}
		return count
	}

	code, stdout, stderr := runCreate(t, "create", "--inbox-prefix", "_INBOX.", "--bucket", bus.KnownSessionBucket, "--nats", uri)
	if code != 1 || !strings.Contains(stderr, "envoy-infra buckets: read "+bus.KnownSessionBucket+": ") || !strings.Contains(stderr, refusal) || stdout != "" {
		t.Fatalf("a create refused through its wait = %d, want 1 naming the refusal\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if lines := strings.Count(stderr, "refused under this user's grant"); lines != 1 {
		t.Errorf("the run wrote %d lines saying its read is refused, want 1\n%s", lines, stderr)
	}
	if lines := logged(); lines != 0 {
		t.Fatalf("the run's connection logged %d refusals of its read at ERROR over the wait, want none", lines)
	}

	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("control: connect as the gke user: %v", err)
	}
	defer client.Close()
	if _, err := client.JS().StreamInfo(stream); !errors.Is(err, bus.ErrPublishDenied) {
		t.Fatalf("control: the read without the option = %v, want %v", err, bus.ErrPublishDenied)
	}
	for deadline := time.Now().Add(5 * time.Second); logged() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("control: a client without bus.WithPublishRefusalsUnlogged logged no refusal of its read at ERROR in 5 s")
		}
	}
	t.Logf("control: a client without the option logged %d refusal of its one read at ERROR", logged())
}
