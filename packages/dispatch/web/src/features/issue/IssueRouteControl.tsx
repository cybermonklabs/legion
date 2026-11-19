import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import {
  focusVisibleRing,
  surfaceMutedStrongBg,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionOfAddress } from "../refs/routes";
import { authorText } from "./page-structure";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "./RouteReach";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * Where a message on this issue goes, and whether it reaches anyone, in its row of Details
 * (dispatch://INFRA-1196 R13): the route as the control that opens its editor - a `session:`
 * route named as `AgentName` names an agent, with the agent's own live link beside it, a role
 * route as its address - and its reach in words. The header keeps one marker of its own, where
 * the route reaches nobody; every other reading of the route is here.
 */
export function RouteControl({
  editing,
  isClosed,
  issue,
  onEdit,
  writes,
}: {
  /** Whether the route's editor is open under it, which hides the control that opens it. */
  editing: boolean;
  isClosed: boolean;
  issue: IssueDetails;
  onEdit: () => void;
  writes: IssueHeaderWrites;
}): ReactNode {
  const agentText = useAgentText();
  const { drafts } = writes;
  const routeSession = sessionOfAddress(drafts.route);
  const routeLabel = `Messages default to ${
    drafts.route === ""
      ? "no route"
      : routeSession === undefined
        ? drafts.route
        : agentText.sessionText(routeSession, undefined)
  }`;
  const routeControl = (name: ReactNode) => (
    <button
      aria-label={routeLabel}
      className={`inline-flex min-h-11 max-w-full min-w-0 items-center truncate rounded-full px-3 py-1 text-left text-sm outline-none focus-visible:ring-2 md:min-h-7 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
      disabled={isClosed}
      onClick={onEdit}
      type="button"
      {...authorText}
    >
      <TruncatedText>{name}</TruncatedText>
    </button>
  );
  return (
    <>
      {editing ? null : routeSession === undefined ? (
        routeControl(drafts.route === "" ? "No route" : drafts.route)
      ) : (
        <AgentName
          className="inline-flex min-w-0 items-center"
          control={routeControl}
          kind="control"
          sessionId={routeSession}
          storedTitle={undefined}
        />
      )}
      {/* Whether the route reaches anyone speaks for the saved route only, so a route being
          saved hides it until the next read judges it. */}
      {drafts.route === (issue.route ?? "") ? (
        <>
          <UnreachableRouteMarker issue={issue} testId="issue-details-route-unreachable" />
          <UnresponsiveRouteMarker issue={issue} />
          {issue.route !== null && issue.route_status === "unknown" ? (
            issue.route_status_reason === "refilling" ? (
              // A listener refilling its agent list has not failed, and says so rather than
              // reading as a listener that did not answer.
              <span
                className={`shrink-0 text-xs ${textMutedOnSurface}`}
                data-testid="issue-route-refilling"
              >
                Route reach unknown: {AGENT_LIST_REFILLING}
              </span>
            ) : (
              <RouteReachUnknownMarker className="shrink-0" testId="issue-route-unknown" />
            )
          ) : null}
        </>
      ) : null}
    </>
  );
}
