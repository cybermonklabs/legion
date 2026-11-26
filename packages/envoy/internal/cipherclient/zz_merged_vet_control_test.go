package cipherclient

// Control of the merged-tree vet, never merged: a caller of CredentialsFileFailure, which main
// added after this branch's last merge of main (1af83932649d; main 5d3e9b84219b has it, at
// detect.go:483). It fails to compile at the branch's tip alone and compiles in the tree the tip
// makes with main, so a vet that passes it ran on the merged tree.
func zzMergedVetControl() string { return CredentialsFileFailure(nil) }
