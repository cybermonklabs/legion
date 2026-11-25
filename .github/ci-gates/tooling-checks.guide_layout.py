# The tooling-checks job's guide-layout step, gated on a filter of its own: the test spawns
# the checker over six guides, and no other step of the job reads one of them. The job's own
# filter takes them too, as it takes every step gate's reads (ci_filter_coverage.gate_read_sets):
# a job's `if:` may test one changes output only, so without them a guide-only change selected
# no job and this step - the one check that change is about - ran nowhere.
GATE = gate("guide_layout", TOOLING_JOB, step=GUIDES_STEP, reads=GUIDE_LAYOUT_READS)
