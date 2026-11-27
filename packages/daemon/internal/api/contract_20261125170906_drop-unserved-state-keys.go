package api

// 20261125170906 drop-unserved-state-keys: the plugin's state schema drops two keys no daemon
// serves. The state schema of `packages/contracts/src/legion-api.ts` no longer declares
// `slot.lentTo` or `admission.free`, which no daemon has written (both came with a widening of
// contract 12's schemas that changed no daemon), and pi-legion's `read_state` summary no longer
// prints them. No answer this daemon serves moves, so nothing is served down: a reader below this
// entry declares two keys this daemon never writes, and a reader at this entry is served every key
// it reads.
const dropUnservedStateKeysContract = 20261125170906

func init() {
	registerEntry(contractEntry{id: dropUnservedStateKeysContract, slug: "drop-unserved-state-keys"})
}
