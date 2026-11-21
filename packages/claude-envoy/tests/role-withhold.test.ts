import { expect, test } from "bun:test"
import { createEnvoyClient, type EnvoyFetch } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { readPersistedRole, roleStateFile, SessionIdentity } from "../src/session-identity"
import { noInterest, sessionOptions, useScratchState, waitFor } from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

const ROLE = "legion-acme-1-implementer"

/** How many reads answer that the role is withheld before the listener lifts the withhold. */
const WITHHELD_READS = 3

// A Legion daemon withholds a role from a session whose claim does not hold it, which frees the
// session's standing claim and refuses its every claim of the role. The session has lost the
// role: a claim at every heartbeat would be refused at every heartbeat, and only the daemon lifts
// the withhold, so the heartbeat reads the role and claims it once the read says it may. The
// listener here is a stand-in behind the real client, so each answer is what the client makes of
// the listener's own words.
test("a withheld role is lost until the listener lifts it", async () => {
  const stateDirectory = scratchState()
  const identity = new SessionIdentity("ses_claude", "/tmp")
  // racing: the heartbeat's read finds no holder and nothing withheld yet, and its claim is
  // refused as withheld; withheld: every read says so; lifted: the daemon lifted the withhold;
  // held: the claim after the lift landed.
  let phase: "racing" | "withheld" | "lifted" | "held" = "racing"
  let withheldReads = 0
  // Each role call the listener answered, in order, with what it answered.
  const roleCalls: string[] = []
  const unclaimed = { error: `no holder for role ${ROLE}`, reason: "unclaimed", claim_revision: 9 }
  const listener: EnvoyFetch = async (input, init) => {
    const url = new URL(input.toString())
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined
    if (url.pathname === "/v1/interests/subscribe") {
      return Response.json(noInterest(body.session_id))
    }
    if (url.pathname === "/v1/roles/set") {
      if (phase === "racing" || phase === "withheld") {
        roleCalls.push("claim refused withheld")
        phase = "withheld"
        return Response.json(
          { error: "role_withheld", role: ROLE, withheld: true },
          { status: 409 },
        )
      }
      roleCalls.push("claim granted")
      phase = "held"
      return Response.json({ ...noInterest(), topics: [`notifications.role.${ROLE}`] })
    }
    if (url.pathname === `/v1/roles/${ROLE}`) {
      roleCalls.push(`read ${phase} ${url.search}`)
      if (phase === "held") {
        return Response.json({ role: ROLE, holder: identity.id, last_seen: 1, state: "live" })
      }
      if (phase !== "withheld") return Response.json(unclaimed, { status: 404 })
      withheldReads += 1
      if (withheldReads === WITHHELD_READS) phase = "lifted"
      return Response.json({ ...unclaimed, withheld: true }, { status: 404 })
    }
    if (url.pathname === "/v1/interests/unsubscribe") return Response.json({ removed: [] })
    // The interests read, and the deregistration at shutdown.
    return Response.json(noInterest())
  }
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    fetch: listener,
    retry: false,
  })
  const options = sessionOptions(identity, stateDirectory, { client, heartbeatMs: 25 })
  const session = await startChannelSession(options)

  try {
    await session.rememberRole(ROLE)
    await waitFor(
      async () => phase === "held",
      "the claim at the first heartbeat after the listener lifted the withhold",
      () => roleCalls.join(", "),
    )
    const refused = roleCalls.indexOf("claim refused withheld")
    const granted = roleCalls.indexOf("claim granted")
    // No claim between the one the listener refused and the one after the lift: every heartbeat
    // in between read the role, and claimed nothing while the read said it was withheld.
    expect(
      roleCalls.slice(refused + 1, granted).filter((call) => call.startsWith("claim")),
    ).toEqual([])
    // Each read was made for this session, and the claim went out at the first heartbeat whose
    // read said the withhold was lifted.
    const read = (answered: string) => `read ${answered} ?session_id=${identity.id}`
    expect(roleCalls.slice(0, refused)).toEqual([read("racing")])
    expect(roleCalls.slice(refused, granted + 1)).toEqual([
      "claim refused withheld",
      ...Array.from({ length: WITHHELD_READS }, () => read("withheld")),
      read("lifted"),
      "claim granted",
    ])
    // The record stays where `claude --resume` finds it: the role was this session's, and is
    // again.
    expect(await readPersistedRole(roleStateFile(stateDirectory, identity.id))).toEqual({
      session_id: identity.id,
      role: ROLE,
      client_instance_id: options.clientInstanceID,
    })
  } finally {
    await session.shutdown()
  }
  // Run alone, the file gets bun's default of 5 s, no more than the wait above: its own bound is
  // what lets a stuck wait name its step and the role calls it saw (`waitFor`).
}, 30_000)
