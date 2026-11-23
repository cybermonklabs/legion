#!/bin/sh
# The executable Playwright starts in place of Chromium when the browser gets a network namespace
# of its own (`launchOptions.executablePath`, e2e/browser-namespace.ts's `chromiumLaunchOptions`).
# It is given Chromium's arguments, and Chromium's two pipes on descriptors 3 and 4, and hands
# them to e2e/browser-namespace.ts's `launch`, whose head says what it does and why.
exec bun "$(dirname -- "$0")/browser-namespace.ts" launch "$@"
