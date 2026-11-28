// The markdown `e2e/issue-layout-seed.ts` writes. One builder per document, so the whole issue,
// the request baseline and the twice-the-nodes issue write the same shapes at different sizes.
// Every typed block carries an explicit `#id`, so a seed names the ids its tests read rather than
// looking one up by its text.

/**
 * The three sentences a seeded spec puts at its head: three spoken sentences, which is what the
 * head draws one bullet of each.
 *
 * With the block's own fence they are inside the 240 runes the server cuts a block's reference
 * excerpt to (`packages/envoy/internal/dispatch/refs/graph.go`), which is the text the tree
 * carries for a node and the one the head reads: the head parses no stored markdown of its own.
 * They hold characters outside ASCII, so a cut made over bytes rather than runes would show in
 * what c-three-things reads back. A summary written longer than that excerpt loses its last
 * sentence to the cut, and the head draws the whole sentences it has rather than a fragment,
 * which is the rule a later part's unit test of the head reads.
 */
export const SUMMARY_SENTENCES = [
  "The issue page draws one ordered body in place of nine tabs.",
  "Every property with a value is visible at every width.",
  "Each requirement's reasoning — its decisions — is one press away.",
] as const;

/** The block id of those three sentences. `callout` is the nearest thing today's block schema
 *  holds to a summary node (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:1-19`,
 *  read 2026-11-05: the five types are `callout`, `ask`, `requirement`, `plan_task` and
 *  `check`). */
export const SUMMARY_BLOCK_ID = "summary";

/** The block id of the `ask` block a seeded spec carries, which the server indexes into the
 *  issue's open question. */
export const OPEN_QUESTION_BLOCK_ID = "q-open";

/** The block id of the question whose answer shaped three statements. */
export const ANSWERED_QUESTION_BLOCK_ID = "q-addresses";

/** How a seeded check's newest result stands, in the words `NODE_STATE_WORDS.check` draws
 *  (`web/src/features/issue-page/page-words.ts`), plus `no-control`: a pass of an after-merge
 *  check that records no negative control, which reads `passed` on the page and draws no gap -
 *  the server refuses such a result of a before-merge check, so the page names nothing it lacks
 *  - and is what `c-chain` reads that for. */
export const CHECK_STATE_ORDER = [
  "passed",
  "failed",
  "no-result",
  "older",
  "unmarked",
  "no-control",
] as const;

export type SeededCheckState = (typeof CHECK_STATE_ORDER)[number];

/** The state check `#c<index>` is seeded in: the six states in order, repeating for a document
 *  that holds more than six checks. */
export function checkStateOf(index: number): SeededCheckState {
  return CHECK_STATE_ORDER[(index - 1) % CHECK_STATE_ORDER.length];
}

export function requirementBlockId(index: number): string {
  return `r${index}`;
}

export function checkBlockId(index: number): string {
  return `c${index}`;
}

export function planTaskBlockId(index: number): string {
  return `t${index}`;
}

/** The block id of the requirement whose own words cite a document of the issue. The
 *  citation sits inside a typed block, because a document a block cites is a node under that
 *  block and a paragraph of a document is the document's own text and no block of the tree at
 *  all. A requirement is the typed block that carries it: a requirement whose `from` is absent
 *  sits under the issue, so the document it cites is drawn one level under that, inside the
 *  three levels the fold draws. */
export const CITATION_BLOCK_ID = "r-cites";

/** The words of that requirement, which name one document of `issueKey` by its address. */
export function citationText(issueKey: string, slug: string): string {
  return `The measurements behind the first requirement are in dispatch://${issueKey}/artifact/${slug}.`;
}

/** That requirement as markdown, for a seed to insert once the document it cites exists. */
export function citationRequirement(issueKey: string, slug: string): string {
  return `:::requirement{#${CITATION_BLOCK_ID}}\n${citationText(issueKey, slug)}\n:::`;
}

/** The `dispatch://` address of one block of an issue's spec. */
export function specBlockRef(issueKey: string, blockId: string): string {
  return `dispatch://${issueKey}/spec#b-${blockId}`;
}

/** `1` to `count`. */
function counted(count: number): number[] {
  return Array.from({ length: count }, (_, index) => index + 1);
}

/** Requirement `#r1` holds two sentences and more than 100 characters, so a test can read a row's
 *  first line against the stored text; every other requirement holds one short sentence. */
export function requirementText(index: number): string {
  if (index === 1) {
    return (
      "Every property of the issue that carries a value is visible at every width the page draws, " +
      "with no control opened and no sideways scroll. The right-hand panel holds the rest."
    );
  }
  return `Requirement ${index} of the redesign stands on a row of its own.`;
}

export function checkText(index: number): string {
  return `Check ${index} of the redesign holds its own row and its own state.`;
}

