package specgraph

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"testing"
	"testing/fstest"
)

// sweepFloors is, for each sweep and each base (sweepBases), the count of inputs that sweepAgainst
// judged when the table was written, by what the reader did with each: read it, as the base or as
// another state, or refused it, and the digest of the kind of each judged input in the order of the
// inputs (sweepFloor). The sweeps are the tree sweep (TestNoInputThatSaysNothingMovesAStateTowardTrue),
// the members added (TestAddingAMemberMovesNoStateTowardTrue), the texts inside a block and the
// attributes left out (TestNoNameOfNothingInsideABlockMovesAStateTowardTrue), and the refusals added
// (TestAddingARefusalOfCIMovesAStateTowardTrueOnlyByTakingOutItsCommitsResults). A sweep that judges
// fewer inputs of a base of either kind fails by name (atFloor), and so does a sweep whose inputs of
// a base read, in their order, as other kinds than the digest's, and a base with no floor or a floor
// of no base (floorsCover). So a fault that drops inputs, of a sweep's meaning or of its writers, a
// sweep that builds inputs and does not derive them, a sweep that stops judging an input that moves
// a state toward true, and a fault that moves inputs from one kind to the other, as many each way
// as it likes, cannot pass. A base, a scenario, a sweep or a reader that adds, takes out or moves an
// input changes the digest, so the table is written again, on purpose, from a run of the sweeps. It
// is a function that gives a new table at each call, so that no writer can lower a floor
// (sweepMaps).
func sweepFloors() map[string]map[string]sweepFloor {
	return map[string]map[string]sweepFloor{
		"tree": {
			"carry": {read: 92, refused: 887, kinds: "4b4bc3d5681861fa3c5e31a3445550bf045613034faf3334a059a97b1e6569b8"},
			"carry with a failure of c0, c1's parent, and a later pass of c2":            {read: 120, refused: 1114, kinds: "b5b7ca9e78db016406b643742dd825094e3bc5bed4a1c94222ff93ae015d0b1a"},
			"carry with a later failing example planted by no step":                      {read: 104, refused: 934, kinds: "8c033eb1ccc7825e20bdd1e80cbdf7f809513a77e2f9e465d79a5448c361d8a6"},
			"carry with a later failing example planted on c1 that missed its fault":     {read: 106, refused: 936, kinds: "fd899ed2b533803156e8f0ea1f1791df5781e32ed7852a7e7220ad411fd16134"},
			"carry with a later failing example planted on c9, no commit of the history": {read: 106, refused: 936, kinds: "fd899ed2b533803156e8f0ea1f1791df5781e32ed7852a7e7220ad411fd16134"},
			"carry with c2's failure at 10:30 and a later pass of m2":                    {read: 107, refused: 1024, kinds: "81bfa5c76c3d951ea82e5bf5d448fbcf7d788653af00ea5fb2171fa1089f2bc9"},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 107, refused: 1010, kinds: "db85a152049db6314365eaac77f8a5a146c154b5203445d5d4d91e0acea220a3"},
			"carry with its failing example planted by no step":                          {read: 88, refused: 878, kinds: "63d24a8caf3dcda9b99feb7a9ad3034ac43c6dbdd452f5f0c4d83f77d9e790f1"},
			"earlier-text":           {read: 37, refused: 397, kinds: "e8c7c1b40b53b5310832b96df4a4a6f3e804f3fa628620aebf22a7205b42c14a"},
			"one-commit-two-answers": {read: 68, refused: 724, kinds: "95959c64bf134e5574b910a568fa4d4bfc46598a187cc1bda610d3307604ceed"},
			"production-and-graph":   {read: 28, refused: 454, kinds: "470381f80ccca43f7c9393e03053eee27492bcfbd4e3d17e2acc6deeaad15979"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 {read: 29, refused: 461, kinds: "1b35a485f73602d42de57b543b3a481ac39c5cea6aa067e3bbe34d79d38b6ca5"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              {read: 29, refused: 461, kinds: "1b35a485f73602d42de57b543b3a481ac39c5cea6aa067e3bbe34d79d38b6ca5"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                {read: 29, refused: 461, kinds: "1b35a485f73602d42de57b543b3a481ac39c5cea6aa067e3bbe34d79d38b6ca5"},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                          {read: 29, refused: 461, kinds: "1b35a485f73602d42de57b543b3a481ac39c5cea6aa067e3bbe34d79d38b6ca5"},
			"production-and-graph with its program check of production as a program check of the graph":                       {read: 28, refused: 454, kinds: "470381f80ccca43f7c9393e03053eee27492bcfbd4e3d17e2acc6deeaad15979"},
			"production-and-graph with its program check of production as an agent of production":                             {read: 38, refused: 444, kinds: "344f26d0521a311cd835162442d2e97af91f7af4b79e17366f91b5c3bb28b13c"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  {read: 30, refused: 456, kinds: "f79e1ba8e0f9ea373e964953df67423a9c41100c9ae00d594144aafa876a3993"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               {read: 30, refused: 456, kinds: "f79e1ba8e0f9ea373e964953df67423a9c41100c9ae00d594144aafa876a3993"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": {read: 30, refused: 456, kinds: "f79e1ba8e0f9ea373e964953df67423a9c41100c9ae00d594144aafa876a3993"},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           {read: 30, refused: 456, kinds: "f79e1ba8e0f9ea373e964953df67423a9c41100c9ae00d594144aafa876a3993"},
			"refusal":    {read: 93, refused: 894, kinds: "f2850def2a97f1c50ce3fb877dffd8cecaaf9e7bcedc2476448f93d856c4b618"},
			"silent-leg": {read: 46, refused: 621, kinds: "134c7cc0d21b3fb261a85aff4d907cc76bda72699ebf4973e5631aef6bb0f5a0"},
			"silent-leg with its matrix dropped at m2": {read: 46, refused: 605, kinds: "446f9f67ca41f6b15bf9ddc1b6fdc3496a4336f55c40c5c9798e93b9ff6499bf"},
			"wait-cycle": {read: 72, refused: 672, kinds: "69081d743a0a0aff5f165e2ecb3e55b7cf3e674578934253cde1618072ae9a83"},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 92, refused: 815, kinds: "2d1aa7be5574505c012c5b2b976a7976372935aca31fc0f4678996069315b148"},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 72, refused: 672, kinds: "69081d743a0a0aff5f165e2ecb3e55b7cf3e674578934253cde1618072ae9a83"},
		},
		"members": {
			"carry": {read: 103, refused: 22, kinds: "863bffd3bc6c02bde295eaa707e167d1bbcec4f7de5319000194c7c2580ce115"},
			"carry with a failure of c0, c1's parent, and a later pass of c2":            {read: 193, refused: 33, kinds: "ecfe057e905cfe313749771cd22d36e8d686158bf947637830f12caa6a91f5c6"},
			"carry with a later failing example planted by no step":                      {read: 132, refused: 26, kinds: "fd50ab1a3e7c8b655f30d909ba43dca497d6d1147a7bbe9fc9a76566c6baaf29"},
			"carry with a later failing example planted on c1 that missed its fault":     {read: 123, refused: 26, kinds: "ce580b37740730db8ea1cd316aeffe67fd6960e3b148b1cdfa278b162b23c3a3"},
			"carry with a later failing example planted on c9, no commit of the history": {read: 132, refused: 27, kinds: "27aaa489223e98266f1ad1368f7450a6e262da1314194e02d10206237a38f835"},
			"carry with c2's failure at 10:30 and a later pass of m2":                    {read: 180, refused: 32, kinds: "b74874bd517e48a0f3d3d123811b89f635032764424e7f56fd95e86ffa102498"},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 180, refused: 32, kinds: "b74874bd517e48a0f3d3d123811b89f635032764424e7f56fd95e86ffa102498"},
			"carry with its failing example planted by no step":                          {read: 108, refused: 21, kinds: "994a2c1ab36eabafb4d505106687b2c69d8935ee3308b7fd91d8905da7a9c0f2"},
			"earlier-text":           {read: 33, refused: 7, kinds: "a06251a9f2940c06b2a527a3118256009a376131f214325e79b4a090d29e2006"},
			"one-commit-two-answers": {read: 150, refused: 21, kinds: "fa4239f521685eb6aa431ae96026a0192f14f32f97ab1bf59c0ff62f662414c5"},
			"production-and-graph":   {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                          {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with its program check of production as a program check of the graph":                       {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with its program check of production as an agent of production":                             {read: 107, refused: 26, kinds: "fb3b1f40519ba207dae74caaf5e7b759a6cac25634e5d66fb3a69c834ab85dc1"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  {read: 104, refused: 26, kinds: "09163bd7d7fed558a69cd64eb4a1b49e84327b7d9a197e675126aa52b55556c4"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               {read: 104, refused: 26, kinds: "09163bd7d7fed558a69cd64eb4a1b49e84327b7d9a197e675126aa52b55556c4"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": {read: 104, refused: 26, kinds: "09163bd7d7fed558a69cd64eb4a1b49e84327b7d9a197e675126aa52b55556c4"},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           {read: 104, refused: 26, kinds: "09163bd7d7fed558a69cd64eb4a1b49e84327b7d9a197e675126aa52b55556c4"},
			"refusal":    {read: 103, refused: 22, kinds: "863bffd3bc6c02bde295eaa707e167d1bbcec4f7de5319000194c7c2580ce115"},
			"silent-leg": {read: 140, refused: 31, kinds: "76f717343b6507ce61cc31c4973abb75cb68b6cb627cb938f1fca4572eb8c846"},
			"silent-leg with its matrix dropped at m2": {read: 140, refused: 31, kinds: "76f717343b6507ce61cc31c4973abb75cb68b6cb627cb938f1fca4572eb8c846"},
			"wait-cycle": {read: 97, refused: 13, kinds: "980a1fd8871c57e58eca7c6d2c24d47d8556ea2d429c5298a7e17d72f7b95e01"},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 127, refused: 17, kinds: "add8d01c453374c641ce908955badecb63445ad74bc715a041931e5d8419f78f"},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 97, refused: 13, kinds: "980a1fd8871c57e58eca7c6d2c24d47d8556ea2d429c5298a7e17d72f7b95e01"},
		},
		"texts": {
			"carry": {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with a failure of c0, c1's parent, and a later pass of c2":            {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with a later failing example planted by no step":                      {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with a later failing example planted on c1 that missed its fault":     {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with a later failing example planted on c9, no commit of the history": {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with c2's failure at 10:30 and a later pass of m2":                    {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"carry with its failing example planted by no step":                          {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"earlier-text":           {read: 107, refused: 193, kinds: "f52f3973872a3c48f09546c90b01916c20925e24b46cf1fe087651bd5415689c"},
			"one-commit-two-answers": {read: 208, refused: 302, kinds: "340c5aa53e75b22db4fae7ebcaa5806b69cf01cc170da2c270f4d7bf815f19a9"},
			"production-and-graph":   {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                          {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with its program check of production as a program check of the graph":                       {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with its program check of production as an agent of production":                             {read: 62, refused: 120, kinds: "62b9da0676405b5b0f99a21a6f04413b63930726b35d6449dfbec4f162a18d4f"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           {read: 77, refused: 129, kinds: "04bf72d610703acdeb1571c689ccc617aea6811a2556463e35ee70987d8f4ccf"},
			"refusal":    {read: 201, refused: 374, kinds: "83b0783fd5327a025b09bcf2355d0fffd1ea7aadbcb3f42e276204805aa11238"},
			"silent-leg": {read: 125, refused: 220, kinds: "6d6791f8fea594c36523f16e4515c8c89e4cfdf9094f82d5d24b945a84c11086"},
			"silent-leg with its matrix dropped at m2": {read: 125, refused: 220, kinds: "6d6791f8fea594c36523f16e4515c8c89e4cfdf9094f82d5d24b945a84c11086"},
			"wait-cycle": {read: 149, refused: 201, kinds: "60f4e112a775ac6eee26ba5a4002d52b63023c2efcccb20fb997ac69eb47cfcb"},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 191, refused: 247, kinds: "2e4e26baa75e6d60a2b8ae8bec317bdca6799e1052c28c3d8a310eae0fde30e5"},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 149, refused: 201, kinds: "60f4e112a775ac6eee26ba5a4002d52b63023c2efcccb20fb997ac69eb47cfcb"},
		},
		"attributes": {
			"carry": {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with a failure of c0, c1's parent, and a later pass of c2":            {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with a later failing example planted by no step":                      {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with a later failing example planted on c1 that missed its fault":     {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with a later failing example planted on c9, no commit of the history": {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with c2's failure at 10:30 and a later pass of m2":                    {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"carry with its failing example planted by no step":                          {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"earlier-text":           {read: 10, refused: 12, kinds: "37f4500a4ae02370a9acc017fc43e72670f7439fa7162380a07c1570a18e63d1"},
			"one-commit-two-answers": {read: 35, refused: 33, kinds: "c1664bdfdf0d2f81d10efa3ab5070c7be6b9760a644ab72b12cf764ed16defaa"},
			"production-and-graph":   {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                          {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with its program check of production as a program check of the graph":                       {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with its program check of production as an agent of production":                             {read: 5, refused: 13, kinds: "2eb001fa1e3c8067b0cdf9535b835599af7fe6fd8d75039ee0a53e29cb2a8e54"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           {read: 8, refused: 16, kinds: "ad5ad06ca3db8aa1038b2439b9f4537c27f7b4d15b98264c70ea00c8895e96ed"},
			"refusal":    {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"silent-leg": {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"silent-leg with its matrix dropped at m2": {read: 10, refused: 16, kinds: "9b9d9d6bd5cadabe14732e9fa35a25d65c235aba82300f2a492e21e038807a7d"},
			"wait-cycle": {read: 22, refused: 24, kinds: "d014cec22725db1b2b70af9ccc3eb6ca056809af23d1220dd897661a445ffc45"},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 27, refused: 31, kinds: "54256b40a5305c3f5792ba9539942e5d9cf47e77295769c4c2904bc3d3c7d5ae"},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 22, refused: 24, kinds: "d014cec22725db1b2b70af9ccc3eb6ca056809af23d1220dd897661a445ffc45"},
		},
		"refusals": {
			"carry": {read: 14, refused: 0, kinds: "61f0612e3b652329d20c30aea0457638cde75d7c641c3cfe060a2be5c8a4f287"},
			"carry with a failure of c0, c1's parent, and a later pass of c2":            {read: 18, refused: 0, kinds: "1c7607f66a242ad9eab9f415df391361653f4c84af4db95e2122f07ee44eefc1"},
			"carry with a later failing example planted by no step":                      {read: 16, refused: 0, kinds: "c3aaa479db71d909d2e6f62ca5a9d217a667fa0f319ade48735cf8c61ff94bdf"},
			"carry with a later failing example planted on c1 that missed its fault":     {read: 16, refused: 0, kinds: "c3aaa479db71d909d2e6f62ca5a9d217a667fa0f319ade48735cf8c61ff94bdf"},
			"carry with a later failing example planted on c9, no commit of the history": {read: 18, refused: 0, kinds: "1c7607f66a242ad9eab9f415df391361653f4c84af4db95e2122f07ee44eefc1"},
			"carry with c2's failure at 10:30 and a later pass of m2":                    {read: 16, refused: 0, kinds: "c3aaa479db71d909d2e6f62ca5a9d217a667fa0f319ade48735cf8c61ff94bdf"},
			"carry with c2's failure between c1's pass and c1's failing example":         {read: 16, refused: 0, kinds: "c3aaa479db71d909d2e6f62ca5a9d217a667fa0f319ade48735cf8c61ff94bdf"},
			"carry with its failing example planted by no step":                          {read: 12, refused: 0, kinds: "12e1da3ead842f7a8c9b1195872f7401a1e2c54b1d59b257e73cc91232efe904"},
			"earlier-text":           {read: 5, refused: 0, kinds: "a33805cfe8452895be97edd6552029f20be635f67944c9c8978004f84b502649"},
			"one-commit-two-answers": {read: 20, refused: 0, kinds: "882c9656cf8a964d384d9d839b6a2747cc8323c2f0c87b59bf06af39bd5bb51e"},
			"production-and-graph":   {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 {read: 3, refused: 0, kinds: "12b0f0dcaefb10c02a83aa9adb025978ddb5512dc04eb39df6811c6a6bf9770c"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              {read: 3, refused: 0, kinds: "12b0f0dcaefb10c02a83aa9adb025978ddb5512dc04eb39df6811c6a6bf9770c"},
			"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                {read: 3, refused: 0, kinds: "12b0f0dcaefb10c02a83aa9adb025978ddb5512dc04eb39df6811c6a6bf9770c"},
			"production-and-graph with CI's refusal at c1 of its query of the graph":                                          {read: 3, refused: 0, kinds: "12b0f0dcaefb10c02a83aa9adb025978ddb5512dc04eb39df6811c6a6bf9770c"},
			"production-and-graph with its program check of production as a program check of the graph":                       {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with its program check of production as an agent of production":                             {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           {read: 4, refused: 0, kinds: "4209fcf0702262840e93f40abf4df012722838494df760ff04aa1ce529a86f16"},
			"refusal":    {read: 13, refused: 0, kinds: "f1d44c28ce88355243a1de9bca7e4435def0ffa65dbc16ff9125806046fdc8d4"},
			"silent-leg": {read: 10, refused: 0, kinds: "52bfb770ff4c153e61a09af372bf62bc11a32ace160c060736324150fccb13d6"},
			"silent-leg with its matrix dropped at m2": {read: 10, refused: 0, kinds: "52bfb770ff4c153e61a09af372bf62bc11a32ace160c060736324150fccb13d6"},
			"wait-cycle": {read: 18, refused: 0, kinds: "1c7607f66a242ad9eab9f415df391361653f4c84af4db95e2122f07ee44eefc1"},
			"wait-cycle with a second, passing check of a-message-survives-a-restart": {read: 24, refused: 0, kinds: "f8a205b2323f27564e2518c6f1d9fe1e2d5855538ec670664330285500914827"},
			"wait-cycle with the outcomes of its two first runs swapped":              {read: 18, refused: 0, kinds: "1c7607f66a242ad9eab9f415df391361653f4c84af4db95e2122f07ee44eefc1"},
		},
	}
}

// sweepFloor is the floor of one sweep of one base: the inputs that the reader read, as the base or
// as another state, and the inputs that it refused, as sweepAgainst counted the inputs that it
// judged (sweepCount), and kinds, the hex sha256 of the kind of each judged input in their order,
// one byte each, 'r' read and 'x' refused (sweepCount.kinds).
type sweepFloor struct {
	read, refused int
	kinds         string
}

// markReadingsFloor is the count of readings of a member left out that
// TestAMemberLeftOutOfAFileIsReadByItsMark read when the floor was written.
const markReadingsFloor = 82

// atFloor fails the test unless, of the inputs of the base name that sweepAgainst judged (n), the
// reader read at least the floor's count and refused at least the floor's count, and their kinds,
// in their order, have the floor's digest (sweepFloors). It counts no input that the sweep built and
// did not judge.
func atFloor(t *testing.T, sweep, name string, n sweepCount) {
	t.Helper()
	floor, held := sweepFloors()[sweep][name]
	if read := n.same + n.moved; !held || read < floor.read || n.refused < floor.refused {
		t.Errorf("the %s sweep judged %d inputs of %s that the reader read and %d that it refused, and its floor is %d read and %d refused (held %t; sweepFloors)",
			sweep, read, name, n.refused, floor.read, floor.refused, held)
	}
	if kinds := fmt.Sprintf("%x", sha256.Sum256(n.kinds)); held && kinds != floor.kinds {
		t.Errorf("the %s sweep judged the %d inputs of %s, in their order, as kinds whose digest is %s, and its floor's digest is %s (sweepFloors): an input moved from one kind to the other, or the sweep judged other inputs",
			sweep, len(n.kinds), name, kinds, floor.kinds)
	}
}

// floorsCover fails the test unless the sweep has a floor of inputs for each base of bases and for
// no other (sweepFloors).
func floorsCover(t *testing.T, sweep string, bases map[string]fstest.MapFS) {
	t.Helper()
	if got, want := slices.Sorted(maps.Keys(sweepFloors()[sweep])), slices.Sorted(maps.Keys(bases)); !slices.Equal(got, want) {
		t.Errorf("the %s sweep has floors of inputs for the bases %q, and the sweep reads the bases %q (sweepFloors)", sweep, got, want)
	}
}
