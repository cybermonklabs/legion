// A test preload for every package whose tests run code that holds secrets: each test starts with
// none held, so a value one test holds (a token a stub broker minted, a command's output used as a
// bearer) never scrubs a line another test pins. `bunfig.toml` in each such package names it.
import { afterEach } from "bun:test";
import { forgetHeldSecrets } from "./held-secrets";

afterEach(forgetHeldSecrets);
