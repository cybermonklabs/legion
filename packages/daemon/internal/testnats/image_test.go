package testnats

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sjawhar/envoy/testnatsimages"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A start that fails after Docker created the container — here a readiness wait that can never
// succeed, as a start that fails under load does — leaves no container behind once it returns: the
// caller is handed no container to remove. The container is asked for by its label on the same
// Docker daemon testcontainers created it on. The three seconds are the means of the failure, the
// span after which testcontainers gives up on a log line NATS never prints, and no wait for anything
// that happens. Where no container runtime answers, the test skips, and on GitHub Actions it fails
// (skipWithoutContainerRuntime).
func TestAFailedStartLeavesNoContainer(t *testing.T) {
	skipWithoutContainerRuntime(t)
	label := "legion-testnats-" + rand.Text()
	created, err := startContainer(testwait.Context(t), testnatsimages.Image,
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
