import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import ts from "typescript";

import { answerRoute } from "../../__tests__/send-answers";
import { nodesOf, parsed, shippedSources, siteOf } from "../../__tests__/source-scan";
import type { Ask } from "../../api/types";
import { useOutstandingAnswerWrite } from "../inbox/outstanding-answers";
import { useAskSnooze } from "../inbox/useAskSnooze";
import { sourceLine } from "../settings/ArchitectureSourcesSection";
import { writesOf } from "./write-store";

/**
 * Every write of a row carries a line (dispatch://INFRA-299/spec#b-r6). The types ask each write
 * for one of two things - the row it writes (`WriteSpec.target`, `useKeyedWrite`'s `target`) or
 * the reason it names none (`unlined`, in its own value) - so a write that leaves both out does
 * not compile. Where a type cannot hold that door, this file does: it reads every `writes.run`
 * and `useKeyedWrite` call of `web/src` that the app ships, as the compiler parses it
 * (`source-scan.ts`), and names any that carries neither, and it reads the lines those calls name
 * and holds them to the grammar `<row type>:<row id>`. A call may spread its line in rather than
 * write it out, as `...(<value> satisfies WriteLine)`: the compiler holds that value to
 * `WriteLine` at the call itself, so that call is one a type reaches, and it is read as a call
 * that names its line.
 */

/** One write a module makes: where it is written, and the object its press names. */
interface WriteCall {
  readonly at: string;
  readonly spec: ts.ObjectLiteralExpression;
  readonly file: ts.SourceFile;
}

/** Every `writes.run({…})`, `useKeyedWrite({…})` and `useSendMarkOnce({…})` of `source`, generic
 *  forms (`useKeyedWrite<Answer, Variables>({…})`) among them. A `.run()` that names no object
 *  literal - a keymap row's, a session switch's - is no write of the store. */
function writeCalls(path: string, source: string): readonly WriteCall[] {
  const file = parsed(path, source);
  return nodesOf(file).flatMap((node) => {
    if (!ts.isCallExpression(node)) return [];
    const callee = node.expression;
    const name = ts.isIdentifier(callee)
      ? callee
      : ts.isPropertyAccessExpression(callee) && callee.name.text === "run"
        ? callee.name
        : undefined;
    if (name === undefined || !["useKeyedWrite", "useSendMarkOnce", "run"].includes(name.text)) {
      return [];
    }
    const [spec] = node.arguments;
    if (spec === undefined || !ts.isObjectLiteralExpression(spec)) return [];
    return [{ at: siteOf(file, name), file, spec }];
  });
}

/** The field of `spec` named `name`, as the text of its value, or undefined where the object has
 *  none at its own level; a field written in shorthand (`unlined,`) is its own name. */
function field({ file, spec }: WriteCall, name: string): string | undefined {
  for (const property of spec.properties) {
    if (property.name === undefined || !ts.isIdentifier(property.name)) continue;
    if (property.name.text !== name) continue;
    if (ts.isShorthandPropertyAssignment(property)) return name;
    if (ts.isPropertyAssignment(property)) return property.initializer.getText(file);
    return property.getText(file);
  }
  return undefined;
}

/** The line a `target` expression names, as a template with its row id written `<id>`, or
 *  undefined where the expression is no literal (a helper, or a value its caller handed in). */
