import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect, useRef } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { nextFrame } from "../../../__tests__/frames";
import { ApiError, api } from "../../../api/client";
import type {
  Actor,
  ArtifactText,
  AuthenticatedUser,
  Comment,
  Decision,
  DecisionKeep,
  IssueDecisions,
} from "../../../api/types";
import {
  pageFocusedRow,
  pageInkText,
  pageOwnWordsRule,
  pageStateText,
} from "../../../theme/classes";
import { actorLabelText } from "../../refs/AgentName";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import type { PublishedReferences } from "../../refs/reference-names";
import { useKeymapScope } from "../../shell/keymap";
import type { OutlineRequest } from "../IssueTree";
import { artifactsBySlug } from "../issue-artifacts";
import { type TreeNode, targetNaming } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import { OutlineRequestProvider } from "../outline-request";
import { KEY_REPORTS, PAGE_CHROME_ATTRIBUTE } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  EMPTY_STATE_LINES,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  STATE_LINES,
} from "../page-words";
import { presetView } from "../presets";
import { blockReads, Publisher, publishedNames } from "../projection-fixture";
import { ViewStage } from "../slots/stage";
import { AppProviders } from "../slots/stage-fixtures";
import { decisionBehind, nodesInOrder } from "../tree-keys";
import { overflowing, wordsScrollSheet } from "../words-scroll-fixtures";
import { DecisionsFilterBody } from "./DecisionsFilterBody";
import {
  answeredAsk,
  ask,
  closing,
  DECISIONS_DOCUMENT,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  READER_ACTOR,
  shaped,
  specRef,
  statement,
} from "./decisions-fixtures";
import type { DecisionsAsk } from "./decisions-parts";

/**
 * What the Decisions filter's body draws, read as a reader reads it. Which entry stands in which
 * part is the model's, read in `decisions-parts.test.ts`; these rows read the drawing: the four
 * parts under their headings, the words each state reads, Keep and Change on the review list
 * alone, the person's answer paragraph by paragraph as the store holds it, and where the focus
 * goes. How a row is laid out and where a long question is cut are the browser's to say, which
 * no DOM of a unit test lays out, so the browser suite's c-row, c-decisions and c-review-list
 * read them.
 */

/** The page's line for an issue the view draws nothing for, which the slot hands the body. */
const EMPTY_LINE = EMPTY_STATE_LINES.decisions;

/** The link every entry ends on: the page's words and the mark after them. */
const IN_CONVERSATION = `${DECISIONS_WORDS.in_conversation} ›`;

/** The foot's control, by the page's words it ends on, whatever count it holds. */
const FOOT_CONTROL = { name: (name: string) => name.endsWith(DECISIONS_WORDS.agents_many) };

/** What the person wrote, with the line breaks and the spaces they typed. */
const STORED = "Keep the rail.\n\n  It is 56 px wide -  no wider.\n";

const KEPT: DecisionKeep = {
  artifact_id: DECISIONS_DOCUMENT.id,
  block_id: "kept",
  changed_version: 1,
  kept_at: "2026-11-15T09:00:00Z",
  kept_by: READER,
};

/** One issue that holds an entry of every part: an open question, two decisions a statement
 *  rests on that wait, one the reader kept, an answered ask with the agent's write-up of it
 *  under it, a declined ask, and two decisions no statement rests on. */
function wholeIssue(): {
  asks: DecisionsAsk[];
  children: TreeNode[];
  made: Decision[];
} {
  return {
    asks: [
      closing("a-declined", {
        at: "2026-11-13T09:00:00Z",
        kind: "declined",
        reason: "The release owner decides this one.",
      }),
    ],
    children: [
      statement("s1"),
      statement("s2"),
      statement("s3"),
      openAsk("q-open", true, {
        options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
        text: "At which width does the header condense?",
      }),
      answeredAsk(
        "a-answered",
        { at: "2026-11-14T09:00:00Z", selected: ["Changes needed"], text: STORED },
        {
          children: [decisionNode("reading", { text: "The reader wants the rail kept." })],
          options: [{ label: "Works" }, { label: "Changes needed" }],
          text: "Does the built page work for you?",
        }
      ),
      ask("a-declined", { state: "declined", text: "On a phone, where does the box sit?" }),
      decisionNode("first", { at: "2026-11-10T12:00:00Z" }),
      decisionNode("second", { at: "2026-11-11T12:00:00Z", unsure: true }),
      decisionNode("kept"),
      decisionNode("minor"),
      decisionNode("replaced"),
    ],
    made: [
      decision("reading", { asks: ["a-answered"], kind: "person", waiting_review: false }),
      decision("first", {
        changed_at: "2026-11-10T12:00:00Z",
        links: [shaped(specRef("s1"))],
      }),
      decision("second", {
        changed_at: "2026-11-11T12:00:00Z",
        links: [shaped(specRef("s1")), shaped(specRef("s2")), shaped(specRef("s3"))],
        unsure: true,
      }),
      decision("kept", { kept: KEPT, links: [shaped(specRef("s2"))], waiting_review: false }),
      decision("minor"),
      decision("replaced", {
        links: [shaped(specRef("s1"))],
        replaced: true,
        waiting_review: false,
      }),
    ],
  };
}

const D = (block: string) => decisionNode(block).id;

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Where the page is, so a row that follows a link reads the address it went to. */
function Where(): ReactNode {
  const { hash, pathname } = useLocation();
  return <output data-testid="where">{`${pathname}${hash}`}</output>;
}

/** The app's main element, which focuses itself in an effect as it mounts, as the app focuses it
 *  at each page change but the first (`app.tsx`): React runs the effects of the body drawn in it,
 *  and so any focus the body places as it mounts, before this one. */
function PageChangeMain({ children }: { children: ReactNode }): ReactNode {
  const main = useRef<HTMLElement>(null);
  useEffect(() => {
    main.current?.focus();
  }, []);
  return (
    <main ref={main} tabIndex={-1}>
      {children}
    </main>
  );
}

interface Drawn extends RenderResult {
  readonly client: QueryClient;
  /** Draws the body again with other props, as the page does when its address moves. */
  readonly again: (props: BodyProps) => void;
}

interface BodyProps {
  /** The issue's nodes as a later read leaves them, where the tree grew since the body was
   *  first drawn (`useIssueTree.ts` builds it read by read); the body is drawn over that tree,
   *  with the view the slot hands it, unless `view` says otherwise. */
  readonly children?: readonly TreeNode[];
  /** Whether the body is on the page: false takes it off, as the page does when the view is
   *  turned off, and true draws a new one. */
  readonly drawn?: boolean;
  readonly isClosed?: boolean;
  /** What the page publishes of its batched names read, published to the names store and handed
   *  to the body, as the slot hands it: what it publishes for the tree's statements
   *  (`publishedStatements`) unless a case gives another. */
  readonly published?: PublishedReferences;
  readonly request?: OutlineRequest;
  readonly reveal?: string;
  readonly view?: TreeNode;
  readonly waitingPress?: number;
}

/** A Change a person sent on a decision: a comment of the issue anchored to the decision's own
 *  words in the decisions document (`DecisionActions.tsx`). */
function changeOf(block: string, at: string, body: string): Comment {
  return {
    anchor: {
      artifact_id: DECISIONS_DOCUMENT.id,
      block_id: null,
      mark_id: `mark-${block}`,
      orphaned: false,
      quote: `The decision ${block}.`,
      version: 1,
    },
    author: READER_ACTOR,
    body,
    created_at: at,
    edited_at: null,
    id: `change-${block}`,
  } as Comment;
}

/** The decisions document the decisions read of `made` stands for: its title, then each
 *  decision's heading and words as `made` orders them, the replaced ones after the rest and a
 *  level down under `## Replaced`, which is where the server's section rule finds each
 *  (`decision-review.ts`'s `decisionHeadings`). A Change's send reads it to say which place of
 *  its quote is the decision's own (`DecisionActions.tsx`, decision 92). */
function decisionsText(made: readonly Decision[]): string {
  const section = (level: string) => (row: Decision) => `${level} ${row.heading}\n\n${row.text}`;
  const replaced = made.filter((row) => row.replaced);
  return [
    `# Decisions of ${KEY}`,
    ...made.filter((row) => !row.replaced).map(section("##")),
    ...(replaced.length === 0 ? [] : ["## Replaced", ...replaced.map(section("###"))]),
  ].join("\n\n");
}

/** What the page publishes of its one batched read for every statement `root` holds
 *  (`useIssueTree.ts`): each named by the reading of its words the read sends for a block
 *  (`text_tree`), as the page names it (`targetNaming`), so the names store projects the line a
 *  decision's "informs" line names it by (`KnownReferences.lines`). The engine must have loaded
 *  (`displayTree`). */
function publishedStatements(root: TreeNode): PublishedReferences {
  const statements = nodesInOrder(root).flatMap((node) => {
    if (node.type !== "requirement") return [];
    const tree = node.tree ?? displayTree(node.text);
    return [tree === undefined ? node : { ...node, tree }];
  });
  return {
    ...targetNaming(blockReads(issueDetails(), statements)),
    asked: new Set(
      statements.flatMap(({ reference }) => (reference === undefined ? [] : [reference]))
    ),
  };
}

/** The body over the issue whose tree holds `children`, with `asks`, `comments` and the
 *  decisions read of `made` as the cache holds them. `made` or `asks` null leaves that read out
 *  of the cache, as before it answers. It is drawn under the names store, with what the page
 *  publishes for the tree's statements published to it and handed to the body
 *  (`publishedStatements`), where a case gives nothing else, and with no names line. */
function renderBody(
  {
    asks = [],
    children,
    comments = [],
    made = [],
  }: {
    asks?: DecisionsAsk[] | null;
    children: TreeNode[];
    comments?: Comment[];
    made?: Decision[] | null;
  },
  props: BodyProps = {},
  {
    answeredAgo,
    main = false,
    requests = false,
    stage = false,
  }: {
    /** How many milliseconds ago the cached reads answered, under the app's own `staleTime` of
     *  30 s (`main.tsx`) in place of the default here, where no answer ever goes stale. */
    answeredAgo?: number;
    /** Whether the body is drawn in the app's main element, which takes the focus as the body
     *  mounts, after the body's own first effects, as on a page opened by a link inside the app
     *  (`PageChangeMain`). */
    main?: boolean;
    /** Whether the page's outline requests are held above the body, as the issue page holds
     *  them (`slots/providers.tsx`), with a control beside the body to take the focus to. */
    requests?: boolean;
    /** Whether the body is drawn in the stage the page draws a view in (`slots/stage.tsx`),
     *  which gives the focus to the tab-stop row or a refused read's Retry where nothing holds
     *  it or the tree holds it with no row, and keeps it in the view as a refused read's line
     *  leaves with it. */
    stage?: boolean;
  } = {}
): Drawn {
  const client = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: answeredAgo === undefined ? Number.POSITIVE_INFINITY : 30_000,
      },
    },
  });
  const answered = answeredAgo === undefined ? undefined : { updatedAt: Date.now() - answeredAgo };
  // The reads the page's tree is built from, under the keys it reads them by.
  if (made !== null) client.setQueryData(["decisions", KEY], decisionsRead(made), answered);
  if (asks !== null) client.setQueryData(["asks", KEY], asks, answered);
  client.setQueryData(["comments", KEY], comments, answered);
  // The decisions document's blocks (`issueDetails` names it by its role): none, so a Change is
  // placed by its quote alone, and no case makes a request for them.
  client.setQueryData(["artifact", DECISIONS_DOCUMENT.id, "blocks"], [], answered);
  // Who is reading, as the page's first read of it left it, where the reads are dated: the page
  // read it as it loaded, before any view was opened.
  if (answered !== undefined) {
    client.setQueryData(
      ["whoami"],
      { event_head: "0", kind: "user", login: READER } satisfies AuthenticatedUser,
      answered
    );
  }
  const issue = issueDetails();
  const root = issueTree(children);
  const rootPublished = publishedStatements(root);
  // The view the slot hands the body where no address names a node and no words are typed
  // (`slots/view-decisions.tsx`).
  const decisionsView = presetView(root, "decisions", "", new Set()).root;
  const body = ({
    children: grown,
    drawn = true,
    isClosed = false,
    published: given,
    request,
    reveal,
    view,
    waitingPress,
  }: BodyProps) => {
    const tree = grown === undefined ? root : issueTree(grown);
    const published = given ?? (grown === undefined ? rootPublished : publishedStatements(tree));
    const shown =
      view ??
      (grown === undefined ? decisionsView : presetView(tree, "decisions", "", new Set()).root);
    const list = drawn ? (
      <DecisionsFilterBody
        artifacts={artifactsBySlug(issue)}
        emptyLine={EMPTY_LINE}
        holdsEscape={false}
        isClosed={isClosed}
        issue={issue}
        namesLine={NO_FAILED_READS}
        published={published}
        reader={READER}
        {...(request === undefined ? {} : { request })}
        {...(reveal === undefined ? {} : { reveal })}
        root={tree}
        view={shown}
        {...(waitingPress === undefined ? {} : { waitingPress })}
      />
    ) : null;
    const staged = stage ? <ViewStage>{list}</ViewStage> : list;
    const held = main ? <PageChangeMain>{staged}</PageChangeMain> : staged;
    return (
      <MemoryRouter>
        <AppProviders client={client}>
          <Publisher published={published} />
          <IssueScope />
          <Where />
          {/* Inside `NoTooltips`, as the view's slot draws it, so an author is named as the
              page names one. */}
          <NoTooltips>
            {requests ? (
              <OutlineRequestProvider>
                <button type="button">Elsewhere</button>
                {held}
              </OutlineRequestProvider>
            ) : (
              held
            )}
          </NoTooltips>
        </AppProviders>
      </MemoryRouter>
    );
  };
  const view = render(body(props));
  return { ...view, again: (next) => view.rerender(body(next)), client };
}

