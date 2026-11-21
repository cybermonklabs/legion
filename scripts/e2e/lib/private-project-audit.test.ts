import { describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  auditConnection,
  auditConsumerInfo,
  auditControllerPods,
  auditFinishedPodDeleted,
  namesProject,
} from "./private-project-audit";

// private-project-audit.ts (the audit private-project-devbox.sh runs) against recorded inputs: each
// judgement passes on a clean record, fails naming what it found on a record holding the fault it
// is for, and says UNKNOWN on a record that holds nothing to judge, so an empty read never passes.

const PRIV = "PRIVSCRATCH";
const markers = ["privscratch-marker-0a1b", "ses-private-architect"];
const lines = (...rows: unknown[]): string =>
  `${rows.map((row) => JSON.stringify(row)).join("\n")}\n`;
const publicRecord = [
  { subject: "notifications.dispatch.issue.PUBSCRATCH.PUBSCRATCH-1.comment.created", data: "{}" },
  { subject: "notifications.agent.PUBSCRATCH.ses-public", data: { body: "a word" } },
  { subject: "notifications.legion.pubscratch.wake", data: "role_message" },
];

describe("namesProject", () => {
  test("reads the key as a subject token, an issue key, the wake lane or a marker", () => {
    expect(
      namesProject({ subject: `notifications.dispatch.issue.${PRIV}.${PRIV}-3.x` }, PRIV, [])
    ).toBe(true);
    expect(namesProject({ subject: `notifications.dispatch.issue.${PRIV}-3.x` }, PRIV, [])).toBe(
      true
    );
    expect(namesProject({ subject: `notifications.role.${PRIV}.legion-architect` }, PRIV, [])).toBe(
      true
    );
    expect(namesProject({ subject: "notifications.legion.privscratch.wake" }, PRIV, [])).toBe(true);
    expect(
      namesProject(
        { subject: "notifications.legion.wake", data: "privscratch-marker-0a1b" },
        PRIV,
        markers
      )
    ).toBe(true);
  });

  test("does not read another key that opens with the same letters as the project", () => {
    expect(
      namesProject({ subject: `notifications.dispatch.issue.${PRIV}X.${PRIV}X-1.x` }, PRIV, [])
    ).toBe(false);
  });
});

describe("auditConnection", () => {
  test("a record of the other project alone passes", () => {
    const verdict = auditConnection("public daemon", lines(...publicRecord), PRIV, markers);
    expect(verdict.kind).toBe("PASS");
    expect(verdict.line).toBe(
      `PASS public daemon: 3 messages read, none of ${PRIV} (0 GitHub events not judged)`
    );
  });

  test("a message of the project fails, quoting it", () => {
    const leak = { subject: `notifications.role.${PRIV}.legion-architect`, data: "{}" };
    const verdict = auditConnection("public daemon", lines(...publicRecord, leak), PRIV, markers);
    expect(verdict.kind).toBe("FAIL");
    expect(verdict.line).toContain(`notifications.role.${PRIV}.legion-architect`);
  });

  test("a message on another subject that carries a marker of the project fails", () => {
    const leak = {
      subject: "notifications.legion.wake",
      data: { session: "ses-private-architect" },
    };
    const verdict = auditConnection("public session", lines(...publicRecord, leak), PRIV, markers);
    expect(verdict.kind).toBe("FAIL");
    expect(verdict.line).toContain("ses-private-architect");
  });

  test("the scratch repository's GitHub events reach both daemons and are not judged", () => {
    const github = {
      subject: "notifications.github.acme.scratch.pull_request.4.opened",
      data: `${PRIV}-1`,
    };
    const verdict = auditConnection("public daemon", lines(...publicRecord, github), PRIV, markers);
    expect(verdict.kind).toBe("PASS");
    expect(verdict.line).toContain("1 GitHub events not judged");
  });

  test("an empty, missing, unreadable or GitHub-only record is UNKNOWN, naming the connection", () => {
    expect(auditConnection("private daemon", "", PRIV, markers)).toEqual({
      kind: "UNKNOWN",
      line: "UNKNOWN private daemon: its record is empty, so it read nothing",
    });
    expect(auditConnection("private daemon", undefined, PRIV, markers).kind).toBe("UNKNOWN");
    expect(auditConnection("private daemon", "not json\n", PRIV, markers).line).toBe(
      "UNKNOWN private daemon: line 1 of its record is not JSON"
    );
    const github = { subject: "notifications.github.acme.scratch.push", data: "{}" };
    expect(auditConnection("private daemon", lines(github), PRIV, markers).line).toBe(
      "UNKNOWN private daemon: it read no message of any project (1 GitHub events, which name none)"
    );
  });
});

