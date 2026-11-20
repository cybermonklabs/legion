# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of which files check-omp-offline.sh reads: a file that is no test code is not, a helper
# under __tests__ or tests/, a .spec. file and a JavaScript test are, nothing under testdata,
# node_modules, dist, .git or .jj is, and a file whose one start goes through env or a shell's -c
# is. Sourced by check-omp-offline.test.sh, whose fixtures, run_check, named and counters they
# use; not run on its own.

echo "case: a start in a file that is no test code is not read, in either language"
root=$(fixture non_test)
put "$root" packages/daemon/internal/podsafety/runner.go <<'EOF'
package podsafety

func Models(home string) error {
	cmd := exec.Command(os.Getenv("LEGION_TEST_OMP"), "models", "list")
	cmd.Env = []string{"HOME=" + home}
	return cmd.Run()
}
EOF
put "$root" packages/pi-envoy/src/run-pinned.ts <<'EOF'
export async function models() {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: "/tmp/home" } }).exited;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts only the fixture's starts" "$(contains "$out" '3 starts of Oh My Pi in 4 test files read')"

echo "case: a helper under __tests__ is test code"
root=$(fixture tests_helper)
put "$root" packages/pi-envoy/src/__tests__/omp-helper.ts <<'EOF'
export async function models(home: string) {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: home } }).exited;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names its start" "$(named omp-helper.ts 2)"

echo "case: a helper under tests/, a .spec. file and a JavaScript test are test code; nothing under testdata, node_modules, dist, .git or .jj is read"
root=$(fixture ts_files)
put "$root" packages/pi-envoy/tests/omp-runner.ts <<'EOF'
export async function models(home: string) {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: home } }).exited;
}
EOF
put "$root" packages/pi-envoy/extensions/models-omp.spec.ts <<'EOF'
test("models", async () => {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
put "$root" packages/pi-envoy/extensions/models-omp.test.js <<'EOF'
test("models", async () => {
  await Bun.spawn([process.env.LEGION_TEST_OMP, "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
for skipped in testdata node_modules dist .git .jj; do
  put "$root" "packages/pi-envoy/$skipped/skipped-omp.test.ts" <<'EOF'
test("models", async () => {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
done
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the start in the helper under tests/" "$(named tests/omp-runner.ts 2)"
check "names the start in the .spec. file" "$(named models-omp.spec.ts 2)"
check "names the start in the JavaScript test" "$(named models-omp.test.js 2)"
check "reads no file under the five skipped directories" "$(is "$(contains "$out" 'skipped-omp')" false)"
check "counts the three" "$(contains "$out" '3 problems over the 6 starts of Oh My Pi in 7 test files read')"

# Each file below holds one start and nothing else the check reads by name: no door, no helper,
# no other program string. The check passes over a file whose every start names, as a string, a
# program that is neither the binary nor a launcher it reads through (WRAPPERS), so each of these
# is read only while env and the shells stay launchers there.
echo "case: a file whose one start is env or a shell's -c, in either language, is read"
root=$(fixture prefilter)
put "$root" packages/daemon/internal/podsafety/env_start_test.go <<'EOF'
package podsafety

func TestEnvStart(t *testing.T) {
	cmd := exec.Command("env", "HOME=/tmp/home", "omp", "models")
	cmd.Env = []string{"HOME=" + t.TempDir()}
}
EOF
put "$root" packages/daemon/internal/podsafety/shell_start_test.go <<'EOF'
package podsafety

func TestShellStart(t *testing.T) {
	cmd := exec.Command("bash", "-c", "omp models list")
	cmd.Env = []string{"HOME=" + t.TempDir()}
}
EOF
put "$root" packages/pi-envoy/extensions/env-start.test.ts <<'EOF'
import { spawn } from "node:child_process";
test("env", () => {
  spawn("env", ["HOME=/tmp/home", "omp", "models"], { env: { HOME: "/tmp/home" } });
});
EOF
put "$root" packages/pi-envoy/extensions/shell-start.test.ts <<'EOF'
import { spawn } from "node:child_process";
test("shell", () => {
  spawn("bash", ["-c", "omp models list"], { env: { HOME: "/tmp/home" } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the Go start through env" "$(named env_start_test.go 4)"
check "names the Go start through bash -c" "$(named shell_start_test.go 4)"
check "names the TypeScript start through env" "$(named env-start.test.ts 3)"
check "names the TypeScript start through bash -c" "$(named shell-start.test.ts 3)"
check "counts the four" "$(contains "$out" '4 problems over the 7 starts of Oh My Pi')"
