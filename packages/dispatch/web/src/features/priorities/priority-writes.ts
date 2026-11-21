import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api, priorityPatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { CreatePriorityInput, PatchPriorityInput } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { isConflict } from "../writes/conflicts";
import { type Shape, shape } from "../writes/tab-storage";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  defineWriteKind,
  ofKind,
  useWriteSelection,
  useWrites,
  type Write,
} from "../writes/write-store";
import { NOTHING_KEPT } from "./field-arrival";

/** One write of an existing priority's owner or order: its id, what it changes, and the words the
 *  shell's pending-writes indicator names the write by while it is out. A title or a description
 *  is written through `rename` and `describe` instead (`PriorityTextWrite`). */
export interface PriorityPatch {
  readonly id: string;
  readonly input: PatchPriorityInput;
  readonly label: string;
}

/** The attributes a write names the text its edit started from for (`expected`). The owner and
 *  the order take none. */
const EDITED_TEXTS = ["description", "title"] as const;

export type EditedText = (typeof EDITED_TEXTS)[number];

/**
 * Where this page's writes of one text of one priority stand, for what the next write of it
 * names as `expected`: the text the server holds when that write's turn comes. Writes to one
 * priority go out in press order (`priority:<id>`), so while one is out the server holds its text
 * by the time the next goes, and once it lands it holds it until somebody else writes.
 */
export interface TextChain {
  /** Which chain it is: a write carries it (`TextPress.chain`), so one still out on a chain
   *  that was replaced settles nothing of the chain that replaced it. */
  readonly id: number;
  /** The writes pressed here that have not settled. */
  readonly out: number;
  /** Whether one of them was refused: the server holds none of their texts then. */
  readonly refused: boolean;
  /** The text of the last write pressed. */
  readonly last: string;
  /** What the page's read may show while it catches up with these writes: the text it held at
   *  the first press, and each text written since. A read of any of them is the server's answer
   *  to these writes arriving late, not another person's write. */
  readonly known: readonly string[];
}

/** Whether a write pressed now goes after the writes of `chain`: none of them was refused, and
 *  the page's read shows a text they know, so it has not moved to another person's. Not once
 *  the read shows the last text written with none out, which ends the chain. */
function continues(chain: TextChain | undefined, held: string): chain is TextChain {
  if (chain === undefined || chain.refused || !chain.known.includes(held)) return false;
  return chain.out > 0 || chain.last !== held;
}

/** What a write pressed now names as `expected`, where the page reads `held`: the text of the last
 *  write pressed here while the chain goes on, and else `held`. */
export function expectedText(chain: TextChain | undefined, held: string): string {
  return continues(chain, held) ? chain.last : held;
}

/** The chain once a write of `text` is pressed where the page reads `held`: `chain` gone on, or
 *  a new one, `fresh` its id. */
export function pressedText(
  chain: TextChain | undefined,
  text: string,
  held: string,
  fresh: number
): TextChain {
  if (continues(chain, held)) {
    return { ...chain, known: [...chain.known, text], last: text, out: chain.out + 1 };
  }
  return { id: fresh, known: [held, text], last: text, out: 1, refused: false };
}

/** The chain once one of its writes settled, `refused` or landed; none once a refusal leaves no
 *  write out, so the next write names the text the page reads. */
export function settledText(chain: TextChain, refused: boolean): TextChain | undefined {
  const next = { ...chain, out: Math.max(0, chain.out - 1), refused: chain.refused || refused };
  return next.out === 0 && next.refused ? undefined : next;
}

/** One write of a priority's title or description as the write store holds it, and a reload
 *  keeps it: the priority, the text written - the reader's own - and the text the write names as
 *  the one it goes over (`expected`). A text held with no write of it sent (`Writes.claimUnsent`)
 *  names the text it was held over: the one a write of it would have named (`expectedText`)
 *  where the reader wrote it, and the one the page read where a field kept it on its own. */
