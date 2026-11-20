# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of how check-omp-offline.sh follows the binary in TypeScript, whatever its name: a spawn of
# process.env.LEGION_TEST_OMP itself, Bun's $`...`, a name bound to it, Bun.env, a constant naming
# it, a function that returns it, which, a joined, resolved or literal path, an argv or cmd: by
# name, a shell's -c, "omp", OMP_BIN, either part of ||, a + or a template, a parameter and
# ompPath. Sourced by check-omp-offline.test.sh, whose fixtures, run_check, named and counters they
# use; not run on its own.

echo "case: a spawn of process.env.LEGION_TEST_OMP itself is a start"
root=$(fixture ts_door_itself)
put "$root" packages/pi-envoy/extensions/door-omp.test.ts <<'EOF'
test("models", async () => {
  const listed = Bun.spawn([process.env.LEGION_TEST_OMP!, "models", "list"], {
    env: { HOME: "/tmp/home" },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the spawn" "$(named door-omp.test.ts 2)"

echo "case: Bun's \$\`\${omp} ...\` is a start, read as shell, and the .env() chained to it holds the helper"
root=$(fixture ts_dollar)
put "$root" packages/pi-envoy/extensions/dollar-omp.test.ts <<'EOF'
import { $ } from "bun";
const pinned = process.env.LEGION_TEST_OMP as string;

test("bare", async () => {
  await $`${pinned} models list`.env({ HOME: "/tmp/home" });
});
test("after cd", async () => {
  await $`cd ${dir} && ${pinned} models list`;
});
test("helped", async () => {
  await $`${pinned} models list`.env({ HOME: "/tmp/home", ...ompOffline(cleanup) });
});
test("another program", async () => {
  await $`echo ${pinned}`;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare one" "$(named dollar-omp.test.ts 5)"
check "names the one after cd" "$(named dollar-omp.test.ts 8)"
check "passes the helped one" "$(is "$(named dollar-omp.test.ts 11)" false)"
check "counts three starts, and no start of echo" "$(contains "$out" '2 problems over the 6 starts of Oh My Pi')"

echo "case: the binary under any name: LEGION_TEST_OMP, Bun.env, a constant naming it, a function that returns it"
root=$(fixture ts_values)
put "$root" packages/pi-envoy/extensions/pinned.ts <<'EOF'
export const PINNED_VARIABLE = "LEGION_TEST_OMP";

export function pinnedBinary(): string {
  return process.env.LEGION_TEST_OMP ?? "";
}
EOF
put "$root" packages/pi-envoy/extensions/values-omp.test.ts <<'EOF'
import { PINNED_VARIABLE, pinnedBinary } from "./pinned";

const VARIABLE = "LEGION_TEST_OMP";
const renamed = process.env.LEGION_TEST_OMP;
const fromBun = Bun.env.LEGION_TEST_OMP as string;
const byConstant = process.env[VARIABLE]!;

test("each", async () => {
  await Bun.spawn([renamed!, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([fromBun, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([byConstant, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([pinnedBinary(), "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([process.env[PINNED_VARIABLE]!, "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the name bound to it" "$(named values-omp.test.ts 9)"
check "names Bun.env's" "$(named values-omp.test.ts 10)"
check "names the constant's" "$(named values-omp.test.ts 11)"
check "names the function's, from the file it is imported from" "$(named values-omp.test.ts 12)"
check "names the one by another file's constant" "$(named values-omp.test.ts 13)"

echo "case: the binary by which, a joined path, a literal path, an argv or cmd: by name, and in a shell's -c"
root=$(fixture ts_paths)
put "$root" packages/pi-envoy/extensions/paths-omp.test.ts <<'EOF'
import path from "node:path";
const found = Bun.which("omp")!;
const joined = path.join(home, ".local", "bin", "omp");

test("each", async () => {
  await Bun.spawn([found, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([joined, "models"], { env: { HOME: "/tmp/home" } }).exited;
  const argv = [found, "models"];
  await Bun.spawn(argv, { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn(["bash", "-c", `${found} models list`], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn({ cmd: [found, "models"], env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn({ cmd: [found, "models"], env: { ...ompOffline(cleanup) } }).exited;
});
EOF
put "$root" packages/pi-envoy/extensions/literal-omp.test.ts <<'EOF'
test("literal", async () => {
  await Bun.spawn(["/usr/local/bin/omp", "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
put "$root" packages/pi-envoy/extensions/shell-string-omp.test.ts <<'EOF'
import { spawn } from "node:child_process";
test("shell", () => {
  spawn("omp models list", { shell: true, env: { HOME: "/tmp/home" } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names Bun.which's" "$(named paths-omp.test.ts 6)"
check "names the joined path" "$(named paths-omp.test.ts 7)"
check "names an argv bound to a name" "$(named paths-omp.test.ts 9)"
check "names the shell's -c" "$(named paths-omp.test.ts 10)"
check "names cmd: with an env of no helper" "$(named paths-omp.test.ts 11)"
check "passes cmd: with the helper in its env" "$(is "$(named paths-omp.test.ts 12)" false)"
check "names the literal path, alone in its file" "$(named literal-omp.test.ts 2)"
check "names a command line spawn gives a shell, alone in its file" "$(named shell-string-omp.test.ts 3)"

echo "case: the binary by path.resolve, which, \"omp\", OMP_BIN, an env's [\"LEGION_TEST_OMP\"], either part of ||, a + or a template a function of the file returns, a parameter, and ompPath"
root=$(fixture ts_spellings)
put "$root" packages/pi-envoy/extensions/spellings-omp.test.ts <<'EOF'
import path from "node:path";
import which from "which";
const resolved = path.resolve(home, "bin", "omp");
const found = await which("omp");
const either = process.env.OTHER_TOOL || process.env.LEGION_TEST_OMP;
function joined(): string {
  return home + "/.local/bin/omp";
}
function templated(): string {
  return `${home}/.local/bin/omp`;
}
function start(program: string) {
  return Bun.spawn([program, "models"], { env: { HOME: "/tmp/home" } });
}

test("each", async () => {
  await Bun.spawn([resolved, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([found, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn(["omp", "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([process.env["OMP_BIN"]!, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([process.env["LEGION_TEST_OMP"]!, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([either, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([joined(), "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([templated(), "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([ompPath, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await start(process.env.LEGION_TEST_OMP!).exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names path.resolve's" "$(named spellings-omp.test.ts 17)"
check "names which's" "$(named spellings-omp.test.ts 18)"
check "names \"omp\" itself" "$(named spellings-omp.test.ts 19)"
check "names the env's [\"OMP_BIN\"], which no name of the binary spells" "$(named spellings-omp.test.ts 20)"
check "names the env's [\"LEGION_TEST_OMP\"]" "$(named spellings-omp.test.ts 21)"
check "names the second part of ||" "$(named spellings-omp.test.ts 22)"
check "names a + of strings a function of the file returns" "$(named spellings-omp.test.ts 23)"
check "names a template a function of the file returns" "$(named spellings-omp.test.ts 24)"
check "names ompPath" "$(named spellings-omp.test.ts 25)"
check "names the parameter its function's call is given" "$(named spellings-omp.test.ts 13)"
check "counts the ten" "$(contains "$out" '10 problems over the 13 starts of Oh My Pi')"
