import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact, HandoffSummary } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import {
  pageHairBg,
  pageInkText,
  pageKindTag,
  pageLineBorder,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageRowAct,
  pageStateText,
  pageStatusProgressText,
} from "../../theme/classes";
import { ActivityLine } from "../activity/ActivityLine";
import { type SentencePart, SentenceParts } from "../activity/SentenceParts";
import { HISTORY_ENTRY_ATTRIBUTE } from "../margin/details-focus-keeper";
import { KEEPS_DETAILS_ATTRIBUTE } from "../margin/Margin";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { EditedMark } from "../refs/PostMarks";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { Identifier, Time } from "../steps/StepsTab";
import type { StepRow } from "../steps/steps-model";
import { type HandoffBodyReads, type HandoffFold, HistoryHandoff, NoValue } from "./HistoryHandoff";
import {
  type HistoryBlock,
  type HistoryDay,
  type HistoryEntry,
  type HistoryTurn,
  handoffLine,
  type NamedBlock,
} from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { DeletedLine, lineOrKindWord, useNodeLine } from "./node-line";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";

// The entries of the issue's history as `IssueHistory.tsx` draws them: each kind of entry, and
// the key each run of uploads is drawn under.

/** One entry, the prototype's `.hev`: 12 px on 18 px in the muted grey, 14 px from a 2 px rule
 *  at its left in the page's line, the rule 4 px from the column's edge, 4 px above and 6 px
 *  below. The prototype's rule is #EEEAE3, which no page token holds; `page-line` is #ECEAE7. */
const ENTRY = `ml-1 border-l-2 pt-1 pb-1.5 pl-3.5 text-xs leading-[18px] ${pageLineBorder} ${pageMutedText}`;

/** When an entry was made, which opens it (`.ht`), 6 px before what follows: the quiet grey, as
 *  the prototype's #A8A29E reads 2.52:1 on the paper, under the 4.5:1 text needs. */
const TIME = `mr-1.5 ${pageQuietText}`;

/** Who made the entry (`.hev b`). */
const AUTHOR = `font-semibold ${pageInkText}`;

/** An agent's words (`.hb`): 14 px on 20 px in the issue's own colour, the prototype's
 *  #44403C, 2 px under the entry's line. */
const WORDS = `mt-0.5 min-w-0 text-sm leading-5 [overflow-wrap:anywhere] ${pageKindTag.issue}`;

/**
 * The key each run of uploads among `days` is drawn under, by the run's id. The model names a
 * run by its newest upload (`HistoryEntry`), so a newer upload that joins a run renames it;
 * drawn under that name, the run would be mounted anew, and a focus on one of its files' links
 * or Pins would fall to the page. So a run keeps the key it was first drawn under: the key one
 * of its files was drawn under (`held`, by each file's upload) unless another run here took it,
 * else its own id, with a count where that is taken too. A newer upload can split a run drawn
 * before (`buildHistory`): the runs are read newest first, so the part that holds the newer
 * upload keeps the run's key, and the files split off, more than a minute older than that
 * upload, are drawn anew under their own run's id, which can be the very key the newer part
 * kept. Every file of the run is then held under its key.
 */
export function runKeys(
  days: readonly HistoryDay[],
  held: Map<number, string>
): ReadonlyMap<string, string> {
  const keys = new Map<string, string>();
  const taken = new Set<string>();
  for (const day of days) {
    for (const entry of day.entries) {
      if (entry.kind !== "uploads") continue;
      let key = entry.id;
      for (const file of entry.files) {
        const kept = held.get(file.id);
        if (kept !== undefined && !taken.has(kept)) {
          key = kept;
          break;
        }
      }
      for (let count = 1; taken.has(key); count += 1) key = `${entry.id}:${count}`;
      taken.add(key);
      keys.set(entry.id, key);
      for (const file of entry.files) held.set(file.id, key);
    }
  }
  return keys;
}

