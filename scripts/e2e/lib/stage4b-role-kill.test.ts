import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { rowWaits, untilTrue } from "./row-waits";
import { scriptAssignments, scriptFunctions } from "./script-functions";

// Stage 4b's `kill` end of one role: role_kill_sh signals the worker-shim of one role-init, and
// role_generation_ended watches the kill land on that role alone. role_kill_sh runs here on real
// processes this test starts: a stand-in role-init whose children are a stand-in shim (argv
// `legion worker-shim`, the leader of a process group of its own) and a helper the agent detached.
// role_generation_ended runs as the script defines it, with the cluster reads stubbed.
const lib = join(import.meta.dir, "role-namespaces.sh");
const stage4b = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const fn = scriptFunctions(stage4b);
const snippet = scriptAssignments(stage4b);
// On the memory root (`memoryRoot`): the rows leave their proc roots here (own_proc), about 320
// entries in all, and the file's afterAll removes them under Bun's 5 s for a hook, which on a dev
// box's shared disk at a load near 1,500 timed out.
const dir = temporaryDirectory("stage4b-role-kill-test", { root: memoryRoot() });
// The home every bash this file starts runs under, so none of them reads the caller's. The
// children below take PATH, this home and the case's own names, and nothing else of the caller's.
const home = join(dir, "home");
beforeAll(() => {
  mkdirSync(home, { recursive: true });
});

const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