const pod = (name: string, namespace: string, account: string, phase = "Running") => ({
  metadata: { name, namespace },
  spec: { serviceAccountName: account },
  status: { phase },
});

describe("auditControllerPods", () => {
  const namespace = "legion-privscratch";
  const account = "legion-privscratch-controller";

  test("a controller pod under the project's account in its namespace passes", () => {
    const listed = JSON.stringify({ items: [pod("controller-0", namespace, account)] });
    expect(auditControllerPods(listed, namespace, account)).toEqual({
      kind: "PASS",
      line: `PASS controller pod: controller-0 runs as ${account} in ${namespace}`,
    });
  });

  test("a controller pod under another account, or in another namespace, fails naming it", () => {
    const shared = JSON.stringify({ items: [pod("controller-0", "legion", "legion-controller")] });
    expect(auditControllerPods(shared, namespace, account).line).toBe(
      `FAIL controller pod: controller-0 runs as legion-controller in legion, want ${account} in ${namespace}`
    );
    const borrowed = JSON.stringify({ items: [pod("controller-0", namespace, "legion-daemon")] });
    expect(auditControllerPods(borrowed, namespace, account).kind).toBe("FAIL");
  });

  test("no controller pod read is UNKNOWN", () => {
    expect(auditControllerPods(JSON.stringify({ items: [] }), namespace, account).line).toBe(
      "UNKNOWN controller pod: no controller pod was read"
    );
    expect(auditControllerPods(undefined, namespace, account).kind).toBe("UNKNOWN");
  });
});

describe("auditConsumerInfo", () => {
  const reading = (durable: string, filters: string[], delivered: number) =>
    JSON.stringify({ durable, filters, delivered, ack_pending: 0, num_pending: 0 });
  const before = [
    reading("legion-go-PUBSCRATCH-dispatch", ["notifications.dispatch.issue.PUBSCRATCH.>"], 10),
    reading("legion-go-PUBSCRATCH-github", ["notifications.github.>"], 4),
  ].join("\n");

  test("its own filters and a delivered count that moved pass", () => {
    const after = [
      reading("legion-go-PUBSCRATCH-dispatch", ["notifications.dispatch.issue.PUBSCRATCH.>"], 12),
      reading("legion-go-PUBSCRATCH-github", ["notifications.github.>"], 4),
    ].join("\n");
    expect(auditConsumerInfo("public daemon consumers", before, after, "PUBSCRATCH", PRIV)).toEqual(
      {
        kind: "PASS",
        line: "PASS public daemon consumers: 2 consumers filter PUBSCRATCH's subjects or none's; legion-go-PUBSCRATCH-dispatch delivered more during the run",
      }
    );
  });

  test("a filter naming the private project fails, naming the consumer", () => {
    const after = reading(
      "legion-go-PUBSCRATCH-dispatch",
      [`notifications.dispatch.issue.${PRIV}.>`],
      12
    );
    expect(
      auditConsumerInfo("public daemon consumers", before, after, "PUBSCRATCH", PRIV).line
    ).toBe(
      `FAIL public daemon consumers: legion-go-PUBSCRATCH-dispatch filters notifications.dispatch.issue.${PRIV}.>, which is not PUBSCRATCH's`
    );
  });

  test("a count that never moved fails, and no reading is UNKNOWN", () => {
    expect(
      auditConsumerInfo("public daemon consumers", before, before, "PUBSCRATCH", PRIV).kind
    ).toBe("FAIL");
    expect(auditConsumerInfo("public daemon consumers", before, "", "PUBSCRATCH", PRIV).kind).toBe(
      "UNKNOWN"
    );
    expect(
      auditConsumerInfo("public daemon consumers", before, undefined, "PUBSCRATCH", PRIV).kind
    ).toBe("UNKNOWN");
  });
});