export interface PriorityTextWrite {
  readonly id: string;
  readonly attribute: EditedText;
  readonly text: string;
  readonly expected: string;
}

const keptTextWrite = shape.record({
  attribute: shape.oneOf(...EDITED_TEXTS),
  expected: shape.string,
  id: shape.string,
  text: shape.string,
}) satisfies Shape<PriorityTextWrite>;

/** The request a write of a priority's text makes: the text, and the text it goes over as
 *  `expected`. */
function textRequest(write: PriorityTextWrite) {
  return priorityPatchRequest(
    write.id,
    write.attribute === "title"
      ? { expected: { title: write.expected }, title: write.text }
      : { description: write.text, expected: { description: write.expected } }
  );
}

/** The words the shell's line names a write of `text` as priority `title`'s `attribute` by. */
export function textWriteLabel(attribute: EditedText, title: string, text: string): string {
  return attribute === "title"
    ? `Rename the priority “${title}” to “${text}”`
    : `Description of the priority “${title}”`;
}

/** What stands for a text of the reader's held with no write of it refused
 *  (`Writes.claimUnsent`): the conflict Dispatch answers a write over another person's text
 *  with, so no Retry sends it, and the shell's line says it in that conflict's words. */
const CHANGED_MEANWHILE = new ApiError(409, {
  code: "PRIORITY_CHANGED",
  error: "Another person changed this text while you wrote.",
});

/** A press of a title or a description: its write, the words the shell's line names it by, and
 *  the chain of this page's writes of that text it goes on (`TextChain.id`): what it settles is
 *  that chain's, and nothing of a chain that replaced it. */
interface TextPress {
  readonly write: PriorityTextWrite;
  readonly label: string;
  readonly chain: number;
}

/** A page mounted now that shows priorities (`usePriorityWrites`): the app's client it reads them
 *  through, and what it does with a write of a text a conflict refused - one of a priority it
 *  shows it takes, to keep the reader's text beside that priority's field, and says so. */
interface MountedPage {
  readonly client: QueryClient;
  readonly takes: (write: Write<PriorityTextWrite>) => boolean;
}

const mountedPages = new Set<MountedPage>();

/**
 * A title or a description written: made from its request alone on its priority's line
 * (`priority:<id>`, `priorityPatchRequest`), and kept across a reload with its record. Dispatch
 * refuses one whose `expected` another person's text has replaced (`409 PRIORITY_CHANGED`), and
 * would refuse that body the same way every time, so no Retry sends it again: a page of the same
 * app mounted now that shows its priority takes the refusal (`claims`), and the store holds it,
 * kept across a reload, while that page offers the reader's text beside the field. A text of the
 * reader's a field kept on its own as another person's arrived is held the same way, as a write
 * that conflict refused, though none was sent (`Writes.claimUnsent`), until a write of the same
 * text of the same priority that went out is refused too, which takes its place (`supersedes`):
 * at once where its refusal lands later, whether a page shows the priority or not, and else once
 * the held text is handed back to the shell's line, so the line names that text once. Where no
 * page shows the priority, the shell's line offers the text to copy (`draft`).
 */
export const priorityTextWrite = defineWriteKind<PriorityTextWrite>({
  claims: (client, write) =>
    [...mountedPages].some((page) => page.client === client && page.takes(write)),
  draft: (write) => write.detail.text,
  kept: keptTextWrite,
  name: "priority-text",
  supersedes: (refused, unsent) =>
    refused.detail.id === unsent.detail.id &&
    refused.detail.attribute === unsent.detail.attribute &&
    refused.detail.text === unsent.detail.text,
});

/** A new priority, its record the title it is written with. Dispatch refuses one whose id - the
 *  title's slug - a priority already carries (`409 PRIORITY_EXISTS`), and a Retry would send the
 *  same title, so the same id: the shell's line offers the title to copy instead (`draft`). */
export const newPriorityWrite = defineWriteKind<string>({
  draft: (write) => write.detail,
  kept: shape.string,
  name: "priority-new",
});