// role_kill_sh reads only the role-init's own list of its children (INIT/task/*/children) and
// those children's files, so its cost is the role-init's children, not the host's process table,
// where it once ran a sed for every entry of /proc: one call over a devbox's 28,203 processes took
// 166 s, past these rows' 120 s. The rows below still hand it a proc root of their own, made by
// own_proc: the role-init and each child the kernel lists for it, each a link to its real /proc
// entry, so role_kill_sh reads those processes' own files and signals their real pids. The waits
// are on events the stand-ins print, never on a count of polls, and each ends 20 s before the
// row's bound of 120 s at the latest, naming what it waited for (lib/row-waits.ts).
//
// own_proc INIT PID... makes that proc root for INIT in a fresh directory under the file's scratch
// directory, and leaves its path in $root: lib/proc-root.ts's procRootOf and notInRoot in bash,
// since each row's own shell makes its root. The kernel's list of INIT's children
// (/proc/INIT/task/INIT/children) is read with a builtin, so no process of this shell's own is in
// it. Each PID named must be one of those children: a row's refusal is read as a pass only with the
// child it is about in view, so a root without it is status 2, naming the child, and the row's
// shell still stops what it started.
const ownProc = `own_proc() {
  local init=$1 p
  local -a children=()
  shift
  root=$(mktemp -d ${JSON.stringify(join(dir, "proc-XXXXXX"))}) || return 2
  read -r -a children <"/proc/$init/task/$init/children" || :
  for p in "$init" "\${children[@]}"; do ln -s "/proc/$p" "$root/$p" || return 2; done
  for p in "$@"; do
    [ -e "$root/$p" ] || { echo "the kernel lists no child $p of role-init $init" >&2; return 2; }
  done
}`;
const env = { PATH: process.env.PATH ?? "", HOME: home };
const waits = rowWaits(120_000);
describe("role_kill_sh", () => {
  // Stops what a row started. Bun runs no `finally` of a body it timed out, and does run its
  // afterEach.
  let stop: (() => Promise<void>) | undefined;
  afterEach(async () => {
    await stop?.();
    stop = undefined;
  });

  test("kills the shim's group and leaves a detached helper of the role-init alone", async () => {
    // The stand-in shim is `cat` on a FIFO named worker-shim, exec'd as `legion`: the role-init's
    // open of the FIFO's write end returns only once the shim holds its read end, so the shim's argv
    // is `legion worker-shim` before its pid is printed. The role-init prints the shim's status once
    // it has reaped it.
    const init = Bun.spawn(
      [
        "bash",
        "-c",
        `cd ${JSON.stringify(dir)} && mkfifo worker-shim
setsid bash -c 'exec -a legion cat worker-shim' & s=$!
exec 3>worker-shim
echo "shim $s"
sleep 300 & echo "helper $!"
wait "$s"; echo "shim ended $?"
wait`,
      ],
      { env, stdout: "pipe" }
    );
    // The role-init's output, read as it comes: printed waits for a line of it, until the end of
    // the row's waits, and then fails naming the line and what the role-init printed instead.
    let out = "";
    let ended = false;
    void (async () => {
      const decoder = new TextDecoder();
      for await (const chunk of init.stdout) out += decoder.decode(chunk, { stream: true });
      ended = true;
    })();
    const printed = async (line: RegExp) => {
      await untilTrue(`the stand-in role-init to print ${line}`, waits.end(), () => {
        if (line.test(out)) return true;
        if (ended) throw new Error(`the stand-in role-init ended before ${line}: ${out}`);
        return `it printed ${JSON.stringify(out)}`;
      });
      return line.exec(out)?.[1];
    };
    // The shim's pid is the test's until the role-init has reaped it.
    let shim = 0;
    let helper = 0;
    let reaped = false;
    stop = async () => {
      // Only the processes this test started, by the pids it recorded.
      for (const pid of [helper, reaped ? 0 : shim, init.pid]) {
        if (pid > 0 && alive(pid)) process.kill(pid, "SIGKILL");
      }
      await init.exited;
    };
    await printed(/^helper (\d+)$/m);
    shim = Number(/^shim (\d+)$/m.exec(out)?.[1]);
    helper = Number(/^helper (\d+)$/m.exec(out)?.[1]);
    const lines = `. ${JSON.stringify(lib)}\n${ownProc}\nown_proc ${init.pid} ${shim} ${helper} && proc_root=$root sh -c "$role_kill_sh" role-kill ${init.pid}`;
    const kill = Bun.spawnSync(["bash", "-c", lines], { env });
    expect(kill.stderr.toString()).toBe("");
    expect(kill.exitCode).toBe(0);
    // 137: the shim ended of SIGKILL.
    expect(await printed(/^shim ended (\d+)$/m)).toBe("137");
    reaped = true;
    expect(alive(helper)).toBe(true);
  });

  test("a role-init with no worker-shim child is refused, nothing signalled", () => {
    const lines = `. ${JSON.stringify(lib)}\n${ownProc}\nsleep 300 & h=$!\nown_proc $$ $h && proc_root=$root sh -c "$role_kill_sh" role-kill $$; rc=$?\nkill -0 $h && echo "helper alive"\nkill -KILL $h\nexit $rc`;
    const kill = Bun.spawnSync(["bash", "-c", lines], { env });
    expect(kill.exitCode).toBe(1);
    expect(kill.stderr.toString()).toContain("runs no worker-shim");
    expect(kill.stdout.toString()).toBe("helper alive\n");
  });

  test("a child naming worker-shim in a later argv word is not the shim", () => {
    // argv `bash -c <script> worker-shim <fifo> <ready>`: worker-shim is argv[3], not argv[1]. The
    // decoy waits in bash's builtin read on a FIFO it holds open itself, so it starts no process of
    // its own. Its write to the second FIFO is its first act once exec'd, and this shell's open of
    // that FIFO returns only then: before it the decoy is a copy of this shell, whose argv[1] is -c,
    // and the row would pass whatever role_kill_sh made of a later word.
    const fifo = join(dir, "decoy.fifo");
    const ready = join(dir, "decoy-ready.fifo");
    const lines = `. ${JSON.stringify(lib)}\n${ownProc}\nmkfifo ${JSON.stringify(fifo)} ${JSON.stringify(ready)}\nbash -c 'exec 3<>"$1"; echo >"$2"; read -r -t 300 -u 3 _' worker-shim ${JSON.stringify(fifo)} ${JSON.stringify(ready)} & h=$!\nread -r _ <${JSON.stringify(ready)}\nown_proc $$ $h && proc_root=$root sh -c "$role_kill_sh" role-kill $$; rc=$?\nkill -0 $h && echo "decoy alive"\nkill -KILL $h\nexit $rc`;
    const kill = Bun.spawnSync(["bash", "-c", lines], { env });
    expect(kill.exitCode).toBe(1);
    expect(kill.stderr.toString()).toContain("runs no worker-shim");
    expect(kill.stdout.toString()).toBe("decoy alive\n");
  });
});