describe("auditFinishedPodDeleted", () => {
  const namespace = "legion-privscratch";
  const account = "legion-privscratch-controller";
  const controller = `system:serviceaccount:${namespace}:${account}`;
  const deleted = (name: string, phase: string, by: string) => ({
    type: "DELETED",
    object: pod(name, namespace, "legion-privscratch-pod", phase),
    deleted_by: by,
  });

  test("a finished pod of the project the controller deleted passes", () => {
    const watched = lines(
      { type: "ADDED", object: pod("issue-1", namespace, "legion-privscratch-pod") },
      deleted("issue-1", "Succeeded", controller)
    );
    expect(auditFinishedPodDeleted(watched, namespace, account)).toEqual({
      kind: "PASS",
      line: `PASS finished pod deleted: ${account} deleted issue-1, a finished pod of ${namespace}`,
    });
  });

  test("a deletion by another principal, or of a running pod, does not count", () => {
    const watched = lines(
      deleted("issue-1", "Succeeded", "system:serviceaccount:legion:legion-controller"),
      deleted("issue-2", "Running", controller)
    );
    const verdict = auditFinishedPodDeleted(watched, namespace, account);
    expect(verdict.kind).toBe("FAIL");
    expect(verdict.line).toContain("issue-1 by system:serviceaccount:legion:legion-controller");
  });

  test("a run that watched no deletion fails naming the controller", () => {
    expect(auditFinishedPodDeleted("", namespace, account).line).toBe(
      `FAIL finished pod deleted: ${account} deleted no finished pod of ${namespace} during the run (0 deletions watched)`
    );
  });
});

describe("the command", () => {
  test("prints one line per judgement and exits 1 when any is not PASS", async () => {
    const dir = mkdtempSync(join(tmpdir(), "private-project-audit-"));
    writeFileSync(join(dir, "public.jsonl"), lines(...publicRecord));
    writeFileSync(join(dir, "private.jsonl"), "");
    writeFileSync(join(dir, "markers-priv"), `${markers.join("\n")}\n`);
    writeFileSync(join(dir, "markers-pub"), "PUBSCRATCH-1\n");
    writeFileSync(
      join(dir, "pods.json"),
      JSON.stringify({
        items: [pod("controller-0", "legion-privscratch", "legion-privscratch-controller")],
      })
    );
    writeFileSync(join(dir, "deletions.jsonl"), "");
    const run = Bun.spawn(
      [
        "bun",
        join(import.meta.dir, "private-project-audit.ts"),
        "--connection",
        `public daemon,${join(dir, "public.jsonl")},${PRIV},${join(dir, "markers-priv")}`,
        "--connection",
        `private daemon,${join(dir, "private.jsonl")},PUBSCRATCH,${join(dir, "markers-pub")}`,
        "--controller-pods",
        join(dir, "pods.json"),
        "--deletions",
        join(dir, "deletions.jsonl"),
        "--namespace",
        "legion-privscratch",
        "--controller-account",
        "legion-privscratch-controller",
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    const printed = (await new Response(run.stdout).text()).trim().split("\n");
    expect(await run.exited).toBe(1);
    expect(printed).toEqual([
      `PASS public daemon: 3 messages read, none of ${PRIV} (0 GitHub events not judged)`,
      "UNKNOWN private daemon: its record is empty, so it read nothing",
      "PASS controller pod: controller-0 runs as legion-privscratch-controller in legion-privscratch",
      "FAIL finished pod deleted: legion-privscratch-controller deleted no finished pod of legion-privscratch during the run (0 deletions watched)",
    ]);
  });
});
