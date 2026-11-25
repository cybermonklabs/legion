import type { ColumnDef } from "@tanstack/react-table";
import { createContext, Fragment, type ReactNode, useContext } from "react";
import { Link } from "react-router-dom";
import { ChevronIcon } from "../../components/DisclosureToggle";
import {
  focusVisibleRing,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useMarkdownHeadline } from "../refs/markdown-engine";
import { Timestamp } from "../refs/Timestamp";
import { PriorityDescription } from "./PriorityDescription";
import {
  NEVER_MOVED,
  PRIORITY_COLUMNS,
  PRIORITY_COLUMNS_BY_ID,
  type PriorityItem,
} from "./priorities-model";
import { priorityPath } from "./priority-routes";
import { columnDefs } from "./priority-table-model";

/** Everything a row of the priorities table can do, which the page holds and the cells call,
 *  and the box it is drawn in. */
export interface PriorityRowActions {
  /** Whether this reader may write a priority: the add, the description and the drag are
   *  theirs alone, and every other reader sees the same rows without them. A title is renamed
   *  on the priority's own page, which the title links to. */
  readonly canWrite: boolean;
  /** Whether the order on screen is the order itself, which is the only view a drag can move:
   *  under another sort or a grouping, a drop would write a place the reader cannot see. */
  readonly canReorder: boolean;
  /** The priority whose row is open, which shows its whole description. */
  readonly openId: string | undefined;
  readonly onOpen: (item: PriorityItem) => void;
  /** Writes `description` as the priority's description, over `from`, the server's text the
   *  description's editor last took in, which the write names as the text it goes over unless a
   *  write of the page's own goes before it (`describe`), made on `on` (`PriorityDescription`'s
   *  `onSave`). Answers whether it landed. */
  readonly onDescribe: (
    item: PriorityItem,
    description: string,
    from: string,
    on: string
  ) => Promise<boolean>;
  /** The reader's texts kept beside a priority's description while it shows another person's,
   *  where the page reads `held`, and the change of them: held in the write store
   *  (`usePriorityWrites`) rather than by the editor, which a row scrolling away takes with it.
   *  A row holds no title's texts: the title is written on the priority's own page. */
  readonly kept: (id: string, held: string) => readonly string[];
  /** What the write of `text`, one of those kept texts of priority `id`, was made on, where the
   *  page holds that write (`usePriorityWrites`'s `keptMadeOn`). */
  readonly keptMadeOn: (id: string, text: string) => string | undefined;
  readonly onKeep: (id: string, texts: readonly string[]) => void;
  /** The row the reader is dragging, and the two ends of that drag. */
  readonly dragging: string | undefined;
  readonly onDragStart: (id: string) => void;
  readonly onDragEnd: () => void;
  /** One place up or down, which is the drag done from the keyboard. */
  readonly onMove: (item: PriorityItem, by: -1 | 1) => void;
  /** Whether the table stands in a phone's box (`useTableLayout`'s `compact`), where it draws
   *  the place and the title alone (`PRIORITY_PHONE_HIDDEN`), and the title's cell names what
   *  the other columns hold. */
  readonly compact: boolean;
}

/**
 * What the rows of the table can do, for its cells to read (`PrioritiesTable` provides it). It is
 * read here rather than built into the column declarations: new declarations bring new cell
 * renderers, which TanStack draws as components of their own (`flexRender`), so each change of
 * what a row can do - and each read of the priorities changes the order - would draw every cell
 * afresh, an editor the reader is typing in among them, which then never sees another person's
 * text arrive under it.
 */
export const PriorityRowActionsContext = createContext<PriorityRowActions | null>(null);

function useRowActions(): PriorityRowActions {
  const actions = useContext(PriorityRowActionsContext);
  if (actions === null) throw new Error("a cell of the priorities table is drawn outside it");
  return actions;
}

/** The grip a row is dragged by: two columns of three dots. */
function GripIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="h-4 w-4" fill="currentColor" viewBox="0 0 16 16">
      <circle cx="5.5" cy="3.5" r="1.5" />
      <circle cx="10.5" cy="3.5" r="1.5" />
      <circle cx="5.5" cy="8" r="1.5" />
      <circle cx="10.5" cy="8" r="1.5" />
      <circle cx="5.5" cy="12.5" r="1.5" />
      <circle cx="10.5" cy="12.5" r="1.5" />
    </svg>
  );
}

