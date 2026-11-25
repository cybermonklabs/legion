# operator-token-minters/fleet.jq: fleet_memberships's records (step-workload-identity.sh) from the
# GKE Hub API's pages, read with -s: an `unreachable` record naming the locations the listing could
# not reach, when there are any, then a `member` per membership (its id and location, and for a GKE
# cluster its project, location and name).
include "rec";
      ([.[].unreachable[]?] | unique) as $unreachable
      | (if ($unreachable | length) > 0 then ["unreachable", ($unreachable | join(", "))] | rec else empty end),
        (.[].resources[]? as $m
         | (($m.name // "") | capture("/locations/(?<l>[^/]+)/memberships/(?<id>[^/]+)$") // {l: "", id: ($m.name // "")}) as $n
         | (($m.endpoint.gkeCluster.resourceLink // "")
            | capture("^//container\\.googleapis\\.com/projects/(?<p>[^/]+)/(locations|zones)/(?<l>[^/]+)/clusters/(?<c>[^/]+)$")
            // {p: "", l: "", c: ""}) as $g
         | ["member", $n.id, $n.l, $g.p, $g.l, $g.c] | rec)
