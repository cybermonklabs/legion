import { describe, expect, test } from "bun:test";
import type { Actor } from "@legion/contracts";
import { actorText } from "../actor-text";

describe("actorText", () => {
  test("a person's own write is the person, and their agent's write names the agent", () => {
    const person: Actor = { kind: "user", id: "ana@example.com" };
    expect(actorText(person)).toBe("user ana@example.com");
    expect(actorText({ ...person, act: { session: "0123456789abcdef" } })).toBe(
      "user ana@example.com, acting through session 0123456789abcdef"
    );
    expect(actorText({ ...person, act: { token_id: "t-1", token_name: "architect" } })).toBe(
      "user ana@example.com, acting through token architect"
    );
  });

  test("an item's history names the command bar for a change of a run", () => {
    const person: Actor = { kind: "user", id: "ana@example.com" };
    const bar = { agent: "command-bar", run: "0190c7d2-1111-7222-8333-444444444444" };
    expect(actorText({ ...person, act: { ...bar, scopes: ["person", "approve"] } })).toBe(
      "user ana@example.com, through the command bar"
    );
    expect(
      actorText({
        ...person,
        act: { ...bar, scopes: ["person"], token_id: "t-1", token_name: "architect" },
      })
    ).toBe("user ana@example.com, through the command bar with token architect");
  });

  test("a session keeps its service subject, and a system actor its id", () => {
    expect(actorText({ kind: "session", id: "ses-1" })).toBe("session ses-1");
    expect(
      actorText({
        kind: "session",
        id: "ses-1",
        service: "system:serviceaccount:legion:runner",
      })
    ).toBe("session ses-1 (as legion/runner)");
    expect(actorText({ kind: "system", id: "architecture-importer" })).toBe(
      "system architecture-importer"
    );
  });
});
