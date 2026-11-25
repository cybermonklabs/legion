import { describe, expect, test } from "bun:test";
import projectTokens from "../fixtures/project-tokens.json";
import {
  controllerToken,
  isStepRole,
  LEGION_ROLES,
  legionProjectToken,
  parseRoleToken,
  roleToken,
  roleTopic,
  sanitizeToken,
} from "./legion-roles";

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;

describe("role token grammar", () => {
  test("matches the injective Legion token grammar", () => {
    const token = roleToken("omp", "LEGION-42", "implementer");

    expect(token).toBe("legion-omp-legion-42-implementer");
    expect(roleTopic(token)).toBe("notifications.role.legion-omp-legion-42-implementer");
  });

  test("round-trips the complete issue key", () => {
    const issue = "LEGION-42";
    const token = roleToken("omp", issue, "implementer");

    expect(parseRoleToken("omp", token)).toEqual({
      project: "omp",
      issue,
      role: "implementer",
    });
  });

  test("encodes and round-trips a Dispatch issue key", () => {
    const token = roleToken("legion", "LEGION-7", "architect");

    expect(token).toBe("legion-legion-legion-7-architect");
    expect(parseRoleToken("legion", token)).toEqual({
      project: "legion",
      issue: "LEGION-7",
      role: "architect",
    });
  });

  test("rejects an invalid Dispatch issue key", () => {
    expect(() => roleToken("omp", "sjawhar/legion#42", "implementer")).toThrow(
      "Invalid IssueKey: sjawhar/legion#42"
    );
  });

  test("round-trips the controller token", () => {
    const token = controllerToken("omp");

    expect(token).toBe("legion-omp-controller");
    expect(parseRoleToken("omp", token)).toEqual({ controller: true });
  });

  test("rejects invalid mint projects loudly", () => {
    expect(() => roleToken("omp-tool", "LEGION-42", "implementer")).toThrow(
      "Invalid Legion project token"
    );
  });

  // A token's last part, the agent's name, is read by its fields since the roles of a deployment
  // (contract 258), so a token that ends in no Legion role but in a name of the step grammar,
  // `operator`, parses with that name as its `role`.
  test("rejects a token for another project, the controller as an issue's role, or a role outside the grammar", () => {
    expect(parseRoleToken("other", controllerToken("omp"))).toBeUndefined();
    expect(parseRoleToken("omp", "legion-omp-legion-42-operator")).toEqual({
      project: "omp",
      issue: "LEGION-42",
      role: "operator",
    });
    expect(parseRoleToken("omp", "legion-omp-legion-42-controller")).toBeUndefined();
    expect(parseRoleToken("omp", `legion-omp-legion-42-${"a".repeat(33)}`)).toBeUndefined();
    expect(parseRoleToken("omp", "legion-omp-legion-42-")).toBeUndefined();
  });

  test("a role token of a deployment role parses by its fields", () => {
    expect(parseRoleToken("acme", "legion-acme-acme-1-code-checker")).toEqual({
      project: "acme",
      issue: "ACME-1",
      role: "code-checker",
    });
    // The Go daemon's claim.NewToken spells the same token for the same role.
    expect(roleToken("acme", "ACME-1", "checker")).toBe("legion-acme-acme-1-checker");
    expect(() => roleToken("acme", "ACME-1", "controller")).toThrow(
      "Invalid Legion role: controller"
    );
  });
});

describe("isStepRole", () => {
  test("isStepRole is each role of the step grammar but the controller", () => {
    for (const role of ["implementer", "merger", "checker", "code-checker"]) {
      expect(isStepRole(role), role).toBeTrue();
    }
    for (const role of ["controller", "Checker", "", "a".repeat(33)]) {
      expect(isStepRole(role), role).toBeFalse();
    }
  });
});

// The Go daemon's claim.ProjectToken reads the same fixture: a project an operator writes names one
// controller role under either daemon and in the plugin that compares them.
describe("legionProjectToken", () => {
  test("spells every project as fixtures/project-tokens.json does", () => {
    for (const [project, token] of Object.entries(projectTokens.tokens)) {
      expect(legionProjectToken(project, "project")).toBe(token);
    }
  });

  test("refuses a project with no alphanumeric character, naming the field", () => {
    for (const project of projectTokens.refused) {
      expect(() => legionProjectToken(project, "project")).toThrow(
        "project must include at least one alphanumeric character"
      );
    }
  });
});

describe("sanitizeToken", () => {
  test("lowercases and normalizes dots, uppercase letters, and underscores", () => {
    expect(sanitizeToken("OMP.Project_Name")).toBe("omp-project-name");
    expect(sanitizeToken("...---___")).toBe("x");
  });

  test("always produces an envoy-valid token from fuzzed inputs", () => {
    let seed = 0x12345678;
    const next = () => {
      seed = (seed * 1664525 + 1013904223) >>> 0;
      return seed;
    };
    const randomTokenPart = () => {
      const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-";
      const length = (next() % 32) + 1;
      let value = "";
      for (let index = 0; index < length; index += 1) {
        value += alphabet[next() % alphabet.length];
      }
      return value;
    };
    const randomDispatchProject = () => {
      const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
      const length = (next() % 10) + 1;
      let value = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"[next() % 26];
      for (let index = 1; index < length; index += 1) {
        value += alphabet[next() % alphabet.length];
      }
      return value;
    };

    for (let index = 0; index < 1_000; index += 1) {
      const issue = `${randomDispatchProject()}-${(next() % 10_000) + 1}`;
      const role = LEGION_ROLES[next() % LEGION_ROLES.length];
      const token = roleToken("omp", issue, role);

      expect(sanitizeToken(randomTokenPart())).toMatch(ENVOY_ROLE_TOKEN);
      expect(token).toMatch(ENVOY_ROLE_TOKEN);
    }
  });
});