/** An element's text as a reader reads it: every run of white space one space. */
function words(element: Element | null | undefined): string | null {
  return element == null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

/** What the body says of each read that stands refused, in the page's words, by which a reader
 *  tells the two reads' lines apart. */
const REFUSED_READ_WORDS = {
  asks: STATE_LINES.asks_unreadable,
  decisions: STATE_LINES.issue_decisions_unreadable,
} as const;

/** The page's line for a failed read that the body draws for `read`, found by what it says, as
 *  a reader finds it; null where the body draws none for `read`. */
function failedLine(drawn: RenderResult, read: keyof typeof REFUSED_READ_WORDS): Element | null {
  return drawn.queryByText(REFUSED_READ_WORDS[read])?.closest("[data-failed-read]") ?? null;
}

/** The Retry of the line a refused read draws, or null where the body draws none for `read`. */
function retryOf(drawn: RenderResult, read: keyof typeof REFUSED_READ_WORDS): HTMLElement | null {
  return failedLine(drawn, read)?.querySelector<HTMLElement>("button") ?? null;
}

/** The alert of the line a refused read draws, which a screen reader says as it is drawn, or
 *  null where the body draws none for `read`. */
function alertOf(drawn: RenderResult, read: keyof typeof REFUSED_READ_WORDS): Element | null {
  return failedLine(drawn, read)?.querySelector('[role="alert"]') ?? null;
}

/**
 * Presses Enter on the Retry of `read`'s line, which holds the focus, and holds the run that
 * press starts until it refuses it: `hold` makes that read's request return the promise it is
 * handed. The read stands refused all the while, so its line stays drawn with the same Retry,
 * which holds the focus: busy while the run is out, and no longer once the run is refused.
 */
async function retryRefusedAgain(
  drawn: RenderResult,
  read: keyof typeof REFUSED_READ_WORDS,
  hold: (run: Promise<never>) => void,
  user: UserEvent
): Promise<void> {
  const retry = retryOf(drawn, read);
  expect(retry).not.toBeNull();
  expect(document.activeElement).toBe(retry);
  const run = Promise.withResolvers<never>();
  hold(run.promise);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(retry?.getAttribute("aria-busy")).toBe("true"));
  expect(retryOf(drawn, read)).toBe(retry);
  expect(document.activeElement).toBe(retry);
  await act(async () => {
    run.reject(new Error("refused"));
    await run.promise.catch(() => undefined);
  });
  await waitFor(() => expect(retry?.getAttribute("aria-busy")).toBeNull());
  expect(retryOf(drawn, read)).toBe(retry);
  expect(document.activeElement).toBe(retry);
}

function rowOf(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
  if (row === null) throw new Error(`the body draws no row for ${id}`);
  return row;
}

/** The id of each row one part draws, in the order it draws them. */
function rowsOfPart(view: RenderResult, part: string): string[] {
  return [
    ...view.container.querySelectorAll(`[data-decisions-part="${part}"] [data-node-row]`),
  ].map((row) => row.getAttribute("data-node-id") ?? "");
}

function focusedRow(view: RenderResult): string | undefined {
  const active = document.activeElement;
  return [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .find((row) => row === active || row.contains(active))
    ?.getAttribute("data-node-id") as string | undefined;
}

/** Focuses the view's `tree` as the page's chord does where the tree holds no tab-stop row
 *  (`reachPart`, `useIssueKeymap.ts`): from outside the view, so the focus is moved off the view
 *  first. The stage leaves on the tree a focus moved onto it from inside the view, from a Retry
 *  the stage focused say, as the reader's own (`slots/stage.tsx`). */
function chordFocus(tree: HTMLElement): void {
  (document.activeElement as HTMLElement | null)?.blur();
  tree.focus();
}

/** What the page writes of its own inside `scope`, each run with the author's text taken out,
 *  as the browser suite's c-plain-words reads the page's chrome. */
function chromeOf(scope: Element): string[] {
  return [...scope.querySelectorAll("[data-page-chrome]")]
    .map((element) => {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll("[data-author-text]")) authored.remove();
      return (copy.textContent ?? "").replace(/\s+/g, " ").trim();
    })
    .filter((text) => text !== "");
}

// The rows' words, their parents' lines and the statements' names are drawn from readings, which
// the page's engine reads and projects, so it is loaded before any row runs.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R2, c-decisions: four parts under their own headings and in this order: the open questions,
// the decisions waiting for review, the reader's own decisions newest first, and the folded
// foot. No other sentence stands over them or between them (R22).
test("the four parts are drawn under their headings, in order, and no sentence explains them", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());

  const parts = [...view.container.querySelectorAll("[data-decisions-part]")].map((part) => ({
    count: words(part.querySelector("[data-decisions-count]")),
    heading: words(part.querySelector("[data-decisions-heading] [data-page-chrome], button")),
    part: part.getAttribute("data-decisions-part"),
    rows: [...part.querySelectorAll("[data-node-row]")].map((row) =>
      row.getAttribute("data-node-id")
    ),
  }));
  expect(parts).toEqual([
    {
      count: "1",
      heading: DECISIONS_WORDS.parts.open_questions,
      part: "open_questions",
      rows: ["q-open"],
    },
    // Newest first, by the time each was made.
    {
      count: "2",
      heading: DECISIONS_WORDS.parts.review,
      part: "review",
      rows: [D("second"), D("first")],
    },
    // The decision the reader kept, then the asks by the time each closed.
    {
      count: "3",
      heading: DECISIONS_WORDS.parts.yours,
      part: "yours",
      rows: [D("kept"), "a-answered", "a-declined"],
    },
    // Folded at the foot: its heading says how many it holds, and none is drawn.
    {
      count: "2",
      heading: `2 ${DECISIONS_WORDS.agents_many}`,
      part: "agents",
      rows: [],
    },
  ]);

  // The headings are the whole of what the page says of the parts: outside the rows, the body
  // writes nothing else of its own, and no paragraph stands over or between them.
  const outside = view.container.querySelector("[data-decisions-body]")?.cloneNode(true) as Element;
  for (const row of outside.querySelectorAll("[data-node-row]")) row.remove();
  expect(chromeOf(outside)).toEqual([
    DECISIONS_WORDS.parts.open_questions,
    DECISIONS_WORDS.parts.review,
    DECISIONS_WORDS.parts.yours,
    DECISIONS_WORDS.agents_many,
  ]);
  expect(outside.querySelectorAll("p").length).toBe(0);

  // One landmark named by the filter holds the list (R19), and in it one tree under the
  // outline's own name, each headed part a group named by its heading, and every entry a row
  // of it.
  expect(view.getByRole("region", { name: PRESET_NAMES.decisions })).toBe(
    view.container.querySelector("[data-decisions-body]") as HTMLElement
  );
  const tree = view.getByRole("tree", { name: CONTROL_LABELS.tree });
  expect(view.getAllByRole("tree").length).toBe(1);
  expect(
    [...tree.querySelectorAll('[role="group"]')].map((group) =>
      words(
        view.container.ownerDocument.getElementById(group.getAttribute("aria-labelledby") ?? "")
      )
    )
  ).toEqual([
    DECISIONS_WORDS.parts.open_questions,
    DECISIONS_WORDS.parts.review,
    DECISIONS_WORDS.parts.yours,
  ]);
  expect(
    [...tree.querySelectorAll("[data-node-row]")].every(
      (row) => row.getAttribute("role") === "treeitem"
    )
  ).toBe(true);

  // A press on a part's heading or on its count is a press on no entry, so it moves no focus:
  // with the focus on no row, no row takes it, and with it on a row, it stays there.
  await user.click(view.getByText(DECISIONS_WORDS.parts.review));
  expect(focusedRow(view)).toBeUndefined();
  const count = view.container.querySelector(
    '[data-decisions-part="yours"] [data-decisions-count]'
  );
  if (count === null) throw new Error("the reader's own decisions draw no count");
  await user.click(count);
  expect(focusedRow(view)).toBeUndefined();
  await act(async () => rowOf(view, D("first")).focus());
  await user.click(view.getByText(DECISIONS_WORDS.parts.open_questions));
  expect(focusedRow(view)).toBe(D("first"));
  // A press on a link of another row moves nothing while it is held: the row the reader is on
  // keeps its body open until the press is over, and the pressed row takes the focus then, so
  // the press ends on the link it went down on, and the link is followed.
  const pressedRow = rowOf(view, D("kept"));
  const pressedLink = pressedRow.querySelector<HTMLElement>("a[data-entry-link]");
  if (pressedLink === null) throw new Error("the kept decision draws no link");
  await user.pointer({ keys: "[MouseLeft>]", target: pressedLink });
  expect(rowOf(view, D("first")).querySelector("[data-node-body]")).not.toBeNull();
  expect(pressedRow.querySelector("[data-node-body]")).toBeNull();
  await user.pointer({ keys: "[/MouseLeft]", target: pressedLink });
  await waitFor(() => expect(pressedRow.querySelector("[data-node-body]")).not.toBeNull());
  expect(focusedRow(view)).toBe(D("kept"));
  expect(rowOf(view, D("first")).querySelector("[data-node-body]")).toBeNull();
  expect(view.getByTestId("where").textContent).toBe(`/issues/${KEY}#b-kept`);

  // The foot unfolds from its own control, which is a plain button in the tab order, and
  // folds again. Its rows stand after it and are the tree's own.
  const foot = view.getByRole("button", FOOT_CONTROL);
  // The mark before the count is a shape hidden from assistive technology, not text: the
  // page's typeface keeps neither triangle. Its path is what says whether the foot is open.
  const mark = () =>
    foot.querySelector('svg[aria-hidden="true"] path')?.getAttribute("d") ?? "no mark";
  const folded = mark();
  expect(folded).not.toBe("no mark");
  expect(foot.textContent).not.toMatch(/[▾▸]/u);
  expect(foot.getAttribute("aria-expanded")).toBe("false");
  expect(foot.hasAttribute("tabindex")).toBe(false);
  expect(tree.hasAttribute("aria-owns")).toBe(false);
  await user.click(foot);
  expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
  expect(foot.getAttribute("aria-expanded")).toBe("true");
  expect(words(foot)).toBe(`2 ${DECISIONS_WORDS.agents_many}`);
  expect(mark()).not.toBe("no mark");
  expect(mark()).not.toBe(folded);
  expect(foot.textContent).not.toMatch(/[▾▸]/u);
  const footRows = view.container.querySelector('[data-decisions-part="agents"] [role="group"]');
  expect(tree.getAttribute("aria-owns")).toBe(footRows?.id ?? "no group");
  expect(footRows?.getAttribute("aria-labelledby")).toBe(foot.id);
  await user.click(foot);
  expect(rowsOfPart(view, "agents")).toEqual([]);
  expect(mark()).toBe(folded);

  // Tab from the row the reader stands on goes to the foot's control, which is inside the
  // list: the row is drawn as focused no more, its body closed and its card plain.
  const first = rowOf(view, D("first"));
  await act(async () => first.focus());
  expect(first.querySelector("[data-node-body]")).not.toBeNull();
  expect(first.querySelector("[data-node-card]")).toHaveClass(pageFocusedRow);
  await user.tab();
  expect(document.activeElement).toBe(foot);
  expect(first.querySelector("[data-node-body]")).toBeNull();
  expect(first.querySelector("[data-node-card]")).not.toHaveClass(pageFocusedRow);
});

/** An issue whose only decision is an agent's that no statement rests on: the list draws its
 *  foot and no row. */
const FOOT_ONLY = { children: [decisionNode("minor")], made: [decision("minor")] };

/** The foot's control, or null where the list draws no foot. */
function footControlOf(drawn: RenderResult): HTMLElement | null {
  return drawn.container.querySelector<HTMLElement>('[data-decisions-part="agents"] button');
}

// R22: the foot's words agree with its count.
test("the foot's heading agrees with how many decisions it holds", () => {
  const view = renderBody(FOOT_ONLY);
  expect(words(footControlOf(view))).toBe(`1 ${DECISIONS_WORDS.agents_one}`);
});

