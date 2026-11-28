import { lazy, type ReactNode, Suspense, useId, useState } from "react";

import {
  linkText,
  secondaryButtonCompact,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import type { BrushWindow } from "./evals-url";
import type { TimelineState } from "./OpenTimeline";
import type { EvalsScope } from "./useEvalsViews";

/** The open merge timeline, loaded once the reader opens it (`OpenTimeline`). */
const OpenTimeline = lazy(() =>
  import("./OpenTimeline").then((module) => ({ default: module.OpenTimeline }))
);

/**
 * The tab's merge timeline section (dispatch://INFRA-2918/spec R13, R6, R7; plan D8). Closed on
 * the first paint: it subscribes nothing, holds no row and loads none of the open timeline's code
 * until the reader opens it (`OpenTimeline`, dispatch://INFRA-2918, T33), and closing it lets its
 * view go. While the brush is set, the section says so and clears it, whether open or closed,
 * since the brush sets the target cards' days too.
 */
export function TimelineSection({
  brush,
  onBrush,
  project,
  scope,
  unserved = false,
}: {
  /** The brush the URL holds, which the target cards read too. */
  brush: BrushWindow | null;
  onBrush: (brush: BrushWindow | null) => void;
  project: string;
  scope: EvalsScope;
  /** Whether the server refused one of the tab's views as a frame it cannot read. */
  unserved?: boolean;
}): ReactNode {
  const headingId = useId();
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<TimelineState>("subscribed");
  return (
    <section
      aria-labelledby={headingId}
      className="flex flex-col gap-2"
      data-section="timeline"
      data-state={open ? state : "closed"}
    >
      <div className="flex flex-wrap items-center gap-3">
        <h2 className={`text-[15px] font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
          Merge timeline
        </h2>
        <button
          aria-expanded={open}
          className={secondaryButtonCompact}
          onClick={() => {
            setState("subscribed");
            setOpen(!open);
          }}
          type="button"
        >
          {open ? "Hide the merge timeline" : "Show the merge timeline"}
        </button>
        {brush === null ? null : (
          <span className={`text-xs ${textMutedOnCanvas}`}>
            {`Brushed ${new Date(brush.start).toLocaleString()} to ${new Date(brush.end).toLocaleString()}`}
            <button
              className={`ml-3 underline ${linkText}`}
              onClick={() => onBrush(null)}
              type="button"
            >
              clear brush window
            </button>
          </span>
        )}
      </div>
      {open ? (
        <Suspense
          fallback={
            <p className={`text-sm ${textMutedOnCanvas}`} role="status">
              Merge timeline: loading…
            </p>
          }
        >
          <OpenTimeline
            brush={brush}
            onBrush={onBrush}
            onState={setState}
            project={project}
            scope={scope}
            unserved={unserved}
          />
        </Suspense>
      ) : null}
    </section>
  );
}