/**
 * A row's place in the one order, with the disclosure that opens the row under it: one cell at
 * every width, no wider than 80 px, in which a phone's 44 px targets stand one above the other.
 * Where the reader may drag the order, the place is the grip's words, so a row is dragged by its
 * place.
 */
function OrderCell({ item }: { item: PriorityItem }) {
  const actions = useRowActions();
  const open = actions.openId === item.priority.id;
  const place = (
    <span className={`font-semibold tabular-nums ${textSecondaryOnCanvas}`}>{item.order}</span>
  );
  return (
    <div className="flex flex-col items-start gap-1">
      {actions.canWrite && actions.canReorder ? (
        <button
          aria-label={`Move ${item.order} ${item.priority.title}: drag it, or press the up and down arrows`}
          className={`-ml-1 flex cursor-grab items-center gap-1 rounded p-0.5 active:cursor-grabbing ${textMutedOnCanvas} ${focusVisibleRing}`}
          draggable
          onDragEnd={actions.onDragEnd}
          onDragStart={(event) => {
            // Firefox starts no drag that carries no data, so the drag carries the id it moves.
            event.dataTransfer.setData("text/plain", item.priority.id);
            event.dataTransfer.effectAllowed = "move";
            actions.onDragStart(item.priority.id);
          }}
          onKeyDown={(event) => {
            if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
            event.preventDefault();
            actions.onMove(item, event.key === "ArrowUp" ? -1 : 1);
          }}
          type="button"
        >
          <GripIcon />
          {place}
        </button>
      ) : (
        place
      )}
      <button
        aria-expanded={open}
        aria-label={`${open ? "Close" : "Open"} ${item.order} ${item.priority.title}`}
        className={`-ml-1 rounded p-0.5 ${textMutedOnCanvas} ${focusVisibleRing}`}
        onClick={() => actions.onOpen(item)}
        type="button"
      >
        <ChevronIcon expanded={open} />
      </button>
    </div>
  );
}

/** The most of a description a closed row projects for its excerpt, in characters: more than two
 *  lines of the Priority column hold at the widths a window gives it, so the two-line clamp is
 *  what ends the excerpt in '…' where more follows, and where two lines would hold more, the cut
 *  ends it so. */
const EXCERPT_MAX = 400;

/** The three counts a phone names under the title, by their columns' ids, with the field of the
 *  row each reads and its header's words, looked up once. */
const PHONE_COUNTS = (
  [
    ["in-flight", "inFlight"],
    ["waiting", "waiting"],
    ["backlog", "backlog"],
  ] as const
).map(([column, field]) => ({ column, field, header: PRIORITY_COLUMNS_BY_ID.get(column)?.header }));

