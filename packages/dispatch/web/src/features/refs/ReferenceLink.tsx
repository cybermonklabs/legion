import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { linkHoverText, linkText } from "../../theme/classes";
import { referenceTriggerProps } from "./RefPreview";
import { parseNodeReference, referenceHref, shortForm } from "./routes";

/**
 * A `dispatch://` reference drawn as a link to what it names, with the preview the whole app
 * shows when a reader hovers it. The words are `label` when the caller knows a better name — a
 * block's own words, read from the reference graph — and the reference's short form otherwise.
 * A reference this app can route nowhere is drawn as the text it is, since there is nowhere to
 * go.
 */
export function ReferenceLink({
  label,
  reference,
  testId,
}: {
  label?: string;
  reference: string;
  testId?: string;
}): ReactNode {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return <span data-testid={testId}>{reference}</span>;
  }
  const words = label ?? shortForm(route);
  // A person has no page (R5), as has any reference whose path the gate refuses: the words
  // stand on their own, since there is nowhere for them to go.
  const href = referenceHref(route);
  if (href === undefined) {
    return <span data-testid={testId}>{words}</span>;
  }
  return (
    <Link
      {...referenceTriggerProps(route)}
      className={`underline ${linkText} ${linkHoverText}`}
      data-testid={testId}
      to={href}
    >
      {words}
    </Link>
  );
}
