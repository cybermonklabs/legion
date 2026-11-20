import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { primaryButtonBg, primaryButtonHoverBg, textSecondaryOnSurface } from "../../theme/classes";
import { ShellMessagePage } from "../shell/ShellMessagePage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { CeremonyAlert } from "./CeremonyAlert";
import { type CeremonyRefusal, ceremonyRefusal } from "./refusals";
import { creationOptions, passkeysSupported, registrationJSON } from "./webauthn";

/** The path of the link a project's owner makes for a person from outside: `<outside
 *  origin>/enroll#<token>`, the token in the fragment so no server logs it and no browser sends it
 *  as a Referer. The app shows this page in place of the sign-in gate, since the person has no
 *  session yet. */
export const ENROLL_PATH = "/enroll";

/**
 * The page the owner's link opens: it checks the link (`/auth/passkey/register/begin`, which
 * leaves it unused) and names the address it was made for, and its one button makes the passkey
 * on the person's device and enrolls it, which uses the link and signs the person in, and then
 * opens Dispatch. A link Dispatch refuses - used, expired, ended by a newer one, a Reset or a
 * delisting - says why and offers nothing to press, since only a new link gets past it.
 *
 * The press opens a ceremony of its own rather than reusing the check's: the check's is five
 * minutes old at most, and a person can read the page for longer than that.
 */
export function Enroll(): ReactNode {
  useDocumentTitle("Enroll a passkey · Dispatch");
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const token = location.hash.replace(/^#/, "");
  const supported = passkeysSupported();
  const check = useQuery({
    enabled: supported && token !== "",
    gcTime: 0,
    queryFn: () => api.passkeyEnrollmentOptions(token),
    queryKey: ["passkey-enrollment", token],
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
  });
  const [enrolling, setEnrolling] = useState(false);
  const [refused, setRefused] = useState<CeremonyRefusal | undefined>(undefined);

  const enroll = async () => {
    setEnrolling(true);
    setRefused(undefined);
    try {
      const options = await api.passkeyEnrollmentOptions(token);
      const passkey = await navigator.credentials.create({ publicKey: creationOptions(options) });
      if (passkey === null) throw new DOMException("no passkey was made", "NotAllowedError");
      await api.passkeyEnroll(registrationJSON(passkey as PublicKeyCredential));
      // The session is the enrolled person's: nothing read for anyone before it may stand.
      queryClient.removeQueries();
      navigate("/", { replace: true });
    } catch (error) {
      setRefused(ceremonyRefusal(error, "create"));
    } finally {
      setEnrolling(false);
    }
  };

  const body = (): ReactNode => {
    if (token === "") {
      return (
        <CeremonyAlert
          refusal={{
            message:
              "This link is incomplete: nothing follows its #. Open the link exactly as the project's owner sent it.",
            retry: false,
          }}
        />
      );
    }
    if (!supported) {
      return (
        <CeremonyAlert
          refusal={{
            message:
              "This browser cannot make a passkey. Open the link in a current version of Chrome, Edge, Firefox or Safari.",
            retry: false,
          }}
        />
      );
    }
    if (check.isPending) {
      return (
        <p className={`mt-3 ${textSecondaryOnSurface}`} role="status">
          Checking the link…
        </p>
      );
    }
    if (check.isError) {
      const refusal = ceremonyRefusal(check.error, "create");
      return (
        <CeremonyAlert
          onRetry={refusal.retry ? () => void check.refetch() : undefined}
          refusal={refusal}
          retrying={check.isFetching}
        />
      );
    }
    const email = check.data.publicKey.user.name;
    return (
      <>
        <p className={`mt-3 break-words ${textSecondaryOnSurface}`}>
          The passkey signs you in to Dispatch as <strong>{email}</strong>.
        </p>
        <p className={`mt-3 ${textSecondaryOnSurface}`}>
          Your device asks you to confirm with your fingerprint, your face, a PIN or a security key.
          The link works once: after this, you sign in with the passkey alone.
        </p>
        {refused?.retry === false ? null : (
          <button
            aria-disabled={enrolling ? true : undefined}
            className={`mt-8 inline-flex rounded-lg px-4 py-2 font-semibold aria-disabled:cursor-not-allowed aria-disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
            onClick={() => {
              if (!enrolling) void enroll();
            }}
            type="button"
          >
            {enrolling ? "Creating the passkey…" : "Create a passkey"}
          </button>
        )}
        {refused === undefined ? null : <CeremonyAlert refusal={refused} />}
      </>
    );
  };

  return <ShellMessagePage title="Enroll a passkey">{body()}</ShellMessagePage>;
}
