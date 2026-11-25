package natscallout

// A scratch control for the merged-tree vet: admittedServiceGrant is a helper of this package's
// tests at this branch's head, and main at 7102278681e2 replaced it with admittedServiceGrants, so
// this compiles at the head alone and not on the tree merged into main.
var _ = admittedServiceGrant
