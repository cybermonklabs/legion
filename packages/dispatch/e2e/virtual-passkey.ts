import type { CDPSession, Page } from "./test";

/** A person's device for a passkey ceremony in Chromium: a virtual authenticator on one page's
 *  target, reached through that page's CDP session, and its id there. */
export interface VirtualPasskey {
  readonly cdp: CDPSession;
  readonly authenticatorId: string;
}

/**
 * Adds a virtual authenticator to `page`, as a person's phone or laptop: CTAP2 over the platform's
 * own transport, holding discoverable credentials and verifying its user at every ceremony with
 * no prompt (`automaticPresenceSimulation`, `isUserVerified`), which is what Dispatch's
 * ceremonies ask for (a discoverable credential, user verification required). Chromium alone has
 * the WebAuthn domain. The authenticator lives as long as the page does, so a ceremony in another
 * page of the same context needs one of its own. `outside-passkey.e2e.ts` stands on it.
 */
export async function addVirtualPasskey(page: Page): Promise<VirtualPasskey> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      automaticPresenceSimulation: true,
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      protocol: "ctap2",
      transport: "internal",
    },
  });
  return { authenticatorId, cdp };
}
