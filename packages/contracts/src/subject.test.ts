import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { PROJECT_KEY_PATTERN } from "./dispatch-tools";
import {
  agentSubject,
  dispatchIssueSubject,
  legacyAgentSubject,
  legacyDispatchIssueSubject,
  legionControllerNoticeSubject,
  legionWakeSubject,
  NO_PROJECT,
  roleSubject,
  uncapturedTopicWarning,
} from "./subject";

test("builds the Go daemon's controller notice topic, byte for byte notify.ControllerTopic", () => {
  expect(legionControllerNoticeSubject("legion")).toBe("notifications.legion.legion.controller");
});

test("the listener's warning for an uncaptured topic says it is registered and waits for a stream subject", () => {
  // The listener's warning is generated from this builder (listenerwire.UncapturedTopicWarning in
  // packages/envoy/listenerwire/envelope_generated.go), which `bun run check:go` holds to it; this
  // pins the sentence itself. It must not say the topic was left out: the listener keeps it.
  expect(uncapturedTopicWarning("notifications.lxspec.probe", "ses_a")).toBe(
    "notifications.lxspec.probe is registered, but no stream subject captures it yet, so nothing is delivered to ses_a on it until one does; to hear what agents publish, follow a notifications.team.<name> topic"
  );
});

interface BuilderCase {
  readonly fn: string;
  readonly args: readonly string[];
  readonly want: string;
}

// The builders' rows of fixtures/subject-project.json, which the Go twins of these builders run
// too (packages/envoy/internal/contracts/subject_project_test.go), so a name both sides build is
// one name.
const builders: Record<string, (...args: string[]) => string> = {
  dispatchIssueSubject: (project = "", key = "", type = "") =>
    dispatchIssueSubject(project, key, type),
  agentSubject: (project = "", session = "") => agentSubject(project, session),
  legacyAgentSubject: (session = "") => legacyAgentSubject(session),
  roleSubject: (project = "", role = "") => roleSubject(project, role),
  legionWakeSubject: (token = "") => legionWakeSubject(token),
};

const fixture = JSON.parse(
  readFileSync(path.join(import.meta.dir, "../fixtures/subject-project.json"), "utf8")
) as { builders: BuilderCase[] };

test("each builder of a project's message names writes the shared fixture's name", () => {
  // Seven rows when this was written: a read that finds none passes while covering nothing.
  expect(fixture.builders.length).toBeGreaterThanOrEqual(7);
  for (const row of fixture.builders) {
    const build = builders[row.fn];
    expect(build, `the fixture names a builder, ${row.fn}, this test does not know`).toBeDefined();
    expect(build?.(...row.args)).toBe(row.want);
  }
});

test("the old issue form names the key alone, the new one its project first", () => {
  expect(legacyDispatchIssueSubject("INFRA-12", ">")).toBe(
    "notifications.dispatch.issue.INFRA-12.>"
  );
  expect(dispatchIssueSubject("INFRA", "INFRA-12", ">")).toBe(
    "notifications.dispatch.issue.INFRA.INFRA-12.>"
  );
});

test("no project is a token no project key can be", () => {
  expect(NO_PROJECT).toBe("_");
  expect(PROJECT_KEY_PATTERN.test(NO_PROJECT)).toBe(false);
});
