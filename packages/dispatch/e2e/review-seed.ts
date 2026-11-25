/** The one corpus both review layers open `e2e/review-routes.ts`'s pages against: the accessibility
 *  pass and the containerised visual review read the same rows, so a finding of one is a finding
 *  about the page the other shot.
 *
 *  It builds on the fixtures that already exist rather than writing a second corpus:
 *  `seedWorkspace` fills the Inbox, the Agents page, a broadcast, the answers, the projects and
 *  the issue tree, and the delivery fixtures fill `/delivery` and `/pipeline`. What it adds is
 *  the four documents the issue tabs need, the priorities the two priority pages are read
 *  against - `seedWorkspace` writes none, and both of those pages draw a sentence instead of a
 *  row for a deployment that has written no priority - the work under them and beside them,
 *  shaped as production's (`WORK`), two sessions that hold some of it, named as production's
 *  claim holders often are (`claimHolderSessions`), the issue header of a working issue: its
 *  whose-turn chip, two labels and a message route someone receives, and the issues the visual
 *  lane's command-bar rows read (`seedBarIssues`), which no reviewed page draws. */
import type { IssueStatus } from "@legion/contracts/dispatch-tools";
import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createIssue,
  createIssueArtifact,
  createPlanReview,
  createPriority,
  getArtifact,
  getBroadcast,
  getIssue,
  getProjectArtifact,
  patchIssue,
  putAgentState,
  putDocumentRead,
} from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineRows,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { blockId } from "./editor";
import { awaitFact } from "./local-instance-support";
import { sql } from "./psql";
import type { BarSeed, ReviewSeed, SeededReadMarks } from "./review-routes";
import { sessionIds } from "./session-ids";
import { sessionIds } from "./session-ids";
import { asWorkspaceSession, reviewSessions, seedWorkspace, workspaceSessions } from "./workspace";

/** The decision `decisions.md` records, which the Decisions tab lists under the reader's review
 *  and the Spec tab draws beside the block it shaped. */
const DECISION = "Seed one corpus for both review layers";
const REASON = "Chosen so a scan and a shot read the same page.";

/** The person `/people/:login` is opened on: the login `seedWorkspace()` makes every human
 *  write of this corpus as (`e2e/workspace.ts`'s `alice`, which is also `e2e/api.ts`'s default
 *  author), and the login both layers sign in as (`withAlice`, `e2e/users.ts`). Her page is
 *  therefore the comments she wrote here, drawn as rows rather than as the empty state. */
const PERSON = "alice";

/** A `serves` pointer is refused unless it names a `requirement` block that exists, and
 *  `seedWorkspace`'s spec carries none. Rather than edit `e2e/workspace.ts`, whose spec every
 *  other spec of the suite reads, the requirements live in a document of this seed's own, which
 *  is also the second, non-primary issue artifact the `issue-artifact` row opens: one document
 *  serves both needs, and the issue keeps the one spec `seedWorkspace` wrote. A pointer may name
 *  a block of another document of the same issue's tree, which is what these two are. */
const REQUIREMENTS = `# What the review layers open

:::requirement{#r1}
Every route of the router is reviewed, or excluded with a reason.
:::

:::requirement{#r2}
A page is read once it is drawn, never as a loading skeleton.
:::
`;

/** The machine `/machines/:machine` is opened on: the one the first seeded session reports.
 *  Read from the registry rather than written here, so a rename in `e2e/workspace.ts` moves
 *  that page's URL with it, and a registry that carries none fails by name rather than opening
 *  `/machines/undefined`, which draws a page saying it knows no such machine. */
function firstMachine(): string {
  const machine = workspaceSessions[0]?.machine_id;
  if (machine === undefined) {
    throw new Error("seedReviewRoutes: no seeded session names a machine");
  }
  return machine;
}

/** A session of the corpus's registry (`reviewSessions`) that holds work here: three of the four
 *  workspace sessions that stay live however long a run takes, and the two claim holders beside
 *  them (`claimHolderSessions`), one named by a title led by an emoji and one by its bare id. The
 *  Observer holds none, so the Agents page keeps one session under Idle, which a claim would move
 *  under Active; nor does the Archivist, since its claim, as it goes quiet, would read as free on
 *  a later run. */
type Holder = "planner" | "tester" | "reviewer" | "cacheWarm" | "untitled";

/** One issue this corpus adds to `seedWorkspace`'s: its project and title, the status it stands
 *  in, the priority whose work it is by that priority's place in `priorities` (from 0), or none,
 *  and the session that claimed it, where one did. */
interface Work {
  readonly project: "CORE" | "OPS";
  readonly title: string;
  readonly status: Exclude<IssueStatus, "done">;
  readonly under: number | null;
  readonly holder?: Holder;
}

