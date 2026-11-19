import { act, type RenderResult } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { artifact as specDocument } from "../../__tests__/proof-document";
import { renderWithQueries } from "../../__tests__/query-provider";
import type {
  Agent,
  ArchitectureTree,
  Artifact,
  ArtifactApproval,
  Ask,
  AskRead,
  Priorities,
  PriorityRow,
} from "../../api/types";
import { PAGE_COLORS } from "../../theme/classes";
import { ProofDocument } from "../doc/ProofDocument";
import { DocumentRuntime } from "../doc/runtime";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { MarginProvider } from "../margin/margin-context";
import { NoTooltips } from "./NoTooltips";

/** What `shared-controls.options.test.tsx` and `shared-controls.tooltips.test.tsx` both draw the
 *  shared controls with, and read them by. */

/** Which side of `NoTooltips` a control is drawn on: `inside` is the new issue page's surface,
 *  `outside` every page of `main`. */
export type Surface = "inside" | "outside";

/** Draws `node` the way a page does: under a router and a query client of its own, inside
 *  `NoTooltips` or outside it. */
export function draw(node: ReactNode, surface: Surface): RenderResult {
  return renderWithQueries(
    <MemoryRouter>{surface === "inside" ? <NoTooltips>{node}</NoTooltips> : node}</MemoryRouter>
  );
}

/** Takes a drawn control off the page, its container with it, so the next draw's read of
 *  `document.body` holds that draw alone. */
export function undraw(view: RenderResult): void {
  view.unmount();
  view.container.remove();
}

/** Lets every read a control started answer and every render after it land. */
export async function settleReads(): Promise<void> {
  for (let turn = 0; turn < 5; turn += 1) {
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
  }
}

/** The tokens of a class string, for reading whether an element carries each. */
export function tokens(className: string): string[] {
  return className.split(/\s+/).filter(Boolean);
}

export function classOf(element: Element): string[] {
  return tokens(element.getAttribute("class") ?? "");
}

/** What holds an element at its width or on one line: `shrink-0` and `flex-none` keep its width
 *  in a flex row, `whitespace-nowrap`, `text-nowrap` and `whitespace-pre` keep its words on one
 *  line, and `truncate` cuts what does not fit. */
export const HOLDS_ITS_LINE = [
  "flex-none",
  "shrink-0",
  "text-nowrap",
  "truncate",
  "whitespace-nowrap",
  "whitespace-pre",
];

/** What of `HOLDS_ITS_LINE` an element carries, its inline `flex-shrink: 0` among them. */
export function holdsItsLine(element: Element): string[] {
  const held = classOf(element).filter((token) => HOLDS_ITS_LINE.includes(token));
  return element instanceof HTMLElement && element.style.flexShrink === "0"
    ? [...held, "flex-shrink: 0"]
    : held;
}

/** `theme/no-raw-colors.test.ts`'s `COLOR_FAMILIES` and `RAW_COLOR_PATTERN`, copied as they
 *  stand there: they are constants of that test file, which exports nothing. The pattern is the
 *  palette guard's own reading of a raw Tailwind colour. */
const COLOR_FAMILIES = [
  "slate",
  "sky",
  "rose",
  "amber",
  "emerald",
  "red",
  "orange",
  "yellow",
  "lime",
  "green",
  "teal",
  "cyan",
  "blue",
  "indigo",
  "violet",
  "purple",
  "fuchsia",
  "pink",
  "gray",
  "zinc",
  "neutral",
  "stone",
] as const;
export const RAW_COLOR_PATTERN = new RegExp(
  `\\b(?:text|bg|border|ring|placeholder|divide|outline|caret|accent|decoration|from|via|to)-(?:(?:${COLOR_FAMILIES.join("|")})-\\d{2,3}|white|black|transparent)\\b`
);

/** The variants of a focus indicator, kept inside `NoTooltips`: `focusVisibleRing`,
 *  `hasFocusVisibleRing` and `main`'s focus rule on a field stay, since the page's palette holds
 *  no focus colour and the application has one focus indicator. */
export const FOCUS_VARIANTS: readonly string[] = ["focus", "focus-visible", "has-focus-visible"];

/** A token as its variants and the utility after them. A colon inside brackets or parentheses
 *  is an arbitrary value's (`[overflow-wrap:anywhere]`, `content-[attr(data-initial)]`), not a
 *  variant's. */
