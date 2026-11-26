import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api, priorityPatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { CreatePriorityInput, PatchPriorityInput } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { isConflict } from "../writes/conflicts";
import { endedWithoutVerdict } from "../writes/keyed-write";
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

/** The attributes a write names the server's text it goes over for (`expected`). The owner and
 *  the order take none. */
const EDITED_TEXTS = ["description", "title"] as const;

export type EditedText = (typeof EDITED_TEXTS)[number];

/** One of the writes of one text of one priority this app's pages made (`TextWrites`) that a
 *  write of that text pressed later may go after (`TextChain`): its press (`TextPress.press`), the
 *  text it writes, the text it names as the one it goes over (`expected`), and, once it landed,
 *  how many reads of the priorities had answered by then, a read still out then counted among
 *  them (`landed`): that read may have left before the write committed, so it does not show the
 *  write. */
export interface ChainedWrite {
  readonly press: number;
  readonly text: string;
  readonly over: string;
  readonly landed: number | undefined;
}

/**
 * The writes of one text of one priority this app's pages made (`TextWrites`), in press order,
 * that the server holds or will hold past the text the field last took in from it (the field's
 * `from`): each still out, or landed and not yet drawn by a read of the priorities. Writes to one
 * priority go out in press order (`priority:<id>`), so where they land the server holds the
 * newest of them by the time a write pressed now goes, and that write names it (`expectedText`).
 * A write that ends any way but landing - refused, for whatever reason, stopped, its key spent,
 * refused after an attempt that went unanswered, or discarded - leaves at once (`settledText`)
 * and is never named, whether or not it landed: where it did, a later read brings its text to
 * the field, which takes it in (`arrival`) as no change where the field still holds that text,
 * and else as another person's, and the field's `from` moves to it.
 */
export type TextChain = readonly ChainedWrite[];

const NO_WRITES: TextChain = [];

/**
 * The writes of `chain` a write pressed now goes after, where the field last took `from` in from
 * the server and the page draws the read of the priorities that was the `drawn`th to answer. A
 * write whose landing that read is counted past (`ChainedWrite.landed`) leaves, with every write
 * before it: that read left after the write committed, so it shows its text or a later one, and
 * the field took that in. Where `from` is none of the texts the writes left write or go over, the
 * field took another person's text in after them all, which they go behind - one still out is
 * refused, or written over - so none is left.
 */
export function aheadOf(chain: TextChain | undefined, from: string, drawn: number): TextChain {
  if (chain === undefined) return NO_WRITES;
  let read = 0;
  chain.forEach((write, at) => {
    if (write.landed !== undefined && write.landed < drawn) read = at + 1;
  });
  const left = chain.slice(read);
  return left.some((write) => write.over === from || write.text === from) ? left : NO_WRITES;
}

/** What a write pressed now names as `expected`, after the writes of `chain` it goes after
 *  (`aheadOf`), where the field last took `from` in from the server: the text of the newest of
 *  them, which the server holds by its turn, and else `from`. */
export function expectedText(chain: TextChain, from: string): string {
  return chain.at(-1)?.text ?? from;
}

/** `chain` once press `press` of `text` goes after it, naming `over` as the text it goes over. */
export function pressedText(
  chain: TextChain,
  press: number,
  text: string,
  over: string
): TextChain {
  return [...chain, { landed: undefined, over, press, text }];
}

/** `chain` once its write `press` ended: landed, marked `landed` (`ChainedWrite.landed`), or, with
 *  `landed` absent, any other way, which leaves the chain. A write the chain no longer holds
 *  changes nothing. */
