import { describe, expect, test } from "bun:test";
import { claimProject, roleClaimKey, roleProject } from "./subject-project";

// The Go twin (packages/envoy/internal/contracts/subject_project.go RoleProject) reads a role as
// legion-<token>-<anything non-empty>, and so answers INFRA for each of these names. The TS reader's
// LEGION_ROLE_TOKEN ends in `.`, which matches no line terminator, so it answers "_" and the two
// twins of KC1 decide one claim two ways: a differential run over 539,475 (role, requested) pairs at
// f565979e50 found these three role names, and nothing else, on which they disagree (51 claim pairs,
// 3 roleProject answers). No route reaches them today, since roles.PatternString refuses the name,
// and nothing calls ClaimProject in this slice (the listener's route decides with RoleProject), but
// the twins of RoleProject, ClaimProject and RoleClaimKey must answer every input alike before
// t-c-8 makes them the decision.
describe("a role name with a line terminator after its Legion token", () => {
  for (const role of ["legion-infra-\n", "legion-infra-\r", "legion-infra-\u2028"]) {
    test(JSON.stringify(role), () => {
      expect(roleProject(role)).toBe("INFRA");
      expect(claimProject(role, "")).toBe("INFRA");
      expect(claimProject(role, "PRIV")).toBeInstanceOf(Error);
      expect(roleClaimKey("INFRA", role)).toBe(role);
    });
  }
});
