import type { ReactNode } from "react";

import { canvasText, card, textMutedOnSurface } from "../../theme/classes";

/** The one card a page shows in place of the shell, centred on the canvas: the sign-in page, the
 *  enrollment of a passkey, and the page that says Dispatch could not be reached. */
export function ShellMessagePage({
  children,
  title,
}: {
  children?: ReactNode;
  title: string;
}): ReactNode {
  return (
    <main className={`grid min-h-dvh place-items-center px-6 ${canvasText}`}>
      <section className={`w-full max-w-md rounded-2xl p-8 shadow-sm ${card}`}>
        <p className={`text-sm font-medium tracking-wide uppercase ${textMutedOnSurface}`}>
          Dispatch
        </p>
        <h1 className="mt-3 text-2xl font-semibold">{title}</h1>
        {children}
      </section>
    </main>
  );
}
