# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of check-omp-offline.sh's TypeScript reading of a start and its environment: a start with
# no helper, the helper form, ompEnv as the helper by its name, the reasoned form, a start spelled
# in prose, a regular expression holding a backtick, and a Sync start under the helper. Sourced by
# check-omp-offline.test.sh, whose fixtures, run_check, named and counters they use; not run on its
# own.

echo "case: a TypeScript start whose environment calls no helper is refused"
root=$(fixture ts_bare)
put "$root" packages/pi-envoy/extensions/bare-omp.test.ts <<'EOF'
const omp = process.env.LEGION_TEST_OMP;

test("models", async () => {
  const listed = Bun.spawn([omp!, "models", "list"], {
    env: { HOME: "/tmp/home", PATH: "/usr/bin:/bin" },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the start" "$(contains "$out" 'bare-omp.test.ts:4: a start of the real Oh My Pi whose environment calls neither ompOffline nor ompEnv')"
check "annotates the line" "$(contains "$out" '::error file=packages/pi-envoy/extensions/bare-omp.test.ts,line=4::')"

echo "case: the helper form of a TypeScript start in a test passes"
root=$(fixture ts_helped)
put "$root" packages/pi-envoy/extensions/helped-omp.test.ts <<'EOF'
import { ompOffline } from "@legion/pi-shared/test/omp-offline";

test("models", async () => {
  const listed = Bun.spawn([binary, "models", "refresh"], {
    cwd: workspace,
    env: { HOME: home, PATH: "/usr/bin:/bin", ...ompOffline(cleanup) },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

# The harness's own spawns pass through ompEnv's body, which calls ompOffline in the same file; a
# test that imports ompEnv passes only because ompEnv is a helper by its name.
echo "case: ompEnv imported from the harness is the helper by its name"
root=$(fixture ts_env_helper)
put "$root" packages/pi-envoy/extensions/env-helper-omp.test.ts <<'EOF'
import { ompEnv } from "@legion/pi-shared/test/omp-harness";

test("by ompEnv", async () => {
  await Bun.spawn([omp, "models"], { env: { ...ompEnv(home, cleanup) } }).exited;
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

echo "case: the reasoned form of a TypeScript start passes"
root=$(fixture ts_reasoned)
put "$root" packages/pi-envoy/extensions/tmux-omp.test.ts <<'EOF'
async function tmuxCommand(binary: string, args: readonly string[], environment: Record<string, string>) {
  // omp-offline: `binary` is tmux; its server runs under paneEnvironment's, which holds the proxy.
  const child = Bun.spawn([binary, "-u", ...args], {
    env: environment,
  });
  return child.exited;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the reason" "$(contains "$out" 'offline helper (3) or carries a reason (1)')"

echo "case: a TypeScript start spelled in a comment or a string starts nothing"
root=$(fixture ts_prose)
put "$root" packages/pi-envoy/extensions/prose-omp.test.ts <<'EOF'
// Bun.spawn([omp, "models", "list"]) once ran here with no proxy.
const code = `const r = Bun.spawnSync([omp, "agents"]);`;
const text = 'Bun.spawn([binary, "models"])';
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no start of its own" "$(contains "$out" '3 starts of Oh My Pi in 5 test files read')"

# A backtick inside a regular expression literal, read as the opening of a template, would blank
# every line up to the next backtick and hide whatever start sits there. The second line is the
# one that would: in the first, the quote before the backtick opens a string the line's end closes.
echo "case: a regular expression holding a backtick hides no start after it"
root=$(fixture ts_regex)
put "$root" packages/pi-envoy/extensions/regex-omp.test.ts <<'EOF'
const QUOTED = /["$`\\]/;
const TICKS = /`+/g;

test("models", async () => {
  const listed = Bun.spawn([omp, "models", "list"], {
    env: { HOME: home },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the start" "$(contains "$out" 'regex-omp.test.ts:5: a start of the real Oh My Pi')"

echo "case: a Sync start under the helper is refused"
root=$(fixture ts_sync)
put "$root" packages/pi-shared/test/agents.ts <<'EOF'
import { ompOffline } from "./omp-offline";

export function bundledAgents(omp: string, cleanup: (() => Promise<void>)[]) {
  const unpacked = Bun.spawnSync([omp, "agents", "unpack"], {
    env: { HOME: "/tmp/home", ...ompOffline(cleanup) },
  });
  return unpacked.exitCode;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says why" "$(contains "$out" 'agents.ts:4: a Sync start of Oh My Pi under ompOffline.s proxy')"
