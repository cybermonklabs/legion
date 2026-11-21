// `bun --preload` this file into a process a test starts, with LEGION_TIMER_RECORD_FILE and
// LEGION_TIMER_RECORD_ROOTS set, to record the timers that process's own code arms
// (`./timer-record`); or hand it to an Oh My Pi pane as one more extension, whose default export
// does nothing. Without both variables it installs nothing; nothing in production loads it.
import { recordTimersFromEnvironment } from "./timer-record";

recordTimersFromEnvironment(process.env);

/** The pane extension's factory: the record is installed as the module loads. */
export default function timerRecordExtension(): void {}
