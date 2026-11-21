import type { ReactNode } from "react";

import type { GoingBlockData } from "../../../api/types";
import { textSecondaryOnSurface } from "../../../theme/classes";
import { AgentStatusBlock } from "./AgentStatusBlock";
import { ChartBlock } from "./ChartBlock";
import { IssueListBlock } from "./IssueListBlock";
import { MetricBlock } from "./MetricBlock";
import { ProgressBlock } from "./ProgressBlock";
import { TableBlock } from "./TableBlock";
import { TimelineBlock } from "./TimelineBlock";

/** The drawing of one block, by its kind; a kind outside the seven is no block this page draws. */
function blockBody(block: GoingBlockData): ReactNode {
  switch (block.kind) {
    case "chart":
      return <ChartBlock block={block} />;
    case "progress":
      return <ProgressBlock block={block} />;
    case "table":
      return <TableBlock block={block} />;
    case "issue_list":
      return <IssueListBlock block={block} />;
    case "agent_status":
      return <AgentStatusBlock block={block} />;
    case "metric":
      return <MetricBlock block={block} />;
    case "timeline":
      return <TimelineBlock block={block} />;
    default:
      return null;
  }
}

/**
 * The one typed block an answer draws beside its text, from the block frame alone: its title, and
 * its rows drawn as its kind draws them. Every number it shows is a value of those rows, which the
 * server's own read returned; the spec names fields and never a number. A frame of a kind this
 * page does not know draws nothing, and the answer stays prose and links.
 */
export function AnswerBlock({ block }: { readonly block: GoingBlockData }): ReactNode {
  const body = blockBody(block);
  if (body === null) {
    return null;
  }
  return (
    <figure className="mt-3" data-kind={block.kind} data-testid="going-block">
      {block.title === "" ? null : (
        <figcaption className={`mb-1 text-xs font-medium ${textSecondaryOnSurface}`}>
          {block.title}
        </figcaption>
      )}
      {body}
    </figure>
  );
}
