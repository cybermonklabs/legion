import { dispatchWritingBox } from "@legion/contracts/dispatch-writing-boxes";
import type { DocumentJSON } from "@legion/proof-editor";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useId, useRef, useState } from "react";
import type { Doc } from "yjs";
import { whoAmIQuery } from "../../api/queries";
import type { DisplayTree } from "../../api/types";
import { hasFocusVisibleRing, textMutedOnCanvas } from "../../theme/classes";
import { borderDefault, dangerText } from "../../theme/shell-classes";
import { colorForLogin } from "../doc/connection";
import { DocumentRefPicker } from "../doc/DocumentRefPicker";
import { DocumentReferences, useReferenceClick } from "../doc/document-references";
import { createDoc, type EditorHandle } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { compiledBlockSchema } from "../doc/schema";
import { watchPicturePath } from "../refs/reference-target";
import { answered, arrival, NOTHING_KEPT, type SentText, usedOn } from "./field-arrival";
import { ChangedMeanwhile, SAVE_AFTER_MS } from "./InPlaceField";

const NONE_WRITTEN = "No description is written.";
/** The writing list's row of a priority's description, whose picker the editor hosts
 *  (`DocumentRefPicker`): the line under the editor is the row's hint. */
const DESCRIPTION_BOX = dispatchWritingBox("priority-description", "priority-description");
/** The line under a description drawn from the server's reading of it where the server's parser
 *  does not take the description whole. */
const NOT_MARKDOWN =
  "Dispatch cannot read this description as markdown, so it cannot be edited on this page.";

/** The editor a description is drawn in, and where it stands against the server's text
 *  (`FieldStand`). */
interface Held {
  readonly handle: EditorHandle;
  /** The markdown the editor held when it last matched the server's text: after it took that
   *  text in, and after each write it sent. Only a text that differs from it is written, since
   *  the editor reads a text back in its own form (`* ` for `- `, a soft break as a space, a
   *  closing newline), and that is no change the reader made. */
  base: string;
  /** The markdown the reader's edit is made on, which each write hands on (`write`), in the form
   *  a write sends, so a write made on the editor's own earlier one names that one's text: the
   *  last it wrote, or the last of the server's it came to hold - one it drew (`take`, `redraw`),
   *  or one a read brought while the reader held it (`arrival`). A read that moves `base` under
   *  typing she has not written leaves it: that typing began on it. */
  made: string;
  /** What each markdown the editor wrote was made on, by the markdown: Use my text of one of them
   *  writes that same edit again, and is made on what it was made on (`putBack`). */
  readonly madeOn: Map<string, string>;
  /** The server's text the editor last took in, as the server holds it rather than as the editor
   *  reads it back: the text it opened on (`take`), was drawn to (`redraw`), or saw arrive
   *  (`arrival`). A read is all that moves it, never a write, which may not land. Each write
   *  hands it on with what it writes (`write`), for the write to name as the text it goes over
   *  (`expected`). So a write made after the page drew another person's text, and before the
   *  editor took that text in, names the text the reader edited, never theirs, and Dispatch
   *  refuses it rather than writing over it. */
  from: string;
  /** The texts written that the server has not answered with yet, oldest first: its answer
   *  with one of them is the reader's own write coming back, not another person's text. The
   *  page's answer marks one that landed, and takes out one that did not (`answered`). */
  sent: readonly SentText[];
  /** The write waiting for the typing to rest. */
  timer: number | undefined;
  /** Whether the editor drew its text from the server's reading of it (`draw`), rather than
   *  taking it in as markdown. It then holds nothing of the reader's, and writes nothing (`write`).
   *  No write of theirs is counted out (`sent`) while it does: it draws a reading once none is
   *  (`follow`), or as it takes another person's text in, which ends them all (`arrival`), so the
   *  answer to a write that lands then is the server's text, taken in as any other is. */
  fromTree: boolean;
}

/** The component's props as they stand at its latest render, which the editor follows: the
 *  server's text, the server's reading the editor draws it from (none where it takes the
 *  markdown in), and where a write goes. */