describe("role_generation_ended", () => {
  let runs = 0;
  // BEFORE is the pod's role generations at the kill, AFTER what a poll reads; OP is the pod uid
  // the poll's `op get pod` prints, `fails` for a read that exits 1 with nothing printed, or the
  // empty string for the pod the API server answers for and does not have (`--ignore-not-found`).
  // The op stub records its arguments, so the read's own `--ignore-not-found` is proved too.
  function poll(before: string, after: string, op = "uid-a") {
    const evidence = join(dir, `evidence-${++runs}`);
    const args = join(dir, `op-args-${runs}.txt`);
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -euo pipefail
mkdir -p ${JSON.stringify(evidence)}
evidence=${JSON.stringify(evidence)}
ended_pod=pod-a ended_pod_uid=uid-a ended_role=implementer ended_generation=2 ended_identity=container-1
ended_peers=$(awk '$1 != "implementer"' <<<"$BEFORE")
op() { echo "$*" >>${JSON.stringify(args)}; [ "$OP" != fails ] || return 1; echo "$OP"; }
roles_container_identity() { echo container-1; }
role_inits() { printf '%s' "$AFTER"; }
${fn("role_generation_ended")}
if role_generation_ended; then echo ended; else echo waiting; fi`,
      ],
      { env: { PATH: process.env.PATH ?? "", HOME: home, BEFORE: before, AFTER: after, OP: op } }
    );
    expect(result.stderr.toString()).toBe("");
    const violation = join(evidence, "pane-endpoint-violation.txt");
    return {
      out: result.stdout.toString(),
      violation: existsSync(violation) ? readFileSync(violation, "utf8") : "",
      op: existsSync(args) ? readFileSync(args, "utf8") : "",
    };
  }
  const before = "architect 1 10\nimplementer 2 20\nplanner 1 30\n";
  // Every poll reads the pod with --ignore-not-found, so a pod the kill deleted reads empty
  // rather than failing.
  const read = "get pod pod-a -o jsonpath={.metadata.uid} --ignore-not-found\n";

  test("the killed generation still running is a wait", () => {
    expect(poll(before, before)).toEqual({ out: "waiting\n", violation: "", op: read });
  });

  test("the killed generation gone, every peer as it was, is the end", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\n")).toEqual({
      out: "ended\n",
      violation: "",
      op: read,
    });
  });

  test("a sibling's first launch meanwhile is no violation", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\ntester 1 40\n")).toEqual({
      out: "ended\n",
      violation: "",
      op: read,
    });
  });

  test("a pod read that fails is a wait, no violation", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\n", "fails")).toEqual({
      out: "waiting\n",
      violation: "",
      op: read,
    });
  });

  // A kill that took the whole pod is the loudest violation of all, and the one a wait would hide
  // behind its own timeout: the pod is gone, so no later poll can ever say more.
  test("a pod the kill deleted is a violation naming it, never a wait for the pod to come back", () => {
    const result = poll(before, "", "");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toBe(
      "the kill of implementer generation 2 deleted pod pod-a (uid uid-a)\n"
    );
  });

  test("a pod of another uid is a violation", () => {
    const result = poll(before, before, "uid-b");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toContain("replaced pod uid-a");
  });

  test("a peer gone or replaced is a violation naming it", () => {
    const result = poll(before, "architect 2 11\nplanner 1 30\n");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toContain(
      "ended another role's generation in pod pod-a: gone architect 1 10;"
    );
  });
});

// The teardown removes full-agent-negative's workspace file with the planner killed: the removal
// must not go through the planner's namespaces, which a killed planner no longer has.
describe("remove_negative_config", () => {
  function teardown(pod: string) {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -euo pipefail
negative_config_pod=${JSON.stringify(pod)} negative_config_workspace=/legion/workspaces/o/r/legsmoke-3
pod_exec() { echo "no role-init of $2 runs in this container" >&2; return 1; }
roles_exec() { echo "roles_exec $*"; }
${fn("remove_negative_config")}
remove_negative_config && echo removed`,
    ]);
    return { out: result.stdout.toString(), code: result.exitCode };
  }

  test("removes the file in the roles container, no planner needed", () => {
    expect(teardown("pod-3")).toEqual({
      out: "roles_exec pod-3 rm -f /legion/workspaces/o/r/legsmoke-3/.omp/config.yml\nremoved\n",
      code: 0,
    });
  });

  test("no file written is nothing to remove", () => {
    expect(teardown("")).toEqual({ out: "removed\n", code: 0 });
  });
});

