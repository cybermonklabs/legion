# Tell the controller about the ask

Part of `skill://legion-worker/references/findings.md`. Read it once a finding ask is open.

The answer reaches only the ask's followers. Once the ask is open, `envoy_publish` `dispatch://<REPORT KEY>/ask/<id>` to the controller topic with `expects_reply: false`; the controller files what the answer asks for, and you file nothing. The controller itself skips this step, since it follows each ask it opens. When the listener only kept that publish, do nothing more for an ask on the report issue: the controller's catch-up finds it. An ask on the report issue blocks nothing of your own issue; one on your own issue holds its acceptance. Name the ask in your summary. A production check look names the message or the ask in its result and summary.
