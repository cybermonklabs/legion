import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import {
  type FakeDocumentRuntime,
  type FakeEditor,
  fakeDocumentRuntime,
} from "../../../__tests__/document-runtime";
import { answerRequests } from "../../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../../__tests__/proof-document";
import { api } from "../../../api/client";
import type { CreateEditor, EditorOptions } from "../../doc/editor";
import { DocumentRuntime } from "../../doc/runtime";
import { Margin } from "../../margin/Margin";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { DETAILS_ATTRIBUTE } from "../margin-slot";
import { StagedDocument } from "../StagedDocument";
import { blockNodeId } from "../tree-drafts";
import {
  answerSeededReads,
  columnOf,
  type DrawnPage,
  drawPage,
  newClient,
  ON_STATEMENT,
  rowOf,
  STATEMENT,
  STATEMENT_WORDS,
} from "./margin-fixtures";
import { TEXT_COMPOSER_ATTRIBUTE, THREAD_NODE_ATTRIBUTE } from "./TextMargin";

/**
 * R5 and R6: in a block's editor, the editor's own selection bar - Comment, Suggest and Ask - and
 * the chord `$mod+Alt+m` open the composer of comments on text in the column, beside the block's
 * node, with the kind the bar chose, and never in Details, which draws no composer and no thread
 * of the editor on the new page. The send names the mark the editor wrote, `artifact` and
 * `mark_id` alone (plan decision 10).
 *
 * Each case draws the new page at Conversation with Details' drawer, the shell's margin in it
 * (`Margin`), and the block's editor this base has: the editor of the spec that the body draws for
 * a row being edited (`StagedDocument`), with a fake room and editor (`fakeDocumentRuntime`). The
 * bar's actions reach the page through the editor's own hook (`onMarkAction`), with the mark the
 * bar wrote over the words of the statement, as the real editor hands them; the chord is pressed
 * through the page's keymap on the focused editor, whose Comment on the selection does what the
 * real editor's does (`chordRuntime`). Details is open when the editor asks, so each case reads
 * that it closes first. Every send is held out (`pending`), so the composer's state after Send is
 * what is read.
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** The statement's row, whose block the editor's selection is in. */
const STATEMENT_NODE = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);

/** The words of the statement the editor's selection takes, and where the bar read them in the
 *  statement's block. */
const QUOTE = "draws the plan";
const PLACEMENT = { blockId: STATEMENT, prefix: "The page ", readings: 1, suffix: "." };

/** What the editor hands the page for one of its selection bar's actions (`onMarkAction`). */
type BarAction = Parameters<NonNullable<EditorOptions["onMarkAction"]>>[0];

/** The bar's `kind` over the statement's words, with the provisional mark `markId` it wrote. */
function barAction(kind: "ask" | "comment" | "suggest", markId: string): BarAction {
  return { from: 9, kind, markId, placement: PLACEMENT, quote: QUOTE, to: 23 };
}

/** How the editor's promise for one action stands: the page resolves it once the compose's send
 *  is out, which keeps the mark, and rejects it as the composer closes unsent. */
interface Asked {
  state: "pending" | "rejected" | "resolved";
}

/** Follows `promise`, the page's answer to one action of the editor's. */
function asked(promise: unknown): Asked {
  const held: Asked = { state: "pending" };
  Promise.resolve(promise).then(
    () => {
      held.state = "resolved";
    },
    () => {
      held.state = "rejected";
    }
  );
  return held;
}

/**
 * A fake room and editor whose editor's Comment on the selection - what the chord runs
 * (`ProofDocument`'s `comment-on-selection`) - does what the real editor's does: it writes a
 * provisional mark over the selected words, here `m-chord` over the statement's, and hands it to
 * the page as the bar's Comment does (`onMarkAction`). `answers` holds the page's answer.
 */
function chordRuntime(): { readonly answers: Asked[]; readonly fake: FakeDocumentRuntime } {
  const fake = fakeDocumentRuntime({ text: STATEMENT_WORDS });
  const answers: Asked[] = [];
  const createEditor: CreateEditor = async (root, options) => {
    const handle = await fake.runtime.createEditor(root, options);
    return Object.assign(handle, {
      commentOnSelection: () => {
        answers.push(asked(options.onMarkAction?.(barAction("comment", "m-chord"))));
        return true;
      },
    });
  };
  return { answers, fake: { ...fake, runtime: { ...fake.runtime, createEditor } } };
}

