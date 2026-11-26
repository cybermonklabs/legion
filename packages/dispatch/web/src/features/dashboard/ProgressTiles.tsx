import { type ReactNode, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import type { IssueAskRow, IssueCheckResults, IssueCoverage, IssueDetails } from "../../api/types";
import {
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
  pageSegmentBg,
  pageSegmentDoneBg,
  pageSegmentProgressBg,
} from "../../theme/classes";
import { MOVING_STATUSES } from "../issue-page/issue-status";
import { DASHBOARD_WORDS, NODE_TYPE_NAMES } from "../issue-page/page-words";
import { useKnownReferences } from "../refs/reference-names";
import { buildDispatchReference, parseNodeReference, referenceHref } from "../refs/routes";
import { childrenTile, metTile, passedTile, questionsTile, tileList } from "./tiles";

/** One of the four tiles, by its name in `DASHBOARD_WORDS.tiles`. */
type TileKey = keyof typeof DASHBOARD_WORDS.tiles;

/** One entry of a tile's list: the node its number counts, by its reference, which the entry
 *  links and is named by, and the words it is drawn by until the batched name read answers. */
interface TileEntry {
  readonly before?: string;
  readonly fallback: string;
  readonly ref: string;
}

/** A part of a tile's bar, a share of its whole in one colour. */
interface BarPart {
  readonly className: string;
  readonly count: number;
}

interface TileView {
  readonly bar: readonly BarPart[];
  readonly count: number;
  readonly entries: readonly TileEntry[];
  readonly key: TileKey;
  readonly whole: number;
}

/** The four tiles' numbers, wholes, bars and entries, from the reads Dashboard holds. */
export function tileViews({
  asks,
  coverage,
  issue,
  results,
  viewer,
}: {
  asks: readonly IssueAskRow[] | undefined;
  coverage: IssueCoverage | undefined;
  issue: IssueDetails;
  results: IssueCheckResults | undefined;
  viewer: string | undefined;
}): readonly TileView[] {
  const met = metTile(issue, coverage, results);
  const passed = passedTile(results);
  const children = childrenTile(issue);
  const questions = questionsTile(asks, viewer);
  const plain = (count: number): readonly BarPart[] => [{ className: pageSegmentDoneBg, count }];
  return [
    {
      bar: plain(met.counted.length),
      count: met.counted.length,
      entries: met.counted.map((statement) => ({
        fallback: NODE_TYPE_NAMES.requirement,
        ref: statement.ref,
      })),
      key: "met",
      whole: met.whole,
    },
    {
      bar: plain(passed.counted.length),
      count: passed.counted.length,
      entries: passed.counted.map((check) => ({ fallback: NODE_TYPE_NAMES.check, ref: check.ref })),
      key: "passed",
      whole: passed.whole,
    },
    {
      // Done is drawn in the passed colour, a child whose work moves in the progress colour
      // (`MOVING_STATUSES`), and every other status in the bar's own grey.
      bar: children.statuses.map(({ count, status }) => ({
        className:
          status === "done"
            ? pageSegmentDoneBg
            : MOVING_STATUSES.has(status)
              ? pageSegmentProgressBg
              : pageSegmentBg,
        count,
      })),
      count: children.counted.length,
      entries: children.counted.map((child) => ({
        before: child.key,
        fallback: "",
        ref: buildDispatchReference({ key: child.key, kind: "issue" }),
      })),
      key: "children",
      whole: children.whole,
    },
    {
      bar: plain(questions.counted.length),
      count: questions.counted.length,
      entries: questions.counted.map((ask) => ({
        fallback: NODE_TYPE_NAMES.ask,
        ref: buildDispatchReference({ id: ask.id, key: issue.key, kind: "ask" }),
      })),
      key: "questions",
      whole: questions.whole,
    },
  ];
}

/** A tile's bar: its number against its whole, as a meter, each part in its own colour. The
 *  number beside it says the same in words, so the colours repeat it. */
function Bar({ name, tile }: { name: string; tile: TileView }): ReactNode {
  return (
    // A bar split into parts, one per status of the children, which a `<meter>` cannot draw.
    // biome-ignore lint/a11y/useSemanticElements: a native meter draws one fill, not the parts
    <span
      aria-label={name}
      aria-valuemax={tile.whole}
      aria-valuemin={0}
      aria-valuenow={tile.count}
      className={`flex h-1.5 w-full overflow-hidden rounded-full ${pageSegmentBg}`}
      role="meter"
    >
      {tile.whole === 0
        ? null
        : tile.bar.map((part, index) => (
            <span
              className={`block h-full ${part.className}`}
              // biome-ignore lint/suspicious/noArrayIndexKey: a bar's parts are its order and nothing else
              key={index}
              style={{ width: `${(part.count / tile.whole) * 100}%` }}
            />
          ))}
    </span>
  );
}

/** The list under a tile: each entry a link to the node, named as every link to it is named,
 *  20 at a time with a control that says how many are left. */
function TileList({ id, name, tile }: { id: string; name: string; tile: TileView }): ReactNode {
  const [pages, setPages] = useState(1);
  const { names } = useKnownReferences();
  const { left, shown } = tileList(tile.entries, pages);
  return (
    <div className="mt-2 space-y-2">
      <ul aria-label={name} className="space-y-1 text-sm" id={id}>
        {shown.map((entry) => {
          const route = parseNodeReference(entry.ref);
          const href = route === undefined ? undefined : referenceHref(route);
          const words = names.get(entry.ref) ?? entry.fallback;
          return (
            <li className="min-w-0 truncate" key={entry.ref}>
              {href === undefined ? (
                words
              ) : (
                <Link className={`underline ${pageInkText}`} to={href}>
                  {entry.before === undefined ? null : (
                    <span className={pageMutedText}>{entry.before} </span>
                  )}
                  {words}
                </Link>
              )}
            </li>
          );
        })}
      </ul>
      {left > 0 ? (
        <button
          className={`rounded-lg border px-3 py-1 text-xs font-medium ${pageLineBorder} ${pageMutedText}`}
          onClick={() => setPages((count) => count + 1)}
          type="button"
        >
          {`${DASHBOARD_WORDS.next_twenty} · ${left} ${DASHBOARD_WORDS.left}`}
        </button>
      ) : null}
    </div>
  );
}

function Tile({ tile }: { tile: TileView }): ReactNode {
  const [open, setOpen] = useState(false);
  const listId = useId();
  const name = DASHBOARD_WORDS.tiles[tile.key];
  return (
    <div className={`rounded-xl border p-3 ${pageHairBorder}`}>
      <button
        aria-controls={open ? listId : undefined}
        aria-expanded={open}
        className="flex w-full flex-col items-start gap-2 text-left"
        onClick={() => setOpen((shown) => !shown)}
        type="button"
      >
        <span className={`text-xs font-medium ${pageMutedText}`}>{name}</span>
        <span className="flex items-baseline gap-1">
          <span className={`text-2xl leading-none font-semibold tabular-nums ${pageInkText}`}>
            {tile.count}
          </span>
          <span className={`text-xs tabular-nums ${pageQuietText}`}>
            {" / "}
            {tile.whole}
          </span>
        </span>
        <Bar name={name} tile={tile} />
      </button>
      {open ? <TileList id={listId} name={name} tile={tile} /> : null}
    </div>
  );
}

/**
 * Dashboard's third part (dispatch://INFRA-5702/spec#b-r-tiles): four tiles, each a number with a
 * bar of that number against its whole - the statements met, the checks passed, the children
 * done and the open questions whose turn is not the reader's - and a press opens under a tile the
 * list of what its number counts.
 */
export function Tiles(reads: Parameters<typeof tileViews>[0]): ReactNode {
  const { asks, coverage, issue, results, viewer } = reads;
  const tiles = useMemo(
    () => tileViews({ asks, coverage, issue, results, viewer }),
    [asks, coverage, issue, results, viewer]
  );
  return (
    <section aria-label={DASHBOARD_WORDS.parts.progress} className="space-y-2">
      <h2 className={`text-xs font-semibold tracking-wide uppercase ${pageQuietText}`}>
        {DASHBOARD_WORDS.parts.progress}
      </h2>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {tiles.map((tile) => (
          <Tile key={tile.key} tile={tile} />
        ))}
      </div>
    </section>
  );
}
