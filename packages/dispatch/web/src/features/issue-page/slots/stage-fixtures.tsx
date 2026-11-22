import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render } from "@testing-library/react";
import type { ReactNode } from "react";
import {
  createPath,
  type NavigateFunction,
  type Navigator,
  Router,
  useLocation,
  useNavigate,
} from "react-router-dom";

import type { IssueDetails } from "../../../api/types";
import { MarginProvider } from "../../margin/margin-context";
import { parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { ProvidersSlot } from "./providers";
import { type SlotsFilled, StageSlot, type ViewSlots } from "./stage";

/**
 * What the stage's unit tests share (`stage.test.tsx`, `view-decisions-slot.test.tsx`): the
 * page's stage drawn at an address under the page's own providers, and which slot it drew.
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

/** The page's stage at the address the router holds, under the page's own providers, as the
 *  page's root draws it (`IssueOutlinePage.tsx`): the route read from the address at every
 *  render, and the slots `filled` and `views` give where the case gives them. `held.navigate`
 *  moves the router, as a press of a view's control does. */
export function AddressedStage({
  filled,
  held,
  issue,
  views,
}: StageViews & {
  readonly held: { navigate?: NavigateFunction };
  readonly issue: IssueDetails;
}): ReactNode {
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

/** The page's stage at `address` under the location the browser makes for it, as a load does or
 *  a fragment typed in the address bar: its key is "default", the one key the router gives every
 *  entry it did not make itself. `follow` draws the stage at another address under that same key,
 *  as the browser changes the fragment alone, with no load. `stage` gives the stage its own slots
 *  (`AddressedStage`). */
export function drawAtBrowserEntry(
  address: string,
  issue: IssueDetails,
  stage: StageViews = {}
): RenderResult & { readonly follow: (to: string) => void } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const draw = (at: string): ReactNode => (
    <Router location={at} navigator={NO_NAVIGATION}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <AddressedStage {...stage} held={{}} issue={issue} />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </Router>
  );
  const view = render(draw(address));
  return { ...view, follow: (to) => view.rerender(draw(to)) };
}

/** Which slot the stage drew, by each slot's own mark: a view's body - the Decisions view's, the
 *  Plan view's or the Verification view's - or a case's stand-in view (`data-stand-in-view`), or
 *  the body that draws the outline or a line in its place. */
export function drawnSlot(view: RenderResult): string {
  const { container } = view;
  if (container.querySelector("[data-issue-body]") !== null) return "the body";
  const drawn = container.querySelector(
    "[data-decisions-body], [data-plan-body], [data-verification-body], [data-stand-in-view]"
  );
  return drawn === null ? "nothing" : "the view";
}
