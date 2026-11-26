/** Threads' composer census items and their rows in `draft-matrix.threads.test.tsx`. */
import { answers, type CoverageEntry, cells } from "../../../__tests__/composer-census/coverage";

const HOST = "Threads record";
const TYPED = ["typed"];
const REPLY = "CommentBox | CommentBox <MentionComposer mutationKey={sendKey}>";
const EDIT = "CommentEditBox | CommentEditBox <MentionComposer mutationKey={mutationKey}>";

const REASONS = {
  1: "1. A box writes on the issue of the record it answers, and a record is one issue's: another issue's address draws another issue page with its own records, so no box of Threads moves from one issue to another.",
  2: "2. An open ask draws no box at its foot, since its answer box is the ask body's, which Dashboard's matrix holds (`draft-matrix.thread-item.test.tsx`), so the box at its foot first mounts once the ask settles, with nothing typed in it yet.",
  3: "3. A record's kind is its thread's, which never changes, so each of its boxes writes a reply to a comment, a reply on an ask or a message for as long as the record is drawn.",
  4: "4. The editor opens over the comment's words as the record holds them when Edit is pressed: a newer body that lands while it is open is the editor's own case, which the edit matrix holds (`conversation/draft-matrix.edit.test.tsx`).",
} as const;

const oneIssue = { reason: REASONS[1] };
const openAsk = { reason: REASONS[2] };
const oneKind = { reason: REASONS[3] };
const goesFor = (kind: string) =>
  cells(`${HOST} · a reply box goes for another entry's, then opens again · ${kind}`, TYPED);
const leaves = cells(`${HOST} · the reader leaves a reply box, then opens it again`, TYPED);
const apart = cells(`${HOST} · the box at the foot and a reply box keep their own drafts`, TYPED);
const another = (box: string) =>
  cells(`${HOST} · another record opens, then the record again · ${box}`, TYPED);
