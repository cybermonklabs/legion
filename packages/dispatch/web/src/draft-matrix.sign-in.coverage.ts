/**
 * The draft matrix's coverage list, sign-in (INFRA-1503): each composer census item that
 * `draft-matrix.sign-in.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry } from "./__tests__/composer-census/coverage";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. Run under each client: TanStack's own, which the other app-shell rows use, and `DispatchQueryClient`, built as `main.tsx` builds it. While the check holds no answer the first joins a check still out, and the second waits for it and asks again once it lands, so a page that only invalidates draws alice under the first, and alice and then bob under the second.",
  2: '2. `app-sign-out.test.tsx`, "a composer\'s typed text is not shown to another person signed in from another tab", is this cell, and it is not repeated here.',
  3: "3. Signed out (`signedOut`), the page takes the shell and every composer away, which `app-sign-out.test.tsx` holds to the same rule as (2).",
  4: "4. In `AppShell`, above every host, the layout draws the shell around the page: the compact header with its Menu and the Inbox's Peek (whose drawer is the Inbox files' reason), the navigation drawer and the focus it keeps (`useDialog`), the sidebar or its rail, the main column's classes, and the keyboard fit of a page that fills the screen (`useKeyboardFit`). Each draws or moves something beside the route element and the margin a host is in, never them, so no composer moves, and the rows above make their moves at the test page's width.",
  5: "5. On the new issue page, once its reader turns it on (`issueFrame`: the frame-shell chunk hands `AppShell` its `frame`), the shell draws the frame's icon rail beside the page where the width allows it (`FrameShell`'s `rail`, on `railFits`) in place of the sidebar and its rail, its navigation sheet over the page at every width (the rail's foot control and the header's `☰` open it, the header's where the rail does not fit: `ShellProviders`' `navigationShown`), and no compact header of its own, and it draws the navigation's contents compact there (`NavigationContents`' `compact`). Each draws or moves something beside the route element and the margin a host is in, as (4) says, so no composer moves.",
  6: "6. Which shell is drawn is the person's choice of issue page (`ChosenShell`, which reads `useStoredIssuePageChoice` for the login it is handed): for `tabs` today's `AppShell` inside the margin's provider, else the frame-shell chunk's `FrameShell`, which draws the same `AppShell` inside the new issue page's providers (`WithFrameProviders`; only that chunk hands in `providers`, so which of its two returns draws is fixed for the life of the shell). The choice changes in Settings, or in another tab of this browser (its `storage` event), and the other shell is then drawn in place of the one before: every composer in it ends its mount, and each composer the new shell draws opens on what the draft stores keep for its address, since the stores outlive the shell (`AuthGate`), as they do when the lapse row's sign-in draws the shell again. The rows above draw today's shell, and none changes the choice.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "AuthGate | AuthenticatedApp | conditional | (shellFor === whoAmI.data.login) ? …",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    },
    { row: "the same person's sign-in from another tab keeps the shell and the words typed in it" },
    {
      row: "after a reload, a composer whose reads the query client holds as the shell is drawn opens on the draft the tab kept",
    },
    {
      row: "a lapse, then the same person's sign-in, opens the composer on the words typed before the lapse",
    },
    {
      row: "a sign-out, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts",
    },
    {
      row: "a sign-out in another tab, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts",
    },
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
  ...answers(
    "AuthGate | AuthenticatedApp | early return | if (signedOut) return …",
    {
      row: "a lapse, then the same person's sign-in, opens the composer on the words typed before the lapse",
    },
    {
      row: "a sign-out, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts",
    },
    {
      row: "a sign-out in another tab, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts",
    },
    { reason: REASONS[3] }
  ),
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
  ...answers(
    "AuthGate | AuthenticatedApp | address prop | user={whoAmI.data}",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    },
    { row: "the same person's sign-in from another tab keeps the shell and the words typed in it" },
    { reason: REASONS[6] }
  ),
  ...answers(
    "AuthenticatedApp | ChosenShell | address prop | user={user}",
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (QueryClient)",
    },
    {
      row: "an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)",
    },
    { row: "the same person's sign-in from another tab keeps the shell and the words typed in it" },
    { reason: REASONS[6] }
  ),
  ...answers(
    'ChosenShell | AppShell | conditional | if (useStoredIssuePageChoice(user.login) === "tabs")',
    { reason: REASONS[6] }
  ),
  ...answers(
    'ChosenShell | AppShell | early return | if (useStoredIssuePageChoice(user.login) === "tabs") return …',
    { reason: REASONS[6] }
  ),
  ...answers("WithFrameProviders | InboxDrawerHost | conditional | if (providers === undefined)", {
    reason: REASONS[6],
  }),
  ...answers(
    "WithFrameProviders | InboxDrawerHost | early return | if (providers === undefined) return …",
    { reason: REASONS[6] }
  ),
  ...answers("WithFrameProviders | MarginColumn | conditional | if (providers === undefined)", {
    reason: REASONS[6],
  }),
  ...answers(
    "WithFrameProviders | MarginColumn | early return | if (providers === undefined) return …",
    { reason: REASONS[6] }
  ),
  ...answers("AppShell | InboxDrawerHost | form | <div className>", { reason: REASONS[4] }),
  ...answers("AppShell | MarginColumn | form | <div className>", { reason: REASONS[4] }),
  ...answers("AppShell | - | form | (isCompactViewport && !issueFrame) ? …", {
    reason: REASONS[4],
  }),
  ...answers("AppShell | - | form | ((isCompactViewport || issueFrame) && navigationOpen) ? …", {
    reason: REASONS[4],
  }),
  ...answers(
    "useKeyboardFit | - | form | if (!enabled || element === null || viewport == null) return …",
    { reason: REASONS[4] }
  ),
  ...answers("useDialog | - | form | (open) ? …", { reason: REASONS[4] }),
  ...answers("useDialog | - | form | if (!open) return …", { reason: REASONS[4] }),
  ...answers("ShellProviders | - | form | (navigationShown) ? …", { reason: REASONS[5] }),
  ...answers("AppShell | - | form | isCompactViewport || …", { reason: REASONS[5] }),
  ...answers("FrameShell | - | form | (railFits) ? …", { reason: REASONS[5] }),
  ...answers("AppShell | - | form | (isCompactViewport) ? …", { reason: REASONS[5] }),
];
