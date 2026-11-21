import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useState } from "react";

import { usersQuery } from "../../api/queries";
import { AnchoredPanel } from "../../components/AnchoredPanel";
import { useTooltips } from "../../components/tooltips";
import {
  borderDefault,
  card,
  focusBorder,
  inputClasses,
  linkText,
  menuItemActiveBg,
  pageHairBorder,
  pageInkText,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageRowHoverBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";

/**
 * Each colour the picker draws inside `NoTooltips`, the new issue page's, by the role its colour
 * has on today's pages: the panel is the page's paper inside its hairline rule, the box the
 * page's field, which keeps the application's focus rule, a person the ink, the mark grey under
 * the pointer, what qualifies a person the muted grey, the wait for the people the quiet grey,
 * and the tick of the person asked now the ink.
 */
const ON_PAGE = {
  field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  frame: `${pageHairBorder} ${pagePaperBg}`,
  loading: pageQuietText,
  person: `${pageInkText} ${pageRowHoverBg} focus-visible:bg-page-mark`,
  qualifier: pageMutedText,
  tick: pageInkText,
} as const;

/**
 * What the card's **Hand this question to…** opens, under the card's split button: everyone who
 * has signed in (`usersQuery`, read when the picker opens and kept for the session), with the
 * assignee of the ask's own issue first, since handing it back to them is what lets the ask
 * follow its issue again. Typing narrows the list; Enter hands the question to the first person
 * left, and the person it waits on now is ticked. Inside `NoTooltips` it draws in the page's
 * colours (`ON_PAGE`), and a login, a value, breaks anywhere rather than be cut short, since no
 * tooltip there gives back what an ellipsis cut.
 */
export function AskHandOnPicker({
  anchorRef,
  assignee,
  onClose,
  onPick,
  waitingOn,
}: {
  anchorRef: RefObject<HTMLElement | null>;
  /** The assignee of the ask's issue, where the card has it (only an Inbox row carries one). */
  assignee: string | null;
  onClose: () => void;
  onPick: (login: string) => void;
  waitingOn: string;
}): ReactNode {
  const tooltips = useTooltips();
  const people = useQuery(usersQuery());
  const [filter, setFilter] = useState("");
  const logins = (people.data ?? []).map((user) => user.login);
  const ordered = [
    ...(assignee === null ? [] : [assignee]),
    ...logins.filter((login) => login !== assignee),
  ];
  const needle = filter.trim().toLowerCase();
  const shown = needle === "" ? ordered : ordered.filter((login) => login.includes(needle));
  const pick = (login: string) => {
    onClose();
    if (login !== waitingOn) onPick(login);
  };
  return (
    <AnchoredPanel
      anchorRef={anchorRef}
      className={`z-50 w-64 rounded-lg border p-1 shadow-lg outline-none ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.frame}`}
      flip
      label="Hand this question to"
      onDismiss={onClose}
      role="dialog"
    >
      <input
        aria-label="Hand this question to"
        className={`mb-1 block min-h-9 w-full rounded-md border px-2.5 text-sm outline-none ${tooltips ? inputClasses(true) : ON_PAGE.field}`}
        onChange={(event) => setFilter(event.target.value)}
        onKeyDown={(event) => {
          const first = shown[0];
          if (event.key !== "Enter" || first === undefined) return;
          event.preventDefault();
          pick(first);
        }}
        placeholder="Hand this question to…"
        value={filter}
      />
      {people.isPending ? (
        <p className={`px-2.5 py-2 text-sm ${tooltips ? textMutedOnSurface : ON_PAGE.loading}`}>
          Loading…
        </p>
      ) : (
        <ul className="max-h-64 overflow-auto">
          {shown.map((login) => (
            <li key={login}>
              <button
                className={`flex min-h-11 w-full items-center justify-between gap-3 rounded-md px-2.5 text-left text-sm outline-none sm:min-h-9 ${tooltips ? `${textSecondaryOnSurface} ${menuItemActiveBg}` : ON_PAGE.person}`}
                data-ask-hand-on-person={login}
                onClick={() => pick(login)}
                type="button"
              >
                <span
                  className={tooltips ? "min-w-0 truncate" : "min-w-0 [overflow-wrap:anywhere]"}
                >
                  {login}
                  {login === assignee ? (
                    <span className={tooltips ? textMutedOnSurface : ON_PAGE.qualifier}>
                      {" "}
                      · assignee
                    </span>
                  ) : null}
                </span>
                {login === waitingOn ? (
                  <span
                    aria-label="Asked of now"
                    className={`font-semibold ${tooltips ? linkText : ON_PAGE.tick}`}
                    role="img"
                  >
                    ✓
                  </span>
                ) : null}
              </button>
            </li>
          ))}
        </ul>
      )}
    </AnchoredPanel>
  );
}
