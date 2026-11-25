import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
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
} from "../../theme/classes";
import { ActivityLine } from "../activity/ActivityLine";
import { HISTORY_ENTRY_ATTRIBUTE } from "../margin/details-focus-keeper";
import { KEEPS_DETAILS_ATTRIBUTE } from "../margin/Margin";
import { AgentName, type AgentPart, AgentParts } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { EditedMark } from "../refs/PostBody";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { HistoryHandoff } from "./HistoryHandoff";
import type { HistoryDay, HistoryEntry, HistoryTurn, NamedBlock } from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { DeletedLine } from "./NodeRow";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";

// The entries of the issue's history as `IssueHistory.tsx` draws them: a day's line, each kind of
// entry, and the key each run of uploads is drawn under.

/** A day's line, the prototype's `.hdl`: 12 px on 18 px at weight 600 in the muted grey, 5 px
 *  above and below, its words as written. */
export const DAY_LINE = `py-[5px] text-xs leading-[18px] font-semibold ${pageMutedText}`;

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
  drawnId,
  entry,
  issueKey,
  item,
  onPin,
  pinned,
}: {
  /** The id the entry is drawn under: its own, or a run's key (`runKeys`). */
  drawnId: string;
  entry: HistoryEntry;
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
        <span className="min-w-40 flex-1 break-words">
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
        {entry.event.type === "handoff.recorded" ? (
          <HistoryHandoff handoffId={entry.event.payload.handoff_id} />
        ) : null}
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
  description: readonly AgentPart[];
}): ReactNode {
  const name = artifact.name;
  const linked = description.findIndex((part) => typeof part === "string" && part.includes(name));
  return (
    <span>
      <AgentParts
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

/** The line of a block repaired or found invalid that names the block by its node's words
 *  (`HistoryEntry.block`), as `DocumentLine` draws a document's: its words, with the block's -
 *  the part of the line `block.part` names - a link to its node in the page's ink and its rows'
 *  underline, where its address reads as one. */
function BlockLine({
  block,
  description,
}: {
  block: NamedBlock;
  description: readonly AgentPart[];
}): ReactNode {
  const { path } = block.block;
  return (
    <span>
      <AgentParts
        parts={description}
        text={(part, index) =>
          index !== block.part || path === undefined ? (
            part
          ) : (
            <Link
              className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
              to={path}
              {...authorText}
            >
              {part}
            </Link>
          )
        }
      />
    </span>
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
            source={textSourceOf(turn.kind, turn.id)}
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
