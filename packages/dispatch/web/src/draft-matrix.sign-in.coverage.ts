/**
 * The draft matrix's coverage list, sign-in (INFRA-1503): each composer census item that
 * `draft-matrix.sign-in.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons of its header, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry } from "./__tests__/composer-census/coverage";

/** The numbered reasons of the matrix file's header. */
const REASONS = {
  1: "1. Run under each client: TanStack's own, which the other app-shell rows use, and `DispatchQueryClient`, built as `main.tsx` builds it. While the check holds no answer the first joins a check still out, and the second waits for it and asks again once it lands, so a page that only invalidates draws alice under the first, and alice and then bob under the second.",
  2: '2. `app-sign-out.test.tsx`, "a composer\'s typed text is not shown to another person signed in from another tab", is this cell, and it is not repeated here.',
  3: "3. Signed out (`signedOut`), the page takes the shell and every composer away, which `app-sign-out.test.tsx` holds to the same rule as (2).",
  4: "4. In `AppShell`, above every host, the layout draws the shell around the page: the compact header with its Menu and the Inbox's Peek (whose drawer is the Inbox files' reason), the navigation drawer and the focus it keeps (`useDialog`), the sidebar or its rail, the main column's classes, and the keyboard fit of a page that fills the screen (`useKeyboardFit`). Each draws or moves something beside the route element and the margin a host is in, never them, so no composer moves, and the rows above make their moves at the test page's width.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "AuthGate | AuthenticatedApp | conditional | (shellFor === undefined || shellFor === whoAmI.data.login) ? …",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    },
    { row: "the same person's sign-in from another tab keeps the shell and the words typed in it" },
    { reason: REASONS[1] },
    { reason: REASONS[2] }
  ),
  ...answers(
    "AuthGate | AuthenticatedApp | early return | if (whoAmI.isPending) return …",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    }
  ),
  ...answers("AuthGate | AuthenticatedApp | early return | if (signedOut) return …", {
    reason: REASONS[3],
  }),
  ...answers(
    "AuthGate | AuthenticatedApp | conditional | if (whoAmI.data !== undefined)",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    },
    { row: "the same person's sign-in from another tab keeps the shell and the words typed in it" }
  ),
  ...answers("AppShell | InboxDrawer | form | <div className>", { reason: REASONS[4] }),
  ...answers("AppShell | - | form | (isCompactViewport) ? …", { reason: REASONS[4] }),
  ...answers("AppShell | - | form | (isCompactViewport && navigationOpen) ? …", {
    reason: REASONS[4],
  }),
  ...answers(
    "useKeyboardFit | - | form | if (!enabled || element === null || viewport == null) return …",
    { reason: REASONS[4] }
  ),
  ...answers("useDialog | - | form | (open) ? …", { reason: REASONS[4] }),
  ...answers("useDialog | - | form | if (!open) return …", { reason: REASONS[4] }),
];
