import { describe, expect, test } from "bun:test";
import { isAncestor, nearestAncestorNamed } from "../process-ancestry";

test("isAncestor finds this process and its parent, and not a process outside its chain", () => {
  expect(isAncestor(String(process.pid))).toBe(true);
  expect(isAncestor("1")).toBe(false);
  // The parent is one of the row's own processes. The test runner's parent is whatever started
  // it, which can be init (a container whose runner execs the command), and the walk stops at init
  // by design, so the row starts a child that starts a grandchild, and the grandchild asks about
  // the child: a parent that is never init, whatever the row runs under.
  const module = new URL("../process-ancestry.ts", import.meta.url).pathname;
  const grandchild = `import { isAncestor } from ${JSON.stringify(module)};
process.stdout.write(\`\${isAncestor(String(process.ppid))} \${process.ppid}\`);`;
  const child = `const run = Bun.spawnSync([process.execPath, "-e", ${JSON.stringify(grandchild)}]);
process.stdout.write(\`\${run.stdout.toString()} \${process.pid} \${run.stderr.toString()}\`);`;
  const chain = Bun.spawnSync([process.execPath, "-e", child]);
  const [answer, parent, childPid] = chain.stdout.toString().split(" ");
  expect({ parent, answer }).toEqual({ parent: childPid, answer: "true" });
  const stranger = Bun.spawnSync([
    process.execPath,
    "-e",
    "process.stdout.write(String(process.pid))",
  ]);
  expect(isAncestor(stranger.stdout.toString())).toBe(false);
});

describe("nearestAncestorNamed", () => {
  // pid 40 (bash) <- 30 (codex) <- 20 (bash) <- 10 (claude) <- 1
  const table = new Map([
    [40, { command: "bash", parent: 30 }],
    [30, { command: "codex", parent: 20 }],
    [20, { command: "bash", parent: 10 }],
    [10, { command: "claude", parent: 1 }],
  ]);
  const reader = {
    command: (pid: number) => table.get(pid)?.command,
    parent: (pid: number) => table.get(pid)?.parent,
  };

  test("answers the nearest of the names, walking up from the process", () => {
    expect(nearestAncestorNamed(["codex", "claude"], 40, reader)).toBe("codex");
    expect(nearestAncestorNamed(["codex", "claude"], 20, reader)).toBe("claude");
    expect(nearestAncestorNamed(["claude"], 40, reader)).toBe("claude");
  });

  test("reads Claude Code's npm binary, claude.exe, as claude", () => {
    // pid 3 (bash) <- 2 (claude.exe) <- 1 (codex)
    const npm = new Map([
      [3, { command: "bash", parent: 2 }],
      [2, { command: "claude.exe", parent: 5 }],
      [5, { command: "codex", parent: 1 }],
    ]);
    const npmReader = {
      command: (pid: number) => npm.get(pid)?.command,
      parent: (pid: number) => npm.get(pid)?.parent,
    };
    expect(nearestAncestorNamed(["codex", "claude"], 3, npmReader)).toBe("claude");
  });

  test("answers undefined when no ancestor runs as any of them", () => {
    expect(nearestAncestorNamed(["omp"], 40, reader)).toBeUndefined();
    expect(nearestAncestorNamed(["codex"], 999, reader)).toBeUndefined();
  });
});