let stubs: { mockRestore(): void }[] = [];
answerTextReadsWithTheSeededText();
beforeAll(async () => {
  await warmMarkdownRenderer();
});
beforeEach(() => {
  // The spec's own reads, which its editor makes: its record, its blocks and the reader's read
  // mark on it.
  answerRequests(
    artifactRecordHandler((id) => (id === SPEC_DOCUMENT.id ? SPEC_DOCUMENT : undefined)),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    ...decisionsAndReadMarkHandlers()
  );
});
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** Every send the composer makes, held out: a comment's or a suggestion's, and an ask's. */
function pending(): {
  readonly ask: Mock<typeof api.createAsk>;
  readonly comment: Mock<typeof api.createComment>;
} {
  const out = Promise.withResolvers<never>();
  const comment = spyOn(api, "createComment").mockReturnValue(out.promise);
  const ask = spyOn(api, "createAsk").mockReturnValue(out.promise);
  stubs.push(comment, ask);
  return { ask, comment };
}

/** The new page at Conversation with Details' drawer and the shell's margin in it, the spec's
 *  editor of `fake` beside it as the body draws it for a row being edited, once the statement's
 *  row is drawn and the editor is made. The issue holds the comment on the statement's words, so
 *  the column is drawn with its thread. */
async function drawn(fake: FakeDocumentRuntime): Promise<{
  readonly client: QueryClient;
  readonly editor: FakeEditor;
  readonly view: DrawnPage;
}> {
  // The shell's margin reads the reader's inbox beside the page's own reads.
  stubs = [
    ...answerSeededReads({ threads: [ON_STATEMENT] }),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
  const client = newClient();
  const view = drawPage(client, "conversation", {
    beside: (
      <DocumentRuntime.Provider value={fake.runtime}>
        <StagedDocument
          artifact={SPEC_DOCUMENT}
          isClosed={false}
          issueKey={KEY}
          onToolbarChange={() => {}}
          onVersionChange={() => {}}
          showDiff={false}
        />
        <Margin />
      </DocumentRuntime.Provider>
    ),
    drawer: true,
  });
  await waitFor(() =>
    expect(rowOf(view, STATEMENT_NODE), "the statement's row is not drawn").not.toBeNull()
  );
  await waitFor(() => expect(fake.connections, "the editor opened no room").toHaveLength(1));
  await act(async () => fake.sync());
  const editor = await waitFor(() => {
    const made = fake.editors[0];
    expect(made, "the spec's editor was not made").toBeDefined();
    return made as FakeEditor;
  });
  return { client, editor, view };
}

/** Details' drawer, the shell's margin (`Margin`). */
function detailsOf(view: RenderResult): HTMLElement {
  const details = view.container.querySelector<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`);
  expect(details, "the shell's Details drawer is not drawn").not.toBeNull();
  return details as HTMLElement;
}

/** Opens Details, and fails where it draws a composer, or a thread's card, of the editor. */
async function detailsHoldNone(view: DrawnPage, said: string): Promise<void> {
  await act(async () => view.held.drawer?.setOpen(true));
  const details = detailsOf(view);
  expect(details.hidden, `${said}: Details did not open`).toBe(false);
  expect(
    within(details).queryAllByRole("form", { hidden: true, name: "Comment composer" }),
    `${said}: Details draws a composer`
  ).toEqual([]);
  expect(
    details.querySelectorAll("[data-margin-item]").length,
    `${said}: Details draws a thread`
  ).toBe(0);
}

/** The composer in the column, once it is drawn there, beside the statement's node. */
async function composerBeside(view: RenderResult, said: string): Promise<HTMLElement> {
  const column = await columnOf(view, COLUMN);
  expect(column, `${said}: no region "${COLUMN}" is drawn`).not.toBeNull();
  return waitFor(() => {
    const composer = (column as HTMLElement).querySelector<HTMLElement>(
      `[${TEXT_COMPOSER_ATTRIBUTE}]`
    );
    expect(composer, `${said}: the region "${COLUMN}" holds no composer`).not.toBeNull();
    expect(
      composer?.getAttribute(THREAD_NODE_ATTRIBUTE),
      `${said}: the composer stands beside another node than the statement's`
    ).toBe(STATEMENT_NODE);
    return composer as HTMLElement;
  });
}

/** Waits for Details to close. */
async function detailsClosed(view: DrawnPage, said: string): Promise<void> {
  await waitFor(() => expect(view.held.drawer?.open, `${said}: Details stays open`).toBe(false));
}

