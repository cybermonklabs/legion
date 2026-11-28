**How it is measured.** The Legion daemon keeps a record of each change of what regulates its admission: `admission.capChanges` for the cap, and `admission.signalChanges` for the switch of admission by signals, each signal's limit, whether each signal holds admission, and each signal's value. Dispatch's Legion intake copies that record into this eval (`evals.AdmissionWriter`). It writes one reading for each entry, once, in the transaction that applies the daemon's state, at the time the daemon recorded the change. It computes none of the values.

- `admission-cap` is the cap, in issues. A `cap_change` reading is a change that the daemon recorded: its value is the cap after the change. Its attributes and its note give the cap before, the ceiling, who set it and why. A `cap_observed` reading is a cap that the daemon's state reported and that no change explains, at the time of that state.
- `admission-idle`, `admission-queue`, `admission-headroom`, `admission-kills` and `admission-approvals` hold each signal's value and its limit, under the `col` `value` and `limit`, in the signal's unit. A reading with no value ends its line.
- `admission-holds` is 1 where a signal holds admission and 0 where it does not, with one `col` for each signal.
- `admission-switch` is 1 where admission by signals is on and 0 where it is off.

Each series folds by its last value, and each line holds a value until the next reading of its series.

**What it does not count.** A change that the daemon did not record, and an entry of a signal that this build of Dispatch does not know.