// R2: a part that holds nothing is not drawn, and an issue that holds no ask and no decision
// says so in the page's one line for that. A read refused before it first answers says so in a
// line of its own instead, with a Retry, and the focus stays in the list.
test("a part that holds nothing is not drawn, and an empty issue says so once", async () => {
  const some = renderBody({ children: [openAsk("q-open")] });
  expect(
    [...some.container.querySelectorAll("[data-decisions-part]")].map((part) =>
      part.getAttribute("data-decisions-part")
    )
  ).toEqual(["open_questions"]);
  expect(some.queryByText(EMPTY_LINE)).toBeNull();
  some.unmount();

  const none = renderBody({ children: [statement("s1")] });
  expect(none.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
  expect(none.getByText(EMPTY_LINE)).toBeTruthy();
  // The outline's tree is still on the page, as it is under every filter.
  expect(none.getByRole("tree", { name: CONTROL_LABELS.tree })).toBeTruthy();
  none.unmount();

  // The decisions read the mount starts never answers. Until it does, an issue with no open
  // question may be one whose decisions have not arrived, so no line says it holds nothing.
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockReturnValue(
    Promise.withResolvers<never>().promise
  );
  try {
    const cold = renderBody({ children: [statement("s1")], made: null });
    await act(() => Promise.resolve());
    expect(getIssueDecisions).toHaveBeenCalledTimes(1);
    expect(cold.queryByText(EMPTY_LINE)).toBeNull();
    // Its answer, which holds no decision, comes into the cache, and the line is drawn.
    await act(async () => cold.client.setQueryData(["decisions", KEY], decisionsRead([])));
    expect(cold.getByText(EMPTY_LINE)).toBeTruthy();
    cold.unmount();
  } finally {
    getIssueDecisions.mockRestore();
  }

  const user = userEvent.setup();

  // The decisions read is refused before it first answers. In place of the decisions it would
  // give, its line says so over the list with a Retry that asks that read alone again, and no
  // line says the issue holds nothing. The body is drawn in the stage, as the page draws a
  // view: the tree's own focus, which `t d` gives it from outside the view, goes to that Retry.
  // Enter on it asks again, and while that run is out the read stands refused: its line stays,
  // its Retry busy and still holding the focus, and once the run is refused too that same Retry
  // holds it. Enter asks once more; the read answers this time, and its decision is drawn with
  // the focus on its row: at no point is the focus left on the page's body.
  const made = [decision("first", { links: [shaped(specRef("s1"))] })];
  const refused = spyOn(api, "getIssueDecisions").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the store is not reachable" })
  );
  try {
    const failed = renderBody({ children: [statement("s1")], made: null }, {}, { stage: true });
    await waitFor(() => expect(failedLine(failed, "decisions")).not.toBeNull());
    expect(words(retryOf(failed, "decisions"))).toBe("Retry");
    expect(failedLine(failed, "asks")).toBeNull();
    expect(failed.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
    expect(failed.queryByText(EMPTY_LINE)).toBeNull();
    const tree = failed.getByRole("tree", { name: CONTROL_LABELS.tree });
    expect(tree.contains(retryOf(failed, "decisions"))).toBe(false);
    await act(async () => chordFocus(tree));
    expect(document.activeElement).toBe(retryOf(failed, "decisions"));

    await retryRefusedAgain(failed, "decisions", (run) => refused.mockReturnValue(run), user);
    refused.mockResolvedValue(decisionsRead(made));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(failedLine(failed, "decisions")).toBeNull());
    expect(failed.client.getQueryData(["decisions", KEY])).toBeDefined();
    expect(refused).toHaveBeenCalledTimes(3);
    // The line is gone and the tree the page builds from that read does not hold its decision
    // yet: the tree holds the focus, in the list.
    await waitFor(() => expect(document.activeElement).toBe(tree));
    failed.again({ children: [statement("s1"), decisionNode("first")] });
    await waitFor(() => expect(focusedRow(failed)).toBe(D("first")));
    failed.unmount();
  } finally {
    refused.mockRestore();
  }

  // The asks read is refused the same way: its line in place of the questions it would give,
  // no line that the issue holds nothing, and the tree's focus, which `t d` gives it from
  // outside the view, on its Retry where no row is drawn.
  const refusedAsks = spyOn(api, "listIssueAsks").mockRejectedValue(new Error("refused"));
  try {
    const failed = renderBody({ asks: null, children: [statement("s1")] }, {}, { stage: true });
    await waitFor(() => expect(failedLine(failed, "asks")).not.toBeNull());
    expect(words(retryOf(failed, "asks"))).toBe("Retry");
    expect(failedLine(failed, "decisions")).toBeNull();
    expect(failed.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
    expect(failed.queryByText(EMPTY_LINE)).toBeNull();
    await act(async () => chordFocus(failed.getByRole("tree", { name: CONTROL_LABELS.tree })));
    expect(document.activeElement).toBe(retryOf(failed, "asks"));
    failed.unmount();

    // With the decisions read drawn, its rows take the focus and the Retry does not: a focus on
    // the tree from outside the view, as the page's chord gives one where it finds no tab-stop
    // row, or on the tab-stop row itself goes to a decision. Shift+Tab from that row takes the
    // focus up to the Retry over the list, which is off every row, as the foot's control is, so
    // the row the reader left is drawn as one they are not on.
    const partial = renderBody({
      asks: null,
      children: [statement("s1"), decisionNode("first")],
      made,
    });
    await waitFor(() => expect(retryOf(partial, "asks")).not.toBeNull());
    await act(async () => chordFocus(partial.getByRole("tree", { name: CONTROL_LABELS.tree })));
    expect(focusedRow(partial)).toBe(D("first"));
    await act(async () => (document.activeElement as HTMLElement).blur());
    const row = rowOf(partial, D("first"));
    await act(async () => row.focus());
    expect(focusedRow(partial)).toBe(D("first"));
    expect(row.querySelector("[data-node-body]")).not.toBeNull();
    expect(row.querySelector("[data-node-card]")).toHaveClass(pageFocusedRow);
    await user.tab({ shift: true });
    expect(document.activeElement).toBe(retryOf(partial, "asks"));
    expect(row.querySelector("[data-node-body]")).toBeNull();
    expect(row.querySelector("[data-node-card]")).not.toHaveClass(pageFocusedRow);
    expect(focusedRow(partial)).toBeUndefined();
    partial.unmount();
  } finally {
    refusedAsks.mockRestore();
  }

  // A list that holds only the foot, with the asks read refused before it first answers. A
  // question or the asks' Retry may yet come, so the foot's control takes no focus until that
  // read has answered: the tree's focus, which `t d` gives it from outside the view before the
  // refusal comes back, goes to the Retry once its line is drawn, and a page opened on the list
  // puts it there too. Enter on the Retry with the read refused again leaves its line drawn
  // while the read runs, its Retry busy and still holding the focus, and that same Retry holds
  // it once the run is refused. Enter with the read answering no question takes the line off
  // the page with the focus on its Retry and leaves a list that holds only the foot with both
  // reads held, so the focus goes on from the tree to the foot's control; a later read that adds
  // a question takes it from there to the question's row, as the foot hands it on. Where the
  // read's answer brings the question with it, drawn in the one render, the focus goes from the
  // busy Retry to the question's row, and the foot's control takes it at no point on the way.
  const asksOut = Promise.withResolvers<never>();
  const asksRead = spyOn(api, "listIssueAsks").mockReturnValue(asksOut.promise);
  try {
    const early = renderBody({ ...FOOT_ONLY, asks: null }, {}, { stage: true });
    expect(footControlOf(early)).not.toBeNull();
    const tree = early.getByRole("tree", { name: CONTROL_LABELS.tree });
    await act(async () => chordFocus(tree));
    expect(document.activeElement).toBe(tree);
    await act(async () => {
      asksOut.reject(new Error("refused"));
      await asksOut.promise.catch(() => undefined);
    });
    await waitFor(() => expect(document.activeElement).toBe(retryOf(early, "asks")));
    early.unmount();

    asksRead.mockRejectedValue(new Error("refused"));
    const opened = renderBody({ ...FOOT_ONLY, asks: null }, {}, { stage: true });
    await waitFor(() => expect(document.activeElement).toBe(retryOf(opened, "asks")));
    await retryRefusedAgain(opened, "asks", (run) => asksRead.mockReturnValue(run), user);

    asksRead.mockResolvedValue([]);
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(opened.client.getQueryData<readonly unknown[]>(["asks", KEY])).toEqual([])
    );
    await waitFor(() => expect(document.activeElement).toBe(footControlOf(opened)));
    opened.again({ children: [openAsk("q-open"), decisionNode("minor")] });
    await waitFor(() => expect(focusedRow(opened)).toBe("q-open"));
    opened.unmount();

    asksRead.mockRejectedValue(new Error("refused"));
    const answered = renderBody({ ...FOOT_ONLY, asks: null }, {}, { stage: true });
    await waitFor(() => expect(document.activeElement).toBe(retryOf(answered, "asks")));
    const answer = Promise.withResolvers<never[]>();
    asksRead.mockReturnValue(answer.promise);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retryOf(answered, "asks")?.getAttribute("aria-busy")).toBe("true"));
    expect(document.activeElement).toBe(retryOf(answered, "asks"));
    // The question's row is the one element that takes the focus while the tree grows and the
    // read answers, so the foot's control never takes it on the way.
    const tookFocus: (EventTarget | null)[] = [];
    const recordFocus = (event: Event) => {
      tookFocus.push(event.target);
    };
    document.addEventListener("focusin", recordFocus);
    try {
      await act(async () => {
        // The tree grows first and its render waits for the act, so the read's answer, taken
        // before the act ends, is drawn in the one render with the question, as the page's
        // tree built from that answer draws them.
        answered.again({ children: [openAsk("q-open"), decisionNode("minor")] });
        answer.resolve([]);
        // The read's run (`Query.promise`): the query takes its answer as the run ends, first.
        await answered.client.getQueryCache().find({ queryKey: ["asks", KEY] })?.promise;
      });
    } finally {
      document.removeEventListener("focusin", recordFocus);
    }
    expect(
      tookFocus.map((target) =>
        target === rowOf(answered, "q-open")
          ? "the question's row"
          : target === footControlOf(answered)
            ? "the foot's control"
            : "another element"
      )
    ).toEqual(["the question's row"]);
    await waitFor(() => expect(focusedRow(answered)).toBe("q-open"));
    answered.unmount();
  } finally {
    asksRead.mockRestore();
  }

  // A refused read asked again by no Retry, as by a refetch the page makes of its own, stands
  // refused while it runs, as under a Retry: its line stays, the Retry busy and still holding
  // the focus. The line leaves with the focus on its Retry once the read answers: the focus goes
  // to the next refused read's Retry, and from the last one to the tree, never to the page's
  // body.
  const decisionsRefused = spyOn(api, "getIssueDecisions").mockRejectedValue(new Error("refused"));
  const asksRefused = spyOn(api, "listIssueAsks").mockRejectedValue(new Error("refused"));
  try {
    const both = renderBody(
      { asks: null, children: [statement("s1")], made: null },
      {},
      { stage: true }
    );
    await waitFor(() => expect(retryOf(both, "decisions")).not.toBeNull());
    await waitFor(() => expect(retryOf(both, "asks")).not.toBeNull());
    const retry = retryOf(both, "decisions");
    await act(async () => retry?.focus());
    expect(document.activeElement).toBe(retry);
    const decisionsAnswer = Promise.withResolvers<IssueDecisions>();
    decisionsRefused.mockReturnValue(decisionsAnswer.promise);
    await act(async () => {
      void both.client.refetchQueries({ queryKey: ["decisions", KEY] });
    });
    await waitFor(() => expect(retry?.getAttribute("aria-busy")).toBe("true"));
    expect(retryOf(both, "decisions")).toBe(retry);
    expect(document.activeElement).toBe(retry);
    await act(async () => decisionsAnswer.resolve(decisionsRead([])));
    await waitFor(() => expect(failedLine(both, "decisions")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(retryOf(both, "asks")));
    asksRefused.mockResolvedValue([]);
    await act(async () => {
      void both.client.refetchQueries({ queryKey: ["asks", KEY] });
    });
    await waitFor(() => expect(failedLine(both, "asks")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement).toBe(both.getByRole("tree", { name: CONTROL_LABELS.tree }))
    );
    both.unmount();
  } finally {
    decisionsRefused.mockRestore();
    asksRefused.mockRestore();
  }
});

// R8: `t d` gives the view's tree the focus as the view opens, and on a running page that is
// before a refused read says so: the view asks the read again as it mounts, and the refusal
// comes back a second or so later. Where the list draws no row, the focus the tree holds then
// goes to that read's Retry once its line is drawn, for each of the two reads.
test("a read refused after the tree took the focus gives that focus to its Retry", async () => {
  for (const read of ["decisions", "asks"] as const) {
    const answer = Promise.withResolvers<never>();
    const spy =
      read === "decisions"
        ? spyOn(api, "getIssueDecisions").mockReturnValue(answer.promise)
        : spyOn(api, "listIssueAsks").mockReturnValue(answer.promise);
    try {
      const view =
        read === "decisions"
          ? renderBody({ children: [statement("s1")], made: null }, {}, { stage: true })
          : renderBody({ asks: null, children: [statement("s1")] }, {}, { stage: true });
      const tree = view.getByRole("tree", { name: CONTROL_LABELS.tree });
      await act(async () => tree.focus());
      expect(document.activeElement).toBe(tree);
      await act(async () => {
        answer.reject(
          new ApiError(503, { code: "UNAVAILABLE", error: "the store is not reachable" })
        );
        await answer.promise.catch(() => undefined);
      });
      // Where the focus is, by name: a Retry by the read its line is for.
      const focusIs = () => {
        const active = document.activeElement;
        const line = (["decisions", "asks"] as const).find(
          (each) => active !== null && active === retryOf(view, each)
        );
        if (line !== undefined) return `the ${line} Retry`;
        return active === tree ? "the tree" : (active?.tagName ?? "nothing");
      };
      await waitFor(() => expect(failedLine(view, read)).not.toBeNull());
      await waitFor(() => expect(focusIs()).toBe(`the ${read} Retry`));
      view.unmount();
    } finally {
      spy.mockRestore();
    }
  }
});

// A refused read's line says the read's failure again only for a new one. A run of the read that
// ends with none - cancelled, or paused while the browser is offline - leaves the read refused
// and its line drawn, and the line drew its alert afresh all the same, as though the run had been
// refused: a screen reader said the failure again when nothing new had failed. Through a run
// cancelled under it the alert stays the element it was and the Retry keeps the focus; a run
// refused after that draws a new alert. So for each of the two reads.
test("a refused read's run that ends with no new refusal draws no new alert, and one refused again does", async () => {
  const user = userEvent.setup();
  for (const read of ["decisions", "asks"] as const) {
    const spy =
      read === "decisions"
        ? spyOn(api, "getIssueDecisions").mockRejectedValue(new Error("refused"))
        : spyOn(api, "listIssueAsks").mockRejectedValue(new Error("refused"));
    try {
      const view =
        read === "decisions"
          ? renderBody({ children: [statement("s1")], made: null }, {}, { stage: true })
          : renderBody({ asks: null, children: [statement("s1")] }, {}, { stage: true });
      await waitFor(() => expect(retryOf(view, read)).not.toBeNull());
      const retry = retryOf(view, read);
      await act(async () => retry?.focus());
      expect(document.activeElement).toBe(retry);
      const alert = alertOf(view, read);
      expect(alert).not.toBeNull();

      // Enter on the Retry starts a run, which is held and then cancelled, under the key the
      // page reads the read by.
      spy.mockReturnValue(Promise.withResolvers<never>().promise);
      await user.keyboard("{Enter}");
      await waitFor(() => expect(retry?.getAttribute("aria-busy")).toBe("true"));
      await act(async () => view.client.cancelQueries({ queryKey: [read, KEY] }));
      await waitFor(() => expect(retry?.getAttribute("aria-busy")).toBeNull());
      expect(retryOf(view, read), "the cancelled run drew a new Retry").toBe(retry);
      expect(
        alertOf(view, read),
        "the cancelled run said the read's failure again, though nothing new failed"
      ).toBe(alert);
      expect(document.activeElement, "the cancelled run took the focus off the Retry").toBe(retry);

      await retryRefusedAgain(view, read, (run) => spy.mockReturnValue(run), user);
      const again = alertOf(view, read);
      expect(again, "the read refused again drew no alert").not.toBeNull();
      expect(again, "the read refused again kept the alert it drew before").not.toBe(alert);
      view.unmount();
    } finally {
      spy.mockRestore();
    }
  }
});

