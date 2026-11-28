import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, Suspense, useEffect } from "react";
import { useLocation, useParams } from "react-router-dom";

import { holdFirstPaint } from "./firstPaint";

/**
 * The route of a project's Evals tab (`/projects/:key/evals`): its page, drawn under a boundary of
 * its own that shows `fallback` while the page's code loads, and, from the moment the route
 * matches, the views the tab's first render opens (`holdFirstPaint`), so Dispatch reads them while
 * the page's code loads and the tab mounts rather than after (dispatch://INFRA-2918, T33). The
 * boundary is why the hold starts then: a route inside the boundary of the page's own lazy code
 * would commit, and run its effects, only once that code had loaded.
 */
export function EvalsTabRoute({
  children,
  fallback,
}: {
  children: ReactNode;
  fallback: ReactNode;
}): ReactNode {
  const queryClient = useQueryClient();
  const { key = "" } = useParams();
  const { search } = useLocation();
  useEffect(
    () => holdFirstPaint(queryClient, key, new URLSearchParams(search), Date.now()),
    [queryClient, key, search]
  );
  return <Suspense fallback={fallback}>{children}</Suspense>;
}
