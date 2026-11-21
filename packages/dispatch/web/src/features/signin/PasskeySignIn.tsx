import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { primaryButtonBg, primaryButtonHoverBg, textSecondaryOnSurface } from "../../theme/classes";
import { CeremonyAlert } from "./CeremonyAlert";
import { type CeremonyRefusal, ceremonyRefusal } from "./refusals";
import { passkeysSupported, signWithPasskey } from "./webauthn";

/**
 * The sign-in page's own part where the origin's one sign-in is a passkey (Dispatch's outside
 * origin, whose `401 NO_IDENTITY` says `sign_in: "passkey"`): a button that asks the device for
 * any passkey it holds for Dispatch (`/auth/passkey/login/begin` names none, since each is a
 * discoverable credential) and signs its person in with the signature. The sign-in check is then
 * read afresh, so the gate opens the shell where it stands, and every write the person left kept
 * resumes, as after any sign-in. A refusal stays beside the button: another passkey, on another
 * device, may still sign in.
 */
export function PasskeySignIn(): ReactNode {
  const queryClient = useQueryClient();
  const [signingIn, setSigningIn] = useState(false);
  const [refused, setRefused] = useState<CeremonyRefusal | undefined>(undefined);

  if (!passkeysSupported()) {
    return (
      <CeremonyAlert
        refusal={{
          message:
            "This browser cannot use a passkey. Open Dispatch in a current version of Chrome, Edge, Firefox or Safari.",
          retry: false,
        }}
      />
    );
  }

  const signIn = async () => {
    setSigningIn(true);
    setRefused(undefined);
    try {
      const options = await api.passkeySignInOptions();
      await api.passkeySignIn(await signWithPasskey(options));
      await queryClient.resetQueries({ queryKey: ["whoami"] });
    } catch (error) {
      setRefused(ceremonyRefusal(error, "get"));
    } finally {
      // Signed in, the gate has unmounted this page already; otherwise the button is pressable
      // again.
      setSigningIn(false);
    }
  };

  return (
    <>
      <p className={`mt-3 ${textSecondaryOnSurface}`}>
        Sign in with the passkey you made through the link a project&apos;s owner sent you.
      </p>
      <button
        aria-disabled={signingIn ? true : undefined}
        className={`mt-8 inline-flex rounded-lg px-4 py-2 font-semibold aria-disabled:cursor-not-allowed aria-disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
        onClick={() => {
          if (!signingIn) void signIn();
        }}
        type="button"
      >
        {signingIn ? "Signing in…" : "Sign in with a passkey"}
      </button>
      {refused === undefined ? null : <CeremonyAlert refusal={refused} />}
    </>
  );
}
