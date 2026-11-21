import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useContext, useState } from "react";

import { usersQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { useHeldThroughAnswer } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { usePageWords } from "../../components/tooltips";
import {
  badgeLow,
  badgePrimary,
  pageLabelChip,
  pagePaneBg,
  pageQuietText,
  textMutedOnSurface,
} from "../../theme/classes";
import {
  badgeReachHandlers,
  badgeSelectBadge,
  badgeSelectPageBadge,
  badgeSelectPageTrigger,
  badgeSelectPageWrapper,
  badgeSelectTrigger,
  badgeSelectWrapper,
} from "../issue/badge-select";
import { Timestamp } from "../refs/Timestamp";
import { useLiveRegistration } from "../writes/useKeyedWrite";
import { AskHandOnLiveContext } from "./hand-on-live";
import { inboxActionMark } from "./inbox-action-mark";
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
 * names), and it is the trigger of the shared list's single pick (`MultiSelect`), whose text box
 * narrows the people as the reader types: a pick of somebody else is the hand-on, in one step,
 * and a pick of the person it already waits on writes nothing, as a `<select>` set to the value
 * it holds fires nothing. Its closed trigger takes the list's keys (`keysOnTrigger`), so an
 * arrow, Home, End, Page Up, Page Down or a letter there only opens the list and moves its
 * highlight - never a write, and never the Inbox's own key for that letter - and only Enter or a
 * click on a person hands the question on. Its people are everyone who has signed in
 * (`usersQuery`, read only once the reader reaches for the control and then kept for the
 * session), with the assignee of the ask's own issue offered first where the card has it -
 * `assignee` is the Inbox row's `issue.assignee`, the one read that hydrates the issue, and null
 * on every other surface - since handing the ask back to them is what empties the stored field
 * and lets the ask follow its issue again. Where it is null that person is still in the people
 * list, under their own login: the named choice is a label, not the only way to pick them, and
 * the control's own value comes from the ask itself.
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
  const [open, setOpen] = useState(false);
  const write = useAskHandOn(ask);
  const page = usePageWords();
  const onLive = useContext(AskHandOnLiveContext);
  // The ask held is the one the live hand-on is of, which is the card's own unless a host has
  // drawn the card for another ask since that press (`useAskHandOn`).
  useLiveRegistration(write.liveAskId, onLive);
  // The trigger is held (`aria-disabled`, which keeps the focus on it) from the pick until the
  // commit after the write's answer (`useHeldThroughAnswer`): the one in which the list lets go of
  // the ask (the registration above) and, where the hand-on takes the ask out of the reader's
  // list, takes the row and this card away. A refusal lets go of the trigger the commit after its
  // answer, with Retry beside it. While held, the list does not open.
  const held = useHeldThroughAnswer(write.pending);
  const waitingOn = askWaitingOnPerson(ask) ?? "";
  // The people are listed only while the list stands open, which is after the reader reached for
  // the control and they were asked for: the query keeps its people for the session, so a card
  // that built its options from whatever another control had already fetched would map every
  // signed-in person on every render of every open ask on the page.
  const logins = (open ? (people.data ?? []) : [])
    .map((user) => user.login)
    .filter((login) => login !== assignee);
  // Whoever holds the ask stays in the list even where the people list does not carry them (it
  // is unread until the reader reaches for the control), so the list always holds the value it
  // marks.
  const others =
    waitingOn === "" || waitingOn === assignee || logins.includes(waitingOn)
      ? logins
      : [waitingOn, ...logins];
  const options = assignee === null ? others : [assignee, ...others];
  const handedOnBy = ask.asked_of_by ?? "";
  const handedOnAt = ask.asked_of_at ?? null;
  const tone = waitingOn === "" ? badgeLow : badgePrimary;
  // Inside `NoTooltips` each colour is one of the new issue page's: the line is its quiet words,
  // and the badge its label chip, or the quiet words on the same ground while `waitingOn` is
  // empty (the ask names no login), as the assignee's badge is drawn there.
  const badgeTone =
    page === null
      ? `${tone.bg} ${tone.text}`
      : waitingOn === ""
        ? `${pagePaneBg} ${pageQuietText}`
        : pageLabelChip;
  // Inside `NoTooltips` who the ask waits on and who handed it on are values, so each breaks
  // anywhere and the badge's wrapper and the trigger in it narrow to the line rather than keep
  // their width; the trigger still covers the badge however many lines it takes
  // (`badgeSelectPageTrigger`).
  return (
    <div
      className={`mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs ${page === null ? textMutedOnSurface : pageQuietText}`}
    >
      {/* The wrapper is the tap target with the focus ring, and the badge inside it is the
          list's trigger. */}
      <span
        className={page === null ? badgeSelectWrapper : badgeSelectPageWrapper}
        data-ask-hand-on={ask.id}
        {...badgeReachHandlers(() => setWanted(true))}
      >
        <MultiSelect
          ariaDisabled={held}
          chevron={false}
          emptyMessage={people.isPending ? "Loading people…" : "Nobody matches that."}
          error={people.isError ? "Could not load the people list." : null}
          keysOnTrigger
          label="Hand this question to"
          // The Inbox's command bar presses the trigger by this mark (its Hand on action).
          marks={{ trigger: inboxActionMark("inbox.hand-on") }}
          mode="single"
          onOpenChange={(next) => setOpen(next && !held)}
          onPick={(login) => {
            if (login !== waitingOn) write.submit(login);
          }}
          open={open}
          optionLabel={(login) => (login === assignee ? `${login} (the issue's assignee)` : login)}
          options={options}
          searchLabel="Search people"
          triggerClassName={page === null ? badgeSelectTrigger : badgeSelectPageTrigger}
          // The server's own answer, `""` where it names nobody, which only a Dispatch older
          // than every project naming a default answers: no option is that value, and nobody is
          // no pick (the route takes a person's email and refuses anything else), but the
          // badge's words still name the trigger after the list's name.
          value={waitingOn}
        >
          <span
            className={`${page === null ? badgeSelectBadge : badgeSelectPageBadge} ${badgeTone}`}
          >
            {write.pending
              ? "Handing on…"
              : waitingOn === ""
                ? "Asked of nobody"
                : `Asked of ${waitingOn}`}
          </span>
        </MultiSelect>
      </span>
      {handedOnBy === "" || handedOnAt === null ? null : (
        <span className={page === null ? undefined : "min-w-0 max-w-full [overflow-wrap:anywhere]"}>
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
