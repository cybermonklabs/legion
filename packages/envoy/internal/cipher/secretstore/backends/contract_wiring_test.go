package backends_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCallsContractCountsARunnableTestsDirectCallAlone holds callsContract, which
// TestEachRegisteredStoreRunsTheContract reads each store's test files with, to a call go test
// makes: one statement of a runnable test's own body calling secretstoretest.StoreContract with
// the store's ID counts, with the testing package imported under its own name or another. A file
// with no call does not count, and nor does a call in a helper no test calls, in a closure or under
// a branch of a test, in a benchmark, in a method, in a function whose name go test does not run
// (Testmemory), or one naming another store.
//
// The named revert: count any call expression of the file, as the gate did, and the helper, the
// closure, the branch, the benchmark, the method and the lowercase name count.
func TestCallsContractCountsARunnableTestsDirectCallAlone(t *testing.T) {
	const imports = "package memory_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest\"\n)\n\n"
	for _, row := range []struct {
		name, source string
		counts       bool
	}{
		{"a runnable test's direct call", imports + "func TestMemory(t *testing.T) {\n\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n}\n", true},
		{"a direct call with testing imported under another name",
			"package memory_test\n\nimport (\n\ttt \"testing\"\n\n\t\"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest\"\n)\n\nfunc TestMemory(t *tt.T) {\n\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n}\n", true},

		{"no contract call", imports + "func TestOther(t *testing.T) {}\n", false},
		{"a call in a helper no test calls", imports + "func memoryContract(t *testing.T) {\n\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n}\n", false},
		{"a call in a closure of a test", imports + "func TestMemory(t *testing.T) {\n\trun := func() { secretstoretest.StoreContract(t, \"memory\", fixture) }\n\t_ = run\n}\n", false},
		{"a call under a branch of a test", imports + "func TestMemory(t *testing.T) {\n\tif testing.Short() {\n\t\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n\t}\n}\n", false},
		{"a call in a benchmark", imports + "func BenchmarkMemory(b *testing.B) {\n\tsecretstoretest.StoreContract(b, \"memory\", fixture)\n}\n", false},
		{"a call in a method", imports + "type suite struct{}\n\nfunc (suite) TestMemory(t *testing.T) {\n\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n}\n", false},
		{"a call in a function go test does not run", imports + "func Testmemory(t *testing.T) {\n\tsecretstoretest.StoreContract(t, \"memory\", fixture)\n}\n", false},
		{"a call naming another store", imports + "func TestMemory(t *testing.T) {\n\tsecretstoretest.StoreContract(t, \"aws\", fixture)\n}\n", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "contract_test.go"), []byte(row.source), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := callsContract(t, dir, []string{"contract_test.go"}, "StoreContract", "memory"); got != row.counts {
				t.Errorf("callsContract = %t, want %t", got, row.counts)
			}
		})
	}
}
