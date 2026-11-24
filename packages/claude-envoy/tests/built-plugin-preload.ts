// The plugin build the real-binary rows share (`builtPlugin` in built-plugin.ts) is removed once
// every file of the run has run: a hook in a preload belongs to the whole run, where one in a test
// file or a module it imports belongs to that file alone, and Bun 1.4.4's test runner fires no
// `exit` or `beforeExit` handler. `bunfig.toml` names this file.
import { afterAll } from "bun:test"
import { removeBuiltPlugin } from "./built-plugin"

afterAll(removeBuiltPlugin)
