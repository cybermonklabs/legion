/** What `two-builds.multiprocess.e2e.ts` walks: every route of the router in `web/src/app.tsx`,
 *  each with the page component it renders, and the lazily loaded pieces that are no route of
 *  their own. A page whose chunk no walked route loads is a chunk the walk never asks the other
 *  build's process for, so `e2e/__tests__/two-builds-routes.test.ts` holds this list to the
 *  router: every `lazy()` page and every `<Route>` of `app.tsx` must be here, and nothing else.
 *
 *  This module holds data only, so the bun test can import it without Playwright. */

/** One `<Route>` of `app.tsx`: its `path` as the router writes it, and the component it renders. */
export interface WalkRoute {
  readonly path: string;
  readonly page: string;
}

export const walkRoutes = [
  { page: "InboxPage", path: "/" },
  { page: "AnswersPage", path: "/answers" },
  { page: "AgentsPage", path: "/agents" },
  { page: "BroadcastsPage", path: "/agents/broadcasts" },
  { page: "BroadcastPage", path: "/agents/broadcasts/:id" },
  // The live route renders nothing of its own: the walk still loads the live view's chunk,
  // which `MountedLiveViews` imports as the address brings that view up.
  { page: "LiveViewRoute", path: "/agents/:sessionId/live" },
  { page: "AgentArtifactPage", path: "/agents/:sessionId/artifacts/:slug" },
  { page: "MachinesPage", path: "/machines" },
  { page: "MachinePage", path: "/machines/:machine" },
  { page: "IssuePage", path: "/issues/:key/*" },
  { page: "ProjectPage", path: "/projects/:key" },
  { page: "ProjectPage", path: "/projects/:key/architecture" },
  { page: "ProjectPage", path: "/projects/:key/issues" },
  { page: "ProjectPage", path: "/projects/:key/documents" },
  { page: "DocumentPage", path: "/projects/:key/documents/:slug" },
  { page: "InboxPage", path: "/credentials/*" },
  { page: "MachineLoginRoute", path: "/credentials/machine" },
  { page: "SettingsPage", path: "/settings" },
  { page: "DeliveryPage", path: "/delivery" },
  { page: "PipelinePage", path: "/pipeline" },
  { page: "NotFoundPage", path: "*" },
] as const satisfies readonly WalkRoute[];

export type WalkRoutePath = (typeof walkRoutes)[number]["path"];

/** The chunks a page loads only once it needs them, each named by what shows it has run. */
export const lazyPieces = {
  "ask-card":
    "the ask card (features/doc/AskBlockCard.tsx): the read-only card the shell gives every " +
    "text body for a decision block in it (features/doc/TextAskCardHost.tsx), and the card a " +
    "document's own decision block draws (features/doc/AskBlockCardHost.tsx), which is what a " +
    "walked document page that holds a decision block loads it as",
  "create-issue":
    "the Create issue dialog (features/issue/CreateIssueDialogHost.tsx), which `c` opens",
  echarts: "the delivery timeline's chart, echarts (features/delivery/Timeline.tsx)",
  editor:
    "the document editor, @legion/proof-editor and its stylesheet (features/doc/editor.ts), " +
    "and the document's Yjs stack: yjs, @hocuspocus/provider, pending-edits and pending-sync " +
    "(features/doc/connection.ts)",
  "headless-markdown":
    "the headless Markdown renderer, @legion/proof-editor/headless, which every page warms at " +
    "boot (features/refs/markdown-engine.ts)",
  inbox:
    "the Inbox list, its cards and the peek drawer (features/inbox/inbox-chunk.ts), which the " +
    "entry starts where the Inbox is the page and the drawer loads anywhere else",
  "issue-tab":
    "an issue tab's panel other than the Spec's (features/issue/tab-chunks.ts), each a chunk " +
    "fetched when its tab first opens",
  "markdown-worker":
    "the Markdown parse worker (features/refs/markdown-parse.worker.ts), which a body longer " +
    "than markdownWorkerThreshold is parsed in rather than on the page's own thread " +
    "(features/refs/markdown-engine.ts)",
  mermaid:
    "the Mermaid renderer's Web Worker (features/refs/mermaid-render.worker.ts), which a body " +
    "holding a fenced `mermaid` block starts (features/refs/mermaid-diagrams.ts)",
  "ref-preview":
    "the reference-preview layer (features/refs/RefPreviewLayer.tsx), which the shell asks for " +
    "in its first effect, after the page it landed on has drawn",
  "search-bar":
    "the search and actions bar (features/search/SearchPaletteHost.tsx), which `$mod+k` opens",
  "shortcut-help":
    "the keyboard-shortcut sheet (features/shell/ShortcutHelpHost.tsx), which `?` opens",
} as const;

export type LazyPiece = keyof typeof lazyPieces;