// The pod-side /proc readers run here against a fake /proc tree (proc_root), never the host's: a
// Legion role running on the host has a real `legion worker-shim` with an `omp` child, which a scan
// of the host's /proc would find.
interface FakeProc {
  pid: number;
  ppid: number;
  argv: string[];
  pgrp?: number;
}
function fakeProc(name: string, procs: FakeProc[]): string {
  const root = join(dir, `proc-${name}`);
  for (const p of procs) {
    const d = join(root, String(p.pid));
    mkdirSync(d, { recursive: true });
    writeFileSync(join(d, "cmdline"), `${p.argv.join("\0")}\0`);
    writeFileSync(join(d, "status"), `Name:\t${p.argv[0]}\nPid:\t${p.pid}\nPPid:\t${p.ppid}\n`);
    writeFileSync(join(d, "stat"), `${p.pid} (${p.argv[0]}) S ${p.ppid} ${p.pgrp ?? p.pid} 0\n`);
  }
  // Each parent's list of its children, as the kernel writes it in its one thread's
  // /proc/<pid>/task/<pid>/children: in the order the children are given, each pid followed by a
  // space, and no newline. A parent the tree does not list gets its list alone.
  const children = new Map<number, number[]>();
  for (const p of procs) children.set(p.ppid, [...(children.get(p.ppid) ?? []), p.pid]);
  for (const [ppid, pids] of children) {
    const task = join(root, String(ppid), "task", String(ppid));
    mkdirSync(task, { recursive: true });
    writeFileSync(join(task, "children"), pids.map((pid) => `${pid} `).join(""));
  }
  return root;
}
/** Runs `call` after `definition` with `root` as its proc root. `timeout` kills a call that hangs
 *  and reads as no answer. */
function runSnippet(definition: string, call: string, root: string, timeout?: number) {
  const r = Bun.spawnSync(["bash", "-c", `${definition}\n${call}`], {
    env: { PATH: process.env.PATH ?? "", HOME: home, proc_root: root },
    timeout,
  });
  return { out: r.stdout.toString().trim(), err: r.stderr.toString().trim(), code: r.exitCode };
}

// agent_pid_sh finds a role's Oh My Pi by the same rule: the process whose argv[0] is omp under a
// parent whose argv[1] is worker-shim.
describe("agent_pid_sh", () => {
  const def = snippet("agent_pid_sh");
  const probe = (root: string) =>
    runSnippet(def, `sh -c "$agent_pid_sh"'\n    echo "$pid $proc"'`, root);

  test("finds the one Oh My Pi whose parent's argv[1] is worker-shim, in the proc root it is given", () => {
    const root = fakeProc("agent-one", [
      { pid: 1, ppid: 0, argv: ["legion", "launcher", "role-init", "--role", "planner"] },
      { pid: 7, ppid: 1, argv: ["/opt/legion/bin/legion", "worker-shim", "--", "omp"] },
      { pid: 9, ppid: 7, argv: ["omp", "--mode", "rpc"] },
      { pid: 12, ppid: 9, argv: ["bash", "-c", "omp"] },
    ]);
    expect(probe(root)).toEqual({ out: `9 ${root}`, err: "", code: 0 });
  });

  test("a parent naming worker-shim in a later argv word holds no agent", () => {
    const root = fakeProc("agent-later", [
      { pid: 1, ppid: 0, argv: ["legion", "launcher", "--role", "worker-shim"] },
      { pid: 9, ppid: 1, argv: ["omp"] },
    ]);
    const run = probe(root);
    expect(run.err).toBe("no Oh My Pi under a legion worker-shim");
    expect(run.code).toBe(1);
  });

  test("two Oh My Pi processes under worker-shims are refused, both named", () => {
    const root = fakeProc("agent-two", [
      { pid: 7, ppid: 1, argv: ["legion", "worker-shim"] },
      { pid: 9, ppid: 7, argv: ["/usr/bin/omp"] },
      { pid: 17, ppid: 1, argv: ["legion", "worker-shim"] },
      { pid: 19, ppid: 17, argv: ["omp"] },
    ]);
    const run = probe(root);
    expect(run.err).toMatch(
      /^more than one Oh My Pi under a legion worker-shim: pids (9 and 19|19 and 9)$/
    );
    expect(run.code).toBe(1);
  });
});

