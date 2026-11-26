import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";
import { defaults } from "./stage4b-reservations";

// Stage 4b's capacity reads (lib/stage4b-room.jq): the reservation an issue pod carries under the
// run's overrides and defaults, the room for the run's pods on the nodes its scheduling admits, and
// whether one of those nodes can hold two of them, each run on fixtures shaped as kubectl prints a
// node list and a pod list.
const lib = join(import.meta.dir);
const include = 'include "stage4b-room";';
const GiB = 1073741824;
// The run's override (stage4b-sandbox-tree.sh's override_issue over the defaults): 2.95 CPU and
// 16 GiB a pod.
const overridden = { ...defaults, issue: { ...defaults.issue, cpu: "2950m", memory: "16Gi" } };
const defaultPod = { cpu: 2, memory: 8 * GiB };
// The obsolete per-role run_resources shape used by the failing rehearsal. Its missing issue key
// makes the preflight's issue-pod reservation unreadable.
const legacyPerRoleResources = {
  architect: { cpu: "250m", memory: "1Gi" },
  planner: { cpu: "250m", memory: "1Gi" },
  implementer: { cpu: "750m", memory: "6Gi" },
  tester: { cpu: "1", memory: "5Gi" },
  reviewer: { cpu: "500m", memory: "2Gi" },
  merger: { cpu: "200m", memory: "1Gi" },
  controller: { cpu: "1", memory: "4Gi" },
};

function jq(filter: string, input: unknown, args: string[] = []) {
  return JSON.parse(
    runJq(["-c", "-L", lib, ...args, `${include} ${filter}`], JSON.stringify(input))
  );
}

const stage4b = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const repository = join(import.meta.dir, "..", "..", "..");
const scratch = temporaryDirectory("stage4b-room-test");
const preflightReservationCall = "preflight_issue_pod_reservation\n";

function runPreflightReservation(scriptPath: string, resources: object) {
  const source = readFileSync(scriptPath, "utf8");
  const call = source.includes(preflightReservationCall) ? preflightReservationCall : "";
  const run = Bun.spawnSync([
    "bash",
    "-c",
    `root=${JSON.stringify(repository)}
run_resources=${JSON.stringify(JSON.stringify(resources))}
check=preflight
${scriptFunctions(scriptPath)("blocked")}
${scriptFunctions(scriptPath)("expected_issue_pod_reservation")}
${scriptFunctions(scriptPath)("preflight_issue_pod_reservation")}
${call}`,
  ]);
  return { code: run.exitCode, out: run.stdout.toString(), err: run.stderr.toString() };
}

// node is a node NAME with ALLOCATABLE and LABELS, Ready and schedulable unless EDIT says otherwise.
function node(
  name: string,
  allocatable: { cpu: string; memory: string },
  labels: Record<string, string> = {},
  edit: object = {}
) {
  return {
    metadata: { name, labels },
    spec: {},
    status: { allocatable, conditions: [{ type: "Ready", status: "True" }] },
    ...edit,
  };
}
// tainted is EDIT for a node carrying TAINTS.
const tainted = (...taints: object[]) => ({ spec: { taints } });
// placed is a pod on NODE in PHASE whose containers request CONTAINERS and whose init containers
// request INIT, each a {cpu, memory} with either side optional.
function placed(
  nodeName: string,
  containers: Record<string, string>[],
  init: Record<string, string>[] = [],
  phase = "Running"
) {
  return {
    spec: {
      nodeName,
      containers: containers.map((requests) => ({ resources: { requests } })),
      initContainers: init.map((requests) => ({ resources: { requests } })),
    },
    status: { phase },
  };
}
// The labels the fixtures' nodes carry, and the run's selectors over them.
const amd64 = { "kubernetes.io/arch": "amd64" };
const general = { ...amd64, "example.com/class": "general" };
// dedicated is the taint on the builds node, and the toleration that admits it.
const dedicated = { key: "example.com/dedicated", value: "builds", effect: "NoSchedule" };
const toleratesDedicated = { ...dedicated, operator: "Equal" };
// gvisor is the RuntimeClass the run's pods run under, with SCHEDULING, or with none, which adds
// nothing at admission.
const gvisor = (scheduling?: object) => ({
  metadata: { name: "gvisor" },
  handler: "runsc",
  ...(scheduling === undefined ? {} : { scheduling }),
});
// sandboxed is GKE Sandbox's gvisor RuntimeClass: its nodes carry its label and its taint.
const sandboxTaint = { key: "sandbox.gke.io/runtime", value: "gvisor", effect: "NoSchedule" };
const sandboxed = gvisor({
  nodeSelector: { "sandbox.gke.io/runtime": "gvisor" },
  tolerations: [{ ...sandboxTaint, operator: "Equal" }],
});