/** One text of one priority this page keeps the reader's texts beside. */
export interface KeptAt {
  readonly id: string;
  readonly attribute: EditedText;
}

/** The writes of the store a page claimed to settle with the reader (`priorityTextWrite.claims`,
 *  `Writes.claim`), by id, each with the mark of the read of the priorities it waits for, and
 *  `null` once that read has answered. */
type ClaimMarks = ReadonlyMap<number, number | null>;

const NOTHING_CLAIMED: ClaimMarks = new Map();

/** A write of a priority's text a conflict refused, as the store holds it. */
type RefusedTextWrite = Extract<Write<PriorityTextWrite>, { readonly status: "refused" }>;

/** Every write of a priority's text a conflict refused, as the store holds them, in press
 *  order. */
function textConflicts(rows: readonly Write[]): readonly RefusedTextWrite[] {
  const conflicts: RefusedTextWrite[] = [];
  for (const row of rows) {
    const write = ofKind(row, priorityTextWrite);
    if (write?.status === "refused" && isConflict(write.refusal)) conflicts.push(write);
  }
  return conflicts;
}

/** Whether this page holds `write`: it claimed it (`claimMarks`), and the store still says it is
 *  claimed - one handed back since is the shell's line's, or another page's. */
function heldHere(write: RefusedTextWrite, claimMarks: ClaimMarks): boolean {
  return write.claimed && claimMarks.has(write.id);
}

/** The writes of the store this page holds for one text of one priority, newest first. */
function heldAt(
  conflicts: readonly RefusedTextWrite[],
  claimMarks: ClaimMarks,
  { attribute, id }: KeptAt
): readonly RefusedTextWrite[] {
  const held: RefusedTextWrite[] = [];
  for (let at = conflicts.length - 1; at >= 0; at -= 1) {
    const write = conflicts[at];
    if (write === undefined || !heldHere(write, claimMarks)) continue;
    if (write.detail.id === id && write.detail.attribute === attribute) held.push(write);
  }
  return held;
}

/** The writes of `holding`, those a page holds for one text of one priority (`heldAt`), whose
 *  text the page offers: none of the writes that carry it still waits for a read (`takeWrites`).
 *  Newest first. */
function offeredOf(
  holding: readonly RefusedTextWrite[],
  claimMarks: ClaimMarks
): readonly RefusedTextWrite[] {
  const waiting = new Set(
    holding.filter((write) => claimMarks.get(write.id) !== null).map((write) => write.detail.text)
  );
  return holding.filter((write) => !waiting.has(write.detail.text));
}

/** The key a priority's text is held under here: its chain's. */
function textKey(attribute: EditedText, id: string): string {
  return `${attribute}:${id}`;
}

const NO_PRIORITIES: readonly string[] = [];

