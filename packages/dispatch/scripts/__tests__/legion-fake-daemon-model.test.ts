import { expect, test } from "bun:test";

import {
  type DaemonState,
  FakeDaemonModel,
  type PushEvent,
  ROLE,
  RUNNING,
} from "../legion-fake-daemon-model";

/** A replay in the daemon's shape: running issues with slots and a working role each, a few
 *  queued roots and finished ones, and roles in every claim state the real daemon reports. */
function replay(): DaemonState {
  const issues: DaemonState["issues"] = {};
  RUNNING.forEach((phase, index) => {
    for (let copy = 0; copy < 3; copy++) {
      const key = `ACME-${index * 10 + copy + 1}`;
      const role = ROLE[phase] ?? "implementer";
      issues[key] = {
        key,
        phase,
        slot: { index: index * 3 + copy, admittedAt: "2026-11-01T00:00:00Z" },
        ...(role === "architect"
          ? { architect: { state: "working" } }
          : {
              workers: {
                [role]: { claim: { state: "working" } },
                [role === "planner" ? "tester" : "planner"]: { claim: { state: "retired" } },
              },
            }),
        pullRequest: { checksVerdict: "pending" },
      };
    }
  });
  for (let n = 100; n < 105; n++) issues[`ACME-${n}`] = { key: `ACME-${n}`, phase: "admitted" };
  for (let n = 200; n < 203; n++) issues[`ACME-${n}`] = { key: `ACME-${n}`, phase: "done" };
  return {
    admission: { cap: 30, waiting: ["ACME-100", "ACME-101", "ACME-102"] },
    issues,
    settings: { admission_cap: 30 },
  };
}

/** Each issue's phase, slot and working roles as the events say, newest per field winning. */
function fold(events: readonly PushEvent[]) {
  const issues = new Map<string, { phase?: string; slot?: unknown; roles: Map<string, string> }>();
  for (const event of events) {
    if (event.issue === undefined) continue;
    const held = issues.get(event.issue) ?? { roles: new Map() };
    if (event.kind === "phase") held.phase = event.value as string;
    if (event.kind === "slot") held.slot = event.value;
    if (event.kind === "claim_state") {
      const claim = event.value as { role: string; state: string };
      held.roles.set(claim.role, claim.state);
    }
    issues.set(event.issue, held);
  }
  return issues;
}

test("a snapshot carries every issue's phase, its slot and its role in a turn", () => {
  const state = replay();
  const model = new FakeDaemonModel(structuredClone(state));
  const folded = fold(model.snapshotEvents());

  for (const issue of Object.values(state.issues)) {
    const sent = folded.get(issue.key);
    expect(sent?.phase).toBe(issue.phase);
    if (issue.slot) expect(sent?.slot).toBe(issue.slot.index);
    const role = ROLE[issue.phase ?? ""];
    if (role !== undefined) expect(sent?.roles.get(role)).toBe("working");
  }
});

test("an hour of simulated moves leaves every running issue running, in its slot, with a turn", () => {
  const state = replay();
  let tick = 0;
  const model = new FakeDaemonModel(structuredClone(state), {
    now: () => new Date(Date.parse("2026-11-10T00:00:00Z") + tick * 3000),
    random: (() => {
      let seed = 7;
      return () => {
        seed = (seed * 16807) % 2147483647;
        return seed / 2147483647;
      };
    })(),
  });
  const boot = model.snapshotEvents();
  // One tick every 3 s for an hour.
  for (; tick < 1200; tick++) model.simulate();
  const folded = fold([...boot, ...model.kept]);

  const running = Object.values(state.issues).filter((issue) =>
    RUNNING.includes(issue.phase as (typeof RUNNING)[number])
  );
  let moved = 0;
  for (const issue of running) {
    const now = model.state.issues[issue.key];
    expect(RUNNING).toContain(now?.phase as (typeof RUNNING)[number]);
    expect(now?.slot?.index).toBe(issue.slot?.index);
    const sent = folded.get(issue.key);
    expect(sent?.phase).toBe(now?.phase);
    const role = ROLE[now?.phase ?? ""] ?? "";
    expect(sent?.roles.get(role)).toBe("working");
    if (now?.phase !== issue.phase) moved++;
  }
  // The issues moved: the board is live, not frozen.
  expect(moved).toBeGreaterThan(running.length / 2);
  // Nothing outside the running phases moved, and none finished.
  for (const issue of Object.values(state.issues)) {
    if (!RUNNING.includes(issue.phase as (typeof RUNNING)[number])) {
      expect(model.state.issues[issue.key]?.phase).toBe(issue.phase);
    }
  }
});