// R2: wherever a decision is drawn, one waiting for a person's review reads "Waiting for
// review", one a person kept "Kept by" with their name, and a replaced one "Replaced", each by
// a word and a shape (R6).
test("a decision reads how it stands in the three states the page names", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const state = (id: string) => {
    const row = rowOf(view, id);
    const word = row.querySelector("[data-node-margin] [data-node-state]");
    const box = word?.parentElement;
    return {
      // The box that holds the state's word and its shape carries its colour: a review row's
      // state is violet, and a foot row's grey.
      colour: (["decision", "quiet"] as const).find((tone) =>
        box?.classList.contains(pageStateText[tone])
      ),
      shape: box?.querySelector("svg")?.getAttribute("aria-label") ?? null,
      token: row.getAttribute("data-node-state"),
      word: words(word),
    };
  };
  expect(state(D("second"))).toEqual({
    colour: "decision",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.waiting_review],
    token: "waiting_review",
    word: NODE_STATE_WORDS.decision.waiting_review,
  });
  // The name is the person's own, drawn as an author's text and never as an id of the page's.
  expect(state(D("kept"))).toEqual({
    colour: "decision",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.kept],
    token: "kept",
    word: `${NODE_STATE_WORDS.decision.kept} alice`,
  });
  expect(
    rowOf(view, D("kept")).querySelector("[data-kept-by][data-author-text]")?.textContent
  ).toBe("alice");
  expect(state(D("replaced"))).toEqual({
    colour: "quiet",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.replaced],
    token: "replaced",
    word: NODE_STATE_WORDS.decision.replaced,
  });
  // A decision that waits and is tied to nothing reads the model's own state where it stands
  // (R35: "a second rule decides the mark on a row").
  expect(state(D("minor"))).toMatchObject({
    colour: "quiet",
    word: NODE_STATE_WORDS.decision.waiting_review,
  });
  // The one word the model holds for an agent's doubt, on the decision it marks.
  expect(words(rowOf(view, D("second")).querySelector("[data-node-unsure]"))).toBe(
    CONTROL_LABELS.unsure
  );
  expect(rowOf(view, D("first")).querySelector("[data-node-unsure]")).toBeNull();
});

// R35: the review list's decisions each draw Keep and Change with no focus on them, and no
// other row of the filter draws either: not the reader's own decisions, not a question, and
// not a decision no statement rests on, whether or not it waits.
test("Keep and Change are drawn on the review list's rows and on no other", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const controls = (id: string) =>
    [...rowOf(view, id).querySelectorAll("button[data-row-action]")].map((button) => [
      button.getAttribute("data-row-action"),
      button.textContent,
    ]);
  expect(document.activeElement?.closest("[data-node-row]") ?? null).toBeNull();
  for (const id of [D("second"), D("first")]) {
    expect(controls(id)).toEqual([
      ["keep", CONTROL_LABELS.keep_decision],
      ["change", CONTROL_LABELS.change_decision],
    ]);
  }
  for (const id of [D("kept"), D("minor"), D("replaced"), "q-open", "a-answered", "a-declined"]) {
    expect(controls(id)).toEqual([]);
  }
  // Nor does the focus draw them on a row that is no entry of the list.
  await act(async () => rowOf(view, D("minor")).focus());
  expect(controls(D("minor"))).toEqual([]);
  // Every control inside a row is out of the tab order (R9): of everything in the body that
  // can take the focus, only the row the tab stop rests on and the foot's own control are
  // reachable by Tab, and every other one carries tabindex -1.
  const reachable = [
    ...view.container.querySelectorAll<HTMLElement>(
      "[data-decisions-body] :is(button, a[href], input, textarea, select, [tabindex])"
    ),
  ].filter((element) => element.getAttribute("tabindex") !== "-1");
  expect(
    reachable
      .map((element) =>
        element.hasAttribute("data-node-row")
          ? "the row"
          : element.hasAttribute("data-decisions-heading")
            ? "the foot's control"
            : element.outerHTML
      )
      .sort()
  ).toEqual(["the foot's control", "the row"]);
});

// R2, R35: Keep is `DecisionActions.tsx`'s own write. The kept decision leaves the review list
// for the reader's own decisions, where it reads "Kept by" with their name, and the focus goes
// to the decision that stood after it.
test("a kept decision moves to the reader's own decisions and the focus to the next", async () => {
  const user = userEvent.setup();
  const kept = spyOn(api, "keepDecision").mockResolvedValue({
    ...KEPT,
    block_id: "second",
    kept_at: "2026-11-16T09:00:00Z",
  } as never);
  const view = renderBody(wholeIssue());
  try {
    const keep = rowOf(view, D("second")).querySelector('button[data-row-action="keep"]');
    if (keep === null) throw new Error("the review row draws no Keep");
    await user.click(keep);
    await waitFor(() => expect(kept).toHaveBeenCalled());
    // The Keep is of the words this row drew: the block and the sum of its text.
    expect(kept.mock.calls[0]?.slice(0, 3)).toEqual([
      DECISIONS_DOCUMENT.id,
      "second",
      "sha256:second",
    ]);

    await waitFor(() => expect(rowsOfPart(view, "review")).toEqual([D("first")]));
    expect(rowsOfPart(view, "yours")).toEqual([D("second"), D("kept"), "a-answered", "a-declined"]);
    expect(
      words(rowOf(view, D("second")).querySelector("[data-node-margin] [data-node-state]"))
    ).toBe(`${NODE_STATE_WORDS.decision.kept} alice`);
    expect(rowOf(view, D("second")).querySelectorAll("button[data-row-action]").length).toBe(0);
    await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  } finally {
    kept.mockRestore();
  }
});

// R6: the focus goes on to the next decision for the reader's own Keep or Change alone. A
// decision that leaves the review list by no act of the reader on this list - a Change sent
// from another page, a read that says another person kept it, even after a Keep of the
// reader's that Dispatch refused, or that it was replaced - keeps the focus where it now
// stands: under Your decisions, or in the foot, which unfolds for it.
test("a decision that leaves the review list by no act of the reader keeps the focus", async () => {
  // The address names the decision, and a Change on it arrives in the comments read.
  const changed = renderBody(wholeIssue(), { reveal: D("second") });
  await waitFor(() => expect(focusedRow(changed)).toBe(D("second")));
  await act(async () =>
    changed.client.setQueryData(
      ["comments", KEY],
      [changeOf("second", "2026-11-16T09:00:00Z", "Name the read, please.")]
    )
  );
  await waitFor(() => expect(rowsOfPart(changed, "review")).toEqual([D("first")]));
  expect(rowsOfPart(changed, "yours")[0]).toBe(D("second"));
  expect(rowOf(changed, D("second")).getAttribute("data-node-state")).toBe("change_requested");
  expect(focusedRow(changed)).toBe(D("second"));
  changed.unmount();

  // The reader is on the decision, and a decisions read says bob kept it.
  const whole = wholeIssue();
  const keptByBob = decisionsRead(
    whole.made.map((made) =>
      made.block_id === "second"
        ? {
            ...made,
            kept: { ...KEPT, block_id: "second", kept_by: "bob" },
            waiting_review: false,
          }
        : made
    )
  );
  const kept = renderBody(whole);
  await act(async () => rowOf(kept, D("second")).focus());
  await act(async () => kept.client.setQueryData(["decisions", KEY], keptByBob));
  await waitFor(() => expect(rowsOfPart(kept, "review")).toEqual([D("first")]));
  expect(rowsOfPart(kept, "yours")).toContain(D("second"));
  expect(
    words(rowOf(kept, D("second")).querySelector("[data-node-margin] [data-node-state]"))
  ).toBe(`${NODE_STATE_WORDS.decision.kept} bob`);
  expect(focusedRow(kept)).toBe(D("second"));
  kept.unmount();

  // The reader is on the decision, and a decisions read says it was replaced, which puts it in
  // the folded foot.
  const replaced = renderBody(whole);
  await act(async () => rowOf(replaced, D("second")).focus());
  await act(async () =>
    replaced.client.setQueryData(
      ["decisions", KEY],
      decisionsRead(
        whole.made.map((made) =>
          made.block_id === "second" ? { ...made, replaced: true, waiting_review: false } : made
        )
      )
    )
  );
  await waitFor(() => expect(rowsOfPart(replaced, "review")).toEqual([D("first")]));
  await waitFor(() => expect(rowsOfPart(replaced, "agents")).toContain(D("second")));
  expect(rowOf(replaced, D("second")).getAttribute("data-node-state")).toBe("replaced");
  await waitFor(() => expect(focusedRow(replaced)).toBe(D("second")));
  replaced.unmount();

  // The reader pressed Keep on the decision and Dispatch refused it, which the row says; then a
  // decisions read says bob kept it. A refused Keep is no review of the reader's, so the focus
  // stays on the decision where it now stands, not on the next one.
  const user = userEvent.setup();
  const refusal = spyOn(api, "keepDecision").mockRejectedValue(
    new ApiError(409, {
      code: "DECISION_CHANGED",
      error: "The decision changed since you read it.",
    })
  );
  const refused = renderBody(whole);
  try {
    const keep = rowOf(refused, D("second")).querySelector<HTMLElement>(
      'button[data-row-action="keep"]'
    );
    if (keep === null) throw new Error("the review row draws no Keep");
    await user.click(keep);
    await waitFor(() =>
      expect(
        rowOf(refused, D("second")).querySelector('[data-entry-actions] [role="alert"]')
      ).not.toBeNull()
    );
    expect(focusedRow(refused)).toBe(D("second"));
    await act(async () => refused.client.setQueryData(["decisions", KEY], keptByBob));
    await waitFor(() => expect(rowsOfPart(refused, "review")).toEqual([D("first")]));
    await waitFor(() => expect(focusedRow(refused)).toBe(D("second")));
  } finally {
    refusal.mockRestore();
  }
});

// R6, the class of the review list's case above (the tester's round 2): an entry that moves to
// another part of the list by no act of the reader keeps the focus where it now stands. Another
// person answers the open question the reader is on, which moves it under Your decisions; a
// decisions read ties the foot's decision the reader is on to a statement, which moves it to
// the review list. Each row is drawn again in its new part, and the focus is on it there, not
// on the page.
test("an entry that moves to another part by no act of the reader keeps the focus", async () => {
  const whole = wholeIssue();
  const answered = renderBody(whole);
  await act(async () => rowOf(answered, "q-open").focus());
  expect(focusedRow(answered)).toBe("q-open");
  answered.again({
    children: whole.children.map((child) =>
      child.id === "q-open"
        ? answeredAsk(
            "q-open",
            { at: "2026-11-16T09:00:00Z", by: { id: "bob", kind: "user" }, text: "At 1280 px." },
            {
              options: [
                { description: "Where the rail fits.", label: "1280 px" },
                { label: "1024 px" },
              ],
              text: "At which width does the header condense?",
            }
          )
        : child
    ),
  });
  await waitFor(() => expect(rowsOfPart(answered, "yours")).toContain("q-open"));
  expect(rowsOfPart(answered, "open_questions")).toEqual([]);
  await waitFor(() => expect(focusedRow(answered)).toBe("q-open"));
  answered.unmount();

  const tied = renderBody(whole);
  await act(async () => tied.getByRole("button", FOOT_CONTROL).click());
  await waitFor(() => expect(rowsOfPart(tied, "agents")).toContain(D("minor")));
  await act(async () => rowOf(tied, D("minor")).focus());
  expect(focusedRow(tied)).toBe(D("minor"));
  await act(async () =>
    tied.client.setQueryData(
      ["decisions", KEY],
      decisionsRead(
        whole.made.map((made) =>
          made.block_id === "minor" ? { ...made, links: [shaped(specRef("s3"))] } : made
        )
      )
    )
  );
  await waitFor(() => expect(rowsOfPart(tied, "review")).toContain(D("minor")));
  await waitFor(() => expect(focusedRow(tied)).toBe(D("minor")));
});

// The page's reads are refreshed by events, never by age (decision 180, R8): the stage mounts the
// view again on each press of Decisions, so under the app's `staleTime` of 30 s a press 31 s after
// the reads answered asks none of them again. Staleness is per observer, so that holds for every
// observer the view mounts: the seam's four reads, and the decisions read that Keep and Change
// draw from on each row of the review list (`DecisionActions.tsx`). The tester, at ed6312c5 on a
// running build: a press 35 s after the load asked the decisions read again on an issue whose
// review list holds rows, and asked nothing on an issue whose review list holds none.
test("the view opened after the app's staleTime asks no read again, its rows' Keep and Change included", async () => {
  const names = [
    "getIssueDecisions",
    "listIssueAsks",
    "listComments",
    "getArtifactBlocks",
  ] as const;
  // Each read held, so a read the mount started reaches no server.
  const reads = names.map((read) =>
    spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise)
  );
  try {
    const view = renderBody(wholeIssue(), {}, { answeredAgo: 31_000 });
    // Keep and Change are drawn on the review list's rows, so their own read is mounted too.
    expect(rowOf(view, D("first")).querySelectorAll("button[data-row-action]").length).toBe(2);
    await act(() => nextFrame());
    expect(
      Object.fromEntries(names.map((read, index) => [read, reads[index]?.mock.calls.length]))
    ).toEqual({ getArtifactBlocks: 0, getIssueDecisions: 0, listComments: 0, listIssueAsks: 0 });
    view.unmount();
  } finally {
    for (const read of reads) read.mockRestore();
  }
});

// The same rule for who is reading (decision 369 (c)): the page read it as it loaded, and a
// press of Decisions 31 s later asks it no second time, from any observer the view mounts. A row
// that draws a thread's read mark (`outline-thread-read.tsx`) observes it through
// `useMarkThreadRead`'s `useIsHuman`. The tester, at a381aa82 on a running build: Decisions
// pressed 35 s after the load asked `/auth/whoami` once, and so did the conversation pressed 35 s
// after that, with no other read.
test("the view opened after the app's staleTime asks who is reading no second time", async () => {
  const who = spyOn(api, "whoAmI").mockReturnValue(Promise.withResolvers<never>().promise);
  try {
    const view = renderBody(wholeIssue(), {}, { answeredAgo: 31_000 });
    // A row of the view draws a thread's read mark, so its observer is mounted.
    expect(view.container.querySelectorAll("[data-thread-read]").length).toBeGreaterThan(0);
    await act(() => nextFrame());
    expect(who).not.toHaveBeenCalled();
    view.unmount();
  } finally {
    who.mockRestore();
  }
});

