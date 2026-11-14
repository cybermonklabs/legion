import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useState } from "react";

import { usersQuery } from "../../api/queries";
import { AnchoredPanel } from "../../components/AnchoredPanel";
import {
  borderDefault,
  card,
  inputClasses,
  linkText,
  menuItemActiveBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";

/**
 * What the card's **Hand this question to…** opens, under the card's split button: everyone who
 * has signed in (`usersQuery`, read when the picker opens and kept for the session), with the
 * assignee of the ask's own issue first, since handing it back to them is what lets the ask
 * follow its issue again. Typing narrows the list; Enter hands the question to the first person
 * left, and the person it waits on now is ticked.
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
      className={`z-50 w-64 rounded-lg border p-1 shadow-lg outline-none ${card} ${borderDefault}`}
      flip
      label="Hand this question to"
      onDismiss={onClose}
      role="dialog"
    >
      <input
        aria-label="Hand this question to"
        className={`mb-1 block min-h-9 w-full rounded-md border px-2.5 text-sm outline-none ${inputClasses(true)}`}
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
        <p className={`px-2.5 py-2 text-sm ${textMutedOnSurface}`}>Loading…</p>
      ) : (
        <ul className="max-h-64 overflow-auto">
          {shown.map((login) => (
            <li key={login}>
              <button
                className={`flex min-h-11 w-full items-center justify-between gap-3 rounded-md px-2.5 text-left text-sm outline-none sm:min-h-9 ${textSecondaryOnSurface} ${menuItemActiveBg}`}
                data-ask-hand-on-person={login}
                onClick={() => pick(login)}
                type="button"
              >
                <span className="min-w-0 truncate">
                  {login}
                  {login === assignee ? (
                    <span className={textMutedOnSurface}> · assignee</span>
                  ) : null}
                </span>
                {login === waitingOn ? (
                  <span
                    aria-label="Asked of now"
                    className={`font-semibold ${linkText}`}
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
