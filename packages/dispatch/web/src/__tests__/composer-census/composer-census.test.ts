/**
 * The composer census (INFRA-1503) held to the draft matrix: every item the census finds, of
 * every mount of a draft field (`MentionComposer`, the ask card's answer field and
 * `AskReplyComposer`), has an entry in a draft matrix file's coverage list, a row of that file or
 * one of its numbered reasons; every entry names an item the census still finds; a row names a
 * test of its file; and a reason has words. So a host that adds a conditional above its field, a
 * new mount, or a new form of the layout fails here until a matrix file answers for it. The
 * census fails closed: a mount it cannot place, or an element, a helper or a slot between a mount
 * and the routes it cannot read, fails here too, by where it is.
 *
 * The census reads the `web/src` this file is in, or the copy of it `COMPOSER_CENSUS_ROOT`
 * names, so that a change planted in a scratch copy is censused with this test unchanged. The
 * coverage lists and the matrix files' titles are always this tree's.
 */
import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { coverage as signIn } from "../../draft-matrix.sign-in.coverage";
import { coverage as agentsRow } from "../../features/agents/draft-matrix.agents-row.coverage";
import { coverage as docked } from "../../features/conversation/draft-matrix.docked.coverage";
import { coverage as edit } from "../../features/conversation/draft-matrix.edit.coverage";
import { coverage as inlineReply } from "../../features/conversation/draft-matrix.inline-reply.coverage";
import { coverage as issuePage } from "../../features/conversation/draft-matrix.issue-page.coverage";
import { coverage as issuePageOutline } from "../../features/conversation/draft-matrix.issue-page-outline.coverage";
import { coverage as phoneThread } from "../../features/conversation/draft-matrix.phone-thread.coverage";
import { coverage as ask } from "../../features/inbox/draft-matrix.ask.coverage";
import { coverage as messageBox } from "../../features/inbox/draft-matrix.message-box.coverage";
import { coverage as threadBox } from "../../features/inbox/draft-matrix.thread-box.coverage";
import { coverage as textComments } from "../../features/issue-page/text-comments/draft-matrix.text-comments.coverage";
import { coverage as margin } from "../../features/margin/draft-matrix.margin.coverage";
import { type CensusItem, composerCensus, keyParts } from "./census";
import type { CoverageEntry } from "./coverage";
import { testTitles } from "./test-titles";

const WEB_SRC = join(import.meta.dir, "..", "..");

/** The draft matrix files, under `web/src`, with their coverage lists. */
const MATRIX: readonly { readonly file: string; readonly entries: readonly CoverageEntry[] }[] = [
  { entries: signIn, file: "draft-matrix.sign-in.test.tsx" },
  { entries: agentsRow, file: "features/agents/draft-matrix.agents-row.test.tsx" },
  { entries: docked, file: "features/conversation/draft-matrix.docked.test.tsx" },
  { entries: edit, file: "features/conversation/draft-matrix.edit.test.tsx" },
  { entries: inlineReply, file: "features/conversation/draft-matrix.inline-reply.test.tsx" },
  { entries: issuePage, file: "features/conversation/draft-matrix.issue-page.test.tsx" },
  {
    entries: issuePageOutline,
    file: "features/conversation/draft-matrix.issue-page-outline.test.tsx",
  },
  { entries: phoneThread, file: "features/conversation/draft-matrix.phone-thread.test.tsx" },
  { entries: ask, file: "features/inbox/draft-matrix.ask.test.tsx" },
  { entries: messageBox, file: "features/inbox/draft-matrix.message-box.test.tsx" },
  { entries: threadBox, file: "features/inbox/draft-matrix.thread-box.test.tsx" },
  {
    entries: textComments,
    file: "features/issue-page/text-comments/draft-matrix.text-comments.test.tsx",
  },
  { entries: margin, file: "features/margin/draft-matrix.margin.test.tsx" },
];

const { items, unread } = composerCensus(process.env.COMPOSER_CENSUS_ROOT ?? WEB_SRC);
const byKey = new Map(items.map((item) => [item.key, item]));