/**
 * The three writes these pages make: a new priority, a change to one, and the move that is a
 * drag of the order. Each goes out under its own key and is retried under it (`useKeyedWrite`),
 * and each refreshes the whole `["priorities"]` family when it settles — the body the pages
 * read and the summary the rail reads sit under that one prefix — so a write a reader made
 * shows as the server holds it rather than as the page guessed. The changes to one priority go
 * out one after another, in the order they were pressed, as the request a reload makes again.
 *
 * A title or a description is written through `rename` and `describe`, which name the text the
 * reader's edit started from (`expected`, `expectedText`): where somebody else wrote the text
 * meanwhile the server refuses the write `409 PRIORITY_CHANGED` and writes nothing, so nobody's
 * text is written over unseen. A write that would change nothing is not sent. A chain of writes
 * that all landed ends once the refresh the last of them started has answered: the page reads
 * what they wrote then, and a text of theirs it reads again later is somebody else's.
 *
 * The reader's own texts a title or a description keeps beside itself while it shows another
 * person's (`ChangedMeanwhile`) are writes of the store, by priority and attribute (`kept`,
 * `keep`, `keeping`), and not the field's or the page's: a row of the table scrolling away takes
 * the field with it, and the field drawn again shows them again, and a description that keeps
 * any is drawn though its disclosure or its row is closed. A write of one of `texts` of a
 * priority in `shown` that Dispatch refuses `409 PRIORITY_CHANGED` is the page's to settle with
 * the reader, whether it was pressed here or a reload took it up, before this page was drawn or
 * since (`priorityTextWrite.claims`, `Writes.claim`): the store holds it, kept across a reload,
 * its text is offered beside the field from there, and nothing stands for it on the shell's
 * line. A page that draws no field for a text - the priorities table draws no title's - names
 * only those it draws, so the shell's line keeps the others.
 * The page reads the priorities again and offers that text once that read has answered, so it
 * never offers the reader's text before it shows the other person's; until then no write of that
 * text of that priority is sent, since any would name a text Dispatch no longer holds, and one
 * the reader writes meanwhile is held to wait for a read of its own. A text a field keeps on its
 * own as another person's arrives over the reader's (`arrival`), with no write of it refused, is
 * held the same way, as a write that conflict refused though none was sent (`keep`,
 * `Writes.claimUnsent`), and offered at once: the field shows the other person's text already. A
 * write of that text the server refuses takes its place (`priorityTextWrite.supersedes`): at once
 * where the refusal comes later, and else once the page hands the held text back.
 * Keeping the other person's text, or using one of the reader's, which goes as a press of its
 * own, lets the write go (`Writes.settle`), and so does a read that shows its text, which the
 * server then holds. Leaving the page hands every write it holds back to the shell's line
 * (`Writes.unclaim`), which offers the reader's text to copy, as it does a text held as a field
 * leaves with the page, and a page drawn later that shows its priority - after a reload too -
 * takes it again. A page that shows no priority - the New priority form's - takes none.
 *
 * Whether the reader may write at all is `useIsHuman`: every priority write is classed as the
 * deployment's (`internal/dispatch/api/priorities.go`), which a credential scoped to a project
 * and a token naming no person are both refused, so a page that offered the controls to them
 * would offer a refusal.
 */
