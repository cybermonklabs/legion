import { expect, test } from "bun:test";

import { agentRoleWord } from "./agent-role";

// dispatch://INFRA-1196 ruling 6: a row names an agent by its role's word, as the approved
// pictures do, and not by its session's whole title.

test("a Legion role's title reads as the role's word", () => {
  expect(agentRoleWord("Legion architect · INFRA-1196")).toBe("architect");
  expect(agentRoleWord("Legion controller · INFRA")).toBe("controller");
  expect(agentRoleWord("Legion implementer")).toBe("implementer");
});

test("what a title names its role for is left to the opened body", () => {
  expect(agentRoleWord("Planner (issue layout)")).toBe("Planner");
  expect(agentRoleWord("Reviewer - pull request 2813")).toBe("Reviewer");
  expect(agentRoleWord("Tester: the preview")).toBe("Tester");
  expect(agentRoleWord("  Design research  ")).toBe("Design research");
});

test("the Legion daemon keeps its whole title, and a title of no role's word is itself", () => {
  expect(agentRoleWord("Legion daemon · INFRA")).toBe("Legion daemon · INFRA");
  expect(agentRoleWord("(untitled)")).toBe("(untitled)");
  expect(agentRoleWord("An agent")).toBe("An agent");
});
