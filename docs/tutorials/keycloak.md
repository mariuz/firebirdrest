# Sign-in with Keycloak

The [authentication tutorial](authentication.md) signs tokens with a shared secret. In production
an identity provider usually issues them: users sign in there, your application gets a token,
and FirebirdREST checks it against the provider's published keys. This tutorial does that with
[Keycloak](https://www.keycloak.org), with a role claim that selects the Firebird role. The same
settings work for any OpenID Connect provider ([below](#other-providers)).

You need the [getting-started](getting-started.md) setup: Docker with Compose, `curl`, and the
repository checked out.

## 1. Start Keycloak next to FirebirdREST

[`examples/keycloak/compose.yml`](https://github.com/mariuz/firebirdrest/blob/main/examples/keycloak/compose.yml)
adds Keycloak to the example stack and changes how FirebirdREST checks tokens:

```yaml
firebirdrest:
  environment:
    FBREST_JWT_SECRET: ""
    FBREST_JWT_JWKS_URL: http://keycloak:8080/realms/bookstore/protocol/openid-connect/certs
    FBREST_JWT_AUD: firebirdrest
```

- `jwt-jwks-url` is where Keycloak publishes its signing keys (a JWKS). FirebirdREST fetches it
  at startup, every hour (`jwt-jwks-refresh`), and when a token names a key it hasn't seen.
- `jwt-aud` accepts only tokens issued *for this API*. Without it, any token from the realm
  would be accepted, including ones meant for other applications.

```sh
docker compose -f docker-compose.yml -f examples/keycloak/compose.yml up --build -d
```

Keycloak takes about 15 seconds to start; its admin console is at <http://localhost:8180>
(`admin` / `admin`). Until the realm below exists, FirebirdREST logs `cannot fetch jwt-jwks-url`
and retries every 30 seconds; requests with a token are refused meanwhile.

## 2. Create the realm, the client and the users

[`examples/keycloak/setup.sh`](https://github.com/mariuz/firebirdrest/blob/main/examples/keycloak/setup.sh)
does it with Keycloak's admin CLI:

```sh
docker compose -f docker-compose.yml -f examples/keycloak/compose.yml exec keycloak bash /setup/setup.sh
```

It creates:

| | |
|---|---|
| realm `bookstore` | where the users and the application live |
| client `bookstore-app` | the application users sign in to: a public client (a browser or mobile app has no secret to keep) |
| a *User Attribute* mapper | copies the user's `firebird_role` attribute into the token's `role` claim |
| an *Audience* mapper | adds `firebirdrest` to the token's `aud` |
| user `ana`, password `ana-password` | `firebird_role` = `web_user` |
| user `bob`, password `bob-password` | no `firebird_role` |

FirebirdREST reads the role from a claim that holds one string (`jwt-role-claim-key`, `role` by
default, or a dotted path such as `app_metadata.role`). Keycloak's own roles come as a list
(`realm_access.roles`), so a user attribute mapped to a `role` claim is the direct way to name
one Firebird role per user. In the admin console these are under *Users → ana → Attributes* and
*Clients → bookstore-app → Client scopes → bookstore-app-dedicated*.

## 3. Sign in and call the API

A real application sends the user through Keycloak's login page (the authorization code flow,
for example with [keycloak-js](https://www.keycloak.org/securing-apps/javascript-adapter) or any
OpenID Connect library) and receives the token at the end. From a terminal, the password grant
gets one directly:

```sh
TOKEN=$(curl -s -d client_id=bookstore-app -d grant_type=password \
  -d username=ana -d password=ana-password \
  localhost:8180/realms/bookstore/protocol/openid-connect/token | jq -r .access_token)
```

Its payload (decode the middle part at [jwt.io](https://jwt.io)) has what FirebirdREST uses:

```json
{
  "aud": ["firebirdrest", "account"],
  "sub": "3f38e4ec-2243-4e66-9bec-b9df995cb795",
  "role": "web_user",
  "email": "ana@example.com",
  "azp": "bookstore-app"
}
```

Ana's requests run as the `web_user` role:

```sh
curl localhost:3000/rpc/whoami -H "Authorization: Bearer $TOKEN"
```

```json
"3f38e4ec-2243-4e66-9bec-b9df995cb795 as WEB_USER"
```

```sh
curl -X POST 'localhost:3000/orders?select=id,customer_id' -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Prefer: return=representation' -d '{"customer_id":1}'
```

```json
[{"id":2,"customer_id":1}]
```

Bob's token has no `role` claim, so his requests use the anonymous role, `web_anon`, which may
not read orders:

```sh
BOB_TOKEN=$(curl -s -d client_id=bookstore-app -d grant_type=password \
  -d username=bob -d password=bob-password \
  localhost:8180/realms/bookstore/protocol/openid-connect/token | jq -r .access_token)
curl localhost:3000/orders -H "Authorization: Bearer $BOB_TOKEN"
```

```json
{"code":"28000","message":"no permission for SELECT access to TABLE ORDERS","details":null,"hint":null}
```

That fallback is `db-anon-role`. Without it, a token without a role is refused with `401`. A
token that was tampered with, has expired, or was issued for another audience gets `401`
(`FBRST301`) before any database work.

## 4. Use the claims in the database

Every claim reaches PSQL as a context variable, so procedures, views and triggers can act on
the signed-in user (see [Request context in PSQL](../references/security.md#request-context-in-psql)):

```sql
-- the orders of the signed-in customer only
CREATE VIEW my_orders AS
  SELECT o.* FROM orders o JOIN customers c ON c.id = o.customer_id
  WHERE c.email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.email');
```

`sub` is Keycloak's stable user id; `email` changes when the user changes it, so key long-lived
data on `sub`.

## 5. Rotate keys

In Keycloak, *Realm settings → Keys → Add provider → rsa-generated* with a higher priority than
the current key makes it the signing key. FirebirdREST picks it up when the first token signed
with it arrives (at most one fetch every 30 seconds, so a burst of tokens with unknown keys
can't flood Keycloak); keep the old key enabled until the tokens it signed have expired, then
disable it. Keycloak's encryption key (`use: enc`) in the same JWKS is ignored.

## 6. Clean up

```sh
docker compose -f docker-compose.yml -f examples/keycloak/compose.yml down -v
```

## Other providers

FirebirdREST needs three things from any OpenID Connect provider: its JWKS URL, a claim with the
Firebird role, and an audience for the API.

| Provider | `jwt-jwks-url` | Role claim | `jwt-aud` |
|---|---|---|---|
| Keycloak | `https://<host>/realms/<realm>/protocol/openid-connect/certs` | a *User Attribute* (or *Hardcoded claim*) mapper, as above | an *Audience* mapper |
| Auth0 | `https://<tenant>.auth0.com/.well-known/jwks.json` | a post-login Action: `api.accessToken.setCustomClaim('role', ...)` | the API's identifier, requested as `audience` at sign-in |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys` | app roles arrive as a list (`roles`): map the one to use to a single-valued claim | the application ID URI |
| Google, GitHub | not suited: their tokens carry no role, so every user would get the same one | | |

A provider that only emits roles as a list needs one more step on its side: map the role (or a
group) to a single-valued claim, since one string is what `jwt-role-claim-key` reads. The role
is checked by Firebird's grants on every statement, so a claim naming a role the authenticator
user wasn't granted is refused (`403`, `FBRST300`).
