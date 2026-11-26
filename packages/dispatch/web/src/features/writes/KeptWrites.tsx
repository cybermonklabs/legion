import type { ReactNode } from "react";

import {
  dangerText,
  secondaryButtonCompact,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";
import type { KeptWrite } from "./write-storage";
import { useWrites, type Write } from "./write-store";

/** Where focus goes when a Discard takes its button off the page: the next Discard button in the
 *  list (else the one before it), and once none is left, the next control after the list or the
 *  sign-in page's link before it (`SignInPage` in `app.tsx`), so a keyboard reader does not land
 *  on the page body. */
function nextControlAfterDiscard(button: HTMLButtonElement): HTMLElement | null {
  const section = button.closest("section");
  if (section === null) return null;
  const buttons = [...section.querySelectorAll<HTMLButtonElement>("button")];
  const index = buttons.indexOf(button);
  const sibling = buttons[index + 1] ?? buttons[index - 1];
  if (sibling !== undefined) return sibling;
  const scope = section.parentElement?.closest("main") ?? section.parentElement;
  if (scope === null) return null;
  const controls = [
    ...scope.querySelectorAll<HTMLElement>("a[href], button:not([disabled])"),
  ].filter((control) => !section.contains(control));
  const after = controls.find(
    (control) => (section.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
  );
  return after ?? controls.at(-1) ?? null;
}

/**
 * What a lapsed session keeps, on the sign-in page: the person's writes by name, under the login
 * whose sign-in takes them up again (`Writes.signedIn`), so they see what waits for them; one
 * Dispatch refused offers Discard, since only they can drop it. Nothing while the page keeps none,
 * as after a sign-out. The writes the reload kept that the store has not taken up yet are listed
 * too, from their own records (`Writes.notTakenUp`), so the list needs no kind of write: until
 * the store takes one up it shows by its name and by where its record says it stood - one kept
 * refused reads "Not saved" and offers Discard, which drops its record (`Writes.discardKept`) -
 * and from then on as the store's own row. A page that loads signed out holds only such records:
 * the store takes up nothing until someone signs in (`app.tsx`). Its host reads both, `rows` of
 * the store's and `records` of what the reload kept, to know whether to draw the list at all
 * (`KeptWritesHost`), and hands them in.
 */
export function KeptWrites({
  records,
  rows,
}: {
  records: readonly KeptWrite[];
  rows: readonly Write[];
}): ReactNode {
  const writes = useWrites();
  const shown = [...rows, ...records].sort((a, b) => a.pressedAt - b.pressedAt);
  if (shown.length === 0) return null;
  const logins = [...new Set(shown.map((write) => write.login))];
  return (
    <section aria-label="Kept changes" className="mt-6 space-y-3 text-sm">
      {logins.map((login) => (
        <div key={login}>
          <p className={textSecondaryOnSurface}>Kept until you sign in again as {login}:</p>
          <ul className="mt-2 space-y-1">
            {shown
              .filter((write) => write.login === login)
              .map((write) => (
                <li
                  className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1"
                  data-kept-write={write.key}
                  key={write.key}
                >
                  {("id" in write ? write.status : write.state.status) === "refused" ? (
                    <>
                      <span className={`min-w-0 flex-1 ${dangerText}`}>
                        {write.label} — Not saved
                      </span>
                      <button
                        className={secondaryButtonCompact}
                        onClick={(event) => {
                          const button = event.currentTarget;
                          // The focus goes on only after a key pressed the button. Chromium
                          // focuses a button that the pointer presses and draws it no focus
                          // ring, so a hand-off would put the focus, unseen, on the next row's
                          // Discard, and the next Space or Enter would discard that write too
                          // (`usePanelFocusReturn` in `PendingWritesIndicator.tsx`).
                          const next =
                            document.activeElement === button && button.matches(":focus-visible")
                              ? nextControlAfterDiscard(button)
                              : null;
                          const dropped =
                            "id" in write
                              ? writes.discard(write.id)
                              : writes.discardKept(write.key);
                          if (dropped) next?.focus({ preventScroll: true });
                        }}
                        type="button"
                      >
                        Discard
                      </button>
                    </>
                  ) : (
                    <span className="min-w-0 flex-1">{write.label}</span>
                  )}
                </li>
              ))}
          </ul>
        </div>
      ))}
    </section>
  );
}