const listOf = (file: string) => file.replace(/\.test\.tsx$/, ".coverage.ts");

/** An item as a failure names it: its host, the mount or component it is about, its kind and
 *  expression, and where it is. */
function named(item: CensusItem): string {
  const detail = item.detail === "" ? "" : `; ${item.detail}`;
  return `${item.host} · ${item.mount} · ${item.kind} · ${item.expression} (${item.at.join(", ")}${detail})`;
}

/** The matrix files an item belongs in. A file answers for a mount where it answers for that
 *  mount's lifetime, and for a place (a host's code, about one component) where it answers for an
 *  item there. So: the files answering at the same place above one of the same mounts that also
 *  answer for one of those mounts; else any file answering at the same place above one of the
 *  same mounts, as the issue page's file does above the Conversation's; else at the same place;
 *  else for one of the mounts. */
function homesOf(item: CensusItem): string[] {
  const filesWhere = (rule: (other: CensusItem) => boolean) =>
    MATRIX.filter(({ entries }) =>
      entries.some((entry) => {
        const other = byKey.get(entry.key);
        return other !== undefined && rule(other);
      })
    ).map(({ file }) => file);
  const samePlace = (other: CensusItem) => other.host === item.host && other.mount === item.mount;
  const sameMount = (other: CensusItem) => other.above.some((name) => item.above.includes(name));
  const placed = filesWhere((other) => samePlace(other) && sameMount(other));
  const owners = filesWhere((other) => other.kind === "lifetime" && sameMount(other));
  const both = placed.filter((file) => owners.includes(file));
  return [both, placed, filesWhere(samePlace), owners].find((files) => files.length > 0) ?? [];
}

/** An entry's item as a failure names it, read back from its key. */
function entryNamed(entry: CoverageEntry): string {
  const { expression, host, kind, mount } = keyParts(entry.key);
  return `${host} · ${mount} · ${kind} · ${expression}`;
}

test("the composer census places every mount and reads everything between a mount and the routes", () => {
  expect(unread).toEqual([]);
});

test("every item the composer census finds has a draft matrix row or a numbered reason", () => {
  const answered = new Set(MATRIX.flatMap(({ entries }) => entries.map((entry) => entry.key)));
  const missing = items
    .filter((item) => !answered.has(item.key))
    .map((item) => {
      const homes = homesOf(item).map(listOf);
      return homes.length > 0
        ? `${named(item)} has no row and no reason; it belongs in ${homes.join(" or ")}`
        : `${named(item)} has no row and no reason, and no matrix file answers for ${item.above.join(", ")}: it belongs in a draft matrix file of its own`;
    });
  expect(missing).toEqual([]);
});

test("every coverage entry names an item the composer census still finds", () => {
  const stale = MATRIX.flatMap(({ entries, file }) =>
    entries
      .filter((entry) => !byKey.has(entry.key))
      .map((entry) => `${listOf(file)}: ${entryNamed(entry)} is no item of the census`)
  );
  expect([...new Set(stale)]).toEqual([]);
});

test("every row entry names a test of its draft matrix file", () => {
  const wrong: string[] = [];
  for (const { entries, file } of MATRIX) {
    const { titles, unread } = testTitles(join(WEB_SRC, file));
    for (const line of unread) wrong.push(`${file}, ${line}`);
    for (const entry of entries) {
      if (!("row" in entry) || titles.has(entry.row)) continue;
      wrong.push(
        `${listOf(file)}: "${entry.row}", for ${entryNamed(entry)}, is no test of ${file}`
      );
    }
  }
  expect(wrong).toEqual([]);
});

test("every reason entry has words", () => {
  const empty = MATRIX.flatMap(({ entries, file }) =>
    entries
      .filter((entry) => "reason" in entry && !/\p{L}{2,}/u.test(entry.reason))
      .map((entry) => `${listOf(file)}: the reason for ${entryNamed(entry)} has no words`)
  );
  expect(empty).toEqual([]);
});