// A closed issue takes no write, so the review list keeps its rows and draws no control.
test("a closed issue draws the review list with no Keep and no Change", () => {
  const view = renderBody(wholeIssue(), { isClosed: true });
  expect(rowsOfPart(view, "review")).toEqual([D("second"), D("first")]);
  expect(view.container.querySelectorAll("button[data-row-action]").length).toBe(0);
});

// Decision 110 (2), R6: an entry the reader is on that another person deletes takes its row,
// and the focus with it, off the page. The focus goes to the entry that stood after it in its
// part, else to the one before it where it was the last, else, where its part is left empty,
// to the view's first row, else, where no row is left, to the tree: never to the page.
test("a focused entry that another person deletes gives the focus to its neighbour", async () => {
  /** Reads the issue again with `block`'s decision deleted: gone from the tree and from the
   *  decisions read. */
  const deleted = async (
    view: Drawn,
    issue: { children: TreeNode[]; made: Decision[] },
    block: string
  ) => {
    await act(async () => {
      view.client.setQueryData(
        ["decisions", KEY],
        decisionsRead(issue.made.filter((made) => made.block_id !== block))
      );
      view.again({ children: issue.children.filter((child) => child.id !== D(block)) });
    });
  };
  const whole = wholeIssue();

  // The newer of the two decisions that wait for review: the focus goes to the older, after it.
  const next = renderBody(whole);
  await act(async () => rowOf(next, D("second")).focus());
  await deleted(next, whole, "second");
  expect(rowsOfPart(next, "review")).toEqual([D("first")]);
  await waitFor(() => expect(focusedRow(next)).toBe(D("first")));
  next.unmount();

  // The older, the last of them: the focus goes to the newer, before it.
  const last = renderBody(whole);
  await act(async () => rowOf(last, D("first")).focus());
  await deleted(last, whole, "first");
  expect(rowsOfPart(last, "review")).toEqual([D("second")]);
  await waitFor(() => expect(focusedRow(last)).toBe(D("second")));
  last.unmount();

  // The reader's one decision, the only entry of its part: the part goes, and the focus goes to
  // the view's first row, the open question, not to the decision drawn just before it.
  const own = {
    children: [statement("s1"), openAsk("q-open"), decisionNode("first"), decisionNode("kept")],
    made: [
      decision("first", { links: [shaped(specRef("s1"))] }),
      decision("kept", { kept: KEPT, links: [shaped(specRef("s1"))], waiting_review: false }),
    ],
  };
  const only = renderBody(own);
  expect(rowsOfPart(only, "yours")).toEqual([D("kept")]);
  await act(async () => rowOf(only, D("kept")).focus());
  await deleted(only, own, "kept");
  expect(only.container.querySelector('[data-decisions-part="yours"]')).toBeNull();
  await waitFor(() => expect(focusedRow(only)).toBe("q-open"));
  only.unmount();

  // The view's one entry: no row is left to take the focus, and the tree keeps it, which leaves
  // it in the list.
  const lone = {
    children: [statement("s1"), decisionNode("first")],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  };
  const alone = renderBody(lone);
  await act(async () => rowOf(alone, D("first")).focus());
  await deleted(alone, lone, "first");
  expect(alone.container.querySelector("[data-node-row]")).toBeNull();
  await waitFor(() => expect(document.activeElement).toBe(alone.getByRole("tree")));
  // The focus is off every row, so what the deleted row's focus opened is let go: the same
  // decision read back draws as a row the reader is not on.
  await act(async () => {
    alone.client.setQueryData(["decisions", KEY], decisionsRead(lone.made));
    alone.again({ children: lone.children });
  });
  await waitFor(() => expect(rowOf(alone, D("first"))).toBeTruthy());
  expect(rowOf(alone, D("first")).querySelector("[data-node-body]")).toBeNull();
});

// R2, R9, c-decisions: an answered ask draws its question and its options, and the person's
// answer word for word as the store holds it, each paragraph the paragraph stored, with no
// sentence of the page's beside it restating it and no reading: the agent's write-up of the
// answer is no row of the filter.
test("an answered ask draws the person's answer as stored, and no reading", async () => {
  const view = renderBody(wholeIssue());
  const row = rowOf(view, "a-answered");

  expect(words(row.querySelector("[data-node-words]"))).toBe("Does the built page work for you?");
  expect(
    [...row.querySelectorAll("[data-node-option]")].map((option) => [
      option.getAttribute("data-node-option"),
      option.textContent,
    ])
  ).toEqual([
    ["", "○Works"],
    ["chosen", "●Changes needed"],
  ]);
  // Each paragraph of what they wrote, its doubled spaces included: the answer is drawn by the
  // row's reader (`RowText`), as a row's words are, so a paragraph is an element of the page and
  // the blank line and the indent between two are no characters of it.
  const stored = ["Keep the rail.", "It is 56 px wide -  no wider."];
  expect(
    [...row.querySelectorAll("[data-answer-text] p")].map((paragraph) => paragraph.textContent)
  ).toEqual(stored);
  expect(words(row.querySelector("[data-answer-author]"))).toBe("alice");
  // The reader's own answer carries the ink rule their own words carry (R34): each class of the
  // page's rule, so the reading holds whatever classes the theme writes the rule with.
  const answer = row.querySelector("[data-node-answer]");
  const missing = pageOwnWordsRule.split(" ").filter((name) => !answer?.classList.contains(name));
  expect(missing).toEqual([]);

  // With the focus on it the same holds, and what the page writes of its own on the row is the
  // kind, the state, the word naming the answer, the link and the body's one control.
  await act(async () => row.focus());
  expect(
    [...row.querySelectorAll("[data-answer-text] p")].map((paragraph) => paragraph.textContent)
  ).toEqual(stored);
  // (the two marks are the options' own: the one chosen and the one not).
  expect(chromeOf(row).sort()).toEqual(
    [
      NODE_TYPE_NAMES.ask,
      "·",
      NODE_STATE_WORDS.ask.answered,
      "○",
      "●",
      CONTROL_LABELS.answer,
      IN_CONVERSATION,
      CONTROL_LABELS.keep_open,
    ].sort()
  );

  // The agent's write-up of the answer is drawn nowhere in the filter.
  expect(view.container.querySelector(`[data-node-id="${D("reading")}"]`)).toBeNull();
  expect(view.queryByText("The reader wants the rail kept.")).toBeNull();
});

// R2: a declined ask reads as a question the person declined, never as an answer: its state
// word, who declined it by a name and a shape, and the note they wrote, word for word.
test("a declined ask says who declined it and draws no answer", () => {
  const view = renderBody(wholeIssue());
  const row = rowOf(view, "a-declined");

  expect(row.getAttribute("data-node-state")).toBe("declined");
  expect(words(row.querySelector("[data-node-margin] [data-node-state]"))).toBe(
    NODE_STATE_WORDS.ask.declined
  );
  expect(words(row.querySelector("[data-ask-declined] p"))).toBe(
    `alice ${DECISIONS_WORDS.declined}`
  );
  expect(row.querySelector("[data-declined-by] svg")?.getAttribute("data-glyph")).toBe(
    GLYPH_TOKENS.authors.person
  );
  expect(row.querySelector("[data-declined-note]")?.textContent).toBe(
    "The release owner decides this one."
  );
  expect(row.querySelector("[data-node-answer]")).toBeNull();
  expect(row.querySelector("[data-answer-text]")).toBeNull();
});