export function splitToken(token: string): {
  readonly utility: string;
  readonly variants: string[];
} {
  const variants: string[] = [];
  let depth = 0;
  let start = 0;
  for (let index = 0; index < token.length; index += 1) {
    const character = token[index];
    if (character === "[" || character === "(") depth += 1;
    else if (character === "]" || character === ")") depth -= 1;
    else if (character === ":" && depth === 0) {
      variants.push(token.slice(start, index));
      start = index + 1;
    }
  }
  return { utility: token.slice(start), variants };
}

/** Every utility that takes a colour after its property, and what follows the property. */
const COLOUR_PROPERTY =
  /^-?(?:text|bg|border(?:-[xytrblse])?|outline|ring(?:-offset)?|inset-ring|shadow|inset-shadow|drop-shadow|fill|stroke|decoration|divide|placeholder|caret|accent|from|via|to)-(.+)$/;
const SHADE = new RegExp(`^(?:${COLOR_FAMILIES.join("|")})-\\d{2,3}$`);

/**
 * The colour a utility names, or `undefined` for a utility that names none.
 *
 * The same property prefix carries colours and sizes alike (`text-page-ink` and `text-xs`,
 * `border-page-line` and `border-2`, `ring-page-ink` and `ring-2`), so a utility names a colour
 * by what follows its property, an opacity (`/40`) dropped: a shade of a palette family
 * (`sky-700`), one of the keyword colours `white`, `black` and `transparent`, an arbitrary colour
 * (`[#fff]`, `[rgb(…)]`, `[color:…]`, `[var(--color-…)]`), or a token of the page's palette by its
 * own name (`page-ink`, `icon-rail`), which `PAGE_COLORS` must then hold, so a misspelt page
 * token is a colour and fails. Anything else after the same prefixes names no colour: a size
 * (`text-xs`, `border-2`, `ring-2`, `border-l-4`), a style (`border-dashed`, `outline-none`,
 * `text-left`, `shadow-lg`), an arbitrary value that is no colour (`text-[10px]`), and `current`
 * and `inherit`, which paint the element's own text colour rather than name one; that text
 * colour is a token of its own, read here too.
 */
export function colourOf(utility: string): string | undefined {
  const named = COLOUR_PROPERTY.exec(utility)?.[1];
  if (named === undefined) return undefined;
  const name = named.replace(/\/(?:\d+|\[[^\]]*\])$/, "");
  if (SHADE.test(name) || name === "white" || name === "black" || name === "transparent") {
    return name;
  }
  if (/^\[(?:#|rgba?\(|hsla?\(|oklch\(|oklab\(|color:|var\(--color-)/.test(name)) return name;
  return /^(?:page|icon-rail)(?:-[a-z]+)*$/.test(name) ? name : undefined;
}

/** What `elements` draw that is no colour of the page: each token the palette guard reads as
 *  raw, and each colour `PAGE_COLORS` does not name, focus indicators skipped. Each offender
 *  names the element it is on after `name`, what the elements are of. */
export function offPageColours(
  name: string,
  elements: readonly Element[]
): { readonly colours: number; readonly offenders: string[] } {
  const offenders: string[] = [];
  let colours = 0;
  for (const element of elements) {
    const testId = element.getAttribute("data-testid");
    const where = `${name}: <${element.tagName.toLowerCase()}${testId === null ? "" : ` data-testid="${testId}"`}>`;
    for (const token of classOf(element)) {
      const { utility, variants } = splitToken(token);
      if (variants.some((variant) => FOCUS_VARIANTS.includes(variant))) continue;
      if (RAW_COLOR_PATTERN.test(utility)) {
        offenders.push(`${where} ${token}: a raw colour`);
        continue;
      }
      const colour = colourOf(utility);
      if (colour === undefined) continue;
      colours += 1;
      if (!Object.hasOwn(PAGE_COLORS, colour)) offenders.push(`${where} ${token}: no page colour`);
    }
  }
  return { colours, offenders };
}

export const LIVE_SESSION = "session-live-0001";
export const STALLED_SESSION = "session-stalled-02";
export const GONE_SESSION = "session-gone-00003";

/** One session as `GET /api/v1/agents` lists it, seen three minutes ago (`AgentName.test.tsx`'s
 *  `listed`, which takes the heartbeat where this one takes the state). */
export function listed(sessionId: string, title: string, state: Agent["state"] = "live"): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: Date.now() - 3 * 60_000,
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    state,
    title,
  };
}

