import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { lazyChunk } from "../shell/chunk-loads";
import { NotFoundPage } from "../shell/NotFoundPage";
import { credentialCapabilitiesQuery } from "./capabilities";

// Through `lazyChunk` like every route chunk of `app.tsx`: the gate renders this page as soon as
// the capabilities answer, so a reader is waiting on the download and its failure is one the
// chunk-failure recovery must reload for, never one it may read as the entry's prefetch
// (`features/shell/DeploymentResilience.tsx`).
const MachineLoginPage = lazyChunk(() =>
  import("./MachineLoginPage").then((module) => ({ default: module.MachineLoginPage }))
);

/**
 * `/credentials/machine` is served only while this deployment's secrets broker still serves the
 * device grant, which is the only thing that makes a machine print a code for a person to type
 * (`credentialCapabilitiesQuery`, Dispatch's relay of the broker's RFC 8414 metadata). A
 * deployment whose machines sign in with their own cloud identity prints no code, so the page
 * answers like any other address Dispatch does not have.
 *
 * While the answer is still out the route renders nothing rather than "not found": a person who
 * followed a link to a page that does exist must not read that it does not, and the page's own
 * lazy chunk is a blank moment anyway.
 *
 * The gate is the route `app.tsx` names, and the page behind it is this module's own
 * `lazyChunk()`: the gate is a few lines and the ask for the deployment's capabilities starts with
 * the route, while the page stays a chunk of its own, loaded only where the deployment serves it
 * (`e2e/two-builds-routes.ts` walks the route under this name).
 */
export function MachineLoginRoute(): ReactNode {
  const capabilities = useQuery(credentialCapabilitiesQuery());
  if (capabilities.isPending) {
    return null;
  }
  return capabilities.data?.machine_code_page === true ? <MachineLoginPage /> : <NotFoundPage />;
}