// R6, R9: the note a person wrote as they declined is drawn by the renderer, as a person's answer
// under an ask is (`RowText`), never as the characters they typed: its marks are drawn as what
// they are, and a reference in it reads as its target's name from the page's one names read.
test("a declined ask's note is drawn through the renderer, a reference in it read as its target", async () => {
  await warmMarkdownRenderer();
  const cited = "dispatch://CORE-2/spec#b-r1";
  const name = "the queue's drain";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const view = renderBody(
    {
      ...wholeIssue(),
      asks: [
        closing("a-declined", {
          at: "2026-11-13T09:00:00Z",
          kind: "declined",
          reason: `Ask about ${cited} **first**.`,
        }),
      ],
    },
    { published: publishedNames(new Map([[cited, name]])) }
  );
  const note = () => rowOf(view, "a-declined").querySelector("[data-declined-note]");
  try {
    await waitFor(() => expect(note()?.textContent).toBe(`Ask about ${name} first.`));
    expect(note()?.querySelector("strong")?.textContent).toBe("first");
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

// R6: where the asks read sends the server's reading of the note (`reason_tree`), the row draws
// that tree rather than a parse of the stored text. The tree reads differently from the text
// beside it, so a note drawn from a parse would read "the old words".
test("a declined ask's note is drawn from the reading the asks read sends", async () => {
  await warmMarkdownRenderer();
  const view = renderBody({
    ...wholeIssue(),
    asks: [
      closing("a-declined", {
        at: "2026-11-13T09:00:00Z",
        kind: "declined",
        reason: "the **old** words",
        reason_tree: displayTree("the **new** words, for now"),
      }),
    ],
  });
  const note = () => rowOf(view, "a-declined").querySelector("[data-declined-note]");
  try {
    await waitFor(() => expect(note()?.textContent).toBe("the new words, for now"));
    expect(note()?.querySelector("strong")?.textContent).toBe("new");
  } finally {
    view.unmount();
  }
});

// c-decisions: "The decision that shaped three requirements is one entry with one node id, and
// it reaches all three. Each entry links to the node that holds it."
test("a decision reaches each statement it shaped, and every entry links to its node", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  expect(view.container.querySelectorAll(`[data-node-id="${D("second")}"]`).length).toBe(1);
  const informs = rowOf(view, D("second")).querySelector("[data-decision-informs]");
  expect(words(informs?.querySelector("[data-page-chrome]"))).toBe(DECISIONS_WORDS.informs);
  expect(
    [...(informs?.querySelectorAll("a") ?? [])].map((link) => [
      link.textContent,
      link.getAttribute("href"),
    ])
  ).toEqual([
    // Each statement named as a chip names a block (`chipName`): the first line of its own
    // words, "The page draws sN. It draws nothing else.", cut by R1's first-line rule and then
    // by the chips' rule within 32 characters.
    ["The page draws s1.", `/issues/${KEY}#b-s1`],
    ["The page draws s2.", `/issues/${KEY}#b-s2`],
    ["The page draws s3.", `/issues/${KEY}#b-s3`],
  ]);
  // A decision tied to nothing names nothing.
  expect(rowOf(view, D("minor")).querySelector("[data-decision-informs]")).toBeNull();

  const link = (id: string) => {
    const found = rowOf(view, id).querySelector("a[data-entry-link]");
    return [words(found), found?.getAttribute("href") ?? null];
  };
  expect(link("q-open")).toEqual([IN_CONVERSATION, `/issues/${KEY}/asks/q-open`]);
  expect(link("a-answered")).toEqual([IN_CONVERSATION, `/issues/${KEY}/asks/a-answered`]);
  expect(link(D("first"))).toEqual([IN_CONVERSATION, `/issues/${KEY}#b-first`]);
  expect(link(D("minor"))).toEqual([IN_CONVERSATION, `/issues/${KEY}#b-minor`]);
  // Every row the body draws carries one.
  expect(
    [...view.container.querySelectorAll("[data-node-row]")].filter(
      (row) => row.querySelectorAll("a[data-entry-link]").length !== 1
    )
  ).toEqual([]);
  view.unmount();

  // A parent with no words of its own is named by its kind's word, once, and never by its id
  // (R7): here an open question that holds no words, which the decision was made under.
  const wordless = renderBody({
    children: [
      statement("s1"),
      openAsk("q2", true, { children: [decisionNode("wordless")], text: "" }),
    ],
    made: [decision("wordless", { links: [shaped(specRef("s1"))] })],
  });
  const parent = rowOf(wordless, D("wordless")).querySelector("[data-node-parent-line]");
  expect(parent?.getAttribute("data-parent-id")).toBe("q2");
  expect(words(parent)).toBe(NODE_TYPE_NAMES.ask);
});

// R2, R8: an entry's parent line is its parent's projected first line (`useNodeLine`), as the
// outline's parent line is, so a reference in the parent's words reads as its target's name from
// the page's one names read, and the line is cut at the first sentence; never the parent's words
// as written.
test("an entry's parent line reads a reference in its parent's words by its target's name", async () => {
  await warmMarkdownRenderer();
  const address = "dispatch://CORE-2/spec#b-r1";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const view = renderBody(
    {
      children: [
        statement("s1"),
        openAsk("q2", true, {
          children: [decisionNode("under")],
          text: `Serve ${address} first. Then the rest.`,
        }),
      ],
      made: [decision("under", { links: [shaped(specRef("s1"))] })],
    },
    { published: publishedNames(new Map([[address, "The rows hold"]])) }
  );
  const line = () =>
    rowOf(view, D("under")).querySelector("[data-node-parent-line]")?.lastElementChild?.textContent;
  try {
    await waitFor(() => expect(line()).toBe("Serve The rows hold first."));
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

// R5 (c), ruling 5: a decision's row draws no author in its margin, so its focused body says
// the model holds none, in the page's own words; an agent's margin names it by its role's word
// alone, so its focused body gives the session's whole title.
test("a focused row says Author unknown for a decision with no author, and gives an agent's whole title", async () => {
  const planner: Actor = {
    id: "session-planner",
    kind: "session",
    origin: { session_title: "Legion planner · CORE-1" },
  };
  const view = renderBody({
    children: [
      statement("s1"),
      openAsk("q-planner", true, { author: planner }),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  });

  const decided = rowOf(view, D("first"));
  await act(async () => decided.focus());
  expect(words(decided.querySelector("[data-node-body] [data-node-author]"))).toBe(
    CONTROL_LABELS.unknown_author
  );
  expect(decided.querySelector("[data-node-margin] [data-node-author]")).toBeNull();

  const asked = rowOf(view, "q-planner");
  await act(async () => asked.focus());
  expect(words(asked.querySelector("[data-node-margin] [data-node-author]"))).toBe("planner");
  expect(words(asked.querySelector("[data-node-body] [data-node-author-title]"))).toBe(
    "Legion planner · CORE-1"
  );
  // The whole title is the body's alone: the margin names the agent by its role's word.
  expect(asked.querySelector("[data-node-margin] [data-node-author-title]")).toBeNull();
});

// R5 (e), R22: a statement on a decision's "informs" line is named by its own words, cut at a
// word within 32 characters as a chip's are (`chipName`, decision 83), and never by an address.
test("each statement on the informs line is named within 32 characters", () => {
  const view = renderBody({
    children: [
      statement("s1", { text: "Requirement 1 of the redesign stands on a row of its own." }),
      statement("s2"),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1")), shaped(specRef("s2"))] })],
  });

  const informs = rowOf(view, D("first")).querySelector("[data-decision-informs]");
  const names = [...(informs?.querySelectorAll("a") ?? [])].map((link) => link.textContent ?? "");
  expect(names.length).toBe(2);
  const name = names[0] ?? "";
  expect(name.length).toBeLessThanOrEqual(32);
  expect(name.endsWith("…")).toBe(true);
  expect(name.startsWith("Requirement 1 of the redesign")).toBe(true);
  expect(name).not.toContain("dispatch://");
  expect(name).not.toContain("#b-");
  // The other statement is named by its own words too, "The page draws s2. It draws nothing
  // else.", cut the same way: its first line, which is its first sentence.
  expect(names[1]).toBe("The page draws s2.");
  expect(
    [...(informs?.querySelectorAll("[data-decision-statement]") ?? [])].map((shape) =>
      shape.getAttribute("data-decision-statement")
    )
  ).toEqual([statement("s1").id, statement("s2").id]);
  view.unmount();

  // A statement is named by its first block's words, as the outline's chip for it is: a short
  // first paragraph is the whole of its name, and the paragraph after it no part of it.
  const short = renderBody({
    children: [
      statement("s1", { text: "Rows stand alone.\n\nEach row draws one node of the tree." }),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  });
  expect(words(rowOf(short, D("first")).querySelector("[data-decision-statement] a"))).toBe(
    "Rows stand alone."
  );
});

// R8 over an entry's words: the body hands its rows what the page publishes of its batched names
// read in the render that draws them (`published`), as the outline does, and the page publishes
// to the names store only after the rows have rendered (`Publisher`): so a reference the page
// asks for waits on the page's answer from a row's first render, drawn as its author wrote it
// meanwhile, and is never read by the row; a row that read the store alone read it itself first,
// one request for each row.
test("a decision's row waits on the page's read for a reference the page asks for", async () => {
  const address = "dispatch://CORE-2/spec#b-r1";
  const written = `See ${address} first.`;
  const name = "the queue's drain";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const view = renderBody(
    {
      children: [statement("s1"), decisionNode("first", { text: written })],
      made: [decision("first", { links: [shaped(specRef("s1"))] })],
    },
    { published: publishedNames(new Map(), { asked: [address] }) }
  );
  const rowWords = () =>
    rowOf(view, D("first")).querySelector("[data-node-words]")?.textContent ?? "";
  try {
    await waitFor(() => expect(rowWords()).toContain(written));
    expect(getReferences, "a row read the reference the page asks for").not.toHaveBeenCalled();
    view.again({ published: publishedNames(new Map([[address, name]])) });
    await waitFor(() => expect(rowWords()).toContain(`See ${name} first.`));
    expect(getReferences, "a row read the reference the page named").not.toHaveBeenCalled();
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

// R9: the list is one tab stop and the outline's own keys work it: `j` and `k` move through
// the parts in the order they are drawn, and so does `q` through the open questions, Shift+K
// presses Keep, Enter goes to the entry's node in the conversation, and the key that cycles how
// much is drawn unfolds the foot.
test("the outline's keys move through the parts and work a row's controls", async () => {
  const user = userEvent.setup();
  const kept = spyOn(api, "keepDecision").mockReturnValue(Promise.withResolvers<never>().promise);
  const view = renderBody(wholeIssue());
  try {
    const stops = () => [...view.container.querySelectorAll('[tabindex="0"]')];
    expect(stops().map((stop) => stop.getAttribute("data-node-id"))).toEqual(["q-open"]);

    await act(async () => rowOf(view, "q-open").focus());
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("second"));
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("first"));
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("kept"));
    await user.keyboard("k");
    expect(focusedRow(view)).toBe(D("first"));
    // The tab stop rests where the reader stands.
    expect(stops().map((stop) => stop.getAttribute("data-node-id"))).toEqual([D("first")]);

    await user.keyboard("{Shift>}K{/Shift}");
    await waitFor(() => expect(kept).toHaveBeenCalled());
    expect(kept.mock.calls[0]?.[1]).toBe("first");

    // The key that cycles how much is drawn unfolds the foot, and folds it again.
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
    await user.keyboard("{End}");
    expect(focusedRow(view)).toBe(D("replaced"));
    await user.keyboard("{Home}");
    expect(focusedRow(view)).toBe("q-open");
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([]);

    // Enter on a row goes to its node in the conversation.
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.getByTestId("where").textContent).toBe(`/issues/${KEY}/asks/q-open`)
    );

    // A fold with the focus on one of the foot's rows puts it on the foot's control, which
    // stays, rather than dropping it to the page with the row.
    await user.keyboard("z");
    await act(async () => rowOf(view, D("minor")).focus());
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([]);
    expect(document.activeElement).toBe(footControlOf(view));

    // Let close on a row pinned open, pressed while another row has the focus, leaves the focus
    // where it is: the press moves none, so the body it closes takes none to the page.
    await act(async () => rowOf(view, D("kept")).focus());
    await user.keyboard("o");
    await act(async () => rowOf(view, "q-open").focus());
    expect(rowOf(view, D("kept")).querySelector("[data-node-body][data-pinned]")).not.toBeNull();
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.let_close }));
    expect(document.activeElement).toBe(rowOf(view, "q-open"));
    expect(rowOf(view, "q-open").querySelector("[data-node-body]")).not.toBeNull();
    expect(rowOf(view, D("kept")).querySelector("[data-node-body]")).toBeNull();
    view.unmount();

    // `q` walks the open questions in the order the list draws them, the one that waits on the
    // reader first, whatever order the tree holds them in, and on the last one drawn it says
    // there is no further one.
    const waitingFirst = renderBody({
      children: [statement("s1"), openAsk("q-other", false), openAsk("q-mine", true)],
    });
    expect(rowsOfPart(waitingFirst, "open_questions")).toEqual(["q-mine", "q-other"]);
    await act(async () => rowOf(waitingFirst, "q-mine").focus());
    await user.keyboard("q");
    expect(focusedRow(waitingFirst)).toBe("q-other");
    expect(waitingFirst.container.querySelector("[data-row-report]")).toBeNull();
    await user.keyboard("q");
    expect(focusedRow(waitingFirst)).toBe("q-other");
    expect(words(rowOf(waitingFirst, "q-other").querySelector("[data-row-report]"))).toBe(
      KEY_REPORTS.no_next_question
    );
  } finally {
    kept.mockRestore();
  }
});

// R14, R19: the Contents pane's list, which asks the outline to go to a node, and an address
// that names an entry, each put the focus on that entry's row, and unfold the foot where the
// entry is one of its own.
test("the page's ask and the address put the focus on an entry, unfolding the foot for it", async () => {
  const view = renderBody(wholeIssue());
  expect(focusedRow(view)).toBeUndefined();

  view.again({ request: { focusBranch: false, id: D("first"), seq: 1 } });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  // The same ask is acted on once: the reader's own move is not undone by it.
  await act(async () => rowOf(view, "q-open").focus());
  view.again({ request: { focusBranch: false, id: D("first"), seq: 1 } });
  expect(focusedRow(view)).toBe("q-open");

  expect(rowsOfPart(view, "agents")).toEqual([]);
  view.again({ reveal: D("replaced") });
  await waitFor(() => expect(focusedRow(view)).toBe(D("replaced")));
  expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
});

// R1, R6: an address that names a node that is neither a question nor a decision, a comment
// here, outranks the view: the comment is drawn first, under no heading and in no part, as the
// outline draws a row for its context, and the focus goes to it. It is no entry of the list,
// so an issue that holds no question and no decision still says so.
test("an address that names a comment draws its row first and focuses it", async () => {
  const said = node({
    at: "2026-11-12T09:00:00Z",
    author: READER_ACTOR,
    id: "c-said",
    reference: `dispatch://${KEY}/comment/c-said`,
    text: "The rail stays where it is.",
    type: "comment",
  });
  const children = [statement("s1"), said];
  const view = renderBody(
    { children },
    {
      reveal: said.id,
      view: presetView(issueTree(children), "decisions", "", new Set([said.id])).root,
    }
  );

  const first = view
    .getByRole("tree", { name: CONTROL_LABELS.tree })
    .querySelector<HTMLElement>("[data-node-row]");
  expect(first?.getAttribute("data-node-id")).toBe(said.id);
  expect(first?.closest("[data-decisions-part]") ?? null).toBeNull();
  expect(first?.getAttribute("aria-level")).toBe("1");
  await waitFor(() => expect(focusedRow(view)).toBe(said.id));

  const row = rowOf(view, said.id);
  // A row drawn for its context, as the outline marks one: no tag, since a comment is no typed
  // kind, its author out of the reader's ink, its words, and no state.
  expect(row.getAttribute("data-node-context")).toBe("");
  expect(row.querySelector("[data-node-tag]")).toBeNull();
  const author = row.querySelector("[data-node-margin] [data-node-author] [data-author-text]");
  expect(words(author)).toBe("alice");
  // Out of the reader's ink: no class of the page's ink is on it.
  expect(pageInkText.split(" ").filter((name) => author?.classList.contains(name))).toEqual([]);
  expect(words(row.querySelector("[data-node-words]"))).toBe("The rail stays where it is.");
  expect(row.hasAttribute("data-node-state")).toBe(false);
  expect(row.querySelector("[data-node-margin] [data-node-state]")).toBeNull();
  const link = row.querySelector("a[data-entry-link]");
  expect(words(link)).toBe(IN_CONVERSATION);
  expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/comments/${said.id}`);
  expect(row.querySelectorAll("button[data-row-action]").length).toBe(0);
  // No heading and no count stand over it, and the issue says it holds no decision.
  expect(view.container.querySelectorAll("[data-decisions-heading]").length).toBe(0);
  expect(view.getByText(EMPTY_LINE)).toBeTruthy();
});

/** A session nobody named, which the page calls by its word for an agent. */
const UNTITLED: Actor = { id: "0f8e2c4a-1111-4222-8333-444455556666", kind: "session" };

/** When the posts below were deleted, and when edited. */
const DELETED_AT = "2026-11-12T10:00:00Z";
const EDITED_AT = "2026-11-12T11:00:00Z";

/** How the deleted line opens: the page's words, then the session nobody named as the page
 *  names an author, never as the model's line holds it. */
const DELETED_OPENING = `${CONTROL_LABELS.post_deleted_by} ${CONTROL_LABELS.unnamed_author} · `;

/** A comment the session nobody named wrote and deleted, as the model holds it
 *  (`tree-drafts.ts`'s `addTurns`): its delete, and the deleted line as its text, the actor
 *  named as no registry knows it and the time as stored, with no reading. */
function deletedComment(id: string, children: readonly TreeNode[] = []): TreeNode {
  return node({
    at: "2026-11-12T09:00:00Z",
    author: UNTITLED,
    children,
    deletedAt: DELETED_AT,
    deletedBy: UNTITLED,
    id,
    reference: `dispatch://${KEY}/comment/${id}`,
    text: `${CONTROL_LABELS.post_deleted_by} ${actorLabelText(UNTITLED)} · ${DELETED_AT}`,
    type: "comment",
  });
}

// R2 and R3 of dispatch://INFRA-1523: a comment an address named is drawn with the marks the
// outline's row draws it with once its author corrected it. One its author deleted draws the
// deleted line in place of its words, in the page's words, its actor named as the page names an
// author and never as the model's text holds it, and its time as the page draws a time.
test("a deleted comment an address named draws its deleted line, never the model's line", () => {
  const gone = deletedComment("c-gone");
  const children = [statement("s1"), gone];
  const view = renderBody(
    { children },
    {
      reveal: gone.id,
      view: presetView(issueTree(children), "decisions", "", new Set([gone.id])).root,
    }
  );
  const row = rowOf(view, gone.id);
  const deletedLine = row.querySelector("[data-node-words] [data-post-deleted]");
  expect(deletedLine?.textContent?.slice(0, DELETED_OPENING.length)).toBe(DELETED_OPENING);
  expect(deletedLine?.hasAttribute(PAGE_CHROME_ATTRIBUTE)).toBe(true);
  expect(row.textContent).not.toContain(actorLabelText(UNTITLED));
  expect(row.textContent).not.toContain(DELETED_AT);
});

// R2 of dispatch://INFRA-1523: a message an address named that its author edited draws its
// words as they read now, and says ` · edited` after its time, outside its words, in the page's
// own words, as the outline's row says it in its margin.
test("an edited message an address named draws its words and says · edited after them", () => {
  const edited = node({
    at: "2026-11-12T09:00:00Z",
    author: READER_ACTOR,
    editedAt: EDITED_AT,
    id: "m-edited",
    reference: `dispatch://${KEY}/message/m-edited`,
    text: "The rail stays where it is, at 56 px.",
    type: "message",
  });
  const children = [statement("s1"), edited];
  const view = renderBody(
    { children },
    {
      reveal: edited.id,
      view: presetView(issueTree(children), "decisions", "", new Set([edited.id])).root,
    }
  );
  const row = rowOf(view, edited.id);
  expect(words(row.querySelector("[data-node-words]"))).toBe(edited.text);
  const mark = row.querySelector("[data-node-edited]");
  expect([mark?.textContent, mark?.hasAttribute(PAGE_CHROME_ATTRIBUTE)]).toEqual([
    ` · ${CONTROL_LABELS.post_edited}`,
    true,
  ]);
  expect(mark?.closest("[data-node-words]")).toBeNull();
});

// R2 and R3 of dispatch://INFRA-1523: an entry's parent line is the outline's own
// (`NodeRow.tsx`'s `ParentLink`), so it carries a corrected post's marks. A comment its author
// deleted is drawn as its deleted line, its actor named as the page names an author and never as
// the model's line holds it, and it stays the link to the comment, out of the tab order as
// everything inside a row is.
test("an entry whose parent is a deleted comment draws the parent line as the deleted line", () => {
  const view = renderBody({
    children: [statement("s1"), deletedComment("c-gone", [decisionNode("under")])],
    made: [decision("under", { links: [shaped(specRef("s1"))] })],
  });
  const line = rowOf(view, D("under")).querySelector("[data-node-parent-line]");
  expect(line?.getAttribute("data-parent-id")).toBe("c-gone");
  const deletedLine = line?.querySelector("a [data-post-deleted]");
  expect(deletedLine?.textContent?.slice(0, DELETED_OPENING.length)).toBe(DELETED_OPENING);
  expect(line?.textContent).not.toContain(actorLabelText(UNTITLED));
  expect(line?.textContent).not.toContain(DELETED_AT);
  expect(
    [...(line?.querySelectorAll<HTMLAnchorElement>("a") ?? [])].map((link) => [
      link.getAttribute("href"),
      link.tabIndex,
    ])
  ).toEqual([[`/issues/${KEY}/comments/c-gone`, -1]]);
});

// R2 of dispatch://INFRA-1523: an entry whose parent is a comment its author edited says
// ` · edited` after the parent's line, in the page's own words, inside the link to the comment;
// a parent with no words of its own is named by its kind's word once, then the same mark.
test("an entry whose parent is an edited comment draws · edited after the parent's line", () => {
  const edited = node({
    children: [decisionNode("under")],
    editedAt: EDITED_AT,
    id: "c-edited",
    reference: `dispatch://${KEY}/comment/c-edited`,
    text: "Where does the rail sit?",
    type: "comment",
  });
  const wordless = node({
    children: [decisionNode("bare")],
    editedAt: EDITED_AT,
    id: "c-wordless",
    reference: `dispatch://${KEY}/comment/c-wordless`,
    text: "",
    type: "comment",
  });
  const view = renderBody({
    children: [statement("s1"), edited, wordless],
    made: [
      decision("under", { links: [shaped(specRef("s1"))] }),
      decision("bare", { links: [shaped(specRef("s1"))] }),
    ],
  });
  const line = rowOf(view, D("under")).querySelector("[data-node-parent-line]");
  const link = line?.querySelector("a");
  expect(link?.textContent).toBe(`${edited.text} · ${CONTROL_LABELS.post_edited}`);
  expect(link?.querySelector(`[${PAGE_CHROME_ATTRIBUTE}]`)?.textContent).toBe(
    ` · ${CONTROL_LABELS.post_edited}`
  );
  expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/comments/c-edited`);
  expect(words(rowOf(view, D("bare")).querySelector("[data-node-parent-line]"))).toBe(
    `${NODE_TYPE_NAMES.comment} · ${CONTROL_LABELS.post_edited}`
  );
});

// R6: the page holds which of the Contents list's requests an outline or a view took
// (`outline-request.tsx`), so a view drawn again, turned off and on, does not take the reader
// back to the request it took before.
test("a body drawn again does not act on a request the page already took", async () => {
  const request: OutlineRequest = { focusBranch: false, id: D("first"), seq: 1 };
  const view = renderBody(wholeIssue(), { request }, { requests: true });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));

  const elsewhere = view.getByRole("button", { name: "Elsewhere" });
  await act(async () => elsewhere.focus());
  view.again({ drawn: false, request });
  expect(view.container.querySelector("[data-decisions-body]")).toBeNull();

  view.again({ request });
  expect(rowOf(view, D("first"))).toBeTruthy();
  await act(async () => {});
  expect(document.activeElement).toBe(elsewhere);
});

// R4, R6: a list that holds only the foot draws no row, so it has no tab stop to take the
// focus. A page opened on it with nothing focused gives the focus to the foot's control, and
// so does a focus on the tree, which is where `t d` puts it when the tree holds no tab stop
// (`useIssueKeymap.ts`): the tree element keeps no focus while the list draws a row or the
// foot. The foot leaving the page with the focus on its control leaves that focus in the list.
test("a list that holds only the foot gives the focus to the foot's control", async () => {
  const user = userEvent.setup();
  expect(document.activeElement).toBe(document.body);
  const view = renderBody(FOOT_ONLY);
  await waitFor(() => expect(document.activeElement).toBe(footControlOf(view)));

  (document.activeElement as HTMLElement).blur();
  expect(footControlOf(view)).not.toHaveFocus();
  await act(async () => view.getByRole("tree", { name: CONTROL_LABELS.tree }).focus());
  expect(document.activeElement).toBe(footControlOf(view));
  view.unmount();

  // The tree is built read by read (`useIssueTree.ts`): a list that holds only the foot until
  // the asks read answers gives the focus to the foot's control, and once the question's row
  // is drawn the focus goes to it, where the stage would have put it.
  const early = renderBody(FOOT_ONLY);
  await waitFor(() => expect(document.activeElement).toBe(footControlOf(early)));
  early.again({ children: [openAsk("q-open"), decisionNode("minor")] });
  await waitFor(() => expect(focusedRow(early)).toBe("q-open"));
  early.unmount();

  // A read ties the foot's one decision to a statement, which takes it up to the review list,
  // and the foot leaves the page with the focus on its control. The focus goes to the
  // decision's row there, never to the page's body, whether the reader put it on the control,
  // by Tab, or the body did, on a page opened here.
  const tieable = { children: [statement("s1"), decisionNode("minor")], made: [decision("minor")] };
  const tied = decisionsRead([decision("minor", { links: [shaped(specRef("s1"))] })]);
  for (const placed of ["the reader", "the body"] as const) {
    const left = renderBody(tieable);
    await waitFor(() => expect(document.activeElement).toBe(footControlOf(left)));
    if (placed === "the reader") {
      await act(async () => (document.activeElement as HTMLElement).blur());
      await user.tab();
      expect(document.activeElement).toBe(footControlOf(left));
    }
    await act(async () => left.client.setQueryData(["decisions", KEY], tied));
    await waitFor(() => expect(rowsOfPart(left, "review")).toEqual([D("minor")]));
    expect(footControlOf(left)).toBeNull();
    await waitFor(() =>
      expect({ placed, row: focusedRow(left) }).toEqual({ placed, row: D("minor") })
    );
    left.unmount();
  }

  // A reader who presses the control keeps the focus on it, though the foot's rows it unfolds
  // are drawn.
  const pressed = renderBody(FOOT_ONLY);
  await waitFor(() => expect(document.activeElement).toBe(footControlOf(pressed)));
  await user.click(footControlOf(pressed) as HTMLElement);
  expect(rowsOfPart(pressed, "agents")).toEqual([D("minor")]);
  expect(document.activeElement).toBe(footControlOf(pressed));
});

// The app focuses its main element at each page change but the first (`app.tsx`), so a page
// opened on a list that holds only the foot by a link inside the app - from another issue, the
// Inbox or a board - finds the focus on what holds the list. Nobody put it there, as nobody puts
// it on the page itself, so the foot's control takes it, here once the decisions read answers
// after the page mounted. The body is drawn in the stage, as the page draws a view.
test("a list that holds only the foot, opened by a link inside the app, gives its control the focus once the reads answer", async () => {
  const answer = Promise.withResolvers<IssueDecisions>();
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockReturnValue(answer.promise);
  try {
    const view = renderBody({ ...FOOT_ONLY, made: null }, {}, { main: true, stage: true });
    // The foot is drawn from the tree, but until the read answers a row may yet come, so its
    // control takes no focus.
    await act(() => Promise.resolve());
    expect(footControlOf(view)).not.toBeNull();
    expect(document.activeElement).toBe(view.getByRole("main"));
    await act(async () => answer.resolve(decisionsRead(FOOT_ONLY.made)));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the reads answered after the page mounted, and the focus stayed on the app's main element"
      ).toBe(footControlOf(view))
    );
    view.unmount();
  } finally {
    getIssueDecisions.mockRestore();
  }
});

// The same with the reads cached, as on Back to the page's address: the body places the focus in
// the commit the page mounts in, and the app then moves it to its main element, since React runs
// a child's effects before its parent's. The control takes it back in the next frame.
test("a list that holds only the foot, opened by a link inside the app with its reads cached, takes back the focus the app puts on its main element", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const view = renderBody(FOOT_ONLY, {}, { main: true, stage: true });
  expect(
    document.activeElement,
    "the app's main element did not take the focus after the body placed it"
  ).toBe(view.getByRole("main"));
  await act(() => nextFrame());
  expect(
    document.activeElement,
    "the reads were cached, and the focus stayed on the app's main element"
  ).toBe(footControlOf(view));
  view.unmount();
});

// R13, the tester's F12: the header's count of what waits on the reader turns the filter on
// and puts the focus on a thing that waits, in the filter's order: each open question whose
// turn is the reader's, then each decision of the review list. Each press goes to the next
// of them and round to the first again. A press that lands before the reads are in is taken
// once the list holds something that waits.
test("each press of the count of what waits goes to the next thing that waits", async () => {
  const whole = wholeIssue();
  const view = renderBody(whole, { waitingPress: 1 });
  await waitFor(() => expect(focusedRow(view)).toBe("q-open"));
  // The same press is taken once: the reader's own move is not undone by it.
  await act(async () => rowOf(view, D("kept")).focus());
  view.again({ waitingPress: 1 });
  expect(focusedRow(view)).toBe(D("kept"));
  // The next press goes to the next thing that waits, wherever the reader has gone since.
  view.again({ waitingPress: 2 });
  await waitFor(() => expect(focusedRow(view)).toBe(D("second")));
  view.again({ waitingPress: 3 });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  // And round to the first again.
  view.again({ waitingPress: 4 });
  await waitFor(() => expect(focusedRow(view)).toBe("q-open"));
  view.unmount();

  // With no question waiting on the reader, the newest decision of the review list.
  const noQuestion = renderBody(
    { ...whole, children: whole.children.filter((child) => child.id !== "q-open") },
    { waitingPress: 1 }
  );
  await waitFor(() => expect(focusedRow(noQuestion)).toBe(D("second")));
  noQuestion.unmount();

  // A press that lands before the decisions read is in is taken once it is.
  const early = renderBody({ children: [statement("s1"), decisionNode("first")] });
  early.again({ waitingPress: 2 });
  expect(focusedRow(early)).toBeUndefined();
  await act(async () =>
    early.client.setQueryData(
      ["decisions", KEY],
      decisionsRead([decision("first", { links: [shaped(specRef("s1"))] })])
    )
  );
  await waitFor(() => expect(focusedRow(early)).toBe(D("first")));
});

// The tester's F6: a Change sent on a decision is a comment anchored to its words. Once it is
// in the comments read the decision says a change was asked for, leaves the review list for
// the reader's own decisions, draws no Keep or Change, and draws what the reader asked under
// its words with a link to it in the conversation.
test("a decision the reader sent a Change on says so, leaves the review list and draws the change", async () => {
  // The send reads the decisions document's text to say which place of the quote in it is the
  // decision's own, and the reading of that text is the page's engine's, which must be loaded.
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  const whole = wholeIssue();
  const sent = changeOf("second", "2026-11-16T09:00:00Z", "Name the read, please.");
  const create = spyOn(api, "createComment").mockResolvedValue(sent as never);
  const list = spyOn(api, "listComments").mockResolvedValue([sent] as never);
  const markdown = decisionsText(whole.made);
  const text = spyOn(api, "getArtifactText").mockImplementation(
    async (): Promise<ArtifactText> => ({
      markdown,
      markdown_tree: displayTree(markdown),
      token: "token",
      version: 1,
    })
  );
  const view = renderBody(whole);
  try {
    expect(rowsOfPart(view, "review")).toEqual([D("second"), D("first")]);
    const change = rowOf(view, D("second")).querySelector('button[data-row-action="change"]');
    if (change === null) throw new Error("the review row draws no Change");
    await user.click(change);
    await user.type(view.getByLabelText(CONTROL_LABELS.your_change), "Name the read, please.");
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    // The comment is anchored to the decision's own words in the decisions document, at the
    // first place they stand there: the heading above them ends with no full stop, so the quote
    // starts nowhere before them.
    expect(create.mock.calls[0]?.slice(0, 2)).toEqual([
      KEY,
      {
        anchor: { artifact: DECISIONS_DOCUMENT.id, occurrence: 0, quote: "The decision second." },
        body: "Name the read, please.",
      },
    ]);

    // The comments are read again, and the decision is no longer the reader's to review.
    await waitFor(() => expect(rowsOfPart(view, "review")).toEqual([D("first")]));
    // The reader sent it, so the focus goes on to the next decision of the review list.
    await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
    expect(list).toHaveBeenCalled();
    expect(rowsOfPart(view, "yours")[0]).toBe(D("second"));
    const row = rowOf(view, D("second"));
    expect(row.getAttribute("data-node-state")).toBe("change_requested");
    expect(words(row.querySelector("[data-node-margin] [data-node-state]"))).toBe(
      NODE_STATE_WORDS.decision.change_requested
    );
    expect(row.querySelectorAll("button[data-row-action]").length).toBe(0);
    expect(row.querySelector("[data-change-words]")?.textContent).toBe("Name the read, please.");
    const link = row.querySelector("a[data-change-link]");
    expect(words(link)).toBe(DECISIONS_WORDS.change);
    expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/comments/change-second`);
    expect(words(row.querySelector("[data-change-by]"))).toBe("alice");
    // The count of the review list's heading moved with it.
    expect(
      words(view.container.querySelector('[data-decisions-part="review"] [data-decisions-count]'))
    ).toBe("1");
  } finally {
    create.mockRestore();
    list.mockRestore();
    text.mockRestore();
  }
});