/** Types into each of `fields` of `composer` and presses its Send. */
async function send(
  composer: HTMLElement,
  fields: readonly string[],
  user: UserEvent
): Promise<void> {
  for (const name of fields) {
    await user.type(within(composer).getByRole("textbox", { name }), "Why this word?");
  }
  await user.click(composer.querySelector<HTMLElement>('button[type="submit"]') as HTMLElement);
}

/** The anchor of the last call of `create`, once it has been called `count` times. */
async function anchorSent(
  create: Mock<typeof api.createComment> | Mock<typeof api.createAsk>,
  count: number,
  said: string
): Promise<unknown> {
  await waitFor(() =>
    expect(create, `${said}: the composer sent nothing`).toHaveBeenCalledTimes(count)
  );
  return (create.mock.calls.at(-1)?.[1] as { anchor?: unknown } | undefined)?.anchor;
}

/** Each action of the bar, the composer's fields of its kind, and the request its send makes. */
const BAR = [
  { action: "comment", fields: ["Comment"], request: "comment" },
  { action: "suggest", fields: ["Replacement", "Reason"], request: "comment" },
  { action: "ask", fields: ["Question"], request: "ask" },
] as const;

test("the editor's Comment, Suggest and Ask open the column's composer", async () => {
  const user = userEvent.setup();
  const fake = fakeDocumentRuntime({ text: STATEMENT_WORDS });
  const { client, editor, view } = await drawn(fake);
  const sends = pending();
  const onMarkAction = editor.options.onMarkAction;
  try {
    expect(onMarkAction, "the editor was made with no hook for its bar's actions").toBeDefined();
    await act(async () => view.held.drawer?.setOpen(true));
    const sent = { ask: 0, comment: 0 };
    for (const { action, fields, request } of BAR) {
      const markId = `m-${action}`;
      let answer: Asked = { state: "pending" };
      act(() => {
        answer = asked(onMarkAction?.(barAction(action, markId)));
      });
      await detailsClosed(view, `the bar's ${action}`);
      const composer = await composerBeside(view, `the bar's ${action}`);
      for (const name of fields) {
        expect(
          within(composer).queryByRole("textbox", { name }),
          `the bar's ${action} opened a composer of another kind: no field "${name}"`
        ).not.toBeNull();
      }
      expect(
        composer.textContent,
        `the bar's ${action}: the composer is not headed by the words`
      ).toContain(QUOTE);

      await send(composer, fields, user);
      sent[request] += 1;
      expect(
        await anchorSent(sends[request], sent[request], `the bar's ${action}`),
        `the bar's ${action}: the send names more or less than the editor's mark`
      ).toStrictEqual({ artifact: SPEC_DOCUMENT.id, mark_id: markId });
      await waitFor(() =>
        expect(
          answer.state,
          `the bar's ${action}: the editor was not told to keep its mark once the send was out`
        ).toBe("resolved")
      );

      await detailsHoldNone(view, `after the bar's ${action}`);
    }
  } finally {
    view.finish();
    client.clear();
  }
});

test("the chord in a block's editor opens the column's composer, not Details'", async () => {
  const user = userEvent.setup();
  const { answers, fake } = chordRuntime();
  const { client, editor, view } = await drawn(fake);
  const sends = pending();
  try {
    await act(async () => view.held.drawer?.setOpen(true));
    // The editor holds the focus and a selection, as a reader's caret in it and Shift with an
    // arrow leave it.
    editor.root.contentEditable = "true";
    editor.root.tabIndex = 0;
    act(() => editor.root.focus());
    editor.selectionEmpty = false;
    await user.keyboard("{Control>}{Alt>}m{/Alt}{/Control}");

    expect(answers, "the chord in the editor asked for no composer").toHaveLength(1);
    await detailsClosed(view, "the chord");
    const composer = await composerBeside(view, "the chord");
    expect(
      within(composer).queryByRole("textbox", { name: "Comment" }),
      "the chord opened a composer of another kind than a comment"
    ).not.toBeNull();

    await send(composer, ["Comment"], user);
    expect(
      await anchorSent(sends.comment, 1, "the chord"),
      "the chord: the send names more or less than the editor's mark"
    ).toStrictEqual({ artifact: SPEC_DOCUMENT.id, mark_id: "m-chord" });
    await waitFor(() =>
      expect(
        answers[0]?.state,
        "the chord: the editor was not told to keep its mark once the send was out"
      ).toBe("resolved")
    );

    await detailsHoldNone(view, "after the chord");
  } finally {
    view.finish();
    client.clear();
  }
});