/** The work this corpus adds beside `seedWorkspace`'s issues, in the order it is created, shaped
 *  as production's is: issue titles there run from 67 to 194 characters, about 120 at the
 *  median, with a few of up to 1,000, and most of the work in flight is held by a session (38 of
 *  the 59 issues active under no priority). The text is invented and names no one real.
 *
 *  - Active under no priority: five, beside the broadcast issue `seedWorkspace` writes (whose
 *    row the `priorities-in-flight` route waits for), with titles of 120 to 190 characters and
 *    one of 756, each held by a session: one by each of the three workspace sessions that hold
 *    work and one by each claim holder (`Holder`), so that tab lists six rows, five of them
 *    claimed.
 *  - The first priority's work: eleven, beside the workflow issue, its child and the folds
 *    issue `seedWorkspace` writes, with titles of 85 to 190 characters and one of 736, in every
 *    open status, and three held by sessions, so its own page lists fourteen rows. One of the
 *    eleven stands in the icebox.
 *  - The rest of the order's work: one or two under each of the other priorities, so every row
 *    of the Priorities tab counts some, and several count two or more of a kind. One stands in
 *    the backlog, held by the claim holder whose title is led by an emoji, so the tab of priority
 *    work in backlog or icebox lists three rows with the folds issue and the icebox one, one of
 *    them claimed.
 *
 *  The two longest are created last, so every issue before them keeps the key it had before
 *  them, which the priorities' descriptions name. */
const WORK = {
  composerOrder: {
    holder: "planner",
    project: "CORE",
    status: "in_progress",
    title:
      "The broadcast composer keeps each recipient's place in the send order when the list it " +
      "was picked from refreshes under the sender",
    under: null,
  },
  tokenRotation: {
    holder: "tester",
    project: "OPS",
    status: "testing",
    title:
      "Rotating the agent token on a build host restarts no session that holds a claim, and a " +
      "session that loses its connection during the rotation reconnects on its own",
    under: null,
  },
  railCount: {
    holder: "reviewer",
    project: "CORE",
    status: "needs_review",
    title:
      "The Inbox count on the rail settles within a second of a reply being read on another " +
      "device, and never counts one reply twice while the read that clears it is still in " +
      "flight on a slow link",
    under: null,
  },
  heartbeats: {
    holder: "untitled",
    project: "OPS",
    status: "retro",
    title:
      "A machine that misses three heartbeats in a row is listed as unresponsive rather than " +
      "gone, with the moment it was last seen and the issue it was working on then",
    under: null,
  },
  runOnPush: {
    holder: "reviewer",
    project: "CORE",
    status: "in_progress",
    title:
      "Each change starts its verification run the moment it is pushed, before any reviewer is " +
      "asked to read it",
    under: 0,
  },
  runRecord: {
    holder: "planner",
    project: "CORE",
    status: "in_progress",
    title:
      "The record a verification run leaves beside a change names the commit it read, the " +
      "command it ran and the line of the change each failed check points at",
    under: 0,
  },
  reviewerSees: {
    project: "CORE",
    status: "testing",
    title:
      "A reviewer opening a change sees whether its checks ran, passed and read its newest " +
      "commit, without opening a log",
    under: 0,
  },
  queueHolds: {
    holder: "tester",
    project: "OPS",
    status: "needs_review",
    title:
      "The review queue holds back a change whose checks have not finished, and lets it through " +
      "the moment they all pass",
    under: 0,
  },
  readPast: {
    project: "CORE",
    status: "todo",
    title:
      "A check that failed on a change that is right can be read past with a reason, and the " +
      "reason stays beside that check for the next reader of the same change",
    under: 0,
  },
  retireSweep: {
    project: "OPS",
    status: "todo",
    title:
      "Retire the nightly verification sweep once every change runs its own checks before review",
    under: 0,
  },
  forcePush: {
    project: "CORE",
    status: "triage",
    title:
      "The verification record of a change survives a force-push, and says which of its results " +
      "were read from a commit that is no longer the head of the change",
    under: 0,
  },
  touchedRows: {
    project: "CORE",
    status: "retro",
    title:
      "The browser suite runs only the rows that read the pages a change touches, and says which " +
      "rows it left out and why",
    under: 0,
  },
  runResumes: {
    project: "OPS",
    status: "in_progress",
    title:
      "A verification run still going when its build host is replaced resumes on the next host " +
      "from the last check it finished, rather than starting again from the first check on an " +
      "empty host",
    under: 0,
  },
  recordInMargin: {
    project: "CORE",
    status: "icebox",
    title:
      "Show a change's verification record in the margin of its pull request, beside the lines " +
      "each of its checks read",
    under: 0,
  },
  docsCommands: {
    project: "OPS",
    status: "todo",
    title: "The docs build fails when a page names a command the command-line tool no longer has",
    under: 1,
  },
  releaseCompare: {
    project: "OPS",
    status: "in_progress",
    title:
      "The release run compares its checks with the last release's and stops on any reading " +
      "that moved past its threshold",
    under: 2,
  },
  releaseStopped: {
    project: "OPS",
    status: "testing",
    title:
      "A release that stopped says which reading it missed, on the release's own page, in one line",
    under: 2,
  },
  notesLine: {
    project: "CORE",
    status: "needs_review",
    title: "Each change carries a line for the release notes, or says why its readers need none",
    under: 3,
  },
  buildProgress: {
    project: "OPS",
    status: "in_progress",
    title: "A build writes what it has done so far where the next host can read it",
    under: 4,
  },
  drainWaits: {
    project: "OPS",
    status: "todo",
    title:
      "The drain of a host waits for its builds to write their progress before the host is returned",
    under: 4,
  },
  oneRecord: {
    project: "CORE",
    status: "in_progress",
    title:
      "Every check writes its result to the one record a change keeps, with the commit it read",
    under: 5,
  },
  foldReports: {
    holder: "cacheWarm",
    project: "CORE",
    status: "backlog",
    title:
      "Fold the unit suites' and the browser suites' reports into the one record a change keeps, " +
      "and retire the two pages that drew them",
    under: 5,
  },
  searchMisses: {
    project: "CORE",
    status: "todo",
    title:
      "The hundred searches that opened no page last month each open the page that answers them",
    under: 6,
  },
  flakyList: {
    project: "CORE",
    status: "in_progress",
    title:
      "The suites record which rows failed on which commit, and list a row that failed and then " +
      "passed on one commit as flaky",
    under: 7,
  },
  flakyWeekly: {
    project: "CORE",
    status: "retro",
    title:
      "The weekly read of the flaky list fixes the row at its top and writes down what made it flaky",
    under: 7,
  },
  patchRun: {
    project: "OPS",
    status: "testing",
    title:
      "A patch run builds an image the day a security patch is released and replaces the hosts " +
      "one at a time",
    under: 8,
  },
  setupCommand: {
    project: "CORE",
    status: "todo",
    title:
      "The one command that starts the stack on a new machine loads the sample data and opens " +
      "the tour",
    under: 9,
  },
  costPage: {
    project: "OPS",
    status: "in_progress",
    title:
      "A daily page shows what the build fleet and the checks spent on each merged change, beside " +
      "the week before",
    under: 10,
  },
  typedKept: {
    project: "CORE",
    status: "needs_review",
    title:
      "What a person types into a session is kept where the next host can read it, as they type it",
    under: 11,
  },
  loadRun: {
    project: "OPS",
    status: "triage",
    title:
      "A load run opens each page the way a reader does at ten times today's traffic and lists " +
      "the pages slower than a second",
    under: 12,
  },
  handOver: {
    holder: "cacheWarm",
    project: "OPS",
    status: "in_progress",
    title:
      "A build host drained for replacement while a verification run still holds it waits for " +
      "the run to write the last check it finished, then hands the run to the next host with its " +
      "record, its cache key and the list of checks it has still to run, and the next host picks " +
      "the run up from that list rather than from the first check; the run's own page says which " +
      "host read which checks, how long the hand-over took and whether any check was read twice, " +
      "a run that could not be handed over says why in one line at the top of that page, beside a " +
      "link to the drained host's own log, and the fleet's dashboard counts the runs handed over " +
      "each day beside the runs that started again from nothing, so the people who replace hosts " +
      "can see what a replacement still costs",
    under: null,
  },
  runSummary: {
    project: "OPS",
    status: "testing",
    title:
      "A change opened for review shows, above its first line, one summary of every check its " +
      "verification run made: which checks passed, which failed and on which line of the change, " +
      "which were read past and the reason a person gave for each, which were read from a commit " +
      "that is no longer the head of the change, and how long the whole run took against the " +
      "median of the last week's runs; a reviewer who opens the summary reaches each check's own " +
      "record in one click, a reviewer who does not still sees in its first line whether the " +
      "change is ready to read, and a change whose run is still going says which check it is on " +
      "and how many are left, rather than drawing an empty box that a reviewer could take for a " +
      "run that found nothing to say",
    under: 0,
  },
} satisfies Record<string, Work>;

