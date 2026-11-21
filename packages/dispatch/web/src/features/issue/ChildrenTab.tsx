import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { StatusPill } from "../../components/Pill";
import { card, linkHoverText, linkText, textSecondaryOnSurface } from "../../theme/classes";
import { PlannedChanges } from "../going/PlannedChanges";
import { statusText } from "../project/board-model";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { GitHubLink } from "./GitHubLink";

/** The issue's children, under the open conversation's planned changes of them and the new
 *  issues it plans under this one (`PlannedChanges`). */
export function ChildrenTab({ issue }: { issue: IssueDetails }): ReactNode {
  const children = issue.children ?? [];
  return (
    <div className="space-y-3">
      <PlannedChanges
        surface={{
          children: children.map((child) => child.key),
          issue: issue.key,
          kind: "children",
        }}
      />
      {children.length === 0 ? (
        <EmptyState label="Children empty state" message="No child issues" />
      ) : (
        <ul className="space-y-2">
          {children.map((child) => (
            <li className={`rounded-lg p-3 ${card}`} key={child.key}>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Link
                  className={`font-medium ${linkText} ${linkHoverText}`}
                  to={buildIssuePath({ key: child.key, kind: "issue" })}
                  {...referenceTriggerProps({ key: child.key, kind: "issue" })}
                >
                  {child.key} · {child.title}
                </Link>
                {/* The same pill the issue header shows, with the same lifecycle label: a reader
                    two inches below `In progress` must not meet `in_progress`. */}
                <StatusPill>{statusText(child.status)}</StatusPill>
              </div>
              <div
                className={`mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm ${textSecondaryOnSurface}`}
              >
                <span>
                  {child.subtree_done}/{child.subtree_total} done
                </span>
                <Timestamp at={child.active_at} />
                {(child.external_links ?? []).map((link) => (
                  <span className="flex items-center" key={link.url}>
                    <GitHubLink link={link} />
                  </span>
                ))}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
