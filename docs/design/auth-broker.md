# Gateway auth broker + per-backend auth hooks

Status: DESIGN / for review (not yet implemented)

## Motivation

Today each SSO backend solves auth its own way: the token logic lives either
inside the MCP server process (gitlab/hawk proxies call `sso-helper`) or in a
`run.sh` wrapper that exports a secret (ox-security, jira, opsgenie, grafana).
Consequences:

- The MCP servers are **not shareable** — the "secret sauce" (how we hijack a
  Swisscom SSO session via sso-helper, or which Keychain item holds a PAT) is
  baked into the server or its wrapper.
- There is **no uniform auth mechanism**; every backend reinvents it.
- MCP clients (kiro-cli v3) speak **MCP-OAuth** to the server. We don't answer
  it, so clients fall back to a dead `/authorize` prompt.

Goal: make the gateway the **single authorization broker** for its routes.
Clients do standard OAuth against the gateway; the gateway obtains the real
upstream token by running a **per-backend auth hook** (a declared command that
outputs a token). The MCP servers then carry **no auth logic** and become
shippable; the secret sauce lives only in the (unshipped) hook command.

### Scope: what this IS and IS NOT

This is a **standard way for an agent (MCP client) to obtain a backend's
upstream credential** — a token-provisioning handshake between the client and
the gateway on behalf of a backend. It is about *how the agent gets the token
the backend needs*, so servers can ship without embedded auth.

This is **NOT** multi-user access control. The gateway runs on a personal
machine for a single consumer; it does not gate "which person may use which
backend." The `Authorizer` interface comment about a "future OIDCAuthorizer"
refers to that separate, still-pending multi-user/shared-gateway concern and is
explicitly out of scope here. The two are orthogonal: this feature provisions a
backend credential to the local agent; OIDCAuthorizer (if ever built) would
decide who is allowed to reach the gateway at all.

## Architecture

```
kiro-cli ──OAuth(authorization_code+PKCE)──▶ gateway (authorization server)
                                                 │  runs the route's auth hook
                                                 ▼
                                          auth hook command  (e.g. sso-helper,
                                          Keychain read, or hand-pasted token)
                                                 │ prints {token, expires_at}
                                                 ▼
   kiro ──Authorization: Bearer <token>──▶ gateway ──forwards header──▶ MCP server ──▶ upstream API
```

The MCP server is a pure resource server: it reads the forwarded
`Authorization` header and calls its upstream API. No sso-helper, no Keychain,
no wrapper.

## Backend declaration

A route opts in by declaring an `auth` block in `backends.json`:

```json
"anyorg": {
  "command": ["python3", ".../anyorg-mcp-proxy.py"],
  "auth": {
    "provider": ["bash", ".../anyorg-token.sh"],   // prints token to stdout
    "token_format": "jwt",                          // jwt => read exp; opaque => use ttl
    "ttl_seconds": 7200,                            // fallback when exp unknown
    "expose": "real"                                // "real" (token to client) | "opaque"
  }
}
```

- `provider`: a command the gateway runs to obtain a token. Contract: print the
  bearer token to stdout (optionally a JSON `{ "token": "...", "expires_at": N }`);
  non-zero exit = auth failed. This is the ONLY place the secret sauce lives and
  is never part of the shipped server.
- `expose` — selects the **token model** (see "Token model" below):
  - `real` (default, **IMPLEMENTED**) — the gateway returns the provider's token
    to the client as the OAuth `access_token`. The gateway keeps **no token
    state**; the server just uses the forwarded token upstream.
  - `opaque` (**declared but NOT yet implemented**) — the gateway would return an
    opaque handle and keep the real token server-side. Requesting it today logs
    a warning and falls back to `real`.
- Backends with **no** `auth` block behave exactly as today (no OAuth; the
  gateway stays open-access locally and the server handles its own auth if any).
  This is the migration safety net.

## Token model: `real` vs `opaque` (and why we use `real`)

There are two ways the gateway can hand a client the credential:

| | `expose: real` (chosen) | `expose: opaque` (future) |
|---|---|---|
| `access_token` returned to client | the **real** upstream token | an opaque handle (`gwt_…`) |
| Gateway token state | **none** (stateless) | a `handle → real token` store |
| Who validates the token | the upstream API (bad token → 401) | the gateway, then upstream |
| Refresh | re-run the hook (route encoded in the refresh token) | gateway refreshes under the handle |
| Hides the real token from the client | no | yes |

**We use `real`, and the gateway is deliberately token-stateless.** Rationale:

- This gateway runs on a **single user's personal machine**; the MCP client
  (kiro-cli) is **trusted** and already holds every other backend's credentials
  (PATs in Keychain, SSO cookies, …). Handing it the upstream token leaks nothing
  it doesn't already have.
- Stateless = no server-side token store to grow, leak, or get out of sync. The
  only transient state is the short-lived authorization `code` during the
  `/authorize` → `/token` exchange.
- Refresh stays stateless: the refresh token encodes its route
  (`gwr_<route>_<random>`), so `/token?grant_type=refresh_token` just re-runs
  that route's provider hook for a fresh token.

**When `opaque` would be worth implementing:** a **multi-user / shared** gateway,
or an **untrusted** client, where the real upstream credential must not reach the
client. That is the separate, still-pending multi-user concern (the "future
OIDCAuthorizer"); it is explicitly out of scope here. The `expose` field is kept
in the schema so that model can be added later without a config break.

## OAuth endpoints the gateway serves

Minimal OAuth 2.0 + PKCE authorization-server surface, per the MCP auth spec:

- `GET /.well-known/oauth-protected-resource` and the route-scoped variant →
  advertise the authorization server for the resource.
- `GET /.well-known/oauth-authorization-server` → metadata:
  `authorization_endpoint`, `token_endpoint`, `code_challenge_methods_supported:
  ["S256"]`, `grant_types_supported: ["authorization_code","refresh_token"]`.
- `GET /authorize?response_type=code&client_id=..&code_challenge=..&
  code_challenge_method=S256&redirect_uri=http://127.0.0.1:PORT&state=..&
  resource=.../<route>/mcp`:
  1. Resolve the route and its `auth` hook.
  2. Run the hook (obtain + cache the real token). On failure → redirect with
     `error=access_denied`.
  3. Mint a one-time authorization `code` bound to (route, code_challenge,
     redirect_uri, state); 302 to `redirect_uri?code=..&state=..`.
- `POST /token`:
  - `grant_type=authorization_code`: verify PKCE (`S256(code_verifier) ==
    code_challenge`), redirect_uri match, code unexpired/one-time. Return
    `{access_token, token_type:"Bearer", expires_in, refresh_token}`.
    `access_token` is the real token (`expose:real`) or an opaque handle.
  - `grant_type=refresh_token`: re-run the hook to get a fresh upstream token,
    return a new `access_token` (+ rotated `refresh_token`). This is how a
    short-lived MFA-bound token (anyorg ~2h) refreshes transparently without
    re-prompting the user — provided the hook itself is non-interactive when the
    underlying SSO session is warm.

## Request-time behavior

- For auth-hook routes, the gateway **forwards the client's `Authorization`
  header to the backend** so an auth-free server can use it upstream. (Today the
  gateway consumes/ignores it.)
- The gateway validates the presented token minimally (known/unexpired handle,
  or signature if JWT) per the route's `expose` mode.

## Token lifecycle & caching

- The gateway caches the per-route token with its expiry; concurrent calls share
  it; refresh runs the hook again. This is the single fleet-wide token cache —
  individual servers no longer cache.
- Interactivity: a hook MAY be interactive on first use (browser/MFA) but SHOULD
  be silent on refresh when the session is warm. (The sso-helper token-cache
  improvement is a separate workstream that makes this cheap.)

## Migration (incremental, non-breaking)

1. Ship the broker + hook support. No backend declares `auth` yet → zero change
   in behavior.
2. Migrate anyorg first: strip sso-helper from the proxy (read forwarded
   `Authorization`), add an `auth` hook pointing at a private `anyorg-token.sh`.
3. Migrate the rest one at a time (hawk, ox-security, grafana, jira …): each
   moves its `run.sh`/in-proxy auth into a hook; the server becomes auth-free.
4. Each migrated server is now publishable; the hook stays private.

## Open questions

- Does kiro-cli require Dynamic Client Registration (`/register`), or does a
  fixed `client_id` (`Q+DEV+CLI` observed) + PKCE suffice? Verify before building
  `/authorize`.
- Per-route `expose` default: `real` is simplest and matches "ship the server";
  `opaque` only where the upstream token must be hidden from the client.
- Where the gateway persists its token cache (memory only vs disk) given the
  gateway itself is long-lived and launchd-managed.

## Testing

- Pure: PKCE S256 verify, code mint/consume (one-time, binding), metadata docs.
- Integration: a fake auth hook (prints a token) + a fake backend; drive the
  full authorize→token→authenticated-call sequence; assert refresh re-runs the
  hook; assert no-`auth` backends are unaffected.