type WorkName = keyof typeof WORK;

/** The priorities this corpus writes, in the order they are written, which is the one order: a
 *  priority takes the last place of it as it is written (`POST /api/v1/priorities`). They are
 *  shaped as production's thirteen are, so the shots read the pages a reader reads there: titles
 *  of 12 to 94 characters, a mix of short and long; owners of 5 to 43 characters, a person's
 *  login, a controller, a list of controllers and "None named"; and a description on every one,
 *  of 400 to 1,900 characters, some of several paragraphs. Four name an issue of `WORK` by its
 *  Dispatch reference in their first line, by `keys`, and three open on a short line with more
 *  paragraphs after it, as production's do: the Priorities tab's closed rows read each as its
 *  start, the paragraphs run together and the reference read as its short form. The text is
 *  invented and names no one real. The Priorities tab lists all of them, the tab of priority
 *  work in backlog or icebox names the first after its chip, and the first's own page is the
 *  `priority` route.
 *
 *  The first is the one `/priorities/:id` opens. It is written as the signed-in person rather
 *  than over the agent bearer, as every one is, because the margin of that page draws who wrote
 *  the priority and a bearer would name a session there instead. Its description holds a link,
 *  so the accessibility pass reads a link inside the rendered description a writer may open into
 *  its editor: a control wrapping that text would put the link inside it (axe's
 *  `nested-interactive`). It names no issue by reference, which that page would draw as a chip
 *  whose title arrives after the page has drawn. */
