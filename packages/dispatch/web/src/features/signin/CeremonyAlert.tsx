import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { QueryError } from "../../components/QueryError";
import { linkHoverText, linkText } from "../../theme/classes";
import type { CeremonyRefusal } from "./refusals";

/** Why a passkey ceremony stopped, as the enrollment and sign-in pages show it: the shared
 *  refusal line, with Retry only where `onRetry` is given for a refusal a second try can get
 *  past, and a link to the sign-in where a passkey the person made already may sign them in. */
export function CeremonyAlert({
  onRetry,
  refusal,
  retrying,
}: {
  onRetry?: () => void;
  refusal: CeremonyRefusal;
  retrying?: boolean;
}): ReactNode {
  return (
    <div className="mt-4">
      <QueryError
        message={refusal.message}
        onRetry={refusal.retry ? onRetry : undefined}
        retrying={retrying}
      />
      {refusal.signIn === true ? (
        <Link
          className={`mt-3 inline-flex min-h-11 items-center text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to="/"
        >
          Sign in with your passkey
        </Link>
      ) : null}
    </div>
  );
}
