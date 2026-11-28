import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, Suspense, useEffect } from "react";
import { Outlet, useLocation, useMatch } from "react-router-dom";

import { holdFirstPaint } from "./firstPaint";

/**
 * The layout route, with no path, over a project's Evals tab's route (`/projects/:key/evals`):
 * the tab's page (its `<Outlet />`), drawn under a boundary of its own that shows `fallback` while
 * the page's code loads, and, from the moment the route matches, the views the tab's first render
 * opens (`holdFirstPaint`), so Dispatch reads them while the page's code loads and the tab mounts
 * rather than after (dispatch://INFRA-2918, T33). The boundary is why the hold starts then: a
 * route inside the boundary of the page's own lazy code would commit, and run its effects, only
 * once that code had loaded; this route draws its boundary below itself, so the page's load
 * suspends that boundary and not this route. Being a layout route keeps the tab's route one
 * `<Route>` of the page alone in app.tsx's router, as every other page's is. A route with no path
 * sees none of its child's params, so this one reads the project's key from the address.
 */
export function EvalsTabRoute({ fallback }: { fallback: ReactNode }): ReactNode {
  const queryClient = useQueryClient();
  const key = useMatch("/projects/:key/evals")?.params.key ?? "";
  const { search } = useLocation();
  useEffect(
    () => holdFirstPaint(queryClient, key, new URLSearchParams(search), Date.now()),
    [queryClient, key, search]
  );
  return (
    <Suspense fallback={fallback}>
      <Outlet />
    </Suspense>
  );
}
