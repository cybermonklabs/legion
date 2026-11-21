import type { PasskeyRefusal } from "@legion/contracts";

import { ApiError, NetworkError, RequestTimeoutError } from "../../api/client";
import { ceremonyFailure } from "./webauthn";

/** What a person reads when a passkey ceremony did not sign them in, whether pressing the
 *  ceremony's button again can get past it, and whether a passkey the person made already may
 *  sign them in instead (`signIn`: the page then links to the sign-in). */
export interface CeremonyRefusal {
  readonly message: string;
  readonly retry: boolean;
  readonly signIn?: boolean;
}

type Ceremony = "create" | "get";

const newLink = "Ask a project's owner for a new link.";
const newLinkFromOwner = "Ask the project's owner for a new one.";

/** The words of the codes Dispatch refuses a ceremony with (`PASSKEY_REFUSALS`), by the ceremony
 *  that meets them: the link's codes only at the enrollment (`create`), the stored passkey's only
 *  at the sign-in (`get`), the rest at either. A code a ceremony never meets is worded by the
 *  fallback in `ceremonyRefusal`, which names it. */
const refusals: Record<Ceremony, Partial<Record<PasskeyRefusal, CeremonyRefusal>>> = {
  create: {
    INVITATION_UNKNOWN: {
      message: `Dispatch does not hold this link: the project's owner made a newer one, reset the address, or took it off the access list. ${newLinkFromOwner}`,
      retry: false,
    },
    // The person may have made a passkey with it: they opened the link again after enrolling, or
    // their enrollment went through and its answer was lost.
    INVITATION_USED: {
      message:
        "This link has been used already. If you made a passkey with it, sign in with that passkey. If not, ask the project's owner for a new one.",
      retry: false,
      signIn: true,
    },
    INVITATION_EXPIRED: { message: `This link has expired. ${newLinkFromOwner}`, retry: false },
    INVITATION_NOT_LISTED: {
      message:
        "The project's access list no longer names this address, so this link cannot enroll a passkey. Ask the project's owner.",
      retry: false,
    },
    // A passkey reads every project that names its person, so one owner binds a person from
    // outside: a link of one owner enrolls nobody another owner's project names.
    LISTED_BY_ANOTHER_OWNER: {
      message:
        "Another project's owner also names this address, so this link cannot enroll a passkey. Ask the project's owner.",
      retry: false,
    },
    WORKSPACE_ADDRESS: {
      message:
        "This link is for an address of the company's Google Workspace, whose people sign in with Google and never with a passkey.",
      retry: false,
    },
    FULL_MEMBER: {
      message:
        "This address belongs to a member of the company's Google Workspace, who signs in with Google and never with a passkey.",
      retry: false,
    },
    PASSKEY_REFUSED: {
      message: "Dispatch could not verify the new passkey. Try again.",
      retry: true,
    },
  },
  get: {
    NOT_LISTED: {
      message: `No project's access list names you any more, so Dispatch signed you out and removed your passkeys. ${newLink}`,
      retry: false,
    },
    PASSKEY_UNKNOWN: {
      message: `Dispatch does not know this passkey: it was removed, so it signs in no more. ${newLink}`,
      retry: false,
    },
    PASSKEY_SIGN_COUNT: {
      message: `Dispatch refused this passkey: its sign-in count did not go up since its last use, which happens when a passkey has been copied. ${newLink}`,
      retry: false,
    },
    WORKSPACE_ADDRESS: {
      message:
        "This passkey is for an address of the company's Google Workspace, whose people sign in with Google and never with a passkey.",
      retry: false,
    },
    PASSKEY_REFUSED: { message: "Dispatch could not verify the passkey. Try again.", retry: true },
  },
};

/** The codes either ceremony meets in the same words. */
const eitherCeremony: Partial<Record<PasskeyRefusal, CeremonyRefusal>> = {
  MEMBERSHIP_UNREADABLE: {
    message: "Dispatch could not read the access lists just now and changed nothing. Try again.",
    retry: true,
  },
  // Each begin's challenge is single-use: a finish meets this past five minutes, on a replay, or
  // after another enrollment of the same address finished first. A press begins anew.
  CEREMONY_EXPIRED: {
    message: "This request ran out, or another one finished first. Press the button to try again.",
    retry: true,
  },
  PASSKEY_RATE_LIMITED: {
    message: "Too many passkey requests reached Dispatch just now. Wait a moment, then try again.",
    retry: true,
  },
  // Only a proxy between the person and Dispatch's load balancer adds a second X-Forwarded-For
  // line, so a second press meets it again.
  FORWARDED_FOR_INVALID: {
    message:
      "Dispatch could not tell where this request came from, so it made no passkey request. A network proxy between you and Dispatch may cause this: try another network, or tell the project's owner.",
    retry: false,
  },
};

/** What the person reads when the ceremony (`create` enrolls, `get` signs in) failed with
 *  `error`: Dispatch's refusal by its code, a request that got no answer, or the browser's own
 *  refusal of the ceremony, made before anything reached Dispatch. */
export function ceremonyRefusal(error: unknown, ceremony: Ceremony): CeremonyRefusal {
  if (error instanceof ApiError) {
    const code = error.code as PasskeyRefusal | undefined;
    const known =
      code === undefined ? undefined : (refusals[ceremony][code] ?? eitherCeremony[code]);
    if (known !== undefined) return known;
    // A deployment with passkey sign-in off answers these routes 404, as the company's own
    // address does. Its outside origin offers the passkey alone with the setting off too (whoami's
    // `sign_in`), so a sign-in there meets this as well.
    if (error.status === 404) {
      return {
        message:
          ceremony === "create"
            ? "This address does not sign anyone in with a passkey. Open the link exactly as the project's owner sent it."
            : "Dispatch signs nobody in with a passkey at this address now. Ask a project's owner.",
        retry: false,
      };
    }
    return {
      message: `Dispatch did not ${ceremony === "create" ? "enroll" : "accept"} the passkey (${error.code ?? error.status}). Try again.`,
      retry: true,
    };
  }
  if (error instanceof NetworkError || error instanceof RequestTimeoutError) {
    return { message: "Dispatch could not be reached. Try again.", retry: true };
  }
  return { message: ceremonyFailure(error, ceremony), retry: true };
}
