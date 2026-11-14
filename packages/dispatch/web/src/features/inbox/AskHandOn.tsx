import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useState } from "react";

import { usersQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { badgeLow, badgePrimary, textMutedOnSurface } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "../issue/badge-select";
import { Timestamp } from "../refs/Timestamp";
import { AskHandOnLiveContext } from "./hand-on-live";
import { askWaitingOnPerson } from "./list-row";
import { useAskHandOn } from "./useAskHandOn";

/**
 * Who an open ask waits on, and the one control that hands it to somebody else. It sits in the
 * card rather than in the Inbox, so every surface that draws an open ask has it: the Inbox row
 * and its reading pane, the issue page, a document's margin, a document's ask block and
 * `ThreadBody`.
 *
 * The badge is the server's own answer (`waiting_on_person`: whoever the ask was handed to,
 * else the assignee of its issue, else the default assignee of its project, which every project
 * names), and the picker over it is the same
 * invisible native `<select>` the assignee and priority pickers use, so the menu is one tap on
 * a phone and one keyboard interaction on a desktop. Its people are everyone who has signed in
 * (`usersQuery`, read only once the reader reaches for the control and then kept for the
 * session), with the assignee of the ask's own issue offered first where the card has it -
 * `assignee` is the Inbox row's `issue.assignee`, the one read that hydrates the issue, and
 * null on every other surface - since handing the ask back to them is what empties the stored
 * field and lets the ask follow its issue again. Where it is null that person is still in the
 * people list, under their own login: the named choice is a label, not the only way to pick
 * them, and the control's own value comes from the ask itself.
 *
 * Under it, where somebody has already handed the ask on, who did it and when
 * (`asked_of_by`, `asked_of_at`). A refusal shows the server's reason with Retry where the
 * reader pressed it, as `AssignToMe` and `SnoozeControl` do - and, as those two do, it asks
 * the list drawing it to hold the ask while that write is live (pending or failed), because
 * the optimistic `waiting_on_person` takes the row out of the Inbox's own list the moment the
 * write starts (`isMine`, `inAssigneeFilter`): without the hold this card would be unmounted
 * before the server answered, and a refusal would put the row back with a fresh card that
 * never failed, so the reader would see it return and no reason. The hold comes from the list
 * itself, through the context it provides (`hand-on-live.ts`), rather than from a prop every
 * host passes down: the card the reader pressed can be one the Inbox never rendered itself -
 * the decision block its reading column draws through the document page - and a prop that
 * path does not carry is a hold that path does not get. What the list does with the request
 * is the list's own: it holds an ask it already listed a row for and ignores one it did not,
 * since a card can be drawn inside a list that lists no row for its ask - a Reply-thread
 * card, a sibling ask block - and holding that ask would pull it into the reader's list. A
 * card drawn where no list can drop it registers with nobody.
 */
export function AskHandOn({ ask, assignee }: { ask: Ask; assignee: string | null }): ReactNode {
  const [wanted, setWanted] = useState(false);
  const people = useQuery({ ...usersQuery(), enabled: wanted });
  const want = () => setWanted(true);
  const write = useAskHandOn(ask);
  const onLive = useContext(AskHandOnLiveContext);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live) return;
    onLive(ask.id, true);
    return () => onLive(ask.id, false);
  }, [ask.id, live, onLive]);
  const waitingOn = askWaitingOnPerson(ask) ?? "";
  // The list is drawn only once the reader reaches for the control, as it is read: the query
  // keeps its people for the session, so a card that built its options from whatever another
  // control had already fetched would draw one `<option>` per signed-in person for every open
  // ask on the page.
  const logins = (wanted ? (people.data ?? []) : [])
    .map((user) => user.login)
    .filter((login) => login !== assignee);
  // Whoever holds the ask stays selectable even where the people list does not carry them (it
  // is unread until the reader reaches for the control), so the select never shows a value its
  // options lack.
  const others =
    waitingOn === "" || waitingOn === assignee || logins.includes(waitingOn)
      ? logins
      : [waitingOn, ...logins];
  const handedOnBy = ask.asked_of_by ?? "";
  const handedOnAt = ask.asked_of_at ?? null;
  const tone = waitingOn === "" ? badgeLow : badgePrimary;
  return (
    <div
      className={`mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs ${textMutedOnSurface}`}
    >
      <span className={badgeSelectWrapper}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${tone.bg} ${tone.text}`}>
          {write.pending
            ? "Handing on…"
            : waitingOn === ""
              ? "Asked of nobody"
              : `Asked of ${waitingOn}`}
        </span>
        <select
          aria-label="Hand this question to"
          className={badgeSelectOverlay}
          data-ask-hand-on={ask.id}
          onChange={(event) => {
            const { value } = event.target;
            if (value !== "") write.submit(value);
          }}
          onFocus={want}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          value={waitingOn}
        >
          {/* Selected only where the server names no person, which only a Dispatch older than
              every project naming a default answers. Nobody is not a pick: the route takes a
              person's email and refuses anything else. */}
          <option disabled value="">
            Nobody
          </option>
          {assignee === null ? null : (
            <option value={assignee}>{assignee} (the issue's assignee)</option>
          )}
          {others.map((login) => (
            <option key={login} value={login}>
              {login}
            </option>
          ))}
        </select>
      </span>
      {handedOnBy === "" || handedOnAt === null ? null : (
        <span>
          handed on by {handedOnBy} <Timestamp at={handedOnAt} />
        </span>
      )}
      {write.failed ? (
        <span className="basis-full">
          <QueryError
            message={write.error ?? "Could not hand this question on."}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </span>
      ) : null}
    </div>
  );
}