const narrows = cells(`${HOST} · the window narrows below 1024 px, then widens`, TYPED);
const newThread = cells(`${HOST} · New thread opens, then the record again`, TYPED);
const sentThread = cells(`${HOST} · the thread New thread sent, then a page holds it`, TYPED);
const editGoes = cells(`${HOST} · an edit goes for a reply box, then opens again`, TYPED);
const reloaded = (box: string) => cells(`${HOST} · the page reloaded, then back · ${box}`, TYPED);
const everyBox = [another("foot"), another("reply"), another("edit")];

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    `${REPLY} | address prop | mutationKey={sendKey}`,
    goesFor("comment"),
    leaves,
    another("foot"),
    another("reply")
  ),
  ...answers(`${REPLY} | address prop | owner={owner}`, oneIssue),
  ...answers(`${REPLY} | address prop | replyTo={replyTo}`, goesFor("comment"), apart),
  ...answers(`${REPLY} | address prop | standing={onLeave === undefined}`, apart),
  ...answers(`${REPLY} | lifetime | <MentionComposer mutationKey={sendKey}>`, reloaded("reply")),
  ...answers(`${EDIT} | address prop | edit={edit}`, editGoes),
  ...answers(`${EDIT} | address prop | mutationKey={mutationKey}`, editGoes, another("edit")),
  ...answers(`${EDIT} | address prop | owner={owner}`, oneIssue),
  ...answers(`${EDIT} | lifetime | <MentionComposer mutationKey={mutationKey}>`, reloaded("edit")),

  ...answers('OpenedRecord | CommentBox | conditional | !(record.kind === "message") ? …', oneKind),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the census key contains the JSX key's source text.
    "OpenedRecord | CommentBox | key | <li key={`box-${post.id}`}>",
    goesFor("comment")
  ),
  ...answers(
    'OpenedRecord | CommentBox | conditional | (open?.kind === "reply" && open.id === post.id) ? …',
    goesFor("comment"),
    leaves
  ),
  ...answers(
    "OpenedRecord | CommentBox | conditional | drawn by placedEntries(record).flatMap(…)",
    goesFor("comment")
  ),
  ...answers("OpenedRecord | CommentBox | address prop | issueKey={issue.key}", oneIssue),
  ...answers("OpenedRecord | CommentBox | address prop | onLeave={leave}", leaves),
  ...answers("OpenedRecord | CommentBox | address prop | parent={post}", goesFor("comment")),
  ...answers('OpenedRecord | CommentBox | address prop | parentKind="comment"', oneKind),
  ...answers(
    'OpenedRecord | CommentBox | address prop | sendKey={["threads-reply", record.id, post.id]}',
    goesFor("comment"),
    another("reply")
  ),
  ...answers("OpenedRecord | CommentBox | conditional | !(askOpen) ? …", openAsk),
  ...answers("OpenedRecord | CommentBox | conditional | !(foot === null) ? …", openAsk),
  ...answers(
    "OpenedRecord | CommentBox | address prop | parent={record.root}",
    apart,
    another("foot"),
    goesFor("ask")
  ),
  ...answers("OpenedRecord | CommentBox | address prop | parentKind={record.kind}", oneKind),
  ...answers(
    'OpenedRecord | CommentBox | address prop | sendKey={["threads-reply", record.id, "foot"]}',
    another("foot")
  ),
  ...answers(
    'OpenedRecord | CommentBox | conditional | (record.kind === "ask" && open?.kind === "reply" && open.id === record.root.id) ? …',
    goesFor("ask")
  ),
  ...answers('OpenedRecord | CommentBox | address prop | parentKind="ask"', oneKind),
  ...answers(
    'OpenedRecord | CommentBox | address prop | sendKey={["threads-reply", record.id, record.root.id]}',
    goesFor("ask")
  ),

  ...answers("OpenedRecord | CommentEditBox | conditional | (editing) ? …", editGoes),
  ...answers(
    'OpenedRecord | CommentEditBox | conditional | (open?.kind === "reply" && open.id === post.id) ? …',
    editGoes
  ),
  ...answers(
    'OpenedRecord | CommentEditBox | conditional | !(open?.kind === "reply" && open.id === post.id) ? …',
    editGoes
  ),
  ...answers(
    "OpenedRecord | CommentEditBox | conditional | drawn by placedEntries(record).flatMap(…)",
    editGoes
  ),
  ...answers("OpenedRecord | CommentEditBox | address prop | body={post.body}", {
    reason: REASONS[4],
  }),
  ...answers("OpenedRecord | CommentEditBox | address prop | id={post.id}", editGoes),
  ...answers("OpenedRecord | CommentEditBox | address prop | issueKey={issue.key}", oneIssue),

  ...answers("ThreadsView | OpenedRecord | key | <OpenedRecord key={opened.id}>", ...everyBox),
  ...answers(
    "ThreadsView | OpenedRecord | conditional | !(opened === undefined) ? …",
    reloaded("reply")
  ),
  ...answers("ThreadsView | OpenedRecord | conditional | (record !== undefined) ? …", newThread),
  ...answers("ThreadsView | OpenedRecord | conditional | (wide || open) ? …", narrows),
  ...answers("ThreadsView | OpenedRecord | address prop | issue={issue}", oneIssue),
  ...answers("ThreadsView | OpenedRecord | address prop | record={opened}", ...everyBox),
  ...answers(
    "ThreadsView | OpenedRecord | address prop | thread={[...lists.records.values()].filter((one) => one.thread === opened.thread)}",
    another("foot")
  ),
  ...answers("ThreadsView | OpenedRecord | form | (wide || open) ? …", narrows),
  ...answers("ThreadsView | - | form | (wide) ? …", narrows),
  ...answers("ThreadsView | - | form | (newThread) ? …", newThread),
  ...answers("ThreadsView | - | form | (wide || !open) ? …", narrows),
  ...answers("ThreadsView | OpenedRecord | form | !(opened === undefined) ? …", sentThread),
  ...answers("ThreadsView | OpenedRecord | form | key={opened.id}", sentThread),
  ...answers("ThreadsView | - | form | (sent?.id === record) ? …", sentThread),
  ...answers("ThreadsView | - | form | if (opened !== undefined && !marking)", sentThread),
  ...answers("ViewThreadsSlot | ThreadsView | address prop | issue={issue}", oneIssue),
  ...answers("ViewThreadsSlot | ThreadsView | address prop | route={route}", ...everyBox),
];
