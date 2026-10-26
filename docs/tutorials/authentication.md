# Authentication

This tutorial continues from [Getting started](getting-started.md). You'll make a JSON Web Token
(JWT) for the `web_user` role, use it to write data, and then use one of its claims inside the
database.

## How requests get a role

FirebirdREST connects as the user `authenticator`, which has **no privileges of its own**. For
every request it opens a transaction under a Firebird role:

- with no `Authorization` header: the anonymous role, `db-anon-role` (`web_anon` in the example);
- with `Authorization: Bearer <JWT>`: the role named in the token's `role` claim.

Firebird's `GRANT`s for that role then decide what the request may do. The example grants
`web_anon` read access to the catalogue and `web_user` everything:

```sql
GRANT SELECT ON books TO ROLE web_anon;
GRANT ALL    ON books TO ROLE web_user;
GRANT ALL    ON orders TO ROLE web_user;

GRANT web_anon TO USER authenticator;
GRANT web_user TO USER authenticator;
```

## 1. Make a token

The token is signed with the secret in `jwt-secret`. docker-compose sets it to
`reallyreallyreallyreallyverysafe1234`. In production use a long random value.

Any JWT library works. With plain shell tools:

```sh
SECRET='reallyreallyreallyreallyverysafe1234'
b64() { base64 | tr '+/' '-_' | tr -d '=\n'; }
header=$(printf '{"alg":"HS256","typ":"JWT"}' | b64)
payload=$(printf '{"role":"web_user","sub":"ana@example.com","exp":%d}' $(( $(date +%s) + 3600 )) | b64)
signature=$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -hmac "$SECRET" -binary | b64)
TOKEN="$header.$payload.$signature"
```

The token expires in an hour (`exp`). FirebirdREST rejects expired tokens with 401.

## 2. Use it

```sh
curl localhost:3000/customers -H "Authorization: Bearer $TOKEN"
```

```json
[{"id":1,"email":"ana@example.com","name":"Ana"},{"id":2,"email":"bob@example.com","name":"Bob"}]
```

Without the header, the same request gets `401`, because `web_anon` has no grant on `customers`.

Now the write that failed before:

```sh
curl -X POST 'localhost:3000/tags?select=id,name' \
     -H "Authorization: Bearer $TOKEN" -H 'Prefer: return=representation' \
     -H 'Content-Type: application/json' -d '{"name":"classic"}'
```

```json
[{"id":4,"name":"classic"}]
```

And an executable procedure that creates an order:

```sh
curl -X POST localhost:3000/rpc/place_order -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"customer_id":1,"book_id":2}'
```

```json
{"order_id":2}
```

## 3. Read claims inside the database

Every scalar claim is available to PSQL as a transaction context variable. The example function
`whoami` reads the `sub` claim:

```sql
CREATE FUNCTION whoami RETURNS VARCHAR(255) AS
BEGIN
  RETURN COALESCE(RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub'), 'anonymous')
         || ' as ' || CURRENT_ROLE;
END
```

```sh
curl localhost:3000/rpc/whoami -H "Authorization: Bearer $TOKEN"     # "ana@example.com as WEB_USER"
curl localhost:3000/rpc/whoami                                       # "anonymous as WEB_ANON"
```

The same mechanism gives you row-level security. Create a view that shows each user only their
own orders:

```sql
CREATE VIEW my_orders AS
  SELECT o.* FROM orders o JOIN customers c ON c.id = o.customer_id
  WHERE c.email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub');

GRANT SELECT ON my_orders TO ROLE web_user;
```

Then reload the schema cache so the new view appears:

```sql
-- in isql; other tools don't need SET TERM
SET TERM ^;
EXECUTE BLOCK AS BEGIN POST_EVENT 'fbrest_reload'; END^
SET TERM ;^
COMMIT;
```

```sh
curl localhost:3000/my_orders -H "Authorization: Bearer $TOKEN"     # only Ana's orders
```

## Next steps

- [Security and roles](../references/security.md): the full list of context variables, RSA/ECDSA
  keys, audiences and the pre-request hook.
- [Configuration](../references/configuration.md): `jwt-secret`, `jwt-aud`, `jwt-role-claim-key`.

## Next

In production an identity provider issues the tokens instead of a shared secret:
[Sign-in with Keycloak](keycloak.md) sets that up with `jwt-jwks-url`, an audience and a role
claim, and lists the settings for Auth0 and Microsoft Entra ID.