describe("issue_pod_reservation", () => {
  test("is the issue pod's one reservation, cores and bytes", () => {
    expect(
      jq("issue_pod_reservation($expected)", null, [
        "--argjson",
        "expected",
        JSON.stringify(defaults),
      ])
    ).toEqual(defaultPod);
  });

  test("takes the run's override where it sets one", () => {
    expect(
      jq("issue_pod_reservation($expected)", null, [
        "--argjson",
        "expected",
        JSON.stringify(overridden),
      ])
    ).toEqual({ cpu: 2.95, memory: 16 * GiB });
  });

  test("refuses the per-role reservation shape by naming the missing issue CPU reservation", () => {
    expect(() =>
      jq("issue_pod_reservation($expected)", null, [
        "--argjson",
        "expected",
        JSON.stringify(legacyPerRoleResources),
      ])
    ).toThrow("run_resources.issue.cpu is missing");
  });

  test("preflight blocks a per-role reservation before the room read", () => {
    expect(runPreflightReservation(stage4b, legacyPerRoleResources)).toEqual({
      code: 1,
      out: "CHECK preflight: BLOCKED: the run's issue pod reservation is invalid: jq: error (at <unknown>): run_resources.issue.cpu is missing\n",
      err: "",
    });
  });

  test("preflight no longer blocks the legacy reservation when its check is removed", () => {
    const source = readFileSync(stage4b, "utf8");
    const withoutCheck = source.replace(preflightReservationCall, "");
    if (withoutCheck === source)
      throw new Error("stage4b preflight does not invoke its reservation check");
    const copy = join(scratch, "stage4b-sandbox-tree-without-reservation-check.sh");
    writeFileSync(copy, withoutCheck);

    expect(runPreflightReservation(copy, legacyPerRoleResources)).toEqual({
      code: 0,
      out: "",
      err: "",
    });
  });
});

describe("pod_request", () => {
  test("is the larger of the containers' sum and the largest init container, a missing request 0", () => {
    expect(
      jq(
        "pod_request",
        placed(
          "n",
          [{ cpu: "100m", memory: "128Mi" }, { cpu: "200m" }],
          [{ cpu: "2", memory: "64Mi" }]
        )
      )
    ).toEqual({ cpu: 2, memory: 128 * 1048576 });
    expect(
      jq(
        "pod_request",
        placed(
          "n",
          [
            { cpu: "250m", memory: "1Gi" },
            { cpu: "750m", memory: "3Gi" },
          ],
          [{ cpu: "250m", memory: "1Gi" }]
        )
      )
    ).toEqual({ cpu: 1, memory: 4 * GiB });
    expect(jq("pod_request", { spec: {} })).toEqual({ cpu: 0, memory: 0 });
  });
});

