// What the Evals tab says where the server it reached does not serve its views
// (dispatch://INFRA-2918/artifact/plan-md, T41, ledger item 55): a server of the release before
// this page's knows no view kind of the tab's (`evals`, `items`, `legion-read`,
// `project-timeline`) and refuses each subscription of one as a frame it cannot read
// (INVALID_FRAME). The page then closes its socket and reconnects (`createLiveSocket`), which drops
// the refusals of the tab's other views, so a refusal of one of them stands for every view of the
// tab that has not answered. A view a later process answers paints as ever.
import { useQueries } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ApiError } from "../../api/client";
import { type EvalKind, evalsViewQuery, legionReadViewQuery } from "../../api/queries";
import { textMutedOnCanvas } from "../../theme/classes";
import type { EvalsScope } from "./useEvalsViews";

/** Whether `error` is the server refusing one of the tab's views as a frame it cannot read. */
export function isUnservedView(error: unknown): boolean {
  return error instanceof ApiError && error.code === "INVALID_FRAME";
}

/** Whether the server refused one of the tab's first views so: the evals views of `kinds`, and the
 *  header's daemon read where the project has a daemon (`daemon`). The tab renders again only when
 *  that answer changes. */
export function useUnservedViews(
  scope: EvalsScope,
  kinds: readonly EvalKind[],
  daemon: boolean
): boolean {
  return useQueries({
    queries: [
      ...kinds.map((kind) => evalsViewQuery({ ...scope, kind, offset: 0 })),
      ...(daemon ? [legionReadViewQuery(scope.project)] : []),
    ],
    combine: (results) => results.some((result) => isUnservedView(result.error)),
  });
}

/** The line a part of the tab (`what`) shows in place of its numbers while the server does not
 *  serve the tab's views. */
export function UnservedViews({ what }: { what: string }): ReactNode {
  return (
    <p className={`text-sm ${textMutedOnCanvas}`} data-unserved role="status">
      {`${what}: the server does not serve the Evals tab's views yet.`}
    </p>
  );
}
