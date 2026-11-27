import { type ReactNode, useEffect } from "react";

import { askedSurface } from "../shell/asked-surface";
import { chunk } from "../shell/chunk-component";
import { prefetchChunk } from "../shell/chunk-loads";
import { useContentDrawn } from "../shell/content-drawn";

/**
 * Where the shell places the Create issue dialog, and where that dialog's code is fetched: once
 * the page has drawn, or at the `c` that opens it if that comes first (below).
 *
 * The shell imported the dialog itself until dispatch://INFRA-1109, so every page of the
 * dashboard parsed the form, its project list, its duplicate check and the related-items list
 * under it before drawing anything - about 50,000 bytes of the entry chunk for a dialog that
 * opens on a keystroke. The dialog is still mounted only while it is open, so this host changes
 * what is downloaded and not when the dialog is on the page.
 *
 * The dialog is drawn through `askedSurface` (`features/shell/asked-surface.tsx`), not behind
 * `lazy` and `Suspense`: with its chunk in, it opens in the commit of the `c` that asked for it,
 * as it did from the entry, and the keys typed straight after `c` reach its title and not the
 * page - on an issue page a digit would write the issue's priority, and `e` would put the rest
 * into the issue's own title. So the chunk is started as soon as the page's content has drawn
 * (`features/shell/content-drawn.ts`), as the pending-writes host starts its two
 * (`features/writes/write-hosts.tsx`): the shell mounts this host before the route's chunk and
 * reads are in, and a start at that mount would share the landing's network with them. Nobody is
 * waiting on it yet, which is what `prefetchChunk` tells the chunk-failure recovery
 * (`features/shell/DeploymentResilience.tsx`).
 *
 * What a reader loses by its being late - a `c` in the first moments of a page, or after the
 * download failed with nobody waiting: the dialog's place shows a small box that takes the keys
 * until the chunk lands, and then the dialog opens with what they typed in its title.
 */
const createIssueDialogChunk = chunk(() =>
  import("./CreateIssueDialog").then((module) => module.CreateIssueDialog)
);

const Dialog = askedSurface(createIssueDialogChunk, "Create issue");

export function CreateIssueDialogHost({
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  const drawn = useContentDrawn();
  useEffect(() => {
    if (drawn) prefetchChunk(createIssueDialogChunk.load);
  }, [drawn]);
  return open ? <Dialog onClose={onClose} /> : null;
}
