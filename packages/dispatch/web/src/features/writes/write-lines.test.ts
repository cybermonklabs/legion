import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { answerRoute } from "../../__tests__/send-answers";
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
 * and `useKeyedWrite` call of `web/src` that the app ships and names any that carries neither,
 * and it reads the lines those calls name and holds them to the grammar `<row type>:<row id>`.
 */

/** The app's own source: every module under `web/src` but the tests and their fixtures. */
function shippedSources(): readonly string[] {
  const root = join(import.meta.dir, "..", "..");
  return readdirSync(root, { recursive: true })
    .map((entry) => String(entry))
    .filter(
      (path) =>
        (path.endsWith(".ts") || path.endsWith(".tsx")) &&
        !path.includes(".test.") &&
        !path.startsWith("__tests__/")
    )
    .map((path) => path.replaceAll("\\", "/"))
    .sort();
}

/** The object literal that starts at `open` in `source`, read as JavaScript reads it: a brace in
 *  a string, a template, a comment or a template's `${…}` is not a brace of the object. */
function objectAt(source: string, open: number, where: string): string {
  let depth = 0;
  const templates: number[] = [];
  let mode: "code" | "line" | "block" | "'" | '"' | "`" = "code";
  for (let index = open; index < source.length; index += 1) {
    const ch = source[index] ?? "";
    const next = source[index + 1] ?? "";
    if (mode === "line") {
      if (ch === "\n") mode = "code";
      continue;
    }
    if (mode === "block") {
      if (ch === "*" && next === "/") {
        mode = "code";
        index += 1;
      }
      continue;
    }
    if (mode === "'" || mode === '"') {
      if (ch === "\\") index += 1;
      else if (ch === mode) mode = "code";
      continue;
    }
    if (mode === "`") {
      if (ch === "\\") index += 1;
      else if (ch === "`") mode = "code";
      else if (ch === "$" && next === "{") {
        templates.push(depth);
        depth += 1;
        index += 1;
        mode = "code";
      }
      continue;
    }
    if (ch === "/" && next === "/") {
      mode = "line";
      index += 1;
    } else if (ch === "/" && next === "*") {
      mode = "block";
      index += 1;
    } else if (ch === "'" || ch === '"' || ch === "`") {
      mode = ch;
    } else if (ch === "{") {
      depth += 1;
    } else if (ch === "}") {
      depth -= 1;
      if (templates.at(-1) === depth) {
        templates.pop();
        mode = "`";
      } else if (depth === 0) {
        return source.slice(open, index + 1);
      }
    }
  }
  throw new Error(`${where}: the write's object literal never closes`);
}

/** Whether each index of `source` is code: a `useKeyedWrite` a doc comment names is no write. */
function codeMask(source: string): readonly boolean[] {
  const mask: boolean[] = [];
  let mode: "code" | "line" | "block" | "'" | '"' | "`" = "code";
  for (let index = 0; index < source.length; index += 1) {
    const ch = source[index] ?? "";
    const next = source[index + 1] ?? "";
    mask.push(mode === "code");
    if (mode === "line") {
      if (ch === "\n") mode = "code";
      continue;
    }
    if (mode === "block") {
      if (ch === "*" && next === "/") {
        mode = "code";
        mask.push(false);
        index += 1;
      }
      continue;
    }
    if (mode === "'" || mode === '"' || mode === "`") {
      if (ch === "\\") {
        mask.push(false);
        index += 1;
      } else if (ch === mode) mode = "code";
      continue;
    }
    if (ch === "/" && next === "/") mode = "line";
    else if (ch === "/" && next === "*") mode = "block";
    else if (ch === "'" || ch === '"' || ch === "`") mode = ch;
  }
  return mask;
}

/** One write a module makes: where it is written, and the object its press names. */
interface WriteCall {
  readonly at: string;
  readonly spec: string;
}

/** Every `writes.run({…})`, `useKeyedWrite({…})` and `useSendMarkOnce({…})` of `source`, generic
 *  forms (`useKeyedWrite<Answer, Variables>({…})`) among them. A `.run()` that is not one of
 *  these - a keymap row's, a session switch's - names no object literal and is no write of the
 *  store. */
function writeCalls(path: string, source: string): readonly WriteCall[] {
  const calls: WriteCall[] = [];
  const code = codeMask(source);
  const names = /useKeyedWrite|useSendMarkOnce|\.run\b/gu;
  for (const found of source.matchAll(names)) {
    const start = found.index ?? 0;
    // The hooks' own declarations are no calls of them: their parameter lists are destructured
    // objects, which read like a press from here.
    if (
      code[start] !== true ||
      source.slice(Math.max(0, start - 10), start).includes("function ")
    ) {
      continue;
    }
    const end = start + found[0].length;
    const ahead = source.slice(end, end + 800);
    const call = ahead.indexOf("({");
    if (call === -1) continue;
    const between = ahead.slice(0, call).trim();
    const generic = between === "" || (between.startsWith("<") && between.endsWith(">"));
    if (!generic || between.includes("(") || between.includes(")")) continue;
    const line = source.slice(0, end).split("\n").length;
    calls.push({
      at: `${path}:${line}`,
      spec: objectAt(source, end + call + 1, `${path}:${line}`),
    });
  }
  return calls;
}

/** The field of `spec` named `name`, as its text, or undefined where the object has none; a
 *  field written in shorthand (`unlined,`) is its own name. */
function field(spec: string, name: string): string | undefined {
  const at = new RegExp(`(^|[{,\\s])${name}\\s*[:,}]`, "u").exec(spec);
  if (at === null) return undefined;
  if (!at[0].trimEnd().endsWith(":")) return name;
  return spec.slice(at.index + at[0].length).trim();
}

/** The line a `target` expression names, as a template with its row id written `<id>`, or
 *  undefined where the expression is no literal (a helper, or a value its caller handed in). */
function lineOf(target: string): string | undefined {
  const body = /^\([^)]*\)\s*=>\s*([\s\S]*)$/u.exec(target)?.[1] ?? target;
  const literal = /^`([^`]*)`|^"([^"]*)"/u.exec(body.trim());
  if (literal === undefined || literal === null) return undefined;
  return (literal[1] ?? literal[2] ?? "").replaceAll(/\$\{[^}]*\}/gu, "<id>");
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
// whose spec is built from a spread, or made through a helper of its own.
test("every write of web/src names its row or says why it names none", () => {
  expect(calls.length).toBeGreaterThan(30);
  const silent = calls
    .filter(
      ({ spec }) => field(spec, "target") === undefined && field(spec, "unlined") === undefined
    )
    .map(({ at }) => at);
  expect(silent).toEqual([]);
});

// R6. Every line the app names is `<row type>:<row id>`, a read mark's `read:` and then a row's
// own name, or one of the sends that name no row. The table is the lines themselves: a write
// that invents a second line for a row it already has - the `ask-snooze:<id>` this change
// closed - shows up here as a line nobody expected.
test("the lines web/src names are the rows of the grammar", () => {
  const lines = new Set<string>();
  const built: string[] = [];
  for (const { at, spec } of calls) {
    const target = field(spec, "target");
    if (target === undefined) continue;
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
  // callers' marks are in the table above), and one a helper builds for every write of its row.
  // Named by their file and not by their line, so an edit anywhere above them does not rewrite
  // this row; what it holds is which call sites build a line rather than write it out.
  expect(built.map((at) => at.replace(/:\d+$/, "")).sort()).toEqual([
    "features/architecture/ArchitecturePage.tsx",
    "features/conversation/held-sends.ts",
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
