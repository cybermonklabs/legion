import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

import {
  borderDefault,
  dangerText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
} from "../../theme/classes";
import { useUserPreference } from "../shell/userPreference";

/** One property of the issue in Details: its name and its value. */
export interface HeaderDetailsRow {
  /** Names the row for a test (`data-testid`), and keys it. */
  readonly id: string;
  readonly label: string;
  readonly value: ReactNode;
}

type ReportRefusal = (refusal: string | null) => void;

/** The reporter of the row a control is drawn in, which Details gives each row's value. */
const RowRefusal = createContext<ReportRefusal | null>(null);

/** Says, from a control drawn in a row of Details, the words of a write of it that Dispatch
 *  refused, or of a value it refused, while they stand, and null once none does: Details says them
 *  under the header while it is closed. A stopped write is no refusal. Drawn anywhere else, the
 *  control says them in its own line alone. */
export function useDetailsRefusal(refusal: string | null): void {
  const report = useContext(RowRefusal);
  useEffect(() => {
    report?.(refusal);
  }, [report, refusal]);
}

const readOpen = (stored: string | null) => stored === "open";
const writeOpen = (open: boolean) => (open ? "open" : "closed");

/**
 * The end of today's issue header: the row that holds `leading` (the header's labels), then
 * `trailing` (its actions) and the one disclosure, Details, at the row's right end, then the
 * panel. Open, the panel lists every property of the issue a reader does not act on in the header
 * itself, as a definition list of a name and a value per row (`rows`), and then `after`, the
 * panels a row's own control opens (who is subscribed, what references the issue).
 *
 * The groups of the row, in this order: Labels (`leading`), Actions (`trailing`) and Details.
 * When the row is too narrow, Actions and Details move to the next line together and whole, and
 * stay at its right end.
 *
 * The panel lays its rows out by its own width, not the viewport's: one row per line, and two per
 * line from 56 rem (about 900 px), since the header's width at one viewport depends on the
 * sidebar and the margin beside it. In a row the value sits beside its name while there is room
 * for 14 rem of it, and under it otherwise, and a value wraps inside its own cell.
 *
 * Whether Details is open is the reader's: closed until they open it, and then kept open for them
 * in this browser (`issue.details`), on every issue, until they close it. Closed, the panel stays
 * mounted and hidden, so each row keeps what it holds - a write in flight, a refusal - and shows
 * it again when the reader opens Details. `onClose` runs when the reader closes it: the route and
 * parent editors give up an edit not saved, as their Cancel does, and the Advances add form
 * closes with its pick; the repository box has saved what was typed as it lost focus to the
 * press, and a labels or components picker saves its staged set on any press outside it.
 *
 * While Details is closed, the refusal a row's control holds (`useDetailsRefusal`) is said under
 * the header's last line, named by its row (`Repository: …`), as the route's and the parent's
 * are: a write refused out of sight would otherwise roll back with no word anywhere. A failed
 * read is not a refusal and stays in its row.
 */
export function HeaderDetails({
  after,
  leading,
  onClose,
  rows,
  trailing,
}: {
  after?: ReactNode;
  leading: ReactNode;
  onClose?: () => void;
  rows: readonly HeaderDetailsRow[];
  trailing?: ReactNode;
}): ReactNode {
  const [open, setOpen] = useUserPreference("issue.details", readOpen, writeOpen);
  const panelId = useId();
  // Each row's refusal by its id, and one reporter per id, so a control's report is the same
  // function on every render and its effect runs only when its refusal changes.
  const [refusals, setRefusals] = useState<Readonly<Record<string, string>>>({});
  const reporters = useRef(new Map<string, ReportRefusal>());
  const reporterOf = (id: string): ReportRefusal => {
    let report = reporters.current.get(id);
    if (report === undefined) {
      report = (refusal) =>
        setRefusals((current) => {
          if ((current[id] ?? null) === refusal) return current;
          const { [id]: _gone, ...rest } = current;
          return refusal === null ? rest : { ...rest, [id]: refusal };
        });
      reporters.current.set(id, report);
    }
    return report;
  };
  const refused = open ? [] : rows.filter((row) => refusals[row.id] !== undefined);
  return (
    <>
      <div className="flex min-w-0 flex-wrap items-end justify-between gap-x-4 gap-y-2">
        {leading}
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {trailing}
          <button
            aria-controls={panelId}
            aria-expanded={open}
            className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            onClick={() => {
              if (open) onClose?.();
              setOpen(!open);
            }}
            type="button"
          >
            Details
          </button>
        </div>
      </div>
      {refused.length === 0 ? null : (
        <div className="mt-2 min-w-0 space-y-1" data-testid="issue-details-refusals">
          {refused.map((row) => (
            <p
              className={`text-sm [overflow-wrap:anywhere] ${dangerText}`}
              key={row.id}
              role="alert"
            >
              {row.label}: {refusals[row.id]}
            </p>
          ))}
        </div>
      )}
      <div
        className={`@container mt-3 min-w-0 border-t pt-3 ${borderDefault}`}
        data-testid="issue-details"
        hidden={!open}
        id={panelId}
      >
        <dl className="grid min-w-0 grid-cols-1 gap-x-6 gap-y-2 @min-[56rem]:grid-cols-2">
          {rows.map((row) => (
            <div
              className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-1"
              data-testid={`issue-details-${row.id}`}
              key={row.id}
            >
              <dt className={`w-32 shrink-0 text-sm font-medium ${textMutedOnSurface}`}>
                {row.label}
              </dt>
              <dd className="min-w-0 flex-[1_1_14rem] text-sm [overflow-wrap:anywhere]">
                <RowRefusal.Provider value={reporterOf(row.id)}>{row.value}</RowRefusal.Provider>
              </dd>
            </div>
          ))}
        </dl>
        {after}
      </div>
    </>
  );
}
