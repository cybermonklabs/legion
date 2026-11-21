# operator-token-minters/become-rights.jq: follow_becomes's rights (reviews.sh), read from a
# ServiceAccount a line (namespace, name, the right of its first finding, tab-separated): in each
# namespace, create pods, create and get pods/exec, and get, list and watch secrets, and for each
# ServiceAccount by name, create serviceaccounts/token and impersonate; as rights_json writes
# rights, each with `targets`, the ServiceAccounts it makes its holder.
      [split("\n")[] | select(length > 0) | split("\t") | {ns: .[0], name: .[1], why: .[2]}] as $sas
      | def become($list): $list | map("system:serviceaccount:\(.ns):\(.name), which can \(.why)") | join(", or ");
        def right($verb; $res; $sub; $ns; $name; $list):
          {verb: $verb, group: "", resource: $res, sub: $sub, ns: $ns, name: $name,
           key: ([$verb, "", $res, $sub, $ns, $name] | join("|")),
           label: ("\($verb) \($res)\(if $sub != "" then "/" + $sub else "" end)\(if $name != "" then " " + $name else "" end) in \($ns), so it can become " + become($list)),
           targets: [$list[] | "\(.ns):\(.name)"]};
        [(([$sas[].ns] | unique)[] as $n | [$sas[] | select(.ns == $n)] as $in
          | right("create"; "pods"; ""; $n; ""; $in), right("create"; "pods"; "exec"; $n; ""; $in), right("get"; "pods"; "exec"; $n; ""; $in),
            right("get"; "secrets"; ""; $n; ""; $in), right("list"; "secrets"; ""; $n; ""; $in), right("watch"; "secrets"; ""; $n; ""; $in)),
         ($sas[] | right("create"; "serviceaccounts"; "token"; .ns; .name; [.]), right("impersonate"; "serviceaccounts"; ""; .ns; .name; [.]))]
