import type { ReactNode } from "react";

import type { IssueDetails } from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  pageStateText,
  pageWaitingChip,
  textMutedOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionOfAddress } from "../refs/routes";

// Inside `NoTooltips` every marker here is the new issue page's warning, the waiting orange of
// what needs a person's eye: a route that reaches nobody is the page's waiting chip, and a reach
// nobody could judge or a holder that stopped answering is its words in that colour, as the
// page's margin says such a state. There a route a marker names is a value, so it breaks
// anywhere rather than ellipsize behind a tooltip that page does not draw; everywhere else it
// ellipsizes, as on `main`.

type RoutedIssue = Pick<
  IssueDetails,
  "route" | "route_status" | "route_holder" | "route_status_reason"
>;

/** The `Route reach unknown` marker for a route the Envoy listener could not judge
 *  (`route_status: unknown`), shown in the issue header and on an ask's route row
 *  (`AskRecipients`), with a title that says why. It claims neither that the route reaches
 *  someone nor that nobody holds it; the caller decides when to show it. `className` adds the
 *  call site's layout classes and `testId` its test id: the header's marker carries
 *  `issue-route-unknown` and an ask card's none, since the issue page shows both. */
export function RouteReachUnknownMarker({
  className,
  testId,
}: {
  className?: string;
  testId?: string;
}): ReactNode {
  // The reason is a tooltip on a surface that offers one, and nowhere inside `NoTooltips`
  // (`components/tooltips.tsx`, the new issue page), where the words are the whole reading.
  const tooltips = useTooltips();
  return (
    <span
      className={`${className === undefined ? "" : `${className} `}text-xs ${tooltips ? textMutedOnSurface : pageStateText.waiting}`}
      data-testid={testId}
      title={
        tooltips
          ? "The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route."
          : undefined
      }
    >
      Route reach unknown
    </span>
  );
}

/**
 * The warning an issue carries wherever it is shown - the issue header, a List row, a Board
 * card - when its route reaches nobody: `route_status` is `no_holder`, meaning a role nobody
 * running holds, or a session that is not running, as of the server's copy of the listener's
 * session list, at most 5 s old. The server resolves the status on every issue read, so this
 * names it and judges nothing. A route that reaches a live session, an
 * unrouted issue, and a route the listener could not judge (`unknown`) render nothing here, so
 * healthy routes stay quiet and a listener restart does not mark every card.
 *
 * `showRoute` names the route in the label, for the header, where the marker sits in the state
 * row rather than beside the route: a role as its address, a session by `AgentName`. A name can
 * be long, so only the route ellipsizes and the verdict stays whole, the rule `ClaimChip`
 * follows for a holder's name.
 */
export function UnreachableRouteMarker({
  issue,
  showRoute = false,
  testId = "issue-route-unreachable",
}: {
  issue: RoutedIssue;
  showRoute?: boolean;
  /** The marker's test id: the header's is the one every check reads, and a second reading of
   *  the same route on a page carries its own, so the two are never one ambiguous name. */
  testId?: string;
}): ReactNode {
  if (issue.route === null || issue.route_status !== "no_holder") {
    return null;
  }
  return <UnreachableRoute route={issue.route} showRoute={showRoute} testId={testId} />;
}

/**
 * The marker for a route whose session - the routed session, or the role's holder - has missed
 * its heartbeats without ending (`route_status: holder_unresponsive`). It is neither of the
 * other two: the route does reach that session, because the listener stores what is sent while
 * it is silent, so this is not the warning `UnreachableRouteMarker` paints, and it is not the
 * `unknown` of a listener that could not judge the route either - the listener judged it and
 * said the session is stalled. It therefore shares the warning chip's shape, to sit in the same
 * row, in the muted tone a state that needs no action wears (dispatch://INFRA-392).
 */
export function UnresponsiveRouteMarker({
  issue,
  showRoute = false,
}: {
  issue: RoutedIssue;
  showRoute?: boolean;
}): ReactNode {
  const text = useAgentText();
  const tooltips = useTooltips();
  if (issue.route === null || issue.route_status !== "holder_unresponsive") {
    return null;
  }
  const holder = issue.route_holder;
  // The words name the session as `AgentName` draws it on this surface.
  const named =
    holder === null || holder === undefined ? "That session" : text.sessionShown(holder, undefined);
  const explanation = `${named} has missed its heartbeats without ending, so it still holds this route: what is sent to this issue is stored for it and waits until it answers. Nothing has to be done about it unless it stays this way - a session past the listener's gone horizon leaves the role unheld, and the route then reads as reaching nobody.`;
  return (
    <span
      className={`inline-flex min-w-0 max-w-full items-center gap-1 text-xs ${tooltips ? textMutedOnSurface : pageStateText.waiting}`}
      data-testid="issue-route-unresponsive"
      style={{ flexShrink: showRoute ? 1 : 0 }}
      title={tooltips ? explanation : undefined}
    >
      {showRoute ? (
        <span className={tooltips ? "min-w-0 truncate" : "min-w-0 [overflow-wrap:anywhere]"}>
          {issue.route}:
        </span>
      ) : null}
      <span className="shrink-0">Unresponsive, still reached</span>
      <span className="sr-only">. {explanation}</span>
    </span>
  );
}

function UnreachableRoute({
  route,
  showRoute,
  testId,
}: {
  route: string;
  showRoute: boolean;
  testId: string;
}): ReactNode {
  const text = useAgentText();
  const tooltips = useTooltips();
  const sessionId = sessionOfAddress(route);
  // The words name the session as `AgentName` draws it on this surface.
  const explanation = `${sessionId === undefined ? `No running session holds ${route} right now` : `${text.sessionShown(sessionId, undefined)} is not running right now`}, so this issue's messages reach nobody at the moment. A session that is restarting or moving between boxes drops out of the listener for minutes and comes back under the same id, so this is a vacancy only when it is still true on a read ten minutes later; then staff the role, re-route it to a live holder, or clear the route and assign it.`;
  const verdict = sessionId === undefined ? "Nobody holds it right now" : "Not running right now";
  return (
    <span
      className={`inline-flex min-w-0 max-w-full items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold ${tooltips ? `${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}` : pageWaitingChip}`}
      data-testid={testId}
      style={{ flexShrink: showRoute ? 1 : 0 }}
      title={tooltips ? explanation : undefined}
    >
      <span aria-hidden="true" className="shrink-0">
        ⚠
      </span>
      {showRoute ? (
        <span className={tooltips ? "min-w-0 truncate" : "min-w-0 [overflow-wrap:anywhere]"}>
          {sessionId === undefined ? (
            route
          ) : (
            <AgentName inline sessionId={sessionId} storedTitle={undefined} />
          )}
          :
        </span>
      ) : null}
      {/* Beside a route the route is what ellipsizes and the verdict stays whole. Alone, the
          verdict gives way inside the chip when a narrow cell or card holds less than all of
          it, rather than the chip being cut at the cell's edge: it ends in an ellipsis under
          the chip's own title, which says it whole, and inside `NoTooltips`, which draws no
          title, it wraps. */}
      <span
        className={
          showRoute
            ? "shrink-0"
            : tooltips
              ? "min-w-0 truncate"
              : "min-w-0 [overflow-wrap:anywhere]"
        }
      >
        {verdict}
      </span>
      <span className="sr-only">. {explanation}</span>
    </span>
  );
}
