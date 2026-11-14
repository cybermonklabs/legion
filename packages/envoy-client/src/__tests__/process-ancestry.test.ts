import { describe, expect, test } from "bun:test";
import { isAncestor, nearestAncestorNamed } from "../process-ancestry";

test("isAncestor finds this process and its parent, and not a process outside its chain", () => {
  expect(isAncestor(String(process.pid))).toBe(true);
  expect(isAncestor(String(process.ppid))).toBe(true);
  expect(isAncestor("1")).toBe(false);
  const child = Bun.spawnSync([
    process.execPath,
    "-e",
    "process.stdout.write(String(process.pid))",
  ]);
  expect(isAncestor(child.stdout.toString())).toBe(false);
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

  test("answers undefined when no ancestor runs as any of them", () => {
    expect(nearestAncestorNamed(["omp"], 40, reader)).toBeUndefined();
    expect(nearestAncestorNamed(["codex"], 999, reader)).toBeUndefined();
  });
});
