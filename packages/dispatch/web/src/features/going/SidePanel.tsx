import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { whoAmIQuery } from "../../api/queries";
import type { GoingContextInput } from "../../api/types";
import { inputMethodKey } from "../../lib/input-method-key";
import { issueProject } from "../../lib/issue-key";
import { borderDefault, card, dangerText } from "../../theme/classes";
import { nodeRouteFromHref, routeProjectOf } from "../refs/routes";
import { useFollowInPlace } from "../refs/useFollowInPlace";
import { issueOfPage, scopeOfPage } from "../search/bar-scope";
import { ConversationView } from "./ConversationView";
import {
  asksPerson,
  type ClickedItem,
  clearPanelFocus,
  conversationWorking,
  movePanel,
  newConversation,
  setClicked,
  setPlace,
  setWide,
  useGoing,
  useSendQueued,
} from "./going-store";
import { History } from "./History";
import { usePlanCardKeys } from "./PlanRows";
import {
  BoxFrame,
  BoxRow,
  boxHint,
  boxInputClassName,
  boxInputStyle,
  boxWords,
  ConversationHeader,
  focusConversationBox,
} from "./RunControls";
import { useAsk } from "./useAsk";

/** The side panel's element id. */
const SIDE_PANEL_ID = "going-side-panel";

/** The page's own region, which the shell draws (`app.tsx`): Esc in the panel puts the focus
 *  there. */
const PAGE_ID = "main-content";

/** The item a click on the page was on: the issue or document a link or a reference in it names,
 *  as the context of a request names it; undefined for a click on nothing of the kind. */
function clickedItem(target: EventTarget | null): GoingContextInput | undefined {
  if (!(target instanceof Element)) {
    return undefined;
  }
  const holder = target.closest<HTMLElement>("[data-dispatch-ref], a[href]");
  const reference =
    holder?.getAttribute("data-dispatch-ref") ?? holder?.getAttribute("href") ?? undefined;
  if (reference === undefined) {
    return undefined;
  }
  const route = nodeRouteFromHref(reference);
  if (route === undefined) {
    return undefined;
  }
  if ("key" in route) {
    return { issue: route.key, project: issueProject(route.key) };
  }
  return "project" in route ? { project: route.project } : undefined;
}

/** The context of the next request at the side: the page the panel sits beside, and the item a
 *  click on that page named (`clicked`, while the person is still on the page it was clicked on),
 *  which takes the place of the page's own issue and project: a click on a project's document
 *  names no issue. */
function contextOf(
  pathname: string,
  search: string,
  clicked: ClickedItem | undefined
): GoingContextInput {
  const page: GoingContextInput = {
    block: "",
    issue: issueOfPage(pathname, search) ?? "",
    path: pathname,
    project: routeProjectOf(pathname) ?? "",
  };
  return clicked === undefined ? page : { ...page, issue: "", ...clicked.context };
}

/** What the context chip in the panel's box says (design v3, state 10): the item a click on the
 *  page named - its key, or the project of a click that names no issue - else what the page puts
 *  under the bar, as the bar's chip says it (`scopeOfPage`), and "this page" on a page of no
 *  project. */
function chipWords(pathname: string, search: string, clicked: ClickedItem | undefined): string {
  const named = clicked?.context;
  if (named?.issue !== undefined && named.issue !== "") return named.issue;
  if (named?.project !== undefined && named.project !== "") return `the project ${named.project}`;
  return scopeOfPage(pathname, search, undefined, []) ?? "this page";
}

/**
 * The conversation kept at the side of the page (R1; design v3, state 10): a panel 420 px wide at
 * the right, which the page narrows beside (`app.tsx`) and stays usable, and which stays open on
 * every navigation. It has the header, the conversation and the box of the bar (state 2), and no
 * search list; its header has "Open over the page", which moves it back, and its × closes it. Its
 * box asks with the page as the context, and a click on the page that opens or selects an item
 * makes that item the chip and the context of the next request. The box is the bar's: the mode
 * control, the context chip inside the box's frame, Stop while the run works, the plan card's
 * keys while a plan waits, and words sent during a run held until it ends. A link in the panel,
 * such as an item's chip in an answer, opens its item in the page with the router, and the panel
 * stays, with the conversation, and its box names the item the page then shows as the context of
 * the next request. Esc in the panel puts the focus on the page and the panel stays, except that
 * Esc in its empty box while a run works stops the run (R10); Ctrl-K brings the conversation
 * back over the page.
 *
 * Where the shell says how far the navigation reaches (`wideInset`, the left edge of the app's
 * content area, set from the width at which the navigation stands beside the page), the header
 * also holds a button that widens the panel to that content area (A6) and docks it back to its
 * side width. It is the same panel either way: one element, one conversation, one box, so the run
 * and the words not yet sent stay where they are, and only the conversation scrolls, in a column
 * of its own width while the panel is wide. When the bar hands a run over (`handOffToPanel`), the
 * panel takes the focus in its box once the bar's dialog has given the focus back.
 */
