import { describe, expect, test } from "bun:test"
import { parseHookArguments } from "../hooks/hook-arguments"

describe("session-hook's arguments", () => {
  test("name the host Claude Code when hooks.json names none", () => {
    expect(parseHookArguments(["answers"])).toEqual({
      host: "claude",
      mode: "answers",
      modeArguments: [],
    })
  })

  test("take --host codex wherever it stands, and keep the mode's own arguments", () => {
    expect(parseHookArguments(["dispatch-first", "--host", "codex"])).toEqual({
      host: "codex",
      mode: "dispatch-first",
      modeArguments: [],
    })
    expect(parseHookArguments(["answers-refresh", "s", "/w", "--host", "codex"])).toEqual({
      host: "codex",
      mode: "answers-refresh",
      modeArguments: ["s", "/w"],
    })
  })

  test("refuse a host they do not know", () => {
    expect(() => parseHookArguments(["answers", "--host", "omp"])).toThrow(/claude or codex/)
    expect(() => parseHookArguments(["answers", "--host"])).toThrow(/claude or codex/)
  })
})