const priorities = (keys: Readonly<Record<WorkName, string>>) =>
  [
    {
      description:
        "Every change that reaches the build fleet is verified before a person is asked to read " +
        "it, so a reviewer opens work that already builds, passes its checks and says what it " +
        "proves.\n\n" +
        "The work under it is the verification run each change starts, the record it leaves " +
        "beside the change, and the pages both review layers read, from " +
        "[the priorities page](/priorities). It is finished when no reviewer reads a change whose " +
        "checks have not run, and when a check that failed says which line of the change it read.",
      owner: "Verify's, Fleet's and Release's controllers",
      title: "Every change that reaches the build fleet is verified before a person reads it",
    },
    {
      description:
        "The docs site says what the product does today, in the words a new reader looks for. " +
        "Each page names the version it was checked against, and a page nobody has checked in a " +
        "quarter says so at its top rather than reading as current. The work is a pass over every " +
        "page, a check that fails the docs build when a page names a command that no longer " +
        "exists, and a short list of the pages readers open most, which are read first.",
      owner: "alice",
      title: "Docs refresh",
    },
    {
      description:
        `A release leaves the build fleet when its checks are green, as dispatch://${keys.releaseCompare} ` +
        "begins to make it, and nobody waits on a person to press a button for it.\n\n" +
        "Today a release waits for someone to read the run, compare it with the last one and " +
        "start the rollout by hand. Most of that reading is the same every time: the same checks, " +
        "the same thresholds, the same three dashboards. The work under this priority writes " +
        "those readings down as checks the release run makes itself, so a release that meets " +
        "them goes out and one that misses them stops and says which reading it missed.\n\n" +
        "It is not finished while any release needs a person to start it. It is finished when the " +
        "last ten releases went out on their own, and the ones that stopped stopped for a reason " +
        "a person agreed with on reading it. A release that stopped is started again by the " +
        "change that fixes what it missed, never by a person pressing the button anyway, so the " +
        "readings stay the only way out of the fleet.",
      owner: "The platform controller",
      title:
        "A release leaves the build fleet on its own once its checks pass, and nobody waits to start it",
    },
    {
      description:
        "The release notes are written as the work lands rather than on the day of the release. " +
        "Each change that a reader of the notes would notice carries a line for them, written by " +
        "whoever made the change, in the words a reader uses. The release gathers those lines, " +
        "orders them by what a reader does first, and leaves out what no reader sees.\n\n" +
        "It is finished when a release's notes are ready the moment its run is green, and nobody " +
        "spends the release day reading a month of changes to find out what went into it. A " +
        "change with no line for its readers says why it needs none.",
      owner: "None named",
      title: "Release notes",
    },
    {
      description:
        `The first step is dispatch://${keys.buildProgress}. The build fleet runs on hosts that ` +
        "are replaced as a matter of course: an image is " +
        "built, a host takes it, and the host before it is drained and returned. Today a host " +
        "that is drained in the middle of a build loses the build, and the build starts again " +
        "from the beginning on another host.\n\n" +
        "The work under this priority makes a build survive its host. A build writes what it has " +
        "done so far where the next host can read it, the drain waits for that write, and the " +
        "next host starts from it rather than from nothing. It is finished when a week of host " +
        "replacements costs no build more than the minute its new host takes to start, and the " +
        "fleet's dashboard shows no build lost to a drain.",
      owner: "Release's and Docs' controllers",
      title: "A build survives the replacement of the host it runs on",
    },
    {
      description:
        "One record of verification per change.\n\n" +
        "Verification reads every change the same way, whoever made it and however large it is, " +
        "and says in one place what it found.\n\n" +
        "Today there are four places a change is checked: the unit suites, the browser suites, " +
        "the review of its plan and the reading of its result. Each reports on its own page, in " +
        "its own words, and a reader who wants to know whether a change is ready opens all four " +
        "and puts the answer together themselves. Two of them can disagree, and when they do " +
        "the reader has no way to tell which one read the newer state of the change.\n\n" +
        "The work under this priority gives a change one record of its verification. Each check " +
        "writes its result there, with the commit it read, the command it ran and what it found, " +
        "and the record says which results are current and which were read from an older commit. " +
        "A change is ready when every check it names has a current result and each result is a " +
        "pass, and the record says so in those words.\n\n" +
        "It also covers what happens when a check is wrong. A check that fails on a change that " +
        "is right is a defect of the check, and the record keeps the reason a person gave for " +
        "reading past it, so the next reader sees the same reason rather than the same failure " +
        "with nothing beside it. A check that passes on a change that is wrong is found by the " +
        "change that fixes it, and that change names the check, so the record of the check shows " +
        "it once let a defect through.\n\n" +
        "It is finished when a reader answers whether a change is ready from one page, in under " +
        "a minute, without opening a log, and when the four places a change was checked before " +
        "no longer report anything the record does not already hold. The pages those four places " +
        "drew stay readable for the changes they recorded, and each one says, at its top, where " +
        "the record of a newer change is read instead, so a reader who follows an old link is " +
        "sent to the place that answers.",
      owner: "The build fleet's owner",
      title: "Verification reads every change in the same way",
    },
    {
      description:
        "A search over the docs site finds the page a reader means, by the words they use for " +
        "it rather than the words the page was written in. The work is a list of the hundred " +
        "searches readers made last month that opened no page, each with the page that should " +
        "have answered it, and a change to the search that makes each of them open that page. " +
        "It is finished when that list, read again a month later, holds fewer than ten searches.",
      owner: "The docs controller",
      title: "Docs search finds the page a reader means",
    },
    {
      description:
        `Start with dispatch://${keys.flakyList}.\n\n` +
        "The test suites that fail on a change and then pass on the same change, with nothing " +
        "changed between the two runs, are found and fixed rather than run again until they pass.\n\n" +
        "Each run of the suites records which rows failed and on which commit. A row that fails " +
        "and then passes on one commit is a flaky row, and it is listed with the runs it failed " +
        "in. The work under this priority reads that list every week, fixes the row at the top of " +
        "it, and writes down what made it flaky, so the next row that fails for the same reason " +
        "is fixed the same way.",
      owner: "Verification's controller",
      title: "A failing test row is fixed, never run again",
    },
    {
      description:
        "Every host of the build fleet takes a security patch within a day of its release. " +
        "Today a patch waits for the next image, which is built weekly, so a host can run for " +
        "six days with a patch it does not have. The work is a patch run that builds an image " +
        "the day a patch is released and replaces the hosts with it, one at a time, without " +
        "losing a build; and a page that lists each host with the newest patch it runs.\n\n" +
        "It is finished when that page shows no host more than a day behind, for a month.",
      owner: "The fleet controller",
      title: "Patch within a day",
    },
    {
      description:
        "Nobody asks anybody for anything.\n\n" +
        "A person who opens the product for the first time reaches a working setup in one " +
        "sitting, without asking anybody for anything.\n\n" +
        "The work under it is the first page a new reader sees, the one command that starts " +
        "the stack on their machine, the sample data that command loads, and the short tour " +
        "that walks them from that data to their own. Each step says what it is waiting on " +
        "while it waits, and a step that fails says what to do next in the words of the page " +
        "it failed on. It is finished when five people who have never used the product each " +
        "reach their own first page in under half an hour, reading nothing but what the product " +
        "shows them on the way.",
      owner: "Docs and support",
      title: "A one-sitting setup",
    },
    {
      description:
        "The cost of a merged change is read every day, from what the build fleet and the " +
        "checks spent on it, and the reading falls from one week to the next.\n\n" +
        "The work under it is the reading itself, which today is put together by hand once a " +
        "month; a page that shows it each day with the week before it; and the changes that make " +
        "it fall, chosen from what the reading shows is most of the cost. The first of those is " +
        "the browser suite, which every change runs in full although most changes touch none of " +
        "the pages it reads. A change that touches no page runs the rows that read the pages it " +
        "touches and no other.",
      owner: "The eval controller",
      title: "The cost of a merged change falls each week, as the daily reading measures it",
    },
    {
      description:
        `dispatch://${keys.typedKept} keeps what a person types. A session that stops, because ` +
        "its host was replaced or its connection dropped, resumes where it stopped, with nothing a " +
        "person typed into it lost.\n\n" +
        "Today a stopped session starts again from its last saved turn, and whatever the person " +
        "wrote after that turn is gone. Most of the time that is a sentence or two; once a week " +
        "or so it is a long message written over several minutes, and the person writes it " +
        "again from memory. The work under this priority keeps what a person types where the " +
        "session's next host can read it, as they type it, and the next host offers it back to " +
        "them the moment the session resumes.\n\n" +
        "It is finished when a month of host replacements and dropped connections loses no word " +
        "anybody typed, and when the page that resumes a session says it did so in one line " +
        "rather than leaving the person to notice that nothing changed.",
      owner: "None named",
      title: "Sessions resume where they stopped, with nothing a person typed into them lost",
    },
    {
      description:
        "Every page a reader opens answers in under a second, at the load the product is " +
        "heading to rather than the load it has today.\n\n" +
        "The work under this priority is a load run that opens each page the way a reader does, " +
        "at ten times today's traffic, and records how long each one took to answer; a list of " +
        "the pages that took longer than a second, slowest first; and a change for each page on " +
        "that list that brings it under. The run is part of every release, so a change that " +
        "makes a page slower stops the release that carries it and names the page.\n\n" +
        "It is finished when the run's list is empty for three releases in a row, and when the " +
        "slowest page of each run is shown on the release's own page beside its checks, so a " +
        "reader of the release sees how near the bound the slowest page stood.",
      owner: "The release team",
      title: "Every page answers in under a second at the load we are heading to",
    },
  ] as const;