export function SidePanel({ wideInset }: { readonly wideInset?: string }): ReactNode {
  const going = useGoing();
  const location = useLocation();
  const login = useQuery(whoAmIQuery()).data?.login;
  const [words, setWords] = useState("");
  const [history, setHistory] = useState(false);
  const panelRef = useRef<HTMLElement>(null);
  const side = going.place === "side";
  const wide = going.wide && wideInset !== undefined;
  const clicked = going.clicked?.path === location.pathname ? going.clicked : undefined;
  const context = contextOf(location.pathname, location.search, clicked);
  const chip = chipWords(location.pathname, location.search, clicked);
  const asking = useAsk(context);
  const cardKeys = usePlanCardKeys();
  // A link in the panel - an item's chip in an answer, a row of the plan - opens its item in the
  // page with the router, and the panel stays (R1): a load of the whole page would drop it.
  const follow = useFollowInPlace();
  // Words sent while the run worked go once it no longer works, from the panel while it shows the
  // conversation (the bar sends them while it is over the page).
  useSendQueued((held) => side && asking.enter(held));

  // The bar handed a run over: the box takes the focus. This is a passive effect, which React runs
  // after the passive cleanup of the bar's dialog in the same commit, and that cleanup gives the
  // focus back to whatever opened the bar (`useDialog`), so the box's focus is the last word.
  useEffect(() => {
    if (!side || !going.panelFocus) {
      return;
    }
    focusConversationBox();
    clearPanelFocus();
  }, [going.panelFocus, side]);

  // A click on the page, outside the panel, works as usual, and the item it was on is the
  // context of the next request.
  useEffect(() => {
    if (!side) {
      return;
    }
    const onClick = (event: MouseEvent) => {
      if (panelRef.current?.contains(event.target as Node) === true) {
        return;
      }
      const item = clickedItem(event.target);
      movePanel({ kind: "page_click" }, login);
      if (item !== undefined) {
        setClicked(item, window.location.pathname);
      }
    };
    document.addEventListener("click", onClick, true);
    return () => document.removeEventListener("click", onClick, true);
  }, [login, side]);

  if (!side) {
    return null;
  }
  // What the box says it does now (design v2, rule 5).
  const placeholder = boxWords({
    asked: asksPerson(going),
    asking,
    open: going.conversation !== undefined || going.starting !== undefined,
    search: false,
  });
  return (
    // The panel clips what overflows it (`overflow-clip`), so no script, focus or scroll into
    // view can scroll its header and its box out of place: only the conversation scrolls
    // (review B1). Widened, it stands from the edge of the navigation to the window's right edge
    // (`left`), so the navigation stays reachable beside it.
    <aside
      aria-label="Command bar at the side"
      className={`fixed top-0 right-0 bottom-0 z-40 flex flex-col overflow-clip border-l shadow-2xl ${wide ? "w-auto" : "w-full sm:w-[420px]"} ${card} ${borderDefault}`}
      data-testid="going-side-panel"
      data-wide={wide ? "true" : "false"}
      id={SIDE_PANEL_ID}
      onClick={follow}
      // Esc in the panel puts the focus on the page, and the panel stays. An Esc the panel's own
      // parts answered (the plan card, the mode menu, a run it stopped) is theirs.
      onKeyDown={(event) => {
        if (event.key !== "Escape" || event.defaultPrevented) return;
        event.preventDefault();
        document.getElementById(PAGE_ID)?.focus();
      }}
      ref={panelRef}
      style={wide ? { left: wideInset } : undefined}
    >
      <ConversationHeader
        history={history}
        move="Open over the page"
        onClose={() => setPlace("closed")}
        onHistory={() => setHistory((shown) => !shown)}
        onMove={() => setPlace("over")}
        onNew={() => {
          newConversation();
          setHistory(false);
        }}
        size={wideInset === undefined ? undefined : { onToggle: () => setWide(!wide), wide }}
      />
      {history ? (
        <div className="min-h-0 flex-1 overflow-y-auto">
          <History onOpen={() => setHistory(false)} />
        </div>
      ) : (
        // The one element that stays as the panel widens and docks, so the conversation inside it
        // is neither drawn again nor scrolled back to its foot; only its column's width changes.
        <div
          className={`mx-auto flex min-h-0 w-full flex-1 flex-col ${wide ? "max-w-3xl" : ""}`}
          data-testid="going-panel-column"
        >
          <ConversationView
            answering={asking.answering}
            onContinue={asking.onContinue}
            onRetry={asking.retry}
          />
        </div>
      )}
      {/* While History shows, its filter is the one box on the screen (design v3, state 11);
          the panel's box stays in place, hidden, so its words and its focus come back with it. */}
      <div className={`shrink-0 border-t p-3 ${history ? "hidden" : ""} ${borderDefault}`}>
        <div className={`mx-auto w-full ${wide ? "max-w-3xl" : ""}`}>
          <BoxRow
            box={
              <BoxFrame chip={chip} chipTitle={chip}>
                <input
                  aria-label="Ask the command bar"
                  className={boxInputClassName}
                  onChange={(event) => setWords(event.target.value)}
                  onKeyDown={(event) => {
                    // A key of an input method's composition is the input method's
                    // (`inputMethodKey`): the Enter that confirms a conversion sends nothing.
                    if (inputMethodKey(event.nativeEvent) || cardKeys(event)) {
                      return;
                    }
                    if (event.key === "Enter" && !event.shiftKey) {
                      if (asking.enter(words)) {
                        event.preventDefault();
                        setWords("");
                      }
                    } else if (event.key === "Escape" && words === "" && conversationWorking()) {
                      // Esc in the empty box while the run works stops the run (R10), and the
                      // focus stays in the box.
                      event.preventDefault();
                      event.stopPropagation();
                      asking.escape(words);
                    }
                  }}
                  placeholder={placeholder}
                  style={boxInputStyle(placeholder)}
                  type="text"
                  value={words}
                />
              </BoxFrame>
            }
            hint={boxHint(asking, words)}
            onStop={asking.stop}
          />
          {going.refusal === undefined ? null : (
            <p className={`mt-2 text-xs ${dangerText}`} role="alert">
              {going.refusal}
            </p>
          )}
        </div>
      </div>
    </aside>
  );
}