// R6: what a person wrote in a Change is drawn by the renderer, as a person's answer under an
// ask is (`RowText`), never as the characters they typed: its marks are drawn as what they are
// and no asterisk or backtick shows.
test("a Change's body is drawn through the renderer, its marks gone", async () => {
  await warmMarkdownRenderer();
  const body = "Name **the read**, and keep `x` as written.";
  const view = renderBody(wholeIssue());
  try {
    await act(async () =>
      view.client.setQueryData(
        ["comments", KEY],
        [changeOf("second", "2026-11-16T09:00:00Z", body)]
      )
    );
    await waitFor(() =>
      expect(rowOf(view, D("second")).querySelector("[data-change-words]")?.textContent).toBe(
        "Name the read, and keep x as written."
      )
    );
    expect(rowOf(view, D("second")).querySelector("[data-change-words] strong")?.textContent).toBe(
      "the read"
    );
  } finally {
    view.unmount();
  }
});

// INFRA-1523, R2: a Change its author edited says so after its words, as every surface marks an
// edited post: the person's words as stored, then ` · edited` in the page's own words
// (`EditedMark`). A Change nobody edited draws its words and nothing after them.
test("a Change its author edited draws its words, then · edited, and one nobody edited no mark", async () => {
  const edited: Comment = {
    ...changeOf("second", "2026-11-16T09:00:00Z", "Name the read, please."),
    edited_at: "2026-11-16T09:30:00Z",
  };
  const unedited = changeOf("first", "2026-11-16T10:00:00Z", "Keep it shorter.");
  const view = renderBody({ ...wholeIssue(), comments: [edited, unedited] });
  const changeWords = (block: string) => rowOf(view, D(block)).querySelector("[data-change-words]");
  await waitFor(() =>
    expect(changeWords("second")?.textContent).toBe("Name the read, please. · edited")
  );
  const mark = changeWords("second")?.lastElementChild;
  expect(mark?.textContent).toBe(" · edited");
  expect(mark?.hasAttribute(PAGE_CHROME_ATTRIBUTE)).toBe(true);
  expect(changeWords("first")?.textContent).toBe("Keep it shorter.");
  expect(changeWords("first")?.querySelector(`[${PAGE_CHROME_ATTRIBUTE}]`)).toBeNull();
});