describe("role_shims_sh", () => {
  test("lists every argv[1] worker-shim of the proc root it is given, and no later-word one", () => {
    const root = fakeProc("shims", [
      { pid: 7, ppid: 1, argv: ["legion", "worker-shim"] },
      { pid: 8, ppid: 1, argv: ["bash", "-c", "x", "worker-shim"] },
      { pid: 17, ppid: 1, argv: ["legion", "worker-shim"] },
    ]);
    const run = runSnippet(snippet("role_shims_sh"), 'sh -c "$role_shims_sh"', root);
    expect(run.out.split("\n").sort()).toEqual(["17", "7"]);
    expect(run.code).toBe(0);
  });
});

describe("role_inits_sh", () => {
  test("prints ROLE GENERATION PID for each role-init of the proc root it is given", () => {
    const root = fakeProc("inits", [
      { pid: 1, ppid: 0, argv: ["legion", "launcher", "--roles-dir", "/r", "--role", "planner"] },
      {
        pid: 20,
        ppid: 1,
        argv: [
          "legion",
          "launcher",
          "role-init",
          "--role",
          "planner",
          "--generation",
          "3",
          "--",
          "x",
        ],
      },
      {
        pid: 30,
        ppid: 1,
        argv: ["legion", "launcher", "role-init", "--generation", "1", "--", "--role", "tester"],
      },
    ]);
    const run = runSnippet(snippet("role_inits_sh"), 'sh -c "$role_inits_sh"', root);
    expect(run).toEqual({ out: "planner 3 20", err: "", code: 0 });
  });
});

// role_enter_sh's refusals before its first nsenter, read from a fake tree; both hops run end to
// end, with a stand-in nsenter, in the tests further down.
describe("role_enter_sh finds the role-init in the proc root it is given", () => {
  const def = `. ${JSON.stringify(lib)}`;
  test("none of the role is refused, a --role after the shim's -- not counted", () => {
    const root = fakeProc("enter-none", [
      {
        pid: 20,
        ppid: 1,
        argv: ["legion", "launcher", "role-init", "--role", "tester", "--", "--role", "planner"],
      },
    ]);
    const run = runSnippet(def, 'sh -c "$role_enter_sh" role-enter planner true', root);
    expect(run).toEqual({
      out: "",
      err: "no role-init of planner runs in this container",
      code: 1,
    });
  });

  test("two of the role are refused, both named", () => {
    const root = fakeProc("enter-two", [
      { pid: 20, ppid: 1, argv: ["legion", "launcher", "role-init", "--role", "planner"] },
      { pid: 30, ppid: 1, argv: ["legion", "launcher", "role-init", "--role", "planner"] },
    ]);
    const run = runSnippet(def, 'sh -c "$role_enter_sh" role-enter planner true', root);
    expect(run.err).toBe("two role-init processes of planner run in this container: 20 and 30");
    expect(run.code).toBe(1);
  });
});

