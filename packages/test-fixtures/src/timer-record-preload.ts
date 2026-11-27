// `bun --preload` this file into a process a test starts, with LEGION_TIMER_RECORD_FILE and
// LEGION_TIMER_RECORD_ROOTS set, to record the timers that process's own code arms
// (`./timer-record`). An Oh My Pi pane takes it through `BUN_OPTIONS="--preload <this file>"`
// (the binary is a compiled Bun executable, which reads it), so the record is installed before the
// host loads anything: handed over as one more extension, it came after the host had loaded
// `node:timers/promises`, whose timers then went around it. Without both variables it installs
// nothing; nothing in production loads it. Once it has read them, it removes both variables and
// its own `--preload` from the process's environment, so no child of the process inherits the
// record, and each step it writes names the process (`pid`).
import { recordTimersFromEnvironment } from "./timer-record";

recordTimersFromEnvironment(process.env);
