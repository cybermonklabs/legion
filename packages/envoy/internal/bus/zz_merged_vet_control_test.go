package bus

// A merged-tree vet control, never merged: logRefusal takes three arguments at this branch's
// merge base and four on main, so this compiles on the branch alone and not merged into main.
var _ = func() { logRefusal("control", nil, nil) }