describe("run_room", () => {
  const nodes = {
    items: [
      // 3.92 CPU and 15 GiB allocatable, a daemonset and an init-heavy pod on it: 1.92 CPU free.
      node("small", { cpu: "3920m", memory: "15Gi" }, general),
      // Cordoned: never counted.
      node("cordoned", { cpu: "7910m", memory: "30Gi" }, general, { spec: { unschedulable: true } }),
      // Being disrupted by Karpenter: never counted.
      node(
        "disrupted",
        { cpu: "7910m", memory: "30Gi" },
        general,
        tainted({ key: "karpenter.sh/disrupted", effect: "NoSchedule" })
      ),
      // Not Ready: never counted.
      {
        metadata: { name: "unready", labels: general },
        spec: {},
        status: {
          allocatable: { cpu: "7910m", memory: "30Gi" },
          conditions: [{ type: "Ready", status: "False" }],
        },
      },
      // 7.91 CPU and 60 GiB, one issue pod of another project on it and a finished one: room for two
      // more. Its class is not general.
      node("large", { cpu: "7910m", memory: "60Gi" }, { ...amd64, "example.com/class": "builds" }),
      // Tainted NoSchedule: counted only where the run tolerates the taint. The one node labelled
      // example.com/dedicated-node.
      node(
        "builds",
        { cpu: "7910m", memory: "30Gi" },
        { ...general, "example.com/dedicated-node": "yes" },
        tainted(dedicated)
      ),
      // Tainted NoExecute: counted only where the run tolerates the taint.
      node(
        "draining",
        { cpu: "7910m", memory: "30Gi" },
        general,
        tainted({ key: "example.com/maintenance", effect: "NoExecute" })
      ),
      // Tainted PreferNoSchedule alone, which keeps no pod off: counted. An arm64 node.
      node(
        "preferred",
        { cpu: "3920m", memory: "15Gi" },
        { "kubernetes.io/arch": "arm64" },
        tainted({ key: "example.com/spot", effect: "PreferNoSchedule" })
      ),
      // A GKE Sandbox node: counted only under a RuntimeClass that selects and tolerates it.
      node(
        "sandbox",
        { cpu: "7910m", memory: "30Gi" },
        { ...amd64, "sandbox.gke.io/runtime": "gvisor" },
        tainted(sandboxTaint)
      ),
    ],
  };
  const pods = {
    items: [
      placed(
        "small",
        [{ cpu: "100m", memory: "128Mi" }, { cpu: "200m" }],
        [{ cpu: "2", memory: "64Mi" }]
      ),
      placed("large", [{ cpu: "2", memory: "8Gi" }]),
      placed("large", [{ cpu: "2", memory: "8Gi" }], [], "Succeeded"),
      placed("cordoned", [{ cpu: "1", memory: "1Gi" }]),
    ],
  };
  // room is run_room under the scheduling the run's SELECTOR and TOLERATIONS carry once admitted
  // under the RuntimeClass RUNTIME_CLASS.
  const room = (selector: object, tolerations: object[] = [], runtimeClass: object = gvisor()) =>
    jq(
      "admitted_scheduling($selector; $tolerations; $runtime_class) as $scheduling | run_room($scheduling; $nodes[0]; $pods[0]; $pod)",
      null,
      [
        "--argjson",
        "selector",
        JSON.stringify(selector),
        "--argjson",
        "tolerations",
        JSON.stringify(tolerations),
        "--argjson",
        "runtime_class",
        JSON.stringify(runtimeClass),
        "--argjson",
        "nodes",
        JSON.stringify([nodes]),
        "--argjson",
        "pods",
        JSON.stringify([pods]),
        "--argjson",
        "pod",
        JSON.stringify(defaultPod),
      ]
    );
  const counted = (got: { free_nodes: { node: string; fits: number }[] }) =>
    got.free_nodes.map((n) => [n.node, n.fits]);

  test("with no selector, no tolerations and a RuntimeClass of no scheduling, counts every Ready, schedulable, undisrupted node with no NoSchedule or NoExecute taint, and bounds nothing", () => {
    const got = room({});
    expect(got.pod).toEqual(defaultPod);
    expect(got.scheduling).toEqual({ selector: {}, tolerations: [] });
    expect(counted(got)).toEqual([
      ["small", 0],
      ["large", 2],
      ["preferred", 1],
    ]);
    expect(got.free_nodes[0].free.cpu).toBeCloseTo(1.92);
    expect(got.free_nodes[1].free).toEqual({ cpu: 5.91, memory: 52 * GiB });
    expect(got.room).toBeNull();
  });

  test("with a selector, counts only the nodes that carry each of its labels at its value", () => {
    expect(counted(room(amd64))).toEqual([
      ["small", 0],
      ["large", 2],
    ]);
    const got = room(general);
    expect(counted(got)).toEqual([["small", 0]]);
    expect(got.room).toBeNull();
  });

  test("with a selector that matches no node, keeps none and has room 0, so the gate blocks", () => {
    const got = room({ "kubernetes.io/arch": "s390x" });
    expect(got.free_nodes).toEqual([]);
    expect(got.room).toBe(0);
    // The one node it selects carries a taint the run does not tolerate.
    expect(room({ "example.com/dedicated-node": "yes" }).room).toBe(0);
  });

  test("counts a tainted node the run's tolerations admit, by key and value, Exists, and an empty effect", () => {
    expect(counted(room(general, [toleratesDedicated]))).toEqual([
      ["small", 0],
      ["builds", 3],
    ]);
    expect(
      counted(room(general, [{ key: "example.com/maintenance", operator: "Exists" }]))
    ).toEqual([
      ["small", 0],
      ["draining", 3],
    ]);
    // Exists of no key tolerates every taint.
    expect(counted(room(general, [{ operator: "Exists" }]))).toEqual([
      ["small", 0],
      ["builds", 3],
      ["draining", 3],
    ]);
  });

  test("leaves out a tainted node a toleration of another value, key or effect does not admit", () => {
    for (const toleration of [
      { ...toleratesDedicated, value: "other" },
      { ...toleratesDedicated, key: "example.com/other" },
      { ...toleratesDedicated, effect: "NoExecute" },
    ]) {
      expect(counted(room(general, [toleration]))).toEqual([["small", 0]]);
    }
  });

  test("takes the RuntimeClass's node selector over the run's, which narrows the nodes counted", () => {
    const narrowed = room(amd64, [], gvisor({ nodeSelector: { "example.com/class": "general" } }));
    expect(narrowed.scheduling.selector).toEqual(general);
    expect(counted(narrowed)).toEqual([["small", 0]]);
    // Where both name a label, the RuntimeClass's value is the one the pod carries.
    expect(counted(room({ "example.com/class": "builds" }))).toEqual([["large", 2]]);
    expect(
      counted(
        room(
          { "example.com/class": "builds" },
          [],
          gvisor({ nodeSelector: { "example.com/class": "general" } })
        )
      )
    ).toEqual([["small", 0]]);
  });

  test("counts a tainted node the RuntimeClass's tolerations admit, beside the run's own", () => {
    const maintenance = { key: "example.com/maintenance", operator: "Exists" };
    const got = room(general, [maintenance], gvisor({ tolerations: [toleratesDedicated] }));
    expect(got.scheduling.tolerations).toEqual([maintenance, toleratesDedicated]);
    expect(counted(got)).toEqual([
      ["small", 0],
      ["builds", 3],
      ["draining", 3],
    ]);
    // GKE Sandbox's RuntimeClass selects its tainted nodes and tolerates their taint, so they alone
    // count, and without it none of them does.
    expect(counted(room({}, [], sandboxed))).toEqual([["sandbox", 3]]);
    expect(counted(room({})).map(([name]) => name)).not.toContain("sandbox");
  });
});