// R2: the view draws four parts. What another person answered is no part of its own: it stands
// under Your decisions with the reader's own, newest first, and its answer names that person.
test("another person's answer stands under Your decisions with their name", () => {
  const view = renderBody({
    children: [
      answeredAsk("a-mine", { at: "2026-11-14T09:00:00Z", text: "Mine." }),
      answeredAsk("a-bobs", {
        at: "2026-11-13T09:00:00Z",
        by: { id: "bob", kind: "user" },
        text: "Bob's.",
      }),
    ],
  });
  expect(rowsOfPart(view, "yours")).toEqual(["a-mine", "a-bobs"]);
  expect(words(rowOf(view, "a-bobs").querySelector("[data-answer-author]"))).toBe("bob");
  expect(view.container.querySelector('[data-decisions-part="others"]')).toBeNull();
});

// The tester's F16: a key with nowhere to go says so at the row it was pressed on, and the
// report goes when the reader moves on to another row or out of the list (decision 105 as
// amended, `useReleaseFocus`). The focus leaving the list with no press under way, as Tab or a
// script moves it, takes the report away at once. A press on another control of the page keeps
// it drawn while the press is held, so nothing moves under the pointer, and takes it away once
// the press has ended and its click has landed. Pressed on no row, on the foot's control say,
// it says so at the list's tab stop, as the outline's keys do. Until the reads that say which
// questions and decisions there are hold their answers, there being nowhere to go may be false,
// so the key says nothing.
test("a key with nowhere to go reports at the row it was pressed on", async () => {
  const user = userEvent.setup();
  // The decisions read the mount starts never answers.
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockReturnValue(
    Promise.withResolvers<never>().promise
  );
  try {
    const cold = renderBody({ ...wholeIssue(), made: null });
    await act(async () => rowOf(cold, "q-open").focus());
    await user.keyboard("w");
    expect(focusedRow(cold)).toBe("q-open");
    expect(cold.container.querySelector("[data-row-report]")).toBeNull();
    cold.unmount();
  } finally {
    getIssueDecisions.mockRestore();
  }

  // Every read holds its answer from here on.
  const view = renderBody(wholeIssue(), {}, { requests: true });
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(words(rowOf(view, "q-open").querySelector("[data-row-report]"))).toBe(
    KEY_REPORTS.no_decision
  );
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.keyboard("j");
  expect(view.container.querySelector("[data-row-report]")).toBeNull();

  // A script's focus on another control takes the report away at once.
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  const elsewhere = view.getByRole("button", { name: "Elsewhere" });
  await act(async () => elsewhere.focus());
  expect(view.container.querySelector("[data-row-report]")).toBeNull();

  // A press on it takes the focus as it goes down and keeps the report while it is held, and
  // the report goes once the press is let go.
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.pointer({ keys: "[MouseLeft>]", target: elsewhere });
  expect(document.activeElement).toBe(elsewhere);
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.pointer({ keys: "[/MouseLeft]", target: elsewhere });
  await waitFor(() => expect(view.container.querySelector("[data-row-report]")).toBeNull());
  view.unmount();

  // An issue with no open question, and `q` pressed on the foot's control.
  const whole = wholeIssue();
  const noQuestion = renderBody({
    ...whole,
    children: whole.children.filter((child) => child.id !== "q-open"),
  });
  const control = footControlOf(noQuestion);
  await act(async () => control?.focus());
  await user.keyboard("q");
  expect(document.activeElement).toBe(control);
  expect(words(rowOf(noQuestion, D("second")).querySelector("[data-row-report]"))).toBe(
    KEY_REPORTS.no_next_question
  );
  expect(noQuestion.container.querySelectorAll("[data-row-report]").length).toBe(1);
});

// The parts of the list stand at one level, so Left on a row of the parts has no level above it
// to go to, and it says so at the row in the list's own words: the outline's report for that key
// speaks of the whole issue, which the list is not. The focus stays on the row.
test("Left on a row of the parts says the list has no level above it", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await act(async () => rowOf(view, D("first")).focus());
  await user.keyboard("{ArrowLeft}");
  expect(focusedRow(view)).toBe(D("first"));
  expect(words(rowOf(view, D("first")).querySelector("[data-row-report]"))).toBe(
    ANSWER_LINES.no_level_above
  );
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
});

// A row of the foot is drawn a level down, under the foot's control, so Left on one goes up to
// that control, as Left goes to a row's parent in the outline, and says nothing, whatever foot
// row stands above it. The last steps pin that the foot's handler never reaches a row of the
// parts: with the foot open, Left on one still says the list has no level above it.
test("Left on a row of the foot goes up to the foot's control", async () => {
  const view = renderBody(wholeIssue());
  const left = (on: HTMLElement) =>
    act(async () => {
      on.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "ArrowLeft" })
      );
    });
  const control = footControlOf(view) as HTMLElement;
  await act(async () => control.click());
  expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
  const foot = rowOf(view, D("replaced"));
  expect(foot).toHaveAttribute("aria-level", "2");
  await act(async () => foot.focus());
  await left(foot);
  expect(control).toHaveFocus();
  expect(view.container.querySelector("[data-row-report]")).toBeNull();

  const first = rowOf(view, D("first"));
  await act(async () => first.focus());
  await left(first);
  expect(first).toHaveFocus();
  expect(words(first.querySelector("[data-row-report]"))).toBe(ANSWER_LINES.no_level_above);
});

// Nothing inside a row takes the tab order but a block of the focused row's words whose content
// runs past its box: a wide code block keeps a tab stop of its own there, named, so a reader on
// the keyboard can scroll it sideways, and the same block on a row the reader is not on keeps
// none.
test("a focused row's wide code block keeps a tab stop, and another row's does not", async () => {
  // The rows draw the reading of their words, which is the page's engine's.
  await warmMarkdownRenderer();
  const text = `Run it as one line.\n\n\`\`\`\n${"x".repeat(400)}\n\`\`\``;
  const tree = displayTree(text);
  // The rules that make a code block in a row's words scroll, which the row reads through the
  // computed style, and its line laid out past its box, as a wide one's is: a unit test's DOM
  // loads no style sheet and lays nothing out.
  const sheet = wordsScrollSheet();
  document.head.append(sheet);
  const restore = overflowing("[data-node-words] pre");
  try {
    const view = renderBody({
      children: [
        statement("s1"),
        decisionNode("first", { text, tree }),
        decisionNode("second", { text, tree }),
      ],
      made: [
        decision("first", { links: [shaped(specRef("s1"))] }),
        decision("second", { links: [shaped(specRef("s1"))] }),
      ],
    });
    /** The code block of one row's words: its name, and its place in the tab order. */
    const code = (id: string) => {
      const block = rowOf(view, id).querySelector("[data-node-words] pre");
      if (block === null) throw new Error(`the row of ${id} draws no code block in its words`);
      return { label: block.getAttribute("aria-label"), tabIndex: block.getAttribute("tabindex") };
    };
    await act(async () => rowOf(view, D("first")).focus());
    await waitFor(() =>
      expect(code(D("first")), "the focused row's wide code block keeps no tab stop").toEqual({
        label: CONTROL_LABELS.scrolls_code,
        tabIndex: "0",
      })
    );
    expect(code(D("second"))).toEqual({ label: null, tabIndex: "-1" });
    view.unmount();
  } finally {
    restore();
    sheet.remove();
  }
});

// R20: the filter by words narrows the list to the nodes the filter's own view kept. The keys
// that look past the drawn rows walk what it kept first, then the rest of the issue: `q` goes to
// the open question it kept before one it took out, and a question or a decision it took out,
// which draws no row here, `q` and `w` open in the conversation, as an entry's link does. Where
// the issue holds no open question at all, `q` says so.
test("the body draws the entries the filter's view left", async () => {
  const whole = wholeIssue();
  const view = renderBody(whole, { view: issueTree([openAsk("q-open"), decisionNode("first")]) });
  expect(
    [...view.container.querySelectorAll("[data-node-row]")].map((row) =>
      row.getAttribute("data-node-id")
    )
  ).toEqual(["q-open", D("first")]);
  view.unmount();

  const user = userEvent.setup();
  const narrowed = renderBody(
    { ...whole, children: [...whole.children, openAsk("q-later")] },
    { view: issueTree([openAsk("q-later"), decisionNode("first")]) }
  );
  await act(async () => rowOf(narrowed, D("first")).focus());
  await user.keyboard("q");
  expect(focusedRow(narrowed)).toBe("q-later");
  // On the last question it kept, `q` goes on to the one it took out.
  await user.keyboard("q");
  await waitFor(() =>
    expect(narrowed.getByTestId("where").textContent).toBe(`/issues/${KEY}/asks/q-open`)
  );
  narrowed.unmount();

  // From the one decision it kept, `q` goes to the open question it took out.
  const questionOut = renderBody(whole, { view: issueTree([decisionNode("first")]) });
  await act(async () => rowOf(questionOut, D("first")).focus());
  await user.keyboard("q");
  await waitFor(() =>
    expect(questionOut.getByTestId("where").textContent).toBe(`/issues/${KEY}/asks/q-open`)
  );
  questionOut.unmount();

  // `w` on a question it kept goes to the decision behind it, which it took out: the decision
  // names the question's address among its chips.
  const behind = decisionNode("first", {
    chips: [
      {
        answered: true,
        gone: false,
        reading: { words: "Question q-open?" },
        ref: `dispatch://${KEY}/ask/q-open`,
      },
    ],
  });
  const decisionOut = renderBody(
    { children: [statement("s1"), openAsk("q-open"), behind], made: [decision("first")] },
    { view: issueTree([openAsk("q-open")]) }
  );
  await act(async () => rowOf(decisionOut, "q-open").focus());
  await user.keyboard("w");
  await waitFor(() =>
    expect(decisionOut.getByTestId("where").textContent).toBe(`/issues/${KEY}#b-first`)
  );
  decisionOut.unmount();

  // A check no decision names, on an address that names it: `w` on its row goes to the
  // decision that shaped the statement it is drawn under, which is the decision the outline's
  // own walk finds behind it over the whole tree. Named instead, the statement goes to that
  // decision too.
  const check = node({
    id: "verification#b-c1",
    reference: `dispatch://${KEY}/verification#b-c1`,
    text: "The page checks s1.",
    type: "check",
  });
  const checked = [
    statement("s1", { children: [check] }),
    decisionNode("first", {
      chips: [
        {
          answered: true,
          gone: false,
          reading: { words: "The page draws s1." },
          ref: specRef("s1"),
        },
      ],
    }),
  ];
  expect(decisionBehind(issueTree(checked), check.id)?.id).toBe(D("first"));
  const shapesS1 = [decision("first", { links: [shaped(specRef("s1"))] })];
  for (const named of [check.id, statement("s1").id]) {
    const at = renderBody(
      { children: checked, made: shapesS1 },
      {
        reveal: named,
        view: presetView(issueTree(checked), "decisions", "", new Set([named])).root,
      }
    );
    await waitFor(() => expect(focusedRow(at)).toBe(named));
    await user.keyboard("w");
    await waitFor(() => expect({ named, row: focusedRow(at) }).toEqual({ named, row: D("first") }));
    at.unmount();
  }

  // An issue that holds no open question, kept or not.
  const noQuestion = renderBody(
    { ...whole, children: whole.children.filter((child) => child.id !== "q-open") },
    { view: issueTree([decisionNode("first")]) }
  );
  await act(async () => rowOf(noQuestion, D("first")).focus());
  await user.keyboard("q");
  expect(words(rowOf(noQuestion, D("first")).querySelector("[data-row-report]"))).toBe(
    KEY_REPORTS.no_next_question
  );
  expect(noQuestion.getByTestId("where").textContent).toBe("/");
});

// R2: the filter is drawn at every width. Nothing the body draws is taken off the page at a
// width: no element of it carries a class that hides it, at any breakpoint. What each width
// lays out is the browser's to say (c-decisions at 1440 px and at 390 px).
test("nothing the body draws is hidden at a width", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const hiding = /(^|\s)(\S+:)?(hidden|invisible|sr-only)(\s|$)/;
  expect(
    [...view.container.querySelectorAll("[data-decisions-body], [data-decisions-body] *")]
      .filter((element) => hiding.test(element.getAttribute("class") ?? ""))
      .map((element) => element.getAttribute("class"))
  ).toEqual([]);
  expect(view.container.querySelectorAll("[data-decisions-body] [hidden]").length).toBe(0);
});