function lineOf(target: string): string | undefined {
  const body = /^\([^)]*\)\s*=>\s*([\s\S]*)$/u.exec(target)?.[1] ?? target;
  const literal = /^`([^`]*)`|^"([^"]*)"/u.exec(body.trim());
  if (literal === undefined || literal === null) return undefined;
  return (literal[1] ?? literal[2] ?? "").replaceAll(/\$\{[^}]*\}/gu, "<id>");
}

/** The value a spec spreads its line in from, where it spreads one as `...(<value> satisfies
 *  WriteLine)`. A spread written any other way names no line here, since nothing holds it to
 *  one. */
function spreadLine({ file, spec }: WriteCall): string | undefined {
  for (const property of spec.properties) {
    if (!ts.isSpreadAssignment(property)) continue;
    let value = property.expression;
    while (ts.isParenthesizedExpression(value)) value = value.expression;
    if (ts.isSatisfiesExpression(value) && value.type.getText(file) === "WriteLine") {
      return value.expression.getText(file);
    }
  }
  return undefined;
}

/** The sends that name no row, which R6 does not reach: they keep the names they have. */
const NAMES_NO_ROW = ["broadcast"];

/** Whether `line` follows the grammar: a row's own name, `<row type>:<row id>`, or a read mark,
 *  which is `read:` and then a row's own name. */
function namesARow(line: string): boolean {
  const row = line.startsWith("read:") ? line.slice("read:".length) : line;
  return /^[a-z][a-z-]*:\S+$/u.test(row);
}

const calls = shippedSources().flatMap((path) =>
  writeCalls(path, readFileSync(join(import.meta.dir, "..", "..", path), "utf8"))
);

// R6. A write that names a row carries its line; one that names none says so in its own value.
// The types hold every write to that, and this row holds the calls a type cannot reach - a write
// whose spec is built from a spread, or made through a helper of its own. A spread of a value
// the compiler holds to `WriteLine` at the call (`spreadLine`) is a call a type reaches.
test("every write of web/src names its row or says why it names none", () => {
  expect(calls.length).toBeGreaterThan(30);
  const silent = calls
    .filter(
      (call) =>
        field(call, "target") === undefined &&
        field(call, "unlined") === undefined &&
        spreadLine(call) === undefined
    )
    .map(({ at }) => at);
  expect(silent).toEqual([]);
});

// R6. Every line the app names is `<row type>:<row id>`, a read mark's `read:` and then a row's
// own name, or one of the sends that name no row. The table is the lines themselves: a write
// that invents a second line for a row it already has shows up here as a line nobody expected.
test("the lines web/src names are the rows of the grammar", () => {
  const lines = new Set<string>();
  const built: string[] = [];
  for (const call of calls) {
    const { at } = call;
    const target = field(call, "target");
    // A line spread in is built rather than written out, whichever of the two it holds.
    if (target === undefined) {
      if (spreadLine(call) !== undefined) built.push(at);
      continue;
    }
    const line = lineOf(target);
    if (line === undefined) built.push(at);
    else lines.add(line);
  }
  for (const line of lines) {
    if (NAMES_NO_ROW.includes(line)) continue;
    expect({ line, namesARow: namesARow(line) }).toEqual({ line, namesARow: true });
  }
  expect([...lines].sort()).toEqual([
    "agent-token:<id>",
    "agent-tokens:me",
    "ask:<id>",
    "broadcast",
    "comment:<id>",
    "document:<id>",
    "inbox:dismiss",
    "inbox:read",
    "issue:<id>",
    "pin:<id>",
    "pin:agent:<id>",
    "project:<id>",
    "read:agent:<id>",
    "read:document:<id>",
    "read:issue:<id>",
    "read:thread:<id>",
    "repo-mapping:<id>",
    "session:<id>",
    "settings:delivery",
  ]);
  // The lines no call writes out: one the hook's caller hands in (`useSendMarkOnce`, whose
  // callers' marks are in the table above), one a helper builds for every write of its row, one
  // spread in as a `WriteLine` its maker built (`held-sends.ts`, `offer.ts`, `useKeyedWrite.ts`),
  // and one the row's own kind picks between - `ThreadRowCard`'s **Mark read**, which marks an
  // Agent row on `read:agent:<id>` and every other row on `read:thread:<id>`, both of them in the
  // table above. Named by their file and not by their line, so an edit anywhere above them does
  // not rewrite this row; what it holds is which call sites build a line rather than write it
  // out.
  expect(built.map((at) => at.replace(/:\d+$/, "")).sort()).toEqual([
    "features/architecture/ArchitecturePage.tsx",
    "features/conversation/held-sends.ts",
    "features/inbox/ThreadRowCard.tsx",
    "features/search/offer.ts",
    "features/settings/ArchitectureSourcesSection.tsx",
    "features/settings/ArchitectureSourcesSection.tsx",
    "features/settings/ArchitectureSourcesSection.tsx",
    "features/writes/useKeyedWrite.ts",
    "features/writes/useSendMarkOnce.ts",
  ]);
  expect(sourceLine({ project: "CORE", repo: "legion/arch" })).toBe(
    "architecture-source:CORE/legion/arch"
  );
  expect(namesARow(sourceLine({ project: "CORE", repo: "legion/arch" }))).toBe(true);
});

// R6. One row stands on one line: the ask's snooze, which had a line of its own
// (`ask-snooze:<id>`), and the ask's answer are writes of the same row and wait in the same
// line, so the newer never lands before the older.
test("the ask snooze and the ask answer stand on one line", async () => {
  const held = Promise.withResolvers<Ask>();
  answerRoute("PUT", "/api/v1/me/asks/*/snooze").mockImplementation(() => held.promise);
  answerRoute("POST", "/api/v1/asks/*/answer").mockImplementation(() => held.promise);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(
    () => ({
      answer: useOutstandingAnswerWrite<Ask, { text: string }>({
        askId: "ask-1",
        label: () => "Answer to “Ship?”",
        request: () => ({ body: {}, method: "POST", path: "/api/v1/asks/ask-1/answer" }),
      }),
      snooze: useAskSnooze("ask-1"),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
  try {
    await act(async () => {
      hook.result.current.snooze.submit("2026-11-13T00:00:00Z");
      hook.result.current.answer.mutate({ text: "Ship it." });
    });
    expect(
      writesOf(client)
        .rows()
        .map((write) => write.target)
    ).toEqual(["ask:ask-1", "ask:ask-1"]);
  } finally {
    held.resolve({} as Ask);
    hook.unmount();
  }
});
