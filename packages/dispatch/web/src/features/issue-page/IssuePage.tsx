import type { ReactNode } from "react";

import { IssuePage as TodaysIssuePage } from "../issue/IssuePage";
import { lazyChunk } from "../shell/chunk-loads";
import { useEndHoldWhen } from "../shell/content-drawn";
import { useIssuePageChoice } from "./choice";

const IssueOutlinePage = lazyChunk(() =>
  import("./IssueOutlinePage").then((module) => ({ default: module.IssueOutlinePage }))
);

/**
 * The page an issue's address draws: the new page for a reader who chose it, else today's.
 *
 * `app.tsx` loads this file with `lazyChunk`, and the build names a lazy chunk after the module
 * that `import()` names, so this file is `IssuePage.tsx` and exports `IssuePage`: the chunk keeps
 * the name `IssuePage-<hash>.js`, and the browser rows that abort or hold `/assets/IssuePage-*.js`
 * keep their meaning. Today's page is imported statically, so that chunk holds it as it does
 * now; the new page is loaded with `lazyChunk`, so a reader with the setting off downloads
 * nothing of it, and the app's `Suspense` shows its fallback while that chunk loads: the frame's
 * skeleton where the frame's chunk is in (`frame-shell.tsx`), else today's (`IssuePageFallback`
 * in `app.tsx`). A reader waits on that chunk, so its download goes through `renderChunk` as
 * every route's does (`features/shell/chunk-loads.ts`), and React's own `lazy` is refused here
 * by `biome.json`.
 */
export function IssuePage(): ReactNode {
  const outline = useIssuePageChoice()[0] === "outline";
  // A landing on an issue's Spec, Verification or Plan, or on an artifact its address names,
  // holds the shell's extras - the margin, the reference preview, the event map, the Create issue
  // dialog's chunk and the pending-writes host's two chunks - until its live document draws
  // (`app.tsx`, `preloadRouteChunk` and `issueDrawsDocument`), and today's page ends that hold
  // when its document draws or its reads say it draws none. The new page draws no live document,
  // so it ends the hold here; else the reader's extras would wait for `HOLD_LIMIT_MS` or their
  // first key or click (`features/shell/content-drawn.ts`).
  useEndHoldWhen(outline);
  return outline ? <IssueOutlinePage /> : <TodaysIssuePage />;
}