/** One entry of the history (`HistoryEntry`). */
export function Entry({
  addressed,
  bodies,
  drawnId,
  entry,
  fold,
  issueKey,
  item,
  onPin,
  pinned,
}: {
  /** The id of the entry a handoff's address names, which is drawn marked, as the turn an
   *  address names is (`Turn`). */
  addressed: string | undefined;
  /** The reads of the bodies of the history's handoffs (`useHandoffBodies`). */
  bodies: HandoffBodyReads;
  /** The id the entry is drawn under: its own, or a run's key (`runKeys`). */
  drawnId: string;
  entry: HistoryEntry;
  /** The handoffs the reader folded on the issue's page (`useHandoffFold`). */
  fold: HandoffFold;
  issueKey: string;
  item: string | undefined;
  onPin: (eventId: number) => void;
  pinned: (eventId: number) => boolean;
}): ReactNode {
  if (entry.kind === "unread") {
    return (
      <li
        aria-label={CONTROL_LABELS.history_unread}
        className={`my-1 flex items-center gap-3 text-xs leading-[18px] font-semibold ${pageInkText}`}
        {...pageChrome}
      >
        <span className={`h-px flex-1 ${pageHairBg}`} />
        {CONTROL_LABELS.history_unread}
        <span className={`h-px flex-1 ${pageHairBg}`} />
      </li>
    );
  }
  // The entry a handoff's address names is marked, as the turn an address names is (`Turn`).
  const marked = addressed === entry.id;
  const mark = marked ? ` rounded-r-md ${pageMarkBg}` : "";
  // A run is its task and its fields, and the handoff it reports, open as every handoff is.
  if (entry.kind === "run") {
    return (
      <li
        aria-current={marked ? "true" : undefined}
        className={`${ENTRY} flex min-w-0 flex-col gap-1${mark}`}
        data-event-seq={entry.seq}
        data-step-run={entry.row.id}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
      >
        <RunFields row={entry.row} task={entry.task} />
        {entry.handoff === undefined ? null : (
          <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
            <HandoffParts
              bodies={bodies}
              fold={fold}
              handoff={entry.handoff}
              onPin={onPin}
              pinned={pinned}
              recorded={entry.recorded}
            />
          </div>
        )}
      </li>
    );
  }
  if (entry.kind === "handoff") {
    return (
      <li
        aria-current={marked ? "true" : undefined}
        className={`${ENTRY} flex min-w-0 flex-wrap items-baseline gap-x-2${mark}`}
        data-event-seq={entry.seq}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
      >
        <HandoffParts
          bodies={bodies}
          fold={fold}
          handoff={entry.handoff}
          onPin={onPin}
          pinned={pinned}
          recorded={entry.recorded}
        />
      </li>
    );
  }
  // What a dated entry opens with (`.hev`): when, then who, then what they did.
  const head = (
    <>
      <Timestamp at={entry.at} className={TIME} />
      <span className={AUTHOR}>
        <AgentName actor={entry.author} inline />
      </span>{" "}
    </>
  );
  if (entry.kind === "event") {
    const isPinned = pinned(entry.event.id);
    return (
      <li
        className={`${ENTRY} flex min-w-0 flex-wrap items-baseline gap-x-2`}
        data-event-seq={entry.seq}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
      >
        {/* Marked as a node's words, so any link an inline renderer draws inside a quoted text
            (an approval request's question, a resolution's reason) takes the words' own ink and
            underline (`styles.css`), never the old application's link blue. */}
        <span className="min-w-40 flex-1 break-words" data-node-words="">
          {head}
          {/* `main`'s line links a document's name in the application's link blue, which
              Details draws in no line: those two lines are the history's own. So is a line that
              names a block by its node's words, where `main`'s names it by its id. */}
          {entry.event.type === "artifact.created" || entry.event.type === "artifact.renamed" ? (
            <DocumentLine artifact={entry.event.payload.artifact} description={entry.description} />
          ) : entry.block === undefined ? (
            <ActivityLine description={entry.description} event={entry.event} issueKey={issueKey} />
          ) : (
            <BlockLine block={entry.block} description={entry.description} />
          )}
        </span>
        <PinButton
          label={isPinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
          onClick={() => onPin(entry.event.id)}
          pinned={isPinned}
          quiet
        />
      </li>
    );
  }
  // Each file of a run is one event's upload, so each has that event's Pin and Unpin, as every
  // line of the history has its event's: the run's line says who uploaded, and each file
  // stands under it on a line of its own, beside its pin, as an event's words stand beside theirs.
  // Each file is an entry the reader's place is held by (`ViewportAnchor`), so an older page
  // that adds files under the one they read leaves it where it was.
  if (entry.kind === "uploads") {
    return (
      <li
        className={`${ENTRY} flex min-w-0 flex-col`}
        data-event-seq={entry.seq}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
        data-history-uploads={entry.files.length}
      >
        <span className="min-w-0 break-words">
          {head}
          <span {...pageChrome}>{CONTROL_LABELS.history_uploaded}</span>
        </span>
        <ul className="flex min-w-0 flex-col">
          {entry.files.map((file) => {
            const isPinned = pinned(file.id);
            return (
              <li
                className="flex min-w-0 items-center gap-x-2"
                {...{ [HISTORY_ENTRY_ATTRIBUTE]: `file:${file.id}` }}
                key={file.id}
              >
                <span className="min-w-0 flex-1 break-words">
                  {file.slug === undefined ? (
                    file.name
                  ) : (
                    <Link
                      className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
                      to={buildIssuePath({ key: issueKey, kind: "artifact", slug: file.slug })}
                    >
                      {file.name}
                    </Link>
                  )}
                </span>
                <PinButton
                  label={isPinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
                  onClick={() => onPin(file.id)}
                  pinned={isPinned}
                  quiet
                />
              </li>
            );
          })}
        </ul>
      </li>
    );
  }
  return (
    <li
      className="flex min-w-0 flex-col"
      data-event-seq={entry.seq}
      {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
    >
      <Turn issueKey={issueKey} marked={item === entry.root.id} turn={entry.root} />
      {entry.replies.length === 0 ? null : (
        <ol className="ml-3.5 flex min-w-0 flex-col">
          {entry.replies.map((reply) => (
            <li key={reply.id}>
              <Turn issueKey={issueKey} marked={item === reply.id} turn={reply} />
            </li>
          ))}
        </ol>
      )}
    </li>
  );
}

/**
 * A handoff of the issue as the list of its handoffs gives it, in its own entry or in the entry of
 * the run that reports it: when it was written, who wrote it and its phase and version, as the
 * line of its event reads (`handoffLine`), with the Pin of that event where the log holds it
 * (`recorded`), then its fields, open until the reader folds them (`HistoryHandoff`), which stand
 * on a row of their own in the flex line the caller draws them in.
 */
function HandoffParts({
  bodies,
  fold,
  handoff,
  onPin,
  pinned,
  recorded,
}: {
  bodies: HandoffBodyReads;
  fold: HandoffFold;
  handoff: HandoffSummary;
  onPin: (eventId: number) => void;
  pinned: (eventId: number) => boolean;
  recorded: number | undefined;
}): ReactNode {
  const isPinned = recorded !== undefined && pinned(recorded);
  return (
    <>
      <span className="min-w-40 flex-1 break-words" data-handoff-line={handoff.id}>
        <Timestamp at={handoff.created_at} className={TIME} />
        <span className={AUTHOR}>
          <AgentName actor={handoff.author} inline />
        </span>{" "}
        <SentenceParts parts={handoffLine(handoff)} />
      </span>
      {recorded === undefined ? null : (
        <PinButton
          label={isPinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
          onClick={() => onPin(recorded)}
          pinned={isPinned}
          quiet
        />
      )}
      <HistoryHandoff
        bodies={bodies}
        folded={fold.folded(handoff.id)}
        handoffId={handoff.id}
        onFold={(folded) => fold.setFolded(handoff.id, folded)}
      />
    </>
  );
}

/** The label of each field of a run, and how a run ended, in the page's words. */
const STEP_FIELDS = CONTROL_LABELS.step_fields;
const STEP_RESULTS = CONTROL_LABELS.step_results;

/** The colour each result Details has a word for is drawn in, as Details' other states are
 *  drawn: a run that runs in the in-progress blue, one that passed in the passed green, one that
 *  failed in the failed red, and one that stopped in the ink every value of a run is drawn in
 *  (`RunField`). Each is a state of `StepEnd` but `other`, so a state `StepEnd` adds fails to
 *  compile until it has a colour here. */
const RESULT_COLOURS: Record<Exclude<StepRow["state"], "other">, string> = {
  failed: pageStateText.failed,
  passed: pageStateText.passed,
  running: pageStatusProgressText,
  stopped: "",
};

/** One labelled value of a run: its label in the quiet grey, as Details labels its rows, and the
 *  value in the ink beside it, given the width Details has left so it wraps rather than runs
 *  past it. `field` names it for a reader of the page's markup. */
function RunField({
  children,
  field,
  label,
}: {
  children: ReactNode;
  field: string;
  label: string;
}): ReactNode {
  return (
    <div className="grid min-w-0 grid-cols-[88px_minmax(0,1fr)] gap-2" data-step-field={field}>
      <dt className={pageQuietText} {...pageChrome}>
        {label}
      </dt>
      <dd className={`min-w-0 break-words ${pageInkText}`}>{children}</dd>
    </div>
  );
}

/** An agent token or a model id in the type every id of a run is drawn in, broken as the Steps
 *  tab breaks it (`Identifier`), or None for one the record does not hold. */
function IdentifierValue({ value }: { value: string }): ReactNode {
  return value === "" ? (
    <NoValue />
  ) : (
    <span className="font-mono" {...authorText}>
      <Identifier value={value} />
    </span>
  );
}

/** When a run started or ended, as the Steps tab writes it (`Time`), or None for a time the
 *  record does not hold. */
export function TimeValue({ at }: { at: string | null }): ReactNode {
  return at === null ? (
    <NoValue />
  ) : (
    <span {...authorText}>
      <Time at={at} />
    </span>
  );
}

/**
 * One run's task and the ten values the Steps section drew of it (the plan's D10), as a list of
 * labelled values rather than a row of a table, so each value has the width of Details to wrap
 * in: its step, role, type, agent, model, attempt, start, end and result, and where a failed or
 * stopped run sent its task back, where it sent it (`StepEnd.returnedTo`), which wraps at any
 * width. Its agent, model, start and end are drawn as the Steps tab draws them (`Identifier`,
 * `Time`), as text the page did not author, and one the record does not hold reads None. A
 * result this build has no word for shows the server's own.
 */
function RunFields({ row, task }: { row: StepRow; task: string }): ReactNode {
  return (
    <dl className="flex min-w-0 flex-col gap-0.5 text-xs leading-[18px]">
      <RunField field="task" label={CONTROL_LABELS.steps_task}>
        <span className="font-semibold" {...authorText}>
          {task}
        </span>
      </RunField>
      <RunField field="step" label={STEP_FIELDS.step}>
        <span className="font-semibold" {...authorText}>
          {row.label}
        </span>
      </RunField>
      <RunField field="role" label={STEP_FIELDS.role}>
        {row.role === "" ? <NoValue /> : <span {...authorText}>{row.role}</span>}
      </RunField>
      <RunField field="type" label={STEP_FIELDS.type}>
        {row.type === "" ? <NoValue /> : <span {...authorText}>{row.type}</span>}
      </RunField>
      <RunField field="agent" label={STEP_FIELDS.agent}>
        <IdentifierValue value={row.agent} />
      </RunField>
      <RunField field="model" label={STEP_FIELDS.model}>
        <IdentifierValue value={row.model} />
      </RunField>
      <RunField field="attempt" label={STEP_FIELDS.attempt}>
        <span className="tabular-nums" {...authorText}>
          {row.attempt}
        </span>
      </RunField>
      <RunField field="started" label={STEP_FIELDS.started}>
        <TimeValue at={row.started} />
      </RunField>
      <RunField field="ended" label={STEP_FIELDS.ended}>
        <TimeValue at={row.ended} />
      </RunField>
      <RunField field="result" label={STEP_FIELDS.result}>
        {row.state === "other" ? (
          <span {...authorText}>{row.result}</span>
        ) : (
          <span className={`font-medium ${RESULT_COLOURS[row.state]}`} {...pageChrome}>
            {STEP_RESULTS[row.state]}
          </span>
        )}
      </RunField>
      {(row.state === "failed" || row.state === "stopped") && row.returnedTo !== null ? (
        <RunField field="returned_to" label={STEP_FIELDS.returned_to}>
          <span {...authorText}>{row.returnedTo}</span>
        </RunField>
      ) : null}
    </dl>
  );
}

/** The line of a document added or renamed, as `ActivityLine` draws it - its words, with the
 *  last reading of the document's name a link to its page, the new title of a rename, whose slug
 *  does not move - in the page's ink and its rows' underline, as an upload's file is. */
function DocumentLine({
  artifact,
  description,
}: {
  artifact: Pick<
    Artifact,
    "issue_key" | "kind" | "name" | "primary" | "project" | "session_id" | "slug"
  >;
  description: readonly SentencePart[];
}): ReactNode {
  const name = artifact.name;
  const linked = description.findIndex((part) => typeof part === "string" && part.includes(name));
  return (
    <span>
      <SentenceParts
        parts={description}
        text={(part, index) => {
          if (index !== linked) return part;
          const at = part.lastIndexOf(name);
          return (
            <>
              {part.slice(0, at)}
              <Link
                className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
                to={buildReferencePath(documentRoute(artifact))}
              >
                {name}
              </Link>
              {part.slice(at + name.length)}
            </>
          );
        }}
      />
    </span>
  );
}

/** The line of a block repaired or found invalid that names the block by its node's one line
 *  (`HistoryEntry.block`), as `DocumentLine` draws a document's: its words, with the block's -
 *  the part of the line `block.part` names - drawn as that line (`BlockName`). */
function BlockLine({
  block,
  description,
}: {
  block: NamedBlock;
  description: readonly SentencePart[];
}): ReactNode {
  return (
    <span>
      <SentenceParts
        parts={description}
        text={(part, index) => (index === block.part ? <BlockName block={block.block} /> : part)}
      />
    </span>
  );
}

/** The one line of the node that carries a block a line of the history names, as the outline
 *  draws a node's (`node-line.tsx`'s `useNodeLine`): its first line, each reference in it read as
 *  its target's name, and the page's word for its kind where its words hold none; a link to the
 *  node in the page's ink and its rows' underline, where its address reads as one. Nothing of it
 *  until the projection has answered. */
function BlockName({ block }: { block: HistoryBlock }): ReactNode {
  const { line, reads } = useNodeLine(block.node, { surface: "issue-page-history-block" });
  const marks = line === "" ? pageChrome : authorText;
  const words = lineOrKindWord(line, block.node.type);
  return (
    <>
      {reads}
      {block.path === undefined ? (
        <span {...marks}>{words}</span>
      ) : (
        <Link
          className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
          to={block.path}
          {...marks}
        >
          {words}
        </Link>
      )}
    </>
  );
}

/**
 * One turn of a thread the history holds: when, who, and ` · edited` after the author where
 * the author edited it (`EditedMark`); its words whole at any length, or, where its author
 * deleted it, the deleted line in their place, as the outline's rows draw it (`DeletedLine`);
 * and Reply, which a deleted turn has not, as Dispatch takes no reply to it.
 */
function Turn({
  issueKey,
  marked,
  turn,
}: {
  issueKey: string;
  marked: boolean;
  turn: HistoryTurn;
}): ReactNode {
  const writer = useOutlineWriter();
  const drawer = useMarginDrawer();
  const phone = useMediaQuery(PHONE_VIEWPORT_QUERY);
  return (
    <article
      aria-current={marked ? "true" : undefined}
      className={`${ENTRY} flex min-w-0 flex-col ${marked ? `rounded-r-md ${pageMarkBg}` : ""}`}
      data-history-item={turn.id}
      data-history-kind={turn.kind}
    >
      <p className="min-w-0 break-words">
        <Link
          className={`dispatch-inline-link ${TIME} ${pageRowAct}`}
          data-history-time=""
          to={buildIssuePath({ id: turn.id, key: issueKey, kind: turn.kind })}
          {...{ [KEEPS_DETAILS_ATTRIBUTE]: "" }}
        >
          <Timestamp at={turn.at} />
        </Link>
        <span className={AUTHOR}>
          <AgentName actor={turn.author} inline />
        </span>
        <EditedMark at={turn.editedAt} deleted={turn.deleted} marks={pageChrome} />
      </p>
      {/* Marked as a node's words, so `styles.css` keeps a wide table or line of code scrolling
          sideways inside itself and gives the words the rhythm, links, chips and pictures of a
          node's words elsewhere on the page. */}
      <div className={WORDS} data-node-words="">
        {turn.deleted === null ? (
          <MarkdownBody
            markdown={turn.body}
            source={textSourceOf(turn.kind, turn.id, issueKey)}
            surface="issue-page-history-turn"
            tree={turn.bodyTree}
          />
        ) : (
          <DeletedLine deleted={turn.deleted} />
        )}
      </div>
      {writer === undefined || turn.deleted !== null ? null : (
        <p>
          <button
            className={`dispatch-inline-link text-xs font-medium ${pageMutedText} ${pageRowAct}`}
            data-history-reply={turn.id}
            onClick={() => {
              // On a phone Details covers the outline whose foot the line is at, so it closes
              // for the reader to write there.
              if (phone) drawer?.setOpen(false);
              writer.replyTo({
                author: turn.author,
                id: turn.id,
                kind: turn.kind,
                text: turn.body,
              });
            }}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.reply}
          </button>
        </p>
      )}
    </article>
  );
}
