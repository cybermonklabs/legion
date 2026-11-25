package workflow

import "time"

// RetroAfterCloseBound is how long the retro Legion runs after an issue's close may wait for its
// report: 24 hours from the start of the retro's step row, and never sooner. At the bound the
// retro ends as stopped with no rating, its claim is closed, and a later completion is refused
// with the reason (record.RetroEndBound). It is well past the longest retro measured when the
// bound was chosen and inside the 72-hour default linger, which waits for the retro. The code
// that reads it lands in INFRA-5602's task t3.
const RetroAfterCloseBound = 24 * time.Hour