/** The words check `#c<index>` carries after the seed rewords it, which makes every result
 *  recorded against its earlier words read `older`. */
export function rewordedCheckText(index: number): string {
  return `Check ${index} of the redesign holds its own row, its own state and its own shape.`;
}

export function planTaskText(index: number): string {
  return `Task ${index} of the redesign is one step of the build.`;
}

/** The block id of decision `index` of a seeded spec's `## Decisions` part. */
export function decisionBlockId(index: number): string {
  return `d${index}`;
}

/** One decision of a seeded spec, with the facts its `decision` block carries as attributes: the
 *  blocks it shaped (`shaped`), whether it settles the seed's answered ask (`from_ask`), whether
 *  it is unsure (`uncertainty="high"`) and whether a later one replaced it (`replaced`). */
interface SeededDecision {
  readonly body: string;
  readonly fromAsk?: boolean;
  readonly heading: string;
  readonly replaced?: boolean;
  readonly shaped: readonly string[];
  readonly unsure?: boolean;
}

/** The decisions a seeded spec holds, in document order, the last one replaced. Each is the
 *  planner's `decision` block of the spec's `## Decisions` part, a node of the store, whose facts
 *  are its block's attributes (`SeededDecision`). Its words name the blocks it shaped or the
 *  ask it settles; the ask carries its own ties. */
function seededDecisions(options: { askRef: string; issueKey: string }): SeededDecision[] {
  const block = (index: number) => specBlockRef(options.issueKey, requirementBlockId(index));
  return [
    {
      body: `Chosen so the page makes no request per node. Shapes ${block(1)} and ${block(2)}.`,
      heading: "Draw the tree from one read",
      shaped: [block(1), block(2)],
    },
    {
      body: `Decided in ${options.askRef}.`,
      fromAsk: true,
      heading: "Keep every address the old page answered",
      shaped: [],
    },
    {
      body: "A reader who never finds the newest question would change this.",
      heading: "Fold to the frontier and not to a depth",
      shaped: [],
      unsure: true,
    },
    {
      body: "A word and a shape carry every state, so no reader depends on a colour.",
      heading: "Name every state with a word and a shape",
      shaped: [],
    },
    {
      body: `One panel holds the properties and the conversation. Shapes ${block(4)}.`,
      heading: "Put every remaining property in one panel",
      shaped: [block(4)],
    },
    {
      body: `A document is a node under the block that cites it. Shapes ${block(5)}.`,
      heading: "Draw a document under the block that cites it",
      shaped: [block(5)],
    },
    {
      body: `Replaced by the tree above: the rail hid 744 px at 1440 px. Shapes ${block(6)}.`,
      heading: "Keep the metadata rail behind a sideways scroll",
      replaced: true,
      shaped: [block(6)],
    },
  ];
}

/** The words that close decision `index`'s block of a seeded spec: the last of its words, which
 *  hold no line break, so a caret at the block's end stands after them. */
export function decisionClosingWords(
  issueKey: string,
  index: number,
  answeredAskId: string
): string {
  const askRef = `dispatch://${issueKey}/ask/${answeredAskId}`;
  return seededDecisions({ askRef, issueKey })[index - 1]?.body.split(". ").at(-1) ?? "";
}

/** The spec's `## Decisions` part holding the first `count` seeded decisions, each a `decision`
 *  block whose id is `decisionBlockId`, for a seed to append to the spec once `askRef`, the ask
 *  the second one settles, has been answered. */
export function decisionsPart(options: {
  askRef: string;
  count: number;
  issueKey: string;
}): string {
  const blocks = seededDecisions(options)
    .slice(0, options.count)
    .map((decision, at) => {
      const attributes = [
        `#${decisionBlockId(at + 1)}`,
        ...(decision.shaped.length > 0 ? [`shaped="${decision.shaped.join(" ")}"`] : []),
        ...(decision.fromAsk === true ? [`from_ask="${options.askRef}"`] : []),
        ...(decision.unsure === true ? ['uncertainty="high"'] : []),
        ...(decision.replaced === true ? ['replaced="true"'] : []),
      ];
      return `:::decision{${attributes.join(" ")}}\n### ${decision.heading}\n\n${decision.body}\n:::`;
    });
  return `## Decisions\n\n${blocks.join("\n\n")}\n`;
}

function summaryCallout(): string {
  const sentences = SUMMARY_SENTENCES.join(" ");
  return `:::callout{#${SUMMARY_BLOCK_ID} title="What this issue is"}\n${sentences}\n:::`;
}

function openQuestionBlock(): string {
  return `:::ask{#${OPEN_QUESTION_BLOCK_ID} urgency="med"}
At which width does the identity block condense?

- 1024 px: the phone layout begins there anyway.
- 1280 px: the preset row keeps its labels above that width.
:::`;
}