/** A tree the census reads in place of `web/src`, for the case below: the three draft fields as
 *  the census finds them (`DRAFT_FIELDS` in `trees.ts`), and two routes, each drawing a composer
 *  under a context provider written as a `createElement` call, as a module with no JSX writes
 *  it: `PlantedProvider` as `useDocumentToolbars.ts` writes the issue page's, two providers
 *  nested, and `KeyedProvider` with a key among its props, which ends every mount below it when
 *  it changes, so the census reads no further. */
const PLANTED: Readonly<Record<string, string>> = {
  "app.tsx": `import { Route, Routes } from "react-router";
import { KeyedPage, PlantedPage } from "./features/planted/PlantedPage";

export function App() {
  return (
    <Routes>
      <Route path="/planted" element={<PlantedPage id="a" />} />
      <Route path="/keyed" element={<KeyedPage id="b" />} />
    </Routes>
  );
}
`,
  "features/conversation/MentionComposer.tsx": `export function MentionComposer({ mutationKey }: { mutationKey: readonly string[] }) {
  useComposerSend(mutationKey);
  return <textarea />;
}
`,
  "features/inbox/AskThread.tsx": `export function AskReplyComposer() {
  return null;
}
`,
  "features/inbox/useAskAnswerForm.ts": `export function useAskAnswerForm() {
  return undefined;
}
`,
  "features/planted/PlantedPage.tsx": `import { MentionComposer } from "../conversation/MentionComposer";
import { KeyedProvider, PlantedProvider } from "./planted-context";

export function PlantedPage({ id }: { id: string }) {
  return (
    <PlantedProvider value={1}>
      <MentionComposer mutationKey={["planted", id]} />
    </PlantedProvider>
  );
}

export function KeyedPage({ id }: { id: string }) {
  return (
    <KeyedProvider value={1}>
      <MentionComposer mutationKey={["keyed", id]} />
    </KeyedProvider>
  );
}
`,
  "features/planted/planted-context.ts": `import { createContext, createElement, type ReactNode } from "react";

const OuterContext = createContext(0);
const InnerContext = createContext(0);

export function PlantedProvider({ children, value }: { children: ReactNode; value: number }) {
  if (value < 0) return null;
  return createElement(
    OuterContext.Provider,
    { value },
    createElement(InnerContext.Provider, { value: value + 1 }, children)
  );
}

export function KeyedProvider({ children, value }: { children: ReactNode; value: number }) {
  return createElement(OuterContext.Provider, { key: value, value }, children);
}
`,
};

const plantedRoot = temporaryDirectory("composer-census-planted");

test("the composer census reads through a context provider drawn by createElement, and not through a keyed one", () => {
  for (const [path, source] of Object.entries(PLANTED)) {
    mkdirSync(join(plantedRoot, path, ".."), { recursive: true });
    writeFileSync(join(plantedRoot, path), source);
  }
  const planted = composerCensus(plantedRoot);
  const mount = 'PlantedPage <MentionComposer mutationKey={["planted", id]}>';
  const keyed = 'KeyedPage <MentionComposer mutationKey={["keyed", id]}>';
  const found = planted.items
    .filter((item) => item.above.includes(mount))
    .map((item) => `${item.host} · ${item.kind} · ${item.expression}`);
  // The mount placed, the provider's own code read past both of its calls, and the route above.
  const wanted = [
    'PlantedPage · address prop · mutationKey={["planted", id]}',
    'PlantedPage · lifetime · <MentionComposer mutationKey={["planted", id]}>',
    "PlantedProvider · early return · if (value < 0) return …",
    'App · route · <Route path="/planted">',
  ];
  expect(wanted.filter((item) => !found.includes(item))).toEqual([]);
  expect(planted.unread.filter((line) => !line.endsWith(`, above ${keyed}`))).toEqual([]);
  expect(planted.unread.filter((line) => line.endsWith(`, above ${keyed}`))).toHaveLength(1);
});
