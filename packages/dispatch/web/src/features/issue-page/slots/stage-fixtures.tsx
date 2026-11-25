import { expect, type Mock, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import {
  createPath,
  MemoryRouter,
  type NavigateFunction,
  type Navigator,
  Router,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { ApiError, api } from "../../../api/client";
import type { IssueDetails } from "../../../api/types";
import { MarginProvider } from "../../margin/margin-context";
import { ReferenceNamesProvider } from "../../refs/reference-names";
import { parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { issueDetails, KEY } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import type { Preset } from "../page-words";
import { FAILED_READ_RETRY } from "./failed-read-line";
import { ProvidersSlot } from "./providers";
import { type SlotsFilled, StageSlot, type ViewSlots } from "./stage";

/**
 * What the stage's unit tests share (`stage.test.tsx`, `view-decisions-slot.test.tsx`,
 * `view-address.test.tsx`): the page's stage drawn at an address under the page's own providers,
 * the reads it is drawn from, which slot it drew, and where the focus landed. What the app
 * provides above the page (`AppProviders`) is what the stage's tests, the Decisions body's
 * drawn test (`DecisionsFilterBody.drawn.test.tsx`) and the outline's reads test
 * (`outline-reads.test.tsx`) draw in; the outline's own tests and the Plan view's
 * (`IssueTree.test.tsx`, `PlanFilterBody.test.tsx`) draw that stack by hand. Details' tests
 * (`details-more.test.tsx`) draw what they test under the location the browser makes for an
 * address (`renderAtBrowserEntry`), as the stage's tests do.
 */

/** A navigator that goes nowhere, for a router whose location the case sets itself. */
const NO_NAVIGATION: Navigator = {
  createHref: (to) => (typeof to === "string" ? to : createPath(to)),
  go: () => {},
  push: () => {},
  replace: () => {},
};

/** Whether each slot is filled and the slot each view draws, where a case gives the stage its
 *  own (`StageSlot`'s `filled` and `views`); the page's own where it gives none. */
interface StageViews {
  readonly filled?: SlotsFilled;
  readonly views?: ViewSlots;
}

/** The stage's own slots where the case gives them, the issue it is drawn for, and `held`, whose
 *  `navigate` the stage sets to the call that moves the router. */
interface StageProps extends StageViews {
  readonly held: { navigate?: NavigateFunction };
  readonly issue: IssueDetails;
}

/** The page's stage at the address the router holds, under the page's own providers, as the
 *  page's root draws it (`IssueOutlinePage.tsx`): the route read from the address at every
 *  render, and the slots `filled` and `views` give where the case gives them. `held.navigate`
 *  moves the router, as a press of a view's control does. */
function AddressedStage({ filled, held, issue, views }: StageProps): ReactNode {
  const { pathname, search } = useLocation();
  held.navigate = useNavigate();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <StageSlot filled={filled} issue={issue} route={route} views={views} />
    </ProvidersSlot>
  );
}

/** What the app provides above the page, around `children`: the query client, here `client`,
 *  the keymap, the margin and the names store the page publishes its references' names to, as
 *  the app's shell holds it (`app.tsx`'s `ShellProviders`). Each caller puts its own router
 *  around it. */
export function AppProviders({
  children,
  client,
}: {
  readonly children: ReactNode;
  readonly client: QueryClient;
}): ReactNode {
  return (
    <QueryClientProvider client={client}>
      <KeymapProvider>
        <MarginProvider>
          <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
        </MarginProvider>
      </KeymapProvider>
    </QueryClientProvider>
  );
}

/** The page's stage (`AddressedStage`) under what the app provides above the page
 *  (`AppProviders`). Each helper puts its own router around it. */
function ProvidedStage({
  client,
  ...stage
}: StageProps & { readonly client: QueryClient }): ReactNode {
  return (
    <AppProviders client={client}>
      <AddressedStage {...stage} />
    </AppProviders>
  );
}

/** The page's stage at the issue's address of `preset`, Decisions where none is given, with
 *  `after` after it, a search or a fragment, under the page's own providers, as the page's root
 *  draws it (`IssueOutlinePage.tsx`): the stage's own views, each slot filled as `SLOT_FILLED`
 *  has it. The issue is `issueDetails()` and the client a fresh one where the case gives none,
 *  and `held.navigate` moves the router once the stage is drawn (`AddressedStage`). */
export function drawStage(
  after: string,
  {
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
    held = {},
    issue = issueDetails(),
    preset = "decisions",
  }: {
    client?: QueryClient;
    held?: { navigate?: NavigateFunction };
    issue?: IssueDetails;
    preset?: Preset;
  } = {}
): RenderResult {
  return render(
    <MemoryRouter initialEntries={[`${presetPath(KEY, preset)}${after}`]}>
      <ProvidedStage client={client} held={held} issue={issue} />
    </MemoryRouter>
  );
}

/** Draws `children` at `address` under the location the browser makes for it, as a load does or
 *  a fragment typed in the address bar: its key is "default", the one key the router gives every
 *  entry it did not make itself. `follow` draws them at another address under that same key, as
 *  the browser changes the fragment alone, with no load. */
export function renderAtBrowserEntry(
  address: string,
  children: ReactNode
): RenderResult & { readonly follow: (to: string) => void } {
  const draw = (at: string): ReactNode => (
    <Router location={at} navigator={NO_NAVIGATION}>
      {children}
    </Router>
  );
  const view = render(draw(address));
  return { ...view, follow: (to) => view.rerender(draw(to)) };
}

/** The page's stage at `address` under the location the browser makes for it, as a load does or
 *  a fragment typed in the address bar (`renderAtBrowserEntry`): `follow` draws the stage at
 *  another address under the same key, as the browser changes the fragment alone, with no load.
 *  `stage` gives the stage its own slots (`AddressedStage`). */
export function drawAtBrowserEntry(
  address: string,
  issue: IssueDetails,
  stage: StageViews = {}
): RenderResult & { readonly follow: (to: string) => void } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderAtBrowserEntry(
    address,
    <ProvidedStage {...stage} client={client} held={{}} issue={issue} />
  );
}

/** Which slot the stage drew, by each slot's own mark: a view's body - the Decisions view's, the
 *  Plan view's, the Spec view's or the Verification view's - or a case's stand-in view
 *  (`data-stand-in-view`), or the body that draws the outline or a line in its place. */
export function drawnSlot(view: RenderResult): string {
  const { container } = view;
  if (container.querySelector("[data-issue-body]") !== null) return "the body";
  const drawn = container.querySelector(
    "[data-decisions-body], [data-plan-body], [data-spec-body], [data-verification-body], [data-stand-in-view]"
  );
  return drawn === null ? "nothing" : "the view";
}

/** The node id of the row that holds the focus, or undefined where no row does. */
export function focusedRow(): string | undefined {
  return document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
}

/** Waits for the row `selector` finds in `view` to hold the focus, saying what `what` drew
 *  where it does not, and hands the row back. */
export async function rowTakesFocus(
  view: RenderResult,
  selector: string,
  what: string
): Promise<HTMLElement> {
  return await waitFor(() => {
    const row = view.container.querySelector<HTMLElement>(selector);
    expect(row, `${what} drew no such row; the stage drew ${drawnSlot(view)}`).not.toBeNull();
    expect(
      row,
      `${what} left the focus on ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
    return row as HTMLElement;
  });
}

/** A refusal of a read, as Dispatch answers one it cannot serve now. */
export const REFUSED = new ApiError(503, { code: "UNAVAILABLE", error: "the read is refused" });

/** Waits for the outline, drawn in a view's place, to draw the failed read's line that says
 *  `message` with the focus on its Retry, saying what `what` drew where it does not. */
export async function retryTakesFocus(
  view: RenderResult,
  message: string,
  what: string
): Promise<void> {
  await waitFor(() => {
    const line = [...view.container.querySelectorAll("[data-issue-body] [data-failed-read]")].find(
      (each) => each.textContent?.includes(message)
    );
    expect(
      line,
      `${what} drew no line "${message}"; the stage drew ${drawnSlot(view)}`
    ).toBeDefined();
    expect(
      line?.querySelector(FAILED_READ_RETRY),
      `${what} left the focus on ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
  });
}

/** Waits for the outline's line that the issue holds no comment of the address's id, drawn in
 *  a view's place, to hold the focus, saying what `what` left where it does not. */
export async function goneLineTakesFocus(view: RenderResult, what: string): Promise<void> {
  await waitFor(() =>
    expect(
      view.queryByRole("heading", { name: "Comment not found" }),
      `${what} left the focus on ${focusedRow() ?? document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
    ).toHaveFocus()
  );
}

/** An id that names no comment of the issue, which the comment's own read answers 404 for. */
export const GONE_COMMENT = "5f0c2a9e-7d41-4b8a-9c3e-2e6d1b7a4c90";

/** The comment's own read, answering that no comment has the id it is asked for. */
export function commentGone(): Mock<typeof api.getComment> {
  return spyOn(api, "getComment").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such comment" })
  );
}

/** Every read the page's tree is built from, answered with nothing, and who is reading. */
export function stubReads(): { mockRestore(): void }[] {
  return [
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: KEY,
      requirements: [],
      spec_artifact_id: "",
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: KEY,
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
  ];
}
