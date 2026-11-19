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

test("a wait for an id another was admitted after is dropped, never settled", async () => {
  const admission = sessionAdmission("ses_first")
  const skipped = admission.admitted("ses_skipped")
  admission.admit("ses_latest")
  expect(await settled(skipped)).toBe(false)
  // Admitting it later settles a fresh wait, not the dropped one.
  admission.admit("ses_skipped")
  expect(await settled(skipped)).toBe(false)
  expect(await settled(admission.admitted("ses_skipped"))).toBe(true)
})
