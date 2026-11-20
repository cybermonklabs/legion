# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of the TypeScript command forms check-omp-offline.sh reads as starts, and where it looks
# for their helper: exec and execSync of a shell string, execFile, execFileSync, execFileAsync,
# execa, execaSync and execAsync, a shell string's omp after a wrapper, in a $(...), after ; or in
# a shell's -c, and the environment a method or a call is given, a helper in the next statement or
# in the function that returns the binary. Sourced by check-omp-offline.test.sh, whose fixtures,
# run_check, named and counters they use; not run on its own.

echo "case: the environment a method is given is followed to its calls"
root=$(fixture ts_method)
put "$root" packages/pi-envoy/extensions/method-omp.test.ts <<'EOF'
const runner = {
  start(env: Record<string, string>) {
    return Bun.spawn([omp, "models"], { env });
  },
};
runner.start({ HOME: "/tmp/home", ...ompOffline(cleanup) });
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

echo "case: execSync and exec of a command string are starts, read as shell; a Sync one under the helper is refused"
root=$(fixture ts_exec)
put "$root" packages/pi-envoy/extensions/exec-omp.test.ts <<'EOF'
import { exec, execSync } from "node:child_process";
const pinned = process.env.LEGION_TEST_OMP as string;

test("each", () => {
  execSync(`${pinned} models list`, { env: { HOME: "/tmp/home" } });
  exec(`${pinned} models list`, { env: { HOME: "/tmp/home" } }, () => {});
  execSync(`${pinned} models list`, { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
  exec(pinned + " models list", { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } }, () => {});
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare execSync" "$(named exec-omp.test.ts 5)"
check "names the bare exec" "$(named exec-omp.test.ts 6)"
check "refuses the Sync one under the helper" "$(contains "$out" 'exec-omp.test.ts:7: a Sync start of Oh My Pi')"
check "passes exec under the helper" "$(is "$(named exec-omp.test.ts 8)" false)"
check "counts the four" "$(contains "$out" '3 problems over the 7 starts of Oh My Pi')"

echo "case: execFile, execFileSync, execFileAsync, execa and execaSync are starts; the helper passes each but the Sync ones, which a reason passes"
root=$(fixture ts_execfile)
put "$root" packages/pi-envoy/extensions/execfile-omp.test.ts <<'EOF'
import { execFile, execFileSync } from "node:child_process";
import { execa, execaSync } from "execa";
const pinned = process.env.LEGION_TEST_OMP as string;

test("bare", async () => {
  execFile(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } }, () => {});
  execFileSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
  await execFileAsync(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
  await execa(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
  execaSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
});
test("helped", async () => {
  execFile(pinned, ["models", "list"], { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } }, () => {});
  await execFileAsync(pinned, ["models", "list"], { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
  await execa(pinned, ["models", "list"], { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
  execFileSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
  execaSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
});
test("reasoned", () => {
  // omp-offline: models list reads only the catalog the test wrote into HOME.
  execFileSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
  // omp-offline: models list reads only the catalog the test wrote into HOME.
  execaSync(pinned, ["models", "list"], { env: { HOME: "/tmp/home" } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare execFile" "$(named execfile-omp.test.ts 6)"
check "names the bare execFileSync" "$(named execfile-omp.test.ts 7)"
check "names the bare execFileAsync" "$(named execfile-omp.test.ts 8)"
check "names the bare execa" "$(named execfile-omp.test.ts 9)"
check "names the bare execaSync" "$(named execfile-omp.test.ts 10)"
check "passes execFile under the helper" "$(is "$(named execfile-omp.test.ts 13)" false)"
check "passes execFileAsync under the helper" "$(is "$(named execfile-omp.test.ts 14)" false)"
check "passes execa under the helper" "$(is "$(named execfile-omp.test.ts 15)" false)"
check "refuses execFileSync under the helper" "$(contains "$out" 'execfile-omp.test.ts:16: a Sync start of Oh My Pi')"
check "refuses execaSync under the helper" "$(contains "$out" 'execfile-omp.test.ts:17: a Sync start of Oh My Pi')"
check "passes the reasoned execFileSync and execaSync" "$(is "$(contains "$out" 'execfile-omp.test.ts:2[13]:')" false)"
check "counts the twelve" "$(contains "$out" '7 problems over the 15 starts of Oh My Pi')"

echo "case: execAsync of a command string is a start, read as shell, and the helper passes it"
root=$(fixture ts_exec_async)
put "$root" packages/pi-envoy/extensions/exec-async-omp.test.ts <<'EOF'
import { exec } from "node:child_process";
import { promisify } from "node:util";
const execAsync = promisify(exec);
const pinned = process.env.LEGION_TEST_OMP as string;

test("each", async () => {
  await execAsync(`${pinned} models list`, { env: { HOME: "/tmp/home" } });
  await execAsync(`${pinned} models list`, { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare execAsync" "$(named exec-async-omp.test.ts 7)"
check "passes execAsync under the helper" "$(is "$(named exec-async-omp.test.ts 8)" false)"
check "counts the two" "$(contains "$out" '1 problems over the 5 starts of Oh My Pi')"

echo "case: a shell string's omp after nice, timeout, command, exec or env, in a \$(...), after ;, or in a shell's -c is a start, and an argv's after env; as another program's argument it is none"
root=$(fixture ts_wrapped)
put "$root" packages/pi-envoy/extensions/wrapped-omp.test.ts <<'EOF'
import { exec } from "node:child_process";

test("wrapped", () => {
  exec("nice -n 5 omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("timeout 30 omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("command omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("exec omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("env HOME=/tmp/home omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("echo $(omp models list)", { env: { HOME: "/tmp/home" } }, () => {});
  exec("cd /tmp/home; omp models list", { env: { HOME: "/tmp/home" } }, () => {});
  exec("bash -c 'omp models list'", { env: { HOME: "/tmp/home" } }, () => {});
  Bun.spawn(["env", "HOME=/tmp/home", process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: "/tmp/home" } });
  exec("timeout 30 omp models list", { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } }, () => {});
  exec("nice -n 5 echo omp; timeout 30 grep omp models.log", { env: { HOME: "/tmp/home" } }, () => {});
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it after nice -n 5" "$(named wrapped-omp.test.ts 4)"
check "names it after timeout 30" "$(named wrapped-omp.test.ts 5)"
check "names it after command" "$(named wrapped-omp.test.ts 6)"
check "names it after exec" "$(named wrapped-omp.test.ts 7)"
check "names it after env" "$(named wrapped-omp.test.ts 8)"
check "names it in a \$(...)" "$(named wrapped-omp.test.ts 9)"
check "names it after ;" "$(named wrapped-omp.test.ts 10)"
check "names it in a shell's -c" "$(named wrapped-omp.test.ts 11)"
check "names it after env in an argv" "$(named wrapped-omp.test.ts 12)"
check "passes it after timeout under the helper" "$(is "$(named wrapped-omp.test.ts 13)" false)"
check "reads omp as echo's and grep's argument, no start" "$(is "$(named wrapped-omp.test.ts 14)" false)"
check "counts the ten" "$(contains "$out" '9 problems over the 13 starts of Oh My Pi')"

echo "case: a helper in the next statement holds nothing"
root=$(fixture ts_next)
put "$root" packages/pi-envoy/extensions/next-omp.test.ts <<'EOF'
test("next", async () => {
  const listed = Bun.spawn([omp, "models", "list"], { env: { HOME: "/tmp/home" } });
  const proxy = ompOffline(cleanup);
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the spawn" "$(named next-omp.test.ts 2)"

echo "case: a function that returns the binary and calls the helper holds nothing for the start it hands the binary to"
root=$(fixture ts_split)
put "$root" packages/pi-envoy/extensions/split-omp.test.ts <<'EOF'
function pinned(): string {
  ompOffline(cleanup);
  return process.env.LEGION_TEST_OMP!;
}

test("split", async () => {
  await Bun.spawn([pinned(), "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the spawn" "$(named split-omp.test.ts 7)"

echo "case: an environment the call names, or a function of the file builds, is followed to the helper"
root=$(fixture ts_env_name)
put "$root" packages/pi-envoy/extensions/named-env-omp.test.ts <<'EOF'
test("named", async () => {
  const env = { HOME: "/tmp/home", ...ompOffline(cleanup) };
  const listed = Bun.spawn([omp, "models", "list"], { env });
  await listed.exited;
});
function paneEnv(): Record<string, string> {
  return { HOME: "/tmp/home", ...ompOffline(cleanup) };
}
test("from a function", async () => {
  await Bun.spawn([omp, "models"], { env: paneEnv() }).exited;
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts both under the helper" "$(contains "$out" 'offline helper (5) or carries a reason (0)')"
