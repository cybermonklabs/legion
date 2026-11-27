import { expect, test } from "bun:test";

import { appHref, buildReferencePath, referenceHref } from "./routes";

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
  // Reading the dot segments out of these puts back the two slashes the leading-slash check
  // refused, and the result is the protocol-relative URL above by another spelling.
  ["a dot segment hiding two slashes", "/.//evil.example/x", undefined],
  ["a parent segment hiding two slashes", "/a/..//evil.example", undefined],
  ["an encoded dot segment hiding two slashes", "/%2e//evil.example", undefined],
])("%s: %p becomes %p", (_name, path, href) => {
  expect(appHref(path)).toBe(href);
});

// What it answers is built from the parsed URL's own parts, so a path that does qualify is
// written back whole: the percent-encoding a reference's segments carry survives it.
test("an encoded segment is written back as it came", () => {
  expect(appHref("/issues/CORE-1/comment/c%3B1")).toBe("/issues/CORE-1/comment/c%3B1");
});

// A person has no page, so there is no path to gate: `buildReferencePath` does not take one
// (`PagedReferenceRoute`) and the composition answers nothing, which is what leaves the words
// the author wrote standing where a link would be.
test("a person's reference is no link", () => {
  const person = { kind: "person" as const, login: "alice@example.com" };
  expect(referenceHref(person)).toBeUndefined();
  // @ts-expect-error a person opens no page, so the builder refuses the kind.
  buildReferencePath(person);
});

// The slug a pinned block of a spec opens its version under is the builder's own `specSlug`,
// carried through the gate here so the two hooks that wait for the issue's summary
// (`useReferenceHref`, `fetchReferenceHref`) make no composition of their own.
test("a pinned block of a spec opens its version under the slug it is given", () => {
  const spec = { block: { id: "p1", pin: "3" }, key: "CORE-1", kind: "spec" as const };
  expect(referenceHref(spec, "core-1-md")).toBe("/issues/CORE-1/artifacts/core-1-md?v=3");
  // Until the summary names the slug, the live spec at the block.
  expect(referenceHref(spec)).toBe("/issues/CORE-1/spec#b-p1");
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