interface Latest {
  readonly description: string;
  /** Sends the write `held` counted out as `entry` (`sent`), made on `on`, over the server's text
   *  the editor last took in (`PriorityDescription`'s `onSave`), and follows what the page
   *  answers of that very write. */
  readonly send: (held: Held, entry: SentText, on: string) => void;
  readonly reading: DisplayTree | undefined;
}

/** A text the editor could not take in, with the reason. */
interface Failure {
  readonly reason: string;
  readonly text: string;
}

/** Stops the write waiting for the typing to rest, if one waits. */
function stop(held: Held): void {
  window.clearTimeout(held.timer);
  held.timer = undefined;
}

/** The reader's text as compared with the server's: `base` while the editor draws a reading. */
function heldText(held: Held): string {
  return held.fromTree ? held.base : held.handle.getMarkdown();
}

/** Writes `markdown`, what the editor holds, at once, made on `on` - the text its edit began on
 *  (`made`), unless Use my text says otherwise - with the text the editor last took in from the
 *  server (`from`): every write of what the editor holds goes through here. An editor that holds
 *  the server's reading of the text (`fromTree`) writes nothing, whatever changed in it: a reading
 *  is a thing to draw, never a text to store. */
function write(held: Held, send: Latest["send"], markdown: string, on = held.made): void {
  stop(held);
  if (held.fromTree) return;
  held.base = markdown;
  held.made = markdown;
  held.madeOn.set(markdown, on);
  const entry = { landed: false, text: markdown };
  held.sent = [...held.sent, entry];
  send(held, entry, on);
}

/** Writes what the editor holds (`write`) where it differs from the text it last matched. */
function save(held: Held, send: Latest["send"]): void {
  stop(held);
  const markdown = held.handle.getMarkdown();
  if (markdown !== held.base) write(held, send, markdown);
}

/** Draws a text in the editor: from the tree the server read it into where one is given
 *  (`tree`), which the editor builds with no parser of its own, and from its markdown where none
 *  is. Throws as `setMarkdown` does, in an editor that takes edits, for a text holding what this
 *  editor may not create yet, and as `setDocumentJSON` does for a tree holding a node or a mark
 *  type this editor has no schema for: a tree is never held to what the editor may create. */
function draw(held: Held, markdown: string, tree: DisplayTree | undefined): void {
  if (tree === undefined) held.handle.setMarkdown(markdown);
  else held.handle.setDocumentJSON(tree as DocumentJSON);
  held.fromTree = tree !== undefined;
}

/** Puts the server's text into the editor as it opens (`draw`), unless the editor holds it
 *  already: an empty description is the empty editor. Throws as `draw` does. */
function take(held: Held, markdown: string, tree: DisplayTree | undefined): void {
  if (held.handle.getMarkdown() !== markdown) draw(held, markdown, tree);
  held.base = held.handle.getMarkdown();
  held.made = held.base;
  held.from = markdown;
}

/** Draws the server's text in an editor already open (`draw`), held then as the text the editor
 *  matches, the text the reader's next edit is made on (`made`), and the server's text the editor
 *  last took in (`from`). A text it cannot take in leaves what the editor holds in it, held all
 *  the same as the text it matches, so that is not written over the text it could not draw as
 *  the editor closes, and `fail` is told why. Either way it stops the write waiting for the typing
 *  to rest, which would write over the server's text, and the save the redraw's own Yjs update
 *  scheduled. */
function redraw(
  held: Held,
  markdown: string,
  tree: DisplayTree | undefined,
  fail: (failure: Failure) => void
): void {
  held.base = held.handle.getMarkdown();
  held.from = markdown;
  try {
    draw(held, markdown, tree);
    held.base = held.handle.getMarkdown();
    held.made = held.base;
  } catch (error) {
    fail({ reason: error instanceof Error ? error.message : String(error), text: markdown });
  }
  stop(held);
}