/** Writes, before any row draws a page, the read marks the pages of the shot set write as they
 *  are drawn, so each page shows the same reads whichever pages a run drew before it. A read mark
 *  cannot be taken back: an agent's `read_through` only moves forward and its `read_replies` only
 *  add ids (`putUserAgentState`, `api/state.go`), and a document's read version is what the Spec
 *  tab compares its blocks against. So a page that wrote one changed every page drawn after it -
 *  the rail's Agents badge read "New replies 2" before the broadcast page and "New reply 1" after
 *  it - and the visual layer, which seeds once for the whole run, held each of those two states
 *  in the baselines of the rows on either side.
 *
 *  A full visual run writes three marks while its rows draw, read from its stack's request log:
 *  the broadcast page's read of the replies its recipients wrote (`BroadcastPage.tsx`,
 *  `useMarkShownRepliesRead`), and the Spec tab's read of the issue's spec at its newest version,
 *  in the desktop row and again in the phone row (`useSpecMarks`). These are those writes, the same
 *  requests the pages send; after them each page's own write changes nothing. It answers the marks
 *  it wrote, which the visual rows' guard lets a page write again only exactly as they stand
 *  (`isSeededReadMark`, e2e/visual-guard.ts): a page of the shot set that comes to write another
 *  mark as it draws needs that mark written here, and until it is, the guard stops the write and
 *  fails the row that made it, by name. */