export function settledText(chain: TextChain, press: number, landed?: number): TextChain {
  if (landed === undefined) return chain.filter((write) => write.press !== press);
  return chain.map((write) => (write.press === press ? { ...write, landed } : write));
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
  /** What the edit this write carries was made on, and when. Absent from a text a field kept on
   *  its own, which is never sent. */
  readonly made?: {
    /** The text the field held as the edit began (`onSave`'s `on`): the last it wrote, though
     *  that write did not land, or the last of the server's it took in, whichever came last; a
     *  field drawn again starts from the server's. Use my text of a text whose own write is known
     *  hands on what that write was made on instead, since it writes that same edit again. A
     *  newer write made on this one's text carries it (`priorityTextWrite.outdates`). */
    readonly on: string;
    /** When the edit was made, in the order this tab made its edits in (`writeText`): what says
     *  which of two writes is the newer, since the store's own order follows the line, which a
     *  Retry and a reload reorder. */
    readonly at: number;
  };
}

const keptTextWrite = shape.record({
  attribute: shape.oneOf(...EDITED_TEXTS),
  expected: shape.string,
  id: shape.string,
  made: shape.optional(shape.record({ at: shape.number, on: shape.string })),
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
 *  the press it is in its chain (`ChainedWrite.press`). */
interface TextPress {
  readonly write: PriorityTextWrite;
  readonly label: string;
  readonly press: number;
}

/** The answer to a write of a field's text that is not sent: it does not land. */
const NOT_LANDED = Promise.resolve(false);

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
 * page shows the priority, the shell's line offers the text to copy (`draft`). A write of a text
 * that lands makes needless each write of that text of that priority made before it that ended
 * with no verdict and stands on the shell's line, where the landed one was made on its text: the
 * field held that text as the newer edit began (`made.on`), so the newer write carries the
 * reader's stopped edit. So does a newer write of that very text - Use my text writing it again -
 * since nothing of it is lost: where that write landed, the server holds the text, and where a
 * verdict refused it, it still carries the text. The run goes on through each refused write a
 * write it reached was made on, one a verdict of Dispatch's refused included, which stays, since
 * no Retry undoes a verdict: a stopped write that one was made on goes all the same (`outdates`).
 * A write discarded is gone from the store, so a run through it stops there. Which was made first
 * is the writes' own record (`made.at`), never the store's order, which a Retry and a reload
 * change: a Retry puts a write at the back of its line, and a reload hands the writes out in the
 * order they were last pressed. A Retry of a write made needless would name a text the server no
 * longer holds, be refused, and offer the older text back as another person's: the store keeps it
 * no longer, so a reload does not bring it back, and its Retry sends nothing. Naming the same
 * `expected` says nothing of that: a field drawn again - a row opened again, the page come back
 * to, a reload - starts from the server's text, and so does one that took another person's text
 * in, though he wrote back the very text the older write went over. A newer write made on the
 * server's text carries nothing of the older one, which stays on the line and in the tab's
 * storage: its Retry is sent, and refused as a write over another person's text is, which keeps
 * the reader's text.
 */
export const priorityTextWrite = defineWriteKind<PriorityTextWrite>({
  claims: (client, write) =>
    [...mountedPages].some((page) => page.client === client && page.takes(write)),
  draft: (write) => write.detail.text,
  kept: keptTextWrite,
  name: "priority-text",
  outdates: ({ detail: carrier }, { detail: earlier }) =>
    carrier.made !== undefined &&
    earlier.made !== undefined &&
    earlier.made.at < carrier.made.at &&
    carrier.id === earlier.id &&
    carrier.attribute === earlier.attribute &&
    (carrier.made.on === earlier.text || carrier.text === earlier.text),
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

/** When the newest write of a priority's text among `rows`, the writes the store holds, was made
 *  (`PriorityTextWrite.made`'s `at`), one a reload took up included; 0 where none records it. */
function newestStamp(rows: readonly Write[]): number {
  let newest = 0;
  for (const row of rows) {
    const at = ofKind(row, priorityTextWrite)?.detail.made?.at;
    if (at !== undefined && at > newest) newest = at;
  }
  return newest;
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

/** What one app - one query client - knows of the writes of titles and descriptions its pages
 *  made: each text's chain (`TextChain`, by `textKey`), the press each write was, by the record
 *  the write store holds of it (`PriorityTextWrite`), how many presses were made, and when the
 *  newest write was made (`stamp`). Kept with the client, as the write store is (`writesOf`), and
 *  not with a page: a write outlives the page that pressed it, so a page drawn while one is out -
 *  the reader left the page and came back, or the priority's own page and the table were drawn in
 *  turn - goes after it as the page before would have, and names its text. A reload starts a new
 *  client, which knows none: an edit made there before a read shows a write the tab before it
 *  made names the text before that write, is refused, and is offered back beside the field, so
 *  nothing is lost. */
interface TextWrites {
  readonly chains: Map<string, TextChain>;
  readonly presses: WeakMap<PriorityTextWrite, number>;
  made: number;
  /** When the newest write its pages made was made (`PriorityTextWrite.made`'s `at`): the next
   *  is made after it, whatever the clock does (`writeText`). */
  stamp: number;
}

const textWrites = new WeakMap<QueryClient, TextWrites>();

/** What `client`'s app knows of the writes of titles and descriptions its pages made. */
function textWritesOf(client: QueryClient): TextWrites {
  let known = textWrites.get(client);
  if (known === undefined) {
    known = { chains: new Map(), made: 0, presses: new WeakMap(), stamp: 0 };
    textWrites.set(client, known);
  }
  return known;
}

/** Whether the reader chose the text of `refused` - a write a conflict refused - again since: a
 *  write of the same text of the same priority pressed after it is in its chain (`own`), still
 *  out or landed and not yet drawn. The field holds that text, so offering it beside the field
 *  would offer what the field shows, and that later write carries it on: it lands, or meets a
 *  conflict of its own, which is offered. A write a reload took up was pressed by no page of this
 *  app, and is chosen by none. */
function chosenAgain(own: TextWrites, refused: PriorityTextWrite): boolean {
  const press = own.presses.get(refused);
  if (press === undefined) return false;
  const chain = own.chains.get(textKey(refused.attribute, refused.id)) ?? NO_WRITES;
  return chain.some((write) => write.press > press && write.text === refused.text);
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
 * A title or a description is written through `rename` and `describe`. Each write names as the
 * server's text it goes over (`expected`) the text the field last took in from a read of the
 * server's (`from`, as the field hands it) - never the page's own read, which draws another
 * person's text before the field takes it in, so a write the field makes in between would name
 * their text and go over it - or, while a write of that text this app's pages made is out, or
 * landed and not yet drawn by a read, the newest of those, which the server holds by its turn
 * (`TextChain`): a page drawn while one is out goes after it, as the page that pressed it would
 * (`TextWrites`). A write that ends any way but landing is never named, whether or not it landed:
 * where it did, a later read brings its text to the field, which takes it in (`arrival`) as no
 * change where the field still holds that text, and else as another person's. Where somebody else
 * wrote the text meanwhile the server refuses the write `409 PRIORITY_CHANGED` and writes nothing,
 * so nobody's text is written over unseen. "Use my text" is written as any write is: a write of
 * the reader's own still out - the one the field's blur made as they pressed it - is theirs to
 * replace, and it goes after that. A write that would change nothing is not sent. Each write is
 * answered with whether it landed, once its chain has heard: one that did not never comes back
 * to the field. Each records too what its edit was made on, as the field hands it, and when
 * (`PriorityTextWrite.made`), by which one that lands lets go a stopped write made before it
 * whose text it was made on (`priorityTextWrite.outdates`).
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
 * line. One whose text the reader chose again since - a write of that text pressed after it is
 * in its chain - is let go at once instead, and offered nowhere (`chosenAgain`): the field shows
 * that text, and that write lands, or meets a conflict of its own, which is offered. A page that
 * draws no field for a text - the priorities table draws no title's - names only those it draws,
 * so the shell's line keeps the others.
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
  const own = textWritesOf(queryClient);
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
  const read = useQuery({ ...prioritiesQuery(), enabled: false });
  const stored = read.data?.priorities;
  // How many reads of the priorities had answered when the read the page draws now did, its own
  // among them: a field took that read's text in, so it holds past every write of its chain whose
  // landing that read is counted past (`aheadOf`, `ChainedWrite.landed`). A page drawn while a
  // write is out starts from the reads its client has answered, on which that write's landing is
  // counted too. A read that answers later with the priorities the page draws already draws
  // nothing again, and counts as drawn where it is the read a landing started (`text`'s
  // `onSettled`).
  const reads = queryClient.getQueryState(prioritiesQuery().queryKey)?.dataUpdateCount ?? 0;
  const drawn = useRef({ data: read.data, reads });
  useEffect(() => {
    drawn.current = { data: read.data, reads };
  }, [read.data, reads]);
  // Reads the priorities again, and answers once a read that left after this call has answered:
  // where one is out already, the app's client lets it answer and then reads again
  // (`DispatchQueryClient`), and TanStack's own cancels it for a read that starts now.
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["priorities"] }),
    [queryClient]
  );
  // A new priority's press ends once the read it set off has answered, landed or not, so the page
  // that pressed it reads from its end whether the priority is drawn (`NewPriority`'s `onAdd`).
  // Nothing waits on it otherwise: the form closes as it sends, and its refusal is the shell's.
  const add = useKeyedWrite({
    detail: (input: CreatePriorityInput) => input.title,
    kind: newPriorityWrite,
    label: (input) => `New priority “${input.title}”`,
    method: "POST",
    write: (input, bounds) => api.createPriority(input, bounds),
    onSettled: () => refresh(),
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
    // calls for the last press alone. A write that landed stays in its chain until a read that
    // left after it is drawn; any other ending leaves it at once.
    onSettled: (_data, error, press) => {
      const key = textKey(press.write.attribute, press.write.id);
      const chain = own.chains.get(key);
      const landed = error === null;
      if (chain !== undefined) {
        const now = queryClient.getQueryState(prioritiesQuery().queryKey);
        // A read still out may have left before the write committed, and the app's client lets
        // it answer rather than cancel it (`DispatchQueryClient`): it may show the text before,
        // so it does not count as showing the landing, and only a read after it does.
        const past =
          now === undefined ? 0 : now.dataUpdateCount + (now.fetchStatus === "idle" ? 0 : 1);
        own.chains.set(key, settledText(chain, press.press, landed ? past : undefined));
      }
      // A conflict wrote nothing, for anyone: the page that took it reads the priorities itself
      // (`takeWrites`), and one no page took has nothing on the page to read again for.
      if (isConflict(error)) return;
      // A write that ended with no verdict - stopped, its key spent, or refused after an attempt
      // that went unanswered - may land later, or did land unseen: its own event reads the
      // priorities then (`useEventFeed`), and a read now would show nothing of a later landing
      // and hold that one back by the event feed's one read a second (`MIN_REFETCH_INTERVAL_MS`),
      // so the field would edit on from the text before it. Dispatch's other verdicts, a discard
      // and a landing read the priorities at once.
      if (endedWithoutVerdict(error)) return;
      const refreshed = refresh();
      if (!landed) return;
      // That read may answer with the priorities the page draws already, which draws nothing
      // again: the page has read past the landing all the same.
      void refreshed.then(() => {
        const now = queryClient.getQueryState(prioritiesQuery().queryKey);
        if (now !== undefined && now.data === drawn.current.data) {
          drawn.current = { data: now.data, reads: now.dataUpdateCount };
        }
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
        // The store holds the refusal, claimed, once this answers: one the reader chose again
        // since is let go then, as the store's change ends.
        if (chosenAgain(own, write.detail)) queueMicrotask(() => writes.settle(write.id));
        else takeWrites([write.id], refresh());
        return true;
      },
    };
    mountedPages.add(page);
    return () => {
      mountedPages.delete(page);
    };
  }, [own, queryClient, refresh, shown, takeWrites, texts, writes]);
  // And those refused before it showed their priority - a reload's, taken up before the page
  // was drawn, one of a row the table did not draw, or one a page that went handed back - which
  // stand on the shell's line until a page that shows their priority takes them, all of them
  // with one read.
  useEffect(() => {
    const claimed: number[] = [];
    for (const write of conflicts) {
      const { attribute, id } = write.detail;
      if (!shown.includes(id) || !texts.includes(attribute) || !writes.claim(write.id)) continue;
      if (chosenAgain(own, write.detail)) writes.settle(write.id);
      else claimed.push(write.id);
    }
    if (claimed.length > 0) takeWrites(claimed, refresh());
  }, [conflicts, own, refresh, shown, takeWrites, texts, writes]);
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
  const { mutateAsync } = text;
  /** Writes `written` as priority `id`'s `attribute`, where the field last took `from` in from the
   *  server and made the edit on `on` (`PriorityTextWrite.made`): over the newest write of its
   *  chain it goes after (`aheadOf`), or over `from`. Answers whether the write landed, once its
   *  chain has heard (`text`'s `onSettled`, which TanStack runs before it settles the press): one
   *  not sent did not. */
  const writeText = useCallback(
    (
      id: string,
      attribute: EditedText,
      written: string,
      from: string,
      on: string,
      label: string
    ): Promise<boolean> => {
      const key = textKey(attribute, id);
      const chain = aheadOf(own.chains.get(key), from, drawn.current.reads);
      const expected = expectedText(chain, from);
      if (written === expected) return NOT_LANDED;
      // The record the store holds of the write (`priorityTextWrite`). It is stamped after every
      // edit this app made before it, and after every write the store holds, those a reload took
      // up included, whatever clock stamped those: `outdates` weighs a write only against the
      // writes the store holds, so an edit made after a reload is newer than each write it took up.
      own.stamp = Math.max(Date.now(), own.stamp + 1, newestStamp(writes.rows()) + 1);
      const detail = { attribute, expected, id, made: { at: own.stamp, on }, text: written };
      const holding = heldAt(conflicts, claimMarks, { attribute, id });
      if (holding.some((write) => claimMarks.get(write.id) !== null)) {
        // A conflict brought a text of this back, and the page has not read the other person's
        // yet: a write now names a text Dispatch no longer holds, and would be refused the same
        // way. It is held instead, to be offered once a read started now has answered.
        holdText(detail, label, holding, refresh());
        return NOT_LANDED;
      }
      own.made += 1;
      const press = own.made;
      // Its refusal is known as this press by its record (`chosenAgain`).
      own.presses.set(detail, press);
      own.chains.set(key, pressedText(chain, press, written, expected));
      return mutateAsync({ label, press, write: detail }).then(
        () => true,
        () => false
      );
    },
    [claimMarks, conflicts, holdText, mutateAsync, own, refresh, writes]
  );
  /** Writes `title` as priority `id`'s title, where its field last took `from` in from the server
   *  and made the edit on `on` (`writeText`), and answers whether it landed. A title with nothing
   *  in it is refused by the server, so none is sent. */
  const rename = useCallback(
    (id: string, title: string, from: string, on: string, label: string) =>
      title === "" ? NOT_LANDED : writeText(id, "title", title, from, on, label),
    [writeText]
  );
  /** Writes `description` as priority `id`'s description, where its editor last took `from` in
   *  from the server and made the edit on `on` (`writeText`), and answers whether it landed. */
  const describe = useCallback(
    (id: string, description: string, from: string, on: string, label: string) =>
      writeText(id, "description", description, from, on, label),
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
  /** What the newest write of the store this page holds for priority `id`'s `attribute` that
   *  carries `text` - one of the texts kept beside it (`kept`) - was made on
   *  (`PriorityTextWrite.made`): where the reader uses that text, hers is that same edit
   *  written again. Undefined where none is held, or where that write is a text a field kept on
   *  its own, which records none. */
  const keptMadeOn = useCallback(
    (id: string, attribute: EditedText, text: string): string | undefined =>
      heldAt(conflicts, claimMarks, { attribute, id }).find((write) => write.detail.text === text)
        ?.detail.made?.on,
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
  return { add, canWrite, describe, keep, keeping, kept, keptMadeOn, patch, rename };
}
