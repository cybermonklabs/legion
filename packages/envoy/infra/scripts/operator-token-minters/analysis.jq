# operator-token-minters/analysis.jq: analyze's records (iam-analysis.sh) from Policy Analyzer's answer,
# with impersonation: a `row` per binding and identity, a `chain` per impersonator of a service
# account holder through any number of service accounts, and one `error` for an answer that cannot
# be read as complete. $wi, $gsa, $bwi and $broker name the designed links, as analyze says: each an
# account's own Workload Identity member acting as that account.
include "rec";
      def sa_email: if test("^serviceAccount:[^@\\[]+@") then sub("^serviceAccount:"; "") else empty end;
      if (.mainAnalysis | type) != "object" then ["error", "the answer holds no mainAnalysis"] | rec
      elif .mainAnalysis.fullyExplored != true then
        ["error", "the analysis was not fully explored, so a holder it did not reach is not listed: \([.mainAnalysis.nonCriticalErrors[]?.cause // empty] | join("; "))"] | rec
      elif any((.serviceAccountImpersonationAnalysis // [])[]; .fullyExplored != true) then
        ["error", "an impersonation analysis was not fully explored, so an impersonator it did not reach is not listed"] | rec
      else
        [(.serviceAccountImpersonationAnalysis // [])[]
          | (((.analysisQuery.resourceSelector.fullResourceName // "") | capture("/serviceAccounts/(?<e>[^/]+)$").e) // "") as $target
          | if $target == "" then {bad: true}
            else .analysisResults[]? as $r
              | ([$r.identityList.groupEdges[]?.sourceNode]) as $expanded
              | $r.identityList.identities[]?.name as $id
              | {target: $target, id: $id, role: ($r.iamBinding.role // ""), attached: ($r.attachedResourceFullName // ""),
                 expanded: any($expanded[]; . == $id)}
            end] as $imp
        | if any($imp[]; .bad == true) then
            ["error", "an impersonation analysis names no service account, so who impersonates which holder cannot be read"] | rec
          else
            def designed($l): ($l.id == $wi and $l.target == $gsa)
              or ($bwi != "" and $l.id == $bwi and $l.target == $broker);
            # chains(E; SEEN): each chain of impersonators of the service account E, innermost first,
            # never through a principal twice.
            def chains($email; $seen):
              $imp[] | select(.target == $email) | . as $l | select(any($seen[]; . == $l.id) | not)
              | [$l], (($l.id | sa_email) as $next | [$l] + chains($next; $seen + [$l.id]));
            .mainAnalysis.analysisResults[]? as $r
            | ([$r.identityList.groupEdges[]?.sourceNode]) as $expanded
            | ([$r.accessControlLists[]?.accesses[]?.permission // empty] | unique | join(",")) as $perms
            | $r.identityList.identities[]?.name as $id
            | (["row", $id, ($r.iamBinding.role // ""), ($r.attachedResourceFullName // ""), $perms,
                (if any($expanded[]; . == $id) then "expanded" else "" end), ($r.iamBinding.condition.expression // "")] | rec),
              (($id | sa_email) as $email | chains($email; [$id]) as $c | ($c | last) as $o
               | ["chain", $o.id, $o.role, $o.attached, $perms, (if $o.expanded then "expanded" else "" end),
                  (if designed($o) then "designed" else "" end), ($r.iamBinding.role // ""), ($r.attachedResourceFullName // ""), $id,
                  ($o.id + ([$c | reverse[] | " -> impersonates serviceAccount:\(.target) (\(.role))"] | join(""))
                   + " -> " + ($r.iamBinding.role // ""))] | rec)
          end
      end
