import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { primaryButtonBg, primaryButtonHoverBg, textSecondaryOnSurface } from "../../theme/classes";
import { ShellMessagePage } from "../shell/ShellMessagePage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { CeremonyAlert } from "./CeremonyAlert";
import { type CeremonyRefusal, ceremonyRefusal } from "./refusals";
import { creationOptions, passkeysSupported, registrationJSON } from "./webauthn";

/** The state the page leaves on its history entry when it takes the token out of the address. A
 *  reload keeps a history entry's state, so a page loaded with no token tells a link it read
 *  already from a link that came with nothing after its #. */
const LINK_READ = { enrollLinkRead: true } as const;

/**
 * The page the owner's link opens: it checks the link (`/auth/passkey/register/begin`, which
 * leaves it unused) and names who made it, the project it is for and the address it was made for,
 * and its one button makes the passkey on the person's device and enrolls it, which uses the link
 * and signs the person in, and then opens Dispatch. A link Dispatch refuses - used, expired, ended
 * by a newer one, a Reset or a delisting - says why and offers nothing to press, since only a new
 * link gets past it; a used one links to the sign-in, where a passkey it made signs the person in.
 *
 * The page reads the link's token once and then replaces this tab's history entry with one
 * without it, so the address bar, a copy of the address and a return to the entry hold no link
 * that still enrolls; a reload then finds no token, and says to open the link again. The press
 * opens a ceremony of its own rather than reusing the check's: the check's is five minutes old at
 * most, and a person can read the page for longer than that.
 */
export function Enroll(): ReactNode {
  useDocumentTitle("Enroll a passkey · Dispatch");
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [token, setToken] = useState(() => location.hash.replace(/^#/, ""));
  const [enrolling, setEnrolling] = useState(false);
  const [refused, setRefused] = useState<CeremonyRefusal | undefined>(undefined);
  useEffect(() => {
    if (location.hash === "") return;
    // A link opened while this page is open is read the same way, in place of the one before it.
    setToken(location.hash.replace(/^#/, ""));
    setRefused(undefined);
    navigate(
      { hash: "", pathname: location.pathname, search: location.search },
      { replace: true, state: LINK_READ }
    );
  }, [location.hash, location.pathname, location.search, navigate]);
  const supported = passkeysSupported();
  const check = useQuery({
    enabled: supported && token !== "",
    gcTime: 0,
    queryFn: () => api.passkeyEnrollmentOptions(token),
    queryKey: ["passkey-enrollment", token],
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
  });

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
              typeof location.state === "object" &&
              location.state !== null &&
              "enrollLinkRead" in location.state &&
              location.state.enrollLinkRead === LINK_READ.enrollLinkRead
                ? "This page no longer holds the link. Open it again from the project owner's message."
                : "This link is incomplete: nothing follows its #. Open the link exactly as the project's owner sent it.",
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
    const { invitation } = check.data;
    const email = check.data.publicKey.user.name;
    return (
      <>
        <p className={`mt-3 break-words ${textSecondaryOnSurface}`}>
          <strong>{invitation.made_by}</strong> invited you to the project{" "}
          <strong>{invitation.project}</strong>.
        </p>
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