// role_enter_sh end to end against a fake tree, with a stand-in nsenter on PATH that records the
// pid it enters and runs the rest of its argv here: the second hop (role_shim_sh) then reads the
// same fake tree, so it must take the role-init's child whose argv[1] is worker-shim.
function fakeNsenter(name: string): { bin: string; log: string } {
  const bin = join(dir, `bin-${name}`);
  const log = join(dir, `nsenter-${name}.log`);
  mkdirSync(bin, { recursive: true });
  writeFileSync(
    join(bin, "nsenter"),
    `#!/bin/sh\nwhile [ "$1" != -- ]; do [ "$1" = -t ] && echo "$2" >>"${log}"; shift; done\nshift\nexec "$@"\n`,
    { mode: 0o755 }
  );
  return { bin, log };
}
function runEntered(definition: string, call: string, root: string, bin: string) {
  const r = Bun.spawnSync(["bash", "-c", `${definition}\n${call}`], {
    env: { PATH: `${bin}:${process.env.PATH}`, HOME: home, proc_root: root },
  });
  return { out: r.stdout.toString().trim(), err: r.stderr.toString().trim(), code: r.exitCode };
}
const enterTree = (name: string, shims: FakeProc[]) =>
  fakeProc(name, [
    { pid: 1, ppid: 0, argv: ["legion", "launcher", "role-init", "--role", "planner"] },
    ...shims,
  ]);

describe("role_shim_sh", () => {
  const def = `. ${JSON.stringify(lib)}`;
  test("enters the child of PID 1 whose argv[1] is worker-shim, not a later-word one", () => {
    const root = enterTree("shim-one", [
      { pid: 6, ppid: 1, argv: ["bash", "-c", "x", "worker-shim"] },
      { pid: 7, ppid: 1, argv: ["/opt/legion/bin/legion", "worker-shim", "--", "omp"] },
      { pid: 8, ppid: 7, argv: ["legion", "worker-shim"] },
    ]);
    const { bin, log } = fakeNsenter("shim-one");
    const run = runEntered(def, 'sh -c "$role_shim_sh" role-shim planner echo in', root, bin);
    expect(run).toEqual({ out: "in", err: "", code: 0 });
    expect(readFileSync(log, "utf8")).toBe("7\n");
  });

  test("no shim is refused", () => {
    const root = enterTree("shim-none", [
      { pid: 6, ppid: 1, argv: ["bash", "-c", "x", "worker-shim"] },
    ]);
    const { bin } = fakeNsenter("shim-none");
    const run = runEntered(def, 'sh -c "$role_shim_sh" role-shim planner echo in', root, bin);
    expect(run).toEqual({
      out: "",
      err: "no worker-shim runs in the namespaces of planner",
      code: 1,
    });
  });

  test("two shims are refused, both named", () => {
    const root = enterTree("shim-two", [
      { pid: 7, ppid: 1, argv: ["legion", "worker-shim"] },
      { pid: 17, ppid: 1, argv: ["legion", "worker-shim"] },
    ]);
    const { bin } = fakeNsenter("shim-two");
    const run = runEntered(def, 'sh -c "$role_shim_sh" role-shim planner echo in', root, bin);
    // The shell's glob lists 17 before 7.
    expect(run.err).toBe("two worker-shims run in the namespaces of planner: 17 and 7");
    expect(run.code).toBe(1);
  });
});

describe("role_enter_sh runs both hops", () => {
  test("enters the role-init, then its argv[1] worker-shim, and runs the command there", () => {
    const root = enterTree("enter-both", [
      { pid: 6, ppid: 1, argv: ["bash", "-c", "x", "worker-shim"] },
      { pid: 7, ppid: 1, argv: ["legion", "worker-shim"] },
    ]);
    const { bin, log } = fakeNsenter("enter-both");
    const run = runEntered(
      `. ${JSON.stringify(lib)}`,
      'sh -c "$role_enter_sh" role-enter planner sh -c \'echo "$0 $1"\' a "b c"',
      root,
      bin
    );
    expect(run).toEqual({ out: "a b c", err: "", code: 0 });
    expect(readFileSync(log, "utf8")).toBe("1\n7\n");
  });
});

