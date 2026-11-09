import { expect, test } from "bun:test";

import { appHref, buildReferencePath } from "./routes";

// The one gate between a reference a person typed and an `href` a browser follows
// (`RefLink.tsx`'s two writers and `features/doc/picture-view.ts`). Each row below is a string a
// leading-slash check alone, or an origin check alone, would let through.
test.each([
  ["a path of this dashboard", "/issues/CORE-1", "/issues/CORE-1"],
  ["a path with a query", "/agents?role=reviewer", "/agents?role=reviewer"],
  ["a path with a fragment", "/issues/CORE-1/spec#b-1", "/issues/CORE-1/spec#b-1"],
  // Two slashes are a protocol-relative URL: the browser keeps the page's scheme and goes to
  // evil.example.
  ["a protocol-relative URL", "//evil.example/x", undefined],
  // A backslash after the slash is what a leading-slash check misses: every browser reads
  // `/\evil.example/x` as `//evil.example/x`, and the URL parser resolves it to that origin.
  ["a slash and a backslash", "/\\evil.example/x", undefined],
  ["two backslashes", "\\\\evil.example/x", undefined],
  ["a script scheme", "javascript:alert(1)", undefined],
  ["a script scheme in capitals", "JavaScript:alert(1)", undefined],
  ["a data URL", "data:text/html,<img src=x onerror=alert(1)>", undefined],
  ["another origin", "https://evil.example/x", undefined],
  ["an absolute URL of a dashboard", "https://dispatch.test/issues/CORE-1", undefined],
  ["a relative path", "issues/CORE-1", undefined],
  ["nothing", "", undefined],
  ["a reference rather than a path", "dispatch://CORE-1", undefined],
  ["typed markup", '<img src=x onerror="alert(1)">', undefined],
])("%s: %p becomes %p", (_name, path, href) => {
  expect(appHref(path)).toBe(href);
});

// What it answers is built from the parsed URL's own parts, so a path that does qualify is
// written back whole: the percent-encoding a reference's segments carry survives it.
test("an encoded segment is written back as it came", () => {
  expect(appHref("/issues/CORE-1/comment/c%3B1")).toBe("/issues/CORE-1/comment/c%3B1");
});

// A person has no page, so `buildReferencePath` answers the empty path and the gate refuses it:
// the words the author wrote stand where a link would be.
test("a person's reference is no link", () => {
  const person = { kind: "person" as const, login: "alice@example.com" };
  expect(buildReferencePath(person)).toBe("");
  expect(appHref(buildReferencePath(person))).toBeUndefined();
});

// Every other kind this change adds opens a page of this dashboard, so each passes the gate.
test.each([
  { kind: "agent-session" as const, session: "01a1058e-f14f-7684-87eb-3dc885955551" },
  { kind: "role" as const, role: "reviewer" },
  { id: "dispatch-server", kind: "component" as const, project: "CORE" },
  { key: "CORE-1", kind: "issue" as const },
  { key: "CORE-1", kind: "spec" as const },
])("$kind opens a page of this dashboard", (route) => {
  const path = buildReferencePath(route);
  expect(appHref(path)).toBe(path);
});
