package daemon

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// runtime.kubernetes.test_postgres_disk reaches sandbox.Options as its size, which is what gives
// each issue pod that runs the test database a disk of its own, while leaving the key out hands
// the runtime no disk and keeps the cluster in the pod's emptyDir.
func TestPGDiskTheSettingReachesTheSandboxRuntime(t *testing.T) {
	cfg := config.Config{Project: "TEST", Projects: map[string]config.Project{"TEST": {}}}
	k := config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", IssueVolume: "50Gi", Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
		TestPostgres: true, TestPostgresDisk: "20Gi",
	}
	opts, err := sandboxOptions(cfg, k, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	if want := resource.MustParse("20Gi"); !opts.TestPostgres || opts.TestPostgresDisk == nil || opts.TestPostgresDisk.Cmp(want) != 0 {
		t.Errorf("Options.TestPostgres = %t, Options.TestPostgresDisk = %v, want true and %s", opts.TestPostgres, opts.TestPostgresDisk, want.String())
	}

	k.TestPostgresDisk = ""
	opts, err = sandboxOptions(cfg, k, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions without the disk: %v", err)
	}
	if opts.TestPostgresDisk != nil {
		t.Errorf("Options.TestPostgresDisk = %s without runtime.kubernetes.test_postgres_disk, want none", opts.TestPostgresDisk.String())
	}
}
