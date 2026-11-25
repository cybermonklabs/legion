package dispatch

// A scratch control for the merged-tree vet: New takes a token string at this branch's tip and a
// transport on main since #4392, so this compiles at the tip alone and not on the merged tree.
var _ = New("http://dispatch.example", "token")
