import { expect, test } from "bun:test"
import path from "node:path"
import { wordBuiltFixtureSecrets } from "@legion/test-fixtures/fixture-secret"

// Every secret this package's tests hand the clients is random (`fixtureSecret`): the process
// holds each one and scrubs every line it prints of it, so a fixture spelled from words takes
// those words out of the lines a test reads (#4140).
test("every fixture secret this package's tests hand the clients is random", () => {
  expect(wordBuiltFixtureSecrets(path.join(import.meta.dir, ".."))).toEqual([])
})
