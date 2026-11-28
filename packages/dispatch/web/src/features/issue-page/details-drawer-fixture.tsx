import { QueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { Margin } from "../margin/Margin";
import { MarginProvider } from "../margin/margin-context";
import { issue } from "../margin/margin-fixture";
import { useDrawerPage } from "./drawer-page";
import { type MarginDrawer, type MarginDrawerPage, MarginSlotProvider } from "./margin-slot";

// What `details-drawer-focus.test.tsx` and `details-focus-keeper.test.tsx` both draw the new
// issue page's Details with, so a change to the drawer's shape (`MarginDrawer`,
// `MarginDrawerPage`) is made here once.

/** What the margin reads of the margin fixture's issue, answered before it asks. */
export function detailsDrawerClient(): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  return queryClient;
}

/** Registers the new issue page's `MarginDrawerPage` on the shell's drawer (`useDrawerPage`). */
function NewPageDrawerPage(): null {
  useDrawerPage(issue);
  return null;
}

/** The shell as it draws the new issue page's frame (`app.tsx`'s `ShellProviders`): the margin's
 *  providers, with Details as the margin's drawer, closed on the load and holding the new page's
 *  `MarginDrawerPage`; `children`, what the case draws on the page; and the margin itself. */
export function DetailsDrawerShell({ children }: { children: ReactNode }): ReactNode {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const drawer = useMemo<MarginDrawer>(() => ({ open, page, setOpen, setPage }), [open, page]);
  return (
    <MarginProvider>
      <MarginSlotProvider drawer={drawer} drawsSlot>
        <NewPageDrawerPage />
        {children}
        <Margin />
      </MarginSlotProvider>
    </MarginProvider>
  );
}
