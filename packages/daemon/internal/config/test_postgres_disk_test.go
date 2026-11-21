package config

import (
	"strings"
	"testing"
)

// test_postgres_disk settles as the size the file wrote, a bare YAML number included, so the
// daemon gives each issue pod's test database a disk of that size; a file that sets none, or
// sets the key with no value, keeps the test database on the pod's emptyDir.
func TestPGDiskReadsAsTheSizeTheFileWrote(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"a quantity", "    test_postgres: true\n    test_postgres_disk: 20Gi\n", "20Gi"},
		{"the least size", "    test_postgres: true\n    test_postgres_disk: 4Gi\n", "4Gi"},
		{"a bare number", "    test_postgres: true\n    test_postgres_disk: 21474836480\n", "21474836480"},
		{"absent", "    test_postgres: true\n", ""},
		{"absent with no test database", "", ""},
		{"null", "    test_postgres: true\n    test_postgres_disk:\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.TestPostgresDisk; got != tc.want {
				t.Errorf("test_postgres_disk settled %q, want %q", got, tc.want)
			}
		})
	}
}

// A value that is no positive size is refused naming the key, and so is a size under the least
// one, naming the size it read and the least size, since the launcher would reset the test
// database on such a disk at every check. A size for a test database no pod runs is refused
// naming both keys: an operator who sets the disk and leaves test_postgres out must see that no
// pod gets it, at `legion config check` as at boot.
func TestPGDiskRefusesWhatIsNoDiskOfARunningTestDatabase(t *testing.T) {
	const noSize = "runtime.kubernetes.test_postgres_disk must be a positive Kubernetes quantity"
	const noDatabase = "runtime.kubernetes.test_postgres_disk is the disk of the pod's own test database, so it requires runtime.kubernetes.test_postgres: true"
	const tooSmall = ", under the 4Gi least size: the launcher resets the test database while its disk has under 2 GiB free, so a smaller disk would be reset at every check"
	for _, tc := range []struct {
		name, block, want string
	}{
		{"a word", "    test_postgres: true\n    test_postgres_disk: twenty\n", noSize},
		{"a negative size", "    test_postgres: true\n    test_postgres_disk: -1Gi\n", noSize},
		{"zero", "    test_postgres: true\n    test_postgres_disk: 0\n", noSize},
		{"a mapping", "    test_postgres: true\n    test_postgres_disk: {size: 20Gi}\n", noSize},
		{"a fraction of a byte", "    test_postgres: true\n    test_postgres_disk: 500m\n", "runtime.kubernetes.test_postgres_disk is 500m" + tooSmall},
		{"the reset floor", "    test_postgres: true\n    test_postgres_disk: 2Gi\n", "runtime.kubernetes.test_postgres_disk is 2Gi" + tooSmall},
		{"just under the least size", "    test_postgres: true\n    test_postgres_disk: 3999Mi\n", "runtime.kubernetes.test_postgres_disk is 3999Mi" + tooSmall},
		{"no test_postgres", "    test_postgres_disk: 20Gi\n", noDatabase},
		{"test_postgres false", "    test_postgres: false\n    test_postgres_disk: 20Gi\n", noDatabase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
