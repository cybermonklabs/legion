# INFRA-1196 re-review round 2 notes (verified at 776aa368, git objects)
Subagents' self-reported models: VerifyTree/VerifyReads/VerifyPage/VerifyE2E all "Claude Opus, family Claude (Anthropic)" -> same model family.

FIXED: 0 1 3 4 5(by Shift+K/C/f) 6 8 9 10 11 12 13 15 18 19 20 21 22 23 26 27 28(mostly) 29 30 32 33 34 36 41 42 43 44 45 46 47
PARTIAL/NOT: 2 7 14 16 17 24 25 31 35 37 38 39 40 48 49(prose only)

Stand (Medium unless noted):
- [2] zoomedRows gives every row trail `under` -> dup keys at <=1024 (tree-fold.ts:193-229)
- NEW `.` zoom-in no-op at <=1024: tree-zoom-in onZoom then onReveal; goToNode zoomed branch resets zoom to parent (useTreeKeymap.ts:341-342, IssueTree.tsx:222-227)
- NEW path keys relative to zoom root at full width: pins/tabstop lost on zoom (IssueTree.tsx:136 treeRows(rooted) empty trail vs zoomedRows absolute)
- [7] Minor: /spec#b- still suffix match (issue-address.ts:102)
- NEW Minor latent: bringHome re-roots by order not ring member (issue-tree.ts:327-360)
- [14] Cancel/Escape enabled while save out -> refusal shown to nobody (BlockedByControl.tsx:113,127-134; useIssueBlockedBy.ts:32 inline); stale isError re-shown on reopen (:58-61,142); server words carry `blocked_by`
- [16] Words/Everything still navigate to /conversation|/activity -> Tree preset, closes Spec doc, drops ?v= #b- (IssuePage.tsx:202-216, issue-address.ts:61-66,138,141). Words is default-selected so pressing it ejects.
- [17] /issues/K/artifacts/<plan-slug> after Plan activated -> drawn.plan and drawn.selected same doc -> two editors; 512-test circular (drawn-documents.ts:79)
- NEW claim chip ", free for another agent from in 3 minutes (...)" (ClaimChip.tsx:255, page-words.ts:551)
- NEW draft lost when margin shown again >=1280: ConversationTab moves parents (IssuePage.tsx:301 vs :531-534)
- NEW Minor: on /activity picking Questions keeps activity panel in column (IssuePresetPanels.tsx:193-199)
- [24] words: node (page-words :283,:455,:471, page-structure:69), Origin not named/Origins disagree (:301,:309), Nobody read it back (:305) on every head (IssueThreeThings.tsx:155-166), Not judged (:111), Out of date double meaning (:107 vs :558), "1 more nodes", Message route (:493), Issue keys (page-structure:107), "Add/Edit blocked by of KEY" (BlockedByControl.tsx:94), raw topic "via notifications.dispatch.>" (SubscribedAgents)
- [25] status indicator "KEY set to in_progress" raw code (useIssueHeaderWrites.ts), statusText exists board-model.ts:32
- [15] Minor stale comment IssueActionsMenu.tsx:22-25
- R1 (missed in round 1): oneLine (<=1024) truncate cuts words in rows (NodeRow.tsx:120/128,:183/192; TreeRows.tsx:106) ; c-reflow moved rows to assertFitsItsBox which hides it; check text still says assertWhole
- [31] HIGH-ish: fixture at 5f86d6c9; base now 3956a7a6+fold; 8b8fac66 adds combobox "Hand this question to"; e35c6901 (in fold) adds button "Decline to answer"; recordedAtHead==commit
- [35] Minor: e2e redeclares presetPath/blockNodeId/MarginSection/ConversationState
- [37] NOT FIXED: c-open-questions waits only for root + doc row (detail-built), not block rows (issue-layout.e2e.ts:914-931)
- [38] count floor met by block rows; comment/message/result bodies skippable (:1474-1489)
- [40] repo value unasserted; badgeValue never checks text; assignee via innerText (:1077,:1087,:1131-1136)
- [48] Minor: reference-targets count taken at first poll match (live.e2e.ts:729-738)
- [49] floor 36 at md is the app idiom -> accept; reply wrong; body retired list should name -> I fix body
- Census: rollout #2 new-page/old-server is wrong (block rows draw blank: tree-drafts.ts:370, chipOf ''); stale line numbers; loadtest consumers unlisted -> body prose I fix
- Minor cost: claim/release refresh ["issue"] prefix hits every issue-prefixed read (event-refresh.ts:907)
- IssuePage:443 two address models: accept as structure, routed by architect.

R1 expanded (architect 15:05Z): second hole: c-tree row issue-layout.e2e.ts:352 has no @phone tag, run line --project=chromium only, while 12 other rows in that file carry @phone, so 'no row truncates a word' never ran at <=1024. Expected at next head, verify all three: (1) page cuts one-line row at word boundary/sentence end with ellipsis; (2) cutsAWord (:339) applied to one-line rows beside assertFitsItsBox, false :1606 comment corrected; (3) the row runs at phone width (@phone tag or separate row). Verification doc c-tree run line/negative_control updated by architect.

Approval scope (architect 16:35Z): any approval must say what it covers and name what was NOT read: visual baselines (no docker), merge-group shard timing, the fold's main-side content read as main's, re-recorded fixtures I did not regenerate myself.
