// Which id the channel server's connection is admitted as, under a broker token that admits one
// session at a time: what the live view waits on before it takes a `/clear`'s control subject.
import { expect, test } from "bun:test"
import { sessionAdmission } from "../src/envoy-channel-server"

/** Whether `promise` has settled after the turns already queued have run. */
async function settled(promise: Promise<void>): Promise<boolean> {
  let done = false
  void promise.then(() => {
    done = true
  })
  await new Promise<void>((resolve) => setImmediate(resolve))
  return done
}

test("the dial's id is admitted at once, and a /clear's once its move lands", async () => {
  const admission = sessionAdmission("ses_first")
  expect(await settled(admission.admitted("ses_first"))).toBe(true)
  const next = admission.admitted("ses_next")
  expect(await settled(next)).toBe(false)
  admission.admit("ses_next")
  expect(await settled(next)).toBe(true)
  expect(await settled(admission.admitted("ses_next"))).toBe(true)
  // The id before is no longer the connection's.
  expect(await settled(admission.admitted("ses_first"))).toBe(false)
})

// The live view hears of each `/clear` from the mod at once, and the server adopts each one in
// turn, after the move before it has landed: so the server can admit B after the live view has
// asked about C (the deep review of dfa2ea1f59, M1).
test("a wait for the latest id outlives the admission of an earlier one, and lands with its own", async () => {
  const admission = sessionAdmission("ses_first")
  const latest = admission.admitted("ses_c")
  admission.admit("ses_b")
  expect(await settled(latest)).toBe(false)
  admission.admit("ses_c")
  expect(await settled(latest)).toBe(true)
})

test("a wait for an id asked about before a later one is dropped, never settled", async () => {
  const admission = sessionAdmission("ses_first")
  const earlier = admission.admitted("ses_b")
  const latest = admission.admitted("ses_c")
  admission.admit("ses_b")
  expect(await settled(earlier)).toBe(false)
  expect(await settled(latest)).toBe(false)
  admission.admit("ses_c")
  expect(await settled(earlier)).toBe(false)
  expect(await settled(latest)).toBe(true)
})
