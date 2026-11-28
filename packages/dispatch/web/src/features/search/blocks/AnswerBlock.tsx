import type { ReactNode } from "react";

import type { GoingBlockData } from "../../../api/types";
import { textSecondaryOnSurface } from "../../../theme/classes";
import { AgentStatusBlock } from "./AgentStatusBlock";
import { clockWords } from "./block-cells";
import { ChartBlock } from "./ChartBlock";
import { IssueListBlock } from "./IssueListBlock";
import { MetricBlock } from "./MetricBlock";
import { ProgressBlock } from "./ProgressBlock";
import { TableBlock } from "./TableBlock";
import { TimelineBlock } from "./TimelineBlock";

/** The drawing of one block, by its kind; a kind outside the seven is no block this page draws.
 *  A table shows its first rows and "Show all" (`onShowAll`), or every row where it is `whole`. */
function blockBody(
  block: GoingBlockData,
  onShowAll: (() => void) | undefined,
  whole: boolean
): ReactNode {
  switch (block.kind) {
    case "chart":
      return <ChartBlock block={block} />;
    case "progress":
      return <ProgressBlock block={block} />;
    case "table":
      return <TableBlock block={block} onShowAll={onShowAll} whole={whole} />;
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

/** A block's caption (design v2, state 5): what it counts, its title, and when, "DEMO issues by
 *  status · as of 16:40", where `asOf` is given; "" for neither. */
export function blockCaption(title: string, asOf: number | undefined): string {
  if (asOf === undefined) return title;
  const when = `as of ${clockWords(asOf)}`;
  return title === "" ? `${when.charAt(0).toUpperCase()}${when.slice(1)}` : `${title} · ${when}`;
}

/**
 * The one typed block an answer draws beside its text, from the block frame alone, as a card as
 * wide as the answer (design v2, state 5): its caption - its title and, where `asOf` is given,
 * the time its read was made - and its rows drawn as its kind draws them. Every number it shows is
 * a value of those rows, which the server's own read returned; the spec names fields and never a
 * number. A frame of a kind this page does not know draws nothing, and the answer stays prose and
 * links. A table past its first rows offers "Show all" (`onShowAll`); `whole` draws it whole.
 */
export function AnswerBlock({
  asOf,
  block,
  onShowAll,
  whole = false,
}: {
  readonly asOf?: number;
  readonly block: GoingBlockData;
  readonly onShowAll?: () => void;
  readonly whole?: boolean;
}): ReactNode {
  const body = blockBody(block, onShowAll, whole);
  if (body === null) {
    return null;
  }
  const caption = blockCaption(block.title, asOf);
  return (
    <figure className="mt-3 w-full" data-kind={block.kind} data-testid="going-block">
      {caption === "" ? null : (
        <figcaption
          className={`mb-1 text-xs font-medium ${textSecondaryOnSurface}`}
          data-testid="going-block-caption"
        >
          {caption}
        </figcaption>
      )}
      {body}
    </figure>
  );
}
