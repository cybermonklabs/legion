# operator-token-minters/code-iam.jq: the step "code: IAM" (step-code-iam.sh) reads one preview,
# `pulumi preview --json --show-sames`, named by $file: a `binding` record per member an IAM
# resource grants (the kind and id of what it grants on, a secret's project, the role, the member
# and the resource's URN), a `role` record per custom role it declares, `unknown` for a value the
# preview does not know (computed at apply), `unread` for an IAM-like resource type this check does
# not read, and `empty` or `nosames` for a preview that cannot show every binding.
include "rec";
        def sentinel: "04da6b54-80e4-46f7-96ec-b56ff0331ba9";
        def unknown_: type == "string" and contains(sentinel);
        def binding($urn; $scope; $role; $member):
          if ($role | unknown_) or $role == null then ["unknown", $urn, "role"]
          elif ($member | unknown_) or $member == null then ["unknown", $urn, "member"]
          elif any($scope[]; unknown_) then ["unknown", $urn, "resource"]
          else ["binding", $scope[0], $scope[1], $scope[2], $role, $member, $urn] end;
        if (.steps | type) != "array" or (.steps | length) == 0 then ["empty", $file] | rec
        elif ((.changeSummary.same // 0) > 0) and ([.steps[] | select(.op == "same")] | length) == 0 then ["nosames", $file] | rec
        else .steps[] | select(.newState != null) | .urn as $urn | .newState.type as $t | (.newState.inputs // {}) as $i
        | if ($t | test("auditconfig|memberremove"; "i")) or ($t | test("^gcp:iam/"; "i")) then empty
          elif ($t | test("^gcp:(projects|organizations)/iamcustomrole:"; "i")) then
            ["role", (if ($t | test("^gcp:projects/"; "i")) then "projects/\($i.project)/roles/\($i.roleId)"
                      else "organizations/\($i.orgId)/roles/\($i.roleId)" end), (($i.permissions // []) | join(",")), $urn] | rec
          else
            (if ($t | test("^gcp:projects/iam(member|binding|policy):"; "i")) then ["project", ($i.project // ""), ""]
             elif ($t | test("^gcp:organizations/iam(member|binding|policy):"; "i")) then ["organization", ($i.orgId // ""), ""]
             elif ($t | test("^gcp:folder/iam(member|binding|policy):"; "i")) then ["folder", ($i.folder // ""), ""]
             elif ($t | test("^gcp:serviceaccount/iam(member|binding|policy):"; "i")) then ["serviceaccount", ($i.serviceAccountId // ""), ""]
             elif ($t | test("^gcp:secretmanager/secretiam(member|binding|policy):"; "i")) then ["secret", ($i.secretId // ""), ($i.project // "")]
             elif ($t | test("^gcp:storage/bucketiam(member|binding|policy):"; "i")) then ["bucket", ($i.bucket // ""), ""]
             else null end) as $scope
          | if $scope == null then
              if ($t | test("^gcp:(projects|organizations|folder|serviceaccount|secretmanager|storage)/"; "i"))
                 and (($t | split(":"))[1] // "" | test("iam|acl|accesscontrol"; "i"))
              then ["unread", $urn, $t] | rec else empty end
            elif ($t | test("policy:"; "i")) then
              if ($i.policyData | unknown_) or $i.policyData == null then ["unknown", $urn, "policy"] | rec
              else ($i.policyData | fromjson | .bindings[]?) as $b | ($b.members // [])[] as $m | binding($urn; $scope; $b.role; $m) | rec end
            elif ($t | test("binding:"; "i")) then
              if ($i.members | unknown_) then ["unknown", $urn, "member"] | rec
              else ($i.members // [])[] as $m | binding($urn; $scope; $i.role; $m) | rec end
            else binding($urn; $scope; $i.role; $i.member) | rec end
          end
        end