describe("two_pods_per_node", () => {
  const nodes = {
    items: [
      node("small", { cpu: "3920m", memory: "15Gi" }, general),
      // Holds two pods of the default reservation, but carries a taint.
      node("builds", { cpu: "7910m", memory: "30Gi" }, general, tainted(dedicated)),
      // Holds two by cpu but not by memory.
      node("narrow", { cpu: "7910m", memory: "15Gi" }, general),
      // Holds two, but cordoned.
      node("cordoned", { cpu: "7910m", memory: "60Gi" }, general, { spec: { unschedulable: true } }),
      // Holds two; not general.
      node("large", { cpu: "7910m", memory: "60Gi" }, amd64),
    ],
  };
  const decide = (selector: object, tolerations: object[] = [], runtimeClass: object = gvisor()) =>
    jq(
      "admitted_scheduling($selector; $tolerations; $runtime_class) as $scheduling | two_pods_per_node($scheduling; $nodes[0]; $pod)",
      null,
      [
        "--argjson",
        "selector",
        JSON.stringify(selector),
        "--argjson",
        "tolerations",
        JSON.stringify(tolerations),
        "--argjson",
        "runtime_class",
        JSON.stringify(runtimeClass),
        "--argjson",
        "nodes",
        JSON.stringify([nodes]),
        "--argjson",
        "pod",
        JSON.stringify(defaultPod),
      ]
    );

  test("says the pods may share a node when one of the nodes the run may use holds two in its allocatable", () => {
    expect(decide({})).toEqual({
      fit: true,
      reason:
        "node large's allocatable, 7.91 CPU and 60 GiB, holds two of the run's pods (4 CPU, 16 GiB), so the pods may share a node",
    });
    expect(decide(general, [toleratesDedicated])).toEqual({
      fit: true,
      reason:
        "node builds's allocatable, 7.91 CPU and 30 GiB, holds two of the run's pods (4 CPU, 16 GiB), so the pods may share a node",
    });
    // The RuntimeClass's toleration admits the same node.
    expect(decide(general, [], gvisor({ tolerations: [toleratesDedicated] })).fit).toBe(true);
  });

  test("decides nothing, never false, when none of those nodes holds two", () => {
    expect(decide(general)).toEqual({
      fit: null,
      reason:
        "no node of the 2 the run's pods may use holds two of them (4 CPU, 16 GiB) in its allocatable, and a node the cluster adds may, so whether two of the run's pods can share a node cannot be read",
    });
    expect(decide({ "kubernetes.io/arch": "s390x" }).fit).toBeNull();
  });
});
