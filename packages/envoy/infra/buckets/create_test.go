package buckets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
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

// recreate keeps the writer proof: with a writer running on the named host it is refused naming
// it, its proof having run (one host query), and the bucket is left as it was; with none it
// deletes and creates the bucket after the proof, taken twice. The same writer leaves create
// untouched (TestACreateOfAnAbsentBucketTakesNoWriterProof), so the proof is recreate's.
func TestRecreateStillTakesTheWriterProof(t *testing.T) {
	uri, js := server(t)
	makeBuckets(t, js, bus.KnownSessionBucket)
	before := streamOf(t, js, bus.KnownSessionBucket)
	writer := "4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n"
	w := &world{hostAnswers: map[string][]string{"box-1": {writer}}}
	code, stdout, stderr := runCommand(t, w, nil, "recreate", "--nats", uri, "--bucket", bus.KnownSessionBucket, "--host", "box-1")
	if code != 1 || !strings.Contains(stderr, "blocked: host box-1: process 4242 envoy-listener runs") || len(w.calls) != 1 {
		t.Fatalf("recreate while a writer runs = %d, calls %q, want refused by its proof\n%s%s", code, w.calls, stdout, stderr)
	}
	assertUnchanged(t, js, bus.KnownSessionBucket, before)

	w = &world{hostAnswers: map[string][]string{"box-1": {clean(150), clean(150)}}}
	code, stdout, stderr = runCommand(t, w, nil, "recreate", "--nats", uri, "--bucket", bus.KnownSessionBucket, "--host", "box-1")
	if code != 0 || len(w.calls) != 2 || !streamOf(t, js, bus.KnownSessionBucket).Created.After(before.Created) {
		t.Fatalf("recreate with no writer = %d, calls %q, want the bucket made again after two proofs\n%s%s", code, w.calls, stdout, stderr)
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
		want := "envoy-infra buckets create: stopped: interrupt signal received; nothing more is created"
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
