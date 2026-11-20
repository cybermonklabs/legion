# A secret behind the airlock

Read this when a name holds `agent-secrets-placeholder-` and 32 letters and digits, and before you retry a call the broker's proxy refused.

A deployment can keep a secret's value back. The name then holds `agent-secrets-placeholder-` and 32 letters and digits, and the broker's proxy, which the run's `HTTPS_PROXY` names, puts the real value into the call to the secret's service. A shape check reads only the placeholder.
The proxy swaps the placeholder for the real value in the headers the secret's rule names. Three things follow for the command you write:

- Make every call inside the `agent-secrets NAME -- <command>` run. A tool that caches a token across runs, or calls outside the run, stops working. Ask for the grant in the run that spends it.
- Pass no value you obtained some other way. The proxy refuses a request whose headers already carry the real value.
- The placeholder is that one grant's, and works only while the grant lives.

A refusal comes from the proxy, not from the service:

- An HTTP error naming the rule that refused it: a TLS name or `Host` that is not the host the command asked for, a method or a path outside the entry's `allow`, a path with a `.` or `..` segment, an `X-HTTP-Method-Override`, `X-HTTP-Method` or `X-Method-Override` header where the entry lists methods, a `CONNECT` or an `h2c` upgrade inside the proxied connection, plain `http://` to a host an entry binds, or a private address.
  gRPC clients work through the proxy.
- `407` once the grant ends, on every call to any host. A call still running at that moment (a WebSocket, a gRPC stream, a long poll) is ended with the grant. A tunnel to a host no entry binds carries no value, so it runs to its idle limit instead.
- `503` when the broker could not read the grant at all. The grant has not ended and the call can be made again.

A retry fixes neither of the first two, and neither does another name.
A `407` needs a new grant: ask again.
A rule's refusal needs a change to the rules file in the deployment's own repository, which a person reviews.
Report the refusal as you report a service out of reach: the command you ran and the output you saw.

Two calls are not refused by the proxy. A call to a host no entry binds goes out unread, and a placeholder in a header the entry does not name goes out as it is, so the service refuses the placeholder and its own error is what you see.
