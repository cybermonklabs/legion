package testnats

import (
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sjawhar/envoy/testnatsimages"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// A start that fails after Docker created the container — here a readiness wait that can never
// succeed, as a start timing out under load does — leaves no container behind once it returns:
// nothing else would remove a container whose start never returned one. The container is asked
// for by its label on the same Docker daemon testcontainers created it on. Only the image tests
// still run containers, so a machine with no container runtime skips this one as it skips them.
func TestAFailedStartLeavesNoContainer(t *testing.T) {
	RequireContainerRuntime(t)
	label := "legion-testnats-" + rand.Text()
	created, err := startContainer(testnatsimages.Image,
		testcontainers.WithLabels(map[string]string{"legion.test": label}),
		testcontainers.WithWaitStrategy(wait.ForLog("a line NATS never prints").WithStartupTimeout(3*time.Second)),
	)
	if err == nil {
		t.Fatal("a start whose readiness wait cannot succeed returned no error")
	}
	if created == nil {
		t.Fatalf("the failed start created no container (%v), so there was nothing to leak", err)
	}
	docker, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatalf("the Docker client testcontainers uses: %v", err)
	}
	defer docker.Close()
	left, err := docker.ContainerList(t.Context(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "legion.test="+label))})
	if err != nil {
		t.Fatalf("list containers labelled %s: %v", label, err)
	}
	for _, c := range left {
		t.Errorf("container %s of the failed start is still there (%s)", c.ID, c.State)
		_ = docker.ContainerRemove(t.Context(), c.ID, container.RemoveOptions{Force: true})
	}
}

// CreateStream makes the stream its configuration names, and fails the test at once on an answer
// it cannot make one from. Its subject used to be the retry of the server's "error creating store
// for stream": that answer came from the delete-every-stream reset URL ran on a server the tests
// shared, which a server of the test's own needs none of, so no answer is retried now.
func TestCreateStreamMakesTheStreamAndFailsOnARefusedOne(t *testing.T) {
	js := JetStream(t)
	stream := CreateStream(t, js, jetstream.StreamConfig{Name: "MADE", Subjects: []string{"made.>"}})
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.Name != "MADE" {
		t.Errorf("CreateStream made %s, want MADE", info.Config.Name)
	}

	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		CreateStream(refused, js, jetstream.StreamConfig{Name: "a name no stream may have", Subjects: []string{"made.>"}})
	}()
	<-done
	if want := "create stream a name no stream may have"; !strings.Contains(refused.message, want) {
		t.Errorf("a refused create failed with %q, want it to say %q", refused.message, want)
	}
}

// Each test gets a NATS server of its own: two tests reach two URLs, and the stream one of them
// creates is on neither the other's server nor its account. This is what replaces the reset the
// tests needed while they shared one server.
func TestEachTestGetsAServerOfItsOwn(t *testing.T) {
	var first string
	t.Run("creates a stream on its own server", func(t *testing.T) {
		js := JetStream(t)
		CreateStream(t, js, jetstream.StreamConfig{Name: "ITS_OWN", Subjects: []string{"its.own.>"}})
		first = js.Conn().ConnectedUrl()
	})
	t.Run("reaches another server, holding none of it", func(t *testing.T) {
		js := JetStream(t)
		if second := js.Conn().ConnectedUrl(); second == first {
			t.Errorf("both tests reached the NATS server at %s", second)
		}
		names := js.StreamNames(t.Context())
		for name := range names.Name() {
			t.Errorf("stream %s from the previous test is on this test's server", name)
		}
		if err := names.Err(); err != nil {
			t.Fatalf("list streams: %v", err)
		}
	})
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) { f.message = fmt.Sprint(args...); runtime.Goexit() }
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. The test's own server, which asks for no credential, stands in for one.
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	url := URL(t)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		answering(wait, url)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait is still waiting on a server that admits anyone")
	}
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}