/** The question the planner adds once it can name the statements the answer shaped. */
export function answeredQuestionBlock(issueKey: string): string {
  const shaped = [3, 4, 5].map((index) => specBlockRef(issueKey, requirementBlockId(index)));
  return `:::ask{#${ANSWERED_QUESTION_BLOCK_ID} urgency="high" shaped="${shaped.join(" ")}"}
Which addresses must the new page still answer?
:::`;
}

/** The issue's initial spec: its three sentences, its requirements, and the `ask` block the server
 *  indexes into an open question two levels below the issue (issue, document, block). */
export function specDocument(options: {
  openQuestion: boolean;
  requirements: number;
  summary: boolean;
}): string {
  const blocks: string[] = [];
  if (options.summary) blocks.push(summaryCallout());
  for (const index of counted(options.requirements)) {
    blocks.push(`:::requirement{#${requirementBlockId(index)}}\n${requirementText(index)}\n:::`);
  }
  if (options.openQuestion) blocks.push(openQuestionBlock());
  return `${blocks.join("\n\n")}\n`;
}

/** The issue's Verification document: `checks` `check` blocks, each serving one requirement in
 *  turn and each seeded in the state `checkStateOf` names. The one in `no-control` carries no
 *  `negative_control` attribute at all, which is what "records no negative control" is. It runs
 *  `after_merge`, since a server that asks a negative control of a before-merge pass refuses a
 *  passing `before_merge` result with no negative control (`NEGATIVE_CONTROL_REQUIRED`) and
 *  takes a pass of any other check; the one in `unmarked` runs `after_merge` too, so with the App
 *  gone it is the branch nothing can be read of (`recordSeededResults`). */
export function verificationDocument(options: {
  checks: number;
  issueKey: string;
  requirements: number;
}): string {
  const blocks = counted(options.checks).map((index) => {
    const served = ((index - 1) % options.requirements) + 1;
    const serves = specBlockRef(options.issueKey, requirementBlockId(served));
    const state = checkStateOf(index);
    const control =
      state === "no-control"
        ? ""
        : ' negative_control="Draw a row with no state and the test fails."';
    const when = state === "no-control" || state === "unmarked" ? "after_merge" : "before_merge";
    return (
      `:::check{#${checkBlockId(index)} serves="${serves}" surface="browser"` +
      ` run="bun run e2e -- e2e/issue-layout.e2e.ts" when="${when}"${control}}\n` +
      `${checkText(index)}\n:::`
    );
  });
  return `How the issue page is proved.\n\n${blocks.join("\n\n")}\n`;
}

/** The issue's plan: `planTasks` `plan_task` blocks. Every task but the last serves one
 *  requirement, and tasks 2 to 7 need the task before them, which is the 14 pointers the Plan
 *  tab of the page's request baseline counts one reference read for. */
export function planDocument(options: {
  issueKey: string;
  planTasks: number;
  requirements: number;
}): string {
  const taskRef = (index: number) =>
    `dispatch://${options.issueKey}/artifact/plan-md#b-${planTaskBlockId(index)}`;
  const blocks = counted(options.planTasks).map((index) => {
    const attributes: string[] = [];
    if (index < options.planTasks) {
      const served = ((index - 1) % options.requirements) + 1;
      attributes.push(`serves="${specBlockRef(options.issueKey, requirementBlockId(served))}"`);
    }
    if (index >= 2 && index <= 7) attributes.push(`needs="${taskRef(index - 1)}"`);
    const head = attributes.length === 0 ? "" : ` ${attributes.join(" ")}`;
    return `:::plan_task{#${planTaskBlockId(index)}${head}}\n${planTaskText(index)}\n:::`;
  });
  return `How the issue page gets built.\n\n${blocks.join("\n\n")}\n`;
}

/** The ids of the four requirements `seedSpineCases` writes. */
export const SPINE_REQUIREMENTS = {
  servedTwiceA: "r-served-a",
  servedTwiceB: "r-served-b",
  shapedElsewhere: "r-shaped-elsewhere",
  unexplained: "r-unexplained",
} as const;

/** The block id of the one decision `seedSpineCases` writes, a `decision` block of the spec's
 *  `## Decisions` part, which is the block the decisions read answers it under. */
export const SPINE_DECISION_BLOCK_ID = "d-spine";

/** The block id of the check `seedSpineCases` writes. */
export const SPINE_CHECK_BLOCK_ID = "c-two";

/** The question `seedSpineCases` asks and answers, which the one decision names. A chip names
 *  its target by the target's own words and by its address's short form - `LAY-1 ask` - and
 *  never by the target's id (`web/src/features/issue-page/NodeRow.tsx`'s `Chip`), so a spec looking
 *  for that edge looks for these words. */
