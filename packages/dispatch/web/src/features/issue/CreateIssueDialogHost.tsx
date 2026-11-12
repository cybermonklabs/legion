import { lazy, type ReactNode, Suspense } from "react";

import { renderChunk } from "../shell/DeploymentResilience";

/**
 * Where the shell places the Create issue dialog, and where that dialog's code is fetched: when
 * a reader presses `c` or chooses Create issue, never before.
 *
 * The shell imported the dialog itself until dispatch://INFRA-1109, so every page of the
 * dashboard parsed the form, its project list, its duplicate check and the related-items list
 * under it before drawing anything - about 50,000 bytes of the entry chunk for a dialog that
 * opens on a keystroke. The shell already mounts it only while it is open, so this host changes
 * what is downloaded and not when the dialog is on the page.
 *
 * What a reader loses by its being late: the frames between the keystroke and the chunk's
 * arrival show no dialog, and then the dialog opens with its title box focused as ever.
 */
// Under `renderChunk`, like every chunk a render waits on: the reader has asked for the dialog,
// so a download that fails must spend one of the tab's recovery reloads rather than be read as
// the entry's prefetch (`features/shell/DeploymentResilience.tsx`).
const Dialog = lazy(() =>
  renderChunk(() =>
    import("./CreateIssueDialog").then((module) => ({ default: module.CreateIssueDialog }))
  )
);

export function CreateIssueDialogHost({ onClose }: { onClose: () => void }): ReactNode {
  return (
    <Suspense fallback={null}>
      <Dialog onClose={onClose} />
    </Suspense>
  );
}