async function markWhatTheShotPagesRead(
  broadcastId: string,
  issue: string,
  specId: string
): Promise<SeededReadMarks> {
  const broadcast = await getBroadcast(broadcastId);
  const agents: Record<string, readonly string[]> = {};
  let replies = 0;
  for (const recipient of broadcast.recipients ?? []) {
    // The page's own filter: a reply its session wrote, by kind and id, which is what the server
    // takes in `read_replies` and refuses the whole write over one that fails it.
    const shown = (recipient.replies ?? [])
      .filter(
        (reply) => reply.author.kind === "session" && reply.author.id === recipient.session_id
      )
      .map((reply) => reply.id);
    if (shown.length > 0) {
      await putAgentState(recipient.session_id, { read_replies: shown });
      agents[recipient.session_id] = [...(agents[recipient.session_id] ?? []), ...shown];
      replies += shown.length;
    }
  }
  if (replies === 0) {
    throw new Error(
      "seedReviewRoutes: the broadcast holds no reply its recipients wrote, so its page marks " +
        "nothing read and the rows' Agents badge reads a state no baseline holds"
    );
  }
  if (specId === "") {
    throw new Error(`seedReviewRoutes: ${issue} has no spec for its Spec tab to read`);
  }
  // The server lists a document's versions oldest first, and the Spec tab shows the one with the
  // highest number (`versionsNewestFirst`, then the first): so the mark is the highest number, read
  // from every version, and never the list's first, which is the newest only for a one-version spec.
  const numbers = ((await getArtifact(specId)).versions ?? []).map((version) => version.number);
  if (numbers.length === 0) {
    throw new Error(`seedReviewRoutes: ${issue}'s spec has no version for its Spec tab to read`);
  }
  const version = Math.max(...numbers);
  await putDocumentRead(specId, version);
  return { agents, documents: { [specId]: version } };
}

/** Waits, within `boundMs`, until the document service owes no document a settlement, then holds
 *  that the corpus records no check result and holds no pull request's head, so no read of a shot
 *  page has a settlement to run or a commit to ask GitHub about. */
async function awaitNothingOwed(boundMs: number): Promise<void> {
  await awaitFact({
    boundMs,
    fact: "seedReviewRoutes: the document service owes no document of the corpus a settlement",
    holds: (owed) => owed === "",
    // A tenth of a second between reads, as the seed's other waits read.
    intervalMs: 100,
    read: () =>
      sql(
        `SELECT coalesce(string_agg(artifact_id::text, ', ' ORDER BY artifact_id), '')
           FROM doc_settlements_pending`
      ),
    show: (owed) => `settlements owed to ${owed}`,
  });
  const held = await sql(
    `SELECT (SELECT count(*) FROM check_results) || ' ' || (SELECT count(*) FROM pull_request_heads)`
  );
  if (held !== "0 0") {
    const [results, heads] = held.split(" ");
    throw new Error(
      `seedReviewRoutes: the corpus records ${results} check result(s) and ${heads} pull request ` +
        "head(s): the Verification tab's read would ask GitHub which pull request holds a " +
        "result's commit and write the answer, a write the visual rows' corpus must not take"
    );
  }
}

/** The issues the visual lane's command-bar rows read, plan on and change
 *  (`e2e/going-bar.visual.e2e.ts`), since those rows seed nothing. They are OPS's, a project whose
 *  pages no row of the shot set opens, and bob's, so none of them is alice's: the rail and the
 *  Inbox the shot pages draw for her count her open asks, and none of these holds one. None
 *  stands in a status in flight either (`work_under_no_priority`, the rail's Priorities count and
 *  `/priorities`'s first fault list): a new issue is in triage, and the three patched below go
 *  to todo, backlog and icebox. The dome's route names the workspace's Planner, so the Planner
 *  works on the dome, and a message to it that state 6's plan proposes is placed on the dome
 *  (R7, `goingPlaceSend`): "Message to Planner on ‹the dome's key› · as a BTW". A route draws on
 *  no page of the shot set.
 *
 *  Created one at a time, for the reason `seedWorkspace` gives: the server numbers a key as it
 *  inserts the issue, so a round of them at once would give each a different key on each run,
 *  and a key is printed on the bar's shots. */