export const SPINE_ANSWERED_QUESTION = "Which node does a requirement with no parent sit under?";

export const SPINE_SPEC = `:::requirement{#${SPINE_REQUIREMENTS.unexplained}}
Nothing explains this requirement: it names no node it came from.
:::

:::requirement{#${SPINE_REQUIREMENTS.shapedElsewhere}}
A decision links this requirement while the answered question stands elsewhere.
:::

:::requirement{#${SPINE_REQUIREMENTS.servedTwiceA}}
The first requirement one check serves.
:::

:::requirement{#${SPINE_REQUIREMENTS.servedTwiceB}}
The second requirement that same check serves.
:::
`;

/** The spine's `## Decisions` part: one `decision` block whose `shaped` names `shapedElsewhere`
 *  while the ask it settles (`from_ask`), which its words name too, is the answered question a
 *  reader would read as that requirement's origin. */
export function spineDecisionsPart(issueKey: string, askId: string): string {
  const shaped = specBlockRef(issueKey, SPINE_REQUIREMENTS.shapedElsewhere);
  const ask = `dispatch://${issueKey}/ask/${askId}`;
  return (
    `## Decisions\n\n:::decision{#${SPINE_DECISION_BLOCK_ID} shaped="${shaped}" from_ask="${ask}"}\n` +
    "### Link a requirement the question does not\n\n" +
    `Shapes ${shaped}, while ${ask} is the question that was answered.\n:::\n`
  );
}

/** The spine's Verification document: one check whose `serves` names two requirements. */
export function spineVerificationDocument(issueKey: string): string {
  const serves = [SPINE_REQUIREMENTS.servedTwiceA, SPINE_REQUIREMENTS.servedTwiceB]
    .map((requirement) => specBlockRef(issueKey, requirement))
    .join(" ");
  return (
    `How the spine is proved.\n\n:::check{#${SPINE_CHECK_BLOCK_ID} serves="${serves}" ` +
    `surface="browser" run="bun run e2e -- e2e/issue-layout.e2e.ts" ` +
    `negative_control="Draw the check under one parent and the test fails."}\n` +
    "One check serves two requirements and is drawn under both.\n:::\n"
  );
}

/** The block ids of the plan `planOrderDocument` writes. */
export const PLAN_ORDER_TASKS = {
  /** Needs `second`, and is written first: the plan's own order is not the order of its needs. */
  first: "t1",
  /** The task `loose` needs, which the second version of the document drops. */
  gone: "t9",
  /** The two that need each other, which the page draws as one line that lists both. */
  knotA: "t4",
  knotB: "t5",
  /** Needs nothing the plan holds any more: `seedPlanOrder` takes `gone` out again. */
  loose: "t6",
  second: "t2",
  /** Needs nothing, so it is the first step drawn. */
  third: "t3",
} as const;

/** A plan whose tasks are written in an order their `needs` refuse: `t1` needs `t2` and `t2`
 *  needs `t3`, so the page draws `t3`, `t2`, `t1`; `t3` serves two statements, and is one task;
 *  `t4` and `t5` need each other; `t6` needs `t9`, which `seedPlanOrder` then removes, leaving
 *  a `needs` the plan does not hold - the one way such a pointer is made, since a write that
 *  introduces one is refused and a write that keeps one unchanged is not
 *  (`packages/envoy/internal/dispatch/pmdoc/pointers.go`). */
export function planOrderDocument(issueKey: string, holdsGone: boolean): string {
  const task = (id: string) => `dispatch://${issueKey}/artifact/plan-md#b-${id}`;
  const T = PLAN_ORDER_TASKS;
  const block = (id: string, serves: string, needs?: string, alsoServes?: string) =>
    `:::plan_task{#${id} serves="${specBlockRef(issueKey, serves)}` +
    `${alsoServes === undefined ? "" : ` ${specBlockRef(issueKey, alsoServes)}`}"` +
    `${needs === undefined ? "" : ` needs="${task(needs)}"`}}\n` +
    `Task ${id} of the redesign is one step of the build.\n:::`;
  const blocks = [
    block(T.first, requirementBlockId(1), T.second),
    block(T.second, requirementBlockId(2), T.third),
    block(T.third, requirementBlockId(3), undefined, requirementBlockId(1)),
    block(T.knotA, requirementBlockId(1), T.knotB),
    block(T.knotB, requirementBlockId(2), T.knotA),
    block(T.loose, requirementBlockId(3), T.gone),
    ...(holdsGone ? [block(T.gone, requirementBlockId(1))] : []),
  ];
  return `How the issue page gets built.\n\n${blocks.join("\n\n")}\n`;
}
