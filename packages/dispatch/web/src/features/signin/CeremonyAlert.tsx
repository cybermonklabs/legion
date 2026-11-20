import type { ReactNode } from "react";

import { QueryError } from "../../components/QueryError";
import type { CeremonyRefusal } from "./refusals";

/** Why a passkey ceremony stopped, as the enrollment and sign-in pages show it: the shared
 *  refusal line, with Retry only where `onRetry` is given for a refusal a second try can get
 *  past. */
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
    </div>
  );
}