/** Moves an open editor between the server's text as its markdown and the server's reading of
 *  it, as `latest` draws it now (`redraw`, and the component's own doc), once the writer is
 *  settled: no write of theirs is out, and none of their typing is unwritten. So the answer to a
 *  write of theirs comes back to the markdown it was written from, never to a reading drawn over
 *  it, and a reading is drawn over nothing they typed that is not written. The editor asks it
 *  again whenever one of those may have changed - the server's text or the form it is drawn in,
 *  the typing at rest, the editor or the page left, and a write of theirs ending without landing
 *  - so a move held back is made once the writer is settled. */
function follow(held: Held, latest: Latest, fail: (failure: Failure) => void): void {
  if (held.fromTree === (latest.reading !== undefined) || held.sent.length > 0) return;
  if (heldText(held) !== held.base) return;
  redraw(held, latest.description, latest.reading, fail);
}

/** The typing at rest, or the editor or the page left: writes what the editor holds (`save`),
 *  and then moves it to the form the server's text is drawn in now, where that waited on the
 *  typing (`follow`). */
function rest(held: Held, latest: Latest, fail: (failure: Failure) => void): void {
  save(held, latest.send);
  follow(held, latest, fail);
}

/**
 * A priority's description, drawn and written in the editor Dispatch's documents are written in,
 * where it is read: the markdown it is, drawn as headings, lists and links, with no edit mode and
 * no Save button. It is a field of the priority and not a document: the editor holds a Yjs
 * document of its own, with no connection and no cursors, and writes no Proof mark of its own
 * (`proofMarks: false`) - no comment, suggestion or ask on a selection, and no authorship span
 * in the markdown it writes - and what it holds is written through the priority's own write as
 * markdown, once the typing rests, at once when the editor is left, and as the page goes, so far
 * as the page lets a write start and finish then (`api/client.ts`'s `pageGoing`), as every field
 * of a priority is (`InPlaceField`). The priority's own page and the open row of the priorities
 * table draw it alike, so what a description does does not depend on where it is read.
 *
 * A change of the server's text is read as a one-line field reads one (`arrival`): one that
 * answers one of the reader's own writes changes nothing they see. Another person's text is taken
 * in at once - with the reader in the editor or not - while the reader holds no unwritten change
 * and no write of theirs is out, so leaving the editor never writes the old text back over it.
 * Else it is taken in all the same, the waiting write is stopped, and the reader's text is kept
 * beside the editor (`ChangedMeanwhile`) until they use it again or let it go. Where the page
 * holds those texts (`kept`, `onKeptChange`) - in the write store, as the priority's page and
 * the priorities table do (`usePriorityWrites`) - they outlive the editor, the page and a
 * reload: a row of the table scrolled away, or the page left, and an editor drawn again shows
 * them again, and the page draws the editor while it holds any, a closed disclosure included.
 * Else the editor holds them itself, and they go with it.
 * Each write hands on, with the text written, the server's text the editor last took in
 * (`onSave`'s `from`), which the page names as the text the write goes over (`expected`) unless a
 * write of the page's own goes before it. Only a read moves it, never a write, which may not land,
 * and never the page's own read of the text: the page draws another person's text before the
 * editor takes it in, and a write made in between - the typing at rest, the editor left as it
 * turns read-only, the page going - names the text the reader edited, so Dispatch refuses it, and
 * the reader's text is kept beside the other person's as it is where that text arrives first. Use
 * my text writes the kept text at once, whatever the editor held before. Each write hands on too
 * the text the editor held as its edit began (`onSave`'s `on`, `Held.made`), in the form a
 * write sends, Use my text's included where the editor takes the kept text in - but for Use my
 * text of a text a write of the reader's carried, which is that same edit, and hands on what that
 * write was made on: the editor's own record of it, or, in an editor drawn again, the page's
 * (`keptMadeOn`). One written with no editor that takes it in and no write of it known is made on
 * the server's text it goes over. That is not always the text the editor compares with
 * (`Held.base`): a read that answers an earlier write while the reader's typing waits moves that
 * to the write, but the typing began on what the editor held. An editor drawn again took the
 * server's text in as it opened, so its first edit is made on that, whatever an editor before it
 * wrote.
 * A reader who may not write reads it in the same editor, read-only; with none written, they
 * read that none is, and a writer reads it above the empty editor.
 *
 * The editor is the one control here, a textbox named by `label`, which the keyboard reaches
 * as it reaches any field; the box around it shows the focus ring while the editor holds it.
 * Where a page holds the description in a card of its own (`boxed={false}`), the card is that
 * box: the editor draws no border, only the focus ring, and its words start where the card's do.
 * An editor the reader writes in hosts the reference picker every box has, as a document's body
 * does (`DocumentRefPicker`), under the writing list's row of a priority's description
 * (`DISPATCH_WRITING_BOXES`, `priority-description`), whose words stand under the editor; the
 * references the text holds are drawn over it as a document's are (`DocumentReferences`).
 *
 * The stored description a reader who may not write it reads is drawn from the tree the server
 * read it into (`descriptionTree`), as every other stored text is, so no such reader's browser
 * parses it: Go's reading holds marks nested past its bound as text, where the browser's parser
 * spends the square of their depth. A writer's editor takes the description in as its markdown,
 * since what that editor writes back is its own reading of what it took in with the writer's
 * edits in it (`take`, `save`), and the tree is a thing to draw, never a thing to edit: it gives
 * every attribute the server owns its default, and a block Dispatch's parser refuses is no block
 * of the text in it. So it does where the server says its parser takes the description whole
 * (`descriptionTreeExact`), which says nothing more of the tree, and where the answer carries no
 * word on that: the answer of a pod of the build before this one during a rolling deploy, whose
 * tree a writer's editor took in as markdown before, as it does still, while a reader who may not
 * write keeps the tree. Where the server says its parser does not take the description whole - a
 * block it refuses, or a description past its bounds, read as text no write stores - everyone,
 * a writer too, reads the tree read-only, with a line under it that says Dispatch cannot read the
 * description as markdown, so no browser parses it and nothing is ever written from that reading.
 * An answer that carries no tree at all is taken in as its markdown, by every reader, and so is a
 * text of the reader's own the server has not stored (one put back from beside the editor,
 * `putBack`).
 *
 * An editor already drawn moves between the markdown and the reading as the server's word on its
 * text changes - pods of two builds answer in turn during a rolling deploy - and as the reader
 * comes to write it or stops: to the markdown once the reader writes in it, so no keystroke
 * reaches a reading, and to the reading once they do not, so every reader of a text the server's
 * parser does not take whole is shown the server's reading. A writer's editor moves once they are
 * settled (`follow`): with a write of theirs out, its answer would come back to a reading drawn
 * over the markdown it was written from, and a reading would draw over typing not yet written.
 * So the typing is written, as typed, and the editor moves once the answer is in, or once the page
 * says the write ended without landing (`onSave`'s answer), when no answer is coming. An editor
 * drawn from a reading holds nothing of the reader's: no text of it is kept beside the editor as
 * theirs, and none is written.
 */
