import { afterEach, expect, jest, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { ID_AMBIGUOUS } from "@legion/contracts";
import { macBaseKeymap } from "@milkdown/kit/prose/commands";
import { Schema } from "@milkdown/kit/prose/model";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import {
  focusManager,
  QueryClient,
  QueryClientProvider,
  type QueryKey,
} from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import { type CSSProperties, type ReactNode, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { MemoryRouter, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";
import { prosemirrorJSONToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { correction } from "../../__tests__/correction-feed";
import { displayTree } from "../../__tests__/display-trees";
import { jump, realClock } from "../../__tests__/fake-clock";
import { answerRequests } from "../../__tests__/network";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import { markCorrectedPost } from "../../api/corrected-post";
import { primarySpec } from "../../api/issue-cache";
import { issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type {
  Artifact,
  Comment,
  Decision,
  IDCandidate,
  IssueDecisions,
  IssueDetails,
} from "../../api/types";
import { pageFocusedRow } from "../../theme/classes";
import { bindRemoteMarks, createEditor, type EditorHandle } from "../doc/editor";
import { compiledBlockSchema } from "../doc/schema";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildDispatchReference, buildIssuePath, parseIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { agentsOnlyIds } from "./agents-only";
import { decisionsRead, issueDetails } from "./decisions-filter/decisions-fixtures";
import {
  DocumentAdmissionProvider,
  type ReportAdmission,
  useReportAdmission,
} from "./document-admission";
import { DocumentStage, DocumentStages } from "./document-stage";
import { type FocusedNode, FocusedNodeProvider, useFocusedNode } from "./focused-node";
import { IssueTree, LANDING_WAIT_MS } from "./IssueTree";
import { CLEARANCE, FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import { issueAddress, presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import * as nodeCaret from "./node-caret";
import { EDITOR_BOX_ATTRIBUTE, MAC_PLATFORM } from "./node-caret";
import { framesByHand, readComment, TWO_THREADS, waitInAct } from "./outline-fixtures";
import {
  OutlineRequestProvider,
  useAskOutline,
  useFocusBranch,
  useOutlineRequest,
} from "./outline-request";
import {
  type Landing,
  type OutlineWriter,
  OutlineWriterProvider,
  useOutlineWriter,
} from "./outline-writer";
import { KEY_REPORTS } from "./page-structure";
import {
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  NODE_TYPE_NAMES,
  type NodeType,
  PRESET_ORDER,
  type Preset,
  STATE_LINES,
} from "./page-words";
import { EditorBox } from "./StagedDocument";
import type { SlotProps } from "./slot-props";
import { BodySlot } from "./slots/body";
import { FAILED_READ_RETRY } from "./slots/failed-read-line";
import { ProvidersSlot } from "./slots/providers";
import { SLOT_FILLED, StageSlot, VIEW_SLOTS, type ViewSlots } from "./slots/stage";
import { addressesAsShortForms } from "./tree-drafts";
import { TAB_STOP_ROW } from "./tree-focus";
import { commentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import {
  issueTreeQuery,
  outlineQuery,
  type TreeAnswer,
  type TreeAnswerNode,
} from "./tree-read/tree-read";
import { issueAskThreadsQuery } from "./useIssueTree";
import type { OnEditing, OnEditor } from "./useNodeEditing";
import { overflowing } from "./words-scroll-fixtures";

/**
 * What a reader does to the tree with the keyboard alone.
 *
 * The tree is rendered here rather than through `IssuePage`, because what these rows are about
 * is where the focus goes and which control a key presses, and that needs a tree whose shape -
 * one node under two parents, two open questions in different branches - the page's own reads
 * would have to be fabricated backwards to produce.
 */

const KEY = "CORE-1";

/** The issue's spec, whose `decision` blocks are the agent's decisions: the document a Keep is
 *  written against and a Change is anchored in. */
const specDocument: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-spec",
  issue_key: KEY,
  kind: "doc",
  name: "Spec",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

/** The agent's one decision: a `decision` block of the spec's `## Decisions` part, whose entry the
 *  decisions read answers (`decisionsRead`), drawn as the spec's block node `spec#b-d1`. */
const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "Ship behind a flag",
  kept: null,
  kind: "agent",
  links: [],
  ref: `dispatch://${KEY}/spec#b-d1`,
  replaced: false,
  text: ":::decision{#d1}\n### Ship behind a flag\n\nWe ship it behind a flag.\n:::\n",
  text_sha256: "sha256:one",
  unsure: false,
  waiting_review: true,
};

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** happy-dom answers every media query against its own 1024 px window. A test sets the width the
 *  page reads: a `max-width` in px, a `min-width` in rem, and `not all and` either. The outline
 *  draws the same rows at every width (R11). */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Where the page's address stands, which the tree's own navigation moves (`t h`). */
function AddressProbe({ seen }: { seen: { path?: string } }): ReactNode {
  seen.path = useLocation().pathname;
  return null;
}

/** What the page's panel would draw the facts of: the node the tree last set as focused. */
function FocusedNodeProbe({ seen }: { seen: { node?: FocusedNode } }): ReactNode {
  seen.node = useFocusedNode();
  return null;
}

/** How the page's row editor reports what a document's room admitted the page to
 *  (`document-admission.tsx`), handed out for a test to answer as the room. */
function AdmissionProbe({ held }: { held: { report?: ReportAdmission } }): ReactNode {
  held.report = useReportAdmission();
  return null;
}

/** What stands in for the editor of the document a row is editing in: a plain element, or what
 *  takes how to hand the row its editor (`onEditing`) and draws one - the document's own
 *  (`LiveEditor`), or one that hands the row nothing. */
type StandInEditor = ReactNode | ((onEditor: OnEditor) => ReactNode);

/** What a page whose tree hands it nothing to take the editor with hands the editor to. */
const takesNoEditor = () => {};

/** A ProseMirror view of an empty document, which a plain element standing in for the document's
 *  editor hands the row as its editor: a plain element is no ProseMirror editor, and the row puts
 *  a caret in the stand-in only once its hold is on the editor it was handed
 *  (`holdEditsToBlock`), which goes on this view. */
function standInView(): EditorView {
  return new EditorView(null, {
    state: EditorState.create({
      schema: new Schema({ nodes: { doc: { content: "text*" }, text: {} } }),
    }),
  });
}

/** A plain element standing in for the document's editor, handed to the row (`standInView`) as
 *  the page hands its editor over: as it is made, and `undefined` as it goes. */
function HandedStandIn({
  children,
  onEditor,
}: {
  children: ReactNode;
  onEditor: OnEditor;
}): ReactNode {
  useEffect(() => {
    const view = standInView();
    onEditor({ view });
    return () => {
      view.destroy();
      onEditor(undefined);
    };
  }, [onEditor]);
  return children;
}

/** The page's one editor of the document a row of the tree is editing in, as `IssuePresetPanels`
 *  draws it for the row to take onto its stage, in the box it scrolls in there, with `editor`
 *  standing in for the document's own editor. `held.set` is how the tree's `onEditing` reaches
 *  it. */
function EditedDocument({
  editor,
  held,
}: {
  editor: StandInEditor;
  held: { set?: OnEditing };
}): ReactNode {
  const [edited, setEdited] = useState<{ artifactId: string; onEditor?: OnEditor } | undefined>(
    undefined
  );
  held.set = (artifactId, onEditor) =>
    setEdited(artifactId === undefined ? undefined : { artifactId, onEditor });
  const onEditor = edited?.onEditor ?? takesNoEditor;
  return edited === undefined ? null : (
    <DocumentStage artifactId={edited.artifactId}>
      <EditorBox>
        {typeof editor === "function" ? (
          editor(onEditor)
        ) : (
          <HandedStandIn onEditor={onEditor}>{editor}</HandedStandIn>
        )}
      </EditorBox>
    </DocumentStage>
  );
}

/** The tree alone, under the page's keymap and the focused node it sets. `reveal` is the chain
 *  down to the node an address names, which the tree is then to put the focus on
 *  (`slots/body.tsx`); `editor` stands in for the editor of the document a row is editing in,
 *  which the page draws on that row's stage; `emptyLine` is what the tree says where the issue
 *  holds nothing, none where it is not given; `isClosed` is whether the issue is closed;
 *  `readsHeld` is whether the reads the tree is built from hold their answers, until `holdReads`
 *  draws the same tree with them held; `redraw` draws the same tree again, as a read answering
 *  does, and `reread` draws it over another tree, as a read answering with other nodes does;
 *  `admit` answers for a document's room, as the page's row editor reports what that room
 *  admitted the page to (`document-admission.tsx`). */
function renderTree(
  root: TreeNode,
  {
    artifacts = [],
    decisions,
    editor,
    emptyLine,
    isClosed = false,
    readsHeld = true,
    readsSettled = true,
    reveal,
  }: {
    artifacts?: Artifact[];
    decisions?: IssueDecisions;
    editor?: StandInEditor;
    emptyLine?: string;
    isClosed?: boolean;
    readsHeld?: boolean;
    readsSettled?: boolean;
    reveal?: readonly TreeNode[];
  } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  if (decisions !== undefined) client.setQueryData(["decisions", KEY], decisions);
  const edited: (string | undefined)[] = [];
  const reads = { held: readsHeld, settled: readsSettled };
  const tree = { root };
  const editedDocument: { set?: OnEditing } = {};
  const seen: { path?: string } = {};
  const focused: { node?: FocusedNode } = {};
  const admission: { report?: ReportAdmission } = {};
  const draw = () => (
    <MemoryRouter>
      <AddressProbe seen={seen} />
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <DocumentAdmissionProvider>
                <DocumentStages>
                  <IssueScope />
                  <FocusedNodeProbe seen={focused} />
                  <AdmissionProbe held={admission} />
                  <IssueTree
                    artifacts={new Map(artifacts.map((artifact) => [artifact.slug, artifact]))}
                    context={new Set()}
                    emptyLine={emptyLine}
                    focusReveal={reveal !== undefined}
                    holdsEscape={false}
                    isClosed={isClosed}
                    onEditing={(artifactId, onEditor) => {
                      edited.push(artifactId);
                      editedDocument.set?.(artifactId, onEditor);
                    }}
                    readsHeld={reads.held}
                    readsSettled={reads.settled}
                    reveal={reveal}
                    root={tree.root}
                    rootRow
                  />
                  {editor === undefined ? null : (
                    <EditedDocument editor={editor} held={editedDocument} />
                  )}
                </DocumentStages>
              </DocumentAdmissionProvider>
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw());
  return {
    address: () => seen.path,
    admit: (artifactId: string, readOnly: boolean) =>
      act(async () => admission.report?.(artifactId, readOnly)),
    edited,
    focusedNode: () => focused.node,
    holdReads: () => {
      reads.held = true;
      view.rerender(draw());
    },
    redraw: () => view.rerender(draw()),
    reread: (next: TreeNode) => {
      tree.root = next;
      view.rerender(draw());
    },
    /** Draws `next` with every read answered, as the last read to answer draws it. */
    settle: (next: TreeNode) => {
      tree.root = next;
      reads.held = true;
      reads.settled = true;
      view.rerender(draw());
    },
    ...view,
  };
}

/** What the page-body harness hands back from inside the providers: the Contents list's ask and
 *  its Focus branch's setter, the row writer, and the switch that takes the tree off the page and
 *  draws it again. */
interface PageHold {
  ask?: (id: string | undefined, focusBranch: boolean) => void;
  focusBranch?: (next: boolean) => void;
  show?: (shown: boolean) => void;
  writer?: OutlineWriter;
}

/** The tree as `slots/body.tsx` draws it: from the last request of the page's
 *  `OutlineRequestProvider`, under the page's row writer, while it is shown. */
function PageBody({ drawn, held }: { drawn: TreeNode; held: PageHold }): ReactNode {
  held.ask = useAskOutline();
  held.focusBranch = useFocusBranch()[1];
  held.writer = useOutlineWriter();
  const request = useOutlineRequest();
  const [shown, setShown] = useState(true);
  held.show = setShown;
  return shown ? (
    <IssueTree
      artifacts={new Map()}
      context={new Set()}
      emptyLine="Nothing here."
      focusReveal={false}
      holdsEscape={false}
      isClosed={false}
      onEditing={() => {}}
      {...(request === undefined ? {} : { request })}
      reveal={undefined}
      root={drawn}
      rootRow
    />
  ) : null;
}

/**
 * The tree under the providers the page's `slots/providers.tsx` draws around its body - the row
 * writer and the Contents list's requests - with `reader` signed in. It hands back the list's
 * ask and its Focus branch toggle, the writer's landing, a switch that takes the tree off the
 * page and draws it again (a view drawn in its place and turned off), and a redraw over another
 * tree (a read answering).
 */
function renderPageTree(root: TreeNode, reader = "alice") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: reader });
  // The writer's own reads, answered empty, so it asks the network for nothing.
  for (const read of ["asks", "comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  const held: PageHold = {};
  const draw = (drawn: TreeNode) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <OutlineWriterProvider issueKey={KEY}>
              <OutlineRequestProvider>
                <IssueScope />
                <PageBody drawn={drawn} held={held} />
              </OutlineRequestProvider>
            </OutlineWriterProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(root));
  return {
    ask: (id: string | undefined, focusBranch: boolean) =>
      act(async () => held.ask?.(id, focusBranch)),
    focusBranch: (next: boolean) => act(async () => held.focusBranch?.(next)),
    land: (under: string, to: Landing["to"] = "newest") =>
      act(async () => held.writer?.land({ to, under: { id: under, kind: "comment" } })),
    redraw: (drawn: TreeNode) => view.rerender(draw(drawn)),
    show: (shown: boolean) => act(async () => held.show?.(shown)),
    view,
    writer: () => held.writer,
  };
}

/** What a key reported at the row it was pressed on, or undefined where no row draws one. */
function rowReport(container: HTMLElement): string | undefined {
  return container.querySelector("[data-row-report]")?.textContent ?? undefined;
}

/** The words of every report a row draws in `container` from the call on (`[data-row-report]`),
 *  however briefly it stands - one drawn for a commit and gone at the next among them - from the
 *  tree's own changes rather than from what a task happens to leave; the call it answers with
 *  ends the watch and gives them. */
function reportsDrawn(container: HTMLElement): () => string[] {
  const drawn: string[] = [];
  const take = (records: MutationRecord[]): void => {
    for (const record of records) {
      for (const added of Array.from(record.addedNodes)) {
        if (!(added instanceof Element)) continue;
        const reports = added.matches("[data-row-report]")
          ? [added]
          : Array.from(added.querySelectorAll("[data-row-report]"));
        for (const report of reports) drawn.push(report.textContent ?? "");
      }
    }
  };
  const watch = new MutationObserver(take);
  watch.observe(container, { childList: true, subtree: true });
  return () => {
    take(watch.takeRecords());
    watch.disconnect();
    return drawn;
  };
}

/** Every row drawn for `nodeId`, in the order the tree drew them: one per placement. */
function rowsFor(container: HTMLElement, nodeId: string): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(`[data-node-row][data-node-id="${nodeId}"]`)];
}

function focusedRow(container: HTMLElement): string | undefined {
  const active = document.activeElement;
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .find((row) => row.contains(active))
    ?.getAttribute("data-node-id") as string | undefined;
}

/** A comment with its own address, so its row, focused, draws its acts (Copy reference). */
function addressedComment(id: string, text: string): TreeNode {
  return node({
    id,
    reference: buildDispatchReference({ id, key: KEY, kind: "comment" }),
    text,
    type: "comment",
  });
}

/** A control of the page outside the outline - a view control, a control of Details, one under
 *  the outline - drawn after the tree, and how many times a press fired it. */
function controlOutside(): { readonly control: HTMLElement; readonly presses: () => number } {
  let presses = 0;
  const name = "A control outside the outline";
  const drawn = render(
    <button
      onClick={() => {
        presses += 1;
      }}
      type="button"
    >
      {name}
    </button>
  );
  return { control: drawn.getByRole("button", { name }), presses: () => presses };
}

/** Details beside the outline, drawn after the tree, with a card of the margin's in it: the
 *  card's button. */
function cardInDetails(): HTMLElement {
  const thread = "A thread on the statement";
  return render(
    <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
      <button type="button">{thread}</button>
    </aside>
  ).getByRole("button", { name: thread });
}

/** Lets the next frame come, and React draw what it changed. */
async function nextFrame(): Promise<void> {
  const frame = Promise.withResolvers<void>();
  requestAnimationFrame(() => frame.resolve());
  await act(async () => frame.promise);
}

/** Stands in for the browser's size observers, since happy-dom lays nothing out and observes no
 *  size: each observer still observing is held with what it observes, and `resized` calls by hand
 *  the ones observing `target`, or every one where it names none, as a change of size would.
 *  `restore` puts the real one back. */
function sizesByHand(): {
  readonly resized: (target?: Element) => Promise<void>;
  readonly restore: () => void;
} {
  const observing = new Map<
    object,
    { readonly callback: ResizeObserverCallback; readonly targets: Set<Element> }
  >();
  const RealResizeObserver = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    readonly #callback: ResizeObserverCallback;
    constructor(callback: ResizeObserverCallback) {
      this.#callback = callback;
    }
    observe(target: Element) {
      const held = observing.get(this) ?? { callback: this.#callback, targets: new Set() };
      held.targets.add(target);
      observing.set(this, held);
    }
    unobserve(target: Element) {
      observing.get(this)?.targets.delete(target);
    }
    disconnect() {
      observing.delete(this);
    }
  } as unknown as typeof ResizeObserver;
  return {
    resized: (target) =>
      act(async () => {
        for (const [observer, { callback, targets }] of observing) {
          if (target === undefined || targets.has(target)) callback([], observer as ResizeObserver);
        }
      }),
    restore: () => {
      globalThis.ResizeObserver = RealResizeObserver;
    },
  };
}

test("the whole tree is one tab stop, and the focused row's keys work its controls", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const kept = spyOn(api, "keepDecision").mockResolvedValue({
    ...decision,
    waiting_review: false,
  } as never);
  const root = node({
    children: [
      node({
        chips: [
          {
            answered: true,
            gone: false,
            name: "The spec block",
            ref: `dispatch://${KEY}/spec#b-r1`,
          },
        ],
        id: "spec#b-d1",
        reference: decision.ref,
        type: "decision",
        waiting: true,
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, {
    artifacts: [specDocument],
    decisions: decisionsRead([decision]),
  });
  try {
    const [row] = rowsFor(view.container, "spec#b-d1");
    if (row === undefined) throw new Error("the decision has no row");

    // The tree offers one tab stop and nothing inside a row offers another, standing on the
    // row included, so Tab enters the tree once and leaves it again.
    await act(async () => row.focus());
    await waitFor(() => expect(view.getByRole("button", { name: "Keep" })).toBeTruthy());
    const stops = [...view.container.querySelectorAll('[tabindex="0"]')];
    expect(stops.length).toBe(1);
    expect(stops[0]?.getAttribute("data-node-row")).toBe("");

    // So each control of the row takes a key instead, and the keys reach what no Tab does
    // (WCAG 2.1.1): `f` steps to the chip's link, Escape gives the row back, Shift+K presses
    // Keep and Shift+C presses Change and leaves the caret in the form it opens.
    await user.keyboard("f");
    expect((document.activeElement as HTMLElement).textContent).toBe("The spec block");
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(row);

    await user.keyboard("{Shift>}K{/Shift}");
    await waitFor(() => expect(kept).toHaveBeenCalled());
    expect(kept.mock.calls[0]?.[1]).toBe(decision.block_id);

    await user.keyboard("{Shift>}C{/Shift}");
    const field = await view.findByRole("textbox", { name: "Your change" });
    await waitFor(() => expect(document.activeElement).toBe(field));
  } finally {
    kept.mockRestore();
  }
});

test("Enter sends the change the reader typed, instead of putting a caret in the row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const sent = spyOn(api, "createComment").mockResolvedValue({ id: "comment-1" } as never);
  const read = readSpecText();
  const root = node({
    children: [
      node({
        id: "spec#b-d1",
        reference: decision.ref,
        type: "decision",
        waiting: true,
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, {
    artifacts: [specDocument],
    decisions: decisionsRead([decision]),
  });
  try {
    const [row] = rowsFor(view.container, "spec#b-d1");
    await act(async () => row?.focus());
    await user.click(await view.findByRole("button", { name: "Change" }));
    await user.click(view.getByRole("textbox", { name: "Your change" }));
    await user.keyboard("Say more about the flag.");

    // Enter on the form's own control sends it: the row's Enter fires from the row itself and
    // takes nothing from a control the reader reached inside it.
    await act(async () => view.getByRole("button", { name: "Send change" }).focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(sent).toHaveBeenCalled());
    expect(sent.mock.calls[0]?.[0]).toBe(KEY);
    // Nothing went into an editor: the row's Enter never fired.
    expect(view.edited.filter((artifactId) => artifactId !== undefined)).toEqual([]);
  } finally {
    sent.mockRestore();
    read.mockRestore();
  }
});

/** The one decision's tree the two rows below press its controls in. */
function decisionTree(): TreeNode {
  return node({
    children: [node({ id: "spec#b-d1", reference: decision.ref, type: "decision", waiting: true })],
    id: KEY,
    type: "issue",
  });
}

/** Answers the text of the spec, which holds the decision's block in its `## Decisions` part and
 *  which a Change reads as it is sent to count its quote's places over (`DecisionActions.tsx`). */
function readSpecText() {
  return spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: `## Decisions\n\n${decision.text}`,
    token: "sha256:spec-text",
    version: null,
  });
}

// A control that disables itself while its write is out drops the focus to the document body
// if it holds it: a keyboard reader loses their place (measured in Chromium: a `focusout` from
// Keep with no related target 45 ms after its write went out). So Keep moves the focus onto its
// decision's own row first, and the row's face, with Change, stays drawn throughout.
test("Keep keeps the focus on its decision's row while its write is out, and after", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const answer = Promise.withResolvers<never>();
  const kept = spyOn(api, "keepDecision").mockReturnValue(answer.promise);
  const view = renderTree(decisionTree(), {
    artifacts: [specDocument],
    decisions: decisionsRead([decision]),
  });
  try {
    const [row] = rowsFor(view.container, "spec#b-d1");
    if (row === undefined) throw new Error("the decision has no row");
    await act(async () => row.focus());
    const keep = await view.findByRole("button", { name: "Keep" });
    await user.click(keep);
    await waitFor(() => expect(kept).toHaveBeenCalled());
    await waitFor(() => expect(keep).toHaveProperty("disabled", true));
    expect(document.activeElement, "the focus stayed on Keep, which its write disables").toBe(row);
    expect(row.querySelector("[data-node-body]"), "the decision's face closed").not.toBeNull();

    await act(async () => answer.resolve({ ...decision, waiting_review: false } as never));
    await waitFor(() => expect(view.queryByRole("button", { name: "Keep" })).toBeNull());
    expect(document.activeElement).toBe(row);
    expect(view.getByRole("button", { name: "Change" })).toBeTruthy();
  } finally {
    kept.mockRestore();
  }
});

// The same for Change's own Send, which is disabled while the comment is out and goes with the
// form once it lands.
test("Send change keeps the focus on its decision's row while the comment is out, and after", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const answer = Promise.withResolvers<never>();
  const sent = spyOn(api, "createComment").mockReturnValue(answer.promise);
  const read = readSpecText();
  const view = renderTree(decisionTree(), {
    artifacts: [specDocument],
    decisions: decisionsRead([decision]),
  });
  try {
    const [row] = rowsFor(view.container, "spec#b-d1");
    if (row === undefined) throw new Error("the decision has no row");
    await act(async () => row.focus());
    await user.click(await view.findByRole("button", { name: "Change" }));
    await user.keyboard("Say more about the flag.");
    const send = view.getByRole("button", { name: "Send change" });
    await user.click(send);
    await waitFor(() => expect(sent).toHaveBeenCalled());
    await waitFor(() => expect(send).toHaveProperty("disabled", true));
    expect(
      document.activeElement,
      "the focus stayed on Send change, which its write disables"
    ).toBe(row);
    expect(row.querySelector("[data-node-body]"), "the decision's face closed").not.toBeNull();

    await act(async () => answer.resolve({ id: "comment-1" } as never));
    await waitFor(() => expect(view.queryByRole("textbox", { name: "Your change" })).toBeNull());
    expect(document.activeElement).toBe(row);
    expect(view.getByRole("button", { name: "Change" })).toBeTruthy();
  } finally {
    sent.mockRestore();
    read.mockRestore();
  }
});

// R11: a decision's face, with Keep, Change and an open Change form, is drawn whatever the
// focus, so a reader who opened Change, typed and moved on finds the form and its words where
// they left them, and the row they come back to draws it as it stood.
test("a Change the reader typed in stays on its decision's row as the focus moves on", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "spec#b-d1", reference: decision.ref, type: "decision", waiting: true }),
      node({ id: "comment-1", text: "The next words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, {
    artifacts: [specDocument],
    decisions: decisionsRead([decision]),
  });
  const [row] = rowsFor(view.container, "spec#b-d1");
  if (row === undefined) throw new Error("the decision has no row");
  await act(async () => row.focus());
  await view.findByRole("button", { name: "Change" });
  await user.keyboard("{Shift>}C{/Shift}");
  const field = await view.findByRole("textbox", { name: "Your change" });
  await waitFor(() => expect(document.activeElement).toBe(field));
  await user.keyboard("Say more about the flag.");
  // Out of the field by the keyboard alone: Tab to the form's Send change, Escape to the row.
  await user.keyboard("{Tab}{Escape}");
  expect(document.activeElement).toBe(row);

  // The next row takes the focus, and the decision's face keeps the form and its words; the row
  // above takes the focus back with it as it stood.
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  expect(
    (view.getByRole("textbox", { name: "Your change" }) as HTMLTextAreaElement).value,
    "the focus moving on took the change's words with it"
  ).toBe("Say more about the flag.");
  await user.keyboard("k");
  await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-d1"));
  const back = await view.findByRole("textbox", { name: "Your change" });
  expect((back as HTMLTextAreaElement).value).toBe("Say more about the flag.");
});

/** Whether `row` draws the focused row's look, which moves with the focus the tree takes. */
function looksFocused(row: Element): boolean {
  return row.querySelector("[data-node-card]")?.className.includes(pageFocusedRow) === true;
}

/** Whether `row` draws its acts, which every row draws whatever the focus (R11). */
function drawsActs(row: Element): boolean {
  return row.querySelector("[data-row-acts]") !== null;
}

// A press focuses what it lands on as it goes down. Taken by the tree while the press is held,
// that focus would move the focused look under the pointer before the press ends; R11 has every
// row draw its face and its acts whatever the focus, so nothing grows. The tree takes the focus
// once the press is over, the look moves then, and no row's acts come or go.
test("a press on a row moves the focused look once it ends, and draws nothing new", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      addressedComment("comment-1", "The first words."),
      addressedComment("comment-2", "The words pressed on."),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  // No question waits, so the page opens on the first row.
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const [first] = rowsFor(view.container, "comment-1");
  const [pressed] = rowsFor(view.container, "comment-2");
  const words = pressed?.querySelector<HTMLElement>("[data-node-words]");
  if (first === undefined || pressed === undefined || words == null) {
    throw new Error("the rows are not drawn");
  }
  await waitFor(() => expect(looksFocused(first)).toBe(true));
  expect([drawsActs(first), drawsActs(pressed)]).toEqual([true, true]);

  await user.pointer({ keys: "[MouseLeft>]", target: words });
  expect(focusedRow(view.container), "the press did not focus the row it went down on").toBe(
    "comment-2"
  );
  expect(looksFocused(pressed), "the held press moved the focused look").toBe(false);
  expect(looksFocused(first), "the held press let the first row go").toBe(true);
  expect([drawsActs(first), drawsActs(pressed)], "the held press changed a row's acts").toEqual([
    true,
    true,
  ]);

  await user.pointer({ keys: "[/MouseLeft]" });
  await waitFor(() => expect(looksFocused(pressed)).toBe(true));
  expect(looksFocused(first)).toBe(false);
  expect([drawsActs(first), drawsActs(pressed)]).toEqual([true, true]);
});

// The architect's ruling on a press, as amended: a press on a control outside the outline - a
// view control, a control of Details, one under the outline - focuses that control as it goes
// down, and letting the focused row go then changed the page under the pointer. The row keeps
// its focused look while the press is held and through its click, and is let go one task after
// the press ends; its face and its acts are drawn throughout, as every row's are (R11). happy-dom
// lays nothing out, so the browser rows read the presses firing; this reads the rule that keeps
// the page still, and that lets the row go once nothing is under the pointer any more.
test("a press on a control outside the outline keeps the focused row's look through its click, and lets the row go once it ends", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(
    node({
      children: [
        addressedComment("comment-1", "The first words."),
        addressedComment("comment-2", "The second words."),
      ],
      id: KEY,
      type: "issue",
    })
  );
  const outside = controlOutside();
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const [first] = rowsFor(view.container, "comment-1");
  if (first === undefined) throw new Error("the first row is not drawn");
  await waitFor(() => expect(looksFocused(first)).toBe(true));
  // Whether the row looked focused as each click of the control landed.
  const drawnAtClick: boolean[] = [];
  outside.control.addEventListener("click", () => drawnAtClick.push(looksFocused(first)));

  await user.pointer({ keys: "[MouseLeft>]", target: outside.control });
  expect(document.activeElement, "the press did not focus the control it went down on").toBe(
    outside.control
  );
  expect(
    looksFocused(first),
    "the press on a control outside the outline let the focused row go"
  ).toBe(true);
  expect(
    drawsActs(first),
    "the press on a control outside the outline took the focused row's acts away"
  ).toBe(true);
  await user.pointer({ keys: "[/MouseLeft]" });
  expect(outside.presses(), "the press did not fire the control").toBe(1);
  expect(drawnAtClick, "the row was let go before the press's click landed").toEqual([true]);
  // The panel goes back to the issue's own facts, as for any focus that leaves the outline for
  // anything but Details.
  expect(view.focusedNode()).toBeUndefined();

  // The press over, the row is let go: its focused look, and its acts stay as every row's do.
  await waitFor(() =>
    expect(
      looksFocused(first),
      "the row stayed focused after the press on a control outside the outline ended"
    ).toBe(false)
  );
  expect(drawsActs(first)).toBe(true);
});

// Tab is no press, so the focus leaving the outline by it lets go of the row at once - its
// focused look and what a key reported at it, which lasts until the next key, and Tab is one
// (the reviewer's finding on e3136741, where both stayed drawn).
test("Tab out of the outline lets go of the row at once: no focused look and no report", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(
    node({
      children: [
        addressedComment("comment-1", "The first words."),
        addressedComment("comment-2", "The second words."),
      ],
      id: KEY,
      type: "issue",
    })
  );
  const outside = controlOutside();
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const [first] = rowsFor(view.container, "comment-1");
  if (first === undefined) throw new Error("the first row is not drawn");
  await user.keyboard("w");
  await waitFor(() => expect(rowReport(view.container)).toBe(KEY_REPORTS.no_decision));
  expect(first.querySelector("[data-node-card]")?.className).toContain(pageFocusedRow);

  await user.tab();
  expect(document.activeElement).toBe(outside.control);
  await waitFor(() =>
    expect(rowReport(view.container), "Tab out of the outline left the key's report drawn").toBe(
      undefined
    )
  );
  expect(
    first.querySelector("[data-node-card]")?.className,
    "Tab out of the outline left the row it left looking focused"
  ).not.toContain(pageFocusedRow);
  expect(drawsActs(first), "Tab out of the outline took the row's acts away").toBe(true);
});

// Escape anywhere on the page lets go at once of the row a press outside the outline keeps, with
// the press still held; and the outline's own Escape leaves the outline from its row with
// nothing held after the focus has been out to a control and back.
test("Escape lets go of the row a press outside the outline keeps, from the page or from the outline", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(
    node({
      children: [
        addressedComment("comment-1", "The first words."),
        addressedComment("comment-2", "The second words."),
      ],
      id: KEY,
      type: "issue",
    })
  );
  const outside = controlOutside();
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const [first] = rowsFor(view.container, "comment-1");
  if (first === undefined) throw new Error("the first row is not drawn");
  await waitFor(() => expect(looksFocused(first)).toBe(true));

  await user.pointer({ keys: "[MouseLeft>]", target: outside.control });
  expect(document.activeElement).toBe(outside.control);
  expect(
    looksFocused(first),
    "the press held on a control outside the outline let the focused row go"
  ).toBe(true);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(looksFocused(first)).toBe(false));
  await user.pointer({ keys: "[/MouseLeft]" });

  // Back by Shift+Tab, to the row the reader left, which looks focused again; on it, the
  // outline's own Escape leaves the outline and lets the row go.
  await user.tab({ shift: true });
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await waitFor(() => expect(looksFocused(first)).toBe(true));
  await user.keyboard("{Escape}");
  expect(document.activeElement).toBe(document.body);
  await waitFor(() => expect(looksFocused(first)).toBe(false));
});

// Safari focuses no button a press lands on, so a press on a control outside the outline takes
// the focus from the row to no control at all, and the blur names no element. A blur naming no
// element that a press caused keeps the row through the press's click, as any press's does, and
// lets the row go one task after the press ends; a blur with no press - a script's, as the
// browser rows' `focusThePage` makes - lets it go at once.
test("a press outside the outline that leaves the focus on no control keeps the row through its click, and a blur with no press lets it go at once", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(
    node({
      children: [
        addressedComment("comment-1", "The first words."),
        addressedComment("comment-2", "The second words."),
      ],
      id: KEY,
      type: "issue",
    })
  );
  // Something outside the outline that takes no focus, as a button does not in Safari.
  const words = "Words outside the outline";
  const plain = render(<p>{words}</p>).getByText(words);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const [first] = rowsFor(view.container, "comment-1");
  if (first === undefined) throw new Error("the first row is not drawn");
  await waitFor(() => expect(looksFocused(first)).toBe(true));
  const blurredTo: (EventTarget | null)[] = [];
  first.addEventListener("focusout", (event) => blurredTo.push(event.relatedTarget));
  // Whether the row looked focused as each click on the words landed.
  const drawnAtClick: boolean[] = [];
  plain.addEventListener("click", () => drawnAtClick.push(looksFocused(first)));

  // A blur with no press lets the row go.
  await act(async () => first.blur());
  expect(blurredTo).toEqual([null]);
  await waitFor(() => expect(looksFocused(first)).toBe(false));

  // A press outside the tree, on what takes no focus, blurs the row to no element and keeps it
  // while it is held and through its click.
  await act(async () => first.focus());
  await waitFor(() => expect(looksFocused(first)).toBe(true));
  await user.pointer({ keys: "[MouseLeft>]", target: plain });
  expect(blurredTo, "the press did not blur the row to no element").toEqual([null, null]);
  expect(document.activeElement).toBe(document.body);
  expect(
    looksFocused(first),
    "the press outside the outline that left the focus on no control let the row go"
  ).toBe(true);
  await user.pointer({ keys: "[/MouseLeft]" });
  expect(drawnAtClick, "the row was let go before the press's click landed").toEqual([true]);
  await waitFor(() =>
    expect(
      looksFocused(first),
      "the row stayed focused after the press that left the focus on no control ended"
    ).toBe(false)
  );
  expect(drawsActs(first)).toBe(true);
});

// WCAG 2.1.1: every control of a row is out of the tab order, so `f` steps through the focused
// row's chips, then the links its words hold, then each control of the row that no key of the
// keymap presses - Change answer, Copy reference, a command's Copy - in the row's order, and the
// last stop comes back round to the first. Enter on the one it stopped on follows it or presses
// it.
test("f steps through a row's chips, links and then the controls no key presses", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  const original = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const reference = buildDispatchReference({ id: "ask-1", key: KEY, kind: "ask" });
  // A question the reader answered, so its row draws Change answer before its Reply, which `r`
  // presses, and Copy reference; its words link an issue, and it names a block of the spec.
  const root = node({
    children: [
      node({
        answer: {
          at: "2026-11-16T09:00:00Z",
          by: { id: "alice", kind: "user" },
          selected: [],
          text: "Behind a flag.",
        },
        author: { id: "bob", kind: "user" },
        chips: [
          {
            answered: true,
            gone: false,
            name: "The spec block",
            ref: `dispatch://${KEY}/spec#b-r1`,
          },
        ],
        id: "ask-1",
        question: true,
        reference,
        state: "answered",
        text: "Do we ship it as dispatch://CORE-7 says?",
        type: "ask",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  /** Presses `f` and reads what it left the focus on. */
  const stop = async (): Promise<string | null | undefined> => {
    await user.keyboard("f");
    return document.activeElement?.textContent;
  };
  try {
    const { container } = renderPageTree(root, "alice").view;
    await waitFor(() => expect(focusedRow(container)).toBe("ask-1"));
    expect(await stop()).toBe("The spec block");
    expect(await stop()).toBe("CORE-7");
    expect(await stop()).toBe(CONTROL_LABELS.change_answer);
    expect(await stop()).toBe(CONTROL_LABELS.copy_reference);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(reference));
    expect(await stop()).toBe("The spec block");
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: original });
  }
});

// Home on the first row moves the focus to no other row, and from a chip `f` stopped on, the row
// already holds the focus through the chip: the row's own placing, which runs as a row takes the
// focus, does not run. A reader who scrolled the row off the screen from there is brought back
// to it by the key all the same (`moveToRow`), as by a key that moves the focus to another row.
test("Home from a chip of the first row brings the row back on screen after the reader scrolled it off", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        chips: [
          {
            answered: true,
            gone: false,
            name: "The spec block",
            ref: `dispatch://${KEY}/spec#b-r1`,
          },
        ],
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  // happy-dom lays nothing out, so the first row's box is given once the reader has scrolled it
  // off: 400 px above the window's top. Every other box, and that one before, is empty.
  let scrolledOff = false;
  const off = { bottom: -360, top: -400 };
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return scrolledOff && this.matches('[data-node-row][data-node-id="comment-1"]')
      ? new DOMRect(0, off.top, 800, off.bottom - off.top)
      : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const { container } = renderTree(root);
    await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
    await user.keyboard("f");
    expect(document.activeElement?.textContent).toBe("The spec block");
    expect(focusedRow(container)).toBe("comment-1");

    scrolledOff = true;
    await user.keyboard("{Home}");
    expect(document.activeElement).toBe(rowsFor(container, "comment-1")[0]);
    expect(scrolled, "Home left the row the reader scrolled off the screen").toHaveBeenCalledTimes(
      1
    );
    // Scrolled by that much, the row stands whole inside the window.
    const by = (scrolled.mock.calls[0]?.[0] as ScrollToOptions | undefined)?.top ?? 0;
    expect(off.top - by).toBeGreaterThanOrEqual(0);
    expect(off.bottom - by).toBeLessThanOrEqual(window.innerHeight);
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// A code block or table of the focused row's words whose content runs past its box keeps a tab
// stop of its own, so a reader on the keyboard can scroll it sideways; one whose content fits
// keeps none. On it, or on a link inside it, every key the browser scrolls by - the block, or the
// page where the block does not scroll the key's way - is the browser's: the page's keymap
// prevents none of them, and the tree moves no focus, folds nothing and says nothing. Escape from
// it gives the row back, and `j`, which scrolls nothing, still moves to the next row. happy-dom
// scrolls nothing, so a key left to the browser is what a unit case reads; the browser suite
// reads the scroll itself.
test("on a block of the focused row that keeps its tab stop, the keys the browser scrolls by are left to it, and Escape gives the row back", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  await warmMarkdownRenderer();
  const text = [
    "Run it so:",
    "",
    "```sh",
    "bun run e2e --project=chromium --workers=1 --retries=0",
    "```",
    "",
    "| Where | What |",
    "| --- | --- |",
    "| [the notes](https://example.com/notes) | value |",
  ].join("\n");
  // The comment holds a reply, drawn unfolded as every node is (R11), so Right would move into
  // it and Left fold it; the reply is the row after it.
  const root = node({
    children: [
      node({
        children: [node({ id: "reply-1", text: "A reply.", type: "comment" })],
        id: "comment-1",
        text,
        tree: displayTree(text),
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  // The rules that make a block of a row's words scroll, which `useOutOfTabOrder` reads through
  // the computed style: happy-dom applies no rule of styles.css.
  const scrollers = "[data-node-words] pre, [data-node-words] table";
  const sheet = document.createElement("style");
  sheet.textContent = `${scrollers} { overflow-x: auto; }`;
  document.head.append(sheet);
  // A block keeps its tab stop only while its content runs past its box, and happy-dom lays
  // nothing out, so the code block's long line and the table are laid out past theirs from the
  // first pass on.
  const restoreOverflow = overflowing(scrollers);
  try {
    const { container } = renderTree(root);
    await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
    const [row] = rowsFor(container, "comment-1");
    if (row === undefined) throw new Error("the comment has no row");
    expect(row.getAttribute("aria-expanded"), "the comment's reply is drawn folded").toBe("true");
    /** The block of the focused row that keeps its tab stop under `name`, marked as kept. */
    const kept = (name: string): HTMLElement => {
      const block = row.querySelector<HTMLElement>(
        `[data-kept-scroller][tabindex="0"][aria-label="${name}"]`
      );
      if (block === null) throw new Error(`the focused row keeps no tab stop named "${name}"`);
      return block;
    };
    const code = await waitFor(() => kept(CONTROL_LABELS.scrolls_code));
    const link = kept(CONTROL_LABELS.scrolls_table).querySelector<HTMLElement>("a[href]");
    if (link === null) throw new Error("the table draws no link");
    /** The rows as drawn, each with whether it is open: what a move or a fold changes. */
    const drawn = (): string[] =>
      [...container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
        (one) => `${one.dataset.rowKey} ${one.getAttribute("aria-expanded")}`
      );
    /** Presses `key` where the focus is, as the browser sends it, and answers the event once
     *  the page has heard it. */
    const press = (key: string): KeyboardEvent => {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key });
      act(() => {
        document.activeElement?.dispatchEvent(event);
      });
      return event;
    };
    const rows = drawn();
    const report = rowReport(container);
    for (const [on, what] of [
      [code, "the code block"],
      [link, "a link in the table"],
    ] as const) {
      act(() => code.focus());
      // `f` steps from the block to the row's one link, the table's.
      if (on === link) await user.keyboard("f");
      expect(document.activeElement, `the focus did not reach ${what}`).toBe(on);
      for (const key of [
        "ArrowRight",
        "ArrowLeft",
        "ArrowDown",
        "ArrowUp",
        "Home",
        "End",
        "PageUp",
        "PageDown",
        " ",
      ]) {
        const name = key === " " ? "Space" : key;
        expect(press(key).defaultPrevented, `the page took ${name} from ${what}`).toBe(false);
        expect(document.activeElement, `${name} moved the focus off ${what}`).toBe(on);
        expect(drawn(), `${name} on ${what} moved through the tree or folded it`).toEqual(rows);
        expect(rowReport(container), `${name} on ${what} said something`).toBe(report);
      }
      press("Escape");
      expect(document.activeElement, `Escape on ${what} did not give the row back`).toBe(row);
    }

    act(() => code.focus());
    expect(press("j").defaultPrevented, "j on the code block was left to the browser").toBe(true);
    expect(focusedRow(container), "j on the code block did not move to the next row").toBe(
      "reply-1"
    );
  } finally {
    sheet.remove();
    restoreOverflow();
  }
});

// A row whose node Enter puts the caret in - a block of one of the issue's documents, a statement
// and a check among them - carries `data-editable` (`useNodeEditing.ts`'s `nodeEditable`), which
// draws the cue that it takes typing (`styles.css`). A comment, a result and a child issue are no
// block of a document and take no caret, and a closed issue's documents are read-only, so none of
// its rows carries the mark.
test("a statement's and a check's rows carry data-editable, a comment's, a result's and a child's do not, and no row of a closed issue does", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const artifacts: Artifact[] = [
    specDocument,
    {
      ...specDocument,
      id: "artifact-verification",
      name: "Verification",
      slug: "verification",
    },
  ];
  const root = node({
    children: [
      node({
        children: [
          node({
            children: [node({ id: "result-1", text: "What the run saw.", type: "result" })],
            id: "verification#b-c1",
            state: "no-result",
            text: "The check of the statement.",
            type: "check",
          }),
        ],
        id: "spec#b-r1",
        statement: true,
        text: "A statement of the spec.",
        type: "requirement",
      }),
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "CORE-2", text: "The first child", type: "child_issue" }),
    ],
    id: KEY,
    type: "issue",
  });
  const marked = async (isClosed: boolean): Promise<(string | undefined)[]> => {
    const view = renderTree(root, { artifacts, isClosed });
    try {
      // The check's own branch arrives folded; the reader opens it to draw its result.
      const [check] = rowsFor(view.container, "verification#b-c1");
      if (check === undefined) throw new Error("the tree drew no row of the check");
      await act(async () => check.focus());
      await user.keyboard("{ArrowRight}");
      await waitFor(() => expect(rowsFor(view.container, "result-1")).toHaveLength(1));
      const rows = [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")];
      expect(
        rows.map((row) => row.dataset.nodeId),
        "the tree did not draw every row"
      ).toEqual(["spec#b-r1", "verification#b-c1", "result-1", "comment-1", "CORE-2"]);
      return rows
        .filter((row) => row.hasAttribute("data-editable"))
        .map((row) => row.dataset.nodeId);
    } finally {
      view.unmount();
    }
  };

  expect(await marked(false), "the rows marked as taking typing on an open issue").toEqual([
    "spec#b-r1",
    "verification#b-c1",
  ]);
  expect(await marked(true), "a row of the closed issue is marked as taking typing").toEqual([]);
});

// A document's room can admit the page read-only, as the server does a page whose schema it
// cannot trust with writes, and then no block of that document takes typing: the page's row
// editor reports what the room admitted (`document-admission.tsx`), and every row of that
// document drops `data-editable`, not only the row being edited in, while another document's rows
// keep it. Before any room of a document has answered, Enter's rule alone marks its rows, and a
// later admission to write puts the mark back.
test("the rows of a document its room admitted read-only carry no data-editable, another document's still do, and an admission to write puts it back", async () => {
  setViewport(1440);
  const artifacts: Artifact[] = [
    specDocument,
    {
      ...specDocument,
      id: "artifact-verification",
      name: "Verification",
      slug: "verification",
    },
  ];
  const root = node({
    children: [
      node({ id: "spec#b-r1", statement: true, text: "A statement.", type: "requirement" }),
      node({ id: "spec#b-r2", statement: true, text: "Another statement.", type: "requirement" }),
      node({ id: "verification#b-c1", state: "no-result", text: "A check.", type: "check" }),
    ],
    id: KEY,
    type: "issue",
  });
  const every = ["spec#b-r1", "spec#b-r2", "verification#b-c1"];
  const view = renderTree(root, { artifacts });
  const marked = () =>
    [...view.container.querySelectorAll<HTMLElement>("[data-node-row][data-editable]")].map(
      (row) => row.dataset.nodeId
    );
  try {
    expect(marked(), "before any room has answered, a block's row is not marked").toEqual(every);

    await view.admit(specDocument.id, true);
    expect(
      marked(),
      "a row of the document its room admitted read-only is marked as taking typing, or another document's row is not"
    ).toEqual(["verification#b-c1"]);

    await view.admit(specDocument.id, false);
    expect(
      marked(),
      "a row of the document its room admitted to write again is not marked as taking typing"
    ).toEqual(every);
  } finally {
    view.unmount();
  }
});

test("Escape leaves the caret for the placement it was taken from, not the node's first row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  // A check of the Verification document draws a state, and a requirement of the spec is a
  // statement (`isCheck`, `isStatement`): with neither, the outline folds the branch away.
  const shared = {
    id: "spec#b-c1",
    state: "no-result",
    text: "One check, under two requirements.",
    type: "check",
  };
  const root = node({
    children: [
      node({
        children: [node(shared as never)],
        id: "spec#b-r1",
        statement: true,
        type: "requirement",
      }),
      node({
        children: [node(shared as never)],
        id: "spec#b-r2",
        statement: true,
        type: "requirement",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { artifacts: [specDocument] });
  const placements = rowsFor(view.container, "spec#b-c1");
  expect(placements.length).toBe(2);
  const second = placements[1];
  if (second === undefined) throw new Error("the check has one placement only");

  await act(async () => second.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(view.edited.at(-1)).toBe("artifact-spec"));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(view.edited.at(-1)).toBeUndefined());
  expect(document.activeElement).toBe(second);
});

/** An Escape that ends an input-method composition, as the browser sends it to `target`. */
function composingEscape(target: Element): void {
  target.dispatchEvent(
    new KeyboardEvent("keydown", {
      bubbles: true,
      cancelable: true,
      isComposing: true,
      key: "Escape",
    })
  );
}

// The tester's finding at #4205's 2ff6850a, on the ruling that a key that ends an input-method
// composition acts on nothing: a reader typing in Japanese, Chinese or Korean in a row's editor
// presses Escape to drop the candidate the input method shows, and that Escape is the input
// method's. At 2ff6850a the editor's own Escape (`useNodeEditing.ts`'s `leave`) reads the key
// alone, so it ends the editing and takes the caret out mid-word.
test("an Escape that ends an input-method composition in a row's editor leaves the editing as it was", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        id: "spec#b-r1",
        statement: true,
        text: "A statement of the spec.",
        type: "requirement",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { artifacts: [specDocument] });
  const [row] = rowsFor(view.container, "spec#b-r1");
  if (row === undefined) throw new Error("the statement's row is not drawn");
  await act(async () => row.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(view.edited.at(-1)).toBe("artifact-spec"));
  const focused = document.activeElement;
  await act(async () => composingEscape(focused ?? row));
  expect(view.edited.at(-1), "an Escape that ended a composition ended the editing").toBe(
    "artifact-spec"
  );
  expect(document.activeElement, "an Escape that ended a composition moved the focus").toBe(
    focused
  );
});

// Safari sends the Escape that ends a composition after the composition's end, unmarked -
// `isComposing` false - with keyCode 229 (WebKit bug 165004), as the title's editor reads it
// (`IssueTitleRow.tsx`): that Escape is the input method's as well, and leaves the editing and
// the focus as they were.
test("an Escape that ends an input-method composition in a row's editor, sent unmarked with keyCode 229, leaves the editing as it was", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await caretInStatement(user);
  await act(async () => {
    pressIn(editable, "Escape", { isComposing: false, keyCode: 229 });
  });
  await nextFrame();
  expect(view.edited.at(-1), "an Escape that ended a composition ended the editing").toBe(
    "artifact-spec"
  );
  expect(
    document.activeElement === editable,
    "an Escape that ended a composition moved the focus"
  ).toBe(true);
});

// A press on another row while a row is edited ends the editing once the press is over, as Tab
// out of the editor's box does, and the focus stays where the press put it. Left on, the edited
// row kept its editor's box open, and an Escape pressed on the row the press focused was taken
// for the edited row: it gave that row the focus back, where Escape on a row leaves the outline.
test("a press on another row ends the editing, and Escape there leaves the outline rather than going back to the edited row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const [other] = rowsFor(view.container, "comment-1");
  const words = other?.querySelector<HTMLElement>("[data-node-words]");
  if (words === null || words === undefined) throw new Error("the comment's row is not drawn");

  await user.pointer({ keys: "[MouseLeft>]", target: words });
  expect(focusedRow(view.container), "the press did not focus the row it went down on").toBe(
    "comment-1"
  );
  await user.pointer({ keys: "[/MouseLeft]" });
  // The press is over a task after its click.
  await waitInAct(0);
  await user.keyboard("{Escape}");
  expect(
    focusedRow(view.container),
    "Escape on the row the press focused left the focus on a row, not off the outline"
  ).toBeUndefined();
  expect(document.activeElement === document.body, "Escape left the focus on a control").toBe(true);
  await waitFor(() =>
    expect(view.edited.at(-1), "the press on another row did not end the editing").toBeUndefined()
  );
});

// A press outside the outline leaves the row edited. Enter on the row being edited set the editing
// it already had: nothing ran again to put the caret in, and the row said nothing. It puts the
// caret in the node's block again, as the first Enter did.
test("a press outside the outline, then Enter on the row being edited, puts the caret in its block again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  // Something outside the outline that takes no focus.
  const outside = "Words outside the outline";
  const plain = render(<p>{outside}</p>).getByText(outside);
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  if (statement === undefined) throw new Error("the statement's row is not drawn");

  await user.click(plain);
  expect(document.activeElement === document.body, "the press did not take the focus off").toBe(
    true
  );
  await waitInAct(0);
  expect(view.edited.at(-1), "the press outside the outline ended the editing").toBe(
    "artifact-spec"
  );
  await act(async () => statement.focus());
  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(
      statement.querySelector('[contenteditable="true"]') !== null,
      "the row took no editor"
    ).toBe(true)
  );
  const drawn = statement.querySelector<HTMLElement>('[contenteditable="true"]');
  if (drawn === null) throw new Error("the row took no editor");
  // An editor drawn anew for the row draws the document once it has read it.
  if (drawn.querySelector('[data-block-id="r1"]') === null) await drawStatementBlocks(drawn);
  await waitFor(() =>
    expect(
      document.activeElement === drawn && caretBlock() === "r1",
      "Enter on the row after a press outside the outline put no caret in its block"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "the editing is not on").toBe("artifact-spec");
});

// A press the editing goes on through is the editor's own: one on a control the editor draws in
// the box it scrolls in, where it draws the action bar for its selection too. It leaves the
// editing on once it is over, with the focus where it put it; a press on the outline outside that
// box - here, on the tree's own blank space - ends it.
test("a press on a control in the row's editor box keeps the editing, and a press on the outline outside that box ends it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const inBox = "A control the editor draws in its box";
  const { editable, view } = await enterStatement(
    user,
    <>
      <div contentEditable suppressContentEditableWarning />
      <button type="button">{inBox}</button>
    </>
  );
  await drawStatementBlocks(editable);
  await waitFor(
    () =>
      expect(
        document.activeElement === editable && caretBlock() === "r1",
        "Enter did not put the caret in the statement"
      ).toBe(true),
    { timeout: 10_000 }
  );
  const control = view.getByRole("button", { name: inBox });
  const tree = view.container.querySelector<HTMLElement>('[role="tree"]');
  if (tree === null) throw new Error("the outline is not drawn");
  const statement = editable.closest<HTMLElement>("[data-node-row]");
  // The document the row is edited in, and whether the row still holds an editor.
  const editing = () => [
    view.edited.at(-1),
    statement?.querySelector('[contenteditable="true"]') != null,
  ];

  await user.click(control);
  await waitInAct(0);
  expect(editing(), "a press on a control in the editor's box ended the editing").toEqual([
    "artifact-spec",
    true,
  ]);
  expect(document.activeElement === control, "the press left the focus off its control").toBe(true);

  await user.click(tree);
  await waitFor(() =>
    expect(
      editing(),
      "a press on the outline outside the editor's box did not end the editing"
    ).toEqual([undefined, false])
  );
});

// A press in Details while a row is edited ended the editing once it was over, as any press
// outside the editor's box did: the margin draws the document's threads, and the composer the
// reader opened on their selection, only while the page draws the document's editor, so the
// composer and the thread pressed went off the page under the pointer. A press outside the
// outline ends nothing, and the focus stays where it put it; a press on another row of the tree
// still ends the editing once it is over.
test("a press in Details keeps the row's editing, and a press on another row of the tree still ends it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const card = cardInDetails();
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  const words = rowsFor(view.container, "comment-1")[0]?.querySelector<HTMLElement>(
    "[data-node-words]"
  );
  if (statement === undefined || words === null || words === undefined) {
    throw new Error("the outline's rows are not drawn");
  }
  // The document the row is edited in, and whether the row still holds an editor.
  const editing = () => [
    view.edited.at(-1),
    statement.querySelector('[contenteditable="true"]') !== null,
  ];

  await user.click(card);
  // The press is over a task after its click.
  await waitInAct(0);
  expect(editing(), "a press in Details ended the editing").toEqual(["artifact-spec", true]);
  expect(document.activeElement === card, "the press left the focus off the card").toBe(true);

  await user.click(words);
  await waitFor(() =>
    expect(editing(), "a press on another row of the tree did not end the editing").toEqual([
      undefined,
      false,
    ])
  );
  expect(focusedRow(view.container), "the focus left the row the press put it on").toBe(
    "comment-1"
  );
});

// Details takes the focus from the row's editor as it opens, and a press on Details' own words
// moves the page's selection into them and the focus to the page's body. The focus Details gave
// back to the editor as it closed then put the caret at the node's block's start, and the words
// typed next went in there rather than where the reader had left off. The caret comes back at the
// place the editor kept, as the editor's own.
test("the focus given back to a row's editor after a press in Details brings the caret back where the reader left it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  await user.keyboard("{Shift}");
  await select(wordsOf(editable, "r1"), 5);
  // Details beside the outline, with its heading and its Close.
  const details = render(
    <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
      <h2>Details</h2>
      <button type="button">Close</button>
    </aside>
  );
  const heading = details.getByRole("heading", { name: "Details" });
  // Opening Details puts the focus on its Close (`margin/Margin.tsx`).
  await act(async () => details.getByRole("button", { name: "Close" }).focus());
  await user.click(heading);
  // The press is over a task after its click.
  await waitInAct(0);
  expect(
    [
      document.activeElement === document.body,
      heading.contains(window.getSelection()?.anchorNode ?? null),
    ],
    "the press on Details' heading left the focus off the page's body or the selection off its words"
  ).toEqual([true, true]);

  await focusByScript(editable);
  expect(document.activeElement === editable, "the focus did not come back to the editor").toBe(
    true
  );
  expect(selected(), "the caret came back elsewhere").toEqual(["r1:5", "r1:5"]);
  const { $head } = view.state.selection;
  expect(
    [$head.parent.attrs.blockId, $head.parentOffset],
    "the editor's own caret is not where the reader left it"
  ).toEqual(["r1", 5]);
});

/** Gives `editable` the focus as a script does - Details' Close gives it back to where Details
 *  took it from (`margin/Margin.tsx`) - and moves the page's selection as a browser does then: a
 *  selection that stands outside the editable once the editable holds the focus goes to its start,
 *  a reset ProseMirror reads as one (`prosemirror-view`'s `DOMObserver.flush`), and the move is
 *  reported. happy-dom moves no selection on a focus, so that move is made here. Waits out
 *  ProseMirror's own look at the selection, 20 ms after its editable takes the focus. */
async function focusByScript(editable: HTMLElement): Promise<void> {
  await act(async () => editable.focus({ preventScroll: true }));
  if (!editable.contains(window.getSelection()?.anchorNode ?? null)) await select(editable, 0);
  await waitInAct(30);
}

// A press in Details between Enter and the caret's placement keeps the editing and puts off the
// caret: none goes in while the reader is out there (decision 348 (2), and the tester's case
// beside the editor's hold, below). The reader coming back to the row - Details' Close gives the
// focus back to it, or they press in the row's editor box - asks for the caret again, as Enter on
// the edited row does: it goes in the node's block where Enter puts it, the block's start, and
// the reader's acts are counted from then, so a caret the editor moves out of the block before
// they act goes back to the block's start, as it does after Enter.
const COMING_BACK: readonly (readonly [
  name: string,
  comeBack: (user: UserEvent, row: HTMLElement, editable: HTMLElement) => Promise<void>,
])[] = [
  [
    "the focus Details' Close gives back to the row",
    // The script's focus on the row stands for Details' Close giving the focus back, which that
    // Close does by script (`margin/Margin.tsx`): no Close is drawn or pressed here.
    async (_user, row) => {
      await act(async () => row.focus());
    },
  ],
  [
    "a press in the row's editor box",
    async (user, _row, editable) => {
      await user.click(editable);
    },
  ],
];
for (const [name, comeBack] of COMING_BACK) {
  test(`${name}, after a press in Details put off the row's caret, puts the caret in the node's block as Enter does`, async () => {
    setViewport(1440);
    const user = userEvent.setup();
    const parts = await nodeCaret.loadEditsHold();
    const held = Promise.withResolvers<nodeCaret.HoldParts>();
    const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
    try {
      const { editable, view } = await enterBeforeDrawn(user);
      const [row] = rowsFor(view.container, "spec#b-r1");
      if (row === undefined) throw new Error("the statement's row is not drawn");
      await drawStatementBlocks(editable, BLOCKS_AROUND);
      await nextFrame();
      await user.click(cardInDetails());
      // The press is over a task after its click.
      await waitInAct(0);
      await act(async () => held.resolve(parts));
      await nextFrame();

      await comeBack(user, row, editable);
      await waitFor(() =>
        expect(
          document.activeElement === editable && caretBlock() === "r1",
          `${name} put no caret in the statement`
        ).toBe(true)
      );
      expect(selected(), "the caret is not where Enter puts it").toEqual(["r1:0", "r1:0"]);
      expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
      await select(wordsOf(editable, "r2"), 3);
      expect(
        selected(),
        "a caret the editor moved below the statement before the reader acted did not go back to its start"
      ).toEqual(["r1:0", "r1:0"]);
    } finally {
      load.mockRestore();
    }
  });
}

// The fold control, Let it close and the row's other controls prevent their `mousedown`'s
// default, so a press on one leaves the focus where it was. A press on one while a row is edited
// left the focus in the row's editor, and ending the editing at that press's end took the editor's
// box off the page with the focus in it, which the browser dropped to the page's body. A press
// that leaves the focus in the editor keeps the editing.
test("a press on a control that keeps the focus where it was, as the fold control does, keeps the editing and the focus in the row's editor", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await caretInStatement(user, [
    node({
      children: [node({ id: "reply-2", text: "A reply to the second words.", type: "comment" })],
      id: "comment-2",
      text: "The second words.",
      type: "comment",
    }),
  ]);
  const fold = () =>
    rowsFor(view.container, "comment-2")[0]?.querySelector<HTMLElement>(
      "button[data-node-bullet]"
    ) ?? null;
  const control = fold();
  if (control === null) throw new Error("the second comment's row draws no fold control");
  const expanded = control.getAttribute("aria-expanded");
  const statement = editable.closest<HTMLElement>("[data-node-row]");

  await user.click(control);
  // The press is over a task after its click.
  await waitInAct(0);
  expect(
    fold()?.getAttribute("aria-expanded"),
    "the press on the fold control did nothing"
  ).not.toBe(expanded);
  expect(
    [view.edited.at(-1), statement?.querySelector('[contenteditable="true"]') != null],
    "a press that left the focus in the row's editor ended the editing"
  ).toEqual(["artifact-spec", true]);
  expect(
    document.activeElement === editable,
    "the press on the fold control took the focus out of the row's editor"
  ).toBe(true);
});

// The focus can come back to the row being edited with the editing still on - a script's focus
// on it, which is no act of the reader's - and Enter there set the editing it already had, so
// no caret went in. It puts the caret in the node's block again, as the first Enter did.
test("Enter on the row being edited, the focus back on the row, puts the caret in its block again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await caretInStatement(user);
  const statement = editable.closest<HTMLElement>("[data-node-row]");
  if (statement === null) throw new Error("the editor is in no row");
  await user.keyboard("{Shift}");
  await act(async () => statement.focus());
  expect(view.edited.at(-1), "the focus on the row ended the editing").toBe("artifact-spec");

  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(
      document.activeElement === editable && caretBlock() === "r1",
      "Enter on the row being edited put no caret in its block"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "Enter ended the editing").toBe("artifact-spec");
});

// The row being edited listens for Escape before its editor does, so the editor cannot cancel
// it. Read for every Escape in the tree, it took one pressed on another row while the row was
// edited - a press on that row held down, which ends the editing only once it is over - for the
// edited row's, and gave that row the focus back. An Escape on another row is the keymap's, which
// leaves the outline from it.
test("an Escape on another row while a row is edited leaves the outline, and gives the edited row no focus", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const words = rowsFor(view.container, "comment-1")[0]?.querySelector<HTMLElement>(
    "[data-node-words]"
  );
  if (words === null || words === undefined) throw new Error("the comment's row is not drawn");
  await user.pointer({ keys: "[MouseLeft>]", target: words });
  expect(focusedRow(view.container), "the press did not focus the row it went down on").toBe(
    "comment-1"
  );
  expect(view.edited.at(-1), "the press ended the editing while it was held").toBe("artifact-spec");

  await user.keyboard("{Escape}");
  expect(
    focusedRow(view.container),
    "Escape on another row left the focus on a row, not off the outline"
  ).toBeUndefined();
  await user.pointer({ keys: "[/MouseLeft]" });
});

// A focus that reaches another row of the tree with no press - an address's landing on a node of
// this issue, which focuses its row by script a frame after a link in the row's editor is
// followed, or any other script's focus - ends the editing, as a press there does. Left on, the
// tree held every row the focus came to as the one being edited: a Tab out of the tree kept that
// row's body and its facts in Details, and the page held the document's editor for a row the
// reader had left.
test("a script's focus on another row ends the editing, and a Tab out of the tree lets go of that row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const last = node({ id: "comment-2", text: "The last words.", type: "comment" });
  const { view } = await caretInStatement(user, [last]);
  const { control } = controlOutside();
  const row = rowsFor(view.container, "comment-2")[0];
  if (row === undefined) throw new Error("the last comment's row is not drawn");

  await act(async () => row.focus());
  await user.tab();
  expect(document.activeElement, "Tab did not leave the outline").toBe(control);
  await waitFor(() =>
    expect(rowLetGo(row), "the row Tab left kept its body or its focused look").toBe(true)
  );
  expect(view.focusedNode(), "the panel still holds the facts of the row Tab left").toBeUndefined();
  await waitFor(() =>
    expect(view.edited.at(-1), "the focus on another row left the editing on").toBeUndefined()
  );
  expect(
    document.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`),
    "the editor's box is still drawn"
  ).toBeNull();
});

// The focus entering another row while a press is held is the press's to end with: the editing
// ends once the press is over, as a press on that row ends it, and not before, so the editor's
// box leaves no row while the pointer is still down. The press here goes down in the row's own
// editor, which ends nothing of itself, and a script moves the focus to another row while it is
// held.
test("a focus moved onto another row while a press in the row's editor is held ends the editing once the press is over, not before", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const last = node({ id: "comment-2", text: "The last words.", type: "comment" });
  const { editable, view } = await caretInStatement(user, [last]);
  const row = rowsFor(view.container, "comment-2")[0];
  if (row === undefined) throw new Error("the last comment's row is not drawn");

  await user.pointer({ keys: "[MouseLeft>]", target: editable });
  await act(async () => row.focus());
  expect(focusedRow(view.container), "the script's focus is not on the last comment's row").toBe(
    "comment-2"
  );
  await waitInAct(0);
  expect(
    view.edited.at(-1),
    "the focus on another row ended the editing while the press was held"
  ).toBe("artifact-spec");

  await user.pointer({ keys: "[/MouseLeft]" });
  // The press is over a task after its click.
  await waitInAct(0);
  await waitFor(() =>
    expect(
      view.edited.at(-1),
      "the press's end left the editing on, the focus on another row"
    ).toBeUndefined()
  );
  expect(focusedRow(view.container), "the focus left the row the script put it on").toBe(
    "comment-2"
  );
});

// The keymap's Escape for a row's editor named the row Enter took the caret from until its own
// run let go of it, which never ran: the row's own listener takes the key first. It was offered
// from then on in any editable of the page, and an Escape in an editor outside the outline took
// the focus to that row. It is offered in an editable inside the outline alone, while a row is
// edited, and the editing's end gives the row its focus back.
test("Escape in an editor outside the outline, after a row's editing has ended, leaves the focus there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const elsewhere = render(
    <div contentEditable suppressContentEditableWarning />
  ).container.querySelector<HTMLElement>('[contenteditable="true"]');
  if (elsewhere === null) throw new Error("the editor outside the outline is not drawn");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(view.edited.at(-1), "Escape did not end the editing").toBeUndefined());

  await act(async () => elsewhere.focus());
  await user.keyboard("{Escape}");
  expect(
    focusedRow(view.container),
    "Escape in an editor outside the outline moved to a row"
  ).toBeUndefined();
  expect(document.activeElement === elsewhere, "Escape took the focus off the editor").toBe(true);
});

// The tester's red row on c-keys (`the caret Enter puts in a statement deep in its document
// stays on screen while the reader writes`): the row the reader is editing in grows as they
// write, and put back at its top at each change of its size (`NodeRow.tsx`'s
// `useRowOnScreen`) it took the caret off the screen. The editor keeps its caret on screen, and
// the row places nothing of its own. happy-dom lays nothing out and observes no size, so the
// row's box is given, and each size observer still observing is called by hand once it grows.
test("the row the reader is editing in places nothing of its own as it grows", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "spec#b-r1", text: "A statement of the spec.", type: "requirement" }),
    ],
    id: KEY,
    type: "issue",
  });
  // The statement's row 100 px down and 40 px tall, whole on the screen, until it grows to three
  // windows' height, its foot far below the window's.
  let grown = false;
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches("[data-node-row]") && this.dataset.nodeId === "spec#b-r1"
      ? new DOMRect(0, 100, 800, grown ? 3 * window.innerHeight : 40)
      : new DOMRect();
  });
  const sizes = sizesByHand();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const view = renderTree(root, { artifacts: [specDocument] });
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(view.edited.at(-1)).toBe("artifact-spec"));
    scrolled.mockClear();

    grown = true;
    await sizes.resized();
    expect(
      scrolled,
      "the row being edited was put back on screen as it grew"
    ).not.toHaveBeenCalled();
  } finally {
    sizes.restore();
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// The tester's red row on c-keys (`a row that edits a statement is no taller than the window,
// and the page around it stays put`): the editor a row takes holds the whole
// document, and drawn whole in the row it made the row as tall as the document. Its box reaches
// from where it stands down to the box at the foot of the window, and is scrolled - the box, not
// the page - to the node's block, with the caret there; a row so low that less than half the
// room is left under it gets half the room, and the page is scrolled just far enough to show it.
// The editor is made editable before it has read its document, and holds no block for the caret
// or the box until then, so nothing is placed until it has drawn the document's blocks.
// happy-dom lays nothing out, so the row, its box, and the block 1,500 px down the document it
// scrolls, are given (`pageAroundEditingRow`), here a row that draws nothing over or under its box.
test("the row's editor opens at the node's block, in a box that ends at the foot of the window", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "spec#b-r1", text: "A statement of the spec.", type: "requirement" }),
    ],
    id: KEY,
    type: "issue",
  });
  const layout: RowLayout = { above: 0, below: 0, drawn: 3000, stands: 200 };
  const page = pageAroundEditingRow(layout);
  const { floor, room, scrolled } = page;
  try {
    const view = renderTree(root, {
      artifacts: [specDocument],
      editor: <div contentEditable suppressContentEditableWarning />,
    });
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));
    // The row's editor, and then the document's text in it, once the editor has read it.
    const drawText = async (): Promise<void> => {
      let editable: HTMLElement | null | undefined;
      await waitFor(() => {
        editable = rowsFor(view.container, "spec#b-r1")[0]?.querySelector<HTMLElement>(
          '[contenteditable="true"]'
        );
        expect(editable, "the row took no editor").toBeTruthy();
      });
      if (editable === null || editable === undefined) throw new Error("the row took no editor");
      await nextFrame();
      expect(
        editable.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`)?.style.maxHeight,
        "the editor was opened before it had drawn the document's text"
      ).toBe("");
      await drawStatementBlocks(editable);
    };
    const editorBox = async (): Promise<HTMLElement> => {
      let box: HTMLElement | null | undefined;
      await waitFor(() => {
        box = rowsFor(view.container, "spec#b-r1")[0]?.querySelector<HTMLElement>(
          `[${EDITOR_BOX_ATTRIBUTE}]`
        );
        expect(box?.style.maxHeight, "the row's editor was not held to the window").not.toBe("");
      });
      if (box === null || box === undefined) throw new Error("the row holds no editor");
      return box;
    };

    scrolled.mockClear();
    await user.keyboard("{Enter}");
    await drawText();
    const box = await editorBox();
    expect(box.style.maxHeight, "the box does not end at the foot of the window").toBe(
      `${floor - layout.stands}px`
    );
    expect(box.scrollTop, "the box was not scrolled to the statement's block").toBe(1500);
    expect(scrolled, "the page around the row moved").not.toHaveBeenCalled();
    expect(caretBlock(), "the caret is not in the statement's block").toBe("r1");

    // Low on the screen, with less than half the room under it.
    await user.keyboard("{Escape}");
    await waitFor(() => expect(view.edited.at(-1)).toBeUndefined());
    layout.stands = floor - 40;
    await user.keyboard("{Enter}");
    await drawText();
    const low = await editorBox();
    const cap = Math.floor(room / 2);
    expect(low.style.maxHeight, "the box of a row low on the screen is not half the room").toBe(
      `${cap}px`
    );
    expect(scrolled, "the page did not bring the low row's box on screen").toHaveBeenCalledTimes(1);
    expect(
      (scrolled.mock.calls[0]?.[0] as ScrollToOptions | undefined)?.top ?? 0,
      "the page did not bring the low row's box over the foot"
    ).toBeGreaterThanOrEqual(layout.stands + cap - floor);
  } finally {
    page.restore();
  }
});

// The tester's red row `a row that edits a statement is no taller than the window, and the page
// around it stays put`, its second half: the row's body - its facts, Keep open, Copy reference -
// stands under its editor's box, and a box held to the room under its top alone left that body
// under the window: a long statement the reader moved to and edited made a row of 971 px in a
// window 900 px tall. The box is held so the whole row fits the room between the band and the
// foot: a row whose top is at the band gets the room less what the row draws over and under its
// box, and the page moves not at all.
test("a row whose body stands under its editor's box is no taller than the room", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  // The row's top at the band, 60 px down: 2 px of it over the box, and a body of 120 px under it.
  const layout: RowLayout = { above: 2, below: 120, drawn: 3000, stands: 62 };
  const page = pageAroundEditingRow(layout);
  try {
    const { box } = await heldEditorBox(user, page);
    expect(box.style.maxHeight, "the box leaves the row taller than the room").toBe(
      `${page.room - layout.above - layout.below}px`
    );
    expect(page.scrolled, "the page around the row moved").not.toHaveBeenCalled();
  } finally {
    page.restore();
  }
});

// Low on the screen, with less than half the room the row leaves its box under the box's top
// once the body under it is counted: the box takes that half, and the page is scrolled just as
// far as brings the whole row, its body too, over the foot's box.
test("a low row whose body stands under its editor's box takes half the room the row leaves it, and is brought on screen whole", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const layout: RowLayout = { above: 2, below: 120, drawn: 3000, stands: 0 };
  const page = pageAroundEditingRow(layout);
  layout.stands = page.floor - 40;
  try {
    const { box } = await heldEditorBox(user, page);
    const cap = Math.floor((page.room - layout.above - layout.below) / 2);
    expect(box.style.maxHeight, "the box of a low row is not half the room the row leaves it").toBe(
      `${cap}px`
    );
    expect(page.scrolled, "the page did not bring the low row on screen").toHaveBeenCalledTimes(1);
    expect(
      (page.scrolled.mock.calls[0]?.[0] as ScrollToOptions | undefined)?.top ?? 0,
      "the page did not bring the low row's body over the foot"
    ).toBeGreaterThanOrEqual(layout.stands + cap + layout.below - page.floor);
  } finally {
    page.restore();
  }
});

// A body taller than the room leaves the box no room at all: the box still takes three of the
// editor's lines (`node-caret.ts`'s `LEAST_BOX_LINES`), and the page, which shows that box
// already, stays where it was.
test("a row whose body is taller than the room still holds its editor's box to three of its lines", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const layout: RowLayout = { above: 2, below: 0, drawn: 3000, stands: 62 };
  const page = pageAroundEditingRow(layout);
  layout.below = page.room + 100;
  try {
    const { box } = await heldEditorBox(user, page);
    expect(
      box.style.maxHeight,
      "the box under a body taller than the room is not three of the editor's lines"
    ).toBe(`${3 * EDITOR_LINE}px`);
    expect(page.scrolled, "the page moved though the box was on screen").not.toHaveBeenCalled();
  } finally {
    page.restore();
  }
});

// A line height of `normal`, which a computed style keeps as the word rather than a length, is
// read as 1.2 times the editor's font size (`node-caret.ts`'s `NORMAL_LINE_HEIGHT`): the box under
// a body taller than the room takes three lines of 1.2 x 20 px.
test("a box held to three of the editor's lines reads a line of normal height as 1.2 times the editor's font size", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const layout: RowLayout = { above: 2, below: 0, drawn: 3000, stands: 62 };
  const page = pageAroundEditingRow(layout);
  layout.below = page.room + 100;
  try {
    const { box } = await heldEditorBox(user, page, { fontSize: "20px", lineHeight: "normal" });
    expect(
      box.style.maxHeight,
      "the box of an editor whose lines are of normal height is not three lines of 1.2 times its font size"
    ).toBe("72px");
  } finally {
    page.restore();
  }
});

// The body can grow after the editor opened, as the page's reads answer - a requirement's counts
// come with the coverage read - and the second look after the editor's drawing settles holds the
// box again, so the row still fits the room.
test("a body grown by the time the editor's drawing settles is counted when the box is held again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const layout: RowLayout = { above: 2, below: 120, drawn: 3000, stands: 62 };
  const page = pageAroundEditingRow(layout);
  try {
    const { box, editable } = await heldEditorBox(user, page);
    layout.below = 180;
    layout.drawn = 3200;
    await page.resized(editable);
    await waitFor(
      () =>
        expect(box.style.maxHeight, "the box was not held again once the body grew").toBe(
          `${page.room - layout.above - layout.below}px`
        ),
      { timeout: 2_000 }
    );
    expect(page.scrolled, "the page around the row moved").not.toHaveBeenCalled();
  } finally {
    page.restore();
  }
});

/** Where the statement's row and its editor stand on the window, laid out by hand: the editor's
 *  box `stands` px down the window, the row `above` px over the box's top and `below` px - its
 *  body - under the box's foot, and the editor's drawing of the document `drawn` px tall, which
 *  the box is as tall as until it is held to a height. A case moves them as it goes. */
interface RowLayout {
  above: number;
  below: number;
  drawn: number;
  stands: number;
}

/** How tall a line of the editor `heldEditorBox` draws is. */
const EDITOR_LINE = 24;

/** The page around the statement's row while it holds its editor (`pageAroundEditingRow`). */
interface EditingPage {
  /** How far down the window the box at the foot starts. */
  readonly floor: number;
  /** Calls by hand the size observers watching `target`, or every one (`sizesByHand`). */
  readonly resized: (target?: Element) => Promise<void>;
  /** Puts back what the page was laid out by. */
  readonly restore: () => void;
  /** The room between the band at the top and the box at the foot. */
  readonly room: number;
  /** The page's scroll, which moves nothing. */
  readonly scrolled: Mock<typeof window.scrollBy>;
}

/**
 * The page around the statement's row while it holds its editor, by hand, since happy-dom lays
 * nothing out and observes no size: the row, its editor's box, the editor's drawing and the
 * statement's block 1,500 px down the document stand where `layout` says, and nothing else
 * draws; the page's scroll is watched and moves nothing; its size observers are called by hand
 * (`sizesByHand`), as the second look after the drawing above the block settles watches the
 * editor's size; and a band of 60 px stands at the top, over a box of 100 px at the foot.
 * Answers the line the foot's box starts at, the room between the two, the page's scroll, its
 * size observers, and what puts it all back.
 */
function pageAroundEditingRow(layout: RowLayout): EditingPage {
  const boxRect = (box: HTMLElement) => {
    const cap = Number.parseFloat(box.style.maxHeight);
    return new DOMRect(0, layout.stands, 800, Number.isNaN(cap) ? layout.drawn : cap);
  };
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const held = this.matches("[data-node-row]")
      ? this.querySelector<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`)
      : null;
    if (held !== null) {
      const box = boxRect(held);
      return new DOMRect(0, box.top - layout.above, 800, layout.above + box.height + layout.below);
    }
    const box = this.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
    if (box === null) return new DOMRect();
    if (box === this) return boxRect(box);
    if (this.matches('[contenteditable="true"]')) {
      return new DOMRect(0, layout.stands - box.scrollTop, 800, layout.drawn);
    }
    return this.dataset.blockId === "r1"
      ? new DOMRect(0, layout.stands + 1500 - box.scrollTop, 800, 24)
      : new DOMRect();
  });
  const sizes = sizesByHand();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  const published = document.documentElement.style;
  published.setProperty(IDENTITY_HEIGHT_PROPERTY, "60px");
  published.setProperty(FOOT_HEIGHT_PROPERTY, "100px");
  const floor = window.innerHeight - 100;
  return {
    floor,
    resized: sizes.resized,
    restore: () => {
      sizes.restore();
      boxes.mockRestore();
      scrolled.mockRestore();
      published.removeProperty(IDENTITY_HEIGHT_PROPERTY);
      published.removeProperty(FOOT_HEIGHT_PROPERTY);
    },
    room: floor - 60,
    scrolled,
  };
}

/** The statement's row on `page` with Enter pressed on it, once its editor - an editable drawn in
 *  `style`, by default in lines of `EDITOR_LINE` px - has drawn the statement's document and the
 *  row has held the editor's box to a height. The page's scroll is watched from the drawing on:
 *  the move to the row before it placed the row as every move does. Answers the box and the
 *  editable. */
async function heldEditorBox(
  user: UserEvent,
  page: EditingPage,
  style: CSSProperties = { lineHeight: `${EDITOR_LINE}px` }
): Promise<{ readonly box: HTMLElement; readonly editable: HTMLElement }> {
  const { editable } = await enterStatement(
    user,
    <div contentEditable style={style} suppressContentEditableWarning />
  );
  page.scrolled.mockClear();
  await drawStatementBlocks(editable);
  const box = editable.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  if (box === null) throw new Error("the row's editor stands in no box");
  await waitFor(() =>
    expect(box.style.maxHeight, "the row's editor was not held to the window").not.toBe("")
  );
  return { box, editable };
}

/** Blocks of the statement's document as its editor draws them, each by its `data-block-id` and
 *  its words: by default the words above the statement (`r0`), then the statement (`r1`). */
type StatementBlocks = readonly (readonly [id: string, words: string])[];

const STATEMENT_BLOCKS: StatementBlocks = [
  ["r0", "The words above the statement."],
  ["r1", "A statement of the spec."],
];

/** A comment and then a statement of the spec, with Enter pressed on the statement's row, once
 *  the row's editor - `editor`, a plain editable unless given - has drawn `blocks`, and Enter's
 *  caret is in the statement's block (`r1`). Answers the editor's editable. */
async function editStatement(
  user: UserEvent,
  editor: StandInEditor = <div contentEditable suppressContentEditableWarning />,
  blocks: StatementBlocks = STATEMENT_BLOCKS
): Promise<HTMLElement> {
  const { editable } = await enterStatement(user, editor);
  await drawStatementBlocks(editable, blocks);
  await waitFor(
    () =>
      expect(
        document.activeElement === editable && caretBlock() === "r1",
        "Enter did not put the caret in the statement"
      ).toBe(true),
    { timeout: 10_000 }
  );
  return editable;
}

/** `editStatement` with a plain editable as the row's editor and `after` under the statement,
 *  answering the tree as well as the editable. */
async function caretInStatement(user: UserEvent, after: readonly TreeNode[] = []) {
  const entered = await enterStatement(
    user,
    <div contentEditable suppressContentEditableWarning />,
    after
  );
  await drawStatementBlocks(entered.editable);
  await waitFor(
    () =>
      expect(
        document.activeElement === entered.editable && caretBlock() === "r1",
        "Enter did not put the caret in the statement"
      ).toBe(true),
    { timeout: 10_000 }
  );
  return entered;
}

/** A comment and then a statement of the spec, and `after` under them where given, with Enter
 *  pressed on the statement's row, once the row holds its editor, `editor`. Answers the tree and
 *  the editor's editable. The document's own editor (`LiveEditor`) loads its library on its first
 *  use, which can take more than a second. */
async function enterStatement(
  user: UserEvent,
  editor: StandInEditor,
  after: readonly TreeNode[] = []
) {
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "spec#b-r1", text: "A statement of the spec.", type: "requirement" }),
      ...after,
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { artifacts: [specDocument], editor });
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));
  await user.keyboard("{Enter}");
  let found: HTMLElement | null | undefined;
  await waitFor(
    () => {
      found = rowsFor(view.container, "spec#b-r1")[0]?.querySelector<HTMLElement>(
        '[contenteditable="true"]'
      );
      expect(found, "the row took no editor").toBeTruthy();
    },
    { timeout: 10_000 }
  );
  return { editable: found as HTMLElement, view };
}

/** Draws `blocks` of the statement's document in the row's `editable`, as its editor does once it
 *  has read it. */
function drawStatementBlocks(
  editable: HTMLElement,
  blocks: StatementBlocks = STATEMENT_BLOCKS
): Promise<void> {
  return act(async () => appendBlocks(editable, blocks));
}

/** Draws `blocks` in `editable` at once, with no task on after: a case that follows the drawing
 *  within its task - an editor handed over, or taken out, in the task it drew in - goes on from
 *  here. */
function appendBlocks(editable: HTMLElement, blocks: StatementBlocks = STATEMENT_BLOCKS): void {
  for (const [id, words] of blocks) {
    const block = document.createElement("p");
    block.dataset.blockId = id;
    block.textContent = words;
    editable.append(block);
  }
}

/** The block of the editor the caret is in, by its `data-block-id`: the block of the text it is
 *  in, or the block it is in itself where that block holds no text. */
function caretBlock(): string | null | undefined {
  const anchor = window.getSelection()?.anchorNode;
  const element = anchor instanceof Element ? anchor : anchor?.parentElement;
  return element?.closest("[data-block-id]")?.getAttribute("data-block-id");
}

/**
 * Milkdown's view made again, as a change of the editor's plugins makes it: its wrapper put
 * back in the editable's place, and a new one around it. Chromium sends the editable a focusout
 * that names nothing as it leaves the page, drops the focus to the page's body, and moves the
 * caret out to where the editable stood, as the DOM moves every range in a node taken out of
 * it; happy-dom drops the focus alone, so the focusout and the caret's move are made here.
 */
function wrapAgain(editable: HTMLElement): Promise<void> {
  return act(async () => {
    const wrapper = document.createElement("div");
    editable.dispatchEvent(new FocusEvent("focusout", { bubbles: true, relatedTarget: null }));
    editable.parentElement?.replaceChild(wrapper, editable);
    window.getSelection()?.collapse(wrapper, 0);
    wrapper.append(editable);
  });
}

/** An editor that reads its own selection as ProseMirror does: from the document's
 *  `selectionchange`, while it holds the focus, and from nothing else. `own.block` is the block
 *  of that selection, which is what a key the editor handles itself acts on, and `own.offset`
 *  its anchor's offset. */
function ReportReadingEditor({
  own,
}: {
  own: { block?: string | null; offset?: number };
}): ReactNode {
  const editable = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const read = () => {
      if (document.activeElement !== editable.current) return;
      own.block = caretBlock();
      own.offset = window.getSelection()?.anchorOffset;
    };
    document.addEventListener("selectionchange", read);
    return () => document.removeEventListener("selectionchange", read);
  }, [own]);
  return <div contentEditable ref={editable} suppressContentEditableWarning />;
}

/** Drops the `selectionchange` happy-dom reports inside the call that moves the selection: a
 *  browser reports a move a task after it, so no listener hears it in the task that made it. A
 *  report dispatched apart from such a call is heard. Answers what restores happy-dom's own. */
function dropSelectionReports(): () => void {
  const selection = window.getSelection();
  if (selection === null) throw new Error("happy-dom draws no selection");
  let moving = false;
  const move = (change: () => void) => {
    moving = true;
    try {
      change();
    } finally {
      moving = false;
    }
  };
  const collapse = selection.collapse.bind(selection);
  const setBaseAndExtent = selection.setBaseAndExtent.bind(selection);
  const spies = [
    spyOn(selection, "collapse").mockImplementation((node, offset) =>
      move(() => collapse(node, offset))
    ),
    spyOn(selection, "setBaseAndExtent").mockImplementation(
      (anchor, anchorOffset, focus, focusOffset) =>
        move(() => setBaseAndExtent(anchor, anchorOffset, focus, focusOffset))
    ),
  ];
  const drop = (event: Event) => {
    if (moving) event.stopImmediatePropagation();
  };
  document.addEventListener("selectionchange", drop, { capture: true });
  return () => {
    document.removeEventListener("selectionchange", drop, { capture: true });
    for (const spy of spies) spy.mockRestore();
  };
}

// The editor reads a caret put in it only once the browser reports the move - ProseMirror's
// observer listens for the document's `selectionchange` - which a browser does a task later, and
// until then a key the editor handles itself acts on the editor's own selection: measured in
// Chromium on a busy page, the editor held the focus with its selection at the top of the
// document for 0.27 to 0.84 s after Enter, and ten Deletes pressed then removed the spec's first
// heading. The editor takes Enter's caret as its own in the task that puts it there. happy-dom
// reports a move inside the call that makes it, so that report is dropped here, as no listener
// hears a browser's in that task.
test("the editor takes Enter's caret in a statement as its own in the task that puts it there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null } = {};
  try {
    await editStatement(user, <ReportReadingEditor own={own} />);
    expect(own.block, "the editor's own selection is not in the statement's block").toBe("r1");
  } finally {
    restore();
  }
});

// A statement whose block holds no text - an empty paragraph - has no text to put Enter's caret
// in, and an editor given the focus with no caret put in it takes the next key wherever the
// browser puts its own caret, outside the node's block. The caret goes in the block itself, which
// the editor reads as a place in that block, in the task that puts it there.
test("Enter on a statement whose block holds no text puts the caret in that block, as the editor's own", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null } = {};
  try {
    await editStatement(user, <ReportReadingEditor own={own} />, [
      ["r0", "The words above the statement."],
      ["r1", ""],
    ]);
    expect(own.block, "the editor's own selection is not in the empty statement's block").toBe(
      "r1"
    );
  } finally {
    restore();
  }
});

/** A comment, a statement of the spec and a second comment, with Enter pressed on the
 *  statement's row and the frame after it run, while the row's editor - `editor`, a plain
 *  editable handed to the row unless given - has drawn none of the document yet. Answers the tree
 *  and that editable. */
async function enterBeforeDrawn(
  user: UserEvent,
  editor: StandInEditor = <div contentEditable suppressContentEditableWarning />
) {
  const entered = await enterStatement(user, editor, [
    node({ id: "comment-2", text: "The last words.", type: "comment" }),
  ]);
  await nextFrame();
  return entered;
}

// A key the reader presses after Enter and before the row's editor has drawn the document, which
// takes the focus out of the row - `j` to the next row - is them moving on. The caret Enter put
// in the statement once the editor had drawn it took the focus back from the row `j` moved to,
// and the letters they typed next went into the statement, which is saved to the shared
// document. The first look for the caret after that ends the editing instead, and the focus stays
// on the row `j` moved it to.
test("a key that takes the focus out of the row before its editor has drawn the document ends the editing, and the focus stays where the key put it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  // The editor draws the document a frame later.
  await nextFrame();
  await drawStatementBlocks(editable);
  await nextFrame();
  expect(focusedRow(view.container), "the caret took the focus back from the row j moved to").toBe(
    "comment-2"
  );
  expect(
    document.activeElement?.closest('[contenteditable="true"]') ?? null,
    "an editor holds the focus"
  ).toBeNull();
  expect(view.edited.at(-1), "the editing did not end").toBeUndefined();
  expect(
    rowsFor(view.container, "spec#b-r1")[0]?.querySelector('[contenteditable="true"]') ?? null,
    "the statement's row still holds the editor"
  ).toBeNull();
});

// A key that leaves the focus on the row - Delete pressed the moment Enter is, as the browser row
// on a slowed page presses it - is one for the statement: the caret goes into the node's block
// once the editor has drawn the document, all the same.
test("a key that leaves the focus on the row before its editor has drawn the document still has the caret put in the node's block", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  await user.keyboard("{Delete}");
  expect(focusedRow(view.container), "Delete moved the focus off the row").toBe("spec#b-r1");
  // The editor draws the document a frame later.
  await nextFrame();
  await drawStatementBlocks(editable);
  await waitFor(() =>
    expect(
      document.activeElement === editable && caretBlock() === "r1",
      "a Delete pressed before the editor drew the document kept the caret out of the statement"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
});

// The editor draws the document's blocks once it has read it, and may draw others before the
// node's. A caret put in it before it has drawn the node's block goes to the document's first
// text, and the keys the reader presses next edit the top of the document, which is saved to the
// shared document. No caret goes into the editor, and no focus, until it has drawn the node's
// block: the focus stays on the row, where no key edits the document, and the caret goes into
// the node's block once it is drawn.
test("Enter puts no caret in the row's editor until it has drawn the node's own block, and then puts it there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  // The editor draws the words above the statement first.
  await drawStatementBlocks(editable, [["r0", "The words above the statement."]]);
  await nextFrame();
  expect(
    document.activeElement,
    "the focus left the row before the editor drew the statement's block"
  ).toBe(statement);
  expect(
    editable.contains(window.getSelection()?.anchorNode ?? null),
    "a caret was put in the editor before it drew the statement's block"
  ).toBe(false);
  // And the statement a frame later.
  await drawStatementBlocks(editable, [["r1", "A statement of the spec."]]);
  await waitFor(() =>
    expect(
      document.activeElement === editable && caretBlock() === "r1",
      "the caret did not go into the statement once the editor drew its block"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
});

// A press that takes the focus out of the outline before the row's editor has drawn the
// document ended the editing, and the editor's box and the threads Details draws beside the
// document's editor went off the page. Under decision 348 (2) such a press keeps the editing, as
// it does once the caret is placed, and puts off the caret: none goes in once the editor draws,
// and the focus stays on the control pressed. The tree keeps the row being edited as it is - its
// focused look and its facts in the panel. The editor's hold is held back until
// the editor has drawn the statement and goes on before the wait, as in the tester's case beside
// the hold (below), so a caret would go in by then were it not put off.
test("a press out of the outline before the row's editor has drawn the document keeps the editing and the row, and no caret takes the focus from the control once the editor draws", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const parts = await nodeCaret.loadEditsHold();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const { control } = controlOutside();
    const statement = rowsFor(view.container, "spec#b-r1")[0];
    if (statement === undefined) throw new Error("the statement's row is not drawn");
    expect(
      statement.querySelector("[data-node-card]")?.className,
      "the statement's row did not look focused as it was edited"
    ).toContain(pageFocusedRow);
    await user.click(control);
    expect(document.activeElement === control, "the press did not focus the control").toBe(true);
    // The editor draws the document a frame later, and its hold goes on then.
    await nextFrame();
    await drawStatementBlocks(editable);
    await act(async () => held.resolve(parts));
    await nextFrame();
    await waitInAct(20);
    expect(document.activeElement === control, "the caret took the focus from the control").toBe(
      true
    );
    expect(
      editable.contains(window.getSelection()?.anchorNode ?? null),
      "a caret went into the editor while the focus was out of the outline"
    ).toBe(false);
    expect(view.edited.at(-1), "the press out of the outline ended the editing").toBe(
      "artifact-spec"
    );
    expect(
      statement.querySelector('[contenteditable="true"]') !== null,
      "the statement's row no longer holds the editor"
    ).toBe(true);
    expect(
      statement.querySelector("[data-node-card]")?.className,
      "the statement's row no longer looks focused"
    ).toContain(pageFocusedRow);
    expect(view.focusedNode()?.id, "the panel no longer holds the statement's facts").toBe(
      "spec#b-r1"
    );
  } finally {
    load.mockRestore();
  }
});

// The same where the editor never draws the document at all after the press: the row is looked
// at a task after the press has ended with nothing drawn, which ended the editing; it keeps it.
test("a press out of the outline keeps the editing of a row whose editor never draws the node's block, with the focus where the press put it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await enterBeforeDrawn(user);
  const { control } = controlOutside();
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  if (statement === undefined) throw new Error("the statement's row is not drawn");
  await user.click(control);
  expect(document.activeElement === control, "the press did not focus the control").toBe(true);
  // The press is over a task after its click, and the row is looked at then.
  await waitInAct(20);
  await nextFrame();
  expect(view.edited.at(-1), "the press out of the outline ended the editing").toBe(
    "artifact-spec"
  );
  expect(document.activeElement === control, "the focus left the control pressed").toBe(true);
  expect(
    statement.querySelector("[data-node-card]")?.className,
    "the statement's row no longer looks focused"
  ).toContain(pageFocusedRow);
});

// The class search of #4205's 2fffbc34: a key the reader pressed while a press out of the
// outline was still held, before the row's caret was placed, was read as the act that took the
// focus off the row, and the editing ended once the press was over, where the press alone puts
// off the caret. A key pressed while a press is held is part of that press: the editing stays,
// and no caret goes in once the editor has drawn the statement and its hold is on.
test("a key pressed while a press out of the outline is held before the row's caret is placed keeps the editing, and the caret is put off", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const parts = await nodeCaret.loadEditsHold();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const { control } = controlOutside();
    await user.pointer({ keys: "[MouseLeft>]", target: control });
    expect(document.activeElement === control, "the press did not focus the control").toBe(true);
    await user.keyboard("{Shift}");
    await user.pointer({ keys: "[/MouseLeft]", target: control });
    // The press is over a task after it ends, and the row is looked at then.
    await waitInAct(20);
    expect(
      view.edited.at(-1),
      "a key pressed while the press out of the outline was held ended the editing"
    ).toBe("artifact-spec");
    // The editor draws the statement, and its hold goes on.
    await drawStatementBlocks(editable);
    await act(async () => held.resolve(parts));
    await nextFrame();
    await waitInAct(20);
    expect(document.activeElement === control, "the caret took the focus from the control").toBe(
      true
    );
    expect(
      editable.contains(window.getSelection()?.anchorNode ?? null),
      "a caret went into the editor while the focus was out of the outline"
    ).toBe(false);
    expect(view.edited.at(-1), "the editing ended once the editor's hold was on").toBe(
      "artifact-spec"
    );
  } finally {
    load.mockRestore();
  }
});

// A press on another row of the tree before the row's editor has drawn the node's block ends the
// editing once the press is over, a task after its click, as the tree lets go of a row a press
// left (`useReleaseFocus`). Until the caret is placed the editor's box stands in the row as tall
// as what the editor has drawn, and the editing ending a task after the press went down took that
// box out of the row with the pointer still down: the row below rose under the pointer, and the
// click landed on something else. happy-dom lays nothing out, so what is under the pointer as the
// press ends is given: the row below's words while the edited row holds the editor's box, and the
// words below the outline once the box has left the row.
test("a press on the row below before its editor has drawn the node's block lands on that row, and ends the editing once it is over", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  const words = "Words below the outline";
  const below = render(<p>{words}</p>).getByText(words);
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  const next = rowsFor(view.container, "comment-2")[0]?.querySelector<HTMLElement>(
    "[data-node-words]"
  );
  if (statement === undefined || next === null || next === undefined) {
    throw new Error("the outline's rows are not drawn");
  }
  let clicks = 0;
  next.addEventListener("click", () => {
    clicks += 1;
  });
  // The editor draws the words above the statement, and not the statement.
  await drawStatementBlocks(editable, [["r0", "The words above the statement."]]);
  await nextFrame();
  const underPointer = () =>
    statement.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`) === null ? below : next;

  await user.pointer({ keys: "[MouseLeft>]", target: next });
  expect(focusedRow(view.container), "the press did not focus the row below").toBe("comment-2");
  // The press is held over a few frames, as a reader's is.
  await nextFrame();
  await nextFrame();
  await user.pointer({ keys: "[/MouseLeft]", target: underPointer() });
  expect(
    clicks,
    "the editing ended while the press was held, and its click landed below the row"
  ).toBe(1);

  await waitFor(() => expect(view.edited.at(-1), "the editing did not end").toBeUndefined());
  expect(focusedRow(view.container), "the focus left the row pressed").toBe("comment-2");
});

// The row is looked at after each focus move off it, and not only after the reader's first act,
// until the caret is placed: a first act may leave the focus on the row - Shift, Delete - and a
// later one take it out of the outline before the editor has drawn the node's block. Looked at
// after the first act alone, the row of an editor that never draws that block kept the editing
// on, and the tree kept the row - its focused look and its facts in the panel - with
// the focus on the control Tab moved it to. Tab from the row stops in its editor first, where
// the focus is still the row's, and the Tab after that leaves the outline.
test("a later act that takes the focus out of the outline ends the editing of a row whose editor never draws the node's block, and lets go of the row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  const { control } = controlOutside();
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  if (statement === undefined) throw new Error("the statement's row is not drawn");
  await user.keyboard("{Shift}");
  expect(document.activeElement, "Shift moved the focus off the row").toBe(statement);
  await user.tab();
  expect(document.activeElement, "Tab from the row did not stop in its editor").toBe(editable);
  await user.tab();
  expect(document.activeElement, "Tab did not leave the outline").toBe(control);
  await waitFor(() => expect(view.edited.at(-1), "the editing went on").toBeUndefined());
  expect(document.activeElement, "the focus left the control Tab moved it to").toBe(control);
  await waitFor(() =>
    expect(
      statement.querySelector("[data-node-card]")?.className,
      "the statement's row still looks focused"
    ).not.toContain(pageFocusedRow)
  );
  expect(view.focusedNode(), "the panel still holds the statement's facts").toBeUndefined();
});

// A control inside the row is somewhere the reader moved to as well: `Shift+C` focuses a
// change's own box, and `f` steps to the row's links and controls. The caret placed once the
// editor drew took the focus from it, and the words written there went into the shared
// document. The statement here has no address, so its row draws neither its kind tag's link nor
// an act (R11): the case stands a button in the row's content for such a control, out of the tab
// order as every control of a row is, and a key of the reader's, then the focus on it.
test("a key that takes the focus to a control inside the row before its editor draws ends the editing there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  const statement = rowsFor(view.container, "spec#b-r1")[0];
  const content = statement?.querySelector<HTMLElement>("[data-node-content]");
  if (content === null || content === undefined) throw new Error("the statement has no row");
  const inner = document.createElement("button");
  inner.tabIndex = -1;
  inner.type = "button";
  content.append(inner);
  await user.keyboard("{Shift}");
  await act(async () => inner.focus());
  expect(document.activeElement, "the control inside the row did not take the focus").toBe(inner);
  await nextFrame();
  await drawStatementBlocks(editable);
  await waitFor(() => expect(view.edited.at(-1), "the editing did not end").toBeUndefined());
  expect(document.activeElement, "the caret took the focus from the row's control").toBe(inner);
});

// The box the row's editor is drawn in is the row's own, unlike a control inside the row: a
// press of the reader's in it before the editor has drawn the node's block is no move off the
// row, and the caret goes into that block once it is drawn, with the editing on - a key may edit
// the node's own block alone.
test("a press inside the row's editor before it has drawn the node's block keeps the editing on, and the caret goes into that block once it is drawn", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  await user.click(editable);
  expect(view.edited.at(-1), "the press inside the editor ended the editing").toBe("artifact-spec");
  expect(document.activeElement, "the press did not focus the editor").toBe(editable);
  // The editor draws the document a frame later.
  await nextFrame();
  await drawStatementBlocks(editable);
  await waitFor(() =>
    expect(
      document.activeElement === editable && caretBlock() === "r1",
      "the caret did not go into the statement once the editor drew its block"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
});

// Only an act of the reader's ends the page's own placing of the caret. A focus a script gives a
// control out of the row before the row's editor has drawn the document is no reader moving on:
// the editing goes on, and the caret goes into the node's block once the editor has drawn it.
test("a focus a script moves out of the row before its editor has drawn the document leaves the editing on, and the caret goes into the node's block", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user);
  const { control } = controlOutside();
  await act(async () => control.focus());
  expect(document.activeElement, "the script did not focus the control").toBe(control);
  // The editor draws the document a frame later.
  await nextFrame();
  await drawStatementBlocks(editable);
  await waitFor(() =>
    expect(
      document.activeElement === editable && caretBlock() === "r1",
      "a focus a script moved before the editor drew the document kept the caret out of the statement"
    ).toBe(true)
  );
  expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
});

// Two things the editor does to the caret Enter put in a node before the reader has done
// anything. It changes its plugins after it has drawn the document's text - Dispatch's block
// views as it binds, its collaborators' cursors once the room has synced - and ProseMirror then
// makes Milkdown's view again, which takes the editable out of the box it wraps it in and wraps
// it in a new one: the focus falls to the page's body as the editable leaves the page (measured
// in Chromium, 45 ms after Enter had put the caret in a statement). And it wrote its own
// selection back over a caret it had not read yet: measured in Chromium, the caret was in the
// spec's summary 60 ms after it was put in a statement, and the words typed went there. It reads
// the caret in the task that puts it there now (the test above), and a caret moved out of the
// block all the same goes back: until the reader acts the row puts the caret back at the start
// of the node's block; from their first act the caret is where the editor leaves it inside that
// block - a move out of it stops at the block's nearer end (below) - and a focus the editor
// drops comes back at that caret, not at the block's start (the next row).
test("a caret the editor drops or moves goes back to the node's block until the reader acts", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const editable = await editStatement(user);
  const caretInStatement = () => document.activeElement === editable && caretBlock() === "r1";
  // The editor's own selection, from before the caret went into the statement, written back,
  // and the browser reporting it.
  const selectAbove = () =>
    act(async () => {
      const words = editable.querySelector('[data-block-id="r0"]')?.firstChild;
      if (words === null || words === undefined) throw new Error("the editor draws no r0");
      window.getSelection()?.collapse(words, 0);
      document.dispatchEvent(new Event("selectionchange"));
    });

  await wrapAgain(editable);
  expect(caretInStatement(), "the caret the editor dropped was not put back").toBe(true);
  await selectAbove();
  expect(caretInStatement(), "the caret the editor moved was not put back").toBe(true);

  // The reader presses a key: the caret is where the editor leaves it inside the node's block
  // from then on, five characters into the statement, and not put back at the block's start.
  await user.keyboard("{Shift}");
  const statement = wordsOf(editable, "r1");
  await select(statement, 5);
  expect(
    window.getSelection()?.anchorOffset,
    "the caret was moved back after the reader acted"
  ).toBe(5);
  await wrapAgain(editable);
  await waitFor(() => expect(document.activeElement).toBe(editable));
  expect(
    window.getSelection()?.anchorOffset,
    "the caret was put back at the block's start after the reader acted"
  ).toBe(5);
});

// The editor goes on changing its plugins after the reader's first act - its collaborators'
// cursors go in once the room's mapping is ready, which it tries for up to 120 frames
// (`@legion/proof-editor`'s `lib.ts`) - and each change makes Milkdown's view again, which drops
// the focus to the page's body while the reader writes: the keys they press next went nowhere.
// A focus the editable loses to nothing, with no press of the reader's under it, comes back on
// the next frame at the caret it held. A press on blank space is the reader leaving the editor,
// and the focus stays where that press left it.
test("a focus the editor drops while the reader writes comes back at their caret, and a press elsewhere leaves it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const editable = await editStatement(user);
  // Something outside the outline that takes no focus.
  const outside = "Words outside the outline";
  const plain = render(<p>{outside}</p>).getByText(outside);
  const statement = wordsOf(editable, "r1");

  // The reader presses a key and moves the caret five characters into the statement.
  await user.keyboard("{Shift}");
  await select(statement, 5);
  await wrapAgain(editable);
  await waitFor(() =>
    expect(document.activeElement, "the focus the editor dropped did not come back").toBe(editable)
  );
  expect(window.getSelection()?.anchorNode, "the caret came back elsewhere").toBe(statement);
  expect(window.getSelection()?.anchorOffset, "the caret came back elsewhere").toBe(5);

  // A press on blank space takes the focus off the editable to nothing, and it stays there.
  await user.pointer({ keys: "[MouseLeft>]", target: plain });
  expect(document.activeElement, "the press did not take the focus off the editor").toBe(
    document.body
  );
  await user.pointer({ keys: "[/MouseLeft]" });
  await nextFrame();
  expect(document.activeElement, "the focus a press took off the editor came back").toBe(
    document.body
  );
});

// The editor can redraw its text while the editable has no focus - the words the reader's caret
// was in drawn anew - and the caret's points are gone then. The focus came back with no caret put
// in the editor, and the next key went where the browser puts the caret in an editable it focuses
// (in Chromium, the editable's start, outside the node's block). It comes back at the start of
// the node's block instead, as the editor's own in the task that puts it there. The frames are
// held, so the words are drawn anew before the frame that gives the focus back.
test("a focus the editor drops after it redrew the words of the reader's caret comes back in the node's block, as the editor's own", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null; offset?: number } = {};
  try {
    const editable = await editStatement(user, <ReportReadingEditor own={own} />);
    const statement = editable.querySelector<HTMLElement>('[data-block-id="r1"]');
    const words = statement?.firstChild;
    if (statement === null || words === null || words === undefined) {
      throw new Error("the editor draws no r1");
    }
    // The reader presses a key and moves the caret five characters into the statement, which
    // the editor reads.
    await user.keyboard("{Shift}");
    await act(async () => {
      window.getSelection()?.collapse(words, 5);
      document.dispatchEvent(new Event("selectionchange"));
    });
    expect(own.offset, "the editor did not read the reader's move").toBe(5);
    const frames = framesByHand();
    try {
      await wrapAgain(editable);
      // The editor draws the statement's words anew while it has no focus.
      await act(async () => {
        statement.textContent = "A statement of the spec.";
      });
      await frames.run();
    } finally {
      frames.restore();
    }
    expect(document.activeElement, "the focus the editor dropped did not come back").toBe(editable);
    expect(selected(), "the caret did not come back at the start of the node's block").toEqual([
      "r1:0",
      "r1:0",
    ]);
    expect(
      [own.block, own.offset],
      "the editor's own selection is not at the start of the node's block"
    ).toEqual(["r1", 0]);
  } finally {
    restore();
  }
});

// Where the editor has redrawn the words of the reader's caret and taken the node's block out as
// well, a focus it drops has no place in the node's text to come back at. An editable given the
// focus with no caret put in it takes the next key where the browser puts its caret, and a caret
// put at the editor's start edits the top of the document, so the focus stays where it fell. The
// frames are held, so the editor redraws before the frame that would give the focus back.
test("a focus the editor drops after it redrew the reader's caret and took the node's block out stays where it fell", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const editable = await editStatement(user);
  const words = wordsOf(editable, "r1");
  // The reader presses a key and moves the caret five characters into the statement.
  await user.keyboard("{Shift}");
  await select(words, 5);
  const frames = framesByHand();
  try {
    await wrapAgain(editable);
    // The editor draws the statement's block no more while it has no focus.
    await act(async () => {
      editable.querySelector('[data-block-id="r1"]')?.remove();
    });
    await frames.run();
  } finally {
    frames.restore();
  }
  expect(
    document.activeElement,
    "the focus came back to an editor that no longer draws the node's block"
  ).toBe(document.body);
  expect(
    editable.contains(window.getSelection()?.anchorNode ?? null),
    "a caret was put in the editor outside the node's block"
  ).toBe(false);
});

// The caret the focus comes back at is the editor's own in the task that puts it there, as
// Enter's is (above): the editor writes its own selection back over a caret it has not read.
// Here the reader's last move is one the editor never read - the editable dropped the focus
// before the browser reported it - so a caret put back and not reported would leave the editor's
// own selection at the statement's start, under the reader's caret five characters into it.
test("the caret a dropped focus comes back at is the editor's own in the task that puts it there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null; offset?: number } = {};
  try {
    const editable = await editStatement(user, <ReportReadingEditor own={own} />);
    const words = wordsOf(editable, "r1");
    // The reader presses a key and moves the caret five characters into the statement, and the
    // focus drops before the browser reports the move.
    await user.keyboard("{Shift}");
    await act(async () => {
      window.getSelection()?.collapse(words, 5);
    });
    expect(own.offset, "the editor read a move the browser had not reported").toBe(0);
    await wrapAgain(editable);
    await waitFor(() =>
      expect(document.activeElement, "the focus the editor dropped did not come back").toBe(
        editable
      )
    );
    expect(selected(), "the caret came back elsewhere").toEqual(["r1:5", "r1:5"]);
    expect(own.offset, "the editor's own selection is not the caret the focus came back at").toBe(
      5
    );
  } finally {
    restore();
  }
});

/** The statement's document with words above the statement and words below it. */
const BLOCKS_AROUND: StatementBlocks = [
  ["r0", "The words above the statement."],
  ["r1", "A statement of the spec."],
  ["r2", "The words below the statement."],
];

/** The words of block `id` the row's `editable` draws. */
function wordsOf(editable: HTMLElement, id: string): Text {
  const words = editable.querySelector(`[data-block-id="${id}"]`)?.firstChild;
  if (!(words instanceof Text)) throw new Error(`the editor draws no ${id}`);
  return words;
}

/** A move of the selection from `anchor` to `focus`, the browser reporting it. */
function select(
  anchor: Node,
  anchorOffset: number,
  focus: Node = anchor,
  focusOffset: number = anchorOffset
): Promise<void> {
  return act(async () => {
    window.getSelection()?.setBaseAndExtent(anchor, anchorOffset, focus, focusOffset);
    document.dispatchEvent(new Event("selectionchange"));
  });
}

/** Where the selection's anchor and its focus stand, each as its block's id and its offset:
 *  `r1:5` is five characters into r1's words, and an end on an element rather than in words reads
 *  `r1(element):0`. A comparison that fails prints what it compared, and a happy-dom node printed
 *  is the whole page it is in, which ran the test process out of memory: so no node is compared,
 *  only where it stands. */
function selected(): [anchor: string, focus: string] {
  const selection = window.getSelection();
  const placeOf = (node: Node | null | undefined, offset: number | undefined) => {
    const element = node instanceof Element ? node : node?.parentElement;
    const block = element?.closest("[data-block-id]")?.getAttribute("data-block-id") ?? "-";
    return `${block}${node instanceof Text ? "" : "(element)"}:${offset}`;
  };
  return [
    placeOf(selection?.anchorNode, selection?.anchorOffset),
    placeOf(selection?.focusNode, selection?.focusOffset),
  ];
}

/** Presses `key` in the row's `editable`, and answers whether it is left to the editor and the
 *  browser: false where the row refused it, or made the move it names itself. */
function pressIn(editable: HTMLElement, key: string, modifiers: KeyboardEventInit = {}): boolean {
  return editable.dispatchEvent(
    new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key, ...modifiers })
  );
}

/** A block of a document as the room holds it, in ProseMirror's JSON: its type, its attributes -
 *  its `blockId` among them, and a typed block's `block[]` pointers as the array they are - and
 *  what it holds. */
interface LiveBlock {
  readonly attrs?: Readonly<Record<string, string | number | readonly string[]>>;
  readonly content?: readonly (LiveBlock | LiveText)[];
  readonly type: string;
}

/** A run of a block's words. */
interface LiveText {
  readonly text: string;
  readonly type: "text";
}

/** `block` as the room holds it: a Yjs element of its type and attributes, holding its words and
 *  its blocks, as y-prosemirror writes a document there. Yjs holds an attribute of any value, a
 *  heading's level as the number it is, where its own types name strings alone. */
function roomBlock(block: LiveBlock): Y.XmlElement {
  const element = new Y.XmlElement(block.type);
  const attributes = element as Y.XmlElement<Record<string, string | number | readonly string[]>>;
  for (const [name, value] of Object.entries(block.attrs ?? {})) {
    attributes.setAttribute(name, value);
  }
  element.insert(
    0,
    (block.content ?? []).map((child) =>
      "text" in child ? new Y.XmlText(child.text) : roomBlock(child)
    )
  );
  return element;
}

/** A paragraph `blockId` of `words`. */
function paragraph(blockId: string, words: string): LiveBlock {
  return { attrs: { blockId }, content: [{ text: words, type: "text" }], type: "paragraph" };
}

/** A heading `blockId` of `words`, at `level`. */
function heading(blockId: string, level: number, words: string): LiveBlock {
  return { attrs: { blockId, level }, content: [{ text: words, type: "text" }], type: "heading" };
}

/** The statement's document, as the room holds it, with words above the statement and words
 *  below it: `statement` is the statement's own block, `r1`. */
function liveBlocks(
  statement: LiveBlock = paragraph("r1", "A statement of the spec.")
): LiveBlock[] {
  return [
    paragraph("r0", "The words above the statement."),
    statement,
    paragraph("r2", "The words below the statement."),
  ];
}

/** The room's copy of a document and the browser's, each update to either relayed to the other
 *  as the room relays a browser's (`doc/editor.test.ts`), and the row's editor of it, once made. */
interface LiveRoom {
  readonly browser: Y.Doc;
  editor?: EditorHandle;
  readonly server: Y.Doc;
}

function liveRoom(): LiveRoom {
  const browser = new Y.Doc();
  const server = new Y.Doc();
  for (const [from, to] of [
    [server, browser],
    [browser, server],
  ] as const) {
    from.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== "relay") Y.applyUpdate(to, update, "relay");
    });
  }
  return { browser, server };
}

/** The document's own editor (`doc/editor.ts`), made as `ProofDocument` makes it for the row: the
 *  room holds the document - `blocks` - before the editor is made, as it does once it has synced,
 *  so the editor draws the document as it is made (y-prosemirror draws as its plugin goes on) and
 *  is handed to the row (`onEditor`) only then, some microtasks on, and bound to the room's marks
 *  (`bindRemoteMarks`). */
function LiveEditor({
  blocks,
  onEditor,
  room,
}: {
  blocks: readonly LiveBlock[];
  onEditor: OnEditor;
  room: LiveRoom;
}): ReactNode {
  const root = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const parent = root.current;
    if (parent === null) return;
    let live = true;
    let made: EditorHandle | undefined;
    let unbindMarks = () => {};
    const held = room.server.getXmlFragment("prosemirror");
    if (held.length === 0) held.insert(0, blocks.map(roomBlock));
    void createEditor(parent, {
      awareness: null,
      blockSchema: compiledBlockSchema,
      onAskBlockHosts: () => {},
      user: { color: "#f59e0b", name: "alice" },
      watchPicture: () => () => {},
      ydoc: room.browser,
    }).then((handle) => {
      if (!live) {
        void handle.destroy();
        return;
      }
      made = handle;
      room.editor = handle;
      onEditor(handle);
      unbindMarks = bindRemoteMarks(room.browser, handle);
    });
    return () => {
      live = false;
      if (made === undefined) return;
      unbindMarks();
      void made.destroy();
      onEditor(undefined);
    };
  }, [blocks, onEditor, room]);
  return <div ref={root} />;
}

/** Enter on the statement's row, as `editStatement` does, with the document's own editor drawing
 *  `blocks` for it. Answers the editor's editable, its view and the room. */
async function editLive(
  user: UserEvent,
  blocks: readonly LiveBlock[] = liveBlocks()
): Promise<{ editable: HTMLElement; room: LiveRoom; view: EditorView }> {
  const room = liveRoom();
  const editable = await editStatement(
    user,
    (onEditor) => <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />,
    []
  );
  const view = room.editor?.view;
  if (view === undefined) throw new Error("the row's editor was never made");
  return { editable, room, view };
}

/** Each block of `view`'s document, in order: its depth, its id (`-` for none), its type - a
 *  heading's with its level - and a text block's words, a hard break read as a new line. Strings,
 *  never nodes, are what a case compares. */
function blocksOf(view: EditorView): string[] {
  const blocks: string[] = [];
  const { doc } = view.state;
  doc.descendants((node, position) => {
    if (!node.isBlock) return false;
    const id = typeof node.attrs.blockId === "string" ? node.attrs.blockId : "-";
    const type = node.type.name === "heading" ? `heading ${node.attrs.level}` : node.type.name;
    const depth = doc.resolve(position).depth;
    blocks.push(`${depth} ${id} ${type}${node.isTextblock ? `: ${node.textContent}` : ""}`);
    return true;
  });
  return blocks;
}

// The row's editor is the document's whole editor, and Ctrl+Home and Ctrl+End moved its caret to
// the top and the foot of the whole document: measured on a preview, Ctrl+Home put it before the
// spec's `## Summary`, and a `Y` typed then made it `## YSummary`, out of the box above the row.
// They move the caret to the start and the end of the node's own block, and so do Meta's and a
// Mac's Meta+Up and Meta+Down, as the editor's own in the same task; with Shift held they extend
// the selection there. happy-dom reports a move inside the call that makes it, and that report is
// dropped, as no listener hears a browser's in that task.
test("Ctrl+Home and Ctrl+End in a row's editor stop at the node's block's start and end, as the editor's own", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null; offset?: number } = {};
  try {
    const editable = await editStatement(user, <ReportReadingEditor own={own} />, BLOCKS_AROUND);
    const statement = wordsOf(editable, "r1");
    const end = statement.length;
    await select(statement, 5);
    for (const [key, modifiers, offset] of [
      ["End", { ctrlKey: true }, end],
      ["Home", { ctrlKey: true }, 0],
      ["End", { metaKey: true }, end],
      ["Home", { metaKey: true }, 0],
      ["ArrowDown", { metaKey: true }, end],
      ["ArrowUp", { metaKey: true }, 0],
    ] as const) {
      const name = `${Object.keys(modifiers)[0]}+${key}`;
      expect(pressIn(editable, key, modifiers), `${name} was left to the editor`).toBe(false);
      expect(selected(), `${name} put the caret elsewhere`).toEqual([
        `r1:${offset}`,
        `r1:${offset}`,
      ]);
      expect([own.block, own.offset], `the editor's own selection is not ${name}'s`).toEqual([
        "r1",
        offset,
      ]);
    }
    await select(statement, 5);
    expect(pressIn(editable, "End", { ctrlKey: true, shiftKey: true })).toBe(false);
    expect(selected(), "Shift+Ctrl+End did not extend the selection to the block's end").toEqual([
      "r1:5",
      `r1:${end}`,
    ]);
    expect(pressIn(editable, "Home", { ctrlKey: true, shiftKey: true })).toBe(false);
    expect(selected(), "Shift+Ctrl+Home did not extend the selection to the block's start").toEqual(
      ["r1:5", "r1:0"]
    );
  } finally {
    restore();
  }
});

// Ctrl+A selected the whole document, and a letter typed then replaced it. It selects the node's
// block's text alone, and so does Meta+A, a Mac's. A Mac's Ctrl+A is the editor's own move to the
// start of the line's paragraph (`prosemirror-commands`' `macBaseKeymap`), which stays inside the
// block, and it is left to the editor there.
test("Ctrl+A in a row's editor selects the node's block alone, and a Mac's is left to the editor", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const editable = await editStatement(user, undefined, BLOCKS_AROUND);
  const statement = wordsOf(editable, "r1");
  const whole: [string, string] = ["r1:0", `r1:${statement.length}`];
  for (const modifiers of [{ ctrlKey: true }, { metaKey: true }]) {
    await select(statement, 5);
    expect(pressIn(editable, "a", modifiers), "select-all was left to the editor").toBe(false);
    expect(selected(), "select-all selected other than the block's text").toEqual(whole);
  }
  Object.defineProperty(navigator, "platform", { configurable: true, value: "MacIntel" });
  try {
    await select(statement, 5);
    expect(pressIn(editable, "a", { ctrlKey: true }), "a Mac's Ctrl+A was not left to it").toBe(
      true
    );
    expect(selected(), "a Mac's Ctrl+A moved the caret itself").toEqual(["r1:5", "r1:5"]);
    expect(pressIn(editable, "a", { metaKey: true }), "a Mac's Meta+A was left to the editor").toBe(
      false
    );
    expect(selected(), "a Mac's Meta+A selected other than the block's text").toEqual(whole);
  } finally {
    Reflect.deleteProperty(navigator, "platform");
  }
});

/** The statement's words laid out on two lines, as a browser draws a caret's box at each place
 *  in them: the first ten characters on the line from 100 to 120 px down, the rest on the line
 *  from 120 to 140 px. happy-dom lays nothing out. Answers what restores happy-dom's own. */
function statementOnTwoLines(statement: Text): () => void {
  const boxes = spyOn(Range.prototype, "getClientRects").mockImplementation(function (this: Range) {
    const top = this.startContainer === statement && this.startOffset > 10 ? 120 : 100;
    return [new DOMRect(0, top, 0, 20)] as unknown as DOMRectList;
  });
  return () => boxes.mockRestore();
}

// Up from the first line of the node's block and Down from its last, Left at its start and Right
// at its end took the row's caret into the blocks beside it - measured on a preview, Up after Left
// put it before a heading above the statement - and PageUp and PageDown a page away. Each is
// refused there, and the caret stays where it was; inside the block each is the editor's. With
// Shift held, Up on the first line and Down on the last extend the selection to the block's end
// they stop at.
test("the keys that move past the node's block's edges stop at those edges", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const editable = await editStatement(user, undefined, BLOCKS_AROUND);
  const statement = wordsOf(editable, "r1");
  const end = statement.length;
  const lines = statementOnTwoLines(statement);
  try {
    for (const [at, key, modifiers, refused] of [
      [0, "ArrowUp", {}, true],
      [5, "ArrowUp", {}, true],
      [15, "ArrowUp", {}, false],
      [15, "ArrowDown", {}, true],
      [5, "ArrowDown", {}, false],
      [0, "ArrowLeft", {}, true],
      [0, "ArrowLeft", { ctrlKey: true }, true],
      [5, "ArrowLeft", {}, false],
      [end, "ArrowRight", {}, true],
      [5, "ArrowRight", {}, false],
      [5, "PageUp", {}, true],
      [5, "PageDown", {}, true],
    ] as const) {
      await select(statement, at);
      const name = `${Object.keys(modifiers).join("+")} ${key} at ${at}`;
      expect(!pressIn(editable, key, modifiers), `${name} refused: ${!refused}`).toBe(refused);
      expect(selected(), `${name} moved the caret`).toEqual([`r1:${at}`, `r1:${at}`]);
    }
    await select(statement, 5);
    expect(pressIn(editable, "ArrowUp", { shiftKey: true })).toBe(false);
    expect(selected(), "Shift+Up on the first line did not extend to the block's start").toEqual([
      "r1:5",
      "r1:0",
    ]);
    await select(statement, 15);
    expect(pressIn(editable, "ArrowDown", { shiftKey: true })).toBe(false);
    expect(selected(), "Shift+Down on the last line did not extend to the block's end").toEqual([
      "r1:15",
      `r1:${end}`,
    ]);
  } finally {
    lines();
  }
});

/** Types `text` at the editor's own selection as the editor takes typed text: each letter offered
 *  to its input rules (`handleTextInput`, which the editor calls as it reads the letter the browser
 *  put in the page), and put in where no rule takes it. */
function typeInEditor(view: EditorView, text: string): void {
  act(() => {
    for (const letter of text) {
      const { from, to } = view.state.selection;
      const typed = () => view.state.tr.insertText(letter, from, to);
      const taken = view.someProp("handleTextInput", (handle) =>
        handle(view, from, to, letter, typed)
      );
      if (taken !== true) view.dispatch(typed());
    }
  });
}

/** `before`, the blocks `blocksOf` reads, with block `id`'s line, `0 id paragraph: …`, now
 *  holding `words`. */
function withWords(before: readonly string[], id: string, words: string): string[] {
  return before.map((line) =>
    line.startsWith(`0 ${id} `) ? line.replace(/: .*$/s, `: ${words}`) : line
  );
}

// Backspace at the node's block's start and Delete at its end, Ctrl's too, joined it to the block
// beside it. The editor holds every edit of its own to the block (`holdEditsToBlock`), so each
// leaves the document as it was there; inside the block they are the browser's, and a letter
// typed goes in.
test("Backspace at the node's block's start and Delete at its end join nothing to the block beside it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  const end = wordsOf(editable, "r1").length;
  for (const [at, key, modifiers] of [
    [0, "Backspace", {}],
    [0, "Backspace", { ctrlKey: true }],
    [end, "Delete", {}],
    [end, "Delete", { ctrlKey: true }],
  ] as const) {
    await select(wordsOf(editable, "r1"), at);
    pressIn(editable, key, modifiers);
    const name = `${Object.keys(modifiers).join("+")} ${key} at ${at}`;
    expect(blocksOf(view), `${name} changed the document`).toEqual(before);
  }
  await select(wordsOf(editable, "r1"), 5);
  expect(pressIn(editable, "Backspace"), "Backspace inside the block was not the browser's").toBe(
    true
  );
  typeInEditor(view, "Y");
  expect(blocksOf(view), "a letter typed inside the block did not go in").toEqual(
    withWords(before, "r1", "A staYtement of the spec.")
  );
});

// Enter splits the block the caret is in, and in a block that is its own paragraph - a
// decision's heading - the second half is a new block after the node's, with the caret in it.
// The editor refuses it there (`holdEditsToBlock`); Shift+Enter in a paragraph is a line break
// inside the block, and goes in.
test("Enter in a row's editor is refused where it would split the node's block in two", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  await select(wordsOf(editable, "r1"), 5);
  pressIn(editable, "Enter");
  expect(blocksOf(view), "Enter split the node's block").toEqual(before);
  pressIn(editable, "Enter", { shiftKey: true });
  expect(blocksOf(view), "Shift+Enter put no line break in the paragraph").toEqual(
    withWords(before, "r1", "A sta\ntement of the spec.")
  );
});

// A typed block holds its paragraphs, and no split crosses its sides: Enter in it makes a
// paragraph inside it, and so does a paste of two paragraphs, which the editor lets in.
test("Enter and a paste of two paragraphs in a typed block of a row's editor make paragraphs inside it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(
    user,
    liveBlocks({
      attrs: { blockId: "r1" },
      content: [
        { content: [{ text: "A statement of the spec.", type: "text" }], type: "paragraph" },
      ],
      type: "requirement",
    })
  );
  const before = blocksOf(view);
  const words = editable.querySelector('[data-block-id="r1"] p')?.firstChild;
  if (!(words instanceof Text)) throw new Error("the editor draws no words in the requirement");
  await select(words, 5);
  const inRequirement = () => {
    const requirement = view.state.doc.child(1);
    const inside: string[] = [];
    requirement.forEach((child) => {
      inside.push(child.textContent);
    });
    return [requirement.type.name, requirement.attrs.blockId, ...inside];
  };
  pressIn(editable, "Enter");
  expect(inRequirement(), "Enter made no paragraph inside the requirement").toEqual([
    "requirement",
    "r1",
    "A sta",
    "tement of the spec.",
  ]);
  pasteIn(editable, "Two\n\nparagraphs");
  expect(inRequirement(), "two paragraphs pasted did not go into the requirement").toEqual([
    "requirement",
    "r1",
    "A sta",
    "Two",
    "paragraphstement of the spec.",
  ]);
  expect(
    [blocksOf(view)[0], blocksOf(view).at(-1)],
    "an edit in the requirement changed the blocks beside it"
  ).toEqual([before[0], before.at(-1)]);
});

// Milkdown lowers a heading a level for Delete or Backspace at its start (DowngradeHeading), makes
// it a paragraph for Ctrl+Alt+0, and a heading of another level for `# ` typed at its start; and
// Shift+Enter puts a line break in it, which stores as a heading of two lines. The editor refuses
// each in the node's heading (`holdEditsToBlock`), and leaves every block as it was; a letter
// typed goes in.
test("the keys that would change the node's heading's level or lines leave it as it was", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, liveBlocks(heading("r1", 2, "A heading")));
  const before = blocksOf(view);
  for (const [key, modifiers] of [
    ["Delete", {}],
    ["Backspace", {}],
    ["0", { altKey: true, ctrlKey: true }],
    ["Enter", { shiftKey: true }],
  ] as const) {
    await select(wordsOf(editable, "r1"), 0);
    pressIn(editable, key, modifiers);
    const name = `${Object.keys(modifiers).join("+")} ${key}`;
    expect(blocksOf(view), `${name} at the heading's start changed the document`).toEqual(before);
  }
  await select(wordsOf(editable, "r1"), 0);
  typeInEditor(view, "# ");
  expect(blocksOf(view), "`# ` typed at the heading's start changed its level").toEqual(
    withWords(before, "r1", "#A heading")
  );
  // Milkdown names the heading from its words after each change of them (`syncHeadingIdPlugin`),
  // in a transaction of its own that changes the heading's `id`: no edit of the reader's, and the
  // editor lets it in.
  expect(view.state.doc.child(1).attrs.id, "the heading's name did not follow its words").toBe(
    "#a-heading"
  );
});

// A decision is a `decision` block whose heading and words are blocks of their own inside it, and
// the editor holds every block inside the node's block to the type and the attributes it had, as
// it holds the node's block itself (`holdEditsToBlock`): measured on the outline's row of a
// decision, three Backspaces at its heading's start made the heading a paragraph. Delete and
// Backspace at the heading's start lower it a level (DowngradeHeading) and Ctrl+Alt+0 makes it a
// paragraph; Backspace at the start of the words joins them to the heading, and Ctrl+Alt+1 and
// `# ` typed at their start make them a heading. The editor refuses each, and leaves every block
// as it was; a letter typed in either goes in. The editor mints an id for the heading as it draws
// it, so Enter's caret at the decision's start is in a block of another id than `r1`, inside `r1`'s
// section: the row is entered here rather than through `editLive`, which waits for the caret in a
// block of `r1`'s own id.
test("the keys that would change a block inside the node's decision leave it as it was", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const blocks = liveBlocks({
    attrs: { blockId: "r1" },
    content: [
      {
        attrs: { level: 3 },
        content: [{ text: "Ship behind a flag", type: "text" }],
        type: "heading",
      },
      { content: [{ text: "We ship it behind a flag.", type: "text" }], type: "paragraph" },
    ],
    type: "decision",
  });
  const room = liveRoom();
  const { editable } = await enterStatement(user, (onEditor) => (
    <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />
  ));
  const caretInDecision = () => {
    const anchor = window.getSelection()?.anchorNode;
    const element = anchor instanceof Element ? anchor : anchor?.parentElement;
    return element?.closest('[data-block-id="r1"]') != null;
  };
  await waitFor(
    () =>
      expect(
        document.activeElement === editable && caretInDecision(),
        "Enter did not put the caret in the decision"
      ).toBe(true),
    { timeout: 10_000 }
  );
  const view = room.editor?.view;
  if (view === undefined) throw new Error("the row's editor was never made");
  /** The words of the decision's block `tag` draws: its heading, `h3`, or its words, `p`. */
  const wordsIn = (tag: "h3" | "p"): Text => {
    const drawn = editable.querySelector(`[data-block-id="r1"] ${tag}`)?.firstChild;
    if (!(drawn instanceof Text)) throw new Error(`the editor draws no ${tag} in the decision`);
    return drawn;
  };
  /** The blocks `blocksOf` reads, with the id of each block inside the decision left out: the
   *  editor mints one for a block of it an edit goes into. */
  const shapes = (lines: readonly string[]) => lines.map((line) => line.replace(/^1 \S+ /, "1 "));
  const before = blocksOf(view);
  /** `lines`, read without ids (`shapes`), with the line of the decision's block of `type`,
   *  `heading 3` or `paragraph`, now holding `text`. */
  const inside = (lines: readonly string[], type: string, text: string) =>
    lines.map((line) => (line.startsWith(`1 ${type}: `) ? `1 ${type}: ${text}` : line));
  for (const [tag, key, modifiers] of [
    ["h3", "Delete", {}],
    ["h3", "Backspace", {}],
    ["h3", "0", { altKey: true, ctrlKey: true }],
    ["p", "Backspace", {}],
    ["p", "1", { altKey: true, ctrlKey: true }],
  ] as const) {
    await select(wordsIn(tag), 0);
    pressIn(editable, key, modifiers);
    const name = `${Object.keys(modifiers).join("+")} ${key} at the ${tag}'s start`;
    expect(blocksOf(view), `${name} changed the document`).toEqual(before);
  }
  await select(wordsIn("p"), 0);
  typeInEditor(view, "# ");
  const typed = inside(shapes(before), "paragraph", "#We ship it behind a flag.");
  expect(shapes(blocksOf(view)), "`# ` typed at the words' start made them a heading").toEqual(
    typed
  );
  await select(wordsIn("h3"), 4);
  typeInEditor(view, "Y");
  expect(shapes(blocksOf(view)), "a letter typed in the heading did not go in").toEqual(
    inside(typed, "heading 3", "ShipY behind a flag")
  );
});

// The editor answers Backspace, Delete and Enter itself, on its own selection, which it reads from
// the page only once the browser reports a move of the caret, a task later. Measured in a Job like
// CI's, Backspace pressed right after ArrowRight at a heading's start acted at the start, where
// Milkdown lowers the heading a level: the editor refused that (`holdEditsToBlock`), and the
// letter before the caret stayed. Milkdown lowers a heading a level for Delete at its start too
// (DowngradeHeading), so a Delete the editor answered on its own caret would be that lowering, and
// the letter after the caret would stay. The row has the editor read the caret first
// (`confineKey`), so each key there is the browser's delete of its letter, and the heading keeps
// its level. happy-dom moves no caret for an arrow and deletes nothing for either key, so each
// case makes each move and each delete the browser would, and drops happy-dom's report of a move
// made inside the call that makes it. It moves the caret and presses the key in one task: a timer
// the editor set as the caret went in, which a reader's first keys come long after, could
// otherwise run between them, and read the move, or write the editor's own selection back over it
// (its focus handler's, 20 ms on).
for (const [key, side, at, remaining] of [
  ["Backspace", "before", 0, "A heading"],
  ["Delete", "after", 1, "Y heading"],
] as const) {
  test(`${key} right after ArrowRight at the node's heading's start deletes the letter ${side} the caret`, async () => {
    setViewport(1440);
    const user = userEvent.setup();
    const { editable, view } = await editLive(user, liveBlocks(heading("r1", 2, "YA heading")));
    const before = blocksOf(view);
    const restore = dropSelectionReports();
    try {
      const words = wordsOf(editable, "r1");
      const ownOffset = () => view.state.selection.$head.parentOffset;
      expect(
        [caretBlock(), ownOffset()],
        "the editor's own caret is not at the heading's start"
      ).toEqual(["r1", 0]);
      expect(pressIn(editable, "ArrowRight"), "ArrowRight was not left to the browser").toBe(true);
      window.getSelection()?.collapse(words, 1);
      expect(ownOffset(), "the editor read the move before the browser reported it").toBe(0);
      if (pressIn(editable, key)) {
        await changePage(() => {
          words.deleteData(at, 1);
          window.getSelection()?.collapse(words, at);
        });
      }
      expect(
        blocksOf(view),
        `${key} right after ArrowRight did not delete the letter ${side} the caret`
      ).toEqual(withWords(before, "r1", remaining));
    } finally {
      restore();
    }
  });
}

// Milkdown bolds the selection for Ctrl+B (Meta+B on a Mac, `Mod-b`), the editor's own: right
// after Shift+ArrowRight selected two letters, before the browser reported the last move, the bold
// went on the editor's own selection, still short of them - an empty one's bold is a mark for the
// next letter typed - and the letters selected stayed as they were. The row has the editor read
// the selection first (`confineKey`), so the bold goes on the letters selected. happy-dom moves no
// selection for an arrow, so the case makes each move the browser would, in one task with the
// keys, and drops happy-dom's report of it, as the Backspace case above does.
test("Ctrl+B right after Shift+ArrowRight in the node's paragraph bolds the letters selected", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, room, view } = await editLive(
    user,
    liveBlocks(paragraph("r1", "Bold the first two letters."))
  );
  const editor = room.editor;
  if (editor === undefined) throw new Error("the row's editor was never made");
  const before = editor.getMarkdown();
  const restore = dropSelectionReports();
  try {
    const words = wordsOf(editable, "r1");
    const ownSelection = () => {
      const { $anchor, $head } = view.state.selection;
      return [$anchor.parentOffset, $head.parentOffset];
    };
    expect(
      [caretBlock(), ...ownSelection()],
      "the editor's own caret is not at the paragraph's start"
    ).toEqual(["r1", 0, 0]);
    for (const end of [1, 2]) {
      expect(
        pressIn(editable, "ArrowRight", { shiftKey: true }),
        `Shift+ArrowRight to ${end} was not left to the browser`
      ).toBe(true);
      window.getSelection()?.setBaseAndExtent(words, 0, words, end);
    }
    expect(
      ownSelection(),
      "the editor read the last move before the browser reported it"
    ).not.toEqual([0, 2]);
    // `Mod-b` is the key of the platform the editor's keymap read as it loaded
    // (`prosemirror-keymap`).
    const mod = MAC_PLATFORM.test(navigator.platform) ? "metaKey" : "ctrlKey";
    pressIn(editable, "b", { [mod]: true });
    expect(editor.getMarkdown(), "Ctrl+B did not bold the two letters selected").toBe(
      before.replace("Bold the", "**Bo**ld the")
    );
  } finally {
    restore();
  }
});

// While the reader composes with an input method, the input method holds the caret, and
// ProseMirror reads the page itself as the composition starts and as it ends: the row has the
// editor read no caret for a key pressed in a composition - the `Process` a browser sends for a key
// the input method takes, or any key with `isComposing` - and reads it first for a key pressed
// after, as for Backspace. The case makes the move and the delete the browser would, as the
// Backspace case above does.
test("a key pressed in a composition has the row's editor read no caret, and Backspace after it does", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, liveBlocks(heading("r1", 2, "YA heading")));
  const before = blocksOf(view);
  const restore = dropSelectionReports();
  let reports = 0;
  const count = () => {
    reports += 1;
  };
  document.addEventListener("selectionchange", count);
  try {
    const words = wordsOf(editable, "r1");
    const ownOffset = () => view.state.selection.$head.parentOffset;
    expect(
      [caretBlock(), ownOffset()],
      "the editor's own caret is not at the heading's start"
    ).toEqual(["r1", 0]);
    expect(pressIn(editable, "ArrowRight"), "ArrowRight was not left to the browser").toBe(true);
    window.getSelection()?.collapse(words, 1);
    expect(ownOffset(), "the editor read the move before the browser reported it").toBe(0);
    for (const [key, modifiers] of [
      ["Process", { keyCode: 229 }],
      ["a", { isComposing: true }],
    ] as const) {
      reports = 0;
      pressIn(editable, key, modifiers);
      expect(reports, `${key} pressed in a composition had the editor read the caret`).toBe(0);
    }
    expect(ownOffset(), "the editor read the caret during the composition").toBe(0);
    reports = 0;
    const left = pressIn(editable, "Backspace");
    expect(reports, "Backspace after the composition had the editor read no caret").toBe(1);
    if (left) {
      await changePage(() => {
        words.deleteData(0, 1);
        window.getSelection()?.collapse(words, 0);
      });
    }
    expect(
      blocksOf(view),
      "Backspace after the composition did not delete the letter before the caret"
    ).toEqual(withWords(before, "r1", "A heading"));
  } finally {
    document.removeEventListener("selectionchange", count);
    restore();
  }
});

// The row has its editor read the caret before a key the editor answers (`confineKey`), and that
// read is the document's `selectionchange`, which every listener on the page hears. The editor's
// action bar reads each one on a touch selection as the reader's new selection, which it hides and
// shows again 400 ms later (`@legion/proof-editor`'s `dispatch-action-bar.ts`): read before every
// key, the bar blinked at each Ctrl+C on a touch device with a keyboard. The read is made only
// where the page's selection is not the editor's own, so Ctrl+C on a selection the editor holds
// leaves the bar showing. The bar reads a selection as a touch one from the reader's last touch on
// the editable, which the case makes as the browser reports it, before the selection.
test("Ctrl+C on a touch selection the row's editor holds leaves its action bar showing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const words = wordsOf(editable, "r1");
  await act(async () => {
    editable.dispatchEvent(new Event("touchstart", { bubbles: true }));
    editable.dispatchEvent(new Event("touchend", { bubbles: true }));
  });
  await select(words, 2, words, 11);
  const { $anchor, $head } = view.state.selection;
  expect(
    [$anchor.parentOffset, $head.parentOffset],
    "the editor did not take the reader's selection as its own"
  ).toEqual([2, 11]);
  // The bar is drawn beside the editable, and drawn again as the editor's plugins change.
  const bar = () =>
    editable.parentElement?.querySelector<HTMLElement>(":scope > .dispatch-action-bar")?.style
      .display;
  await waitFor(
    () => expect(bar(), "the action bar did not show the touch selection").toBe("flex"),
    {
      timeout: 2_000,
    }
  );
  // `Mod-c` is the platform's own (`prosemirror-keymap`).
  const mod = MAC_PLATFORM.test(navigator.platform) ? "metaKey" : "ctrlKey";
  pressIn(editable, "c", { [mod]: true });
  expect(bar(), "Ctrl+C on the selection the editor holds hid the action bar").toBe("flex");
});

// Milkdown's block shortcuts make the block the caret is in another: Ctrl+Alt+1 a heading,
// Ctrl+Shift+B a quote around it, Ctrl+Alt+7 and Ctrl+Alt+8 a list around it, Ctrl+Alt+C a code
// block, and `# ` typed at its start a heading. The editor refuses each in the node's paragraph
// (`holdEditsToBlock`), and leaves every block as it was.
test("the editor's block shortcuts leave the node's paragraph, and the blocks around it, as they were", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  for (const [key, modifiers] of [
    ["1", { altKey: true, ctrlKey: true }],
    ["B", { code: "KeyB", ctrlKey: true, keyCode: 66, shiftKey: true }],
    ["7", { altKey: true, ctrlKey: true }],
    ["8", { altKey: true, ctrlKey: true }],
    ["c", { altKey: true, ctrlKey: true }],
  ] as const) {
    await select(wordsOf(editable, "r1"), 5);
    pressIn(editable, key, modifiers);
    const name = `${Object.keys(modifiers).join("+")} ${key}`;
    expect(blocksOf(view), `${name} changed the document`).toEqual(before);
  }
  await select(wordsOf(editable, "r1"), 0);
  typeInEditor(view, "# ");
  expect(blocksOf(view), "`# ` typed at the paragraph's start made it a heading").toEqual(
    withWords(before, "r1", "#A statement of the spec.")
  );
});

/** A list item `blockId` holding `block`. */
function listItem(blockId: string, block: LiveBlock): LiveBlock {
  return { attrs: { blockId }, content: [block], type: "list_item" };
}

// Milkdown sinks the list item the caret is in into the one before it for Tab and Ctrl+], and
// lifts it out of its list for Shift+Tab and Ctrl+[. Each moves the node's block into another
// item, which the editor refuses (`holdEditsToBlock`) for a block in a list item.
test("Tab and Shift+Tab in a list item leave the node's block in its list item", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, [
    {
      attrs: { blockId: "l" },
      content: [
        listItem("i0", paragraph("r0", "The item above the statement.")),
        listItem("i1", paragraph("r1", "A statement of the spec.")),
        listItem("i2", paragraph("r2", "The item below the statement.")),
      ],
      type: "bullet_list",
    },
  ]);
  const before = blocksOf(view);
  for (const [key, modifiers] of [
    ["Tab", {}],
    ["Tab", { shiftKey: true }],
    ["]", { ctrlKey: true }],
    ["[", { ctrlKey: true }],
  ] as const) {
    await select(wordsOf(editable, "r1"), 5);
    pressIn(editable, key, modifiers);
    const name = `${Object.keys(modifiers).join("+")} ${key}`;
    expect(blocksOf(view), `${name} moved the list item`).toEqual(before);
  }
});

// Milkdown sinks the list item its own selection is in for Tab (SinkListItem), and the editor
// reads that selection from the page only once the browser reports a move of the caret, a task
// later: a Tab right after ArrowDown to the next item, before that report, would sink the item the
// caret had left. The row has the editor read the caret first (`confineKey`), so Tab sinks the item
// the caret is in. The node's block is the list itself, so a sink of one of its items into the one
// before it stays inside the block, and the editor lets it in (`holdEditsToBlock`). The items and
// their paragraphs carry no id, so the caret in any of them is in the node's block (`caretBlock`);
// the editor mints one for each block a sink makes or moves, so the case reads each paragraph by
// its depth and its words alone. happy-dom moves no caret for an arrow, so the case makes the move
// the browser would, in one task with the Tab, and drops happy-dom's report of it, as the
// Backspace case above does.
test("Tab right after the caret moved to the next item of the node's list sinks that item", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const item = (words: string): LiveBlock => ({
    content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
    type: "list_item",
  });
  const { editable, view } = await editLive(
    user,
    liveBlocks({
      attrs: { blockId: "r1" },
      content: [item("The first item."), item("The second item."), item("The third item.")],
      type: "bullet_list",
    })
  );
  const [, second, third] = Array.from(
    editable.querySelectorAll('[data-block-id="r1"] p'),
    (paragraph) => paragraph.firstChild
  );
  if (!(second instanceof Text) || !(third instanceof Text)) {
    throw new Error("the editor draws no three items in the node's list");
  }
  const ownItem = () => view.state.selection.$head.parent.textContent;
  const restore = dropSelectionReports();
  try {
    await select(second, 3);
    expect(ownItem(), "the editor's own caret is not in the second item").toBe("The second item.");
    window.getSelection()?.collapse(third, 3);
    expect(ownItem(), "the editor read the move before the browser reported it").toBe(
      "The second item."
    );
    pressIn(editable, "Tab");
    // Each paragraph's depth and words: 2 for an item of the node's list, 4 for an item of a
    // list in one of its items.
    const outline = blocksOf(view).flatMap((line) => {
      const read = /^(\d+) \S+ paragraph: (.*)$/s.exec(line);
      return read === null ? [] : [`${read[1]} ${read[2]}`];
    });
    expect(outline, "Tab did not sink the item the caret moved to").toEqual([
      "0 The words above the statement.",
      "2 The first item.",
      "2 The second item.",
      "4 The third item.",
      "0 The words below the statement.",
    ]);
  } finally {
    restore();
  }
});

/** A plan task `blockId` of `words`, which serves the words above the statement: the editor draws
 *  its header `contenteditable="false"`, with a link to the block it serves
 *  (`typed-block-drawing.ts`'s `renderTypedBlock`), where the browser stops for Tab. */
function taskServing(blockId: string, words: string): LiveBlock {
  return {
    attrs: { blockId, serves: [`dispatch://${KEY}/spec#b-r0`] },
    content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
    type: "plan_task",
  };
}

/** The link the plan task `blockId` of the row's `editable` draws in its header. */
function linkOf(editable: HTMLElement, blockId: string): HTMLElement {
  const link = editable.querySelector<HTMLElement>(`[data-block-id="${blockId}"] header a[href]`);
  if (link === null) throw new Error(`the editor draws no link in ${blockId}`);
  return link;
}

/** Whether `row`, which held the editor, has let go of it - the editing ended - and draws no
 *  focused look, as a row the focus left the outline from. */
function rowLetGo(row: HTMLElement): boolean {
  return (
    row.querySelector('[contenteditable="true"]') === null &&
    row.querySelector("[data-node-card]")?.className.includes(pageFocusedRow) !== true
  );
}

// Milkdown answers Tab in a list item alone, and the browser moved the focus on from the row's
// editor to the next control the editor draws: driving a preview, another block's, and the
// editor's box scrolled to it, away from the node's block. Tab from the row's editor goes past
// every control the editor draws outside the node's block, to the first tab stop after its box,
// here a control of the page below the outline, and the editing ends as a Tab out of the outline
// does: the row lets go of the editor, its body and its focused look. The document stays as it
// was.
test("Tab from the row's editor goes past a control another block draws to the first tab stop after its box, and ends the editing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, [
    paragraph("r0", "The words above the statement."),
    paragraph("r1", "A statement of the spec."),
    taskServing("t2", "The task below the statement."),
  ]);
  const outside = controlOutside();
  const link = linkOf(editable, "t2");
  const row = editable.closest<HTMLElement>("[data-node-row]");
  if (row === null) throw new Error("the statement's row does not hold the editor");
  const before = blocksOf(view);
  await user.tab();
  expect(document.activeElement === link, "Tab stopped on the link the next block draws").toBe(
    false
  );
  expect(
    document.activeElement === outside.control,
    "Tab from the row's editor did not go to the first tab stop after its box"
  ).toBe(true);
  expect(blocksOf(view), "Tab changed the document").toEqual(before);
  await waitFor(() =>
    expect(rowLetGo(row), "the row kept its editing, its body or its focused look").toBe(true)
  );
});

// Shift+Tab from the row's editor goes past every control the editor draws outside the node's
// block to the row, the outline's one tab stop, and the editing ends there as Escape's does, the
// focus staying on the row. The document stays as it was. Shift+Tab steps back from the editable
// to what stands before it in the page's order, never into the blocks it holds, so the control
// it passes over here is drawn before the editable, inside the box: the references button of the
// block above the node's (`doc/ProofDocument.tsx`'s "Block references"), in an aside the case
// draws there, as the references-button case below draws one after the editable.
test("Shift+Tab from the row's editor goes to the row past a control another block draws, and ends the editing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const row = editable.closest<HTMLElement>("[data-node-row]");
  const box = editable.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  if (row === null || box === null) throw new Error("the statement's row does not hold the editor");
  const aside = document.createElement("aside");
  aside.setAttribute("aria-label", "Block references");
  const above = document.createElement("button");
  above.type = "button";
  above.setAttribute("aria-label", "1 references on block r0");
  above.setAttribute("data-block-references", "r0");
  aside.append(above);
  box.prepend(aside);
  const before = blocksOf(view);
  await user.tab({ shift: true });
  expect(
    document.activeElement === above,
    "Shift+Tab stopped on the references button of the block above"
  ).toBe(false);
  expect(
    document.activeElement === row,
    "Shift+Tab from the row's editor did not go to the row"
  ).toBe(true);
  expect(blocksOf(view), "Shift+Tab changed the document").toEqual(before);
  await waitFor(() =>
    expect(row.querySelector('[contenteditable="true"]') === null, "the editing did not end").toBe(
      true
    )
  );
  await nextFrame();
  expect(document.activeElement === row, "the focus left the row as the editing ended").toBe(true);
});

// A Tab pressed in an input method's composition is the input method's - marked `isComposing`,
// or unmarked with keyCode 229 as Safari sends the key that ends one - and ends no editing: the
// focus stays in the row's editor, and the row keeps its editing.
test.each([
  ["isComposing", { isComposing: true }],
  ["keyCode 229", { isComposing: false, keyCode: 229 }],
] as const)("a Tab pressed in an input-method composition in the row's editor, marked by %s, leaves the editing and the focus as they were", async (_mark, modifiers) => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await caretInStatement(user);
  controlOutside();
  await act(async () => {
    pressIn(editable, "Tab", modifiers);
  });
  await nextFrame();
  expect(
    document.activeElement === editable,
    "a Tab pressed in a composition moved the focus out of the row's editor"
  ).toBe(true);
  expect(view.edited.at(-1), "a Tab pressed in a composition ended the editing").toBe(
    "artifact-spec"
  );
});

// What the node's own block draws is the node's: from the caret in a plan task's words, Tab stops
// at the link the task's header draws, as the browser would, and the editing stays. Tab from that
// link goes on past the controls the editor draws outside the block, to the first tab stop after
// its box, and ends the editing.
test("Tab from the row's editor stops at a link the node's own block draws, and the editing stays", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable } = await editLive(user, [
    paragraph("r0", "The words above the statement."),
    taskServing("r1", "A statement of the spec."),
    taskServing("t2", "The task below the statement."),
  ]);
  const outside = controlOutside();
  const row = editable.closest<HTMLElement>("[data-node-row]");
  if (row === null) throw new Error("the statement's row does not hold the editor");
  await user.tab();
  expect(
    document.activeElement === linkOf(editable, "r1"),
    "Tab did not stop at the link the node's block draws"
  ).toBe(true);
  await nextFrame();
  expect(
    row.querySelector('[contenteditable="true"]') === editable,
    "the editing ended at a link of the node's block"
  ).toBe(true);
  await user.tab();
  expect(
    document.activeElement === outside.control,
    "Tab from the node's link did not go to the first tab stop after the editor's box"
  ).toBe(true);
  await waitFor(() =>
    expect(rowLetGo(row), "the row kept its editing, its body or its focused look").toBe(true)
  );
});

// The editor draws its action bar beside the editable, outside every block, but the bar's
// Comment, Suggest and Ask act on the selection, which the row holds inside the node's block: while
// the bar shows, Tab from the row's editor stops at each of its controls in turn, as it did before
// the editor's other controls were passed over, and the editing stays. Tab from the bar's last
// control goes on by the same rule, past the box, and ends the editing. The selection is a touch
// one, as the Ctrl+C case above makes it.
test("Tab from the row's editor stops at each control of the action bar its selection shows, and the editing stays", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable } = await editLive(user, [
    paragraph("r0", "The words above the statement."),
    paragraph("r1", "A statement of the spec."),
    taskServing("t2", "The task below the statement."),
  ]);
  const outside = controlOutside();
  const row = editable.closest<HTMLElement>("[data-node-row]");
  if (row === null) throw new Error("the statement's row does not hold the editor");
  const words = wordsOf(editable, "r1");
  await act(async () => {
    editable.dispatchEvent(new Event("touchstart", { bubbles: true }));
    editable.dispatchEvent(new Event("touchend", { bubbles: true }));
  });
  await select(words, 2, words, 11);
  await waitFor(
    () =>
      expect(
        editable.parentElement?.querySelector<HTMLElement>(":scope > .dispatch-action-bar")?.style
          .display,
        "the action bar did not show the touch selection"
      ).toBe("flex"),
    { timeout: 2_000 }
  );
  for (const name of ["Comment", "Suggest", "Ask"]) {
    await user.tab();
    const focused = document.activeElement;
    expect(
      focused?.closest(".dispatch-action-bar") !== null && focused?.textContent === name,
      `Tab did not stop at the action bar's ${name}`
    ).toBe(true);
    expect(
      row.querySelector('[contenteditable="true"]') === editable,
      `the editing ended at the action bar's ${name}`
    ).toBe(true);
  }
  await user.tab();
  expect(
    document.activeElement === outside.control,
    "Tab from the action bar did not go to the first tab stop after the editor's box"
  ).toBe(true);
  await waitFor(() =>
    expect(rowLetGo(row), "the row kept its editing, its body or its focused look").toBe(true)
  );
});

// A document draws a button for each block with comments or asks on it, which shows them and puts
// the reader in that block, in an aside of its own beside the editor, outside every block
// (`doc/ProofDocument.tsx`'s "Block references", each marked with its block's id). The node's own
// block's button is the node's: Tab from the row's editor stops at it, and the editing stays;
// Shift+Tab from it goes back to the editable, and the next Tab from there to it again, and on to
// the first tab stop after the box, which ends the editing. Another block's button, before the
// node's in the aside and after it, is never a stop either way. The editor here is the document's
// own (`LiveEditor`), which draws no aside, so the case draws one beside it as the page does.
test("Tab from the row's editor stops at the references button of the node's own block, and at no other block's", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable } = await editLive(user, [
    paragraph("r0", "The words above the statement."),
    paragraph("r1", "A statement of the spec."),
    taskServing("t2", "The task below the statement."),
  ]);
  const outside = controlOutside();
  const row = editable.closest<HTMLElement>("[data-node-row]");
  const box = editable.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  if (row === null || box === null) throw new Error("the statement's row does not hold the editor");
  const aside = document.createElement("aside");
  aside.setAttribute("aria-label", "Block references");
  for (const id of ["r0", "r1", "t2"]) {
    const button = document.createElement("button");
    button.type = "button";
    button.setAttribute("aria-label", `1 references on block ${id}`);
    button.setAttribute("data-block-references", id);
    aside.append(button);
  }
  box.append(aside);
  /** The block whose references button holds the focus, the editable, or `elsewhere`. */
  const focused = () => {
    const active = document.activeElement;
    if (active === editable) return "the editable";
    return active?.getAttribute("data-block-references") ?? "elsewhere";
  };

  await user.tab();
  expect(focused(), "Tab did not stop at the node's block's references button").toBe("r1");
  await nextFrame();
  expect(
    row.querySelector('[contenteditable="true"]') === editable,
    "the editing ended at the node's block's references button"
  ).toBe(true);
  await user.tab({ shift: true });
  expect(
    focused(),
    "Shift+Tab from the node's references button did not go back to the editable"
  ).toBe("the editable");
  await user.tab();
  expect(focused(), "Tab did not stop at the node's block's references button again").toBe("r1");
  await user.tab();
  expect(
    document.activeElement === outside.control,
    `Tab from the node's references button went to ${focused()}, not the first tab stop after the editor's box`
  ).toBe(true);
  await waitFor(() =>
    expect(rowLetGo(row), "the row kept its editing, its body or its focused look").toBe(true)
  );
});

// A comment's quote is anchored to the innermost block that holds it: driving the page, a comment
// on a statement's words named the paragraph inside the statement's typed block, by the id the
// editor gave that paragraph, and the references button named the same. So a button that names a
// block inside the node's block is the node's too, and Tab from the row's editor stops at it; one
// that names a block beside the node's, or a block the editor does not draw, is not.
test("a references button of a block inside the node's block is a tab stop of the row's editor, and one of a block beside it is not", () => {
  const row = document.createElement("div");
  const buttons = ["r0", "p1", "r1", "gone", "t2"]
    .map((id) => `<button data-block-references="${id}" type="button">1</button>`)
    .join("");
  row.innerHTML = `<div ${EDITOR_BOX_ATTRIBUTE}><div contenteditable="true"><p data-block-id="r0">Above.</p><section data-block-id="r1"><p data-block-id="p1">The statement.</p></section><p data-block-id="t2">Below.</p></div><aside>${buttons}</aside></div>`;
  document.body.append(row);
  try {
    const editable = row.querySelector('[contenteditable="true"]');
    if (editable === null) throw new Error("the stand-in draws no editable");
    const stops = nodeCaret.tabStopsFrom(row, "r1", editable, false) ?? [];
    expect(
      stops.flatMap((stop) => stop.getAttribute("data-block-references") ?? []),
      "the references buttons Tab stops at are not those of the node's block and the block in it"
    ).toEqual(["p1", "r1"]);
  } finally {
    row.remove();
  }
});

// The editor draws a mermaid block's "Mermaid source" toggle as a `summary` of its own
// `contenteditable="false"`, right inside the block's editable `details` (`@legion/proof-editor`'s
// `mermaid-code-block-view.ts`), and the browser stops at it for Tab, as it does at no link of the
// editable's text. A callout may hold a mermaid block, so the toggle of one in the node's block is
// a tab stop of the row's editor, while the link in the block's words is not, and neither is the
// toggle of a mermaid block beside it, another block's.
test("a mermaid block's source toggle inside the node's block is a tab stop of the row's editor, and one beside it is not", () => {
  const row = document.createElement("div");
  const mermaid = `<div class="proof-code-block proof-mermaid-block"><div class="proof-mermaid-preview" contenteditable="false" hidden></div><div class="proof-mermaid-status" contenteditable="false" hidden></div><details class="proof-mermaid-source" open><summary class="proof-mermaid-source-summary" contenteditable="false">Mermaid source</summary><pre class="proof-code-block-pre" data-language="mermaid"><code>graph TD</code></pre></details></div>`;
  row.innerHTML = `<div ${EDITOR_BOX_ATTRIBUTE}><div contenteditable="true"><p data-block-id="r0">Above.</p><section data-block-id="r1"><p>The statement, after <a href="#b-r0">the words above</a>.</p>${mermaid}</section><section data-block-id="t2">${mermaid}</section></div></div>`;
  document.body.append(row);
  try {
    const editable = row.querySelector('[contenteditable="true"]');
    if (editable === null) throw new Error("the stand-in draws no editable");
    const stops = nodeCaret.tabStopsFrom(row, "r1", editable, false) ?? [];
    /** Each stop inside the row, by its element and the block it stands in. */
    const named = stops
      .filter((stop) => row.contains(stop))
      .map((stop) => {
        const block = stop.closest("[data-block-id]")?.getAttribute("data-block-id");
        return `${stop.localName} in ${block ?? "no block"}`;
      });
    expect(
      named,
      "the stops Tab takes from the row's editor are not the source toggle of the node's block alone"
    ).toEqual(["summary in r1"]);
  } finally {
    row.remove();
  }
});

// Backspace and Delete at the start of a list's first item lift it out of its list
// (LiftFirstListItem), which the editor refuses (`holdEditsToBlock`) for a block in that item.
test("Backspace and Delete at the start of a list's first item leave the node's block in it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, [
    paragraph("r0", "The words above the statement."),
    {
      attrs: { blockId: "l" },
      content: [
        listItem("i1", paragraph("r1", "A statement of the spec.")),
        listItem("i2", paragraph("r2", "The item below the statement.")),
      ],
      type: "bullet_list",
    },
  ]);
  const before = blocksOf(view);
  for (const key of ["Backspace", "Delete"]) {
    await select(wordsOf(editable, "r1"), 0);
    pressIn(editable, key);
    expect(blocksOf(view), `${key} at the item's start lifted it`).toEqual(before);
  }
});

// A collaborator's edit applies wherever it lands, words above the node's block and a block put
// before it among them, and the block's place moves with it: the editor holds its own edits alone
// to the block (`holdEditsToBlock`), and holds them to the block where it now stands.
test("a collaborator's edit outside the node's block applies, and the block is held where it moved to", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, room, view } = await editLive(user);
  act(() => {
    room.server.transact(() => {
      const fragment = room.server.getXmlFragment("prosemirror");
      const above = fragment.get(0);
      const words = above instanceof Y.XmlElement ? above.get(0) : undefined;
      if (words instanceof Y.XmlText) words.insert(0, "New: ");
      const added = new Y.XmlElement("paragraph");
      added.setAttribute("blockId", "rx");
      added.insert(0, [new Y.XmlText("A collaborator's words.")]);
      fragment.insert(1, [added]);
    });
  });
  const moved = [
    "0 r0 paragraph: New: The words above the statement.",
    "0 rx paragraph: A collaborator's words.",
    "0 r1 paragraph: A statement of the spec.",
    "0 r2 paragraph: The words below the statement.",
  ];
  expect(blocksOf(view), "the collaborator's edit did not apply").toEqual(moved);
  await select(wordsOf(editable, "r1"), 0);
  pressIn(editable, "Backspace");
  expect(blocksOf(view), "Backspace joined the moved block to the block now before it").toEqual(
    moved
  );
});

/** The words above the statement, in ProseMirror's JSON, as the room holds them once `mark` is
 *  set over `words above`: the mark is a span of the text there, as a collaborator's editor
 *  writes one. */
function wordsAboveMarked(mark: {
  readonly attrs: Readonly<Record<string, unknown>>;
  readonly type: string;
}) {
  return {
    attrs: { blockId: "r0" },
    content: [
      { text: "The ", type: "text" },
      { marks: [mark], text: "words above", type: "text" },
      { text: " the statement.", type: "text" },
    ],
    type: "paragraph",
  };
}

/** The marks the row's `editable` draws in block `id`, each as its kind, its id and the words it
 *  is drawn over. */
function marksDrawn(editable: HTMLElement, id: string): string[] {
  return [
    ...editable.querySelectorAll<HTMLElement>(`[data-block-id="${id}"] [data-proof][data-id]`),
  ].map((span) => `${span.dataset.proof} ${span.dataset.id}: ${span.textContent}`);
}

// A mark set in the room - a collaborator's comment on the words above the statement - reaches the
// row's editor as its span, which y-prosemirror draws from the room, and as its record in the
// room's `marks` map, which the editor projects (`bindRemoteMarks`). Both are the collaboration's
// (`holdEditsToBlock`), so the mark is drawn outside the node's block while the row is edited.
test("a mark set in the room over the words above the statement is drawn in the row's editor while the row is edited", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, room, view } = await editLive(user);
  const before = blocksOf(view);
  const comment = { attrs: { by: "human:bob", id: "comment-1" }, type: "proofComment" };
  act(() => {
    room.server.transact(() => {
      prosemirrorJSONToYXmlFragment(
        view.state.schema,
        { content: [wordsAboveMarked(comment), ...liveBlocks().slice(1)], type: "doc" },
        room.server.getXmlFragment("prosemirror")
      );
      room.server.getMap("marks").set("comment-1", {
        by: "human:bob",
        createdAt: "2026-11-05T00:00:00.000Z",
        kind: "comment",
        quote: "words above",
      });
    });
  });
  expect(marksDrawn(editable, "r0"), "the mark set in the room was not drawn").toEqual([
    "comment comment-1: words above",
  ]);
  expect(blocksOf(view), "the mark changed the document's words").toEqual(before);
  expect(caretBlock(), "the mark took the caret out of the statement").toBe("r1");
});

// The one change of the text the editor's projection of the room's marks makes is to take out the
// span of a suggestion the room has accepted or rejected (`proof-sdk-upstream`'s
// `applyRemoteMarks`): a transaction of the editor's own making, marked as y-prosemirror marks
// the collaboration's, so it applies outside the node's block too.
test("the span of a suggestion the room has accepted goes from the words above the statement while the row is edited", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, room, view } = await editLive(user);
  const before = blocksOf(view);
  const suggested = {
    by: "human:bob",
    content: "lines over",
    createdAt: "2026-11-05T00:00:00.000Z",
    kind: "replace",
    quote: "words above",
  };
  act(() => {
    room.server.transact(() => {
      prosemirrorJSONToYXmlFragment(
        view.state.schema,
        {
          content: [
            wordsAboveMarked({
              attrs: { by: "human:bob", id: "suggestion-1", kind: "replace" },
              type: "proofSuggestion",
            }),
            ...liveBlocks().slice(1),
          ],
          type: "doc",
        },
        room.server.getXmlFragment("prosemirror")
      );
      room.server.getMap("marks").set("suggestion-1", { ...suggested, status: "pending" });
    });
  });
  expect(marksDrawn(editable, "r0"), "the suggestion set in the room was not drawn").toEqual([
    "suggestion suggestion-1: words above",
  ]);
  act(() => {
    room.server.getMap("marks").set("suggestion-1", { ...suggested, status: "accepted" });
  });
  expect(
    marksDrawn(editable, "r0"),
    "the span of the suggestion the room accepted stayed in the row's editor"
  ).toEqual([]);
  expect(blocksOf(view), "the suggestion's span going changed the document's words").toEqual(
    before
  );
});

// A selection moved out of the node's block once the reader has acted - a press on another block
// of the editor, a drag across blocks, a platform's own key for a move - comes back to the
// block's nearer end, as the editor's own in the same task: an end above the block to its start,
// an end below it to its end, and an end inside it stays.
test("a selection moved out of the node's block comes back to the block's nearer end, as the editor's own", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const restore = dropSelectionReports();
  const own: { block?: string | null; offset?: number } = {};
  try {
    const editable = await editStatement(user, <ReportReadingEditor own={own} />, BLOCKS_AROUND);
    const above = wordsOf(editable, "r0");
    const statement = wordsOf(editable, "r1");
    const below = wordsOf(editable, "r2");
    const end = statement.length;
    await user.keyboard("{Shift}");
    await select(above, 5);
    expect(selected(), "a caret above the block did not come to its start").toEqual([
      "r1:0",
      "r1:0",
    ]);
    expect([own.block, own.offset], "the editor's own caret is not at the block's start").toEqual([
      "r1",
      0,
    ]);
    await select(below, 5);
    expect(selected(), "a caret below the block did not come to its end").toEqual([
      `r1:${end}`,
      `r1:${end}`,
    ]);
    expect([own.block, own.offset], "the editor's own caret is not at the block's end").toEqual([
      "r1",
      end,
    ]);
    await select(above, 3, below, 4);
    expect(selected(), "a selection across the block is not the block's text").toEqual([
      "r1:0",
      `r1:${end}`,
    ]);
    await select(statement, 5, below, 4);
    expect(selected(), "a selection from inside the block did not stop at its end").toEqual([
      "r1:5",
      `r1:${end}`,
    ]);
  } finally {
    restore();
  }
});

// Until the node's block is the selection - before the caret is placed in it, with the focus
// Tabbed into the editable before the editor has drawn that block - the editor's own selection is
// wherever it was, and a key the editor answers lands there. No key that could edit reaches the
// editor then: every key is refused but those that move the focus, and the browser's own - a key
// held with Ctrl or Meta that the editor does not bind, a function key.
test("no key edits the document while the node's block of the row's editor is not the selection", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable } = await enterBeforeDrawn(user);
  // The editor draws the words above the statement, and not the statement.
  await drawStatementBlocks(editable, [["r0", "The words above the statement."]]);
  await user.tab();
  expect(document.activeElement, "Tab from the row did not stop in its editor").toBe(editable);
  await select(wordsOf(editable, "r0"), 5);
  for (const [key, modifiers, refused] of [
    ["x", {}, true],
    [" ", {}, true],
    ["Backspace", {}, true],
    ["Delete", {}, true],
    ["Enter", {}, true],
    ["ArrowUp", {}, true],
    ["Home", {}, true],
    ["a", { ctrlKey: true }, true],
    ["z", { ctrlKey: true }, true],
    ["v", { ctrlKey: true }, true],
    ["b", { metaKey: true }, true],
    ["1", { altKey: true, ctrlKey: true }, true],
    ["f", { ctrlKey: true }, false],
    ["p", { ctrlKey: true }, false],
    ["l", { metaKey: true }, false],
    ["F5", {}, false],
    ["Shift", {}, false],
    ["Tab", {}, false],
  ] as const) {
    const name = `${Object.keys(modifiers).join("+")} ${key}`;
    expect(!pressIn(editable, key, modifiers), `${name} refused: ${!refused}`).toBe(refused);
  }
});

// The editor's hold on its edits (`holdEditsToBlock`) is the caret's precondition. What the hold
// is built of loads apart from the editor: held back here, the editor is made, handed to the row
// and has drawn the statement, and no caret goes in - the focus stays on the row, where no key
// edits, and a key pressed in the editor is refused. The caret goes in once the hold is on, and
// the editor then holds its edits to the block.
test("the caret goes into the row's editor only once the editor holds its edits to the node's block", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const parts = await nodeCaret.loadEditsHold();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const room = liveRoom();
    const blocks = liveBlocks();
    const { editable } = await enterStatement(user, (onEditor) => (
      <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />
    ));
    const row = editable.closest<HTMLElement>("[data-node-row]");
    // The editor has drawn the statement, and the frames after that have run.
    await waitFor(() => wordsOf(editable, "r1"));
    await nextFrame();
    await nextFrame();
    expect(
      document.activeElement === row,
      "the focus left the row before the editor held its edits"
    ).toBe(true);
    expect(
      editable.contains(window.getSelection()?.anchorNode ?? null),
      "a caret went into an editor that does not hold its edits yet"
    ).toBe(false);
    await select(wordsOf(editable, "r1"), 3);
    expect(pressIn(editable, "x"), "a key reached an editor that does not hold its edits yet").toBe(
      false
    );

    await act(async () => held.resolve(parts));
    await waitFor(() =>
      expect(
        document.activeElement === editable && caretBlock() === "r1",
        "no caret went into the statement once the editor held its edits"
      ).toBe(true)
    );
    expect(selected(), "the caret is not at the statement's start").toEqual(["r1:0", "r1:0"]);
    const view = room.editor?.view;
    if (view === undefined) throw new Error("the row's editor was never made");
    const before = blocksOf(view);
    pressIn(editable, "Backspace");
    expect(blocksOf(view), "the editor that took the caret does not hold its edits").toEqual(
      before
    );
  } finally {
    load.mockRestore();
  }
});

// A change of the editor's plugins makes ProseMirror make every plugin's view again, and
// y-prosemirror's then draws the whole document again from the room. The tree lets go of its hold
// as it goes, on an editor the page is about to destroy, so a hold let go leaves its plugin on the
// editor (`holdEditsToBlock`), and the plugin refuses no edit from then, as no plugin did.
test("a hold let go changes none of its editor's plugins, and refuses no edit from then", async () => {
  const room = liveRoom();
  room.server.getXmlFragment("prosemirror").insert(0, liveBlocks().map(roomBlock));
  const root = document.createElement("div");
  document.body.append(root);
  const handle = await createEditor(root, {
    awareness: null,
    blockSchema: compiledBlockSchema,
    onAskBlockHosts: () => {},
    user: { color: "#f59e0b", name: "alice" },
    watchPicture: () => () => {},
    ydoc: room.browser,
  });
  const { view } = handle;
  // An `X` put at the start of block `id`, with the caret after it, as an edit of the editor's
  // own: answers the blocks after it.
  const typeAtStart = (id: string) => {
    let at = -1;
    view.state.doc.descendants((node, position) => {
      if (node.attrs.blockId === id) at = position + 1;
      return at < 0;
    });
    const typed = view.state.tr.insertText("X", at);
    view.dispatch(typed.setSelection(TextSelection.create(typed.doc, at + 1)));
    return blocksOf(view);
  };
  try {
    const held = Promise.withResolvers<void>();
    const letGo = nodeCaret.holdEditsToBlock(
      view,
      () => "r1",
      nodeCaret.loadEditsHold(),
      held.resolve
    );
    await held.promise;
    const before = blocksOf(view);
    expect(before, "the editor did not draw the document").toHaveLength(3);
    expect(typeAtStart("r0"), "the hold let in an edit outside its block").toEqual(before);

    let changes = 0;
    const updateState = view.updateState.bind(view);
    const updates = spyOn(view, "updateState").mockImplementation((next) => {
      if (next.plugins !== view.state.plugins) changes += 1;
      updateState(next);
    });
    try {
      letGo();
      expect(typeAtStart("r0"), "the hold let go refused an edit outside its block").toEqual(
        withWords(before, "r0", "XThe words above the statement.")
      );
    } finally {
      updates.mockRestore();
    }
    expect(changes, "letting go of the hold changed the editor's plugins").toBe(0);
  } finally {
    await handle.destroy();
    root.remove();
  }
});

// Until the editor's hold is on, no caret goes in, and a Tab from the row stops in the editable,
// where the editor's own selection is wherever it was - here in the second item of a list above
// the node's block. Milkdown sinks that item into the one before it for Tab and lifts it out of
// its list for Shift+Tab (SinkListItem, LiftListItem), an edit no hold refuses yet, and prevents
// the key's default, which holds the focus in the editor. The row keeps Tab from the editor and
// leaves it to the browser (`confineKey`), which moves the focus on with it.
for (const [name, modifiers] of [
  ["Tab", {}],
  ["Shift+Tab", { shiftKey: true }],
] as const) {
  test(`${name} in the row's editor before its hold is on moves no list item, and is left to the browser`, async () => {
    setViewport(1440);
    const user = userEvent.setup();
    const held = Promise.withResolvers<nodeCaret.HoldParts>();
    const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
    try {
      const room = liveRoom();
      const blocks: LiveBlock[] = [
        {
          attrs: { blockId: "l" },
          content: [
            listItem("i0", paragraph("q0", "The first item above the statement.")),
            listItem("i1", paragraph("q1", "The second item above the statement.")),
          ],
          type: "bullet_list",
        },
        paragraph("r1", "A statement of the spec."),
      ];
      const { editable } = await enterStatement(user, (onEditor) => (
        <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />
      ));
      await waitFor(() => wordsOf(editable, "r1"));
      await nextFrame();
      const view = room.editor?.view;
      if (view === undefined) throw new Error("the row's editor was never made");
      await user.tab();
      expect(document.activeElement, "Tab from the row did not stop in its editor").toBe(editable);
      await select(wordsOf(editable, "q1"), 3);
      expect(
        view.state.selection.$head.parent.textContent,
        "the editor's own caret is not in the list's second item"
      ).toBe("The second item above the statement.");
      const before = blocksOf(view);
      const left = pressIn(editable, "Tab", modifiers);
      expect(blocksOf(view), `${name} moved the list item`).toEqual(before);
      expect(left, `${name} was refused, which holds the focus in the editor`).toBe(true);
    } finally {
      load.mockRestore();
    }
  });
}

/** Fails the load of what the hold is built of, held back so far, with `reject`, and waits a
 *  task on: a rejection nothing handled is reported then, and fails the case it happens in. */
function failLoad(reject: (reason: Error) => void): Promise<void> {
  return waitInAct(20, () => reject(new Error("the hold's parts did not load")));
}

// What the hold is built of can fail to load - a chunk the network did not bring. No hold goes on
// then, so no caret goes in: the editing ends, the row says its editor could not open, and the
// focus is on the row. The failure is answered by the hold the editor was handed to and by the
// row that asked for the load, and the next Enter loads again.
test("a row whose editor's hold fails to load ends the editing and says its editor could not open", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const [row] = rowsFor(view.container, "spec#b-r1");
    await drawStatementBlocks(editable);
    await nextFrame();
    expect(view.edited.at(-1), "the editing ended while the hold was still loading").toBe(
      "artifact-spec"
    );
    expect(
      document.activeElement === row,
      "the focus left the row while the hold was still loading"
    ).toBe(true);

    await failLoad(held.reject);
    await waitFor(() =>
      expect(
        rowReport(view.container),
        "the row whose hold failed to load did not say its editor could not open"
      ).toBe(KEY_REPORTS.editor_closed)
    );
    expect(
      row?.querySelector("[data-row-report]") != null,
      "the report is not at the statement's row"
    ).toBe(true);
    expect(view.edited.at(-1), "the editing did not end").toBeUndefined();
    await waitFor(() =>
      expect(
        row?.querySelector('[contenteditable="true"]') == null,
        "the row still holds the editor"
      ).toBe(true)
    );
    expect(document.activeElement === row, "the focus is not on the statement's row").toBe(true);

    // The next Enter asks for the load again, which now answers.
    load.mockRestore();
    await user.keyboard("{Enter}");
    const again = await waitFor(() => {
      const found = row?.querySelector<HTMLElement>('[contenteditable="true"]');
      if (found == null) throw new Error("the second Enter put no editor in the row");
      return found;
    });
    await drawStatementBlocks(again);
    await waitFor(() =>
      expect(
        document.activeElement === again && caretBlock() === "r1",
        "the second Enter put no caret in the statement"
      ).toBe(true)
    );
  } finally {
    load.mockRestore();
  }
});

// The load can fail before the page has handed the row an editor, or for a page that hands it
// none: the row alone has asked for it then, and answers for it all the same.
test("a hold that fails to load before the row is handed an editor ends the editing too", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { view } = await enterBeforeDrawn(user, () => (
      <div contentEditable suppressContentEditableWarning />
    ));
    const [row] = rowsFor(view.container, "spec#b-r1");
    await failLoad(held.reject);
    await waitFor(() =>
      expect(
        rowReport(view.container),
        "the row whose hold failed to load did not say its editor could not open"
      ).toBe(KEY_REPORTS.editor_closed)
    );
    expect(view.edited.at(-1), "the editing did not end").toBeUndefined();
    expect(document.activeElement === row, "the focus is not on the statement's row").toBe(true);
  } finally {
    load.mockRestore();
  }
});

// Enter on the row already being edited before its editor's hold is on starts nothing new: the
// first Enter's look for the caret goes on, and the second Enter is an act of the reader's in it.
// A new look started at the second Enter counted the reader as not having acted yet, so a focus
// moved off the row after it - a script's - left the editing on, and the caret was to go in once
// the hold was on, taking the focus back. A move off the row once the reader has acted ends the
// editing, and so it does after the second Enter.
test("a second Enter on the row while its editor's hold is loading starts no new look for the caret: a focus moved off the row after it ends the editing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const { control } = controlOutside();
    const [row] = rowsFor(view.container, "spec#b-r1");
    await drawStatementBlocks(editable);
    await nextFrame();
    expect(
      document.activeElement === row,
      "the focus left the row while the hold was still loading"
    ).toBe(true);

    await user.keyboard("{Enter}");
    expect(view.edited.at(-1), "the second Enter ended the editing").toBe("artifact-spec");
    await act(async () => control.focus());
    await waitFor(() =>
      expect(
        view.edited.at(-1),
        "the focus moved off the row after the second Enter left the editing on"
      ).toBeUndefined()
    );
    expect(document.activeElement, "the focus left the control it was moved to").toBe(control);
  } finally {
    load.mockRestore();
  }
});

// The tester's finding at #4205's 46dab3c7, ruled by the architect under decision 348 (2): a
// press in Details leaves the editing as it is, and that holds between Enter and the caret's
// placement too. At 46dab3c7 the row's look for the caret ends the editing on any act of the
// reader's that takes the focus off the row while the editor's hold is loading, a press in
// Details included, so the document's editor and the threads Details draws beside it go off the
// page under the pointer. The press keeps the editing and cancels the caret's placement: once the
// hold is on, no caret goes in and the focus stays in Details.
test("a press in Details while the row's editor is still loading keeps the editing, and no caret takes the focus back from Details", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const parts = await nodeCaret.loadEditsHold();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const room = liveRoom();
    const blocks = liveBlocks();
    const { editable, view } = await enterStatement(user, (onEditor) => (
      <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />
    ));
    const row = editable.closest<HTMLElement>("[data-node-row]");
    await waitFor(() => wordsOf(editable, "r1"));
    await nextFrame();
    await nextFrame();
    expect(
      document.activeElement === row,
      "the focus left the row before the editor held its edits"
    ).toBe(true);
    const card = cardInDetails();

    await user.click(card);
    // The press is over a task after its click.
    await waitInAct(0);
    expect(
      view.edited.at(-1),
      "a press in Details while the editor was loading ended the editing"
    ).toBe("artifact-spec");
    expect(document.activeElement === card, "the press left the focus off the card").toBe(true);

    await act(async () => held.resolve(parts));
    await nextFrame();
    await nextFrame();
    expect(
      document.activeElement === card,
      "the caret took the focus back from Details once the editor held its edits"
    ).toBe(true);
  } finally {
    load.mockRestore();
  }
});

// The class search of #4205's 2fffbc34: what the editor's hold is built of failing to load
// while a press in Details had put off the row's caret left the editing on, with an editor no
// caret could ever go in, until the reader came back to the row. The editing ends as soon as the
// failure is seen, as it does with no caret put off, and the focus stays in Details, where the
// reader is. The row says nothing then: the tree lets go of it, and of what was reported at it,
// as the editing ends with the focus out of the tree (`useReleaseFocus`).
test("a hold that fails to load while a press in Details has put off the row's caret ends the editing, and leaves the focus in Details", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const [row] = rowsFor(view.container, "spec#b-r1");
    if (row === undefined) throw new Error("the statement's row is not drawn");
    await drawStatementBlocks(editable);
    await nextFrame();
    const card = cardInDetails();
    await user.click(card);
    // The press is over a task after its click.
    await waitInAct(0);
    expect(
      view.edited.at(-1),
      "a press in Details while the editor's hold was loading ended the editing"
    ).toBe("artifact-spec");
    expect(document.activeElement === card, "the press left the focus off the card").toBe(true);

    await failLoad(held.reject);
    await waitFor(() =>
      expect(
        view.edited.at(-1),
        "the editing went on once the hold failed to load while the caret was put off"
      ).toBeUndefined()
    );
    await waitFor(() =>
      expect(
        row.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`),
        "the row still draws its editor's box"
      ).toBeNull()
    );
    await nextFrame();
    expect(document.activeElement === card, "the editing's end took the focus from Details").toBe(
      true
    );
    expect(
      rowReport(view.container),
      "a row says its editor could not open, with the focus out of the tree"
    ).toBeUndefined();
    expect(
      editable.contains(window.getSelection()?.anchorNode ?? null),
      "a caret went into the editor whose hold failed to load"
    ).toBe(false);
  } finally {
    load.mockRestore();
  }
});

// A page that draws the row's editor and hands the row nothing to hold would leave that editor
// unguarded. The row waits while the editor draws no block of the node's, and a task after the
// block is drawn with no editor handed over - a page hands its editor over in the task the
// editor is made in - the editing ends with no caret placed, and the row says so.
test("a page that hands the row no editor gets no caret: the editing ends and the row says its editor could not open", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user, () => (
    <div contentEditable suppressContentEditableWarning />
  ));
  const [row] = rowsFor(view.container, "spec#b-r1");
  expect(view.edited.at(-1), "the editing ended before the editor had drawn the node's block").toBe(
    "artifact-spec"
  );
  await drawStatementBlocks(editable);
  await waitFor(() =>
    expect(
      rowReport(view.container),
      "the row handed no editor did not say its editor could not open"
    ).toBe(KEY_REPORTS.editor_closed)
  );
  expect(view.edited.at(-1), "the editing did not end").toBeUndefined();
  expect(
    editable.contains(window.getSelection()?.anchorNode ?? null),
    "a caret went into an editor the row was never handed"
  ).toBe(false);
  expect(document.activeElement === row, "the focus is not on the statement's row").toBe(true);
});

// The same for a page that hands the row no editor, which is seen a task after the editor has
// drawn the node's block: drawn after a press in Details put off the caret, that block went
// unseen, and the editing stayed on with an editor no hold could go on. It ends as soon as that
// is seen, with the focus left in Details and the row saying nothing, as above.
test("a page that hands the row no editor, drawing the node's block after a press in Details put off the caret, ends the editing and leaves the focus in Details", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await enterBeforeDrawn(user, () => (
    <div contentEditable suppressContentEditableWarning />
  ));
  const [row] = rowsFor(view.container, "spec#b-r1");
  if (row === undefined) throw new Error("the statement's row is not drawn");
  const card = cardInDetails();
  await user.click(card);
  // The press is over a task after its click.
  await waitInAct(0);
  expect(view.edited.at(-1), "a press in Details ended the editing").toBe("artifact-spec");

  await drawStatementBlocks(editable);
  await waitFor(() =>
    expect(
      view.edited.at(-1),
      "the editing went on with an editor the page never handed over while the caret was put off"
    ).toBeUndefined()
  );
  await waitFor(() =>
    expect(
      row.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`),
      "the row still draws its editor's box"
    ).toBeNull()
  );
  await nextFrame();
  expect(document.activeElement === card, "the editing's end took the focus from Details").toBe(
    true
  );
  expect(
    rowReport(view.container),
    "a row says its editor could not open, with the focus out of the tree"
  ).toBeUndefined();
  expect(
    editable.contains(window.getSelection()?.anchorNode ?? null),
    "a caret went into an editor the row was never handed"
  ).toBe(false);
});

// The class search of #4205's 7a922b76: a press in Details put off the row's caret, and a press
// then went down on another row of the tree, which ends the editing once it is over (the press
// rule); what the editor's hold is built of failed to load while that press was held. Once the
// press was over, the look for the caret ended the editing as a hold that cannot go on, and the
// row the press had focused said that its editor could not open, of an editor it never asked
// for. That press ends the editing as the reader moving on, as it does with no failure, and the
// row it focused says nothing.
test("a hold that fails to load while a press on another row is held, after a press in Details put off the caret, ends the editing with that press, and the row pressed says nothing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    const words = rowsFor(view.container, "comment-2")[0]?.querySelector<HTMLElement>(
      "[data-node-words]"
    );
    if (words == null) throw new Error("the last comment's row is not drawn");
    await drawStatementBlocks(editable);
    await nextFrame();
    const card = cardInDetails();
    await user.click(card);
    // The press is over a task after its click.
    await waitInAct(0);
    expect(
      view.edited.at(-1),
      "a press in Details while the editor's hold was loading ended the editing"
    ).toBe("artifact-spec");

    await user.pointer({ keys: "[MouseLeft>]", target: words });
    expect(focusedRow(view.container), "the press did not focus the row it went down on").toBe(
      "comment-2"
    );
    await failLoad(held.reject);
    expect(view.edited.at(-1), "the editing ended while the press was held").toBe("artifact-spec");
    await user.pointer({ keys: "[/MouseLeft]", target: words });
    // The press is over a task after its click.
    await waitInAct(0);
    await waitFor(() =>
      expect(view.edited.at(-1), "the editing did not end once the press was over").toBeUndefined()
    );
    await nextFrame();
    expect(
      rowReport(view.container),
      "the row the press focused says its editor could not open"
    ).toBeUndefined();
    expect(focusedRow(view.container), "the focus left the row pressed").toBe("comment-2");
  } finally {
    load.mockRestore();
  }
});

// The class search of #4205's 7a922b76: a press outside the tree on something that takes no
// focus - words, blank space - leaves the focus on the page's body and puts off the row's caret,
// and what the editor's hold is built of then failed to load. The editing ended as Escape's does,
// whose next frame gives the row the focus where it has fallen to nothing (`leaving`): the row
// took the focus from the page's body, where the reader had left it, scrolling the page to it,
// and said nothing there, since the tree had let go of what was reported at it. The end of an
// editing whose caret is put off takes no focus, and with the focus out of the tree the row says
// nothing as it ends: at 6586a4be the report it said then stood at the tree's tab stop for one
// commit, until the tree let go of the row, so each report drawn is watched for.
test("a hold that fails to load after a press on words outside the tree put off the row's caret leaves the focus on the page's body", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const held = Promise.withResolvers<nodeCaret.HoldParts>();
  const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
  const text = "Words outside the outline";
  const outside = render(<p>{text}</p>).getByText(text);
  try {
    const { editable, view } = await enterBeforeDrawn(user);
    await drawStatementBlocks(editable);
    await nextFrame();
    await user.click(outside);
    // The press is over a task after its click.
    await waitInAct(0);
    expect(
      view.edited.at(-1),
      "a press outside the tree while the editor's hold was loading ended the editing"
    ).toBe("artifact-spec");
    expect(document.activeElement, "the press left the focus on an element").toBe(document.body);

    const reports = reportsDrawn(view.container);
    await failLoad(held.reject);
    await waitFor(() =>
      expect(
        view.edited.at(-1),
        "the editing went on once the hold failed to load while the caret was put off"
      ).toBeUndefined()
    );
    await nextFrame();
    await nextFrame();
    expect(
      focusedRow(view.container),
      "the editing's end took the focus from the page's body to a row"
    ).toBeUndefined();
    expect(document.activeElement, "the editing's end took the focus from the page's body").toBe(
      document.body
    );
    expect(
      rowReport(view.container),
      "a row says its editor could not open, with the focus out of the tree"
    ).toBeUndefined();
    expect(
      reports(),
      "a row said its editor could not open as the editing ended, with the focus out of the tree"
    ).toEqual([]);
  } finally {
    load.mockRestore();
  }
});

// The class search of #4205's a0ba8427: the row said its editor could not open wherever the focus
// was as an editing with no caret put off ended, so a hold that failed to load after a script had
// moved the focus out of the tree before the reader acted - a blur, a focus on a control out of
// the outline, which leaves the editing on - drew that report at the tree's tab stop for one
// commit, until the tree let go of the row, and of what was reported at it (`useReleaseFocus`).
// The row says it only where the focus is the row's or inside the tree: the editing ends, and no
// report is drawn at all.
for (const blur of [true, false]) {
  const name = blur ? "blur" : "focus on a control out of the outline";
  test(`a hold that fails to load after a script's ${name} took the focus out of the tree before the reader acted ends the editing, and no row says its editor could not open`, async () => {
    setViewport(1440);
    const user = userEvent.setup();
    const held = Promise.withResolvers<nodeCaret.HoldParts>();
    const load = spyOn(nodeCaret, "loadEditsHold").mockReturnValue(held.promise);
    try {
      const { editable, view } = await enterBeforeDrawn(user);
      const { control } = controlOutside();
      const [row] = rowsFor(view.container, "spec#b-r1");
      if (row === undefined) throw new Error("the statement's row is not drawn");
      await drawStatementBlocks(editable);
      await nextFrame();
      await act(async () => (blur ? row.blur() : control.focus()));
      expect(
        view.container.contains(document.activeElement),
        `the script's ${name} left the focus in the tree`
      ).toBe(false);
      // The row is looked at a task after the focus left it.
      await waitInAct(0);
      expect(view.edited.at(-1), `the script's ${name} ended the editing`).toBe("artifact-spec");

      const reports = reportsDrawn(view.container);
      await failLoad(held.reject);
      await waitFor(() =>
        expect(
          view.edited.at(-1),
          "the editing went on once the hold failed to load"
        ).toBeUndefined()
      );
      await nextFrame();
      await nextFrame();
      expect(
        reports(),
        "a row said its editor could not open as the editing ended, with the focus out of the tree"
      ).toEqual([]);
    } finally {
      load.mockRestore();
    }
  });
}

// The document's editor draws a document the room already holds as it is made, some microtasks
// before the page hands it to the row (`doc/ProofDocument.tsx`'s `onEditorHandle`), and a look at
// the row falls between the two: the row's own watch on its editor's drawing. That look read the
// editor as one the page would never hand over, and ended the editing. The row reads that a task
// later, so the editor handed over in the task it drew the block in takes the caret.
test("an editor that has drawn the node's block microtasks before the page hands it over takes the caret", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  let handOver: OnEditor | undefined;
  const { editable, view } = await enterBeforeDrawn(user, (onEditor) => {
    handOver = onEditor;
    return <div contentEditable suppressContentEditableWarning />;
  });
  const handed = standInView();
  try {
    await act(async () => {
      appendBlocks(editable);
      // The row's watch on the drawing looks at the row here, before the editor is handed over.
      await Promise.resolve();
      await Promise.resolve();
      handOver?.({ view: handed });
    });
    await waitFor(() =>
      expect(
        document.activeElement === editable && caretBlock() === "r1",
        "the editor handed over in the task it drew the block in took no caret"
      ).toBe(true)
    );
    // The task after, in which the row reads whether it was handed an editor.
    await waitInAct(20);
    expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
    expect(rowReport(view.container), "the row said its editor could not open").toBeUndefined();
  } finally {
    handOver?.(undefined);
    handed.destroy();
  }
});

/** What takes a `RemadeStandIn` out of the page: `again` makes it again at once, and `gone`
 *  leaves the row with none until `again`. */
interface Remake {
  again?: () => void;
  gone?: () => void;
}

/** A plain editable standing in for the document's editor, which `remake` takes out of the page
 *  and makes again, as the document makes its editor again for a room that refused its
 *  connection and then admitted it. */
function RemadeStandIn({ remake }: { remake: Remake }): ReactNode {
  const [made, setMade] = useState({ count: 0, drawn: true });
  remake.again = () => setMade(({ count }) => ({ count: count + 1, drawn: true }));
  remake.gone = () => setMade(({ count }) => ({ count, drawn: false }));
  return made.drawn ? (
    <div contentEditable key={made.count} suppressContentEditableWarning />
  ) : null;
}

// Until the reader has acted, the row puts the caret back in the node's block as the editor
// redraws it. The document can make its editor again while the row is edited, and hands the row
// the new one: no caret goes back in until that editor's own hold is on.
test("an editor made again while the row is edited takes the caret back only once it holds its edits", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  let handOver: OnEditor | undefined;
  const remake: Remake = {};
  const { editable, view } = await enterBeforeDrawn(user, (onEditor) => {
    handOver = onEditor;
    return <RemadeStandIn remake={remake} />;
  });
  const [row] = rowsFor(view.container, "spec#b-r1");
  const first = standInView();
  const second = standInView();
  const caretIn = (editor: HTMLElement) =>
    document.activeElement === editor && caretBlock() === "r1";
  try {
    await act(async () => handOver?.({ view: first }));
    await drawStatementBlocks(editable);
    await waitFor(() =>
      expect(caretIn(editable), "Enter put no caret in the statement").toBe(true)
    );

    // The document tears its editor down and makes it again, which draws the statement before
    // the row is handed it.
    await act(async () => {
      handOver?.(undefined);
      remake.again?.();
    });
    const again = row?.querySelector<HTMLElement>('[contenteditable="true"]');
    if (again == null || again === editable) throw new Error("the editor was not made again");
    await drawStatementBlocks(again);
    await nextFrame();
    expect(caretIn(again), "a caret went into an editor that does not hold its edits yet").toBe(
      false
    );

    await act(async () => handOver?.({ view: second }));
    await waitFor(() =>
      expect(caretIn(again), "the editor made again took no caret once it held its edits").toBe(
        true
      )
    );
  } finally {
    handOver?.(undefined);
    first.destroy();
    second.destroy();
  }
});

// The document makes an editor for each connection to the room, and the room can refuse a
// connection whose editor is already being made: that editor draws the statement as it is made
// and is destroyed in the same task, handed to nothing (`doc/ProofDocument.tsx`), and the next
// connection's editor comes only after that connection has synced. The row read the first as an
// editor the page would never hand over, and ended the editing. Where the block is no longer
// drawn a task later the wait starts again, so the next editor, handed over as it draws the
// block, takes the caret.
test("an editor that draws the node's block and goes unhanded leaves the row waiting for the next one, which takes the caret", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  let handOver: OnEditor | undefined;
  const remake: Remake = {};
  const { editable, view } = await enterBeforeDrawn(user, (onEditor) => {
    handOver = onEditor;
    return <RemadeStandIn remake={remake} />;
  });
  const [row] = rowsFor(view.container, "spec#b-r1");
  const handed = standInView();
  // More than a task on: the row has read by then whether it was handed an editor.
  const aTaskOn = () => waitInAct(20);
  try {
    // The refused connection's editor draws the statement, the row's watch on the drawing looks
    // at the row, and the editor goes, all in one task.
    appendBlocks(editable);
    await Promise.resolve();
    await Promise.resolve();
    act(() => remake.gone?.());
    expect(
      row?.querySelector('[contenteditable="true"]') == null,
      "the editor that went unhanded is still in the row"
    ).toBe(true);
    await aTaskOn();
    expect(
      rowReport(view.container),
      "the row said its editor could not open, of an editor that drew the block and went"
    ).toBeUndefined();
    expect(view.edited.at(-1), "the editing ended with the next editor still to come").toBe(
      "artifact-spec"
    );

    // The next connection's editor is made, draws the statement and is handed over.
    await act(async () => remake.again?.());
    const next = row?.querySelector<HTMLElement>('[contenteditable="true"]');
    if (next == null || next === editable) throw new Error("the next editor was not made");
    await act(async () => {
      appendBlocks(next);
      await Promise.resolve();
      await Promise.resolve();
      handOver?.({ view: handed });
    });
    await waitFor(() =>
      expect(
        document.activeElement === next && caretBlock() === "r1",
        "the next editor, handed over, took no caret"
      ).toBe(true)
    );
    await aTaskOn();
    expect(rowReport(view.container), "the row said its editor could not open").toBeUndefined();
    expect(view.edited.at(-1), "the editing ended").toBe("artifact-spec");
  } finally {
    handOver?.(undefined);
    handed.destroy();
  }
});

/** Makes `change` to the page inside the row's editor, as the browser makes an edit there, and
 *  waits for the editor to read the change back (its `MutationObserver`). */
async function changePage(change: () => void): Promise<void> {
  await waitInAct(50, change);
}

// While the editor's document holds no block of the node's - before the room has sent it - no
// edit of the editor's own goes in anywhere: a change the browser makes to the page is put back
// (`holdEditsToBlock`). A collaborator's edit still applies; once it brings the node's block, the
// caret goes in, and an edit inside the block goes in too.
test("no edit of the editor's own goes in while its document holds no block of the node's", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const room = liveRoom();
  // Drawn as one list, so the editor is made once whatever draws the row again.
  const blocks = [paragraph("r0", "The words above the statement.")];
  const { editable } = await enterStatement(user, (onEditor) => (
    <LiveEditor blocks={blocks} onEditor={onEditor} room={room} />
  ));
  const view = await waitFor(() => {
    const drawn = room.editor?.view;
    if (drawn === undefined) throw new Error("the row's editor was never made");
    wordsOf(drawn.dom, "r0");
    return drawn;
  });
  const before = blocksOf(view);
  await changePage(() => {
    wordsOf(editable, "r0").data = "The words above the statement, edited.";
  });
  expect(blocksOf(view), "a change to the page went into the document").toEqual(before);
  act(() => {
    room.server.transact(() => {
      const statement = new Y.XmlElement("paragraph");
      statement.setAttribute("blockId", "r1");
      statement.insert(0, [new Y.XmlText("A statement of the spec.")]);
      room.server.getXmlFragment("prosemirror").insert(1, [statement]);
    });
  });
  await waitFor(() => expect(caretBlock(), "the caret never went into the statement").toBe("r1"));
  typeInEditor(view, "Y");
  expect(blocksOf(view), "a letter typed in the statement did not go in").toEqual([
    ...before,
    "0 r1 paragraph: YA statement of the spec.",
  ]);
});

// The editor leaves typed text, and a key its keymap does not answer, to the browser, which edits
// the page and has the editor read the change back. A change outside the node's block - a
// platform's own delete at the block's edge - is refused (`holdEditsToBlock`), and the editor
// draws the page again from its document; a change inside the block goes in.
test("an edit the browser makes in the row's editor past the node's block is put back", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  await select(wordsOf(editable, "r1"), 5);
  await changePage(() => {
    wordsOf(editable, "r0").data = "The words above the";
  });
  expect(blocksOf(view), "a delete in the words above went into the document").toEqual(before);
  expect(wordsOf(editable, "r0").data, "the page still shows the delete the editor refused").toBe(
    "The words above the statement."
  );
  await changePage(() => {
    wordsOf(editable, "r1").data = "A statement of the spec, edited.";
  });
  expect(blocksOf(view), "an edit of the statement's words did not go in").toEqual(
    withWords(before, "r1", "A statement of the spec, edited.")
  );
});

/** Pastes `text`, and `html` where given, into the row's `editable` as a browser sends a paste. */
function pasteIn(editable: HTMLElement, text: string, html?: string): void {
  const clipboardData = new DataTransfer();
  clipboardData.setData("text/plain", text);
  if (html !== undefined) clipboardData.setData("text/html", html);
  act(() => {
    editable.dispatchEvent(
      new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData })
    );
  });
}

// The editor reads a pasted text as markdown (`@milkdown/plugin-clipboard`), and pasted HTML as
// blocks of its own: two paragraphs pasted into a paragraph make a block after it, and so does a
// heading pasted as HTML. The editor refuses each there (`holdEditsToBlock`). Words pasted inside
// the block go in, and so do two lines, which the paragraph holds as one.
test("a paste into the row's paragraph is refused where it would make blocks beside it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  await select(wordsOf(editable, "r1"), 5);
  pasteIn(editable, "Two\n\nparagraphs");
  expect(blocksOf(view), "two paragraphs pasted made a block").toEqual(before);
  pasteIn(editable, "A heading\n\nwords", "<h2>A heading</h2><p>words</p>");
  expect(blocksOf(view), "a heading pasted made a block").toEqual(before);
  pasteIn(editable, "pasted");
  expect(blocksOf(view), "words pasted inside the block did not go in").toEqual(
    withWords(before, "r1", "A stapastedtement of the spec.")
  );
  pasteIn(editable, "Two\nlines");
  expect(blocksOf(view), "two lines pasted inside the paragraph did not go in").toEqual(
    withWords(before, "r1", "A stapastedTwo\nlinestement of the spec.")
  );
});

// Two lines pasted into a heading put a line break in it, which stores as a heading of two lines:
// measured in Chromium, two lines pasted at a decision's `## ` heading's start stored it as `Two\`,
// `lines...` and `---` under them. The editor refuses it there (`holdEditsToBlock`); one line
// pasted goes in.
test("a paste of two lines into the row's heading is refused, and one line goes in", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user, liveBlocks(heading("r1", 2, "A heading")));
  const before = blocksOf(view);
  await select(wordsOf(editable, "r1"), 0);
  pasteIn(editable, "Two\nlines");
  expect(blocksOf(view), "two lines pasted at the heading's start went in").toEqual(before);
  pasteIn(editable, "Pasted");
  expect(blocksOf(view), "one line pasted at the heading's start did not go in").toEqual(
    withWords(before, "r1", "PastedA heading")
  );
});

// A drop edits the document where it lands, in whichever block that is, and words dragged out of
// the node's block within the editor are moved, not copied. The editor refuses a drop outside the
// node's block, and words dragged out of it to another (`holdEditsToBlock`). happy-dom lays
// nothing out, so the place each event points at is the editor's reading of it (`posAtCoords`).
test("a drop into the row's editor outside the node's block, and a drag of its words out of it, leave the document as it was", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  const inBlock = (id: string, offset: number) => {
    let start = -1;
    view.state.doc.descendants((node, position) => {
      if (node.attrs.blockId === id) start = position + 1;
      return start < 0;
    });
    return start + offset;
  };
  let pointed = inBlock("r0", 4);
  const points = spyOn(view, "posAtCoords").mockImplementation(() => ({
    inside: -1,
    pos: pointed,
  }));
  const drag = (type: string, id: string, dataTransfer: DataTransfer) => {
    const event = new MouseEvent(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, "dataTransfer", { value: dataTransfer });
    act(() => {
      editable.querySelector(`[data-block-id="${id}"]`)?.dispatchEvent(event);
    });
  };
  try {
    const outside = new DataTransfer();
    outside.setData("text/plain", "Dropped words");
    drag("drop", "r0", outside);
    expect(blocksOf(view), "a drop on the words above went in").toEqual(before);
    await select(wordsOf(editable, "r1"), 0, wordsOf(editable, "r1"), 5);
    pointed = inBlock("r1", 2);
    const dragged = new DataTransfer();
    drag("dragstart", "r1", dragged);
    pointed = inBlock("r0", 4);
    drag("drop", "r0", dragged);
    expect(blocksOf(view), "words dragged out of the statement moved").toEqual(before);
    // The editor's drop indicator hides itself 30 ms after a drop, which it is let do while the
    // editor stands.
    await waitInAct(50);
  } finally {
    points.mockRestore();
  }
});

// On a Mac the editor takes Ctrl+H for Backspace, and Ctrl+D and Alt+D for Delete
// (`prosemirror-commands`' `macBaseKeymap`), so each joined the node's block to the block beside
// it at the block's start and its end. The editor reads its keymap once, as it loads, so the
// keymap's own commands stand in for the keys here: each is refused at those edges
// (`holdEditsToBlock`), and inside the block leaves the key to the browser, as Backspace does.
test("on a Mac, Ctrl+H, Ctrl+D and Alt+D in a row's editor join nothing to the block beside it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { editable, view } = await editLive(user);
  const before = blocksOf(view);
  const end = wordsOf(editable, "r1").length;
  const runAt = async (at: number, binding: string) => {
    await select(wordsOf(editable, "r1"), at);
    let taken: boolean | undefined;
    act(() => {
      taken = macBaseKeymap[binding]?.(view.state, view.dispatch, view);
    });
    return taken;
  };
  for (const [at, binding] of [
    [0, "Ctrl-h"],
    [end, "Ctrl-d"],
    [end, "Alt-d"],
  ] as const) {
    await runAt(at, binding);
    expect(blocksOf(view), `${binding} at ${at} changed the document`).toEqual(before);
  }
  for (const binding of ["Ctrl-h", "Ctrl-d"]) {
    expect(await runAt(5, binding), `${binding} inside the block was not the browser's`).toBe(
      false
    );
  }
});

// Escape gives the row the focus at once, and again in the next frame, once the editor has left
// the row and dropped the focus to the page: the frame gives it back to the row. A key the reader
// presses before that frame - one that moves the focus, or a second Escape, which leaves the
// outline - a press on blank space, or a focus a script gives a control, is where the focus
// stays: the frame took the focus back to the row all the same. The frames are held, so each
// comes before the frame.
test("the frame after Escape gives the row back a focus the editor dropped, and leaves it where the reader or a script put it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { control } = controlOutside();
  // Something outside the outline that takes no focus.
  const outside = "Words outside the outline";
  const plain = render(<p>{outside}</p>).getByText(outside);
  const editable = await editStatement(user);
  const statement = editable.closest<HTMLElement>("[data-node-row]");
  const tree = statement?.closest<HTMLElement>('[role="tree"]');
  if (statement === null || tree === null || tree === undefined) {
    throw new Error("the editor is in no row of a tree");
  }
  // Enter on the statement's row, then Escape from its editor.
  const editThenEscape = async () => {
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        statement.querySelector('[contenteditable="true"]'),
        "Enter put no editor in the row"
      ).toBeTruthy()
    );
    await user.keyboard("{Escape}");
    expect(document.activeElement, "Escape left the focus off the row").toBe(statement);
  };
  const frames = framesByHand();
  try {
    // A key: `k` moves the focus to the row above.
    await user.keyboard("{Escape}");
    expect(document.activeElement, "Escape left the focus off the row").toBe(statement);
    await user.keyboard("k");
    expect(focusedRow(tree)).toBe("comment-1");
    await frames.run();
    expect(
      focusedRow(tree),
      "the frame after Escape took the focus back from the row k moved it to"
    ).toBe("comment-1");

    // A script's focus on a control, with no key pressed.
    await user.keyboard("j");
    await editThenEscape();
    await act(async () => control.focus());
    await frames.run();
    expect(
      document.activeElement,
      "the frame after Escape took the focus back from the control a script focused"
    ).toBe(control);

    // A second Escape, which leaves the outline from the row: the focus is on the page.
    await act(async () => statement.focus());
    await editThenEscape();
    await user.keyboard("{Escape}");
    expect(document.activeElement, "the second Escape did not leave the outline").toBe(
      document.body
    );
    await frames.run();
    expect(
      document.activeElement,
      "the frame after Escape took the focus back from where a second Escape left it"
    ).toBe(document.body);

    // A press on blank space, which takes the focus to the page.
    await act(async () => statement.focus());
    await editThenEscape();
    await user.pointer({ keys: "[MouseLeft>]", target: plain });
    await user.pointer({ keys: "[/MouseLeft]" });
    expect(document.activeElement, "the press did not take the focus off the row").toBe(
      document.body
    );
    await frames.run();
    expect(
      document.activeElement,
      "the frame after Escape took the focus back from where a press on blank space left it"
    ).toBe(document.body);

    // The editor leaving the row drops the focus to the page, which is no act of the reader's.
    await act(async () => statement.focus());
    await editThenEscape();
    await act(async () => statement.blur());
    await frames.run();
    expect(
      document.activeElement,
      "the frame after Escape left the focus the editor dropped on the page"
    ).toBe(statement);
  } finally {
    frames.restore();
  }
});

// R11: below 1024 px the outline is no longer read one node at a time; `q` reaches every open
// question there as at every width.
test("q reaches every open question below 1024 px", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "ask-1", question: true, text: "Which flag?", type: "ask" })],
        id: "spec",
        type: "document",
      }),
      node({
        children: [node({ id: "ask-2", question: true, text: "Who ships it?", type: "ask" })],
        id: "CORE-2",
        type: "child_issue",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);

  await user.keyboard("q");
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-1"));
  await user.keyboard("q");
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-2"));
  expect(rowReport(view.container)).toBeUndefined();
});

/** An issue of two comments, the first with a reply under it and the second with none. */
function twoCommentsTree(): TreeNode {
  return node({
    children: [
      node({
        children: [node({ id: "comment-1a", text: "Under the first.", type: "comment" })],
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
}

// At 390 by 664 on a real issue, a zoom into a row taller than the window left the crumb line it
// draws under the band that stays at the top: the line was placed, then the row was placed by
// the page's one rule, which puts a row taller than the room at its top just under the band, and
// again by the row's own as it grew. The line is placed, and the row, its first line on screen
// under it, is placed by nothing - not as it grows either - until the reader's next act, a key
// that moves no focus among them. The same holds for the row `,` takes the focus to from a row
// under the zoom's root: that root, which the zoom out a level draws under its line. happy-dom
// lays nothing out, so the boxes are given, moved by each scroll, under a band of 60 px; each
// size observer still observing is called by hand once the row grows.
test("a zoom into a row taller than the window places its crumb line, and the row keeps its top under it until the reader acts", async () => {
  setViewport(390);
  const user = userEvent.setup();
  const band = 60;
  let scrolledTo = 0;
  let tall = 2 * window.innerHeight;
  // The row given the tall box: the row `.` zooms into, then the row `,` takes the focus to.
  let tallRow = "comment-1";
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.matches("p") && this.querySelector("[data-crumb]") !== null) {
      return new DOMRect(0, 10 - scrolledTo, 390, 20);
    }
    const row = this.closest<HTMLElement>("[data-node-row]");
    if (row?.dataset.nodeId !== tallRow) return new DOMRect();
    return this === row
      ? new DOMRect(0, 40 - scrolledTo, 390, tall)
      : this.matches("[data-node-bullet]")
        ? new DOMRect(0, 42 - scrolledTo, 18, 24)
        : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(((options: ScrollToOptions) => {
    scrolledTo += options.top ?? 0;
  }) as typeof window.scrollBy);
  const sizes = sizesByHand();
  const published = document.documentElement.style;
  published.setProperty(IDENTITY_HEIGHT_PROPERTY, `${band}px`);
  // Where the line and the row stand now: the line's top below the band, and the row's top on
  // screen, which is under the line, the row being drawn under it.
  const placed = () => ({
    line: 10 - scrolledTo >= band,
    row: 40 - scrolledTo >= band && 40 - scrolledTo < window.innerHeight,
  });
  // Two comments, the first with a reply, and a reply under that reply.
  const root = node({
    children: [
      node({
        children: [
          node({
            children: [node({ id: "comment-1b", text: "Under the reply.", type: "comment" })],
            id: "comment-1a",
            text: "Under the first.",
            type: "comment",
          }),
        ],
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  try {
    const view = renderTree(root);
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard(".");
    await waitFor(() =>
      expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull()
    );
    await waitFor(() => expect(scrolled).toHaveBeenCalled());
    expect(placed(), "the zoom left the line under the band or the row off the screen").toEqual({
      line: true,
      row: true,
    });
    const after = scrolledTo;

    tall = 3 * window.innerHeight;
    await sizes.resized();
    expect(scrolledTo, "the zoomed row was put back at its top as it grew").toBe(after);

    // The reader presses a key that moves no focus: grown again, the row is placed by the page's
    // one rule, its top just under the band.
    await user.keyboard("{Shift}");
    tall = 4 * window.innerHeight;
    await sizes.resized();
    expect(
      40 - scrolledTo,
      "the zoomed row was not placed as it grew once the reader had acted"
    ).toBe(band + CLEARANCE);

    // `,` from the reply under the zoom's root zooms out to the first comment and takes the focus
    // to that root, the first reply.
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1a"));
    await user.keyboard(".");
    await waitFor(() =>
      expect(view.container.querySelector('[data-crumb="comment-1"]')).not.toBeNull()
    );
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1b"));
    tallRow = "comment-1a";
    tall = 2 * window.innerHeight;
    await user.keyboard(",");
    await waitFor(() =>
      expect(view.container.querySelector('[data-crumb="comment-1"]')).toBeNull()
    );
    expect(focusedRow(view.container)).toBe("comment-1a");
    expect(
      placed(),
      "the row , took the focus to placed itself, and left the line under the band"
    ).toEqual({ line: true, row: true });
    const out = scrolledTo;
    tall = 3 * window.innerHeight;
    await sizes.resized();
    expect(scrolledTo, "the row , took the focus to was put back at its top as it grew").toBe(out);
  } finally {
    sizes.restore();
    boxes.mockRestore();
    scrolled.mockRestore();
    published.removeProperty(IDENTITY_HEIGHT_PROPERTY);
  }
});

// `.` zooms into a row of the outline's top level and `,` draws the whole outline again with the
// focus still on that row: it took no new focus, so nothing of its own placed it, and a zoom back
// out to the issue placed nothing either - at 1440 by 900 on a long issue the row stood 2,000 to
// 40,000 px below the window until the next key. Once the outline is drawn the row is brought on
// screen by the page's one rule and held there while the rows above it grow, as the cycle key's
// row is. A row `,` moves the focus to - the zoom's own, from a row under it - places itself as it
// takes the focus, and the zoom places it no second time. happy-dom lays nothing out, so the boxes
// are given, moved by each scroll, and the size observers watching the page are called by hand.
test(", from the row . zoomed into brings that row on screen and holds it there as the page grows", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  let scrolledTo = 0;
  // The one row given a box: how tall it is, and where it stands down the page. Every other row
  // is given none, which reads as whole on screen.
  let given = { height: 40, id: "comment-2", top: 300 };
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches("[data-node-row]") && this.dataset.nodeId === given.id
      ? new DOMRect(0, given.top - scrolledTo, 800, given.height)
      : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(((options: ScrollToOptions) => {
    scrolledTo += options.top ?? 0;
  }) as typeof window.scrollBy);
  const sizes = sizesByHand();
  try {
    const view = renderTree(twoCommentsTree());
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard("{End}");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
    await user.keyboard(".");
    await waitFor(() =>
      expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull()
    );
    scrolled.mockClear();

    // The whole outline drawn again puts the 40 px row 2,000 px below the window's foot.
    given = { ...given, top: scrolledTo + window.innerHeight + 2000 };
    await user.keyboard(",");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
    expect(focusedRow(view.container), "the key back out moved the focus off the row").toBe(
      "comment-2"
    );
    // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
    expect(
      scrolled,
      "the key back out of the row . zoomed into left that row off screen"
    ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });

    // The rows above it grow by 500 px more, which puts it below the window's foot again.
    given = { ...given, top: given.top + 500 };
    await sizes.resized(document.body);
    expect(
      scrolled,
      "the row was not held on screen as the rows above it grew"
    ).toHaveBeenLastCalledWith({ top: 500 });

    // `,` from the reply under the first comment's zoom moves the focus to the first comment,
    // a row taller than the window, which places itself once: at its top just under the band.
    await user.keyboard("{Home}");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard(".");
    await waitFor(() =>
      expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull()
    );
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1a"));
    scrolled.mockClear();
    given = {
      height: 2 * window.innerHeight,
      id: "comment-1",
      top: scrolledTo + window.innerHeight + 2000,
    };
    await user.keyboard(",");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
    expect(focusedRow(view.container)).toBe("comment-1");
    expect(scrolled, "the row , moved the focus to was placed twice").toHaveBeenCalledTimes(1);
    expect(scrolled).toHaveBeenCalledWith({ top: window.innerHeight + 2000 - CLEARANCE });
  } finally {
    sizes.restore();
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// `,` from a row under the zoom's root takes the focus to the root's row, another element, which
// places itself as it takes the focus. The zoom out places the row that kept the focus through
// it (`zoomKept`) only while that row still holds the focus once the whole outline is drawn:
// the row the key's move took the focus from is placed by nothing, since bringing it on screen
// would take the page away from the row the reader is on. happy-dom lays nothing out, so both
// rows are given boxes below the window, moved by each scroll.
test(", from a row under the zoom's root places the root's row alone, not the row the focus left", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  let scrolledTo = 0;
  // Where each row given a box stands down the page, 40 px tall; a row given none reads as whole
  // on screen.
  let tops: Readonly<Record<string, number>> = {};
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const top = this.matches("[data-node-row]") ? tops[this.dataset.nodeId ?? ""] : undefined;
    return top === undefined ? new DOMRect() : new DOMRect(0, top - scrolledTo, 800, 40);
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(((options: ScrollToOptions) => {
    scrolledTo += options.top ?? 0;
  }) as typeof window.scrollBy);
  try {
    const view = renderTree(twoCommentsTree());
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard(".");
    await waitFor(() =>
      expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull()
    );
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1a"));
    scrolled.mockClear();

    // The whole outline drawn again puts the first comment's row 2,000 px below the window's
    // foot, and its reply's 100 px below that.
    const below = scrolledTo + window.innerHeight + 2000;
    tops = { "comment-1": below, "comment-1a": below + 100 };
    await user.keyboard(",");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
    expect(focusedRow(view.container)).toBe("comment-1");
    await nextFrame();
    // The root's row alone, its foot brought 8 px clear of the screen's
    // (`identity-block.ts`'s `bringOnScreen`).
    expect(scrolled, "the zoom out placed two rows").toHaveBeenCalledTimes(1);
    expect(
      scrolled,
      "the zoom out brought on screen the row the focus left, not the row it is on"
    ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// A press on a crumb took the focus to the crumb, a button the zoom it makes takes off the page -
// the line drawn for that zoom holds no crumb of the node pressed, and a zoom out to the issue
// draws no line - and the browser dropped the focus to the page's body, at 1440 by 900 and at
// 390 by 664 px alike. A press on a crumb leaves the focus where it was, as one on a row's fold
// control does: on the row the reader is on, which the press keeps drawn by unfolding the path
// down to it, and which the zoom places as it places the row `,` leaves the focus on. happy-dom
// lays nothing out, so the reply's row is given a box below the window once each press has
// started, moved by each scroll.
test("a press on a crumb zooms out with the focus left on the row the reader is on, and brings that row on screen", async () => {
  const user = userEvent.setup();
  let scrolledTo = 0;
  // Where the reply's row stands down the page, 40 px tall, its bullet 8 px into it: undefined
  // until a press has started. Every other element is given no box, which reads as whole on
  // screen.
  let top: number | undefined;
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const row = this.closest<HTMLElement>("[data-node-row]");
    if (top === undefined || row?.dataset.nodeId !== "comment-1a") return new DOMRect();
    if (this === row) return new DOMRect(0, top - scrolledTo, 800, 40);
    return this.matches("[data-node-bullet]")
      ? new DOMRect(0, top + 8 - scrolledTo, 18, 24)
      : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(((options: ScrollToOptions) => {
    scrolledTo += options.top ?? 0;
  }) as typeof window.scrollBy);
  try {
    for (const width of [1440, 390]) {
      setViewport(width);
      top = undefined;
      const view = renderTree(twoCommentsTree());
      const { container } = view;
      await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("comment-1"));
      // Zoomed two levels deep: into the first comment, then into its reply.
      await user.keyboard(".");
      await waitFor(() =>
        expect(container.querySelector(`[data-crumb="${KEY}"]`), `at ${width} px`).not.toBeNull()
      );
      await user.keyboard("j");
      await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("comment-1a"));
      await user.keyboard(".");
      await waitFor(() =>
        expect(container.querySelector('[data-crumb="comment-1"]'), `at ${width} px`).not.toBeNull()
      );
      // A press on the crumb of `id`, from whose start the reply's row stands 2,000 px below the
      // window's foot, where the zoom its click makes draws it; the zoom takes the crumb away.
      const press = async (id: string) => {
        const crumb = container.querySelector<HTMLElement>(`[data-crumb="${id}"]`);
        if (crumb === null) throw new Error(`at ${width} px the crumb of ${id} is not drawn`);
        await user.pointer({ keys: "[MouseLeft>]", target: crumb });
        scrolled.mockClear();
        top = scrolledTo + window.innerHeight + 2000;
        await user.pointer({ keys: "[/MouseLeft]" });
        await waitFor(() =>
          expect(
            crumb.isConnected,
            `at ${width} px the press on ${id}'s crumb zoomed nowhere`
          ).toBe(false)
        );
      };

      // The thread's crumb zooms out to the thread, the issue's crumb still drawn above it.
      await press("comment-1");
      expect(container.querySelector(`[data-crumb="${KEY}"]`), `at ${width} px`).not.toBeNull();
      expect(
        focusedRow(container),
        `at ${width} px the press on the thread's crumb took the focus off the reply's row`
      ).toBe("comment-1a");
      // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
      expect(
        scrolled,
        `at ${width} px the zoom out to the thread left the reply's row off screen`
      ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });

      // The issue's crumb zooms out to the whole outline, which draws no crumb line.
      await press(KEY);
      expect(container.querySelector("[data-crumb]"), `at ${width} px`).toBeNull();
      expect(
        focusedRow(container),
        `at ${width} px the press on the issue's crumb took the focus off the reply's row`
      ).toBe("comment-1a");
      expect(
        scrolled,
        `at ${width} px the zoom out to the issue left the reply's row off screen`
      ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });
      view.unmount();
    }
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// The zoom out a crumb's press makes draws open only what something opened: the node the zoom
// was rooted at is drawn open as the zoom's own row, and folded once the tree is rooted above it
// unless the reader, the frontier, an address or a filter opened it, and a comment is on no
// frontier. `.` on a comment opens only the path above it, so a press on the issue's crumb from
// that comment's reply drew the comment folded, the reply's row nowhere, and the focus went to
// the row drawn at the reply's place, the second comment's; a press on the thread's crumb from a
// reply under the reply `.` zoomed into did the same a level down. The press unfolds the path
// down to the row the reader is on, at both widths, so that row is drawn, keeps the focus, and is
// placed by the zoom. happy-dom lays nothing out, so that row is given a box below the window
// once each press has started, moved by each scroll.
test("a press on a crumb keeps the row the reader is on drawn and focused where the zoom out folds the branch above it", async () => {
  const user = userEvent.setup();
  let scrolledTo = 0;
  // The row given a box and where it stands down the page, 40 px tall, its bullet 8 px into it:
  // undefined until a press has started. Every other element is given no box, which reads as
  // whole on screen.
  let placed: { readonly id: string; readonly top: number } | undefined;
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const row = this.closest<HTMLElement>("[data-node-row]");
    if (placed === undefined || row?.dataset.nodeId !== placed.id) return new DOMRect();
    if (this === row) return new DOMRect(0, placed.top - scrolledTo, 800, 40);
    return this.matches("[data-node-bullet]")
      ? new DOMRect(0, placed.top + 8 - scrolledTo, 18, 24)
      : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(((options: ScrollToOptions) => {
    scrolledTo += options.top ?? 0;
  }) as typeof window.scrollBy);
  try {
    for (const width of [1440, 390]) {
      setViewport(width);
      placed = undefined;
      // Two comments, the first with a reply under it, and that reply with a reply of its own.
      const view = renderTree(
        node({
          children: [
            node({
              children: [
                node({
                  children: [node({ id: "comment-1b", text: "Under the reply.", type: "comment" })],
                  id: "comment-1a",
                  text: "Under the first.",
                  type: "comment",
                }),
              ],
              id: "comment-1",
              text: "The first words.",
              type: "comment",
            }),
            node({ id: "comment-2", text: "The second words.", type: "comment" }),
          ],
          id: KEY,
          type: "issue",
        })
      );
      const { container } = view;
      // A press on the crumb of `id`, from whose start the row of `on` stands 2,000 px below the
      // window's foot, where the zoom its click makes draws it; the zoom takes the crumb away.
      const press = async (id: string, on: string) => {
        const crumb = container.querySelector<HTMLElement>(`[data-crumb="${id}"]`);
        if (crumb === null) throw new Error(`at ${width} px the crumb of ${id} is not drawn`);
        await user.pointer({ keys: "[MouseLeft>]", target: crumb });
        scrolled.mockClear();
        placed = { id: on, top: scrolledTo + window.innerHeight + 2000 };
        await user.pointer({ keys: "[/MouseLeft]" });
        await waitFor(() =>
          expect(
            crumb.isConnected,
            `at ${width} px the press on ${id}'s crumb zoomed nowhere`
          ).toBe(false)
        );
      };
      await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("comment-1"));

      // `.` on the first comment, which opens the path above it alone, then `j` to its reply.
      await user.keyboard(".");
      await waitFor(() =>
        expect(container.querySelector(`[data-crumb="${KEY}"]`), `at ${width} px`).not.toBeNull()
      );
      await user.keyboard("j");
      await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("comment-1a"));
      await press(KEY, "comment-1a");
      expect(container.querySelector("[data-crumb]"), `at ${width} px`).toBeNull();
      expect(
        focusedRow(container),
        `at ${width} px the press on the issue's crumb took the focus off the reply's row`
      ).toBe("comment-1a");
      expect(
        rowsFor(container, "comment-1a"),
        `at ${width} px the zoom out to the issue drew the reply's row nowhere`
      ).toHaveLength(1);
      // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
      expect(
        scrolled,
        `at ${width} px the zoom out to the issue left the reply's row off screen`
      ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });

      // `.` on the reply, which opens the first comment above it, then `j` to the reply under it:
      // the thread's crumb zooms out to the first comment, the reply drawn there as its child.
      await user.keyboard(".");
      await waitFor(() =>
        expect(container.querySelector('[data-crumb="comment-1"]'), `at ${width} px`).not.toBeNull()
      );
      await user.keyboard("j");
      await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("comment-1b"));
      await press("comment-1", "comment-1b");
      expect(container.querySelector(`[data-crumb="${KEY}"]`), `at ${width} px`).not.toBeNull();
      expect(
        focusedRow(container),
        `at ${width} px the press on the thread's crumb took the focus off the reply's reply`
      ).toBe("comment-1b");
      expect(
        rowsFor(container, "comment-1b"),
        `at ${width} px the zoom out to the thread drew the reply's reply nowhere`
      ).toHaveLength(1);
      expect(
        scrolled,
        `at ${width} px the zoom out to the thread left the reply's reply off screen`
      ).toHaveBeenCalledWith({ top: 2000 + 40 + CLEARANCE });
      view.unmount();
    }
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// `z` holds the focused row on screen while the rows it unfolds above the row grow
// (`holdOnScreen`), and a focus `f` left on a chip of the row is the row's own: the hold ended as
// soon as the page grew with the focus on the chip rather than on the row itself, and the row was
// left where the growth pushed it. happy-dom lays nothing out, so the row's box is given, and the
// size observers watching the page are called by hand once the page grows.
test("z holds the focused row on screen as the page grows, with the focus on a chip of the row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({
        chips: [
          {
            answered: true,
            gone: false,
            name: "The spec block",
            ref: `dispatch://${KEY}/spec#b-r1`,
          },
        ],
        id: "comment-2",
        text: "The second words.",
        type: "comment",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  // The second row 300 px down and whole on screen, until the bodies drawn above it push it below
  // the window's foot.
  let top = 300;
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches("[data-node-row]") && this.dataset.nodeId === "comment-2"
      ? new DOMRect(0, top, 800, 40)
      : new DOMRect();
  });
  const sizes = sizesByHand();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const view = renderTree(root);
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
    await user.keyboard("f");
    expect(document.activeElement?.textContent).toBe("The spec block");
    await user.keyboard("z");
    expect(document.activeElement?.textContent, "z took the focus off the chip").toBe(
      "The spec block"
    );
    scrolled.mockClear();

    // What `z` drew above it put its 40 px row 400 px below the screen's foot.
    top = window.innerHeight + 400;
    await sizes.resized(document.body);
    // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
    expect(
      scrolled,
      "the row the focus is in was not brought back on screen as the page grew"
    ).toHaveBeenCalledWith({ top: 448 });
  } finally {
    sizes.restore();
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// At 390 by 664 Right on the row the tree is zoomed to - the view's own root, drawn open over
// what it holds - zoomed into the row it was on, which moved nothing and said nothing. It steps
// into the row as into any open row, and where nothing is under it says so at the row.
test("Right on the row the tree is zoomed to moves to its first child, or says nothing is under it", async () => {
  setViewport(390);
  const user = userEvent.setup();
  const view = renderTree(twoCommentsTree());
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard(".");
  await waitFor(() => expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull());
  expect(focusedRow(view.container)).toBe("comment-1");
  await user.keyboard("{ArrowRight}");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1a"));
  expect(rowReport(view.container)).toBeUndefined();

  await user.keyboard(",");
  await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
  await user.keyboard("{End}");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  await user.keyboard(".");
  await waitFor(() => expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull());
  await user.keyboard("{ArrowRight}");
  await waitFor(() => expect(rowReport(view.container)).toBe(KEY_REPORTS.nothing_under));
  expect(focusedRow(view.container)).toBe("comment-2");
});

/** An issue whose outline holds a comment and then the group of its two children. */
function childrenTree(): TreeNode {
  return node({
    children: [
      node({
        children: [node({ id: "comment-1a", text: "Under the first.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "CORE-2", text: "The first child", type: "child_issue" }),
      node({ id: "CORE-3", text: "The second child", type: "child_issue" }),
    ],
    id: KEY,
    type: "issue",
  });
}

// `t h` opens the children's own address and puts the focus on the group's first row
// each time it is pressed. The address alone moves the focus once per address, so a second
// `t h` after the reader had moved on moved nothing.
test("t h puts the focus on the first child's row, and again after the reader moved on", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(childrenTree());
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));

  await user.keyboard("th");
  await waitFor(() => expect(focusedRow(view.container)).toBe("CORE-2"));
  expect(view.address()).toBe(buildIssuePath({ key: KEY, kind: "children" }));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("CORE-3"));
  await user.keyboard("th");
  await waitFor(() => expect(focusedRow(view.container)).toBe("CORE-2"));
});

// A zoomed tree is rooted at the node the reader zoomed into, which holds no child, so the
// children are no rows of it: `t h` leaves the zoom for the whole outline, as every key that
// takes the reader to a node does.
test("t h from a zoom into a node puts the focus on the first child's row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(childrenTree());
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard(".");
  await waitFor(() => expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull());
  expect(rowsFor(view.container, "CORE-2")).toHaveLength(0);

  await user.keyboard("th");
  await waitFor(() => expect(focusedRow(view.container)).toBe("CORE-2"));
  expect(view.container.querySelector("[data-crumb]")).toBeNull();
});

// `t h` is offered only where the outline holds a child: on an issue with none it opens no
// address, moves no focus and draws no line.
test("t h on an issue with no children changes nothing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const before = view.address();

  await user.keyboard("th");
  expect(view.address()).toBe(before);
  expect(focusedRow(view.container)).toBe("comment-1");
  expect(rowReport(view.container)).toBeUndefined();
});

// `.` zooms into the focused node, and the key then takes the reader to that node so its row
// keeps the focus (`useTreeKeymap.ts`'s `tree-zoom-in`); on the zoomed node's own row it holds
// the zoom where it is rather than walking back out one level. Read below 1024 px, where the
// outline is read as at every width (R11).
test(". zooms into the focused node, and holds the zoom on that node's own row", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "spec#b-r1", type: "requirement" })],
        id: "spec",
        type: "document",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  const [row] = rowsFor(view.container, "spec");
  if (row === undefined) throw new Error("the document has no row");

  await act(async () => row.focus());
  await user.keyboard(".");
  // The tree is on the document now: its own row and what it holds, with the issue left
  // behind in the breadcrumb above the rows.
  await waitFor(() => expect(rowsFor(view.container, "spec#b-r1").length).toBe(1));
  expect(rowsFor(view.container, KEY).length).toBe(0);
  expect(view.getByRole("button", { name: KEY })).toBeTruthy();

  // On the zoomed node's own row - which is where the key left the focus - `.` holds the zoom
  // where it is rather than taking the reader back out of it.
  await user.keyboard(".");
  expect(rowsFor(view.container, "spec#b-r1").length).toBe(1);
  expect(rowsFor(view.container, KEY).length).toBe(0);
});

// A row's key is the path from the issue down to it, and it holds the row being edited and the
// tree's one tab stop. The whole tree drawn from a zoomed node keyed its rows from that node, so a
// zoom re-keyed every row below it. Every row keeps its key through a zoom in and back out.
test("a row keeps its key through a zoom in and back out", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "verification#b-c1", state: "no-result", type: "check" })],
        id: "spec#b-r1",
        statement: true,
        type: "requirement",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  const [check] = rowsFor(view.container, "verification#b-c1");
  const [requirement] = rowsFor(view.container, "spec#b-r1");
  if (check === undefined || requirement === undefined) throw new Error("the tree drew no rows");
  const keyOfCheck = check.getAttribute("data-row-key");
  const keyOfRequirement = requirement.getAttribute("data-row-key");

  // Zoomed into the requirement, the check is the same row under the same key.
  await act(async () => requirement.focus());
  await user.keyboard(".");
  await waitFor(() => expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull());
  expect(rowsFor(view.container, "verification#b-c1")[0]?.getAttribute("data-row-key")).toBe(
    keyOfCheck
  );
  expect(rowsFor(view.container, "spec#b-r1")[0]?.getAttribute("data-row-key")).toBe(
    keyOfRequirement
  );

  // Back out of the zoom, both rows keep the keys they had, and the tab stop rests on the row the
  // zoom was on.
  await user.keyboard(",");
  await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
  expect(rowsFor(view.container, "verification#b-c1")[0]?.getAttribute("data-row-key")).toBe(
    keyOfCheck
  );
  await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));
  expect(rowsFor(view.container, "spec#b-r1")[0]?.tabIndex).toBe(0);
});

// The header is the issue, so the outline draws no row that repeats its key and title, and its
// first row is the first node under the issue; the page opens with the focus on that row where
// no open question waits on the reader.
test("the outline draws no row of the issue, and opens with the focus on its first row", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "ask-1", question: true, text: "Which flag?", type: "ask" }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  expect(rowsFor(view.container, KEY)).toHaveLength(0);
  const rows = [...view.container.querySelectorAll("[data-node-row]")];
  expect(rows.map((row) => row.getAttribute("data-node-id"))).toEqual(["comment-1", "ask-1"]);
  expect(rows.map((row) => row.getAttribute("aria-level"))).toEqual(["1", "1"]);
  // The open question waits on somebody else, so the first row is where the page opens.
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
});

// The focused node is named in the panel by its own first line, and a node with no words - a
// plan task whose block holds none - by the page's word for its kind, never by its id, which is
// no name a reader reads (the rule a parent line already follows).
test("a focused row whose node has no words names it by its kind, not its id", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words. And more.", type: "comment" }),
      node({ id: "plan#b-t1", text: "", type: "plan_task" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  expect(view.focusedNode()?.title).toBe("The first words.");

  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("plan#b-t1"));
  expect(view.focusedNode()?.id).toBe("plan#b-t1");
  expect(view.focusedNode()?.title).toBe("plan task");
});

// The page opens with the focus on the first open question that waits on the reader, in the
// outline's own order, its path unfolded.
test("the page opens with the focus on the first open question that waits on the reader", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({
        children: [
          node({ id: "ask-theirs", question: true, text: "Not yours?", type: "ask" }),
          node({ id: "ask-mine", question: true, text: "Yours?", type: "ask", waiting: true }),
        ],
        held: 2,
        id: "comment-2",
        text: "A thread.",
        type: "comment",
      }),
      node({ id: "ask-later", question: true, text: "Also yours?", type: "ask", waiting: true }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-mine"));
});

// The question the page opens on takes the focus while the outline's other reads are still out,
// and each that answers after draws rows above it, which push its row below the screen without
// changing the row's own size, so the row's own placing never sees it move: the page holds it on
// screen as the page grows, until the reader acts. happy-dom lays nothing out, so the row's box
// is given, and the size observers watching the page are called by hand.
test("the open question the page opens on is held on screen as the page grows, until the reader acts", async () => {
  setViewport(1440);
  const sizes = sizesByHand();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const root = node({
      children: [
        node({ id: "comment-1", text: "The first words.", type: "comment" }),
        node({ id: "ask-mine", question: true, text: "Yours?", type: "ask", waiting: true }),
      ],
      id: KEY,
      type: "issue",
    });
    const { container } = renderTree(root);
    await waitFor(() => expect(focusedRow(container)).toBe("ask-mine"));
    const [question] = rowsFor(container, "ask-mine");
    if (question === undefined) throw new Error("the tree draws no row for ask-mine");
    // What landed above it put its 28 px row 400 px below the screen's foot.
    spyOn(question, "getBoundingClientRect").mockReturnValue(
      new DOMRect(0, window.innerHeight + 400, 600, 28)
    );
    scrolled.mockClear();
    await sizes.resized(document.body);
    // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
    expect(scrolled).toHaveBeenCalledWith({ top: 436 });

    // A wheel is the reader taking over: the page grows on and moves them no more.
    await act(async () => window.dispatchEvent(new Event("wheel")));
    scrolled.mockClear();
    await sizes.resized(document.body);
    expect(scrolled).not.toHaveBeenCalled();
  } finally {
    sizes.restore();
    scrolled.mockRestore();
  }
});

/** Lets `count` animation frames pass, in which the opening gives any focus it has asked for. */
async function framesPass(count = 3): Promise<void> {
  for (let frame = 0; frame < count; frame += 1) {
    await act(() => new Promise<void>((done) => requestAnimationFrame(() => done())));
  }
}

// The reads answer one after another, and the outline's first rows were whatever had arrived:
// the issue's children come with the issue, before the spec's blocks. The opening focused the
// first row drawn, and a second later the row a later read drew above it, with no act of the
// reader's: a second announcement to a screen reader, and `j` or Enter pressed in that second
// acted on the wrong row. The first focus waits until every read has answered and lands once,
// on the first row they order, and a question that arrives after it moves nothing.
test("the page's first focus waits for the reads and lands once, on the first row they order", async () => {
  setViewport(1440);
  const child = node({ id: "comment-2", text: "The child's words.", type: "comment" });
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const focusedRows: string[] = [];
  const note = (event: FocusEvent) => {
    const row = (event.target as HTMLElement).closest?.("[data-node-row]");
    if (row !== null && row !== undefined) focusedRows.push(row.getAttribute("data-node-id") ?? "");
  };
  document.addEventListener("focusin", note);
  try {
    const view = renderTree(node({ children: [child], id: KEY, type: "issue" }), {
      readsHeld: false,
      readsSettled: false,
    });
    await waitFor(() => expect(rowsFor(view.container, "comment-2").length).toBe(1));
    await framesPass();
    expect(focusedRows, "the page focused a row before its reads had answered").toEqual([]);
    view.settle(node({ children: [first, child], id: KEY, type: "issue" }));
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    // A question that waits on the reader arrives after the landing.
    view.reread(
      node({
        children: [
          first,
          child,
          node({ id: "ask-1", question: true, text: "Which flag?", type: "ask", waiting: true }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    await waitFor(() => expect(rowsFor(view.container, "ask-1").length).toBe(1));
    await framesPass();
    expect(focusedRows, "the page's first focus moved after it landed").toEqual(["comment-1"]);
    view.unmount();
  } finally {
    document.removeEventListener("focusin", note);
  }
});

// A read after the page opened on its first row can draw that row's node nowhere - the comment
// deleted - which takes the row out of the page with the focus, and the browser drops it to the
// body. The page then held the focus on no row (the browser row `an open question is unfolded
// and on the first screen`, at 1440 by 900 and at 390 by 664). The opening, whose node the page
// draws nowhere now, ends, and the focus goes to the row at its place. A node drawn under a new
// parent is drawn, unfolded as every node is (R11), and the next case reads it.
test("a read that draws the first row's node nowhere gives the focus to the row now at its place", async () => {
  setViewport(1440);
  const reply = node({ id: "comment-2", text: "A reply.", type: "comment" });
  const later = node({ id: "comment-9", text: "Later words.", type: "comment" });
  const tree = renderPageTree(node({ children: [reply, later], id: KEY, type: "issue" }));
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-2"));

  // The read answers without the reply, and with another comment at its place.
  tree.redraw(
    node({
      children: [node({ id: "comment-1", text: "The first words.", type: "comment" }), later],
      id: KEY,
      type: "issue",
    })
  );
  expect(rowsFor(container, "comment-1")).toHaveLength(1);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
});

// The same where the read draws the row the opening focused again, under its new parent, and a
// row above it: the parent holds an open question that waits on somebody else, so its branch is
// drawn. The row's old element leaves the page with the focus, and the opening gives it back to
// the same node's new row: the first row drawn above it now takes none, since the page's first
// focus lands once and moves to no other row.
test("a focus a redraw drops while the page opens goes back to the same node's new row", async () => {
  setViewport(1440);
  const theirs = node({ id: "ask-theirs", question: true, text: "Not yours?", type: "ask" });
  const later = node({ id: "comment-9", text: "Later words.", type: "comment" });
  const tree = renderPageTree(node({ children: [theirs, later], id: KEY, type: "issue" }));
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("ask-theirs"));
  const theirsKey = rowsFor(container, "ask-theirs")[0]?.dataset.rowKey;

  // A later read places the question under a node that has arrived since.
  tree.redraw(
    node({
      children: [
        node({
          children: [theirs],
          held: 1,
          id: "comment-1",
          text: "The first words.",
          type: "comment",
        }),
        later,
      ],
      id: KEY,
      type: "issue",
    })
  );
  const drawnAgain = rowsFor(container, "ask-theirs").map((row) => row.dataset.rowKey);
  expect(drawnAgain).toHaveLength(1);
  expect(drawnAgain).not.toContain(theirsKey);
  await waitFor(() =>
    expect(focusedRow(container), "the row drawn again lost the focus").toBe("ask-theirs")
  );
});

// The opening takes the focus back only from a row that has left the page. A row still on it
// that lost the focus lost it to something else - here a script's blur, as the browser rows'
// `focusThePage` makes before they press Tab - and the focus stays where that left it, through
// the reads that answer after.
test("a focus taken off the opening's row while the row stays on the page ends the opening", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const tree = renderPageTree(node({ children: [first], id: KEY, type: "issue" }));
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));

  await act(async () => (document.activeElement as HTMLElement | null)?.blur());
  tree.redraw(
    node({
      children: [node({ id: "comment-0", text: "Older words.", type: "comment" }), first],
      id: KEY,
      type: "issue",
    })
  );
  expect(rowsFor(container, "comment-0")).toHaveLength(1);
  expect(document.activeElement).toBe(document.body);
});

// The same for the question the page opened on, which is where the page stays until the reader
// acts: a later read places the question under a node that has arrived since, which gives its
// row another key, and the browser drops the focus of the element that leaves the page. The
// opening gives the focus back to the question's new row. An answer arriving for the question
// then moves nothing: the question is open no more, so the thread the frontier opened for it
// alone folds, and its row leaves the page with the focus, which went to the page's body. The
// opening unfolds the thread again where the outline stands, and the focus stays on the
// question, not on the outline's first row or on the next question that waits on the reader.
// At 390 px, where the outline is read one node at a time, it went to the question as a key
// goes to a node, which zooms to the node's parent there: the answer drew the question's thread
// alone, under a crumb line.
test("the question the page opened on takes back a focus a redraw drops, and keeps it", async () => {
  const question = (open: boolean) =>
    node({ id: "ask-mine", question: open, text: "Yours?", type: "ask", waiting: open });
  const later = node({
    id: "ask-later",
    question: true,
    text: "Also yours?",
    type: "ask",
    waiting: true,
  });
  // A later read places the question under a node that has arrived since, whose thread the
  // frontier opens because the question in it is open.
  const placedUnder = (open: boolean) =>
    node({
      children: [
        node({
          children: [question(open)],
          held: 1,
          id: "comment-1",
          text: "The first words.",
          type: "comment",
        }),
        later,
      ],
      id: KEY,
      type: "issue",
    });
  for (const width of [1440, 390]) {
    setViewport(width);
    const tree = renderPageTree(
      node({ children: [question(true), later], id: KEY, type: "issue" })
    );
    const { container } = tree.view;
    await waitFor(() => expect(focusedRow(container), `at ${width} px`).toBe("ask-mine"));
    const questionKey = rowsFor(container, "ask-mine")[0]?.dataset.rowKey;

    tree.redraw(placedUnder(true));
    const drawnAgain = rowsFor(container, "ask-mine").map((row) => row.dataset.rowKey);
    expect(drawnAgain).toHaveLength(1);
    expect(drawnAgain).not.toContain(questionKey);
    await waitFor(() =>
      expect(
        focusedRow(container),
        `at ${width} px the question's row drawn again lost the focus`
      ).toBe("ask-mine")
    );

    // The answer arrives: the question is open no more, and waits on the reader no more.
    tree.redraw(placedUnder(false));
    await nextFrame();
    expect(
      focusedRow(container),
      `at ${width} px the answer moved the focus off the question`
    ).toBe("ask-mine");
    expect(
      container.querySelector("[data-crumb]"),
      `at ${width} px the answer zoomed the outline into the question's thread`
    ).toBeNull();
    tree.view.unmount();
  }
});

// From the answer on, the question's row the opening unfolded the path to again is the row the
// opening focused (`unfoldTo`), as the row its landing focused was before. It is held on screen
// as the page grows, as that row was until the answer took it out of the page, which ended its
// hold. A later read that takes it out of the page - here one that places the question's thread
// under a node that has arrived since, which nothing opens - has the opening unfold the path to
// the question again. Read as nobody's row, the focus the answer's unfolding gave the question
// ended the opening in the same render, and the later read left the focus on the page. happy-dom
// lays nothing out, so the row's box is given, and the size observers watching the page are
// called by hand.
test("the question the opening unfolded again after its answer keeps the focus through a later read", async () => {
  setViewport(1440);
  const sizes = sizesByHand();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const thread = (open: boolean) =>
      node({
        children: [
          node({ id: "ask-mine", question: open, text: "Yours?", type: "ask", waiting: open }),
        ],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      });
    const tree = renderPageTree(node({ children: [thread(true)], id: KEY, type: "issue" }));
    const { container } = tree.view;
    await waitFor(() => expect(focusedRow(container)).toBe("ask-mine"));

    // The answer folds the thread the frontier alone opened, and the opening unfolds it again.
    tree.redraw(node({ children: [thread(false)], id: KEY, type: "issue" }));
    await nextFrame();
    expect(focusedRow(container), "the answer moved the focus off the question").toBe("ask-mine");
    const unfolded = rowsFor(container, "ask-mine")[0];
    if (unfolded === undefined) throw new Error("the answer's unfolding drew no row for ask-mine");
    // What landed above it put its 28 px row 400 px below the screen's foot.
    spyOn(unfolded, "getBoundingClientRect").mockReturnValue(
      new DOMRect(0, window.innerHeight + 400, 600, 28)
    );
    scrolled.mockClear();
    await sizes.resized(document.body);
    // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
    expect(
      scrolled,
      "the question's row the answer unfolded again was not held on screen"
    ).toHaveBeenCalledWith({ top: 436 });

    tree.redraw(
      node({
        children: [
          node({
            children: [thread(false)],
            held: 2,
            id: "comment-0",
            text: "Older words.",
            type: "comment",
          }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    await nextFrame();
    expect(
      rowsFor(container, "ask-mine"),
      "the later read left the question folded away: the answer's unfolding ended the opening"
    ).toHaveLength(1);
    expect(rowsFor(container, "ask-mine")[0], "the later read kept the question's row").not.toBe(
      unfolded
    );
    expect(
      focusedRow(container),
      "the later read left the focus off the question: the answer's unfolding ended the opening"
    ).toBe("ask-mine");
  } finally {
    sizes.restore();
    scrolled.mockRestore();
  }
});

// The opening unfolds the path to the question it opened on once for the row it focused there
// (`unfoldTo`). A question that leaves the tree - a read that answers holds it no more - leaves
// no row to go back to, and unfolding the path to it at each render drew the tree again from
// its own effect, without end: React reports such a loop as an error and goes on with it, so
// the report is thrown here, which ends it and fails the case.
test("a question the page opened on that leaves the tree is gone back to once, not at every render", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const reported = console.error;
  const loop = spyOn(console, "error").mockImplementation((...said: unknown[]) => {
    if (String(said[0]).startsWith("Maximum update depth exceeded")) {
      throw new Error("the outline drew itself again from its own effect without end");
    }
    reported.apply(console, said);
  });
  try {
    const tree = renderPageTree(node({ children: [first, question], id: KEY, type: "issue" }));
    const { container } = tree.view;
    await waitFor(() => expect(focusedRow(container)).toBe("ask-1"));

    // The question leaves the tree, and the reads that answer after it draw the tree again.
    for (let read = 0; read < 3; read += 1) {
      tree.redraw(node({ children: [first], id: KEY, type: "issue" }));
      await nextFrame();
    }
    expect(rowsFor(container, "ask-1")).toHaveLength(0);
    expect(rowsFor(container, "comment-1")).toHaveLength(1);
  } finally {
    loop.mockRestore();
  }
});

// An address that names a node puts the focus on it, and takes it back from the page itself
// - where a document mounting beside the tree drops it - until the reader's first act. After
// that the focus is the reader's: a second Escape or a press on blank space leaves it on the
// page, and no tree drawn after takes it back (the efficiency reviewer's finding on `/children`
// and `#b-` addresses, where it did on every render).
test("an address's node takes the focus back until the reader acts, and not after", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const named = node({ id: "comment-2", text: "The second words.", type: "comment" });
  const root = node({
    children: [node({ id: "comment-1", text: "The first words.", type: "comment" }), named],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { reveal: [root, named] });
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));

  // Before the reader acts: the focus dropped to the page is taken back.
  await act(async () => (document.activeElement as HTMLElement | null)?.blur());
  view.redraw();
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));

  // A key the reader presses - Shift alone, which moves nothing - and then the focus on the page.
  await user.keyboard("{Shift}");
  expect(focusedRow(view.container)).toBe("comment-2");
  await act(async () => (document.activeElement as HTMLElement | null)?.blur());
  view.redraw();
  expect(document.activeElement, "a tree drawn after the reader acted took the focus back").toBe(
    document.body
  );
});

// The tester's red row on c-outline (`an issue opened by a link opens with the focus on its
// question, or on its first row where none waits`): the app focuses its main element at each
// page change but the first (`app.tsx`), and that element holds the tree, whose reads have drawn
// no row yet. Read as somebody's focus, that ended the opening, and the page opened with the
// focus on no row. It is nobody's, as the page itself is nobody's, and the opening places the
// focus once the rows are drawn. The case's container stands for the app's main element.
test("the page opens on its question, or its first row, with the focus on what holds the tree", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  for (const [children, opensOn] of [
    [[first], "comment-1"],
    [[first, question], "ask-1"],
  ] as const) {
    const tree = renderPageTree(node({ id: KEY, type: "issue" }));
    const { container } = tree.view;
    container.tabIndex = -1;
    await act(async () => container.focus());
    expect(document.activeElement).toBe(container);
    tree.redraw(node({ children: [...children], id: KEY, type: "issue" }));
    await waitFor(() =>
      expect(
        focusedRow(container),
        `the page opened with the focus on no row, not on ${opensOn}, with the focus on what holds the tree`
      ).toBe(opensOn)
    );
    tree.view.unmount();
  }
});

// The same focus meets an address that names a node, opened by a link: the row the address took
// the focus to is given it back from what holds the tree, as from the page itself, until the
// reader acts.
test("an address's node takes the focus back from what holds the tree", async () => {
  setViewport(1440);
  const named = node({ id: "comment-2", text: "The second words.", type: "comment" });
  const root = node({
    children: [node({ id: "comment-1", text: "The first words.", type: "comment" }), named],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { reveal: [root, named] });
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));

  view.container.tabIndex = -1;
  await act(async () => view.container.focus());
  view.redraw();
  await waitFor(() =>
    expect(
      focusedRow(view.container),
      "the address's node left the focus on what holds the tree"
    ).toBe("comment-2")
  );
});

// A read that answers before the frame an address's landing waits for draws the tree again, and
// the landing waits on for that same frame: asked again at each render, it was put off by a frame
// each time, for as long as reads kept drawing the tree.
test("a tree drawn again before an address's landing frame leaves the landing to that frame", async () => {
  setViewport(1440);
  const named = node({ id: "comment-2", text: "The second words.", type: "comment" });
  const root = node({
    children: [node({ id: "comment-1", text: "The first words.", type: "comment" }), named],
    id: KEY,
    type: "issue",
  });
  const frames = framesByHand();
  try {
    const view = renderTree(root, { reveal: [root, named] });
    // The frames the render that drew the address's row asked for, the landing's among them.
    const first = frames.asked();
    view.redraw();
    view.redraw();
    await frames.run(first);
    expect(
      focusedRow(view.container),
      "the address's row took no focus in the frame its landing first asked for"
    ).toBe("comment-2");
  } finally {
    frames.restore();
  }
});

// The same for the frames the page's opening asks: the first row's where no question waits on
// the reader, and the landing on the question where one does.
test("a tree drawn again before the opening's first-row frame leaves the focus to that frame", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const frames = framesByHand();
  try {
    const view = renderTree(root);
    // The frames the first render asked for, the first row's among them.
    const first = frames.asked();
    view.redraw();
    view.redraw();
    await frames.run(first);
    expect(
      focusedRow(view.container),
      "the first row took no focus in the frame the opening first asked for"
    ).toBe("comment-1");
  } finally {
    frames.restore();
  }
});

test("a tree drawn again before the landing on the question the page opens on leaves the landing to its frame", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "ask-1", question: true, text: "Which flag?", type: "ask", waiting: true }),
    ],
    id: KEY,
    type: "issue",
  });
  const frames = framesByHand();
  try {
    const view = renderTree(root);
    // The frames the first render asked for, the question's landing among them.
    const first = frames.asked();
    view.redraw();
    view.redraw();
    await frames.run(first);
    expect(
      focusedRow(view.container),
      "the question took no focus in the frame its landing first asked for"
    ).toBe("ask-1");
  } finally {
    frames.restore();
  }
});

// The asks' read can answer after the first row was drawn and before the frame that is to give
// it the focus: the question takes the focus in the first row's place, and the first row's frame
// asks nothing of the opening, which goes on - here through a later read that draws the
// question's row again under another key.
test("a question whose read answers before the first row's frame takes the focus, and the opening goes on", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const frames = framesByHand();
  try {
    const tree = renderPageTree(node({ children: [first], id: KEY, type: "issue" }));
    const { container } = tree.view;
    tree.redraw(node({ children: [first, question], id: KEY, type: "issue" }));
    await frames.run();
    expect(focusedRow(container), "the question whose read answered took no focus").toBe("ask-1");

    // A later read places the question under a node that has arrived since.
    tree.redraw(
      node({
        children: [
          first,
          node({
            children: [question],
            held: 1,
            id: "comment-2",
            text: "A thread.",
            type: "comment",
          }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    await frames.run();
    expect(
      focusedRow(container),
      "the question's row drawn again lost the focus: the first row's frame ended the opening"
    ).toBe("ask-1");
  } finally {
    frames.restore();
  }
});

// A script's focus on a control before the question's landing frame is no act of the reader's,
// and the landing leaves the focus there (`landOnRow`). A read that draws the question's row
// again under another key before that frame ends the opening too, rather than land on the new
// row from the control.
test("a focus a script puts on a control before the question's landing frame stays there, through a redraw of the question's row", async () => {
  setViewport(1440);
  const { control } = controlOutside();
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const frames = framesByHand();
  try {
    const tree = renderPageTree(node({ children: [question], id: KEY, type: "issue" }));
    const { container } = tree.view;
    const asked = rowsFor(container, "ask-1")[0];
    await act(async () => control.focus());
    // A later read places the question under a node that has arrived since.
    tree.redraw(
      node({
        children: [
          node({
            children: [question],
            held: 1,
            id: "comment-1",
            text: "The first words.",
            type: "comment",
          }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    expect(rowsFor(container, "ask-1"), "the read drew the question's row nowhere").toHaveLength(1);
    expect(rowsFor(container, "ask-1")[0], "the read kept the question's row").not.toBe(asked);
    await frames.run();
    expect(
      document.activeElement,
      "the question's row drawn again took the focus from the control a script focused"
    ).toBe(control);
  } finally {
    frames.restore();
  }
});

// While the question's landing waits for its frame, reads that draw the tree again meanwhile end
// nothing, and the opening goes on - here through a later read that draws the question's row
// again under another key, which gives the focus back to the question's new row.
test("reads that draw the tree again while the question's landing waits end nothing, and the opening goes on", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const frames = framesByHand();
  try {
    const asking = node({ children: [first, question], id: KEY, type: "issue" });
    const tree = renderPageTree(asking);
    const { container } = tree.view;
    // Other reads draw the tree again before the landing's frame.
    tree.redraw(asking);
    tree.redraw(asking);
    await frames.run();
    expect(focusedRow(container), "the question that waits took no focus").toBe("ask-1");

    // A later read places the question under a node that has arrived since.
    tree.redraw(
      node({
        children: [
          first,
          node({
            children: [question],
            held: 1,
            id: "comment-2",
            text: "A thread.",
            type: "comment",
          }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    await frames.run();
    expect(
      focusedRow(container),
      "the question's row drawn again lost the focus: a redraw while its landing waited ended the opening"
    ).toBe("ask-1");
  } finally {
    frames.restore();
  }
});

// The same where the focus is on the question's own row before its landing's frame - a script's
// focus, which is no act of the reader's, put where the landing is to put it: the focus is the
// opening's still, and reads that draw the tree meanwhile end nothing. Read as somebody else's
// focus - neither nobody's nor where the landing found it - it ended the opening: a later read
// that drew the question's row again under another key left the focus on the page, and the
// frame asked for the row that had left it landed nowhere.
test("a focus on the question's own row while its landing waits is still the opening's", async () => {
  setViewport(1440);
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const frames = framesByHand();
  try {
    const tree = renderPageTree(node({ children: [question], id: KEY, type: "issue" }));
    const { container } = tree.view;
    const asked = rowsFor(container, "ask-1")[0];
    await act(async () => asked?.focus());
    expect(focusedRow(container), "the script's focus is not on the question's row").toBe("ask-1");

    // Another read draws the tree again, and a later one places the question under a node that
    // has arrived since, all before the landing's frame.
    tree.redraw(node({ children: [question], id: KEY, type: "issue" }));
    tree.redraw(
      node({
        children: [
          node({
            children: [question],
            held: 1,
            id: "comment-1",
            text: "A thread.",
            type: "comment",
          }),
        ],
        id: KEY,
        type: "issue",
      })
    );
    expect(rowsFor(container, "ask-1"), "the read drew the question's row nowhere").toHaveLength(1);
    expect(rowsFor(container, "ask-1")[0], "the read kept the question's row").not.toBe(asked);
    await frames.run();
    expect(
      focusedRow(container),
      "the question's row drawn again lost the focus: the focus on it while its landing waited ended the opening"
    ).toBe("ask-1");
  } finally {
    frames.restore();
  }
});

// A row the page takes the reader to of its own accord - the node an address names, the open
// question the page opens on, or the outline's first row where no question waits - takes the
// focus in the frame after the commit that draws it, not in that commit: a focus given there
// made the browser lay the whole new page out inside the reader's press, 831 ms of a 3.9 s press
// that changed the view on a large issue. It is focused once, without a scroll; the page's one
// rule places the node and the question after, and the first row stays where it stands. A read
// that draws a row above the first before the frame comes gives the focus to that row, and the
// row the frame was first asked for takes none. Before the frame, a key the reader presses keeps
// the focus where it is, even a key that moves none, and so does a script's focus on a control,
// which is no act of the reader's.
test("a row the page lands on takes the focus in the next frame, without a scroll, unless a key or a script's focus came first", async () => {
  setViewport(1440);
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const named = node({ id: "comment-2", text: "The second words.", type: "comment" });
  const question = node({
    id: "ask-1",
    question: true,
    text: "Which flag?",
    type: "ask",
    waiting: true,
  });
  const addressed = node({ children: [first, named], id: KEY, type: "issue" });
  const asking = node({ children: [first, question], id: KEY, type: "issue" });
  const landings = [
    ["the node an address names", addressed, [addressed, named], "comment-2"],
    ["the open question the page opens on", asking, undefined, "ask-1"],
    // The same issue with no address: no question waits on the reader, so the page opens on its
    // first row.
    ["the first row the page opens on", addressed, undefined, "comment-1"],
  ] as const;
  const realFocus = HTMLElement.prototype.focus;
  const focuses: { readonly element: HTMLElement; readonly options: FocusOptions | undefined }[] =
    [];
  const focus = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    focuses.push({ element: this, options });
    realFocus.call(this, options);
  });
  const focusesOf = (row: HTMLElement | undefined) =>
    focuses.filter(({ element }) => element === row).map(({ options }) => options);
  try {
    for (const [what, root, reveal, landsOn] of landings) {
      focuses.length = 0;
      const view = renderTree(root, reveal === undefined ? {} : { reveal });
      const row = rowsFor(view.container, landsOn)[0];
      expect(row, `${what} draws no row`).toBeDefined();
      expect(focusesOf(row), `${what} took the focus in the commit that drew it`).toEqual([]);
      expect(document.activeElement).toBe(document.body);

      await nextFrame();
      expect(focusedRow(view.container), `${what} has no focus after the next frame`).toBe(landsOn);
      expect(focusesOf(row), `${what} was focused with a scroll, or more than once`).toEqual([
        { preventScroll: true },
      ]);
      view.unmount();
    }

    // A read draws a row above the first before the frame: the frame focuses the row that is
    // first when it comes, and the row it was first asked for, still on the page, takes no focus.
    focuses.length = 0;
    const tree = renderPageTree(node({ children: [first], id: KEY, type: "issue" }));
    const { container } = tree.view;
    const asked = rowsFor(container, "comment-1")[0];
    tree.redraw(
      node({
        children: [node({ id: "comment-0", text: "Older words.", type: "comment" }), first],
        id: KEY,
        type: "issue",
      })
    );
    expect(asked?.isConnected, "the read took the first row it was asked for off the page").toBe(
      true
    );
    await nextFrame();
    expect(focusedRow(container), "the row first when the frame came has no focus").toBe(
      "comment-0"
    );
    expect(focusesOf(asked), "the row the frame was first asked for took the focus").toEqual([]);
    expect(focusesOf(rowsFor(container, "comment-0")[0])).toEqual([{ preventScroll: true }]);
    tree.view.unmount();

    // Before the frame, a key the reader presses with the focus on the page - Shift alone, which
    // moves no focus - or a script's focus on a control with no key pressed: each landing leaves
    // the focus where that left it.
    const { control } = controlOutside();
    const pressShift = () => {
      document.body.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Shift" }));
    };
    for (const [what, root, reveal, landsOn] of landings) {
      for (const [came, run, stays] of [
        ["a key that moves no focus", pressShift, document.body],
        ["a script's focus on a control", () => control.focus(), control],
      ] as const) {
        focuses.length = 0;
        const view = renderTree(root, reveal === undefined ? {} : { reveal });
        expect(document.activeElement).toBe(document.body);
        await act(async () => run());
        await nextFrame();
        expect(document.activeElement, `${what} took the focus from where ${came} left it`).toBe(
          stays
        );
        expect(
          focusesOf(rowsFor(view.container, landsOn)[0]),
          `${what} was focused after ${came}`
        ).toEqual([]);
        view.unmount();
        await act(async () => control.blur());
      }
    }
  } finally {
    focus.mockRestore();
  }
});

// Where no question waits on the reader the page opens on the outline's first row, whose body
// opens with the focus, and scrolls nothing - not even for a first row taller than the window,
// which the page's one rule (`identity-block.ts`'s `bringOnScreen`) would put at the top under
// the band, the page opening scrolled past its header (89 px at 1440 by 900 on a real issue of
// some 160 rows). The reader's first move places the row it moves to as every move does.
test("the first row the page opens on scrolls nothing, and the first j places the next row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  // happy-dom lays nothing out, so the rows' boxes are given: the first row 100 px down and
  // twice the window's height tall, the second under it, below the window's foot. Every other
  // box is empty.
  const tall = 2 * window.innerHeight;
  const second = { bottom: 100 + tall + 40, top: 100 + tall };
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const id = this.matches("[data-node-row]") ? this.dataset.nodeId : undefined;
    if (id === "comment-1") return new DOMRect(0, 100, 800, tall);
    if (id === "comment-2") return new DOMRect(0, second.top, 800, second.bottom - second.top);
    return new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const { container } = renderTree(root);
    await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
    expect(scrolled, "the first row's focus at load scrolled the page").not.toHaveBeenCalled();

    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(container)).toBe("comment-2"));
    expect(scrolled).toHaveBeenCalledTimes(1);
    // Scrolled by that much, the second row stands whole inside the window.
    const by = (scrolled.mock.calls[0]?.[0] as ScrollToOptions | undefined)?.top ?? 0;
    expect(second.top - by).toBeGreaterThanOrEqual(0);
    expect(second.bottom - by).toBeLessThanOrEqual(window.innerHeight);
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

// The tester's finding: what a key reports when it has nowhere to go is drawn at the row it was
// pressed on, and the reader's next move lets it go.
test("a key with nowhere to go says so at its row, until the next move", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard("w");
  await waitFor(() => expect(rowReport(view.container)).toBe(KEY_REPORTS.no_decision));
  const [first] = rowsFor(view.container, "comment-1");
  expect(first?.querySelector("[data-row-report]")).not.toBeNull();
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  expect(rowReport(view.container)).toBeUndefined();
});

/**
 * Presses `key` on the row of `on`, where the key has nowhere to go, at each of `widths` (1440 and
 * 390 px unless a key goes somewhere at one of them), and reads that it says `report` at that row
 * and nowhere else, moves the focus nowhere, and that the next key, `next`, which moves, lets the
 * report go. The tree is `twoCommentsTree`'s, which opens on its first row; `to` are the keys that
 * take the reader from there to `on`, none where the key is pressed there, and `zoomed` says that
 * they zoom, so the crumb line is waited for.
 */
async function expectNowhereToGo({
  key,
  next,
  on,
  report,
  to,
  widths = [1440, 390],
  zoomed = false,
}: {
  readonly key: string;
  readonly next: string;
  readonly on: string;
  readonly report: string;
  readonly to?: string;
  readonly widths?: readonly number[];
  readonly zoomed?: boolean;
}): Promise<void> {
  for (const width of widths) {
    setViewport(width);
    const user = userEvent.setup();
    const view = renderTree(twoCommentsTree());
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    if (to !== undefined) await user.keyboard(to);
    if (zoomed) {
      await waitFor(() => expect(view.container.querySelector("[data-crumb]")).not.toBeNull());
    }
    await waitFor(() => expect(focusedRow(view.container)).toBe(on));
    await user.keyboard(key);
    await waitFor(() =>
      expect(rowReport(view.container), `${key} on ${on} at ${width} px`).toBe(report)
    );
    expect(focusedRow(view.container), `${key} on ${on} at ${width} px moved the focus`).toBe(on);
    expect(
      view.container.querySelectorAll("[data-row-report]").length,
      `${key} at ${width} px said it at more than one place`
    ).toBe(1);
    expect(
      rowsFor(view.container, on)[0]?.querySelector("[data-row-report]"),
      `${key} at ${width} px said it away from the row it was pressed on`
    ).not.toBeNull();
    await user.keyboard(next);
    await waitFor(() => expect(focusedRow(view.container)).not.toBe(on));
    expect(rowReport(view.container), `the move after ${key} at ${width} px`).toBeUndefined();
    view.unmount();
  }
}

// Each key below moved nothing and said nothing: at 1440 and at 390 px, Home, `k` and Up on the
// first row, End, `j` and Down on the last, Right on a row with nothing under it, `.` on the row
// the tree is zoomed to already, and Left on a top-level row of the whole tree with nothing to
// fold. Each says so at the row it was pressed on, in the words of `KEY_REPORTS`, until the next
// key or move. The first comment's reply is drawn unfolded (R11), so the last row is two rows
// down from the first.
test("Home on the first row says it is the first row", async () => {
  await expectNowhereToGo({
    key: "{Home}",
    next: "j",
    on: "comment-1",
    report: KEY_REPORTS.first_row,
  });
});

test("k on the first row says it is the first row", async () => {
  await expectNowhereToGo({ key: "k", next: "j", on: "comment-1", report: KEY_REPORTS.first_row });
});

test("Up on the first row says it is the first row", async () => {
  await expectNowhereToGo({
    key: "{ArrowUp}",
    next: "j",
    on: "comment-1",
    report: KEY_REPORTS.first_row,
  });
});

test("End on the last row says it is the last row", async () => {
  await expectNowhereToGo({
    key: "{End}",
    next: "k",
    on: "comment-2",
    report: KEY_REPORTS.last_row,
    to: "{End}",
  });
});

test("j on the last row says it is the last row", async () => {
  await expectNowhereToGo({
    key: "j",
    next: "k",
    on: "comment-2",
    report: KEY_REPORTS.last_row,
    to: "jj",
  });
});

test("Down on the last row says it is the last row", async () => {
  await expectNowhereToGo({
    key: "{ArrowDown}",
    next: "k",
    on: "comment-2",
    report: KEY_REPORTS.last_row,
    to: "jj",
  });
});

test("Right on a row with nothing under it says so", async () => {
  await expectNowhereToGo({
    key: "{ArrowRight}",
    next: "k",
    on: "comment-2",
    report: KEY_REPORTS.nothing_under,
    to: "jj",
  });
});

test("Left on a top-level row of the whole tree with nothing to fold says the whole issue is drawn", async () => {
  await expectNowhereToGo({
    key: "{ArrowLeft}",
    next: "k",
    on: "comment-2",
    report: KEY_REPORTS.whole_tree,
    to: "jj",
  });
});

test(". on the row the tree is zoomed to says the tree is zoomed to it already", async () => {
  await expectNowhereToGo({
    key: ".",
    next: "j",
    on: "comment-1",
    report: KEY_REPORTS.already_zoomed,
    to: ".",
    zoomed: true,
  });
});

// Where the whole tree is drawn, Left on the row the tree is zoomed to folded nothing, since the
// zoom's own row stays open, and moved nowhere, since its parent is above the zoom: it said
// nothing. At 390 px the same key zooms back out, so the case is read at 1440 px alone.
test("Left on the row the tree is zoomed to, the whole tree drawn, says the tree is zoomed to it already", async () => {
  await expectNowhereToGo({
    key: "{ArrowLeft}",
    next: "j",
    on: "comment-1",
    report: KEY_REPORTS.already_zoomed,
    to: ".",
    widths: [1440],
    zoomed: true,
  });
});

// A zoom the drawn tree no longer holds - the reader zoomed into a row, then a filter left that
// row out, or a read dropped it - is kept for when the row comes back, while the rows are drawn
// from the issue. Left and `,` read that zoom rather than the tree drawn, so on a top-level row
// they moved nothing and said nothing. They say the whole issue is drawn, as on a tree that was
// never zoomed. The case drops the row with a read, which reaches both widths.
test("Left and , on a tree that no longer holds its zoom say the whole issue is drawn", async () => {
  for (const width of [1440, 390]) {
    setViewport(width);
    const user = userEvent.setup();
    const view = renderTree(twoCommentsTree());
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard(".");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).not.toBeNull());
    view.reread(
      node({
        children: [node({ id: "comment-2", text: "The second words.", type: "comment" })],
        id: KEY,
        type: "issue",
      })
    );
    await waitFor(() => expect(rowsFor(view.container, "comment-1").length).toBe(0));
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
    for (const key of ["{ArrowLeft}", ","]) {
      await user.keyboard(key);
      await waitFor(() =>
        expect(rowReport(view.container), `${key} at ${width} px`).toBe(KEY_REPORTS.whole_tree)
      );
      expect(focusedRow(view.container), `${key} at ${width} px moved the focus`).toBe("comment-2");
      await user.keyboard("k");
    }
    view.unmount();
  }
});

/** An issue of two comments, each with a reply under it, so each holds a branch to fold. */
function twoBranchesTree(): TreeNode {
  return node({
    children: [
      node({
        children: [node({ id: "reply-1", text: "Under the first.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({
        children: [node({ id: "reply-2", text: "Under the second.", type: "comment" })],
        held: 1,
        id: "comment-2",
        text: "The second words.",
        type: "comment",
      }),
    ],
    id: KEY,
    type: "issue",
  });
}

// R11: nothing opens folded, and the reader folds a node that holds nodes by its fold control,
// by Left or by `o`, and unfolds it by the control, by Right or by `o`; a folded row draws its
// first line and how many nodes it holds, and `z` unfolds every node the reader folded, with the
// focus where it was.
test("the reader folds a node by its control, Left or o, unfolds it by the control, Right or o, and z unfolds every fold", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(twoBranchesTree());
  const { container } = view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  expect(rowsFor(container, "reply-1")).toHaveLength(1);
  expect(rowsFor(container, "reply-2")).toHaveLength(1);
  const expanded = (id: string) => rowsFor(container, id)[0]?.getAttribute("aria-expanded");

  // Left folds the focused row in place; Right unfolds it.
  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(rowsFor(container, "reply-1")).toHaveLength(0));
  expect(focusedRow(container), "Left moved off the row it folded").toBe("comment-1");
  expect(expanded("comment-1")).toBe("false");
  expect(rowsFor(container, "comment-1")[0]?.querySelector("[data-node-count]")?.textContent).toBe(
    `1 ${CONTROL_LABELS.folded_row}`
  );
  await user.keyboard("{ArrowRight}");
  await waitFor(() => expect(rowsFor(container, "reply-1")).toHaveLength(1));
  expect(focusedRow(container), "Right moved off the row it unfolded").toBe("comment-1");

  // `o` folds the focused row and unfolds it again.
  await user.keyboard("o");
  await waitFor(() => expect(expanded("comment-1")).toBe("false"));
  await user.keyboard("o");
  await waitFor(() => expect(expanded("comment-1")).toBe("true"));

  // The fold control folds a row the focus is not on, and leaves the focus where it is.
  const control = rowsFor(container, "comment-2")[0]?.querySelector<HTMLElement>(
    "button[data-node-bullet]"
  );
  expect(control?.getAttribute("aria-label")).toBe(CONTROL_LABELS.fold);
  await user.click(control as HTMLElement);
  await waitFor(() => expect(rowsFor(container, "reply-2")).toHaveLength(0));
  expect(focusedRow(container)).toBe("comment-1");

  // `z` unfolds every node the reader folded, and moves no focus.
  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(rowsFor(container, "reply-1")).toHaveLength(0));
  await user.keyboard("z");
  await waitFor(() => expect(rowsFor(container, "reply-1")).toHaveLength(1));
  expect(rowsFor(container, "reply-2")).toHaveLength(1);
  expect(focusedRow(container)).toBe("comment-1");
  view.unmount();
});

// A key that goes to a node under a branch the reader folded unfolds that branch, and the node
// takes the focus.
test("q to a question under a branch the reader folded opens that branch and lands on the question", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(
    node({
      children: [
        node({
          children: [
            node({ id: "ask-1", question: true, text: "Which flag?", type: "ask", waiting: true }),
          ],
          id: "comment-1",
          text: "The first words.",
          type: "comment",
        }),
        node({ id: "comment-2", text: "The second words.", type: "comment" }),
      ],
      id: KEY,
      type: "issue",
    })
  );
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-1"));
  await user.keyboard("k");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(rowsFor(view.container, "ask-1").length).toBe(0));
  await user.keyboard("q");
  await waitFor(() =>
    expect(focusedRow(view.container), "q found no row for the question").toBe("ask-1")
  );
  expect(rowReport(view.container)).toBeUndefined();
  view.unmount();
});

// The row the tree is zoomed to is drawn open by definition, so its Fold control drew nothing at
// any width, and wrote a fold that showed only after the zoom out. That row draws no fold control,
// while its treeitem still says it is expanded; zoomed out, the row has its fold control again.
test("the row the tree is zoomed to draws no fold control and stays expanded, and gets it back out of the zoom", async () => {
  for (const width of [1440, 390]) {
    setViewport(width);
    const user = userEvent.setup();
    const view = renderTree(twoBranchesTree());
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const control = () =>
      rowsFor(view.container, "comment-1")[0]?.querySelector("button[data-node-bullet]") ?? null;
    expect(
      control(),
      `comment-1 draws no fold control before the zoom at ${width} px`
    ).not.toBeNull();
    await user.keyboard(".");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).not.toBeNull());
    await waitFor(() => expect(rowsFor(view.container, "reply-1").length).toBe(1));
    expect(control(), `the zoom's own row draws a fold control at ${width} px`).toBeNull();
    expect(
      rowsFor(view.container, "comment-1")[0]?.getAttribute("aria-expanded"),
      `the zoom's own row does not say it is expanded at ${width} px`
    ).toBe("true");
    await user.keyboard(",");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
    await waitFor(() =>
      expect(
        control(),
        `comment-1 lost its fold control out of the zoom at ${width} px`
      ).not.toBeNull()
    );
    view.unmount();
  }
});

/** The tree of `twoBranchesTree` with the address naming `reply-1`, which is drawn by the path
 *  the address holds open, `comment-1` folded in the first state. `reply-1` holds a row of its
 *  own, so the path's last branch is one a reader can fold too. */
function renderAddressedBranches(): ReturnType<typeof renderTree> {
  const root = node({
    children: [
      node({
        children: [
          node({
            children: [node({ id: "reply-1a", text: "Under the reply.", type: "comment" })],
            id: "reply-1",
            text: "Under the first.",
            type: "comment",
          }),
        ],
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({
        children: [node({ id: "reply-2", text: "Under the second.", type: "comment" })],
        id: "comment-2",
        text: "The second words.",
        type: "comment",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const first = root.children[0] as TreeNode;
  return renderTree(root, { reveal: [root, first, first.children[0] as TreeNode] });
}

// An address's own path is held open to land the reader on its row. A branch of it the reader
// folded after the landing was held open still: Left at 1440 px, or the row's Fold control,
// recorded a fold that drew nothing, and Left said nothing, until the address named another
// node. The fold lets that branch, and the path below it, go of the hold, so it folds at once.
test("Left on a branch of the address's own path folds it at once after the landing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderAddressedBranches();
  await waitFor(() => expect(focusedRow(view.container)).toBe("reply-1"));
  await user.keyboard("k");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard("{ArrowLeft}");
  await waitFor(() =>
    expect(
      rowsFor(view.container, "reply-1").length,
      "Left drew the branch the address goes through still open"
    ).toBe(0)
  );
  expect(rowsFor(view.container, "comment-1")[0]?.getAttribute("aria-expanded")).toBe("false");
  expect(focusedRow(view.container), "Left moved off the row it folded").toBe("comment-1");
  view.unmount();
});

// The address's own node holds its path open until the reader folds it: its Fold control folds
// it at once, at 390 px as at every width (R11).
test("the Fold control of the address's own node folds it at once at 390 px", async () => {
  setViewport(390);
  const user = userEvent.setup();
  const view = renderAddressedBranches();
  await waitFor(() => expect(focusedRow(view.container)).toBe("reply-1"));
  await waitFor(() => expect(rowsFor(view.container, "reply-1a").length).toBe(1));
  const control = rowsFor(view.container, "reply-1")[0]?.querySelector<HTMLElement>(
    "button[data-node-bullet]"
  );
  expect(control?.getAttribute("aria-label")).toBe(CONTROL_LABELS.fold);
  await user.click(control as HTMLElement);
  await waitFor(() =>
    expect(
      rowsFor(view.container, "reply-1a").length,
      "the Fold control drew the address's own node still open"
    ).toBe(0)
  );
  expect(rowsFor(view.container, "reply-1")[0]?.getAttribute("aria-expanded")).toBe("false");
  view.unmount();
});

// `q` and `w` each work out where they go from the tree as it stands, so pressed before the
// reads the tree is built from have answered, the absence they say may be false - the question
// may be in the asks' read still out. Until those reads hold an answer (`slots/body.tsx`) the key
// says nothing, and once they do it says there is none.
test("q and w say there is nowhere to go only once the reads the tree is built from are held", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  for (const [key, report, says] of [
    ["q", KEY_REPORTS.no_next_question, "there is no next question"],
    ["w", KEY_REPORTS.no_decision, "no decision is behind the row"],
  ] as const) {
    const view = renderTree(root, { readsHeld: false });
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    // `user.keyboard` answers once the render its keys made has been drawn.
    await user.keyboard(key);
    expect(focusedRow(view.container), `${key} moved with nowhere to go`).toBe("comment-1");
    expect(
      rowReport(view.container),
      `${key} said ${says} before the tree's reads held an answer`
    ).toBeUndefined();

    view.holdReads();
    await user.keyboard(key);
    await waitFor(() => expect(rowReport(view.container)).toBe(report));
    view.unmount();
  }
});

// The reviewer's finding on the Contents pane's asks: an ask is acted on once, whatever draws
// the tree again in between - a view drawn in its place and turned off mounts a new tree with
// the page's last ask still in hand (`outline-request.tsx`), and that ask must not move the
// reader a second time.
test("an ask of the Contents pane is acted on once, across the tree being drawn again", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root);
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  await tree.ask("comment-2", false);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-2"));
  await tree.show(false);
  await tree.show(true);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
});

// The Contents list asks the outline through the page's provider, and each call is a request of
// its own - to a node in the whole outline, or, with Focus branch on, to that node's branch
// alone.
test("a request sent with useAskOutline takes the outline to a node, or to its branch alone", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-1a", text: "Under the first.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root);
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));

  await tree.ask("comment-2", false);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-2"));

  // Its branch alone: the outline is rooted at the node, its own row first, and the rest of
  // the issue is the breadcrumb above it.
  await tree.ask("comment-1", true);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  expect(rowsFor(container, "comment-2")).toHaveLength(0);
  expect(container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull();

  // And back to the whole outline.
  await tree.ask("comment-2", false);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-2"));
  expect(container.querySelector("[data-crumb]")).toBeNull();
});

// Focus branch, the Contents pane's toggle, is the page's, held above the outline by
// `outline-request.tsx`'s provider, and it outlives a view drawn in the outline's place, where
// the outline's zoom did not: the outline drawn again as the view is turned off is a new one,
// and the request that zoomed the one before was acted on already, so it drew every branch while
// the toggle read on. Drawn again while Focus branch is on, the outline draws the last request's
// branch alone, as the outline before it did, and takes the reader nowhere: it is no new
// request, and the focus stays where the view left it. Focus branch turned off, as the Contents
// list turns it, draws the whole outline again.
test("an outline drawn again while Focus branch is on draws the last request's branch alone", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-1a", text: "Under the first.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root);
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));

  // The Contents list's toggle: Focus branch on, and the outline sent to the entry's branch.
  await tree.focusBranch(true);
  await tree.ask("comment-1", true);
  await waitFor(() => expect(rowsFor(container, "comment-2")).toHaveLength(0));
  const branch = [...container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
    (row) => row.dataset.nodeId
  );

  // A view drawn in the outline's place, and turned off from a control outside the outline.
  const control = document.createElement("button");
  document.body.append(control);
  try {
    await tree.show(false);
    await act(async () => control.focus());
    await tree.show(true);
    expect(
      [...container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
        (row) => row.dataset.nodeId
      )
    ).toEqual(branch);
    expect(rowsFor(container, "comment-2")).toHaveLength(0);
    expect(container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull();
    expect(document.activeElement, "drawing the outline again moved the focus").toBe(control);

    await tree.focusBranch(false);
    await tree.ask("comment-1", false);
    await waitFor(() => expect(rowsFor(container, "comment-2")).toHaveLength(1));
    expect(container.querySelector("[data-crumb]")).toBeNull();
  } finally {
    control.remove();
  }
});

// The same outline drawn again where nothing holds the focus: the opening places it, but this is
// no page opening on what waits on the reader, so it does not go to a question outside the
// branch, which would leave the zoom and draw every branch while the toggle read on. It puts the
// focus on the branch's first row, the node Focus branch is on, where it stands.
test("an outline drawn again on Focus branch's node opens on that node, not on a question outside it", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-1a", text: "Under the first.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: "The first words.",
        type: "comment",
      }),
      node({ id: "ask-1", question: true, text: "Which flag?", type: "ask", waiting: true }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root);
  const { container } = tree.view;
  // The page opens on the question that waits on the reader.
  await waitFor(() => expect(focusedRow(container)).toBe("ask-1"));
  await tree.focusBranch(true);
  await tree.ask("comment-1", true);
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  expect(rowsFor(container, "ask-1")).toHaveLength(0);

  await tree.show(false);
  await act(async () => (document.activeElement as HTMLElement | null)?.blur());
  expect(document.activeElement).toBe(document.body);
  await tree.show(true);
  await waitFor(() =>
    expect(focusedRow(container), "the outline drawn again opened on another row").toBe("comment-1")
  );
  expect(
    [...container.querySelectorAll<HTMLElement>("[data-node-row]")].map((row) => row.dataset.nodeId)
  ).toEqual(["comment-1", "comment-1a"]);
  expect(rowsFor(container, "ask-1")).toHaveLength(0);
  expect(container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull();
});

// The fold's half of a finding of the tester's: a reply the reader wrote under a branch they
// folded is not left behind the fold. The writer's landing names what the write was made under;
// once the read that holds the new node draws it, the branch unfolds and the reader's new row
// takes the focus.
test("a write that lands under a folded branch unfolds it and gives the reader's new row the focus", async () => {
  setViewport(1440);
  const thread = (replies: TreeNode[]) =>
    node({
      children: [
        node({
          children: replies,
          held: replies.length,
          id: "comment-1",
          text: "A thread.",
          type: "comment",
        }),
        node({ id: "comment-9", text: "After the thread.", type: "comment" }),
      ],
      id: KEY,
      type: "issue",
    });
  const theirs = node({
    author: { id: "bob", kind: "user" },
    id: "comment-2",
    text: "Bob's reply.",
    type: "comment",
  });
  const mine = node({
    at: "2026-11-16T09:00:00Z",
    author: { id: "alice", kind: "user" },
    id: "comment-3",
    text: "My reply.",
    type: "comment",
  });
  const tree = renderPageTree(thread([theirs]), "alice");
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  // Nothing opens folded (R11), so the reader folds the thread.
  await userEvent.setup().keyboard("{ArrowLeft}");
  await waitFor(() => expect(rowsFor(container, "comment-2")).toHaveLength(0));

  await tree.land("comment-1");
  tree.redraw(thread([theirs, mine]));
  await waitFor(() => expect(focusedRow(container)).toBe("comment-3"));
  expect(rowsFor(container, "comment-2")).toHaveLength(1);
  expect(tree.writer()?.landing).toBeUndefined();
});

// The reviewer's finding on the landing: one the writer already holds as the tree mounts was
// asked for while no outline was drawn - under a view drawn in the tree's place, or before a
// reload - and the reader has been elsewhere since. Drawing the outline ends it and takes them
// nowhere: not to the row an answer or an edit was made on, which a landing reaches at once,
// and not to their newest node once the late fallback's wait is out.
test("a landing made while the outline was not drawn moves no focus when the outline is drawn", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({
        children: [
          node({
            at: "2026-11-16T09:00:00Z",
            author: { id: "alice", kind: "user" },
            id: "comment-3",
            text: "My reply.",
            type: "comment",
          }),
        ],
        held: 1,
        id: "comment-2",
        text: "A thread.",
        type: "comment",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root, "alice");
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));

  // An answer or an edit, which lands on the row it was made on.
  await tree.show(false);
  await tree.land("comment-2", "self");
  await tree.show(true);
  expect(focusedRow(container)).not.toBe("comment-2");
  expect(tree.writer()?.landing).toBeUndefined();
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));

  // A reply, which lands on the reader's newest node under what it answered: here the late
  // fallback's, since no read draws a new one.
  await tree.show(false);
  await tree.land("comment-2");
  jest.useFakeTimers();
  try {
    await tree.show(true);
    expect(tree.writer()?.landing).toBeUndefined();
    await jump(LANDING_WAIT_MS);
    expect(focusedRow(container)).toBe("comment-1");
  } finally {
    realClock();
  }
});

// The late fallback takes the reader to their newest node where no read draws the new one, and
// only while the focus is still the page's to place: a key, a press, a wheel or a touch after
// the landing is the reader going somewhere of their own, and the fallback leaves them there.
test("a landing's late fallback leaves the focus where the reader moved it", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root, "alice");
  const { container } = tree.view;
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    // A reply under the first comment, whose node no read draws.
    await tree.land("comment-1");
    await user.keyboard("j");
    expect(focusedRow(container)).toBe("comment-2");
    await jump(LANDING_WAIT_MS);
    expect(focusedRow(container)).toBe("comment-2");
    expect(tree.writer()?.landing).toBeUndefined();
  } finally {
    realClock();
  }
});

// `r` sets the line at the foot to reply to the focused row, and `a` sets it to answer the
// focused open question (`useTreeKeymap.ts`'s `tree-reply` and `tree-answer`). A comment takes
// no answer, so `a` on one leaves the line as it was and says nothing at the row.
test("r and a on the focused row set the page's writer", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const bob = { id: "bob", kind: "user" } as const;
  const root = node({
    children: [
      node({
        author: bob,
        id: "comment-1",
        reference: buildDispatchReference({ id: "comment-1", key: KEY, kind: "comment" }),
        text: "The first words.",
        type: "comment",
      }),
      node({
        author: bob,
        id: "ask-1",
        question: true,
        reference: buildDispatchReference({ id: "ask-1", key: KEY, kind: "ask" }),
        state: "open",
        text: "Which flag?",
        type: "ask",
        waiting: true,
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const tree = renderPageTree(root, "alice");
  const { container } = tree.view;
  // The page opens on the question that waits on the reader.
  await waitFor(() => expect(focusedRow(container)).toBe("ask-1"));

  await user.keyboard("k");
  await waitFor(() => expect(focusedRow(container)).toBe("comment-1"));
  await user.keyboard("r");
  await waitFor(() =>
    expect(tree.writer()?.reply).toMatchObject({
      actor: bob,
      excerpt: "The first words.",
      id: "comment-1",
      parentKind: "comment",
      to: buildIssuePath({ id: "comment-1", key: KEY, kind: "comment" }),
    })
  );
  expect(tree.writer()?.answer).toBeUndefined();

  const reply = tree.writer()?.reply;
  await user.keyboard("a");
  expect(tree.writer()?.reply).toBe(reply);
  expect(tree.writer()?.answer).toBeUndefined();
  expect(rowReport(container)).toBeUndefined();

  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(container)).toBe("ask-1"));
  await user.keyboard("a");
  await waitFor(() =>
    expect(tree.writer()?.answer).toEqual({
      askId: "ask-1",
      multiple: false,
      nodeId: "ask-1",
      selected: [],
    })
  );
  expect(tree.writer()?.reply).toBeNull();
});

// The architect's ruling on a node's one line: a crumb draws one line for a node, so it is the
// node's first line (`firstLineOf`), cut at a word, never cut again mid-word by its box; the
// issue's crumb is its key.
test("a crumb is its node's first line cut at a word, and the issue's is its key", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const long =
    "A first sentence that runs on well past the hundred characters a first line may hold so it is cut at a word boundary";
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-2", text: "Under it.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: long,
        type: "comment",
      }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard(".");
  await waitFor(() => expect(rowsFor(view.container, "comment-2")).toHaveLength(1));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  await user.keyboard(".");
  const crumbs = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("[data-crumb]")];
    if (found.length < 2) throw new Error("the breadcrumb holds no crumb of the comment");
    return found;
  });
  expect(crumbs[0]?.textContent).toBe(KEY);
  const crumb = crumbs[1]?.textContent ?? "";
  expect(crumb.endsWith("…")).toBe(true);
  expect(long.startsWith(crumb.slice(0, -1))).toBe(true);
  expect(long.charAt(crumb.length - 1)).toBe(" ");
  expect(crumbs[1]?.className).not.toContain("truncate");
});

// No node is named by its id. A crumb of a node with no words of its own, a plan task drawn
// with no text, names it by its kind's word.
test("a crumb of a node with no words is its kind's word, not its id", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-2", text: "Under it.", type: "comment" })],
        held: 1,
        id: "plan#b-t1",
        text: "",
        type: "plan_task",
      }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("plan#b-t1"));
  await user.keyboard(".");
  await waitFor(() => expect(rowsFor(view.container, "comment-2")).toHaveLength(1));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  await user.keyboard(".");
  const crumbs = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("[data-crumb]")];
    if (found.length < 2) throw new Error("the breadcrumb holds no crumb of the plan task");
    return found;
  });
  expect(crumbs[1]?.textContent).toBe(NODE_TYPE_NAMES.plan_task);
});

// The focused node's title in the panel and a crumb each draw one line for a node, cut from
// `lineWords`, so a comment whose words are only a bare address is named by that address's short
// form, as a link to it draws it, and never by the address as written.
test("a node whose words are only a bare address is named by its short form, in the panel and in its crumb", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const address = `dispatch://${KEY}/spec#b-r1`;
  const short = addressesAsShortForms(address);
  expect(short, "the address has no short form to name the node by").not.toContain("dispatch://");
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-2", text: "Under it.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: address,
        type: "comment",
      }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  expect(view.focusedNode()?.title, "the panel names the node by its address as written").toBe(
    short
  );
  await user.keyboard(".");
  await waitFor(() => expect(rowsFor(view.container, "comment-2")).toHaveLength(1));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  await user.keyboard(".");
  const crumbs = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("[data-crumb]")];
    if (found.length < 2) throw new Error("the breadcrumb holds no crumb of the comment");
    return found;
  });
  expect(crumbs[1]?.textContent, "the crumb names the node by its address as written").toBe(short);
});

// R2 and R3 of dispatch://INFRA-1523: a crumb draws a node's one line where its own row is not
// drawn, so it holds a corrected post's marks, as a filter's parent line does (`ParentLink`): a
// deleted comment is drawn as its deleted line, from the delete, never as the model's text holds
// it with the time as stored; an edited one says ` · edited` after its line.
test("a crumb of a deleted comment draws its deleted line, and an edited one's says edited", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const deletedAt = "2026-11-05T12:00:00Z";
  const root = node({
    children: [
      node({
        children: [
          node({
            children: [node({ id: "comment-3", text: "Under it.", type: "comment" })],
            editedAt: "2026-11-05T13:00:00Z",
            held: 1,
            id: "comment-2",
            text: "Ship it.",
            type: "comment",
          }),
        ],
        deletedAt,
        deletedBy: { id: "alice", kind: "user" },
        held: 2,
        id: "comment-1",
        text: `Deleted by alice · ${deletedAt}`,
        type: "comment",
      }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  for (const next of ["comment-2", "comment-3"]) {
    await user.keyboard(".");
    await waitFor(() => expect(rowsFor(view.container, next)).toHaveLength(1));
    await user.keyboard("j");
    await waitFor(() => expect(focusedRow(view.container)).toBe(next));
  }
  await user.keyboard(".");
  const crumbs = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("[data-crumb]")];
    if (found.length < 3) throw new Error("the breadcrumb holds no crumb of both comments");
    return found;
  });
  const deleted = crumbs[1];
  expect({
    deletedLine: deleted
      ?.querySelector("[data-post-deleted]")
      ?.textContent?.startsWith("Deleted by alice · "),
    edited: crumbs[2]?.textContent,
    storedTime: deleted?.textContent?.includes(deletedAt),
  }).toEqual({ deletedLine: true, edited: "Ship it. · edited", storedTime: false });
});

/** Seeds `client` with the reader and every read the body's tree is built from (`slots/body.tsx`),
 *  each answered empty, but the comments', which answer `comments` and are left unread where
 *  none are given, and the tree read, which holds the issue, `nodes` and each of `comments` as a
 *  node (`treeReadOf`), since the outline draws its rows from the tree read. */
function seedBodyReads(
  client: QueryClient,
  {
    comments,
    nodes = [],
  }: { readonly comments?: readonly Comment[]; readonly nodes?: readonly TreeAnswerNode[] } = {}
): void {
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, { requirements: [] } as never);
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [] } as never);
  client.setQueryData(["decisions", KEY], decisionsRead([]));
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  if (comments !== undefined) client.setQueryData(["comments", KEY], comments);
  client.setQueryData(["issue-messages", KEY], []);
  client.setQueryData(issueTreeQuery(KEY).queryKey, treeReadOf(comments ?? [], nodes));
  client.setQueryData(
    outlineQuery(KEY, "conversation").queryKey,
    treeReadOf(comments ?? [], nodes)
  );
}

/** The tree read's answer for an issue that holds `comments`, `nodes` and nothing else, as
 *  `issue-tree.test.ts` answers a comments read: `nodes` under the issue unless one names another
 *  parent, then each comment a node under the comment it replies to, else under the issue, and
 *  none of a thread only agents wrote in, which the read places nowhere (`agents-only.ts`). With
 *  no such node the answer holds no node at all. */
function treeReadOf(
  comments: readonly Comment[],
  nodes: readonly TreeAnswerNode[] = []
): TreeAnswer {
  const agentsOnly = agentsOnlyIds(comments, undefined, []);
  const turns = comments
    .filter((comment) => !agentsOnly.has(comment.id))
    .map((comment) => commentNode(comment));
  const held = [...nodes, ...turns];
  return treeAnswer(held.length === 0 ? [] : [treeNode(KEY, "issue", null), ...held]);
}

/** A question as the tree read answers it: a node under the issue, with its words and who asked
 *  it when. How it stands - open, answered, whose turn it is - is the asks read's to say, or the
 *  issue read's for an open ask it holds (`open_asks`). */
function questionNode(ask: {
  readonly author: Comment["author"];
  readonly created_at: string;
  readonly id: string;
  readonly question: string;
}): TreeAnswerNode {
  return treeNode(ask.id, "ask", KEY, {
    author: [ask.author],
    made_at: ask.created_at,
    text: ask.question,
  });
}

// The reviewer's finding on the outline's empty line: a tree with no node is also one whose reads
// are still out - the asks, the comments, the messages, the decisions, the coverage, the results
// and the documents' blocks each draw nodes once they answer, and only the children come with the
// issue - so the line that the issue holds nothing was drawn on every cold load of an issue with
// no children, and rows then drawn in its place. The body draws it only once those reads have
// answered, and nothing in its place before. Here every read has answered but the comments'.
test("the outline says the issue holds nothing only once the reads its nodes come from have answered", async () => {
  setViewport(1440);
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const route = { key: KEY, kind: "issue" } as const;
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  seedBodyReads(client);
  // The comments' read, held out until the case answers it.
  const held = Promise.withResolvers<never[]>();
  const comments = spyOn(api, "listComments").mockReturnValue(held.promise as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const view = render(
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <ProvidersSlot issue={issue} route={route}>
                <BodySlot issue={issue} route={route} />
              </ProvidersSlot>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
    await waitFor(() => expect(comments).toHaveBeenCalled());
    expect(
      view.container.textContent,
      "the outline said the issue holds nothing with its comments' read still out"
    ).not.toContain(EMPTY_STATE_LINES.conversation);

    await act(async () => held.resolve([]));
    await waitFor(() =>
      expect(
        view.container.textContent,
        "the outline does not say the issue holds nothing once every read has answered empty"
      ).toContain(EMPTY_STATE_LINES.conversation)
    );
  } finally {
    comments.mockRestore();
    names.mockRestore();
  }
});

/** The address of the spec's one block, `t1`, that the tree read answers (`specBlock`); its node
 *  is `spec#b-t1`. The names read is not given it, as it is given no node's own address: it
 *  names the issue's own key, which the root's words write. */
const SPEC_BLOCK = buildDispatchReference({
  block: { id: "t1" },
  key: KEY,
  kind: "artifact",
  slug: "spec",
});

/** The spec's one block, `t1` (`SPEC_BLOCK`), as the tree read answers it: a block of `supertag`
 *  in the spec, with `words` as its words, as the read answers every block's. */
function specBlock(supertag: string, words: string): TreeAnswerNode {
  return treeNode("t1", supertag, KEY, {
    document: { artifact_id: specDocument.id, position: 0, slug: specDocument.slug },
    text: words,
  });
}

/** The spec's one block as a step of the spec (`specBlock`). */
function specStep(words: string): TreeAnswerNode {
  return specBlock("plan_task", words);
}

/** The step's words (`specStep`), which hold the words filter's `lanterns`. */
const STEP_WORDS = "Hang the lanterns first.";

// Under a words filter a plan task's or a callout's words came from the names read alone, so
// until that read answered such a node matched nothing: on a cold load of an address with `?q=`
// whose words only such nodes hold, the outline said that nothing holds the words, and then drew
// the matching rows in the line's place. Under one the body draws that line only once the names
// are held, and nothing in its place before. The tree read now answers the step with its words;
// the names read is not given its address, and names the issue's own key, which the root's words
// write. The step is drawn in Graph, the outline that draws a plan task as a row (R9, R10).
test("under a words filter the outline does not say nothing holds the words while the names read is out, and draws the rows that match them once it has answered", async () => {
  setViewport(1440);
  // The names read, held out until the case answers it with the step's words.
  const held = Promise.withResolvers<never>();
  const names = spyOn(api, "getReferenceTargets").mockReturnValue(held.promise);
  try {
    const { view } = renderAddressedBody(
      `${buildIssuePath({ key: KEY, kind: "graph" })}?q=lanterns`,
      [],
      BodySlot,
      { issue: issueDetails(), nodes: [specStep(STEP_WORDS)] }
    );
    // The names are read once every other read of the tree has answered.
    await waitFor(() => expect(names, "the names were never read").toHaveBeenCalled());
    await nextFrame();
    expect(
      view.container.textContent,
      "the outline said nothing holds the words with the names read still out"
    ).not.toContain(EMPTY_STATE_LINES.no_match);

    await act(async () =>
      held.resolve({
        targets: [{ gone: false, kind: "block", name: STEP_WORDS, ref: SPEC_BLOCK }],
      } as never)
    );
    await waitFor(() =>
      expect(
        rowsFor(view.container, "spec#b-t1"),
        "the step its words match was not drawn once the names read had answered"
      ).toHaveLength(1)
    );
    expect(view.container.textContent).not.toContain(EMPTY_STATE_LINES.no_match);
  } finally {
    names.mockRestore();
  }
});

// And once the names are held the line is drawn where no node holds the words: the step's words,
// which the tree read gives - the names read is not given the step's address - do not hold the
// filter's word, so nothing does.
test("under a words filter the outline says nothing holds the words once the names read has answered with words that do not hold them", async () => {
  setViewport(1440);
  // The names read, held out until the case answers it with the step's words.
  const held = Promise.withResolvers<never>();
  const names = spyOn(api, "getReferenceTargets").mockReturnValue(held.promise);
  try {
    const { view } = renderAddressedBody(
      `${buildIssuePath({ key: KEY, kind: "issue" })}?q=lanterns`,
      [],
      BodySlot,
      { issue: issueDetails(), nodes: [specStep("Sweep the hall first.")] }
    );
    await waitFor(() => expect(names, "the names were never read").toHaveBeenCalled());
    await nextFrame();
    expect(
      view.container.textContent,
      "the outline said nothing holds the words with the names read still out"
    ).not.toContain(EMPTY_STATE_LINES.no_match);

    await act(async () =>
      held.resolve({
        targets: [{ gone: false, kind: "block", name: "Sweep the hall first.", ref: SPEC_BLOCK }],
      } as never)
    );
    await waitFor(() =>
      expect(
        view.container.textContent,
        "the outline did not say nothing holds the words once the names read had answered"
      ).toContain(EMPTY_STATE_LINES.no_match)
    );
    expect(
      rowsFor(view.container, "spec#b-t1"),
      "the step was drawn though its words do not hold the filter's"
    ).toHaveLength(0);
  } finally {
    names.mockRestore();
  }
});

// With no filter on the outline's line reads no words, so it does not wait on the names. The
// spec holds its summary alone, a callout, which is no row of the outline, so the issue holds
// nothing to draw, and the outline says so while the names read is still out. That read names
// the issue's own key, which the root's words write; it is not given the summary's address.
test("with no filter the outline says the issue holds nothing while the names read is out", async () => {
  setViewport(1440);
  // The names read, held out for the whole case.
  const names = spyOn(api, "getReferenceTargets").mockReturnValue(
    Promise.withResolvers<never>().promise
  );
  try {
    const { client, view } = renderAddressedBody(
      buildIssuePath({ key: KEY, kind: "issue" }),
      [],
      BodySlot,
      {
        issue: issueDetails(),
        nodes: [specBlock("callout", "What this is. Why it matters. What is different when done.")],
      }
    );
    await waitFor(() => expect(names, "the names were never read").toHaveBeenCalled());
    await waitFor(() =>
      expect(
        view.container.textContent,
        "the outline did not say the issue holds nothing with no filter on and the names read out"
      ).toContain(EMPTY_STATE_LINES.conversation)
    );
    expect(
      client.isFetching({ queryKey: ["reference-targets"] }),
      "the names read is not out"
    ).toBe(1);
  } finally {
    names.mockRestore();
  }
});

// The line that says the issue holds nothing is the page's to give (`slots/body.tsx`), and it
// is drawn after the tree, where the issue holds nothing and the reads are held. A tree given no
// line draws none there: no empty paragraph under the rows.
test("a tree given no empty line draws none for an issue that holds nothing", () => {
  setViewport(1440);
  const given = renderTree(node({ id: KEY, type: "issue" }), { emptyLine: "Nothing here." });
  expect(given.getByRole("tree").nextElementSibling?.textContent).toBe("Nothing here.");
  given.unmount();

  const none = renderTree(node({ id: KEY, type: "issue" }));
  expect(
    none.getByRole("tree").nextElementSibling,
    "a tree given no empty line drew one with no words"
  ).toBeNull();
});

// The page's stage mounts the outline's slot again on every press of a view, and under the app's
// `staleTime` a read is stale 30 s after it answered (`main.tsx`). The reads the outline waits on
// before it says the issue holds nothing are kept fresh by events, as the tree's own reads are
// (`useIssueTree.ts`): the slot mounted again more than 30 s on reads none of them again.
test("the outline mounted again past 30 s reads none of the reads it waits on again", async () => {
  setViewport(1440);
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const route = { key: KEY, kind: "issue" } as const;
  // The app's client's `staleTime` (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  seedBodyReads(client, { comments: [] });
  const reads = [
    spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsRead([])),
    spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([] as never),
    spyOn(api, "listComments").mockResolvedValue([] as never),
    spyOn(api, "listIssueMessages").mockResolvedValue([] as never),
    spyOn(api, "getIssueTree").mockResolvedValue(treeAnswer([])),
  ];
  const sent = () => reads.reduce((all, read) => all + read.mock.calls.length, 0);
  // The reader, whom the rows read with the tree's freshness, and the names, answered so that
  // neither reaches the network.
  const others = [
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
  ];
  const draw = (shown: boolean) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              {shown ? <BodySlot issue={issue} route={route} /> : null}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = render(draw(true));
    await waitFor(() =>
      expect(view.container.textContent).toContain(EMPTY_STATE_LINES.conversation)
    );
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const loaded = sent();
    view.rerender(draw(false));

    // A press of another view, more than 30 s after the load, mounts the slot again.
    setSystemTime(Date.now() + 31_000);
    view.rerender(draw(true));
    // Long enough for a read the mount started to have been sent.
    await waitInAct(50);
    expect(sent(), "the outline mounted again read again what it waits on").toBe(loaded);
  } finally {
    setSystemTime();
    for (const spy of [...reads, ...others]) spy.mockRestore();
  }
});

/** The page's body at the address the router holds, as the page's root draws it
 *  (`IssueOutlinePage.tsx`): the route read from the address at every render. `held.navigate`
 *  is what a link followed calls. `Slot` is what draws the address: the body, or the stage that
 *  chooses between the body and a view. */
function AddressedBody({
  held,
  issue,
  Slot,
}: {
  held: { navigate?: NavigateFunction };
  issue: IssueDetails;
  Slot: (props: SlotProps) => ReactNode;
}): ReactNode {
  const { pathname, search } = useLocation();
  held.navigate = useNavigate();
  const route = parseIssuePath(pathname, search) ?? ({ key: KEY, kind: "issue" } as const);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <IssueScope />
      <Slot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

/** The page's body opened at `path`, every read the tree is built from answered: the issue
 *  holds `comments`, the tree read's `nodes` - a question, a step - and nothing else, and its
 *  comments' read is left unread where `comments` is undefined, as is each read `unread` names by
 *  its query key. `follow` follows a link to an address, as a row's link or the router's does;
 *  `redraw` draws the same location again, as a read answering does, of `next` where it is
 *  given, as the issue's own read answering with a document added does; `reopen` takes the page
 *  off and draws it anew over the same reads, as a later visit to it does; `client` holds the
 *  reads. `Slot` draws the address, the body where none is given, of `issue`, which holds no
 *  document where none is given. */
function renderAddressedBody(
  path: string,
  comments: readonly Comment[] | undefined,
  Slot: (props: SlotProps) => ReactNode = BodySlot,
  {
    issue = { ...issueDetails(), artifacts: [] },
    nodes = [],
    unread = [],
  }: {
    readonly issue?: IssueDetails | undefined;
    readonly nodes?: readonly TreeAnswerNode[];
    readonly unread?: readonly QueryKey[];
  } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  seedBodyReads(client, { comments, nodes });
  for (const queryKey of unread) client.removeQueries({ queryKey });
  // The view reads a load of the stage sends with it, Plan's among them (`ViewReadProvider`),
  // answered with no node.
  answerRequests(
    http.get("/api/v1/issues/:key/tree", ({ request }) =>
      new URL(request.url).searchParams.has("view") ? Response.json(treeAnswer([])) : undefined
    )
  );
  const held: { navigate?: NavigateFunction } = {};
  let drawn = issue;
  const draw = () => (
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <AddressedBody held={held} issue={drawn} Slot={Slot} />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw());
  return {
    /** Goes back one entry, as the browser's Back does: the entry keeps its own location key. */
    back: () =>
      act(async () => {
        await held.navigate?.(-1);
      }),
    client,
    follow: (to: string) =>
      act(async () => {
        await held.navigate?.(to);
      }),
    redraw: (next: IssueDetails = drawn) => {
      drawn = next;
      view.rerender(draw());
    },
    reopen: () => {
      view.unmount();
      return render(draw());
    },
    view,
  };
}

// A fold of a branch on an address's own path lets it go of the hold for that following. The fold
// was kept under the following's location key, and Back gives an earlier entry's own key back:
// coming back to the address drew its node nowhere, folded under the branch. Following anything
// else forgets the fold, so Back holds the path again and draws the node.
test("Back to an address after a fold on its path draws its node again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS);
    const { container } = body.view;
    await waitFor(() => expect(focusedRow(container)).toBe("head-a"));
    await body.follow(buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" }));
    await waitFor(() => expect(focusedRow(container)).toBe("reply-b"));
    await user.keyboard("{ArrowLeft}");
    await waitFor(() => expect(focusedRow(container)).toBe("head-b"));
    await user.keyboard("{ArrowLeft}");
    await waitFor(() => expect(rowsFor(container, "reply-b").length).toBe(0));

    await body.follow(`/issues/${KEY}`);
    await body.back();
    await waitFor(() =>
      expect(
        rowsFor(container, "reply-b").length,
        "Back to the address drew its node nowhere, folded under the branch"
      ).toBe(1)
    );
    expect(rowsFor(container, "head-b")[0]?.getAttribute("aria-expanded")).toBe("true");
  } finally {
    names.mockRestore();
  }
});

// The architect's rule for an address of a node of the issue: following a link to it opens the
// branches above the node and puts the focus on its row. A zoom the reader held on another branch
// drew no row for the node, and the landing gave up with nothing said: the focus stayed where it
// was and the zoom stood. The following leaves a zoom the node is outside of.
test("a comment's address followed while the reader is zoomed into another branch leaves the zoom and lands on the comment's row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS);
    const { container } = body.view;
    await waitFor(() => expect(focusedRow(container)).toBe("head-a"));
    await user.keyboard(".");
    await waitFor(() => expect(container.querySelector("[data-crumb]")).not.toBeNull());
    expect(rowsFor(container, "reply-b"), "the zoom into head-a drew the other thread").toEqual([]);

    await body.follow(buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" }));
    await waitFor(() =>
      expect(
        focusedRow(container),
        "the comment's address, followed from a zoom into another branch, landed nowhere"
      ).toBe("reply-b")
    );
    expect(
      container.querySelector("[data-crumb]"),
      "the zoom into the other branch stood after the following"
    ).toBeNull();
  } finally {
    names.mockRestore();
  }
});

// The landing was keyed on the node alone, so a link to the node the reader had moved away from,
// followed again - the same address, a new following - landed nowhere. It is keyed on the
// following too (the location's own key), and a tree drawn again at the same following - a read
// answering - lands no more once the reader has moved.
test("the same address followed twice lands both times, and a redraw of it after the reader moved lands no more", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const address = buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" });
  try {
    const body = renderAddressedBody(address, TWO_THREADS);
    const { container } = body.view;
    await waitFor(() => expect(focusedRow(container)).toBe("reply-b"));
    await user.keyboard("k");
    await waitFor(() => expect(focusedRow(container)).not.toBe("reply-b"));
    const moved = focusedRow(container);
    expect(moved, "k left the focus on no row").toBeDefined();

    await body.follow(address);
    await waitFor(() =>
      expect(focusedRow(container), "the same address followed a second time landed nowhere").toBe(
        "reply-b"
      )
    );

    await user.keyboard("k");
    await waitFor(() => expect(focusedRow(container)).toBe(moved));
    body.redraw();
    await nextFrame();
    await nextFrame();
    expect(focusedRow(container), "a redraw of the same following landed on the node again").toBe(
      moved
    );
  } finally {
    names.mockRestore();
  }
});

/** An id that names no comment of the issue, which the item's own read answers 404 for. */
const GONE_COMMENT = "0b6e3f51-9c55-4d9e-8a3e-5b8cfa2c7d10";

/** The item's own read's answer for an id that names no comment of the issue. */
function noSuchComment(): ApiError {
  return new ApiError(404, { code: "NOT_FOUND", error: "no such comment" });
}

/** A read's answer that the server is down, which the reader can retry. */
function serverDown(): ApiError {
  return new ApiError(500, { code: "INTERNAL", error: "the server is down" });
}

// A node the tree does not hold falls back to the address's own page, never to the page's body,
// once every read the tree is built from has answered: for an id no item of the issue holds, the
// old page's landing reads the item and finds none (`missing`), and the page says so where the
// outline would be, with the focus on that line.
test("an address naming an item no node holds draws the item's failure once the reads are held, with the focus on its line", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(noSuchComment());
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "Comment not found" })
    );
    expect(read).toHaveBeenCalledWith(GONE_COMMENT);
    await waitFor(() =>
      expect(document.activeElement, "the failure's line does not hold the focus").toBe(heading)
    );
    expect(heading.parentElement?.textContent).toContain(
      `That link doesn't name a comment on ${KEY} — it may have been deleted.`
    );
    expect(body.view.queryByRole("tree"), "the outline is drawn beside the failure").toBeNull();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The class search of #4205's 7a922b76, on the class of the tester's names-line finding: the
// item's failed line left the page as a document added to the issue had its blocks read, which
// holds the tree's reads out again (`readsHeld`), and came back once that read answered, saying
// its failure again and taking the focus again. The following keeps the landing it settled on
// while the reads are out again: the line stays drawn, with the focus on it and the same alert.
// The page reads no document's blocks: a document added to the issue reaches it as the tree read
// sent again, as the document's creation reads it, which is the read out here. Sent again, that
// read keeps the answer it holds, and with it the reads held (`TreeRead.blocksHeld`); no path of
// the page drops that answer today, so the case drops it (`resetQueries`) to hold the reads out
// again while it is read.
test("an item's failed line stays drawn, with the focus on it and the same alert, while a document added to the issue has its blocks read", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(serverDown());
  const treeOut = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockReturnValue(treeOut.promise);
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "Couldn't load this comment" })
    );
    await waitFor(() =>
      expect(heading, "the failure's line does not hold the focus").toHaveFocus()
    );
    const alert = body.view.getByRole("alert");

    // The issue's read answers with a document added, its spec, and the tree read is sent again
    // for it with its answer dropped, and is out; no decisions document stands beside it, a
    // decision being a block of the spec.
    body.redraw(issueDetails());
    await act(async () => {
      void body.client.resetQueries({
        exact: true,
        queryKey: outlineQuery(KEY, "conversation").queryKey,
      });
    });
    await waitFor(() =>
      expect(tree, "the tree was never read again for the added document").toHaveBeenCalled()
    );
    await nextFrame();
    expect(
      heading.isConnected,
      "the item's failed line left the page while the tree was read again for the added document"
    ).toBe(true);

    await act(async () => treeOut.resolve(treeReadOf(TWO_THREADS)));
    await nextFrame();
    expect(
      heading.isConnected,
      "the item's failed line was drawn anew once the tree read for the added document answered"
    ).toBe(true);
    expect(heading, "the focus left the item's failed line").toHaveFocus();
    expect(
      body.view.getByRole("alert"),
      "the item's alert was drawn again, which says a failure again that came only once"
    ).toBe(alert);
    expect(read, "the item was read again as the reads went out").toHaveBeenCalledTimes(1);
  } finally {
    tree.mockRestore();
    names.mockRestore();
    read.mockRestore();
  }
});

// A turn of a thread only agents wrote in is no node of the outline, on purpose (`agents-only.ts`):
// its address is no missing item. It lands on the turn's nearest drawn ancestor - the issue here,
// which is no row, so the outline's tab stop - and says at that row that the thread is the agents'
// alone. The item is not read as the old page reads one: it is named as a kind of its own.
test("an address naming a turn only agents wrote in lands on its nearest drawn ancestor and says there the thread is the agents' alone", async () => {
  setViewport(1440);
  const agent = { id: "session-1", kind: "session" } as const;
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment");
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: "agents-1", key: KEY, kind: "comment" }),
      [...TWO_THREADS, readComment("agents-1", 5, null, agent)]
    );
    const { container } = body.view;
    await waitFor(() =>
      expect(rowReport(container), "no row says the thread is the agents' alone").toBe(
        KEY_REPORTS.agents_thread
      )
    );
    expect(focusedRow(container), "the landing is not on the outline's tab stop").toBe("head-a");
    expect(
      rowsFor(container, "head-a")[0]?.querySelector("[data-row-report]"),
      "the report is not drawn at the row the address landed on"
    ).not.toBeNull();
    expect(rowsFor(container, "agents-1"), "the outline drew the agents' turn").toEqual([]);
    expect(
      read,
      "the agents' turn was read as an item the issue may not hold"
    ).not.toHaveBeenCalled();
    expect(body.view.queryByRole("heading", { name: "Comment not found" })).toBeNull();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The class search of #4205's 6586a4be, on the class of the landing's line held while a document
// is read: the landing on the nearest drawn ancestor of a turn only agents wrote in was dropped
// while a document added to the issue had its blocks read, which held the tree's reads out
// again (`readsHeld`), since its following settles on no landing of the old page's. The tree was
// drawn with no address to land on, and once the reads answered the landing's key came back as
// a new landing's would: a zoom the reader had made after the landing was left, as a following
// leaves a zoom its node is outside of. The landing stays, so the reader's zoom stands, with the
// focus where they put it. The page reads no document's blocks: a document added to the issue
// reaches it as the tree read sent again, as the document's creation reads it, which is the read
// out here. Sent again, that read keeps the answer it holds (`TreeRead.blocksHeld`), so the reads
// stay held while it is out, and the following keeps the landing it has found besides
// (`Following.found`). The reads are never out again with the zoomed row drawn: every node of the
// tree comes from that one read, and with its answer dropped the row, and the zoom on it, go.
//
// The ancestor is the issue, the outline's tab stop, as it is for every agents-only turn the
// server stores (`agents-only.ts`'s `agentsOnlyIds`). A comment thread is agents-only only where
// no turn of it is under a question (`underQuestion`) and none is a person's, and the tree draws
// an ask only where it is a question (`tree-read/tree-nodes.ts`'s `draftAsk`): no agents-only
// comment hangs under a drawn ask, and the walk up from one (`address-target.ts`'s
// `agentsOnlyAncestor`) meets turns of its own thread, then an approval request, which is no node,
// or nothing, and ends at the issue. A message thread is agents-only only where no turn of it is a
// person's, and the server lists each root - `in_reply_to` null - with every message
// transitively replying to it (`api/messages.go`'s `listIssueRootMessagesQuery` and
// `hydrateMessageThreads`): a message answering a drawn message is a turn of that message's
// thread, which a person wrote in.
test("an agents-only turn's landing on the issue, kept while a document added to the issue has its blocks read, leaves the reader's zoom after it standing", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const agent = { id: "session-1", kind: "session" } as const;
  const comments = [...TWO_THREADS, readComment("agents-1", 5, null, agent)];
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const treeOut = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockReturnValue(treeOut.promise);
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: "agents-1", key: KEY, kind: "comment" }),
      comments
    );
    const { container } = body.view;
    await waitFor(() =>
      expect(rowReport(container), "no row says the thread is the agents' alone").toBe(
        KEY_REPORTS.agents_thread
      )
    );
    // jest-dom: the focus is on a row or a control in it, which focusedRow names by the row's id.
    expect(focusedRow(container), "the landing is not on the outline's tab stop").toBe("head-a");
    await user.keyboard(".");
    await waitFor(() =>
      expect(container.querySelector("[data-crumb]"), "the zoom key zoomed nowhere").not.toBeNull()
    );
    // jest-dom: the focus is on a row or a control in it, which focusedRow names by the row's id.
    const zoomedOn = focusedRow(container);
    expect(zoomedOn, "the zoom left the focus on no row").toBeDefined();

    // The issue's read answers with a document added, its spec, and the tree read is sent again
    // for it, and is out; no decisions document stands beside it, a decision being a block of the
    // spec.
    body.redraw(issueDetails());
    await act(async () => {
      void body.client.invalidateQueries({ queryKey: issueTreeQuery(KEY).queryKey });
    });
    await waitFor(() =>
      expect(tree, "the tree was never read again for the added document").toHaveBeenCalled()
    );
    await nextFrame();
    expect(
      container.querySelector("[data-crumb]"),
      "the reader's zoom was left while the tree was read again for the added document"
    ).not.toBeNull();

    await act(async () => treeOut.resolve(treeReadOf(comments)));
    await nextFrame();
    await nextFrame();
    expect(
      container.querySelector("[data-crumb]"),
      "the reader's zoom was left once the tree read for the added document answered, as by a new landing"
    ).not.toBeNull();
    // jest-dom: the focus is on a row or a control in it, which focusedRow names by the row's id.
    expect(
      focusedRow(container),
      "the focus left the row the reader zoomed to once the tree read for the added document answered"
    ).toBe(zoomedOn);
  } finally {
    tree.mockRestore();
    names.mockRestore();
  }
});

// The fallback's line took the focus the moment it drew, whatever the reader had done since they
// followed the address: a reader who put the focus on a control and pressed a key there while the
// item's read was still out lost it to the line. The line takes the focus only while the reader
// has not acted since the address was followed - as the landing on a row takes none after such an
// act - or in place of whatever held the focus in the body's box, a row of the outline or a line,
// and the same address followed again, with no act since, gives it the focus again.
test("an item's failure drawn after the reader pressed a key leaves the focus where they put it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const address = buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never>();
  const read = spyOn(api, "getComment").mockReturnValue(answer.promise);
  const outside = controlOutside();
  try {
    const body = renderAddressedBody(address, TWO_THREADS);
    await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
    await act(async () => outside.control.focus());
    await user.keyboard("{Shift}");
    await act(async () => answer.reject(noSuchComment()));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
    await nextFrame();
    expect(
      document.activeElement,
      "the failure's line took the focus from the control the reader pressed a key on"
    ).toBe(outside.control);

    await body.follow(address);
    await waitFor(() =>
      expect(
        document.activeElement,
        "the same address followed again, with no act since, left the focus off the failure's line"
      ).toBe(body.view.getByRole("heading", { name: "Comment not found" }))
    );
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// After the reader acted, the fallback's line took the focus wherever nothing held it, so a
// reader who left the focus on the page's body - their own nowhere - lost it to the line drawn
// after, where the address's row drawn after the same act takes none: a press on blank space,
// and Tab from a row of the outline, the page's last tab stop, which a browser run measured on
// a missing comment's address (the line took the focus in all 7 of 7 tries where Tab left it on
// the body). The line takes the focus after an act only where the focus was on the body's box as
// what it replaces left the page, and the focus leaving that box - from the row the page opened
// on, by Tab or a press elsewhere - clears that mark.
test("an item's failure drawn after the reader left the focus on the page's body leaves it there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment");
  const outside = controlOutside();
  // Something outside the outline that takes no focus.
  const words = "Words outside the outline";
  const blank = render(<p>{words}</p>).getByText(words);
  const paths = [
    [
      "a press on a control, then on blank space",
      async () => {
        await user.click(outside.control);
        await user.click(blank);
      },
    ],
    [
      "Tab from a row of the outline, the last tab stop",
      async (container: HTMLElement) => {
        await waitFor(() => expect(focusedRow(container)).toBe("head-a"));
        await user.keyboard("{Tab}");
      },
    ],
  ] as const;
  try {
    for (const [what, leave] of paths) {
      const answer = Promise.withResolvers<never>();
      read.mockReturnValue(answer.promise);
      // Drawn after the control and the words, so the outline is the page's last tab stop.
      const body = renderAddressedBody(
        buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
        TWO_THREADS
      );
      await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
      await leave(body.view.container);
      expect(document.activeElement, `${what} left the focus on an element`).toBe(document.body);

      await act(async () => answer.reject(noSuchComment()));
      await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
      await nextFrame();
      expect(
        document.activeElement,
        `the line that names no comment, drawn after ${what}, took the focus`
      ).toBe(document.body);
      body.view.unmount();
      read.mockClear();
    }
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

/** Moves the focus by a key from the row the page opens on, `head-a`, to the next row of the
 *  outline drawn in `container`: an act of the reader's, which leaves the focus on the outline. */
async function moveOnFromFirstRow(user: UserEvent, container: HTMLElement): Promise<void> {
  await waitFor(() => expect(focusedRow(container)).toBe("head-a"));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(container)).not.toBe("head-a"));
  expect(focusedRow(container), "j left the focus on no row").toBeDefined();
}

// The mark that lets a line take the focus after the reader acted was held on the fallback's own
// box, which the outline is not drawn in: a reader who had moved to a row of the outline by a key
// while the item's read was out lost the focus with the outline as the line replaced it, and the
// line took none, leaving it on the page's body. The mark is the body's box's, which holds the
// outline and every line drawn in its place, so the line takes the focus the outline took off the
// page with it.
test("an item's failure drawn in place of the outline after the reader moved to a row takes the focus", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never>();
  const read = spyOn(api, "getComment").mockReturnValue(answer.promise);
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
    await moveOnFromFirstRow(user, body.view.container);

    await act(async () => answer.reject(noSuchComment()));
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "Comment not found" })
    );
    await waitFor(() =>
      expect(
        document.activeElement,
        "the line that names no comment, drawn in place of the outline the reader had moved in, left the focus on the page's body"
      ).toBe(heading)
    );
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// A line took the focus after the reader acted wherever the focus had been on the body's box, a
// mark a focusout leaving the box clears: the page took out the row the reader had moved to while
// the outline stayed drawn, so the focus fell to the page's body with the mark still set, and no
// focusout left the box when the reader then pressed a control outside it. The line drawn later
// took the focus from that control. After an act the line takes the focus only while nothing else
// holds it as it is drawn. The read here drops every comment, not the reader's alone, so no row is
// left for the focus to move to: a focused row a render drops gives the focus to the nearest row
// still drawn (`IssueTree`), and only where none is left does the focus fall to the page's body,
// with the mark still set.
test("a line drawn after the reader's row left the outline and they pressed a control outside it leaves the focus on that control", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never>();
  const read = spyOn(api, "getComment").mockReturnValue(answer.promise);
  const outside = controlOutside();
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const { container } = body.view;
    await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
    await moveOnFromFirstRow(user, container);
    const moved = focusedRow(container);
    if (moved === undefined) throw new Error("j left the focus on no row");

    // The outline draws its turns from the tree read, so every comment leaves the tree read too.
    await act(async () => {
      body.client.setQueryData(["comments", KEY], []);
      body.client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeReadOf([]));
    });
    await waitFor(() => expect(rowsFor(container, moved)).toEqual([]));
    expect(body.view.queryByRole("tree"), "the outline left the page with the row").not.toBeNull();
    expect(
      document.activeElement,
      "the row's leaving left the focus on an element, not the page's body"
    ).toBe(document.body);
    await user.click(outside.control);
    expect(document.activeElement).toBe(outside.control);

    await act(async () => answer.reject(noSuchComment()));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
    await nextFrame();
    expect(
      document.activeElement,
      "the line that names no comment, drawn after the reader's row left the outline and they pressed a control outside it, took the focus from that control"
    ).toBe(outside.control);
    body.view.unmount();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The line saying the outline holds no row of the block a `#b-` address names is drawn once the
// last of the tree's reads answers, which can be after a read that answered first drew the rows
// and the reader moved to one of them by keys: the page places no focus of its own until every
// read has answered, so they entered the outline with Tab. Drawn in place of the outline, the
// line takes the focus the outline took off the page with it, as an item's line does. Here the
// messages' read is the last to answer.
test("the line that names no row of a block, drawn in place of the outline after the reader moved to a row, takes the focus", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never[]>();
  const messages = spyOn(api, "listIssueMessages").mockReturnValue(answer.promise as never);
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}#b-no-such-block`, TWO_THREADS, BodySlot, {
      unread: [["issue-messages", KEY]],
    });
    await waitFor(() => expect(messages).toHaveBeenCalled());
    await user.tab();
    await moveOnFromFirstRow(user, view.container);

    await act(async () => answer.resolve([]));
    const title = await waitFor(() => view.getByRole("heading", { name: "No row for this block" }));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the line that names no row of the block, drawn in place of the outline the reader had moved in, left the focus on the page's body"
      ).toBe(title)
    );
    view.unmount();
  } finally {
    names.mockRestore();
    messages.mockRestore();
  }
});

// The mark was read by React's `onFocus` and `onBlur` on the body's box, which hear only what React
// draws inside the box: the document a row is being edited in is drawn there by a React portal
// whose React parent is beside the box (`document-stage.tsx`), so the focus leaving it for a
// control outside the box left the mark set, and the line drawn later took the focus from that
// control. The box's own listeners hear every focus inside its DOM, whatever draws it. A stage the
// case moves into the box stands for the row's, with a control a portal draws in it.
test("a line drawn after the focus left a portal inside the outline's box for a control outside it takes no focus", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never>();
  const read = spyOn(api, "getComment").mockReturnValue(answer.promise);
  const outside = controlOutside();
  const stage = document.createElement("div");
  const name = "A control a portal draws";
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS,
      (props) => (
        <>
          <BodySlot {...props} />
          {createPortal(<button type="button">{name}</button>, stage)}
        </>
      )
    );
    const { container } = body.view;
    await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
    await waitFor(() => expect(focusedRow(container)).toBe("head-a"));
    const box = container.querySelector<HTMLElement>("[data-issue-body]");
    if (box === null) throw new Error("the body's box is not drawn");
    await act(async () => box.append(stage));
    const portalled = body.view.getByRole("button", { name });
    await user.click(portalled);
    expect(document.activeElement, "the press left the focus off the portal's control").toBe(
      portalled
    );
    await user.click(outside.control);
    expect(document.activeElement).toBe(outside.control);

    await act(async () => answer.reject(noSuchComment()));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
    await nextFrame();
    expect(
      document.activeElement,
      "the line that names no comment, drawn after the focus left a portal inside the outline's box for a control outside it, took the focus from that control"
    ).toBe(outside.control);
    body.view.unmount();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// happy-dom fires nothing at a focused element it takes out of the page, where Chromium fires blur
// and focusout, their relatedTarget null, while the element is still connected (measured in
// Chromium 156). The case fires them as Chromium does, at the focused row as React takes the
// outline out to draw the line in its place. Heard as the focus leaving the box for nowhere, that
// focusout cleared the mark, and the line took no focus, though the focus fell to the page's body
// with the outline it was on: the line takes it.
test("a line drawn in place of the outline takes the focus though taking out the focused row fires focusout, as Chromium does", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const answer = Promise.withResolvers<never>();
  const read = spyOn(api, "getComment").mockReturnValue(answer.promise);
  const removeChild = Node.prototype.removeChild;
  // The focusouts fired at a focused element as it was taken out of the page.
  let fired = 0;
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    await waitFor(() => expect(read).toHaveBeenCalledWith(GONE_COMMENT));
    await moveOnFromFirstRow(user, body.view.container);

    const removals = spyOn(Node.prototype, "removeChild").mockImplementation(function (
      this: Node,
      child: Node
    ) {
      const focused = document.activeElement;
      if (focused !== null && focused !== document.body && child.contains(focused)) {
        fired += 1;
        focused.dispatchEvent(new FocusEvent("blur", { relatedTarget: null }));
        focused.dispatchEvent(new FocusEvent("focusout", { bubbles: true, relatedTarget: null }));
      }
      return removeChild.call(this, child);
    } as typeof Node.prototype.removeChild);
    let heading: HTMLElement;
    try {
      await act(async () => answer.reject(noSuchComment()));
      heading = await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
    } finally {
      removals.mockRestore();
    }
    expect(fired, "taking the outline out of the page fired no focusout at the focused row").toBe(
      1
    );
    await waitFor(() =>
      expect(
        document.activeElement,
        "the line that names no comment, drawn in place of the outline whose focused row fired focusout as it was taken out, left the focus on the page's body"
      ).toBe(heading)
    );
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The fallback's Retry was today's page's, which reads nothing of the read it asks again: it drew
// no busy state while the read ran, and a second failure left the first alert standing, so a
// screen reader said nothing. It is the new page's failed read's line (`FailedReadLine`): busy
// while the read runs again and never disabled, and each failure draws its alert afresh.
test("an item whose read failed draws a Retry that is busy while it reads again, and says a second failure again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValueOnce(serverDown());
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const retry = await waitFor(() =>
      body.view.getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const first = body.view.getByRole("alert");
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise);
    await user.click(retry);
    await waitFor(() =>
      expect(retry, "the Retry is not busy while its read runs again").toHaveAttribute(
        "aria-busy",
        "true"
      )
    );
    expect(retry, "the Retry is disabled while its read runs again").not.toBeDisabled();
    await act(async () => again.reject(serverDown()));
    await waitFor(() =>
      expect(retry, "the Retry is still busy once its read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    expect(read).toHaveBeenCalledTimes(2);
    expect(
      body.view.getByRole("alert"),
      "the second failure left the first failure's alert standing, which says nothing again"
    ).not.toBe(first);
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

/** An open question of the issue whose turn is the reader's, alice's, as the asks' read answers
 *  it: the tree draws it under the issue, and the page opens on it. */
const WAITING_QUESTION = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "planner", kind: "session" },
  block_id: null,
  created_at: "2026-11-05T00:00:00Z",
  edited_at: null,
  id: "ask-waiting",
  issue_key: KEY,
  kind: "question",
  multiple: false,
  options: [],
  question: "Which flag?",
  state: "open",
  urgency: "med",
  waiting_on: "human",
  waiting_on_person: "alice",
} as const;

/** The ids of the rows the outline in `container` draws, in its order. */
function drawnRowIds(container: HTMLElement): (string | undefined)[] {
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
    (row) => row.dataset.nodeId
  );
}

/** What a wait below says once it gives up: its own assertion's words, the counts and ids they
 *  name, and not the whole page, which `waitFor` adds by default. */
const OWN_WORDS = { onTimeout: (error: Error) => error };

/** The failed reads' lines the page in `container` draws, once there are `count` of them, where
 *  the first stands to the tree - before it, and outside it - and the first's Retry. */
async function failedLines(container: HTMLElement, count: number, read: string) {
  await waitFor(
    () =>
      expect(
        container.querySelectorAll("[data-failed-read]").length,
        `the failed ${read} read drew no line`
      ).toBe(count),
    OWN_WORDS
  );
  const line = container.querySelector<HTMLElement>("[data-failed-read]");
  const tree = container.querySelector('[role="tree"]');
  if (line === null || tree === null) throw new Error("the page drew no line or no tree");
  const retry = line.querySelector<HTMLElement>(FAILED_READ_RETRY);
  if (retry === null) throw new Error(`the failed ${read} read's line drew no Retry`);
  return {
    inTree: tree.contains(line),
    line,
    overTree: (line.compareDocumentPosition(tree) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
    retry,
  };
}

/** Waits until the page in `container` draws no failed read's line, once the `read` read that
 *  failed has answered. */
async function failedLinesGone(container: HTMLElement, read: string): Promise<void> {
  await waitFor(
    () =>
      expect(
        container.querySelectorAll("[data-failed-read]").length,
        `the failed ${read} read's line stayed once it answered`
      ).toBe(0),
    OWN_WORDS
  );
}

/** Waits until the focus in `container` is on the tree's tab-stop row (`TAB_STOP_ROW`), where the
 *  focus the `read` read's Retry held goes as its line leaves. */
async function focusOnTabStopRow(container: HTMLElement, read: string): Promise<void> {
  await waitFor(
    () =>
      expect(
        focusPlace(container),
        `the focus the failed ${read} read's Retry held did not go to the tree's tab-stop row as its line left`
      ).toBe(
        container.querySelector<HTMLElement>(TAB_STOP_ROW)?.dataset.nodeId ?? "the tab-stop row"
      ),
    OWN_WORDS
  );
}

// A names read that failed was silent: what the tree said of it (`IssueTreeView.namesFailure`)
// was read by nothing, so every chip kept its raw address, or names from before, with no word.
// Its line is drawn over the tree, the rows still drawn under it and the page opened on its first
// row as ever (the names place no row); its Retry asks the names read again, and its answer takes
// the line away.
test("a failed names read draws its line over the tree with every row drawn, and its Retry reads the names again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  names.mockRejectedValueOnce(serverDown());
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([] as never);
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS);
    const { container } = view;
    const { inTree, line, overTree, retry } = await failedLines(container, 1, "names");
    expect(line.querySelector('[role="alert"]')?.textContent).toBe(STATE_LINES.names_unreadable);
    expect({ inTree, overTree }, "the names' line is not drawn over the tree").toEqual({
      inTree: false,
      overTree: true,
    });
    await waitFor(
      () =>
        expect(focusedRow(container), "a failed names read moved the page's first focus").toBe(
          "head-a"
        ),
      OWN_WORDS
    );
    const drawn = drawnRowIds(container);
    expect(drawn, "the failed names read took rows off the outline").toEqual(
      expect.arrayContaining(["head-a", "head-b"])
    );

    await user.click(retry);
    await failedLinesGone(container, "names");
    expect(names.mock.calls.length, "the Retry did not ask the names again").toBe(2);
    expect(messages.mock.calls.length, "the names' Retry asked another read again").toBe(0);
    expect(drawnRowIds(container), "the rows changed once the names answered").toEqual(drawn);
  } finally {
    names.mockRestore();
    messages.mockRestore();
  }
});

// A failed read of the outline's own was silent too: with the asks' read failed the reads settled,
// no question's node was built, and the page opened on the first row as if nothing waited on the
// reader, with nothing said. The read's line is drawn over the tree, the rows the other reads hold
// still drawn, and the page opens on its Retry, which asks that read alone again: once it
// answers, the line goes, the question's row is drawn, and the focus the Retry held goes to the
// tree's tab-stop row, as the stage gives it back for a view's line (`FailedLineLeaves`). The
// tree read holds the question as a node, and the asks' read says how it stands: until it does,
// nothing says the question waits on the reader.
test("a failed asks read draws its line over the tree and opens the page on its Retry, which draws the question", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([WAITING_QUESTION] as never);
  asks.mockRejectedValueOnce(serverDown());
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([] as never);
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY]],
    });
    const { container } = view;
    const { inTree, line, overTree, retry } = await failedLines(container, 1, "asks");
    expect(line.querySelector('[role="alert"]')?.textContent).toBe(STATE_LINES.asks_unreadable);
    expect({ inTree, overTree }, "the asks' line is not drawn over the tree").toEqual({
      inTree: false,
      overTree: true,
    });
    expect(drawnRowIds(container), "the failed asks read took the comments' rows away").toEqual(
      expect.arrayContaining(["head-a", "head-b"])
    );
    const onRetry = () =>
      expect(
        retry,
        `the page's first focus is on ${focusedRow(container) ?? "no row"} and not on the asks' Retry`
      ).toHaveFocus();
    await waitFor(onRetry, OWN_WORDS);
    // The opening places its focus once: no frame after moves it to a row.
    await framesPass();
    onRetry();

    await user.click(retry);
    await failedLinesGone(container, "asks");
    expect(asks.mock.calls.length, "the Retry did not ask the asks again").toBe(2);
    expect(messages.mock.calls.length, "the asks' Retry asked another read again").toBe(0);
    expect(rowsFor(container, WAITING_QUESTION.id).length, "the question drew no row").toBe(1);
    await focusOnTabStopRow(container, "asks");
  } finally {
    asks.mockRestore();
    messages.mockRestore();
    names.mockRestore();
  }
});

/** Where the focus is in `container`: the id of the row that holds it, a failed read's Retry
 *  named by what its line says (`onRetryOf`), or no row. */
function focusPlace(container: HTMLElement): string {
  const line = document.activeElement?.closest("[data-failed-read]");
  if (line != null) return onRetryOf(line.querySelector('[role="alert"]')?.textContent ?? "");
  return focusedRow(container) ?? "no row";
}

/** What `focusPlace` says where the focus is on the Retry of the line that says `message`. */
function onRetryOf(message: string): string {
  return `the Retry of the line "${message}"`;
}

// A failed read's line went the moment its Retry was pressed: TanStack sets a read that holds no
// answer back to pending as it reads again, and the line was drawn only while its read was in
// error. The focus the Retry held went to the tree's tab-stop row while the read was still out,
// and the line a second failure drew took none. The line stays while its read is out, busy, its
// Retry holding the focus; a second failure leaves it where it was and says itself again; and once
// the read answers, the line goes and the focus goes to the tree's tab-stop row. The tree read
// holds the question as a node, whose standing the asks' read brings.
test("a failed read's line stays drawn, busy, with the focus on its Retry while its read is out again, and goes once the read answers", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([WAITING_QUESTION] as never);
  asks.mockRejectedValueOnce(serverDown());
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([] as never);
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY]],
    });
    const { container } = view;
    const { line, retry } = await failedLines(container, 1, "asks");
    await waitFor(
      () => expect(retry, "the page did not open on the asks' Retry").toHaveFocus(),
      OWN_WORDS
    );
    // The opening places its focus once.
    await framesPass();
    // The line, its Retry and the focus, as they stand while the `run` Retry's read, the asks'
    // read `reads` times in all, is out.
    const heldOut = async (run: string, reads: number) => {
      await waitFor(
        () => expect(asks.mock.calls.length, `the ${run} Retry did not read the asks`).toBe(reads),
        OWN_WORDS
      );
      expect(
        [container.querySelectorAll("[data-failed-read]").length, retry.isConnected],
        `the asks' line left the page while the ${run} Retry's read was out`
      ).toEqual([1, true]);
      await waitFor(
        () =>
          expect(
            retry,
            `the Retry is not busy while the ${run} Retry's read is out`
          ).toHaveAttribute("aria-busy", "true"),
        OWN_WORDS
      );
      expect(retry, `the focus left the Retry while the ${run} Retry's read was out`).toHaveFocus();
    };

    const failing = Promise.withResolvers<never>();
    asks.mockReturnValueOnce(failing.promise as never);
    const firstAlert = line.querySelector('[role="alert"]');
    await user.click(retry);
    await heldOut("first", 2);
    await act(async () => failing.reject(serverDown()));
    await waitFor(
      () =>
        expect(retry, "the Retry is still busy once its read failed again").not.toHaveAttribute(
          "aria-busy"
        ),
      OWN_WORDS
    );
    expect(
      [container.querySelectorAll("[data-failed-read]").length, retry.isConnected],
      "the asks' line left the page when their read failed again"
    ).toEqual([1, true]);
    expect(retry, "the second failure took the focus off the Retry").toHaveFocus();
    expect(
      line.querySelector('[role="alert"]'),
      "the second failure left the first failure's alert standing, which says nothing again"
    ).not.toBe(firstAlert);

    const answering = Promise.withResolvers<never>();
    asks.mockReturnValueOnce(answering.promise as never);
    await user.click(retry);
    await heldOut("second", 3);
    await act(async () => answering.resolve([WAITING_QUESTION] as never));
    await failedLinesGone(container, "asks");
    expect(rowsFor(container, WAITING_QUESTION.id).length, "the question drew no row").toBe(1);
    await focusOnTabStopRow(container, "asks");
  } finally {
    asks.mockRestore();
    messages.mockRestore();
    names.mockRestore();
  }
});

// A read whose refresh failed while it holds its answer reads `error`, and the outline drew that
// read's line over rows that were all drawn; a page opened over the same reads after that counted
// them settled at once, the line drawn, and opened on its Retry rather than on the question that
// waits on the reader. A read that holds its answer is not failed, whatever its refresh did: it
// draws no line, and the page opens on the question. The tree read holds the question as a node,
// whose standing the asks' read brings.
test("a read whose refresh failed while it holds its answer draws no line, and a page opened after that opens on the question", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([WAITING_QUESTION] as never);
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([] as never);
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY]],
    });
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    await waitFor(
      () =>
        expect(focusPlace(body.view.container), "the page did not open on the question").toBe(
          WAITING_QUESTION.id
        ),
      OWN_WORDS
    );

    asks.mockRejectedValue(serverDown());
    await act(async () => {
      await body.client.invalidateQueries({ queryKey: ["asks", KEY] });
    });
    expect(
      body.client.getQueryState(issueAskThreadsQuery(KEY).queryKey)?.status,
      "the asks' refresh did not fail"
    ).toBe("error");
    expect(
      body.view.container.querySelectorAll("[data-failed-read]").length,
      "the asks' refresh that failed drew a line, though the read holds its answer"
    ).toBe(0);
    expect(
      rowsFor(body.view.container, WAITING_QUESTION.id).length,
      "the asks' refresh that failed took the question's row away"
    ).toBe(1);

    const { container } = body.reopen();
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    await waitFor(
      () =>
        expect(
          focusPlace(container),
          "the page opened after the asks' refresh failed did not open on the question"
        ).toBe(WAITING_QUESTION.id),
      OWN_WORDS
    );
    // The opening places its focus once.
    await framesPass();
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    expect(focusPlace(container), "a frame after the opening moved its focus").toBe(
      WAITING_QUESTION.id
    );
    expect(
      container.querySelectorAll("[data-failed-read]").length,
      "the page opened after the asks' refresh failed drew a line, though the read holds its answer"
    ).toBe(0);
  } finally {
    asks.mockRestore();
    messages.mockRestore();
    names.mockRestore();
  }
});

/** The failed read's line in `container` that says `message`. */
function failedLineSaying(container: HTMLElement, message: string): HTMLElement {
  const line = [...container.querySelectorAll<HTMLElement>("[data-failed-read]")].find(
    (each) => each.querySelector('[role="alert"]')?.textContent === message
  );
  if (line === undefined) throw new Error(`the page drew no line that says "${message}"`);
  return line;
}

// A failed read took the page's opening whatever waited on the reader: with the results' read
// failed and the asks' read answered, the page opened on the results' Retry, and the question that
// waits on the reader, drawn and known, was left for the reader to find. The question keeps the
// opening whatever reads failed, and the results' line is drawn over the tree all the same. The
// tree read holds the question as a node, whose standing the asks' read brings.
test("a failed results read draws its line over the tree, and the page opens on the question that waits", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([WAITING_QUESTION] as never);
  const results = spyOn(api, "getIssueResults").mockRejectedValue(serverDown());
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY], issueResultsQuery(KEY).queryKey],
    });
    const { container } = view;
    const { inTree, line, overTree } = await failedLines(container, 1, "results");
    expect(line.querySelector('[role="alert"]')?.textContent).toBe(STATE_LINES.results_unreadable);
    expect({ inTree, overTree }, "the results' line is not drawn over the tree").toEqual({
      inTree: false,
      overTree: true,
    });
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    await waitFor(
      () =>
        expect(
          focusPlace(container),
          "the page with the results' read failed did not open on the question that waits"
        ).toBe(WAITING_QUESTION.id),
      OWN_WORDS
    );
    // The opening places its focus once: no frame after moves it to the results' Retry.
    await framesPass();
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    expect(focusPlace(container), "a frame after the opening moved its focus").toBe(
      WAITING_QUESTION.id
    );
  } finally {
    asks.mockRestore();
    names.mockRestore();
    results.mockRestore();
  }
});

// With the asks' read failed and no question drawn that waits on the reader - the issue's own read
// holds no open ask here - the asks' Retry takes the opening, since a question the issue's read
// does not hold comes from the asks alone, even where a read whose line is drawn above the asks'
// failed too: the page opened on the first failed line's Retry, the results', whose answer brings
// no question.
test("with the asks and the results reads failed, the page opens on the asks' Retry", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockRejectedValue(serverDown());
  const results = spyOn(api, "getIssueResults").mockRejectedValue(serverDown());
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      unread: [["asks", KEY], issueResultsQuery(KEY).queryKey],
    });
    const { container } = view;
    await failedLines(container, 2, "asks and results");
    expect(
      [...container.querySelectorAll("[data-failed-read] [role='alert']")].map(
        (alert) => alert.textContent
      ),
      "the lines are not drawn in the order the reads are given"
    ).toEqual([STATE_LINES.results_unreadable, STATE_LINES.asks_unreadable]);
    await waitFor(
      () =>
        expect(focusPlace(container), "the page did not open on the asks' Retry").toBe(
          onRetryOf(STATE_LINES.asks_unreadable)
        ),
      OWN_WORDS
    );
    // The opening places its focus once.
    await framesPass();
    expect(focusPlace(container), "a frame after the opening moved its focus").toBe(
      onRetryOf(STATE_LINES.asks_unreadable)
    );
  } finally {
    asks.mockRestore();
    names.mockRestore();
    results.mockRestore();
  }
});

// The questions come from the issue's own read as well as from the asks': its open asks
// (`open_asks`). The opening's comments said none could be known while the asks' read stood
// failed, but a question that waits on the reader which the issue's read holds is drawn all the
// same: the page opens on it, and not on the asks' Retry, whose line is drawn over the tree. The
// tree read holds the question as a node, whose standing the issue's read brings here.
test("with the asks read failed, the page opens on a question that waits on the reader from the issue's own read, not on the asks' Retry", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockRejectedValue(serverDown());
  const issue: IssueDetails = {
    ...issueDetails(),
    artifacts: [],
    open_asks: [{ ...WAITING_QUESTION, last_reply: null, options: [] }],
  };
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      issue,
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY]],
    });
    const { container } = view;
    const { line } = await failedLines(container, 1, "asks");
    expect(line.querySelector('[role="alert"]')?.textContent).toBe(STATE_LINES.asks_unreadable);
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    await waitFor(
      () =>
        expect(
          focusPlace(container),
          "the page with the asks' read failed did not open on the question the issue's read holds"
        ).toBe(WAITING_QUESTION.id),
      OWN_WORDS
    );
    // The opening places its focus once: no frame after moves it to the asks' Retry.
    await framesPass();
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    expect(focusPlace(container), "a frame after the opening moved its focus").toBe(
      WAITING_QUESTION.id
    );
  } finally {
    asks.mockRestore();
    names.mockRestore();
  }
});

// The asks' and the results' reads failed as the reads settled, with no question known to wait,
// so the page was to open on the asks' Retry; the asks' read answers before that Retry's frame,
// with a question that waits on the reader, and takes its line away. The opening was handed on to
// the Retry of the line still drawn, the results', over the question now known: it is handed on
// by the rule the reads settled by, and opens on the question. The tree read holds the question
// as a node, whose standing the asks' read brings.
test("a failed asks read that answers with a question that waits before its Retry's frame hands the opening on to the question, not to another failed read's Retry", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const asks = spyOn(api, "listIssueAskThreads").mockRejectedValue(serverDown());
  const results = spyOn(api, "getIssueResults").mockRejectedValue(serverDown());
  const frames = framesByHand();
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY], issueResultsQuery(KEY).queryKey],
    });
    const { container } = body.view;
    await failedLines(container, 2, "asks and results");
    // jest-dom: no row and no Retry holds the focus, which focusPlace says as "no row".
    expect(
      focusPlace(container),
      "the page took a focus before the frame of the asks' Retry came"
    ).toBe("no row");

    // An event's refresh of the asks, which answers with the question before that frame.
    asks.mockResolvedValue([WAITING_QUESTION] as never);
    await act(async () => {
      await body.client.refetchQueries({ queryKey: ["asks", KEY] });
    });
    await waitFor(
      () =>
        expect(
          [
            container.querySelectorAll("[data-failed-read]").length,
            rowsFor(container, WAITING_QUESTION.id).length,
          ],
          "the asks' answer did not take its line away and draw the question"
        ).toEqual([1, 1]),
      OWN_WORDS
    );
    // The frames asked for by then, and any those ask for in turn.
    await frames.run();
    await frames.run();
    // jest-dom: the focus is on the question's row or a control in it, which focusPlace names.
    expect(
      focusPlace(container),
      "the asks' line that left before its frame handed the opening on elsewhere than to the question that waits"
    ).toBe(WAITING_QUESTION.id);
  } finally {
    frames.restore();
    asks.mockRestore();
    names.mockRestore();
    results.mockRestore();
  }
});

// Where no question waits on the reader, a failed read the rows come from still takes the
// opening: the question that waits may be what it failed to bring. The comments' read failed here,
// and the one question drawn waits on somebody else, so the page opens on the comments' Retry and
// not on that question's row, the outline's first. The tree read holds the question as a node,
// whose standing the asks' read brings.
test("a failed comments read opens the page on its Retry where no question waits on the reader", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const elsewhere = { ...WAITING_QUESTION, waiting_on_person: "bob" };
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([elsewhere] as never);
  const comments = spyOn(api, "listComments").mockRejectedValue(serverDown());
  try {
    const { view } = renderAddressedBody(`/issues/${KEY}`, undefined, StageSlot, {
      nodes: [questionNode(elsewhere)],
      unread: [["asks", KEY]],
    });
    const { container } = view;
    const { line, retry } = await failedLines(container, 1, "comments");
    expect(line.querySelector('[role="alert"]')?.textContent).toBe(STATE_LINES.comments_unreadable);
    await waitFor(
      () =>
        expect(
          rowsFor(container, elsewhere.id).length,
          "the question that waits on somebody else drew no row"
        ).toBe(1),
      OWN_WORDS
    );
    await waitFor(
      () => expect(retry, "the page did not open on the comments' Retry").toHaveFocus(),
      OWN_WORDS
    );
    // The opening places its focus once.
    await framesPass();
    expect(retry, "a frame after the opening moved its focus").toHaveFocus();
  } finally {
    asks.mockRestore();
    comments.mockRestore();
    names.mockRestore();
  }
});

// A failed names read's line left the page while another failed read ran again - its Retry,
// window focus or an event's refresh - since TanStack puts a read that holds no answer back in
// `pending` as it runs, and the names read, held back until every read has answered, was moved
// to a set of none, which stands failed no more. The focus its Retry held went with the line, and
// its alert was said again once the line came back. The names read keeps its set while the other
// read runs: its line stays drawn, as it was, with the focus on its Retry.
test("a failed names read's line stays drawn, with the focus on its Retry, while the failed asks read runs again", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  names.mockRejectedValueOnce(serverDown());
  const asks = spyOn(api, "listIssueAskThreads").mockRejectedValueOnce(serverDown());
  const out = Promise.withResolvers<never>();
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      unread: [["asks", KEY]],
    });
    const { container } = body.view;
    await failedLines(container, 2, "asks and names");
    await waitFor(
      () =>
        expect(focusPlace(container), "the page did not open on the asks' Retry").toBe(
          onRetryOf(STATE_LINES.asks_unreadable)
        ),
      OWN_WORDS
    );
    await framesPass();
    const line = failedLineSaying(container, STATE_LINES.names_unreadable);
    const retry = line.querySelector<HTMLElement>(FAILED_READ_RETRY);
    if (retry === null) throw new Error("the names' line drew no Retry");
    const alert = line.querySelector('[role="alert"]');
    await act(async () => retry.focus());
    expect(focusPlace(container), "the names' Retry took no focus").toBe(
      onRetryOf(STATE_LINES.names_unreadable)
    );

    // An event's refresh of the asks, which runs their read again and leaves it out.
    asks.mockReturnValueOnce(out.promise as never);
    await act(async () => {
      void body.client.invalidateQueries({ queryKey: ["asks", KEY] });
    });
    await waitFor(
      () => expect(asks.mock.calls.length, "the asks read did not run again").toBe(2),
      OWN_WORDS
    );
    expect(
      body.client.getQueryState(issueAskThreadsQuery(KEY).queryKey)?.status,
      "the asks read running again is not pending"
    ).toBe("pending");
    await framesPass();
    expect(line.isConnected, "the names' line left the page while the asks read ran again").toBe(
      true
    );
    expect(
      focusPlace(container),
      "the focus left the names' Retry while the asks read ran again"
    ).toBe(onRetryOf(STATE_LINES.names_unreadable));
    expect(
      line.querySelector('[role="alert"]'),
      "the names' alert was drawn again, which says a failure again that came only once"
    ).toBe(alert);
    expect(
      retry.getAttribute("aria-busy"),
      "the names' Retry reads busy while only the asks read runs"
    ).toBeNull();
    expect(names.mock.calls.length, "the names were read again while the asks read ran").toBe(1);
  } finally {
    await act(async () => out.resolve([] as never));
    asks.mockRestore();
    names.mockRestore();
  }
});

// The tester's finding at #4205's 46dab3c7, on the class of review 5989897443's names-line
// finding: the names line stays while the failed asks read runs again, but once that read
// answers, its answer cites an address the tree did not hold, so the names are read under a new
// set of addresses, and while that read is out the names stand failed no more: the line left the
// page with the focus its Retry held, which went to a row, and came back saying its failure again
// once the new read failed. Measured on a preview of the redesign issue at 1440 by 900: the line
// gone from 1.7 s to 2.8 s after the asks' Retry, the focus on a row. The names have not answered
// at any point, so the line stays drawn, with the focus on its Retry and the same alert. The tree
// read holds the question as a node, with its words; what the asks' read brings it is how the
// question stands, and a person's answer.
test("a failed names read's line stays drawn, with the focus on its Retry, while the names are read under the addresses a recovered read brought", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockRejectedValue(serverDown());
  const asks = spyOn(api, "listIssueAskThreads").mockRejectedValueOnce(serverDown());
  const asksOut = Promise.withResolvers<never>();
  const namesOut = Promise.withResolvers<never>();
  try {
    const body = renderAddressedBody(`/issues/${KEY}`, TWO_THREADS, StageSlot, {
      nodes: [questionNode(WAITING_QUESTION)],
      unread: [["asks", KEY]],
    });
    const { container } = body.view;
    // The asks' line is drawn first, over the names'.
    const { retry: asksRetry } = await failedLines(container, 2, "asks and names");
    await waitFor(
      () => expect(asksRetry, "the page did not open on the asks' Retry").toHaveFocus(),
      OWN_WORDS
    );
    await framesPass();
    const line = failedLineSaying(container, STATE_LINES.names_unreadable);
    const retry = line.querySelector<HTMLElement>(FAILED_READ_RETRY);
    if (retry === null) throw new Error("the names' line drew no Retry");
    const alert = line.querySelector('[role="alert"]');
    await act(async () => retry.focus());

    // The asks read runs again and answers with the question answered by a person, whose answer
    // cites an address the tree did not hold; the names read under the new set of addresses is
    // then out.
    asks.mockReturnValueOnce(asksOut.promise as never);
    await act(async () => {
      void body.client.invalidateQueries({ queryKey: ["asks", KEY] });
    });
    await waitFor(
      () => expect(asks.mock.calls.length, "the asks read did not run again").toBe(2),
      OWN_WORDS
    );
    const namesBefore = names.mock.calls.length;
    names.mockReturnValue(namesOut.promise as never);
    const citing = {
      ...WAITING_QUESTION,
      answer: {
        at: "2026-11-06T00:00:00Z",
        selected: [],
        text: "The flag dispatch://CORE-9 sets.",
        user: "bob",
      },
      state: "answered",
      waiting_on_person: "bob",
    };
    await act(async () => asksOut.resolve([citing] as never));
    await waitFor(
      () =>
        expect(
          names.mock.calls.length,
          "the names were not read again under the addresses the asks brought"
        ).toBeGreaterThan(namesBefore),
      OWN_WORDS
    );
    await framesPass();
    expect(
      line.isConnected,
      "the names' line left the page while the names were read under the new addresses"
    ).toBe(true);
    expect(
      retry,
      "the focus left the names' Retry while the names were read under the new addresses"
    ).toHaveFocus();
    expect(
      line.querySelector('[role="alert"]'),
      "the names' alert was drawn again, which says a failure again that came only once"
    ).toBe(alert);
  } finally {
    await act(async () => namesOut.resolve({ targets: [] } as never));
    asks.mockRestore();
    names.mockRestore();
  }
});

/** Answers the tree read as `treeReadOf` answers `comments`, and each of its views with no node:
 *  a view the stage draws reads its view of the tree (`ViewReadProvider`). */
function answerTreeReads(comments: readonly Comment[]) {
  return spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.view === undefined ? treeReadOf(comments) : treeReadOf([])
  );
}

/** Every preset that draws a view in the outline's place, read from the stage's own slots
 *  (`VIEW_SLOTS`), so a view added there is a view these cases cover. */
const VIEW_PRESETS: readonly Preset[] = PRESET_ORDER.filter(
  (preset) => VIEW_SLOTS[preset] !== undefined
);

/** The stage at `path` (`renderAddressedBody`), every view a stand-in that draws its preset's
 *  name (`data-stand-in-view`), `preset`'s slot `filled` or not and every other slot as
 *  `SLOT_FILLED` has it: the stage's rule, read apart from what each view's part draws and from
 *  which of them have filled their slots. */
function renderStage(path: string, preset: Preset, filled: boolean, issue?: IssueDetails) {
  const views = Object.fromEntries(
    Object.entries(VIEW_SLOTS).map(([each, slot]) => [
      each,
      slot === undefined ? undefined : () => <div data-stand-in-view={each} />,
    ])
  ) as ViewSlots;
  return renderAddressedBody(
    path,
    TWO_THREADS,
    (props) => <StageSlot {...props} filled={{ ...SLOT_FILLED, [preset]: filled }} views={views} />,
    { issue }
  );
}

/** The presets of the stand-in views drawn in `container`, in the page's order. */
function standInViews(container: HTMLElement): (string | null)[] {
  return [...container.querySelectorAll("[data-stand-in-view]")].map((view) =>
    view.getAttribute("data-stand-in-view")
  );
}

// `documentItemPath` writes the address of an item of the spec as the Spec view's address with
// the item beside it (`/spec?comment=<id>`), which keeps the view's preset (`issueAddress`). A
// view whose part has not filled its slot draws nothing, so the address drew no outline and landed
// nowhere, with the focus on the page's body. Under every view, while its slot is not filled, an
// address naming a node of the outline beside the view's own draws the body, which lands on the
// node, or says in the outline's place that the issue holds no such item. Each view is a
// stand-in, so the case reads the stage's rule whichever views their parts have filled.
test("under a view whose slot is not filled, an item's address beside the view's draws the body: it lands on the item's row, or says the issue holds none", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(noSuchComment());
  const trees = answerTreeReads(TWO_THREADS);
  try {
    for (const preset of VIEW_PRESETS) {
      (document.activeElement as HTMLElement | null)?.blur();
      const held = renderStage(`${presetPath(KEY, preset)}?comment=reply-b`, preset, false);
      const { container } = held.view;
      await waitFor(() =>
        expect(
          focusedRow(container),
          `the item's address beside the ${preset} view drew ${container.querySelector("[data-issue-body]") === null ? "no" : "the"} body and left the focus on ${document.activeElement?.tagName}`
        ).toBe("reply-b")
      );
      expect(standInViews(container), `the ${preset} view was drawn beside the body`).toEqual([]);
      held.view.unmount();

      (document.activeElement as HTMLElement | null)?.blur();
      const gone = renderStage(`${presetPath(KEY, preset)}?comment=${GONE_COMMENT}`, preset, false);
      const heading = await waitFor(
        () => gone.view.getByRole("heading", { name: "Comment not found" }),
        {
          onTimeout: () =>
            new Error(
              `the address beside the ${preset} view of an item the issue holds none of drew no line saying so; ${gone.view.container.querySelector("[data-issue-body]") === null ? "no" : "the"} body, with the focus on ${document.activeElement?.tagName}`
            ),
        }
      );
      await waitFor(() =>
        expect(
          document.activeElement,
          `the line that names no item beside the ${preset} view does not hold the focus`
        ).toBe(heading)
      );
      gone.view.unmount();
    }
  } finally {
    names.mockRestore();
    read.mockRestore();
    trees.mockRestore();
  }
});

// Once a view's part fills its slot, an address naming a node beside the view's own is that
// view's to draw, which lands on the node in its own tree: the stage draws the view and no body in
// its place, though the outline holds the node. The body is found by its own mark
// (`data-issue-body`), not by a tree, since in place of the outline it can draw a line.
test("under a view whose slot is filled, an item's address beside the view's draws the view and no body", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const trees = answerTreeReads(TWO_THREADS);
  try {
    for (const preset of VIEW_PRESETS) {
      const body = renderStage(`${presetPath(KEY, preset)}?comment=reply-b`, preset, true);
      await nextFrame();
      await nextFrame();
      const { container } = body.view;
      expect(
        container.querySelector("[data-issue-body]"),
        `the stage drew the body in place of the filled ${preset} view`
      ).toBeNull();
      expect(
        focusedRow(container),
        `the outline's landing ran under the filled ${preset} view`
      ).toBeUndefined();
      expect(standInViews(container), `the stage drew no ${preset} view, or another`).toEqual([
        preset,
      ]);
      body.view.unmount();
    }
  } finally {
    names.mockRestore();
    trees.mockRestore();
  }
});

// The architect's rule for an address of a node: it never leaves the reader on the page's body.
// A `#b-` fragment on the issue's own address naming no block the tree holds, and `/children` on
// an issue with none, landed nowhere and said nothing. Once the reads are held, each draws a
// line in the outline's place that says what the link names is not on this issue's page, with
// the focus on it, as an item the issue holds none of does.
test("a block or the children an address names that the tree holds none of draw a line saying so, with the focus on it", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    for (const [path, heading, line] of [
      [
        `/issues/${KEY}#b-no-such-block`,
        "No row for this block",
        `That link names no row of ${KEY}'s outline.`,
      ],
      [
        buildIssuePath({ key: KEY, kind: "children" }),
        "No child issues",
        `That link names the child issues of ${KEY}, which has none.`,
      ],
    ] as const) {
      (document.activeElement as HTMLElement | null)?.blur();
      const body = renderAddressedBody(path, TWO_THREADS);
      const title = await waitFor(
        () => body.view.getByRole("heading", { name: heading }),
        // The message names what the page drew instead.
        {
          onTimeout: () =>
            new Error(
              `${path} drew no "${heading}" line; ${body.view.queryByRole("tree") === null ? "no" : "an"} outline, with the focus on ${document.activeElement?.tagName}`
            ),
        }
      );
      await waitFor(() =>
        expect(document.activeElement, `${path}'s line does not hold the focus`).toBe(title)
      );
      expect(title.parentElement?.textContent).toContain(line);
      expect(body.view.queryByRole("tree"), `${path} drew the outline beside its line`).toBeNull();
      body.view.unmount();
    }
  } finally {
    names.mockRestore();
  }
});

// The class search of #4205's 6586a4be: the line `/children` draws on an issue with none left the
// page while a document added to the issue had its blocks read, which held the tree's reads out
// again (`readsHeld`), though the children came with the issue's own read alone then; it was
// drawn anew once that read answered, and took the focus again. The line stays drawn while the
// reads are out, with the focus on it. The page reads no document's blocks: a document added to
// the issue reaches it as the tree read sent again, as the document's creation reads it, which is
// the read out here, and the children come with that read too (`tree-read/tree-nodes.ts`'s
// `draftChild`). Sent again, that read keeps the answer it holds, and with it the reads held
// (`TreeRead.blocksHeld`); no path of the page drops that answer today, so the case drops it
// (`resetQueries`) to hold the reads out again while it is read.
test("the line that the issue has no children stays drawn, with the focus on it, while a document added to the issue has its blocks read", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const treeOut = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockReturnValue(treeOut.promise);
  try {
    const body = renderAddressedBody(buildIssuePath({ key: KEY, kind: "children" }), TWO_THREADS);
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "No child issues" })
    );
    await waitFor(() =>
      expect(heading, "the children's line does not hold the focus").toHaveFocus()
    );

    // The issue's read answers with a document added, its spec, and the tree read is sent again
    // for it with its answer dropped, and is out; no decisions document stands beside it, a
    // decision being a block of the spec.
    body.redraw(issueDetails());
    await act(async () => {
      void body.client.resetQueries({
        exact: true,
        queryKey: outlineQuery(KEY, "conversation").queryKey,
      });
    });
    await waitFor(() =>
      expect(tree, "the tree was never read again for the added document").toHaveBeenCalled()
    );
    await nextFrame();
    expect(
      heading.isConnected,
      "the children's line left the page while the tree was read again for the added document"
    ).toBe(true);
    expect(
      heading,
      "the focus left the children's line while the tree was read again for the added document"
    ).toHaveFocus();

    await act(async () => treeOut.resolve(treeReadOf(TWO_THREADS)));
    await nextFrame();
    expect(
      heading.isConnected,
      "the children's line was drawn anew once the tree read for the added document answered"
    ).toBe(true);
    expect(heading, "the focus left the children's line").toHaveFocus();
  } finally {
    tree.mockRestore();
    names.mockRestore();
  }
});

// The ids of an ask and a message no item of the issue holds, which their own reads answer for.
const GONE_ASK = "3c0d5a2e-7f41-4b8e-9d26-1e6a4f0b8c55";
const GONE_MESSAGE = "9a4e6c1b-2d73-4f58-b0e9-6c3a1d7f2e84";

// A following decides its fallback once. A comment the address landed on, deleted while the
// reader is on its row - the comments' read answering without it - left the tree, and the whole
// outline was replaced by the line that names no comment, the focus falling to the page's body.
// A following whose tree has held the address's node draws no fallback for the rest of it: the
// outline stays, and nothing reads the comment as the old page's landing would.
test("a comment the address landed on that leaves the tree leaves the outline drawn, and no line says it is not found", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(noSuchComment());
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const { container } = body.view;
    await waitFor(() => expect(focusedRow(container)).toBe("reply-b"));

    // The outline draws its turns from the tree read, so the comment leaves the tree read too.
    const without = TWO_THREADS.filter((comment) => comment.id !== "reply-b");
    await act(async () => {
      body.client.setQueryData(["comments", KEY], without);
      body.client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeReadOf(without));
    });
    await waitFor(() => expect(rowsFor(container, "reply-b")).toEqual([]));
    // Long enough for a read of the comment to have answered.
    await waitInAct(50);
    expect(
      body.view.queryByRole("heading", { name: "Comment not found" }),
      "the comment that left the tree replaced the outline with the line that names no comment"
    ).toBeNull();
    expect(rowsFor(container, "head-b"), "the outline left the page").not.toEqual([]);
    expect(
      read,
      "the comment the tree held was read as an item it may not hold"
    ).not.toHaveBeenCalled();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// A read of the comments that answers without the comment whose row holds the focus takes that
// row out of the tree, and the browser dropped the focus to the page's body with it: the reader's
// place in the outline was lost, and no row held the focus. The nearest row still drawn takes
// it, as a key's move gives it - the tab stop and the row's body go with it: the row that now
// stands where the dropped one stood, or the last row where the tree now ends before that place.
// The issue holds three comments, each a thread of its own, so each is a row of the tree's top
// level; `focus` is the key that takes the reader from the first row to `dropped`'s.
const THREE_COMMENTS: readonly Comment[] = [
  readComment("first", 1),
  readComment("second", 2),
  readComment("third", 3),
];
async function dropFocusedComment(
  focus: string,
  dropped: string
): Promise<{ readonly container: HTMLElement; readonly unmount: () => void }> {
  const user = userEvent.setup();
  const body = renderAddressedBody(`/issues/${KEY}`, THREE_COMMENTS);
  const { container } = body.view;
  await waitFor(() => expect(focusedRow(container)).toBe("first"));
  await user.keyboard(focus);
  await waitFor(() => expect(focusedRow(container)).toBe(dropped));

  // The outline draws its turns from the tree read, so the comment leaves the tree read too.
  const without = THREE_COMMENTS.filter((comment) => comment.id !== dropped);
  await act(async () => {
    body.client.setQueryData(["comments", KEY], without);
    body.client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeReadOf(without));
  });
  await waitFor(() => expect(rowsFor(container, dropped)).toEqual([]));
  return { container, unmount: body.view.unmount };
}

/** Whether the row of `id` is the reader's place in the tree: the tree's one tab stop, drawn with
 *  the focused row's look. */
async function expectReadersPlace(container: HTMLElement, id: string): Promise<void> {
  const row = rowsFor(container, id)[0];
  expect(row?.tabIndex, `the tree's tab stop is not on ${id}'s row, which holds the focus`).toBe(0);
  await waitFor(() =>
    expect(
      row === undefined ? false : looksFocused(row),
      `${id}'s row holds the focus and does not look focused`
    ).toBe(true)
  );
}

test("a read that drops the focused comment's row leaves the focus on the row that now stands in its place", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const { container, unmount } = await dropFocusedComment("j", "second");
    expect(
      focusedRow(container),
      "a read that dropped the focused comment's row left the focus on no row of the tree, or not on the next row"
    ).toBe("third");
    await expectReadersPlace(container, "third");
    unmount();
  } finally {
    names.mockRestore();
  }
});

test("a read that drops the focused comment's row, the tree's last, leaves the focus on the row before it", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const { container, unmount } = await dropFocusedComment("{End}", "third");
    expect(
      focusedRow(container),
      "a read that dropped the tree's last row, which held the focus, left the focus on no row of the tree, or not on the row before it"
    ).toBe("second");
    await expectReadersPlace(container, "second");
    unmount();
  } finally {
    names.mockRestore();
  }
});

// A read that drops the row being edited takes the editor's focus out with it, and the nearest
// row still drawn takes the focus, as for any row. The editing ends with it, as Escape ends it:
// the page draws the document's editor nowhere, the panel holds the facts of the row the focus
// went to, and the tree holds that row as any other. Left on, the editing waited for a row no
// read draws, and the tree held every row the focus came to as the edited one: a Tab out of the
// tree kept that row's body and its facts in Details.
test("a read that drops the row being edited ends the editing, and a Tab out of the tree lets go of the row the focus went to", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const last = node({ id: "comment-2", text: "The last words.", type: "comment" });
  const { view } = await caretInStatement(user, [last]);
  const { control } = controlOutside();
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  await act(async () => view.reread(node({ children: [first, last], id: KEY, type: "issue" })));
  expect(rowsFor(view.container, "spec#b-r1"), "the read did not drop the statement").toEqual([]);
  await waitFor(() =>
    expect(
      focusedRow(view.container),
      "the focus did not go to the row now in the dropped row's place"
    ).toBe("comment-2")
  );
  await waitFor(() =>
    expect(view.edited.at(-1), "the editing of the row the read dropped went on").toBeUndefined()
  );
  expect(
    document.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`),
    "the editor's box is still drawn"
  ).toBeNull();
  expect(
    view.focusedNode()?.id,
    "the panel does not hold the facts of the row the focus went to"
  ).toBe("comment-2");

  await user.tab();
  expect(document.activeElement, "Tab did not leave the outline").toBe(control);
  const row = rowsFor(view.container, "comment-2")[0];
  if (row === undefined) throw new Error("the last comment's row is not drawn");
  await waitFor(() =>
    expect(rowLetGo(row), "the row Tab left kept its body or its focused look").toBe(true)
  );
  expect(view.focusedNode(), "the panel still holds the facts of the row Tab left").toBeUndefined();
});

// The editing ends wherever the focus is once a read no longer draws the row being edited: in
// Details too, where a press leaves the editing on, and the tree, drawn again with the focus
// there, notes no row as holding it. The frame the editing's end asks for, to give the row the
// focus back, finds no row, and the focus stays in Details. Left on, the page held the document's
// editor for a row no read draws.
test("a read that drops the row being edited ends the editing with the focus in Details, and leaves the focus there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const card = cardInDetails();

  await user.click(card);
  // The press is over a task after its click.
  await waitInAct(0);
  expect(view.edited.at(-1), "a press in Details ended the editing").toBe("artifact-spec");
  await dropStatement(view);
  await waitFor(() =>
    expect(
      view.edited.at(-1),
      "the editing of the row the read dropped went on with the focus in Details"
    ).toBeUndefined()
  );
  // The frame the editing's end asks for comes.
  await nextFrame();
  expect(document.activeElement === card, "the focus left Details").toBe(true);
});

/** Draws the tree `caretInStatement` drew again as two reads do: one with a comment arrived after
 *  the statement, then one without the statement, the row being edited. Checks the statement's
 *  row is gone. */
async function dropStatement(view: {
  readonly container: HTMLElement;
  readonly reread: (next: TreeNode) => void;
}): Promise<void> {
  const first = node({ id: "comment-1", text: "The first words.", type: "comment" });
  const statement = node({
    id: "spec#b-r1",
    text: "A statement of the spec.",
    type: "requirement",
  });
  const arrived = node({ id: "comment-3", text: "A comment that arrived.", type: "comment" });
  await act(async () =>
    view.reread(node({ children: [first, statement, arrived], id: KEY, type: "issue" }))
  );
  await act(async () => view.reread(node({ children: [first, arrived], id: KEY, type: "issue" })));
  expect(rowsFor(view.container, "spec#b-r1"), "the read did not drop the statement").toEqual([]);
}

// A read that takes the row being edited out while a press on another row is held ends the
// editing at once, as such a read does wherever the focus is (above), and not once the press is
// over, as the press would: the editor's box has left the page with its row, so the editing's end
// moves nothing under the pointer. The press lands where the pointer is all the same - its click
// on the row it went down on - and the focus stays where the press put it. Left on until the
// press was over, the page held the document's editor for a row no read draws.
test("a read that takes the row being edited out during a press ends the editing at once, and the press lands where the pointer is", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const { view } = await caretInStatement(user);
  const words = rowsFor(view.container, "comment-1")[0]?.querySelector<HTMLElement>(
    "[data-node-words]"
  );
  if (words === null || words === undefined) throw new Error("the comment's row is not drawn");
  let clicks = 0;
  words.addEventListener("click", () => {
    clicks += 1;
  });

  await user.pointer({ keys: "[MouseLeft>]", target: words });
  expect(focusedRow(view.container), "the press did not focus the row it went down on").toBe(
    "comment-1"
  );
  await dropStatement(view);
  expect(
    view.edited.at(-1),
    "the read that took the row being edited out left its editing on while the press was held"
  ).toBeUndefined();
  expect(
    document.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`),
    "the editor's box is still drawn"
  ).toBeNull();
  expect(focusedRow(view.container), "the read took the focus off the row pressed").toBe(
    "comment-1"
  );

  await user.pointer({ keys: "[/MouseLeft]" });
  expect(clicks, "the press's click did not land on the row it went down on").toBe(1);
  // The press is over a task after its click.
  await waitInAct(0);
  await nextFrame();
  expect(focusedRow(view.container), "the focus left the row the press put it on").toBe(
    "comment-1"
  );
});

// What a following decided is its own: a following that landed on a comment's row leaves the
// next one, to an item the tree holds none of, to say so, and the line that following drew is
// not drawn for a later one while that one's read is out - each follows its own address.
test("each following decides its own fallback: what the one before it found or drew is none of its own", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(noSuchComment());
  const otherGone = "5d2f8a17-4c6e-4b39-a0d1-7e9b3c5f2a68";
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    await waitFor(() => expect(focusedRow(body.view.container)).toBe("reply-b"));

    await body.follow(buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }), {
      onTimeout: () =>
        new Error(
          "an address of a comment the tree holds none of, followed from a comment the reader landed on, drew no line saying so"
        ),
    });

    const other = Promise.withResolvers<never>();
    read.mockReturnValueOnce(other.promise);
    await body.follow(buildIssuePath({ id: otherGone, key: KEY, kind: "comment" }));
    await waitFor(() => expect(read).toHaveBeenCalledWith(otherGone));
    await nextFrame();
    expect(
      body.view.queryByRole("heading", { name: "Comment not found" }),
      "the line the following before drew was drawn for the next one while its read is out"
    ).toBeNull();
    await act(async () => other.reject(noSuchComment()));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// Window focus reads a read in error again (`refetchFailedOnFocus`), and a 404 is one: while it
// ran, the landing was pending again and the cache still counted the 404 as a failure, so the
// line that names no comment was swapped for the one that could not load it, and back. While a
// read runs again the following keeps drawing the landing it last settled on.
test("an item found missing keeps its line while window focus reads it again", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValue(noSuchComment());
  const body = renderAddressedBody(
    buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
    TWO_THREADS
  );
  try {
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "Comment not found" })
    );
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise);
    await act(async () => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await nextFrame();
    expect(
      body.view.queryByRole("heading", { name: "Couldn't load this comment" }),
      "the read run again on window focus drew the line that could not load the comment"
    ).toBeNull();
    expect(
      body.view.queryByRole("heading", { name: "Comment not found" }),
      "the read run again on window focus took away the line that names no comment"
    ).toBe(heading);

    await act(async () => again.reject(noSuchComment()));
    await waitInAct(20);
    expect(body.view.queryByRole("heading", { name: "Comment not found" })).toBe(heading);
    expect(body.view.queryByRole("heading", { name: "Couldn't load this comment" })).toBeNull();
  } finally {
    // The client stops listening for window focus first, so the focus given back reads nothing.
    body.view.unmount();
    focusManager.setFocused(undefined);
    names.mockRestore();
    read.mockRestore();
  }
});

// The failures are counted from the following's start. An item whose read failed, followed again
// after the reader had gone elsewhere, reads again, and while it ran the cache still counted the
// old failure: the line that could not load it was drawn, busy, for a read that then answered.
// A following with no landing settled yet draws nothing in the outline's place while its read is
// out, whatever reads before it failed.
test("an item followed again after its read failed draws no failure while its read runs again, nor once it answers", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValueOnce(serverDown());
  const address = buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" });
  try {
    const body = renderAddressedBody(address, TWO_THREADS);
    await waitFor(() => body.view.getByRole("heading", { name: "Couldn't load this comment" }));
    await body.follow(buildIssuePath({ key: KEY, kind: "issue" }));
    await waitFor(() => expect(rowsFor(body.view.container, "head-a")).not.toEqual([]));

    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise);
    await body.follow(address);
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await nextFrame();
    expect(
      body.view.queryByRole("heading", { name: "Couldn't load this comment" }),
      "the following drew the failure of the read before it while its own read runs"
    ).toBeNull();

    await act(async () => again.reject(noSuchComment()));
    await waitFor(() => body.view.getByRole("heading", { name: "Comment not found" }));
    expect(body.view.queryByRole("heading", { name: "Couldn't load this comment" })).toBeNull();
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// A link to the same item followed again draws its failed line anew from the failure the read
// had before the following, which reads nothing again. A Retry pressed there runs the read again,
// and its line stays drawn, busy, with the focus on the Retry, though this following has counted
// no failure of its own yet: the following keeps the landing it settled on while its read runs.
test("an item's failed line drawn for the same address followed again stays drawn, busy, while its Retry reads again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValueOnce(serverDown());
  const address = buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" });
  try {
    const body = renderAddressedBody(address, TWO_THREADS);
    await waitFor(() => body.view.getByRole("heading", { name: "Couldn't load this comment" }));
    await body.follow(address);
    const retry = await waitFor(() =>
      body.view.getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise);
    await user.click(retry);
    await waitFor(() =>
      expect(
        retry.getAttribute("aria-busy"),
        `the Retry of the address followed again is not busy while its read runs again; ${retry.isConnected ? "it is drawn" : "its line left the page"}`
      ).toBe("true")
    );
    expect(document.activeElement, "the Retry pressed lost the focus").toBe(retry);
    await act(async () => again.reject(serverDown()));
    await waitFor(() => expect(retry.getAttribute("aria-busy")).toBeNull());
    expect(retry.isConnected, "the failed line left the page once its read failed again").toBe(
      true
    );
    expect(read).toHaveBeenCalledTimes(2);
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The reader's press of Retry is an act, and the line drawn once its read answers replaced the
// line that held the focus, which fell to the page's body: the line that names no comment took
// no focus. A line drawn in place of a line of the fallback that held the focus takes it.
test("a Retry whose read answers that the item is gone leaves the focus on the line that says so", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getComment").mockRejectedValueOnce(serverDown());
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: GONE_COMMENT, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const retry = await waitFor(() =>
      body.view.getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise);
    await user.click(retry);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await act(async () => again.reject(noSuchComment()));
    const heading = await waitFor(() =>
      body.view.getByRole("heading", { name: "Comment not found" })
    );
    await waitFor(() =>
      expect(
        document.activeElement,
        "the line that names no comment, drawn in place of the Retry the reader pressed, left the focus on the page's body"
      ).toBe(heading)
    );
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// A Retry pressed on the failed read of an id written short, whose read then answers with the
// several items the id names: the list is drawn in place of the line that held the focus, which
// fell to the page's body with that line, and the list takes it, as the line that names no item
// does. No case held it: a list that took none still passed.
test("a Retry whose read answers that the id names several items leaves the focus on their list", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const read = spyOn(api, "getReferences").mockRejectedValueOnce(serverDown());
  const short = GONE_COMMENT.slice(0, 8);
  const match = (id: string, first_line: string): IDCandidate => ({
    first_line,
    id,
    issue_key: KEY,
    kind: "comment",
    ref: buildDispatchReference({ id, key: KEY, kind: "comment" }),
  });
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: short, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const retry = await waitFor(() =>
      body.view.getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise as never);
    await user.click(retry);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await act(async () =>
      again.reject(
        new ApiError(409, {
          candidates: [
            match(GONE_COMMENT, "The first note"),
            match(`${short}-0000-4000-8000-000000000002`, "The other note"),
          ],
          code: ID_AMBIGUOUS,
          error: `the id prefix "${short}" names 2 items; write more of the id`,
        })
      )
    );
    const list = await waitFor(() => {
      const drawn = body.view
        .getByRole("heading", { name: "Several items match that link" })
        .closest<HTMLElement>('[tabindex="-1"]');
      if (drawn === null) throw new Error("the list of matches is drawn in no focusable box");
      return drawn;
    });
    await waitFor(() =>
      expect(
        document.activeElement,
        "the list of matches, drawn in place of the Retry the reader pressed, left the focus on the page's body"
      ).toBe(list)
    );
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// Census row 8.54 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-21 03:47Z: the
// list of an id's matches stays drawn while its read runs again (`Following.settled`), and what it
// draws then is the refusal the cache holds, which every correction is written into
// (`markCorrectedPost`), never a list kept from before one. A match deleted while the list is
// drawn is drawn deleted, with none of its first line, while a later read is out; and so is one
// deleted while that read is out, which TanStack holds no refusal for: the delete gives the read
// up, writes its mark into the refusal that puts back, and holds it beside the read it asks again.
test("a match deleted after the list of an id's matches was drawn is drawn deleted while the list's read runs again", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const short = GONE_COMMENT.slice(0, 8);
  const other = `${short}-0000-4000-8000-000000000002`;
  const third = `${short}-0000-4000-8000-000000000003`;
  const match = (id: string, first_line: string): IDCandidate => ({
    first_line,
    id,
    issue_key: KEY,
    kind: "comment",
    ref: buildDispatchReference({ id, key: KEY, kind: "comment" }),
  });
  const read = spyOn(api, "getReferences").mockRejectedValueOnce(
    new ApiError(409, {
      candidates: [
        match(GONE_COMMENT, "The first note"),
        match(other, "The other note"),
        match(third, "The third note"),
      ],
      code: ID_AMBIGUOUS,
      error: `the id prefix "${short}" names 3 items; write more of the id`,
    })
  );
  try {
    const body = renderAddressedBody(
      buildIssuePath({ id: short, key: KEY, kind: "comment" }),
      TWO_THREADS
    );
    const { container } = body.view;
    const drawn = () => ({
      deleted: container.querySelectorAll("[data-post-deleted]").length,
      list: container.textContent?.includes("Several items match that link"),
      words: ["The first note", "The other note", "The third note"].filter((words) =>
        container.textContent?.includes(words)
      ),
    });
    await waitFor(() => expect(container.textContent).toContain("The first note"));

    act(() => markCorrectedPost(body.client, correction("comment", GONE_COMMENT, false)));
    await waitFor(() => expect(container.textContent).not.toContain("The first note"));
    // The read runs again, as window focus reads a read in error again, and stays out.
    const again = Promise.withResolvers<never>();
    read.mockReturnValueOnce(again.promise as never);
    act(() => {
      void body.client.refetchQueries({ queryKey: ["references", "to"] });
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    const whileOut = drawn();

    const asked = Promise.withResolvers<never>();
    read.mockReturnValueOnce(asked.promise as never);
    act(() => markCorrectedPost(body.client, correction("comment", other, false)));
    await waitFor(() => expect(read).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(container.textContent).not.toContain("The other note"));
    expect({ whileOut, deletedWhileOut: drawn() }).toEqual({
      whileOut: { deleted: 1, list: true, words: ["The other note", "The third note"] },
      deletedWhileOut: { deleted: 2, list: true, words: ["The third note"] },
    });
  } finally {
    names.mockRestore();
    read.mockRestore();
  }
});

// The landing on an address's row watched for the reader's acts only from the moment the row
// drew, so a row whose read answered late took the focus from a control the reader had moved to
// since they followed the address. Given no focus, it was still brought on screen, which took the
// page away from where the reader had it. A landing whose following saw an act before its row
// drew - a press, a key, a turn of the wheel - does nothing to the page: the row is drawn where it
// stands, the focus and the page where the reader left them. happy-dom lays nothing out, so the
// row is given a box 2,000 px below the window, which the page's one rule (`identity-block.ts`'s
// `bringOnScreen`) scrolls to.
test("a comment's row drawn after the reader acted since following its address moves neither the focus nor the page", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const comments = spyOn(api, "listComments");
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches('[data-node-row][data-node-id="reply-b"]')
      ? new DOMRect(0, window.innerHeight + 2000, 800, 40)
      : new DOMRect();
  });
  const scrolls = [
    spyOn(window, "scroll").mockImplementation(() => {}),
    spyOn(window, "scrollBy").mockImplementation(() => {}),
    spyOn(window, "scrollTo").mockImplementation(() => {}),
    spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {}),
  ];
  const outside = controlOutside();
  const acts = [
    ["a press on a control", () => user.click(outside.control), outside.control],
    [
      "a key pressed on a control",
      async () => {
        await act(async () => outside.control.focus());
        await user.keyboard("{Shift}");
      },
      outside.control,
    ],
    [
      "a turn of the wheel",
      () => act(async () => window.dispatchEvent(new Event("wheel"))),
      document.body,
    ],
  ] as const;
  try {
    for (const [what, run, stays] of acts) {
      const answer = Promise.withResolvers<Comment[]>();
      comments.mockReturnValue(answer.promise as never);
      const body = renderAddressedBody(
        buildIssuePath({ id: "reply-b", key: KEY, kind: "comment" }),
        undefined
      );
      await waitFor(() => expect(comments).toHaveBeenCalled());
      await run();
      expect(document.activeElement, `${what} left the focus elsewhere`).toBe(stays);
      for (const scroll of scrolls) scroll.mockClear();

      // The outline draws its turns from the tree read, which answers the comment with them.
      await act(async () => {
        answer.resolve([...TWO_THREADS]);
        body.client.setQueryData(
          outlineQuery(KEY, "conversation").queryKey,
          treeReadOf(TWO_THREADS)
        );
      });
      await waitFor(() => expect(rowsFor(body.view.container, "reply-b")).not.toEqual([]));
      await nextFrame();
      await nextFrame();
      expect(
        document.activeElement,
        `the comment's row, drawn after ${what}, took the focus from where it was`
      ).toBe(stays);
      expect(
        scrolls.flatMap<unknown>((scroll) => scroll.mock.calls),
        `the comment's row, drawn after ${what}, scrolled the page`
      ).toEqual([]);
      body.view.unmount();
      comments.mockClear();
      await act(async () => outside.control.blur());
    }
  } finally {
    names.mockRestore();
    comments.mockRestore();
    boxes.mockRestore();
    for (const scroll of scrolls) scroll.mockRestore();
  }
});

// An address that opens a document - the document's own address with a `#b-` fragment - names a
// block of that document, which the document is to land on: the outline in the body says nothing
// there of a block it draws no row for, and draws as it stands.
test("a block an address names beside the document it opens draws no line in the outline's place", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  try {
    const body = renderAddressedBody(
      `${buildIssuePath({ key: KEY, kind: "artifact", slug: "notes" })}#b-no-such-block`,
      TWO_THREADS
    );
    await nextFrame();
    await nextFrame();
    expect(
      body.view.queryByRole("heading", { name: "No row for this block" }),
      "the block an address names beside the document it opens drew the line that the outline holds no row of it"
    ).toBeNull();
    expect(rowsFor(body.view.container, "head-a"), "the outline drew no row").not.toEqual([]);
  } finally {
    names.mockRestore();
  }
});

// The line of an item whose read failed is busy while the read the old page's landing answers
// from runs again, read under that read's own key (`landingReadKey`): the graph read that
// resolves an id written short, and an ask's or a message's own read, as a comment's is above.
test("a failed read of an id written short, of an ask and of a message draws a Retry that is busy while it reads again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const reads = [
    {
      name: "a comment's id written short",
      path: buildIssuePath({ id: GONE_COMMENT.slice(0, 8), key: KEY, kind: "comment" }),
      read: spyOn(api, "getReferences"),
    },
    {
      name: "an ask",
      path: buildIssuePath({ id: GONE_ASK, key: KEY, kind: "ask" }),
      read: spyOn(api, "getAsk"),
    },
    {
      name: "a message",
      path: buildIssuePath({ id: GONE_MESSAGE, key: KEY, kind: "message" }),
      read: spyOn(api, "getMessage"),
    },
  ];
  try {
    for (const { name, path, read } of reads) {
      read.mockRejectedValueOnce(serverDown());
      const body = renderAddressedBody(path, TWO_THREADS);
      const retry = await waitFor(
        () => body.view.getByRole("button", { name: CONTROL_LABELS.retry }),
        { onTimeout: () => new Error(`${name}'s failed read drew no Retry`) }
      );
      const again = Promise.withResolvers<never>();
      read.mockReturnValueOnce(again.promise as never);
      await user.click(retry);
      await waitFor(() =>
        expect(
          retry.getAttribute("aria-busy"),
          `${name}'s Retry is not busy while its read runs again`
        ).toBe("true")
      );
      await act(async () => again.reject(serverDown()));
      await waitFor(() =>
        expect(
          retry.getAttribute("aria-busy"),
          `${name}'s Retry is still busy once its read failed again`
        ).toBeNull()
      );
      body.view.unmount();
    }
  } finally {
    names.mockRestore();
    for (const { read } of reads) read.mockRestore();
  }
});

// Under a view whose slot is not filled the stage draws the body only for an address that names
// a node and opens no document (`StageSlot`): the view's own address names no node, and an address
// that keeps the view's preset and opens a document - a block of the spec on the Spec view's own
// address, which the page opens the spec at - is that document's, so each draws the view and no
// body in its place. Which addresses open a document is read as the page reads them
// (`issueAddress`), among a block, `?mark=` and a pinned version beside each view's own address.
test("under a view whose slot is not filled, the view's own address and an address of it that opens a document draw the view, not the body", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const trees = answerTreeReads(TWO_THREADS);
  const issue = issueDetails();
  const specSlug = primarySpec(issue)?.slug;
  // The addresses read here that open a document, across every view.
  const opening: string[] = [];
  try {
    for (const preset of VIEW_PRESETS) {
      const own = presetPath(KEY, preset);
      const documents = [`${own}#b-r1`, `${own}?mark=reply-b`, `${own}?comment=reply-b&v=1`].filter(
        (path) => {
          const { hash, pathname, search } = new URL(path, "http://dispatch.test");
          const route = parseIssuePath(pathname, search);
          if (route === undefined) return false;
          const address = issueAddress(route, search, hash, specSlug);
          return address.preset === preset && address.document !== undefined;
        }
      );
      opening.push(...documents);
      for (const path of [own, ...documents]) {
        const body = renderStage(path, preset, false, issue);
        await nextFrame();
        const { container } = body.view;
        expect(
          container.querySelector("[data-issue-body]"),
          `${path} drew the body in place of the ${preset} view`
        ).toBeNull();
        expect(standInViews(container), `${path} drew no ${preset} view, or another`).toEqual([
          preset,
        ]);
        body.view.unmount();
      }
    }
    expect(opening, "no view's address read here opens a document").not.toEqual([]);
  } finally {
    names.mockRestore();
    trees.mockRestore();
  }
});

// The architect's ruling: a block of the spec on the Spec view's own address opens the Spec view
// at that block, never the outline, while the view's slot is not filled. The view is a stand-in,
// so the case reads the stage's rule apart from what the Spec view draws.
// It names the spec on purpose: the one view whose address can open a document (`routeDocument`).
test("a block of the spec on the Spec view's address draws the Spec view and no body while its slot is not filled", async () => {
  setViewport(1440);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const trees = answerTreeReads(TWO_THREADS);
  try {
    const body = renderStage(`/issues/${KEY}/spec#b-r1`, "spec", false, issueDetails());
    await nextFrame();
    const { container } = body.view;
    expect(
      container.querySelector("[data-issue-body]"),
      "the spec's block drew the body in place of the Spec view"
    ).toBeNull();
    expect(standInViews(container), "the spec's block drew no Spec view, or another").toEqual([
      "spec",
    ]);
    body.view.unmount();
  } finally {
    names.mockRestore();
    trees.mockRestore();
  }
});