function TitleCell({ item }: { item: PriorityItem }) {
  const actions = useRowActions();
  const open = actions.openId === item.priority.id;
  const {
    description,
    description_tree: descriptionTree,
    description_tree_exact: descriptionTreeExact,
    id,
    title,
  } = item.priority;
  const keptDescription = actions.kept(id, description);
  // An open row draws its whole description, and so does a closed one that keeps the reader's
  // text beside it: the notice of a write another person's change refused is on the row whether
  // it is open or not.
  const whole = open || keptDescription.length > 0;
  // A closed row's excerpt: the description's start, in plain words on one line - its paragraphs
  // run together, its markdown read rather than shown, and a `dispatch://` reference written as
  // its short form - which the projection every plain-text place takes of a stored text gives
  // (`useMarkdownHeadline`). Its words are those of the tree the server read the description into
  // (`description_tree`), so no reader's browser parses it, and its markdown's where the answer
  // carries no tree. It is drawn once the projection has answered.
  const excerpted = !whole && description !== "";
  const excerpt = useMarkdownHeadline(
    excerpted ? descriptionTree : undefined,
    EXCERPT_MAX,
    excerpted ? description : undefined
  );
  return (
    <div className="min-w-0 space-y-1">
      {/* The title is read here and renamed on the priority's own page, which it opens: a
          click, or `o` and Enter on the focused row, which open the row's `data-issue-link`.
          It wraps whole, however long, in the type of the Issues table's titles. */}
      <Link
        className={`flex w-full min-w-0 ${linkText} ${linkHoverText}`}
        data-issue-link
        to={priorityPath(item.priority.id)}
      >
        <span className={`min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}>{title}</span>
      </Link>
      {whole ? (
        <PriorityDescription
          canWrite={actions.canWrite}
          description={description}
          descriptionTree={descriptionTree}
          descriptionTreeExact={descriptionTreeExact}
          kept={keptDescription}
          keptMadeOn={(text) => actions.keptMadeOn(id, text)}
          label={`Description of ${item.order}`}
          onKeptChange={(texts) => actions.onKeep(id, texts)}
          onSave={(next, from, on) => actions.onDescribe(item, next, from, on)}
        />
      ) : excerpt === undefined || excerpt.text === "" ? null : (
        // Muted, in at most two lines that end in '…' where it runs on; the whole excerpt is its
        // tooltip, and the open row shows the whole description in its editor.
        <p className={`line-clamp-2 break-words text-xs ${textMutedOnCanvas}`} title={excerpt.text}>
          {excerpt.text}
        </p>
      )}
      {actions.compact ? <PhoneLine item={item} /> : null}
    </div>
  );
}

/** What a phone names under a priority's title, in place of the columns it does not draw: the
 *  owner, the three counts under their headers' words, and the newest event, each whole on a
 *  line that wraps between them. */
function PhoneLine({ item }: { item: PriorityItem }) {
  const owner = item.priority.owner.trim();
  const parts = [
    ...(owner === "" ? [] : [<span key="owner">{owner}</span>]),
    ...PHONE_COUNTS.map(({ column, field, header }) => (
      <span className={`whitespace-nowrap ${countTone(item[field])}`} key={column}>
        {`${header} ${item[field]}`}
      </span>
    )),
    <NewestEvent at={item.newest} key="newest" />,
  ];
  return (
    <p className={`break-words text-xs ${textSecondaryOnCanvas}`}>
      {parts.map((part, at) => (
        <Fragment key={part.key}>
          {at === 0 ? null : " · "}
          {part}
        </Fragment>
      ))}
    </p>
  );
}

/** When anything under the priority last moved, or the words for never. */
function NewestEvent({ at }: { at: string | null }) {
  return at === null ? (
    <span className={`text-xs ${textMutedOnCanvas}`}>{NEVER_MOVED}</span>
  ) : (
    <Timestamp at={at} className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`} />
  );
}

/** The owner, whole: an owner is free text, often a list of who holds the priority, and the
 *  reader reads all of it, so it wraps in its column and nothing cuts it. */
function OwnerCell({ owner }: { owner: string }) {
  return <span className={`block break-words ${textSecondaryOnCanvas}`}>{owner}</span>;
}

/** A count's figures, each as wide as the next, and muted where the count is zero. */
const countTone = (value: number): string => `tabular-nums ${value === 0 ? textMutedOnCanvas : ""}`;

/** A count, at its column's right edge under its header's words (`numeric`). */
const countCell = (value: number): ReactNode => (
  <span className={`block text-right ${countTone(value)}`}>{value}</span>
);

/** How each column of the priorities table draws. Everything else about a column — its width,
 *  its filter, what it groups by — is its declaration's (`PRIORITY_COLUMNS`). Built once: what a
 *  row can do is the cells' to read (`PriorityRowActionsContext`). */
export const PRIORITY_COLUMN_DEFS: ColumnDef<PriorityItem>[] = columnDefs<PriorityItem>(
  PRIORITY_COLUMNS,
  {
    order: {
      accessorFn: (item) => item.place,
      cell: ({ row }) => <OrderCell item={row.original} />,
    },
    title: {
      accessorFn: (item) => item.priority.title,
      cell: ({ row }) => <TitleCell item={row.original} />,
    },
    owner: {
      accessorFn: (item) => item.priority.owner,
      cell: ({ row }) => <OwnerCell owner={row.original.priority.owner} />,
    },
    "in-flight": {
      accessorFn: (item) => item.inFlight,
      cell: ({ row }) => countCell(row.original.inFlight),
    },
    waiting: {
      accessorFn: (item) => item.waiting,
      cell: ({ row }) => countCell(row.original.waiting),
    },
    backlog: {
      accessorFn: (item) => item.backlog,
      cell: ({ row }) => countCell(row.original.backlog),
    },
    closed: {
      accessorFn: (item) => item.closed,
      cell: ({ row }) => countCell(row.original.closed),
    },
    newest: {
      accessorFn: (item) => item.newest ?? "",
      cell: ({ row }) => <NewestEvent at={row.original.newest} />,
    },
  },
  PRIORITY_COLUMNS_BY_ID
);
