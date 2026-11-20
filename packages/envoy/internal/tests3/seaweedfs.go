// Package tests3 runs an S3-compatible server in a container for the tests that need a real one:
// the file store's own tests, which drive the AWS SDK's real client against it so what the store
// sends and how it reads the answers are exercised end to end, and the e2e harness when a run
// asks for one. SeaweedFS stands in for S3: MinIO's image left Docker Hub, and SeaweedFS answers
// the calls the store makes (HeadBucket, HeadObject, PutObject with a SHA-256 checksum, GetObject
// with checksum validation, and S3's own 404 and NoSuchKey shapes). A test that needs the fake's
// failure shapes uses files/filestest instead.
package tests3

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the SeaweedFS image every S3-compatible test container runs: ghcr.io/cybermonklabs/legion/ci,
// the copy of seaweedfs:3.97 (.github/ci-images.yaml). The envoy-go CI job reads this declaration
// and pulls the image before its first test step, so no test reaches the registry mid-run; keep it
// a single-line string constant.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:bb05d66d2963b1cc48073190781c3dd29e2c4c88a2ae2986bf58e38c86d89c6e"

// collection holds every object the server stores. one-collection.sh gives the server one rule,
// every path into this collection, and grows its volumes before anything is written, so no write
// grows a collection; that file says why a write must not.
const collection = "tests3"

// oneCollection is one-collection.sh, the one implementation of that rule, which the shard job of
// .github/workflows/dispatch-browser.yaml runs for the browser suite's server too.
//
//go:embed one-collection.sh
var oneCollection string

// scriptPath is where the container holds one-collection.sh, and serverLog where its server
// writes the log that script reads.
const (
	scriptPath = "/one-collection.sh"
	serverLog  = "/tmp/weed.log"
)

// scriptDeadline bounds one run of a step of one-collection.sh. The script bounds each wait of its
// own and fails by name when one runs out, and the head of that file says how. A prepare whose
// waits each ran to the end of their bounds would take at most 333 s: 71 s for the volume server
// (its 60 s, a second's pause and one request's 10 s), 21 s for the grow, three shells of 60 s and
// 61 s for the rule in force; a check, 70 s. On a store that stops answering, the first wait that
// meets it fails by name within its own bound, 71 s at most. prepare ended within seconds of the S3
// port answering in this script's runs on a dev box, and 11 to 12 s after it with the volume
// server held back 10 s. So a step still running at 6 minutes is one the script did not end, a
// stuck docker exec among them, and it fails the test by name 14 minutes before go test's
// -timeout in the envoy-go job (.github/workflows/ci-job-envoy-go.yaml) ends the whole package
// with no name.
const scriptDeadline = 6 * time.Minute

// requestDeadline bounds one request to the server. Its answers come within a second, and a
// write that waits on storage being made gets its 500 when the filer gives up after 10 s, so a
// request still open at a minute is to a server that stopped answering.
const requestDeadline = time.Minute

// Start runs an S3-compatible server holding the empty bucket, removed when t ends, points this
// process's AWS SDK at it for the rest of t (filestest.PointSDKAt), so files.NewS3(ctx, bucket)
// reaches it exactly as a deployment's reaches its bucket, and returns the endpoint for a caller
// that hands it to another process. t.Setenv makes it unusable in a parallel test.
func Start(t testing.TB, bucket string) string {
	t.Helper()
	ctx := context.Background()
	container, endpoint := startServer(ctx, t, oneCollection)
	if code, output := runScript(ctx, t, container, "prepare"); code != 0 {
		t.Fatalf("give the S3-compatible server its one collection: exit %d:\n%s", code, output)
	}
	// The server creates a bucket on an anonymous PUT of its name.
	if status, body := call(ctx, t, http.MethodPut, endpoint+"/"+bucket, ""); status != http.StatusOK {
		t.Fatalf("create bucket %s: %d %s", bucket, status, body)
	}
	// The first object, written, read back and deleted, which proves the server takes one.
	object := endpoint + "/" + bucket + "/tests3-readiness"
	if status, body := call(ctx, t, http.MethodPut, object, "ready"); status != http.StatusOK {
		t.Fatalf("write the first object: %d %s", status, body)
	}
	if status, body := call(ctx, t, http.MethodGet, object, ""); status != http.StatusOK || body != "ready" {
		t.Fatalf("read back the first object: %d %q, want 200 %q", status, body, "ready")
	}
	if status, body := call(ctx, t, http.MethodDelete, object, ""); status != http.StatusNoContent {
		t.Fatalf("delete the first object: %d %s", status, body)
	}
	if code, output := runScript(ctx, t, container, "check"); code != 0 {
		t.Fatalf("check the S3-compatible server after its first write: exit %d:\n%s", code, output)
	}
	// The same check when the test is done with the server, so a write of the test's own that
	// grew storage fails it by name. A cleanup registered after CleanupContainer's runs before it.
	t.Cleanup(func() {
		if code, output := runScript(ctx, t, container, "check"); code != 0 {
			t.Errorf("check the S3-compatible server after the test: exit %d:\n%s", code, output)
		}
	})
	filestest.PointSDKAt(t, endpoint)
	return endpoint
}