export function usePriorityWrites(
  shown: readonly string[] = NO_PRIORITIES,
  texts: readonly EditedText[] = EDITED_TEXTS
) {
  const queryClient = useQueryClient();
  const writes = useWrites();
  const canWrite = useIsHuman();
  const chains = useRef(new Map<string, TextChain>());
  const made = useRef(0);
  // The mark of the last take, counted up for each: a take clears only its own mark.
  const marks = useRef(0);
  const [claimMarks, setClaimMarks] = useState(NOTHING_CLAIMED);
  // The ids of the writes of the store this page claimed, as they stand at once: the page hands
  // each back to the shell's line as it goes, and one it claimed just before has drawn no render.
  const claimedIds = useRef(new Set<number>());
  // Whether the page has gone (the effect that hands its writes back): a field's own write as it
  // leaves with the page comes after, and a text held then is the shell's line's (`holdText`).
  const gone = useRef(false);
  const conflicts = useWriteSelection("priority-text-conflicts", textConflicts);
  // The priorities as the page reads them, which this only watches: the page's own read fetches
  // them.
  const stored = useQuery({ ...prioritiesQuery(), enabled: false }).data?.priorities;
  // Reads the priorities again: a read that starts now, and answers once it - or a later read
  // that cancels it, and which it waits for (TanStack's `cancelRefetch`) - has answered.
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["priorities"] }),
    [queryClient]
  );
  const add = useKeyedWrite({
    detail: (input: CreatePriorityInput) => input.title,
    kind: newPriorityWrite,
    label: (input) => `New priority “${input.title}”`,
    method: "POST",
    write: (input, bounds) => api.createPriority(input, bounds),
    onSettled: () => void refresh(),
  });
  const patch = useKeyedWrite({
    label: (patch_: PriorityPatch) => patch_.label,
    target: (patch_) => `priority:${patch_.id}`,
    request: (patch_) => priorityPatchRequest(patch_.id, patch_.input),
    onSettled: () => void refresh(),
  });
  const text = useKeyedWrite({
    detail: (press: TextPress) => press.write,
    kind: priorityTextWrite,
    label: (press) => press.label,
    target: (press) => `priority:${press.write.id}`,
    request: ({ write }) => textRequest(write),
    // Counted here, on every write, rather than in the press's own callbacks, which TanStack
    // calls for the last press alone.
    onSettled: (_data, error, press) => {
      const key = textKey(press.write.attribute, press.write.id);
      const chain = chains.current.get(key);
      let landed: TextChain | undefined;
      if (chain !== undefined && chain.id === press.chain) {
        const next = settledText(chain, error !== null);
        if (next === undefined) chains.current.delete(key);
        else chains.current.set(key, next);
        if (next?.out === 0) landed = next;
      }
      // A conflict wrote nothing, for anyone: the page that took it reads the priorities itself
      // (`takeWrites`), and one no page took has nothing on the page to read again for.
      if (isConflict(error)) return;
      const refreshed = refresh();
      if (landed === undefined) return;
      void refreshed.then(() => {
        if (chains.current.get(key) === landed) chains.current.delete(key);
      });
    },
  });
  /** Takes `ids`, writes of the store a conflict refused, to settle with the reader: each one's
   *  text is kept beside its priority's field, newest first by when the write was pressed, but
   *  waits until `settled` - a read of the priorities started for it - answers: until then the
   *  page may still read the text the write went over, and not the other person's. A later take
   *  of the same write waits for its own read, and this one's answer leaves it waiting. */
  const takeWrites = useCallback((ids: readonly number[], settled: Promise<void>) => {
    marks.current += 1;
    const mark = marks.current;
    for (const id of ids) claimedIds.current.add(id);
    setClaimMarks((current) => {
      const next = new Map(current);
      for (const id of ids) next.set(id, mark);
      return next;
    });
    const answered = () =>
      setClaimMarks((current) => {
        const mine = ids.filter((id) => current.get(id) === mark);
        if (mine.length === 0) return current;
        const next = new Map(current);
        for (const id of mine) next.set(id, null);
        return next;
      });
    void settled.then(answered, answered);
  }, []);
  /** Lets the writes `ids` this page holds go (`Writes.settle`): the reader settled them here, or
   *  the server holds their text now. */
  const letGo = useCallback(
    (ids: readonly number[]) => {
      if (ids.length === 0) return;
      for (const id of ids) {
        writes.settle(id);
        claimedIds.current.delete(id);
      }
      setClaimMarks((current) => {
        const next = new Map(current);
        for (const id of ids) next.delete(id);
        return next;
      });
    },
    [writes]
  );
  /** Holds the reader's text `write` in the store as a write another person's text refused,
   *  though none was sent (`Writes.claimUnsent`), named `label`: the page's own, as a write it
   *  claimed is, and the newest of the texts kept beside its field, in place of every write of
   *  `holding` - those the page holds for that text of that priority (`heldAt`) - that carries
   *  the same text. It is offered at once, or, with `settled`, once that read - one started for
   *  it - has answered (`takeWrites`). Once the page has gone, it is the shell's line's at once,
   *  in place of those writes, which the page handed back to the line as it went. */
  const holdText = useCallback(
    (
      write: PriorityTextWrite,
      label: string,
      holding: readonly RefusedTextWrite[],
      settled?: Promise<void>
    ) => {
      const same = holding.filter((one) => one.detail.text === write.text).map((one) => one.id);
      if (gone.current) {
        for (const id of same) writes.supersede(id);
      } else {
        letGo(same);
      }
      const heldId = writes.claimUnsent({
        detail: write,
        kind: priorityTextWrite,
        label,
        refusal: CHANGED_MEANWHILE,
        request: textRequest(write),
        target: `priority:${write.id}`,
      });
      if (gone.current) {
        writes.unclaim(heldId);
        return;
      }
      if (settled !== undefined) {
        takeWrites([heldId], settled);
        return;
      }
      claimedIds.current.add(heldId);
      setClaimMarks((current) => new Map(current).set(heldId, null));
    },
    [letGo, takeWrites, writes]
  );
  // The conflicts of the texts this page draws of the priorities it shows are its own to settle
  // with the reader, from the moment it is drawn until it goes or shows others.
  useEffect(() => {
    if (shown.length === 0) return;
    const page: MountedPage = {
      client: queryClient,
      takes: (write) => {
        if (!shown.includes(write.detail.id) || !texts.includes(write.detail.attribute))
          return false;
        takeWrites([write.id], refresh());
        return true;
      },
    };
    mountedPages.add(page);
    return () => {
      mountedPages.delete(page);
    };
  }, [queryClient, refresh, shown, takeWrites, texts]);
  // And those refused before it showed their priority - a reload's, taken up before the page
  // was drawn, one of a row the table did not draw, or one a page that went handed back - which
  // stand on the shell's line until a page that shows their priority takes them, all of them
  // with one read.
  useEffect(() => {
    const claimed: number[] = [];
    for (const write of conflicts) {
      const { attribute, id } = write.detail;
      if (shown.includes(id) && texts.includes(attribute) && writes.claim(write.id))
        claimed.push(write.id);
    }
    if (claimed.length > 0) takeWrites(claimed, refresh());
  }, [conflicts, refresh, shown, takeWrites, texts, writes]);
  // The page going hands every write it holds back to the shell's line, where the reader's text
  // is offered to copy, until a page drawn later that shows its priority takes it again. React
  // runs a removed tree's cleanups parent first, so a field's own write as it leaves with the
  // page comes after this, and what it holds is the line's from the start (`gone`).
  useEffect(() => {
    const held = claimedIds.current;
    gone.current = false;
    return () => {
      gone.current = true;
      for (const id of held) writes.unclaim(id);
      held.clear();
    };
  }, [writes]);
  // A write this page holds whose text the page reads is one the server holds now: the reader
  // wrote it again, or another person wrote the same text. No text of it is the reader's to put
  // back, so it goes; the field never offers a text the page reads (`kept`), so the reader would
  // never let it go there. Only once the read its claim started has answered: until then the page
  // may read a text the server held before it.
  useEffect(() => {
    if (!stored) return;
    const landed = conflicts.filter((write) => {
      if (!heldHere(write, claimMarks) || claimMarks.get(write.id) !== null) return false;
      const { attribute, id, text: written } = write.detail;
      return stored.find((row) => row.id === id)?.[attribute] === written;
    });
    letGo(landed.map((write) => write.id));
  }, [claimMarks, conflicts, letGo, stored]);
  const { mutate } = text;
  const writeText = useCallback(
    (id: string, attribute: EditedText, written: string, held: string, label: string) => {
      const key = textKey(attribute, id);
      const chain = chains.current.get(key);
      const expected = expectedText(chain, held);
      if (written === expected) return;
      const holding = heldAt(conflicts, claimMarks, { attribute, id });
      if (holding.some((write) => claimMarks.get(write.id) !== null)) {
        // A conflict brought a text of this back, and the page has not read the other person's
        // yet: a write now names a text Dispatch no longer holds, and would be refused the same
        // way. It is held instead, to be offered once a read started now has answered.
        holdText({ attribute, expected, id, text: written }, label, holding, refresh());
        return;
      }
      made.current += 1;
      const next = pressedText(chain, written, held, made.current);
      chains.current.set(key, next);
      mutate({ chain: next.id, label, write: { attribute, expected, id, text: written } });
    },
    [claimMarks, conflicts, holdText, mutate, refresh]
  );
  /** Writes `title` as priority `id`'s title, where the page reads `held`. A title with nothing
   *  in it is refused by the server, so none is sent. */
  const rename = useCallback(
    (id: string, title: string, held: string, label: string) => {
      if (title !== "") writeText(id, "title", title, held, label);
    },
    [writeText]
  );
  /** Writes `description` as priority `id`'s description, where the page reads `held`. */
  const describe = useCallback(
    (id: string, description: string, held: string, label: string) =>
      writeText(id, "description", description, held, label),
    [writeText]
  );
  /** The reader's texts kept beside priority `id`'s `attribute`, newest first, where the page
   *  reads `held`: the texts of the writes of the store it holds for it, by when each was pressed
   *  or held, none twice; but for one still waiting for a read (`takeWrites`), and for `held`
   *  itself, which the server holds, so it is no text of the reader's to put back. */
  const kept = useCallback(
    (id: string, attribute: EditedText, held: string): readonly string[] => {
      if (claimMarks.size === 0) return NOTHING_KEPT;
      const offered = offeredOf(heldAt(conflicts, claimMarks, { attribute, id }), claimMarks);
      const texts = [...new Set(offered.map((write) => write.detail.text))].filter(
        (one) => one !== held
      );
      return texts.length === 0 ? NOTHING_KEPT : texts;
    },
    [claimMarks, conflicts]
  );
  /** Every text of a priority this page keeps the reader's texts beside, offered or waiting: one a
   *  write of the store it holds carries. */
  const keeping = useMemo((): readonly KeptAt[] => {
    const at = new Map<string, KeptAt>();
    for (const write of conflicts) {
      if (!heldHere(write, claimMarks)) continue;
      const { attribute, id } = write.detail;
      at.set(textKey(attribute, id), { attribute, id });
    }
    return [...at.values()];
  }, [claimMarks, conflicts]);
  /** Keeps `texts`, newest first, beside priority `id`'s `attribute`, in place of what was kept
   *  there. A write of the store whose text the page offered and `texts` no longer holds is one
   *  the reader settled - they kept the other person's text, or used one of theirs, which goes as
   *  a press of its own - and it goes (`Writes.settle`). A text still waiting for a read (`kept`)
   *  was never offered, so it is no field's to let go, and stays. A text no write of the store
   *  carries is one the field kept on its own as another person's arrived over it (`arrival`):
   *  it is held there (`holdText`), over the text the page reads, so it outlives the field, the
   *  page and a reload as a refused write's text does. So is one the field put ahead of a text a
   *  newer write carries, and each text ahead of it, so the store's order is the field's. */
  const keep = useCallback(
    (id: string, attribute: EditedText, texts: readonly string[]) => {
      const holding = heldAt(conflicts, claimMarks, { attribute, id });
      const offered = offeredOf(holding, claimMarks);
      letGo(offered.filter((write) => !texts.includes(write.detail.text)).map((write) => write.id));
      // The newest write of the store this page holds that carries each text.
      const newest = new Map<string, number>();
      for (const write of holding) {
        if (!newest.has(write.detail.text)) newest.set(write.detail.text, write.id);
      }
      const row = stored?.find((one) => one.id === id);
      const over = row?.[attribute] ?? "";
      // Oldest first: a text stays where a write newer than every older text's carries it, and
      // is held anew, ahead of them all, where none does; every newer text then follows it.
      let floor = 0;
      for (let place = texts.length - 1; place >= 0; place -= 1) {
        const text = texts[place];
        if (text === undefined) continue;
        const carried = newest.get(text) ?? 0;
        if (carried > floor) {
          floor = carried;
          continue;
        }
        const label = textWriteLabel(attribute, row?.title ?? id, text);
        holdText({ attribute, expected: over, id, text }, label, holding);
        floor = Number.POSITIVE_INFINITY;
      }
    },
    [claimMarks, conflicts, holdText, letGo, stored]
  );
  return { add, canWrite, describe, keep, keeping, kept, patch, rename };
}
