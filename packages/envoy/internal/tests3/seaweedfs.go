// Package tests3 runs an S3-compatible server in a container for the tests that need a real one:
// the file store's own tests, which drive the AWS SDK's real client against it so what the store
// sends and how it reads the answers are exercised end to end, and the e2e harness when a run
// asks for one. SeaweedFS stands in for S3: MinIO's image left Docker Hub, and SeaweedFS answers
// the calls the store makes (HeadBucket, HeadObject, PutObject with a SHA-256 checksum, GetObject
// with checksum validation, and S3's own 404 and NoSuchKey shapes). A test that needs the fake's
// failure shapes uses files/filestest instead.
package tests3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Image is the SeaweedFS image every S3-compatible test container runs: ghcr.io/cybermonklabs/legion/ci,
// the copy of seaweedfs:3.97 (.github/ci-images.yaml). The envoy-go CI job reads this declaration
// and pulls the image before its first test step, so no test reaches the registry mid-run; keep it
// a single-line string constant.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:bb05d66d2963b1cc48073190781c3dd29e2c4c88a2ae2986bf58e38c86d89c6e"

// s3Port is the port the server's S3 listener binds inside its container.
const s3Port = "8333"

// Start runs an S3-compatible server holding the empty bucket, removed when t ends, points this
// process's AWS SDK at it for the rest of t (filestest.PointSDKAt), so files.NewS3(ctx, bucket)
// reaches it exactly as a deployment's reaches its bucket, and returns the endpoint for a caller
// that hands it to another process. t.Setenv makes it unusable in a parallel test.
//
// The server binds the master, volume, filer and S3 servers of one process on fixed ports, so it
// runs on Docker's default bridge network in a network namespace of its own, where no other
// program holds them, and publishes none: a port Docker publishes is one its own allocator
// chooses, without asking the kernel whether another program holds it, and ten taken ports in a
// row fail the start (docs/testing.md, "A container a test starts publishes no port"). The
// endpoint is the container's own address on that network. testcontainers publishes the eight
// ports the image declares on that network, so the container is started with the docker CLI.
func Start(t testing.TB, bucket string) string {
	t.Helper()
	ctx := testwait.Context(t)
	// One CPU and 1 GiB, as every container the repository's tests start is capped on a shared
	// host: one process runs the master, volume, filer and S3 servers for a few small objects.
	id := strings.TrimSpace(docker(t, ctx, "run", "--detach", "--network", "bridge", "--cpus", "1", "--memory", "1g",
		Image, "server", "-s3", "-dir=/data", "-ip=0.0.0.0", "-s3.port="+s3Port))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", "--volumes", id).Run() })
	address := strings.TrimSpace(docker(t, ctx, "inspect", "--format", `{{(index .NetworkSettings.Networks "bridge").IPAddress}}`, id))
	if address == "" {
		t.Fatalf("the S3-compatible server's container %s has no address on Docker's bridge network", id)
	}
	endpoint := "http://" + address + ":" + s3Port
	testwait.Until(t, fmt.Sprintf("the S3-compatible server at %s to answer GET / with 200", endpoint), func() (string, bool) {
		status, err := request(ctx, http.MethodGet, endpoint+"/")
		if err != nil {
			return err.Error(), false
		}
		return fmt.Sprint(status), status == http.StatusOK
	})
	// The server creates a bucket on an anonymous PUT of its name.
	status, err := request(ctx, http.MethodPut, endpoint+"/"+bucket)
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	if status != http.StatusOK {
		t.Fatalf("create bucket %s: %d %s", bucket, status, http.StatusText(status))
	}
	filestest.PointSDKAt(t, endpoint)
	return endpoint
}

// request sends an anonymous request with no body to url, and returns the status it was answered.
func request(ctx context.Context, method, url string) (int, error) {
	built, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	response, err := http.DefaultClient.Do(built)
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close()
	return response.StatusCode, nil
}

// docker runs the docker CLI with args and returns what it printed on stdout, failing t with what
// it printed on stderr when it fails.
func docker(t testing.TB, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, exit.Stderr)
	}
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