async function seedBarIssues(): Promise<BarSeed> {
  const bob = { login: "bob" };
  const project = "OPS";
  const make = async (title: string, spec?: string) =>
    (await createIssue(spec === undefined ? { project, title } : { project, spec, title }, bob))
      .key;
  const mount = await make("Calibrate the telescope mount");
  const craters = await make("Map the lunar craters");
  const mirror = await make("Grind the primary mirror");
  const dome = await make(
    "Raise the observatory dome",
    "# Raise the observatory dome\n\nThe dome turns by hand.\n"
  );
  const shutter = await make("Retire the old shutter");
  const eyepiece = await make("Polish the eyepiece");
  const finder = await make("Align the finder scope");
  await patchIssue(mount, { status: "todo" }, bob);
  await patchIssue(craters, { status: "backlog" }, bob);
  await patchIssue(mirror, { status: "icebox" }, bob);
  await patchIssue(dome, { route: `session:${sessionIds.planner}` }, bob);
  return { craters, dome, eyepiece, finder, mirror, mount, project, shutter };
}

/** Seeds the review corpus into an empty database; the caller resets first. `boundMs` is the
 *  bound of the seed's waits, which `seedWorkspace` gives each of its own: the caller's, so a
 *  browser row's seed gives up at the bound that row's assertions use. */