// multi-repo-acceptance.sh's as_role_probe enters its role through role_enter_sh: run with kubectl
// exec stubbed to run its command here, with the fake tree as the pod's /proc (kubectl exec passes
// the caller's environment on to nothing, so as_role_probe's own proc_root never reaches the pod),
// it reaches the role's worker-shim and hands python3 the probe on stdin with its arguments.
describe("as_role_probe", () => {
  const multi = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
  test("runs the probe in the role's worker-shim", () => {
    const root = enterTree("probe", [{ pid: 7, ppid: 1, argv: ["legion", "worker-shim"] }]);
    const { bin, log } = fakeNsenter("probe");
    writeFileSync(
      join(bin, "kubectl"),
      `#!/bin/sh\nwhile [ "$1" != -- ]; do shift; done\nshift\nproc_root=${root} exec "$@"\n`,
      { mode: 0o755 }
    );
    writeFileSync(join(bin, "python3"), '#!/bin/sh\necho "$*"\ncat\n', { mode: 0o755 });
    const probeScript = join(dir, "as-role.py");
    writeFileSync(probeScript, "PROBE\n");
    const run = runEntered(
      `. ${JSON.stringify(lib)}
as_role_script=${JSON.stringify(probeScript)} LEGION_E2E_REPO_T=t/r LEGION_E2E_REPO_O=o/r o_owner=o
${scriptFunctions(multi)("as_role_probe")}`,
      "as_role_probe pod-x planner /proc https://github.com/o/r /trusted",
      root,
      bin
    );
    expect(run).toEqual({
      out: "-I - /proc t/r o/r o https://github.com/o/r /trusted\nPROBE",
      err: "",
      code: 0,
    });
    expect(readFileSync(log, "utf8")).toBe("1\n7\n");
  });
});

describe("role_kill_sh reads the proc root it is given", () => {
  const def = `. ${JSON.stringify(lib)}`;
  test("two worker-shims of the role-init are refused, nothing signalled", () => {
    const root = fakeProc("kill-two", [
      { pid: 7, ppid: 5, argv: ["legion", "worker-shim"] },
      { pid: 17, ppid: 5, argv: ["legion", "worker-shim"] },
    ]);
    const run = runSnippet(def, 'sh -c "$role_kill_sh" role-kill 5', root);
    // The role-init's list names them in the order they started.
    expect(run).toEqual({ out: "", err: "role-init 5 runs two worker-shims: 7 and 17", code: 1 });
  });

  test("a shim outside a process group of its own is refused, nothing signalled", () => {
    const root = fakeProc("kill-group", [
      { pid: 7, ppid: 5, pgrp: 4, argv: ["legion", "worker-shim"] },
    ]);
    const run = runSnippet(def, 'sh -c "$role_kill_sh" role-kill 5', root);
    expect(run).toEqual({
      out: "",
      err: "the shim 7 of role-init 5 is in process group 4, not its own",
      code: 1,
    });
  });

  // The role-init's list of its children is the only list it reads: no other process of the
  // root is opened. One of them, a process the role-init did not start, has a status that is a
  // FIFO nothing writes, so a role_kill_sh that walked the whole root again would wait on it until
  // the row's 120 s bound on a hang, rather than only take longer as the root grows.
  test("reads the role-init's own list of its children and no other process", () => {
    const root = fakeProc("kill-children", [
      { pid: 7, ppid: 5, pgrp: 4, argv: ["legion", "worker-shim"] },
      { pid: 9, ppid: 1, argv: ["omp"] },
    ]);
    const status = join(root, "9", "status");
    rmSync(status);
    const made = Bun.spawnSync(["mkfifo", status]);
    expect(made.exitCode).toBe(0);
    const run = runSnippet(def, 'sh -c "$role_kill_sh" role-kill 5', root, 120_000);
    expect(run).toEqual({
      out: "",
      err: "the shim 7 of role-init 5 is in process group 4, not its own",
      code: 1,
    });
  }, 120_000);

  test("a role-init with no list of its children is refused, naming the list", () => {
    const root = fakeProc("kill-no-list", [{ pid: 7, ppid: 5, argv: ["legion", "worker-shim"] }]);
    rmSync(join(root, "5", "task"), { recursive: true });
    const run = runSnippet(def, 'sh -c "$role_kill_sh" role-kill 5', root);
    expect(run).toEqual({
      out: "",
      err: `role-init 5 lists no children: ${root}/5/task/*/children is not there`,
      code: 1,
    });
  });
});
