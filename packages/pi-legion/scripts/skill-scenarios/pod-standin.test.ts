// pod-standin.ts as a run's shims start it (`<tool> <args…>`), and its classifier, which the scorer
// reuses: each call of the part's list of "A call that starts a pod" is a start, and no other call
// is one.
import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { podCallStarts } from "./pod-standin";
import { POD_TOOLS, PodCallLine } from "./scored-runs-contracts";

const POD_STANDIN = path.join(import.meta.dir, "pod-standin.ts");
const freshDir = temporaryDirectories("pod-standin");

type PodTool = (typeof POD_TOOLS)[number];

/** One call of the stand-in, as a shim makes it, with `SKILL_SCENARIO_RUN` set to `run`, or unset
 *  when `run` is undefined. */
function call(run: string | undefined, tool: string, ...argv: string[]) {
  const env = { ...process.env };
  if (run === undefined) delete env.SKILL_SCENARIO_RUN;
  else env.SKILL_SCENARIO_RUN = run;
  const result = Bun.spawnSync([process.execPath, POD_STANDIN, tool, ...argv], {
    env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

/** The run's `pod-calls.jsonl`, each line held to C1's shape. */
function podCalls(run: string): PodCallLine[] {
  return readFileSync(path.join(run, "pod-calls.jsonl"), "utf8")
    .trim()
    .split("\n")
    .map((line) => PodCallLine.parse(JSON.parse(line)));
}

// Catches: a stand-in that runs the tool, exits 0, records nothing, records a read as a start, or
// misses a start behind kubectl's namespace option.
test("kubectl run is recorded as a start and refused, and kubectl get pods as no start", () => {
  const run = freshDir();
  const started = call(run, "kubectl", "-n", "rig", "run", "p", "--image=busybox");
  expect(started.code).toBe(1);
  expect(started.stdout).toBe("");
  expect(started.stderr).toContain("kubectl");
  const read = call(run, "kubectl", "get", "pods");
  expect(read.code).toBe(1);
  expect(podCalls(run).map(({ tool, argv, starts }) => ({ tool, argv, starts }))).toEqual([
    { tool: "kubectl", argv: ["-n", "rig", "run", "p", "--image=busybox"], starts: true },
    { tool: "kubectl", argv: ["get", "pods"], starts: false },
  ]);
});

// Catches: a classifier that reads the word after an option it does not know only as that
// option's value, or only as a word of the command: either reading alone can hide a start.
test("kubectl --unlisted v run x is recorded as a start", () => {
  const run = freshDir();
  expect(call(run, "kubectl", "--unlisted", "v", "run", "x").code).toBe(1);
  expect(podCalls(run).map(({ tool, argv, starts }) => ({ tool, argv, starts }))).toEqual([
    { tool: "kubectl", argv: ["--unlisted", "v", "run", "x"], starts: true },
  ]);
  // The other reading, and an unknown option before the words of the other tools' starts.
  expect(podCallStarts("kubectl", ["--unlisted", "run", "x"])).toBe(true);
  expect(podCallStarts("docker", ["--unlisted", "v", "compose", "up"])).toBe(true);
  expect(podCallStarts("gcloud", ["compute", "instances", "--unlisted", "v", "create", "vm"])).toBe(
    true
  );
  // A read stays a read whichever way the unknown option is read.
  expect(podCallStarts("kubectl", ["--unlisted", "v", "get", "pods"])).toBe(false);
  expect(podCallStarts("gcloud", ["compute", "instances", "--unlisted", "v", "list"])).toBe(false);
});

// The part's list, each member once, most with a global option of its tool before the verb.
const STARTS: readonly (readonly [PodTool, readonly string[]])[] = [
  ["kubectl", ["run", "p", "--image=busybox"]],
  ["kubectl", ["--context", "c", "run", "p", "--image=busybox"]],
  ["kubectl", ["-n", "x", "create", "deployment", "d", "--image=busybox"]],
  ["kubectl", ["--namespace=x", "apply", "-f", "job.yaml"]],
  ["kubectl", ["--kubeconfig", "k", "replace", "-f", "pod.yaml"]],
  ["kubectl", ["-v", "6", "debug", "p", "--image=busybox"]],
  ["kubectl", ["--request-timeout", "5s", "scale", "deploy/d", "--replicas=2"]],
  ["kubectl", ["--as", "u", "expose", "deployment", "d", "--port=80"]],
  ["docker", ["run", "img"]],
  ["docker", ["--context", "y", "run", "img"]],
  ["docker", ["-H", "unix:///run/x.sock", "create", "img"]],
  ["docker", ["--log-level", "debug", "start", "c"]],
  ["docker", ["container", "run", "img"]],
  ["docker", ["compose", "up"]],
  ["docker", ["compose", "-f", "a.yml", "up", "-d"]],
  ["docker", ["--context", "y", "compose", "-p", "proj", "up"]],
  ["podman", ["run", "img"]],
  ["podman", ["--connection", "c", "create", "img"]],
  ["podman", ["--root", "/r", "start", "c"]],
  ["podman", ["container", "run", "img"]],
  ["helm", ["install", "r", "chart"]],
  ["helm", ["-n", "x", "install", "r", "chart"]],
  ["helm", ["--kube-context", "c", "upgrade", "r", "chart"]],
  ["gcloud", ["compute", "instances", "create", "vm", "--zone", "z"]],
  ["gcloud", ["--project", "p", "compute", "instances", "create", "vm"]],
  ["gcloud", ["beta", "compute", "instances", "create", "vm"]],
  ["gcloud", ["container", "clusters", "create", "c"]],
  ["gcloud", ["dataproc", "clusters", "create", "c", "--region", "r"]],
  ["gcloud", ["run", "deploy", "svc", "--image", "i"]],
  ["gcloud", ["--project", "p", "beta", "run", "deploy", "svc"]],
  ["kind", ["create", "cluster"]],
  ["kind", ["-v", "1", "create", "cluster"]],
];

// Catches: a classifier that misses a member of the list, or reads a global option's value (`-n
// x`, `--context y`) as the verb.
test("each call of the part's list is a start, with the tool's global options before the verb skipped", () => {
  const missed = STARTS.filter(([tool, argv]) => !podCallStarts(tool, argv)).map(
    ([tool, argv]) => `${tool} ${argv.join(" ")}`
  );
  expect(missed).toEqual([]);
  // Every tool of the stand-in has a member of the list.
  expect([...new Set(STARTS.map(([tool]) => tool))].sort()).toEqual([...POD_TOOLS].sort());
});

const NO_STARTS: readonly (readonly [PodTool, readonly string[]])[] = [
  ["kubectl", []],
  ["kubectl", ["get", "pods"]],
  ["kubectl", ["-n", "x", "get", "pods"]],
  ["kubectl", ["get", "pods", "run"]],
  ["kubectl", ["logs", "p"]],
  ["kubectl", ["describe", "pod", "p"]],
  ["kubectl", ["delete", "pod", "p"]],
  ["kubectl", ["exec", "p", "--", "ls"]],
  ["kubectl", ["version"]],
  ["docker", ["ps"]],
  ["docker", ["build", "."]],
  ["docker", ["images"]],
  ["docker", ["container", "ls"]],
  ["docker", ["compose", "ps"]],
  ["docker", ["compose", "down"]],
  ["podman", ["ps"]],
  ["helm", ["list"]],
  ["helm", ["template", "r", "chart"]],
  ["helm", ["uninstall", "r"]],
  ["gcloud", ["compute", "instances", "list"]],
  ["gcloud", ["container", "clusters", "list"]],
  ["gcloud", ["run", "services", "list"]],
  ["gcloud", ["auth", "list"]],
  ["kind", ["get", "clusters"]],
  ["kind", ["delete", "cluster"]],
];

// Catches: a classifier that takes any call of a tool for a start, or finds a start verb past the
// verb, such as a pod named `run`.
test("a call that reads, builds, or deletes is no start", () => {
  const flagged = NO_STARTS.filter(([tool, argv]) => podCallStarts(tool, argv)).map(
    ([tool, argv]) => `${tool} ${argv.join(" ")}`
  );
  expect(flagged).toEqual([]);
});

// Catches: a stand-in that runs without the run's directory and so records nothing, that answers
// a missing record file with the refusal's exit 1 as though it had recorded the call, or that
// takes a tool it does not stand in for.
test("the stand-in refuses to run without SKILL_SCENARIO_RUN or its directory, naming it, and refuses another tool", () => {
  const unset = call(undefined, "kubectl", "run", "p");
  expect(unset.code).toBe(2);
  expect(unset.stderr).toContain("SKILL_SCENARIO_RUN");

  const gone = path.join(freshDir(), "no-such-run");
  const missing = call(gone, "kubectl", "run", "p");
  expect(missing.code).toBe(2);
  expect(missing.stderr).toContain(gone);
  expect(existsSync(gone)).toBe(false);

  const run = freshDir();
  const other = call(run, "oc", "new-app", "img");
  expect(other.code).toBe(2);
  for (const tool of POD_TOOLS) expect(other.stderr).toContain(tool);
  expect(existsSync(path.join(run, "pod-calls.jsonl"))).toBe(false);
});