export async function seedReviewRoutes({
  boundMs,
}: {
  readonly boundMs: number;
}): Promise<ReviewSeed> {
  const workspace = await seedWorkspace({ boundMs });
  // The registry with the two claim holders beside the workspace's sessions, written before any
  // of them claims an issue, so the Agents page lists them and each claim reads as held.
  await setLiveSessions(reviewSessions);
  const issue = workspace.issues.workflow;

  // `seedPipelineFixture()` is `seedDeliveryFixture()`'s settings row written a second time and
  // then the Pipeline's own rows, and `delivery_settings` holds one row, so the two fixtures are
  // seeded as their halves: the settings and the timeline's rows, then `seedPipelineRows()`,
  // which only updates that one row's job names before writing the Pipeline's runs.
  await seedDeliveryFixture();
  await seedPipelineRows();

  // The freshness rows of the Delivery and the Pipeline pages, pinned to the verdict a
  // reconcile pass reaches here. Nothing seeds a GitHub App for `acme/widgets`, so every pass
  // this server runs against the corpus fails with exactly the error written below, and a
  // failed pass leaves `last_reconcile_at` null and overwrites `last_error` with it
  // (`delivery/reconcile.go`, `store.go`'s `RecordReconcileError`). What is not fixed is
  // WHETHER a pass has run: the first tick is jittered across the five-minute interval
  // (`delivery.ReconcileInterval`), so one run of these pages read "PRs never checked" and the
  // next, ninety seconds later, read "PRs never checked: the GitHub App is not installed on
  // acme/widgets" - eight lines of difference across the two pages, and two labels of
  // different widths, which no mask can cover and no wait can settle inside a row. Writing the
  // verdict here makes both reads the same whether a pass has run or not, as
  // `e2e/delivery.e2e.ts` writes it for the same reason.
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`
  );

  const requirements = await createIssueArtifact(issue, {
    content: REQUIREMENTS,
    name: "requirements.md",
  });
  const served = `dispatch://${issue}/artifact/${requirements.artifact.slug}`;

  const plan = await createIssueArtifact(issue, {
    // A `needs` pointer names a task of the plan's own document, so both tasks are written in
    // the one upload that makes the plan.
    content: `How ${issue} is built.

:::plan_task{#t0 serves="${served}#b-r1"}
Write the route table.
:::

:::plan_task{#t1 serves="${served}#b-r2" needs="dispatch://${issue}/artifact/plan-md#b-t0"}
Draw each page and read the locator that says it is drawn.
:::
`,
    name: "plan.md",
  });
  // A plan is approved by its plan review, never by a person, and the tab draws that chip.
  await createPlanReview(plan.artifact.id, 1);

  // No result is recorded against either check: where a result's commit stands is judged against
  // the head of the pull request Dispatch holds for the issue, which would need a fake-GitHub
  // repository this corpus has no other use for (`e2e/verification.e2e.ts`). Both checks
  // therefore read "No result", which is a state the tab draws.
  await createIssueArtifact(issue, {
    content: `How ${issue} is proved.

:::check{#c-routes serves="${served}#b-r1" surface="cli" run="bun test e2e/__tests__/review-routes.test.ts" negative_control="Drop a row and the test names the route it no longer covers."}
Every route of the router is reviewed or excluded.
:::

:::check{#c-ready serves="${served}#b-r2" surface="browser" run="bunx playwright test --config e2e/playwright.config.ts" negative_control="Point a row at a skeleton and the run times out on it."}
Each page is read once it is drawn.
:::
`,
    name: "verification.md",
  });

  const specId = (await getIssue(issue)).primary_artifact_id ?? "";
  const shaped = await blockId(specId, "One command, a seeded corpus, a signed-in browser.");
  await createIssueArtifact(issue, {
    content: `## ${DECISION}\n\n${REASON} Shapes dispatch://${issue}/spec#b-${shaped}.\n`,
    name: "decisions.md",
  });

  // The workflow issue's header reads as a working issue's does: the decision its spec asks
  // waits on alice, the signed-in reader, so the header carries "Waiting on you"; it carries two
  // labels; and its message route is a role the Planner session holds in the fake Envoy
  // (`workspaceSessions`), so the route reaches someone and the header warns of nothing.
  await patchIssue(issue, { labels: ["dispatch", "testing"], route: "role:planner" });

  // The work this corpus adds (`WORK`), created one at a time for the reason `seedWorkspace`
  // creates its own so: the server numbers a key as it inserts the issue, and the pages these
  // rows shoot print keys. Each description that names one of them is written with its key.
  const names = Object.keys(WORK) as WorkName[];
  const keys = {} as Record<WorkName, string>;
  for (const name of names) {
    const { project, title } = WORK[name];
    keys[name] = (await createIssue({ project, title })).key;
  }

  // The priorities `/priorities` lists, the first of which `/priorities/:id` opens, and the work
  // under them. The workflow issue stands in `in_progress`, so the first's row counts it in
  // flight and its own page lists it; the freshness-folds issue stands in `backlog`, so it is a
  // row of the second fault list, under the first priority. Three issues standing in triage
  // advance the next three, so their rows count work waiting. Every other issue of
  // `seedWorkspace`'s that is in flight advances none, which is a row of the first fault list
  // beside `WORK`'s own. The relations are written last, so the activity of each issue keeps the
  // order the rows above it were seeded in, and each write is one after the other: a priority's
  // margin lists its history newest first by the order the store wrote it, so two writes made at
  // once land in either order and the page differs between two runs of one tree (the 68 pixels
  // the visual review read on `priority` in two rounds). The priorities are written one after
  // another too, since each takes the last place of the order as it is written.
  const [first, second, third, fourth, ...rest] = priorities(keys);
  const priority = await createPriority(first);
  const docs = await createPriority(second);
  const releases = await createPriority(third);
  const notes = await createPriority(fourth);
  const ids = [priority.id, docs.id, releases.id, notes.id];
  for (const one of rest) ids.push((await createPriority(one)).id);
  await patchIssue(issue, { advances: [priority.id] });
  await patchIssue(workspace.issues.folds, { advances: [priority.id] });
  await patchIssue(workspace.issues.rotate, { advances: [docs.id] });
  await patchIssue(workspace.issues.snooze, { advances: [releases.id] });
  await patchIssue(workspace.issues.retracted, { advances: [notes.id] });
  // Each of `WORK`'s in its status, under its priority where it has one, in one write.
  for (const name of names) {
    const { status, under }: Work = WORK[name];
    const id = under === null ? undefined : ids[under];
    if (under !== null && id === undefined) {
      throw new Error(`seedReviewRoutes: ${name} is the work of priority ${under}, not written`);
    }
    await patchIssue(keys[name], id === undefined ? { status } : { advances: [id], status });
  }
  // Then the claims, after every status: a claim is a session's, and the session holds the
  // issue in the status it stands in.
  for (const name of names) {
    const { holder }: Work = WORK[name];
    if (holder !== undefined) {
      await claimIssue(keys[name], asWorkspaceSession(sessionIds[holder]));
    }
  }

  // The slug of the `Runbook` document `seedWorkspace` creates: read back rather than assumed,
  // so a rename there fails this seed instead of the page it opens.
  const runbook = await getProjectArtifact("CORE", "runbook");

  // The command bar's issues, before the wait and the read marks below, which stay last: the
  // dome's spec is a document, which the wait covers too.
  const bar = await seedBarIssues();

  // Nothing owed before any row draws: a settlement the document service still owes runs when a
  // page opens that document's room and writes a version the shot would draw, and a result's
  // commit is one `GET /issues/{key}/results` asks GitHub the pull request of, writing what it
  // learns. The pages' rows stop every write their pages send (e2e/visual-guard.ts) and a Go test
  // holds what the server writes while it answers their reads (visual_shot_reads_test.go); these
  // two the seed must leave settled itself.
  await awaitNothingOwed(boundMs);

  // Last, so the spec's newest version is the one its tab will show.
  const readMarks = await markWhatTheShotPagesRead(workspace.broadcast.id, issue, specId);

  return {
    bar,
    broadcast: workspace.broadcast.id,
    deliveryQuery: TIMELINE_FIXTURE_QUERY,
    issue,
    issueArtifactSlug: requirements.artifact.slug,
    // The machine of the first seeded session, read from the registry this seed wrote rather
    // than named again here, so a rename in e2e/workspace.ts moves this page's URL with it.
    machine: firstMachine(),
    person: PERSON,
    pipelineAt: PIPELINE_AT,
    readMarks,
    // The id the create answered, which is this title written as a slug: read back rather than
    // spelled again here, so the rule that derives it stays the server's.
    priority: priority.id,
    project: "CORE",
    projectDocumentSlug: runbook.slug,
  };
}