/** A priority as the priorities read answers it (`IssuePriorityLine.test.tsx`'s `priority`). */
function priority(id: string, title: string): PriorityRow {
  return {
    description: "",
    id,
    made_at: "2026-11-01T00:00:00Z",
    made_by: { id: "alice", kind: "user" },
    owner: "ACME controller",
    rank: id,
    state: "live",
    title,
    work: {
      by_status: {},
      claimed: 0,
      closed_last_7_days: 0,
      newest_event_at: null,
      open_asks: 0,
    },
  };
}

export const priorities: Priorities = {
  priorities: [
    priority("evals-now", "Evals collecting data now"),
    priority("rocq-lean", "Rocq and Lean speed work restarted"),
  ],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** The project's architecture, holding the one component the components line names (the first
 *  of `IssueComponentsLine.test.tsx`'s `tree`). */
export const architecture: ArchitectureTree = {
  components: [
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "dispatch-server",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "legion/legion",
      title: "Dispatch server",
      total: 0,
    },
  ],
  not_architectural: [],
  retired_links: [],
  sources: [],
  totals: {
    components_without_work: 0,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
};

export const SPEC_ID = "artifact-spec";
export const DECISIONS_ID = "artifact-decisions";
export const VERIFICATION_ID = "artifact-verification";

/** A document of the issue (`ApprovalSteps.test.tsx`'s `doc`). */
export function doc(id: string, name: string, approval?: ArtifactApproval): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: "CORE-1",
    kind: "doc",
    name,
    primary: id === SPEC_ID,
    project: "CORE",
    ref_key: `CORE-1/${name}`,
    session_id: null,
    slug: name.replace(".", "-"),
    versions: [],
    ...(approval === undefined ? {} : { approval }),
  };
}

/** An open blocking question the live session asked, as the Inbox hands it (`AskCard.test.tsx`'s
 *  `answeredAsk`, still open). */
export function openAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: {
      id: LIVE_SESSION,
      kind: "session",
      origin: { session_title: "Implementer", tmux: "legion:3" },
    },
    block_id: null,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which release path?",
    state: "open",
    urgency: "blocking",
    waiting_on: "human",
    ...overrides,
  };
}

/** The open request for the spec's version 1, which an approval entry's version link reads. */
export const specRequest: Ask = openAsk({
  approval: {
    artifact_id: SPEC_ID,
    documents: null,
    name: "spec.md",
    requested_version: 1,
    version: 1,
  },
  id: "ask-spec",
  kind: "approval",
});

/** The ask's read, as `GET /api/v1/asks/{id}` answers it before anyone answers
 *  (`AskCard.test.tsx`'s `thread`). */
export function threadOf(ask: Ask): AskRead {
  return {
    answers: [],
    ask,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  };
}

/** The ProofDocument suites' page (`__tests__/proof-document.tsx`'s `renderProofDocument`): the
 *  issue's spec with a fake document runtime and the margin, on its own page. */
export function proofDocument(): ReactNode {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  return (
    <DocumentRuntime.Provider value={fake.runtime}>
      <MarginProvider>
        <ProofDocument
          artifact={specDocument}
          highlight={undefined}
          isClosed={false}
          onToolbarChange={() => undefined}
          onVersionChange={() => undefined}
          owner={{ key: "CORE-1", kind: "issue" }}
          showDiff={false}
          surface="page"
          user={{ event_head: "0", kind: "user", login: "alice" }}
          version={undefined}
        />
      </MarginProvider>
    </DocumentRuntime.Provider>
  );
}

/** The header's priority write, which no case presses. */
export const idlePriorityWrite: IssuePriorityWrite = {
  failed: false,
  pending: false,
  retry: () => undefined,
  shown: null,
  submit: () => undefined,
};

export const minutesFromNow = (minutes: number) =>
  new Date(Date.now() + minutes * 60_000).toISOString();

/** The instant the claims of a case were made at. */
export const CLAIMED_AT = "2026-09-24T06:00:00Z";
