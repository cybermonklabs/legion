package store

// Control of the merged-tree vet, never merged: a caller of runWatchedHooks in the form this
// branch's tip declares, runWatchedHooks(xid, hooks). Main after cd939dc4f32a (085e2a33de0c) takes
// a logger first, so this file compiles at the tip alone and fails to compile in the tree the tip
// makes with main.
func zzMergedVetControl() { runWatchedHooks("control", nil) }
