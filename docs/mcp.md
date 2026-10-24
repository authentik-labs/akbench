# MCP server

`cmd/apiserver` also serves a read-only [MCP](https://modelcontextprotocol.io)
server at `/api/v1/mcp` (streamable HTTP, stateless) so an LLM can analyze
benchmark results: deployments, test cases and runs (plus their events), the
compare table, k6 metric and CPU/memory time series, trace, CPU profile and
log breakdowns, firing alerts, and raw PromQL. Every tool goes through the
same GET handlers (and input validation) the UI uses; none can create, change,
delete or run anything.

By default it requires the same bearer token (static or OIDC) as the rest of
the API, e.g. for Claude Code:

```sh
claude mcp add --transport http akbench https://<host>/api/v1/mcp \
  --header "Authorization: Bearer $AKBENCH_TOKEN"
```

With `--oidc-issuer` set, MCP clients can also sign in with OAuth through
that provider, as the web UI's own public client. There's no Dynamic Client
Registration, so pass the client ID yourself:

```sh
claude mcp add --transport http akbench https://<host>/api/v1/mcp \
  --client-id <--oidc-client-id> --callback-port 33418
```

An unauthenticated MCP request gets a 401 whose challenge names RFC 9728
metadata at `/.well-known/oauth-protected-resource/api/v1/mcp`, which lists
`--oidc-issuer` as the authorization server. The client runs authorization
code + PKCE there and presents the access token, which akbench checks like
any other web UI token (audience `--oidc-client-id`,
`--oidc-required-group`). For this to work, the provider must also allow
the MCP client's loopback redirect URI, e.g. `http://localhost:33418/callback`.
In authentik, add it as a strict redirect URI, or as the regex
`^http://(localhost|127\.0\.0\.1):\d+/callback$` to allow any port. The
metadata's `resource` is the URL the request came in on (honoring
`X-Forwarded-Proto`/`-Host`) unless `--mcp-resource-url` sets it.

The challenge and metadata add `offline_access` to the web UI's scopes, so
the client gets a refresh token and renews its access token at the provider
without signing in again. In authentik, add the `offline_access` scope
mapping to the provider's selected scopes, or it won't issue one.

With `--mcp-idp-issuer` (and `--mcp-resource-url`) set, it uses MCP
Enterprise-Managed Authorization (SEP-990, ID-JAG) instead and accepts
nothing else. akbench is its own authorization server, with the origin of
`--mcp-resource-url` as its issuer (e.g. `https://bench.example.com`, no
trailing slash). The MCP client signs
the user in at the enterprise IdP and exchanges the ID token there for an
ID-JAG addressed to that issuer (RFC 8693). It then trades the ID-JAG at
akbench's `/oauth/token` for a one-hour access token (RFC 7523 JWT bearer
grant, `client_secret_post` or `client_secret_basic`).

akbench serves RFC 9728 metadata at
`/.well-known/oauth-protected-resource/api/v1/mcp` and RFC 8414 metadata at
`/.well-known/oauth-authorization-server`. It accepts an ID-JAG only if all
of these hold:

- its `typ` is `oauth-id-jag+jwt`
- it's signed by `--mcp-idp-issuer` (keys from its OIDC discovery)
- its `aud` is akbench's issuer and its `client_id` is `--mcp-client-id`,
  the client that authenticated with `BENCH_MCP_CLIENT_SECRET`
- its `resource` is the MCP URL
- it's unexpired and its `jti` hasn't been used before

The access token never carries more scopes than the ID-JAG's `scope` claim.
To use MCP it must carry `--mcp-scopes`, if set. Calls are attributed to
`<sub> via <act.sub>`, the agent named by the ID-JAG's `act` claim.

Access tokens are signed with a key generated at startup. A restart logs MCP
clients out, and the `jti` replay cache lives in memory, so both assume a
single apiserver replica. An access token opens nothing else in the API.

[`hack/mcp-ema-client`](../hack/mcp-ema-client/main.go) runs that whole flow
from the client side. It prints every token, checks the ID-JAG against the
draft's required header and claims, then lists (and optionally calls) the
tools. It's meant for testing an IdP's ID-JAG implementation, such as
authentik's:

```sh
go run ./hack/mcp-ema-client -insecure -mcp-url https://localhost:8443/api/v1/mcp \
  -idp-issuer https://idp.example.com/application/o/enterprise/ -idp-client-id <id> \
  -mcp-client-id <id> -mcp-client-secret <secret> [-id-token <jwt>] [-call list_test_cases]
```

## Example: authentik as the IdP

[`hack/mcp-ema-client/authentik-blueprint.yaml`](../hack/mcp-ema-client/authentik-blueprint.yaml)
sets up authentik's `akbench-ema-idp` provider as the enterprise IdP. What
it may mint ID-JAGs for lives on an authentik system agent, which the
ID-JAG's `act` claim names.

1. Import the blueprint, and copy the `akbench-ema-idp` client secret.
2. In authentik's admin interface, under Directory > Agents (an enterprise
   feature), create an agent with no parent user. Edit it to add the
   `akbench EMA IdP` application and set these ID-JAG fields:
   - audience: akbench's issuer, the origin of `--mcp-resource-url` without
     a trailing slash (`https://localhost:8443`)
   - client ID: `akbench-mcp`
   - resources: `https://localhost:8443/api/v1/mcp`
3. Start akbench as the MCP authorization server, trusting that IdP. Pick
   any secret for the MCP client:

   ```sh
   BENCH_MCP_CLIENT_SECRET=<mcp secret> go run ./cmd/apiserver \
     --mcp-resource-url https://localhost:8443/api/v1/mcp \
     --mcp-idp-issuer https://authentik.company/application/o/akbench-ema-idp/ \
     --mcp-client-id akbench-mcp
   ```

4. Run the client. It logs you in at the IdP in the browser:

   ```sh
   IDP_CLIENT_SECRET=... MCP_CLIENT_SECRET=<mcp secret> \
   go run ./hack/mcp-ema-client -insecure -mcp-url https://localhost:8443/api/v1/mcp \
     -idp-issuer https://authentik.company/application/o/akbench-ema-idp/ \
     -idp-client-id akbench-ema-idp -mcp-client-id akbench-mcp -call list_test_cases
   ```

