import { expect, test } from "bun:test";
import { classifySession, daemonLaunchedAgentProcess } from "./legion-session";

/** The command line the daemon's sandbox runtime writes, as `process.argv` reads it. */
const AGENT_ARGV = ["/usr/bin/bun", "/opt/omp/index.js", "--mode", "rpc", "--session", "ses_1"];
/** The environment the daemon hands a phase worker's agent. */
const WORKER_ENVIRONMENT = {
  LEGION_ROLE: "implementer",
  LEGION_TREE: "REPO-42",
  LEGION_ISSUE: "REPO-43",
  LEGION_BOOT_TOKEN: "boot-token",
};

test("classifies controllers, root architects, sub-architects, phase workers, and ordinary sessions", () => {
  // The root architect's own issue key equals the tree's.
  expect(
    classifySession({
      LEGION_ROLE: "architect",
      LEGION_TREE: "REPO-42",
      LEGION_ISSUE: "REPO-42",
    })
  ).toEqual({ kind: "root-architect", tree: "REPO-42" });
  // A sub-architect on a child issue is a phase worker like any other role.
  expect(
    classifySession({
      LEGION_ROLE: "architect",
      LEGION_TREE: "REPO-42",
      LEGION_ISSUE: "REPO-43",
    })
  ).toEqual({
    kind: "phase-worker",
    role: "architect",
    tree: "REPO-42",
    issue: "REPO-43",
  });
  expect(classifySession({ LEGION_CONTROLLER: "1" })).toEqual({
    kind: "controller",
  });
  expect(
    classifySession({
      LEGION_ROLE: "reviewer",
      LEGION_TREE: "REPO-42",
      LEGION_ISSUE: "REPO-43",
    })
  ).toEqual({
    kind: "phase-worker",
    role: "reviewer",
    tree: "REPO-42",
    issue: "REPO-43",
  });
  expect(classifySession({})).toEqual({ kind: "not-legion" });
});

test("classifies the controller by LEGION_CONTROLLER alone, ignoring a redundant LEGION_ROLE=controller", () => {
  expect(classifySession({ LEGION_CONTROLLER: "1", LEGION_ROLE: "controller" })).toEqual({
    kind: "controller",
  });
});

test("rejects an unrecognized LEGION_ROLE value", () => {
  expect(() => classifySession({ LEGION_ROLE: "explorer", LEGION_TREE: "REPO-42" })).toThrow(
    'LEGION_ROLE "explorer" is not a Legion role'
  );
});

test("rejects a session launched with both controller and tree markers", () => {
  expect(() =>
    classifySession({
      LEGION_CONTROLLER: "1",
      LEGION_CONTROLLER_SECRET: "controller-secret",
      LEGION_TREE: "REPO-42",
    })
  ).toThrow("both controller and tree launch markers");
});

test("reads a role's agent from the three marks, in either spelling of --mode rpc", () => {
  expect(daemonLaunchedAgentProcess(WORKER_ENVIRONMENT, AGENT_ARGV)).toBe(true);
  expect(
    daemonLaunchedAgentProcess(WORKER_ENVIRONMENT, [
      "/usr/bin/bun",
      "/opt/omp/index.js",
      "--mode=rpc",
    ])
  ).toBe(true);
  // The daemon-launched controller is a Legion session too, and gets the same reading.
  expect(
    daemonLaunchedAgentProcess(
      { LEGION_CONTROLLER: "1", LEGION_BOOT_TOKEN: "boot-token" },
      AGENT_ARGV
    )
  ).toBe(true);
  // The boot token as the file pointer the daemon writes counts as much as the value does.
  expect(
    daemonLaunchedAgentProcess(
      {
        LEGION_ROLE: "implementer",
        LEGION_TREE: "REPO-42",
        LEGION_ISSUE: "REPO-43",
        LEGION_BOOT_TOKEN_FILE: "/run/legion/boot-token",
      },
      AGENT_ARGV
    )
  ).toBe(true);
});

test("reads no agent when any one of the three marks is missing", () => {
  // No boot token: the daemon's image probe launches Oh My Pi with both plugin roots in a pod
  // whose environment carries none, and the operator-started controller deletes both names.
  const { LEGION_BOOT_TOKEN: _token, ...noToken } = WORKER_ENVIRONMENT;
  expect(daemonLaunchedAgentProcess(noToken, AGENT_ARGV)).toBe(false);
  // No Legion session: a person's own Oh My Pi outside a role's pod.
  expect(daemonLaunchedAgentProcess({ LEGION_BOOT_TOKEN: "boot-token" }, AGENT_ARGV)).toBe(false);
  // No --mode rpc: a person's own session inside a role's pod, started in the pane.
  expect(
    daemonLaunchedAgentProcess(WORKER_ENVIRONMENT, ["/usr/bin/bun", "/opt/omp/index.js"])
  ).toBe(false);
  // `--mode` naming another mode, and a bare `rpc` argument that no `--mode` precedes.
  expect(
    daemonLaunchedAgentProcess(WORKER_ENVIRONMENT, ["/usr/bin/bun", "--mode", "tui", "rpc"])
  ).toBe(false);
});

test("reads no agent for a shell that inherited the role's whole environment", () => {
  // Every descendant of the agent reads the role and the boot token, which is why the command
  // line, which is not inherited, is the mark that tells them apart.
  expect(daemonLaunchedAgentProcess(WORKER_ENVIRONMENT, ["/bin/bash", "-lc", "bun test"])).toBe(
    false
  );
});
