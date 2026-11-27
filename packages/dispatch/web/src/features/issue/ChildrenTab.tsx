import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { EmptyState } from "../../components/EmptyState";
import { StatusPill } from "../../components/Pill";
import { card, linkHoverText, linkText, textSecondaryOnSurface } from "../../theme/classes";
import { statusText } from "../project/board-model";
import { referenceTriggerProps } from "../refs/ref-preview-state";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { GitHubLink } from "./GitHubLink";

/**
 * The issue's children, one row each. A row's groups, in this order: the child (its key and title,
 * which wrap inside the link), its state (the status pill and how much of its subtree is done, one
 * group that does not split), when it was last active, and its links (at most 10, then "+N").
 * When a line is too narrow, a group moves to the next line whole.
 */
export function ChildrenTab({ issue }: { issue: IssueDetails }): ReactNode {
  if ((issue.children ?? []).length === 0) {
    return <EmptyState label="Children empty state" message="No child issues" />;
  }

  return (
    <ul className="space-y-2">
      {(issue.children ?? []).map((child) => (
        <li className={`rounded-lg p-3 ${card}`} key={child.key}>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <Link
              className={`min-w-0 font-medium [overflow-wrap:anywhere] ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: child.key, kind: "issue" })}
              {...referenceTriggerProps({ key: child.key, kind: "issue" })}
            >
              {child.key} · {child.title}
            </Link>
            <span
              className={`flex shrink-0 items-center gap-x-3 text-sm whitespace-nowrap ${textSecondaryOnSurface}`}
            >
              <StatusPill>{statusText(child.status)}</StatusPill>
              <span>
                {child.subtree_done}/{child.subtree_total} done
              </span>
            </span>
          </div>
          <div
            className={`mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm ${textSecondaryOnSurface}`}
          >
            <Timestamp at={child.active_at} />
            {(child.external_links ?? []).length === 0 ? null : (
              <CappedGroup
                className="flex min-w-0 max-w-full flex-wrap items-center gap-x-3 gap-y-1"
                label="links"
                name="child-links"
              >
                {(child.external_links ?? []).map((link) => (
                  <span className="flex min-w-0 max-w-full items-center" key={link.url}>
                    <GitHubLink link={link} />
                  </span>
                ))}
              </CappedGroup>
            )}
          </div>
        </li>
      ))}
    </ul>
  );
}
