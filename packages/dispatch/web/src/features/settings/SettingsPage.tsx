import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import { isCredentialFeatureOff } from "../../api/client";
import { useRestrictedMember } from "../../hooks/useRestrictedMember";
import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { GrantsSection } from "../credentials/GrantsSection";
import { credentialGrantsQuery } from "../credentials/grants";
import { credentialPendingQuery } from "../credentials/pending";
import { SignInsSection } from "../credentials/SignInsSection";
import { DeliverySettingsForm } from "../delivery/DeliverySettingsForm";
import { IssuePagePreference } from "../issue-page/IssuePagePreference";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentTokensSection } from "./AgentTokensSection";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";
import { settingsSectionGap } from "./classes";
import { GroupsSection } from "./GroupsSection";
import { LegionDaemonsSection } from "./LegionDaemonsSection";
import { ProjectsSection } from "./ProjectsSection";

/** The id an address's `#` part names, or `undefined` for none and for a part that is no valid
 *  percent-encoding (`#%E0%A4%A`), which opens Settings at its top rather than throwing. */
function fragmentId(hash: string): string | undefined {
  if (hash.length <= 1) return undefined;
  try {
    return decodeURIComponent(hash.slice(1));
  } catch {
    return undefined;
  }
}

/**
 * Settings. Its groups, in this order, one under another in a single column at every width
 * (the layout rule of `packages/dispatch/web/src/AGENTS.md`, dispatch://INFRA-5200):
 *
 * 1. Heading: the page's title and the one line that says what it holds.
 * 2. Projects (`ProjectsSection`).
 * 3. Agent tokens (`AgentTokensSection`).
 * 4. Live grants and Sign-ins (`GrantsSection`, `SignInsSection`), drawn only on a Dispatch with
 *    a secrets broker; a Dispatch without one has nothing for them to list.
 * 5. Architecture sources (`ArchitectureSourcesSection`).
 * 6. Delivery timeline: its heading, its line, and its form (`DeliverySettingsForm`).
 * 7. The issue page (`IssuePagePreference`).
 * 8. Legion daemons (`LegionDaemonsSection`).
 *
 * Each section lays out its own rows; none sits beside another. An address that names a section
 * by its `#` part (`/settings#architecture-sources-heading`, which an issue's Components line
 * links to while its project has no source) opens the page at that section.
 */
export function SettingsPage(): ReactNode {
  useDocumentTitle("Settings · Dispatch");
  // A restricted member reads none of the deployment's own rows (`useRestrictedMember`): no
  // group list, no delivery settings, no credential list and no Legion daemon setting, so their
  // Settings draws no Groups, no Delivery timeline and no Legion. Their own grants and sign-ins
  // are theirs to read and to revoke, which Dispatch takes to the secrets broker as it does a full
  // member's.
  const restricted = useRestrictedMember();
  // Whether this Dispatch has a secrets broker, without which Live grants and Sign-ins have
  // nothing to list: the credential list answers `null` where it has none. A restricted member
  // may not read that list, so their page asks the grants list instead, which they may read and
  // which answers 404 FEATURE_OFF where there is no broker. The server reads its broker setting
  // once, at boot, so neither answer of no broker is asked again until the event feed starts after
  // a gap (`reaskHeldCredentialList`): the credential list's own gate holds a `null`, and the
  // grants list's gate here holds a FEATURE_OFF.
  const credentialsAvailable = useQuery(credentialPendingQuery(restricted));
  const grants = useQuery({
    ...credentialGrantsQuery(),
    enabled: restricted ? (query) => !isCredentialFeatureOff(query.state.error) : false,
  });
  const brokered = restricted
    ? !grants.isPending && !isCredentialFeatureOff(grants.error)
    : !credentialsAvailable.isPending && credentialsAvailable.data !== null;

  return (
    <section className="max-w-4xl">
      <header className="mb-6">
        <h1 className={`text-2xl font-semibold ${textPrimaryOnCanvas}`}>Settings</h1>
        <p className={`mt-1 ${textSecondaryOnCanvas}`}>
          Manage the projects, the agent tokens and the settings of this Dispatch.
        </p>
      </header>
      <div className={settingsSectionGap}>
        <ProjectsSection />
        {restricted ? null : <GroupsSection />}
        <AgentTokensSection />
        {brokered ? (
          <>
            <GrantsSection />
            <SignInsSection />
          </>
        ) : null}
        <ArchitectureSourcesSection />
        {restricted ? null : (
          <section aria-labelledby="delivery-timeline-heading">
            <h2
              className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
              id="delivery-timeline-heading"
            >
              Delivery timeline
            </h2>
            <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
              Which repository deploys, and whose merged pull requests the Delivery page counts.
              Saving verifies the Dispatch GitHub App can read the deploy repository.
            </p>
            <DeliverySettingsForm />
          </section>
        )}
        <IssuePagePreference />
        {restricted ? null : <LegionDaemonsSection />}
      </div>
    </section>
  );
}
