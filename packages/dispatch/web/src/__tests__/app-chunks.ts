/**
 * Every module the app loads lazily beyond a route's own page, loaded with the test file that
 * imports this one.
 *
 * The app keeps these out of the entry chunk and out of each page's first commit
 * (dispatch://INFRA-341, R11), and mounts each beside or inside whatever route is open:
 * - the shell's parts: the margin (`features/margin/MarginColumn.tsx`); the search bar, the
 *   reference preview's host and the agent registry's poller once the route is drawn, and the
 *   Inbox drawer and the create-issue dialog when the reader opens them
 *   (`features/shell/deferred-shell.ts`); and the Inbox page at `/` and in the drawer
 *   (`features/inbox/InboxList.tsx`);
 * - the issue page's parts: its header, tab strip, requirements and document
 *   (`features/issue/IssuePage.tsx`), each tab's panel (`features/issue/issue-tab-list.ts`) and
 *   an artifact's details (`features/artifacts/ArtifactRoutePanel.tsx`);
 * - the decision card every Markdown body draws (`features/doc/DeferredAskCard.tsx`).
 *
 * A row that renders the whole app meets each load wherever the load happens to end, whether or
 * not the row reads that part, and a load that ends between two `act` scopes of the row is
 * React's report of a suspended resource outside `act`, which the preload's act check fails the
 * row on. Loaded here, before the file's rows begin, no row meets any of them, and the cold-load
 * lane (`hold-lazy-loads.ts`) holds none of them.
 *
 * A part whose module exports the loader the app asks for it with is loaded through that
 * loader: the Inbox's list (`importInbox`) and each tab's panel (`issueTabChunks`), so a tab
 * added to `issueTabChunks` is loaded here with no change to this file. The parts whose loader
 * no module exports, each loaded inside a `lazy()`, are imported by name below. The loaders are
 * awaited at this module's top level, and a test file's rows begin only once every module it
 * imports has run to its end, top-level awaits included: no row starts before the loads end.
 *
 * So a test module that renders the app's shell (`AuthGate`, `App`, `AppShell`) imports this
 * one, and a part the app starts loading lazily is added here: through its loader where its
 * module exports one, and by name where it does not. A route's own page is not: the file that
 * opens that route and reads its page waits for the page or imports it.
 */
import "../features/artifacts/ArtifactDetails";
import "../features/artifacts/ArtifactDocument";
import "../features/doc/AskBlockCard";
import "../features/inbox/InboxDrawer";
import "../features/issue/CreateIssueDialog";
import "../features/issue/IssueHeader";
import "../features/issue/IssueTabs";
import "../features/issue/SpecRequirements";
import "../features/margin/Margin";
import "../features/refs/RefPreview";
import "../features/refs/agent-registry";
import "../features/search/SearchPalette";
import { importInbox } from "../features/inbox/InboxList";
import { issueTabChunks } from "../features/issue/issue-tab-list";

await Promise.all([importInbox, ...Object.values(issueTabChunks)].map((load) => load()));