// startServer runs the server in a container removed when t ends, with script at scriptPath,
// and answers the container and its S3 endpoint once that port answers.
func startServer(ctx context.Context, t testing.TB, script string) (testcontainers.Container, string) {
	t.Helper()
	container, err := testcontainers.Run(ctx, Image,
		// The image's own entrypoint, which adds the server's volume flags, with the server's
		// output in serverLog, where one-collection.sh reads it, rather than the container's log.
		testcontainers.WithEntrypoint("sh", "-c", `exec /entrypoint.sh "$@" > `+serverLog+` 2>&1`, "sh"),
		testcontainers.WithCmd("server", "-s3", "-dir=/data", "-ip=0.0.0.0", "-s3.port=8333"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(script),
			ContainerFilePath: scriptPath,
			FileMode:          0o755,
		}),
		testcontainers.WithExposedPorts("8333/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("8333/tcp").WithStartupTimeout(90*time.Second)),
		// One CPU and 1 GiB, as every container the repository's tests start is capped on a shared
		// host: one process runs the master, volume, filer and S3 servers for a few small objects.
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			hostConfig.NanoCPUs = 1_000_000_000
			hostConfig.Memory = 1 << 30
		}),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start the S3-compatible server: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "8333/tcp", "http")
	if err != nil {
		t.Fatalf("S3-compatible server endpoint: %v", err)
	}
	return container, endpoint
}

// runScript runs step of one-collection.sh inside c, for collection and serverLog, and answers
// its exit code and its output; a step that has not ended within scriptDeadline fails t.
func runScript(ctx context.Context, t testing.TB, c testcontainers.Container, step string) (int, string) {
	t.Helper()
	code, output, err := runScriptWithin(ctx, c, scriptPath, step, scriptDeadline)
	if err != nil {
		t.Fatal(err)
	}
	return code, output
}

// runScriptWithin is runScript with the script's path in c and the deadline given, which answers
// an error naming the step when it could not run or had not ended by then.
func runScriptWithin(ctx context.Context, c testcontainers.Container, path, step string, deadline time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	type result struct {
		code   int
		output []byte
		err    error
	}
	// c.Exec reads the step's output to its end before it returns, and that read does not stop
	// with ctx (the docker client's hijacked connection has no deadline of its own), so the run
	// goes on apart while this waits for it or for the deadline. A run the deadline left ends
	// when the container is removed with t.
	done := make(chan result, 1)
	go func() {
		code, reader, err := c.Exec(ctx, []string{path, step, collection, serverLog}, tcexec.Multiplexed())
		if err != nil {
			done <- result{err: err}
			return
		}
		output, err := io.ReadAll(reader)
		done <- result{code: code, output: output, err: err}
	}()
	select {
	case r := <-done:
		switch {
		case r.err == nil:
			return r.code, string(r.output), nil
		case ctx.Err() == nil:
			return 0, "", fmt.Errorf("run one-collection.sh %s in the S3-compatible server's container: %w", step, r.err)
		}
	case <-ctx.Done():
	}
	// The deadline passed, with the run still going or with its read ended by ctx.
	return 0, "", fmt.Errorf("one-collection.sh %s did not end within %s", step, deadline)
}

// call sends one anonymous request with body and answers the status and the body of the answer;
// a request with no answer within requestDeadline fails t.
func call(ctx context.Context, t testing.TB, method, url, body string) (int, string) {
	t.Helper()
	status, answer, err := request(ctx, method, url, body, requestDeadline)
	if err != nil {
		t.Fatal(err)
	}
	return status, answer
}

// request is call with its deadline given, which answers an error naming the request when it
// could not be sent or had no whole answer by then.
func request(ctx context.Context, method, url, body string, deadline time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	sent, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBufferString(body))
	if err != nil {
		return 0, "", fmt.Errorf("build %s %s: %w", method, url, err)
	}
	response, err := http.DefaultClient.Do(sent)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", fmt.Errorf("read the answer to %s %s: %w", method, url, err)
	}
	return response.StatusCode, string(answer), nil
}