export function PriorityDescription({
  boxed = true,
  canWrite,
  description,
  descriptionTree,
  descriptionTreeExact,
  kept: keptByPage,
  keptMadeOn,
  label,
  onKeptChange,
  onSave,
}: {
  /** Whether the editor draws a box of its own; a page that draws it in a card draws none. */
  boxed?: boolean;
  canWrite: boolean;
  description: string;
  /** The server's reading of `description` (the priority's `description_tree`), absent from an
   *  answer that carries none: a read made without `Dispatch-Trees: 1`, and an empty
   *  description. */
  descriptionTree: DisplayTree | undefined;
  /** Whether the server's parser takes `description` whole (the priority's
   *  `description_tree_exact`), so a writer's editor may take its markdown in. It says nothing
   *  more of `descriptionTree`, which gives every attribute the server owns its default either
   *  way. It stands exactly where the tree does, except in the answer of a pod of the build
   *  before this one, which sends a tree and no word on it. */
  descriptionTreeExact: boolean | undefined;
  /** The reader's texts kept beside the editor, where the page holds them rather than the
   *  editor - in the write store (`usePriorityWrites`): `onKeptChange` hears each change of
   *  them. */
  kept?: readonly string[];
  /** What the write of `text`, one of the kept texts the page holds, was made on, where the page
   *  holds that write and it recorded that: an editor drawn again wrote none of them itself. */
  keptMadeOn?: (text: string) => string | undefined;
  /** What the description is called, which names the priority it belongs to: "Description of
   *  #3". The editor carries it as its accessible name. */
  label: string;
  onKeptChange?: (kept: readonly string[]) => void;
  /** Writes `next`, the reader's text, over `from`, the server's text the editor last took in,
   *  which the write names as the text it goes over (`expected`) unless a write of the page's own
   *  goes before it, made on `on`: the text the editor held as the edit began, or, for Use my text
   *  of a text whose own write is known, what that write was made on. Answers whether the write
   *  landed: until it does, the editor counts the write as out (`sent`). */
  onSave: (next: string, from: string, on: string) => Promise<boolean>;
}): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const held = useRef<Held | undefined>(undefined);
  // The server's text the editor last took in, or saw arrive, whether an editor was drawn for it
  // or not: Use my text with no editor drawn writes over it (`putBack`).
  const seen = useRef(description);
  // The reader's own texts, kept beside the editor while it shows another person's, where the
  // page does not hold them.
  const [ownKept, setOwnKept] = useState(NOTHING_KEPT);
  const kept = keptByPage ?? ownKept;
  const setKept = onKeptChange ?? setOwnKept;
  // The server's reading of a description it cannot read as markdown: drawn read-only for every
  // reader, a writer too, and never written back.
  const unreadable = descriptionTree !== undefined && descriptionTreeExact === false;
  // Whether the reader writes in the editor: one who may write the description, where the
  // server's parser takes it whole or the server says nothing of that.
  const writes = canWrite && !unreadable;
  // The tree the editor draws the server's text from (`draw`): none for an editor the reader
  // writes in, which takes its markdown in, and the server's reading for every other.
  const reading = writes ? undefined : descriptionTree;
  // Whether the editor takes the text in as its markdown rather than as a reading, which an
  // editor drawn already comes to follow (the effects below).
  const fromMarkdown = reading === undefined;
  // The handle once the editor holds the description: its name, its access and the references
  // drawn over its text follow it.
  const [editor, setEditor] = useState<EditorHandle | undefined>(undefined);
  // A text the editor could not take in, with the reason, until the server's text changes.
  const [failure, setFailure] = useState<Failure>();
  // Each write the editor makes goes through the page (`onSave`), which answers whether that very
  // write landed (`answered`): one that did not never comes back, so the editor waits for no
  // answer with it, and moves to the form the server's text is drawn in where that waited on it
  // (`follow`), while it is still the editor drawn.
  const send = (current: Held, entry: SentText, on: string) => {
    void onSave(entry.text, current.from, on).then((landed) => {
      current.sent = answered(current.sent, entry, landed);
      if (!landed && held.current === current) follow(current, latest.current, setFailure);
    });
  };
  const latest = useRef({ description, kept, reading, send, setKept, writes });
  latest.current = { description, kept, reading, send, setKept, writes };
  const { createEditor } = useContext(DocumentRuntime);
  const login = useQuery(whoAmIQuery()).data?.login;
  const queryClient = useQueryClient();
  const followReference = useReferenceClick();
  // The id of the line under a reading of a description the server's parser does not take whole,
  // which the editor names as its description.
  const why = useId();

  const none = description.trim() === "";
  const failed = failure?.text === description ? failure.reason : undefined;
  // A reader who may not write it has nothing to read in an empty editor.
  const drawn = failed === undefined && (canWrite || !none);

  useEffect(() => {
    const parent = root.current;
    if (!drawn || parent === null || login === undefined) return;
    let mounted = true;
    let ydoc: Doc | undefined;
    let handle: EditorHandle | undefined;
    // Every edit reaches the editor's Yjs document, the reader's and a draw of the server's text
    // alike; `save` writes only what differs from that text, and the editor then moves to the
    // form the text is drawn in where that waited on the typing (`rest`).
    const onUpdate = () => {
      const current = held.current;
      if (current === undefined) return;
      stop(current);
      current.timer = window.setTimeout(
        () => rest(current, latest.current, setFailure),
        SAVE_AFTER_MS
      );
    };
    const mount = async () => {
      const doc = await createDoc();
      if (!mounted) {
        doc.destroy();
        return;
      }
      ydoc = doc;
      const created = await createEditor(parent, {
        access: latest.current.writes ? "edit" : "read",
        awareness: null,
        blockSchema: compiledBlockSchema,
        heatMapMode: "hidden",
        // A description is no document: it holds no decision block a card is drawn for.
        onAskBlockHosts: () => {},
        proofMarks: false,
        user: { color: colorForLogin(login), name: login },
        watchPicture: (route, onPicture) => watchPicturePath(queryClient, route, onPicture),
        ydoc: doc,
      });
      handle = created;
      if (!mounted) {
        void created.destroy();
        return;
      }
      const current: Held = {
        base: "",
        from: "",
        fromTree: false,
        handle: created,
        made: "",
        madeOn: new Map(),
        sent: [],
        timer: undefined,
      };
      take(current, latest.current.description, latest.current.reading);
      seen.current = latest.current.description;
      held.current = current;
      doc.on("update", onUpdate);
      setEditor(created);
    };
    mount().catch((error: unknown) => {
      void handle?.destroy();
      handle = undefined;
      if (mounted) {
        setFailure({
          reason: error instanceof Error ? error.message : String(error),
          text: latest.current.description,
        });
      }
    });

    return () => {
      mounted = false;
      // Leaving the page, or closing the disclosure, leaves the editor: what the reader wrote
      // is written now rather than lost with the timer.
      const current = held.current;
      held.current = undefined;
      if (current !== undefined) save(current, latest.current.send);
      ydoc?.off("update", onUpdate);
      void handle?.destroy();
      ydoc?.destroy();
      setEditor(undefined);
    };
  }, [createEditor, drawn, login, queryClient]);

  // The documents' editor is named "Document editor"; this one is named for the description it
  // holds, says it is read-only where the reader does not write in it, and is described by the
  // line that says why where the server cannot read the description as markdown. It runs before
  // a text that arrives is drawn (the effects below), so a text an editor the reader writes in
  // takes in as markdown is held to what that editor may create.
  useEffect(() => {
    if (editor === undefined) return;
    editor.setAccess(writes ? "edit" : "read");
    const box = editor.view.dom;
    box.setAttribute("aria-label", label);
    if (writes) box.removeAttribute("aria-readonly");
    else box.setAttribute("aria-readonly", "true");
    if (unreadable) box.setAttribute("aria-describedby", why);
    else box.removeAttribute("aria-describedby");
  }, [editor, label, unreadable, why, writes]);

  // The server's text, as it changes under the editor (`arrival`, and the component's own doc).
  // An editor drawn from a reading holds nothing of the reader's - what it holds is the
  // server's reading, never a text of theirs to keep beside it - so it is compared as the text
  // it last matched (`heldText`). With no editor drawn - one that could not draw a text, or one
  // still opening - the text is seen all the same, and an editor drawn later takes it in as it
  // opens (`take`).
  useEffect(() => {
    if (description === seen.current) return;
    seen.current = description;
    const current = held.current;
    if (current === undefined) return;
    const { kept: mine, setKept: keep } = latest.current;
    const next = arrival(description, heldText(current), {
      base: current.base,
      kept: mine,
      made: current.made,
      sent: current.sent,
    });
    current.sent = next.sent;
    current.base = next.base;
    // What the reader's next edit is made on (`FieldStand.made`): for a text the editor takes in,
    // `redraw` below sets it again, in the editor's own form.
    current.made = next.made;
    // The server holds this text, whatever the editor made of it: the text a write names from
    // here, until the next read.
    current.from = description;
    if (next.kept !== mine) keep(next.kept);
    if (next.take) redraw(current, description, latest.current.reading, setFailure);
  }, [description]);

  // The form the editor holds the text in follows `reading` (`follow`): as the form the server's
  // text is drawn in changes, and after each text that arrives (the effect above), which may
  // answer the last write of the writer's still out and leave them settled.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the text and the form it is drawn in are the triggers; the editor reads both as they stand (`latest`)
  useEffect(() => {
    const current = held.current;
    if (current !== undefined) follow(current, latest.current, setFailure);
  }, [description, fromMarkdown]);

  // The page going is the editor left, as a blur is: a small write that starts now is let finish
  // as the page goes (`keepalive`, `api/client.ts`'s `pageGoing`); one that waits behind a write
  // of the same priority still out, or behind a kept write of the same priority a waiting
  // take-up will send (`Writes.run`), does not start before the page is gone. The tab keeps it
  // too, for a reload to make (`features/writes/write-storage.ts`).
  useEffect(() => {
    const leave = () => {
      if (held.current !== undefined) rest(held.current, latest.current, setFailure);
    };
    window.addEventListener("pagehide", leave);
    return () => window.removeEventListener("pagehide", leave);
  }, []);

  // One of the reader's kept texts, put back in the editor from its markdown, since the server
  // holds no reading of it, and written at once (`write`), even where the editor holds it
  // already: the reader chose it over the other person's text, in place of what the editor held,
  // which it is made on - unless a write of hers carried it, known to this editor or to the page:
  // it is that same edit then, made on what that write was made on, and through each kept text
  // the click lets go (`usedOn`). With no editor the reader writes in - another person's text it
  // could not draw, or one the server cannot read as markdown, drawn read-only from the server's
  // reading - it is written as it is, over the server's text the editor last took in, or with no
  // editor drawn, the last that arrived, and made on that text where no write of it is known, and
  // the server's text stays drawn until the answer replaces it.
  const putBack = (text: string) => {
    const current = held.current;
    const known = usedOn(text, kept, (one) => current?.madeOn.get(one) ?? keptMadeOn?.(one));
    setKept(NOTHING_KEPT);
    if (current === undefined || !writes) {
      const over = current?.from ?? seen.current;
      void onSave(text, over, known ?? over);
      return;
    }
    draw(current, text, undefined);
    write(current, send, current.handle.getMarkdown(), known ?? current.made);
  };

  return (
    <>
      {none ? (
        <p className={textMutedOnCanvas}>
          {canWrite ? `${NONE_WRITTEN} Write one below.` : NONE_WRITTEN}
        </p>
      ) : null}
      {failed === undefined ? null : (
        <p className={dangerText} role="alert">
          This description could not be drawn: {failed}
        </p>
      )}
      {drawn ? (
        // biome-ignore lint/a11y/useKeyWithClickEvents lint/a11y/noStaticElementInteractions: dispatch:// link clicks bubble here, as they do on a document; the editor handles the keyboard's activation of its own links.
        <div
          className={`dispatch-doc dispatch-description rounded-md px-2 py-1 has-focus-visible:ring-2 ${boxed ? `border ${borderDefault}` : "-mx-2"} ${hasFocusVisibleRing}`}
          data-read-only={!writes}
          onBlur={(event) => {
            if (
              event.relatedTarget instanceof Node &&
              event.currentTarget.contains(event.relatedTarget)
            )
              return;
            if (held.current !== undefined) rest(held.current, latest.current, setFailure);
          }}
          onClick={followReference}
        >
          <div ref={root} />
          <DocumentReferences editor={editor} source={undefined} />
          <DocumentRefPicker editor={editor} enabled={writes} writing={DESCRIPTION_BOX} />
        </div>
      ) : null}
      {drawn && unreadable ? (
        <p className={textMutedOnCanvas} id={why}>
          {NOT_MARKDOWN}
        </p>
      ) : null}
      {kept.length === 0 ? null : (
        <ChangedMeanwhile
          label={label}
          mine={kept}
          onKeepTheirs={() => setKept(NOTHING_KEPT)}
          onUse={putBack}
        />
      )}
    </>
  );
}
