import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

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
