import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import { ApiError, api, apiErrorMessage } from "../../api/client";
import { projectAccessQuery } from "../../api/queries";
import type { ProjectAccess } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import {
  borderDefault,
  inputClasses,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { exactTimeLabel, Timestamp } from "../refs/Timestamp";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** One enrolled passkey as the owner's access answer lists it. */
type EnrolledPasskey = NonNullable<ProjectAccess["passkeys"]>[number];

/** The link a press of Invite made: the outside origin's `/enroll#<token>`, when Dispatch made it
 *  and when it stops, both on Dispatch's clock, which a passkey's `enrolled_at` is read on too. */
interface Invitation {
  readonly url: string;
  readonly made_at: string;
  readonly expires_at: string;
}

/** The longest delay `setTimeout` keeps: 2^31 − 1 ms, about 24.8 days. */
const LONGEST_TIMEOUT_MS = 2_147_483_647;

/** How often the owner's page reads the access list again while a row shows a link nobody has
 *  used: no Dispatch event tells the page that the person enrolled with it, which uses it. */
const SHOWN_LINK_POLL_MS = 5_000;

/** What the owner reads when Dispatch would not make `email` a link, by its code. */
function invitationRefusal(email: string, error: Error): string {
  switch (error instanceof ApiError ? error.code : undefined) {
    case "WORKSPACE_ADDRESS":
      return `No link can be made for ${email}: it is an address of the company's Google Workspace, which signs in with Google.`;
    case "FULL_MEMBER":
      return `No link can be made for ${email}: they sign in with Google as a member of the company's Google Workspace.`;
    case "PERSON_NOT_LISTED":
      return `No link can be made for ${email}: this project's access list no longer names them directly.`;
    case "LISTED_BY_ANOTHER_OWNER":
      return `No link can be made for ${email}: another project's owner also names them, and a passkey would read that project too.`;
    case "PASSKEY_SIGNIN_OFF":
      return "This deployment no longer signs anyone in with a passkey, so no link can be made.";
    default:
      return apiErrorMessage(error, `Couldn't make a link for ${email}.`);
  }
}

/** Whether the owner, asked what a Reset of `email` does, confirms it. The row's Reset asks
 *  before its first send, and the panel's Retry of a refused Reset asks again, since that Retry
 *  is a new Reset under a new key. */
export function confirmReset(email: string): boolean {
  return window.confirm(
    `Reset ${email}? Dispatch deletes their passkeys and their unused links of every project, signs them out, and revokes their personal tokens and the device logins they approved. They sign in again only through a new link.`
  );
}

const rowClass = `flex min-h-11 flex-wrap items-center justify-between gap-x-3 gap-y-2 border-b px-4 py-2 last:border-0 ${borderDefault}`;
const linkFieldClass = `min-h-11 w-full min-w-0 rounded-md px-3 py-2 font-mono text-xs md:min-h-9 ${inputClasses(false)} ${textPrimaryOnSurface}`;

/**
 * One person on a project's access list, as its Access tab draws them: the email, whether they
 * have signed in, and the owner's Remove. Where the owner reads the list and passkey sign-in is on
 * (`passkeys` given: Dispatch answers the field to the owner alone, and only then), the row also
 * shows each passkey the person enrolled and when, so an owner sees a link someone else used
 * first; Invite, where Dispatch would make the person a link (`invitable`: named directly, from
 * outside the Workspace, no full member), which shows the link to send and when it stops; and
 * Reset, for a person with a passkey, which deletes their passkeys and their unused links of every
 * project, ends their sign-in, and revokes their personal tokens and the device logins they
 * approved.
 *
 * The link is shown only in this row, once: its token is in the answer alone, which Dispatch keeps
 * under no write key, so a later Invite makes a new link, which ends this one. A Reset deletes it,
 * so the row stops showing it once the owner confirms the Reset; and the person's enrollment uses
 * it, so the row stops showing it once the access list, which the row reads again every few
 * seconds while it shows a link, names a passkey enrolled through this project's link after the
 * link was made (`made_at`). A new link and when it stops, and a link made whose answer was lost,
 * are announced to a screen reader through status regions the row holds while Invite can be
 * pressed, since a region that mounts with its words already in it is not announced by every
 * reader.
 */
export function AccessPersonRow({
  email,
  invitable,
  listedByAnotherOwner,
  notSignedIn,
  onRemove,
  onReset,
  passkeys,
  project,
  refusal,
  refusalShownBy,
  removing,
  resetsLanded,
  resetting,
}: {
  readonly email: string;
  readonly invitable: boolean;
  /** A project of another owner names the person directly, so this owner makes them no link
   *  (409 LISTED_BY_ANOTHER_OWNER): one owner binds a person from outside. */
  readonly listedByAnotherOwner: boolean;
  readonly notSignedIn: boolean;
  readonly onRemove: () => void;
  readonly onReset: () => void;
  /** The person's passkeys, or undefined where this reader is shown none (not the owner, or
   *  passkey sign-in off). */
  readonly passkeys: readonly EnrolledPasskey[] | undefined;
  readonly project: string;
  readonly refusal: string | null;
  readonly refusalShownBy: string | undefined;
  readonly removing: boolean;
  /** How many Resets of this person landed while the panel was open: each one deleted the
   *  person's unused links, so the row lets go of the link it shows. */
  readonly resetsLanded: number;
  readonly resetting: boolean;
}): ReactNode {
  const linkId = useId();
  const [invitation, setInvitation] = useState<Invitation | undefined>(undefined);
  const [copied, setCopied] = useState(false);
  const invite = useKeyedWrite<Invitation, void>({
    inline: true,
    label: () => `Make a link for ${email} on ${project}`,
    mutationKey: ["project-invitation", project, email],
    onMutate: () => {
      setInvitation(undefined);
      setCopied(false);
    },
    onSuccess: (answer) => setInvitation(answer ?? undefined),
    method: "POST",
    write: (_none, bounds) => api.createProjectInvitation(project, email, bounds),
  });
  const reset = () => {
    if (!confirmReset(email)) {
      return;
    }
    onReset();
  };
  // A Reset that landed deleted the person's unused links with their passkeys, so the link an
  // earlier Invite showed here, or made with its answer lost, enrolls nobody now: the row stops
  // offering it. A Reset Dispatch refused, or one still out, deletes nothing, and the link stays.
  const resetsSeen = useRef(resetsLanded);
  const forgetUnansweredLink = invite.forgetUnanswered;
  useEffect(() => {
    if (resetsSeen.current === resetsLanded) return;
    resetsSeen.current = resetsLanded;
    setInvitation(undefined);
    setCopied(false);
    forgetUnansweredLink();
  }, [resetsLanded, forgetUnansweredLink]);
  // A link stops working at its expires_at: the row stops offering it then, which also ends its
  // poll, rather than show a dead link as one that "works once".
  useEffect(() => {
    if (invitation === undefined) return;
    const left = Date.parse(invitation.expires_at) - Date.now();
    if (left <= 0) {
      setInvitation(undefined);
      return;
    }
    // A browser fires a timer longer than its 32-bit limit at once; a link lasts 24 hours, far
    // under that, so a later expires_at sets none.
    if (left > LONGEST_TIMEOUT_MS) return;
    const lapse = setTimeout(() => setInvitation(undefined), left);
    return () => clearTimeout(lapse);
  }, [invitation]);
  const shown = passkeys !== undefined;
  const enrolled = passkeys ?? [];
  // A passkey enrolled through this project's link after this link was made used this link: a
  // newer link of the project ends this one, so no other link of this project enrolled it. The row
  // stops offering it. A passkey of another project's link, or one enrolled before this link (the
  // person's first device, when the link is for a second one), leaves it shown. While a project of
  // another owner names the person, the enrollment refuses the link (LISTED_BY_ANOTHER_OWNER), so
  // the row does not offer it then either, and says why beside it.
  const link =
    invitation !== undefined &&
    (listedByAnotherOwner ||
      enrolled.some(
        (passkey) =>
          passkey.project === project &&
          Date.parse(passkey.enrolled_at) > Date.parse(invitation.made_at)
      ))
      ? undefined
      : invitation;
  // While the row shows a link, the access list is read again now and then, so the passkey the
  // person enrolls with it shows here, and the used link goes, with no reload.
  useQuery({
    ...projectAccessQuery(project),
    enabled: link !== undefined,
    refetchInterval: link !== undefined ? SHOWN_LINK_POLL_MS : false,
  });
  const lostAnswer = invite.unanswered !== undefined && !invite.isPending;

  return (
    <li className={rowClass}>
      <span className={`min-w-0 break-all text-sm ${textPrimaryOnSurface}`}>
        {email}
        {notSignedIn ? (
          <span className={`ml-2 ${textSecondaryOnCanvas}`}>not signed in yet</span>
        ) : null}
      </span>
      <div className="flex flex-wrap items-center gap-2">
        {shown && invitable ? (
          <RefusableButton
            busy={invite.isPending ? "Making a link…" : undefined}
            look="secondary-compact"
            onPress={() => invite.mutate()}
            refusal={refusal}
            refusalShownBy={refusalShownBy}
          >
            Invite<span className="sr-only"> {email}</span>
          </RefusableButton>
        ) : null}
        {shown && enrolled.length > 0 ? (
          <RefusableButton
            busy={resetting ? "Resetting…" : undefined}
            look="secondary-compact"
            onPress={reset}
            refusal={refusal}
            refusalShownBy={refusalShownBy}
          >
            Reset<span className="sr-only"> {email}</span>
          </RefusableButton>
        ) : null}
        <RefusableButton
          busy={removing ? "Removing…" : undefined}
          look="secondary-compact"
          onPress={onRemove}
          refusal={refusal}
          refusalShownBy={refusalShownBy}
        >
          Remove<span className="sr-only"> {email}</span>
        </RefusableButton>
      </div>
      {shown && (invitable || enrolled.length > 0 || link !== undefined || listedByAnotherOwner) ? (
        <div className="grid basis-full gap-2">
          <p className="sr-only" data-testid="access-invitation-announcement" role="status">
            {link === undefined
              ? ""
              : `Made a link for ${email}. It works once, until ${exactTimeLabel(new Date(link.expires_at))}.`}
          </p>
          {listedByAnotherOwner ? (
            <p className={`text-sm ${textSecondaryOnSurface}`}>
              Another project's owner also names this person, so no link can be made here: a passkey
              would read that project too.
            </p>
          ) : null}
          {enrolled.length === 0 ? (
            <p className={`text-sm ${textMutedOnSurface}`}>No passkey yet</p>
          ) : (
            enrolled.map((passkey) => (
              <p
                className={`text-sm ${textSecondaryOnSurface}`}
                data-testid="access-passkey"
                key={passkey.enrolled_at}
              >
                Passkey enrolled {exactTimeLabel(new Date(passkey.enrolled_at))} (
                <Timestamp at={passkey.enrolled_at} />)
              </p>
            ))
          )}
          {link === undefined ? null : (
            <div className="grid gap-1">
              <label className={`text-sm font-medium ${textSecondaryOnSurface}`} htmlFor={linkId}>
                Enrollment link<span className="sr-only"> for {email}</span>
              </label>
              <div className="flex min-w-0 gap-2">
                <input
                  className={linkFieldClass}
                  id={linkId}
                  onFocus={(event) => event.currentTarget.select()}
                  readOnly
                  value={link.url}
                />
                {/* A page outside a secure context has no clipboard; the field still selects. */}
                {typeof navigator.clipboard?.writeText === "function" ? (
                  <RefusableButton
                    className="shrink-0"
                    look="secondary-compact"
                    onPress={() =>
                      void navigator.clipboard.writeText(link.url).then(
                        () => setCopied(true),
                        () => setCopied(false)
                      )
                    }
                    refusal={null}
                  >
                    {copied ? "Copied" : "Copy"}
                    <span className="sr-only"> the link for {email}</span>
                  </RefusableButton>
                ) : null}
              </div>
              <p
                className={`text-sm ${textSecondaryOnSurface}`}
                data-testid="access-invitation-expiry"
              >
                Works once, until {exactTimeLabel(new Date(link.expires_at))}. Dispatch shows it
                only here, once: send it to {email} yourself.
              </p>
            </div>
          )}
          {/* Present, empty, before the press, so a screen reader announces the words it is filled
              with: a status region that mounts with its words already in it is not announced by
              every reader. */}
          <p className={lostAnswer ? `text-sm ${textSecondaryOnSurface}` : "sr-only"} role="status">
            {lostAnswer
              ? "Dispatch made a link, but its answer was lost on the way. Press Invite again: a new link ends that one."
              : ""}
          </p>
          {invite.error === null ? null : (
            <QueryError message={invitationRefusal(email, invite.error)} />
          )}
        </div>
      ) : null}
    </li>
  );
}
