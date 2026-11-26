package api

// 20261126011643 admission-signals: #5603 -- admission by signals: the daemon receives the inputs of
// the signals that admission below P0 is judged by, keeps them in its store, computes each signal by
// the one shared definition (internal/signals), shows each in the state, and a person turns
// admission by signals on and sets each limit while the daemon runs.
//
// The launcher wire gains two members, so that the kills signal counts every kill for which an issue
// pod's launcher writes its line `launcher: memory guard: ended`, the kill of a process of no role
// included, as the replay counts those lines. The daemon's answer to the hello of every issue pod's
// launcher carries `noRoleKills` (`shimwire.LauncherHelloAck.NoRoleKills`), and its answer to the
// controller pod's launcher never does, since no kill of the controller's pod counts.
// `launcher_memory_kill` gains `noRole` (`shimwire.LauncherMemoryKill.NoRole`), set on the report of
// a process of no role of an issue pod: the test database's server, or a process a `kubectl exec`
// started. The launcher sends such a report once, on the connection of the first role of the pod
// whose answer carried `noRoleKills` beside what that launcher's reports of a role need there
// (`memoryGuard` for a gVisor pod's, `kernelKills` for a microVM pod's), and drops it with a line
// where connections are up and none carried it (internal/launcher's noRoleReports). The daemon
// credits the report to the issue and the pod of the connection that carried it, never to anything
// the report says, keeps it with every other kill of the project's issue pods under the same event
// id `memory-guard-kill:<guard>:<sequence>`, and tells no role of it (workflow's memoryGuardKill).
//
// The skew is safe both ways. A daemon of an earlier release sends no `noRoleKills`, so a launcher
// of this release sends it no report of no role, which such a daemon would tell the role whose
// connection carried it as a kill of that role's own process; the launcher drops each such report
// with a line beside its `ended` line instead. A launcher of an earlier release ignores
// `noRoleKills` and reports no kill of no role, and the daemon counts what it does report, each
// kill of a role's process, so the kills signal counts the kills of no role only of the pods whose
// launcher is of this release. Both members are left out when false, so the controller pod's answer
// and every report of a role's process are the bytes an earlier release writes.
//
// Each host of a session of the project reports its model requests, for the provider's headroom
// and refusals, on a route of its own, `POST /legion/v1/claims/model-requests`
// (claim.ModelRequestsRequest), answered `{"kept": [<id>, ...]}` (claim.ModelRequestsResponse,
// AnswerModelRequests): every id of the batch the daemon now holds, new or held already, or keeps
// as nothing because the request ended before the hour any read reaches, which the host forgets,
// sending the rest again under the same ids. The body is the caller of a turn outcome
// — `claimToken`, `sessionId` and `secret`, or `bootToken` — with `generation` and `requests`, 1 to
// 200 of `{id, session, model, endedAt, uncachedInputTokens?, refusal?}`, refusal one of `own`,
// `provider` and `unknown`, and `retriedRefusalsReported?`, whether the host reports each HTTP 429
// its transport retries by itself; it is decoded strictly and names no project, which is the
// claim's. The Claude Code bridge sends true, since each such attempt is an `api_retry` line it
// reports; the Oh My Pi plugin sends what its host offers, false on a host that gives an extension
// no event for such an attempt; a body without the member reads as false. While a request of a
// batch that said false is in the hour the refusals read, the provider's refusals are not
// recorded, and its headroom reads every batch alike. The operator's controller, which holds no
// claim the daemon supervises, names its own token, `legion-<project>-controller`, with its
// registration's session and secret, checked against the controller record. A host before this
// entry reports none and still sends its turn outcome, whose body and answer are unchanged; the
// provider's record holds no minute such a host's turn overlaps (ModelRequestsContract,
// record.Postgres.ProviderMinutes).
//
// Every launch of a role's pane or pod, and of the controller's, the daemon's and the operator's,
// carries the project's two refusal patterns by which its host classes each refused request:
// `LEGION_SIGNAL_OWN_REFUSAL` (`signal_own_refusal_pattern`) and `LEGION_SIGNAL_PROVIDER_REFUSAL`
// (`signal_provider_refusal_pattern`), each where the project sets it and neither where it does
// not (runtime.RefusalEnvironment). A host tests the own pattern first, the provider's second, and
// classes a refusal neither matches as unknown; it compiles each as Go's
// `regexp.Compile("(?s)" + pattern)` and JavaScript's `new RegExp(pattern, "su")`, under which the
// loader's subset reads alike (config.SharedPattern). A host of an earlier release reads neither
// variable. An operator's `runtime.tmux.pane_env` and `runtime.kubernetes.pod.env` may not name
// either.
//
// A person turns admission by signals on or off and sets or clears each series' limit while the
// daemon runs, on the controller's `POST /legion/v1/project/signals` (ProjectSignalsRequest:
// `grantId`, `action` one of `on`, `off`, `set-limit` and `clear-limit`, `signal` and `part` naming
// a limit's series as an entry does, `limit`, and the person's cited `reason`, which every action
// requires), answered `{changed, settings}` (ProjectSignalsResponse, AnswerProjectSignals), and on
// the operator's `POST /legion/v1/operator/project/signals` (OperatorSignalsRequest, `legion
// project signals`), the same body with no grant. `settings` is `{on, switch, limits}`: who set the
// switch last, when and why (`{setBy, at, reason}`, `setBy` the form of a pause's actor, null before
// the first change), and each of the six series with its `limit` (null for none) and its last change
// `set`. A refused change answers SIGNAL_SETTING_INVALID with the reason and changes nothing.
//
// `GET /legion/v1/state` gains `admission.signals` (SignalsReading): `on`, `switch`, `series`, each
// of the six series in the order of signals.Names() as `{signal, part, value, absent, limit,
// limitSet, holds, since}`, and `provider.minutes`, each whole minute of the last hour as
// `{at, held, uncachedInputTokens, tokenLimit, ownRefusals, providerRefusals}`; and
// `admission.signalChanges`, the newest 50 entries of the record of changes newest first, `[]`
// before the first, each exactly `{id, at, signal, part, what, value, absent, setBy, reason}`
// (SignalChange). A caller below this entry's contract is served the state without both keys
// (withoutAdmissionSignals, the band below): `signalChanges` is left off rather than emptied
// (Admission.signalChangesHidden), since a strict reader of an earlier contract refuses it either
// way.
//
// admissionSignalsContract is this entry's id, by which every row that admission by signals adds to
// a table of this package names the entry.
const admissionSignalsContract = 20261126011643

func init() {
	registerEntry(contractEntry{id: admissionSignalsContract, slug: "admission-signals",
		bands: []stateNarrowing{
			{contract: admissionSignalsContract, state: withoutAdmissionSignals, dropped: "admission.signals and admission.signalChanges"},
		},
		// The report of model requests and the controller's change of admission by signals each
		// arrive with their answer, so no contract below this entry records either, and each answer
		// is served whole to every contract that reads it.
		answers: []Answer{
			{ID: AnswerModelRequests, Routes: []string{"POST /legion/v1/claims/model-requests"}, Since: admissionSignalsContract},
			{ID: AnswerProjectSignals, Routes: []string{"POST /legion/v1/project/signals"}, Since: admissionSignalsContract},
		},
		identityAnswers: []AnswerID{AnswerModelRequests, AnswerProjectSignals},
	})
}

// ModelRequestsContract is this build's number of the admission-signals entry: the first plugin
// contract whose host reports each model request of its sessions (POST
// /legion/v1/claims/model-requests). A claim registered below it runs a host that reports none, so
// the provider's record holds no minute its turns overlap (record.Postgres.ProviderMinutes).
func ModelRequestsContract() int { return entryNumber(admissionSignalsContract) }
