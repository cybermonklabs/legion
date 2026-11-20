import type { PasskeyRefusal } from "@legion/contracts";

import { ApiError, NetworkError, RequestTimeoutError } from "../../api/client";
import { ceremonyFailure } from "./webauthn";

/** What a person reads when a passkey ceremony did not sign them in, and whether pressing the
 *  ceremony's button again can get past it. */
export interface CeremonyRefusal {
  readonly message: string;
  readonly retry: boolean;
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
    INVITATION_USED: {
      message: `This link has been used already. ${newLinkFromOwner}`,
      retry: false,
    },
    INVITATION_EXPIRED: { message: `This link has expired. ${newLinkFromOwner}`, retry: false },
    INVITATION_NOT_LISTED: {
      message:
        "The project's access list no longer names this address, so this link cannot enroll a passkey. Ask the project's owner.",
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
    // address does.
    if (error.status === 404) {
      return {
        message:
          "This address does not sign anyone in with a passkey. Open the link exactly as the project's owner sent it.",
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
