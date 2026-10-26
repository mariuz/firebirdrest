# Security and roles

FirebirdREST follows PostgREST's model: it authenticates requests, then leaves authorization to
the database.

## The authenticator user and roles

1. FirebirdREST logs in as one database user, the **authenticator**, which has *no privileges of
   its own*.
2. Each request runs in its own transaction under a **Firebird role**: the JWT's role claim, or
   `db-anon-role` for requests without a token.
3. The `GRANT`s on tables, views, procedures, functions and packages for that role decide what
   the request may do. A refused statement becomes `401` (anonymous) or `403` (authenticated).

```sql
CREATE USER authenticator PASSWORD 'a long random password';

CREATE ROLE web_anon;
CREATE ROLE web_user;

GRANT SELECT ON books TO ROLE web_anon;
GRANT ALL    ON books TO ROLE web_user;
GRANT EXECUTE ON PROCEDURE place_order TO ROLE web_user;

GRANT web_anon TO USER authenticator;
GRANT web_user TO USER authenticator;
```

!!! danger "Don't connect as SYSDBA or the database owner"
    In Firebird, `SYSDBA`, the database owner and users with the `RDB$ADMIN` role bypass every
    grant: every request would be able to read and change everything. Create a separate user.
    Note that the official Docker image makes `FIREBIRD_USER` the owner of the database it
    creates.

Firebird silently ignores a role that hasn't been granted to the connecting user and carries on
with no role. FirebirdREST checks `CURRENT_ROLE` when it opens a role's connection pool and
refuses requests for such roles with `403 FBRST300`.

Each role gets its own connection pool of `db-pool` connections, opened when the role is first
used.

## JSON Web Tokens

Send `Authorization: Bearer <token>`. The token is verified with the keys of `jwt-secret`,
`jwt-jwks-url`, or both:

| `jwt-secret` | Algorithms |
|---|---|
| a string (32+ characters recommended) | `HS256`, `HS384`, `HS512` |
| the same, base64 encoded, with `jwt-secret-is-base64 = true` | `HS256`, `HS384`, `HS512` |
| a PEM public key or certificate (`-----BEGIN PUBLIC KEY-----`, `-----BEGIN RSA PUBLIC KEY-----` or `-----BEGIN CERTIFICATE-----`); several blocks give several keys | RSA: `RS256/384/512`, `PS256/384/512`; ECDSA: `ES256/384/512`; Ed25519: `EdDSA` |
| a JWK (`{"kty": "RSA", ...}`) or a JWKS (`{"keys": [...]}`) as JSON | as for PEM, plus `HS*` for `"kty": "oct"` keys |
| `@/path/to/file`: any of the above, read from the file at startup | |

A key only verifies tokens of its own type: an RSA key never accepts an `HS256` token, so a
public key can't be misused as an HMAC secret. A JWK with `alg` accepts only that algorithm, and
JWKS entries with `"use": "enc"` are ignored. When the token names a key with `kid`, only keys
with that `kid` (or without one) are tried; otherwise every key of the right type is.

!!! note "Secret files"
    An `@file` HMAC secret is used byte for byte, including a trailing newline if the file has
    one. Write it with `printf '%s' "$SECRET" > file`, or use base64 and `jwt-secret-is-base64`.

### Keys from an identity provider

Identity providers such as Auth0, Keycloak, Okta, Azure AD or Google publish their signing keys
as a JWKS at a URL, and rotate them. Point `jwt-jwks-url` at it:

```ini
jwt-jwks-url = "https://example.eu.auth0.com/.well-known/jwks.json"
jwt-aud = "https://api.example.com"
jwt-role-claim-key = "https://example.com/role"
```

FirebirdREST fetches the URL at startup and every `jwt-jwks-refresh` seconds (default 3600).
A token whose `kid` isn't known yet triggers an extra fetch, at most once every 30 seconds, so a
newly rotated key works right away. If a fetch fails the previous keys stay in use; if the
first fetch fails, FirebirdREST starts anyway (anonymous requests keep working) and retries every
30 seconds. A key that disappears from the JWKS stops working as soon as the change is fetched.

### Validation cache

Checking an RSA or ECDSA signature costs far more than the rest of the authentication, so
accepted tokens are remembered: up to `jwt-cache-max-entries` tokens (default 1000; 0 turns the
cache off), each until its `exp` or for 5 minutes, whichever is sooner. The cache is emptied when
the JWKS keys change and when the schema cache reloads. Rejected tokens are never cached.

`exp` and `nbf` are checked when present. With `jwt-aud` set, the `aud` claim must match.

The role is read from the `role` claim. Use `jwt-role-claim-key` for another place, with dots
for nested claims:

```ini
jwt-role-claim-key = "app_metadata.role"
```

A valid token without a role claim uses `db-anon-role`. Role names must be plain identifiers
(`[A-Za-z_][A-Za-z0-9_$]*`).

| Problem | Response |
|---|---|
| no token and no `db-anon-role` | `401 FBRST302` |
| malformed, badly signed, expired or wrong audience | `401 FBRST301` |
| role not granted to the authenticator | `403 FBRST300` |
| token sent but neither `jwt-secret` nor `jwt-jwks-url` configured | `500 FBRST300` |
| no key for the token's algorithm or `kid` | `401 FBRST301` |

## Request context in PSQL

Request data is available to views, triggers, procedures, functions and check constraints as
transaction-scoped context variables in the `USER_TRANSACTION` namespace:

| Variable | Value |
|---|---|
| `request.role` | Firebird role of the request |
| `request.method`, `request.path` | HTTP method and path |
| `request.jwt.claim.<name>` | each string, number or boolean claim, e.g. `request.jwt.claim.sub` |
| `request.jwt.claims` | the whole claims object as JSON, if it fits in 255 characters |
| `request.header.origin`, `request.header.user-agent`, `request.header.x-forwarded-for` | those headers, when sent |

```sql
SELECT RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub') FROM RDB$DATABASE
```

Values longer than 255 characters are truncated (a Firebird limit).

### Row-level security

Combine the context with views and triggers:

```sql
-- users only see their own orders
CREATE VIEW my_orders AS
  SELECT o.* FROM orders o JOIN customers c ON c.id = o.customer_id
  WHERE c.email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub');

-- and can only create orders for themselves
CREATE EXCEPTION not_your_customer 'You can only order for yourself';

SET TERM ^;
CREATE TRIGGER orders_owner FOR orders BEFORE INSERT AS
BEGIN
  IF (NOT EXISTS (SELECT 1 FROM customers c WHERE c.id = NEW.customer_id
        AND c.email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub'))) THEN
    EXCEPTION not_your_customer;
END^
SET TERM ;^

GRANT USAGE ON EXCEPTION not_your_customer TO TRIGGER orders_owner;
GRANT SELECT ON customers TO TRIGGER orders_owner;
```

### When the variables are set

Setting the variables costs a database round trip, so by default (`db-request-context = auto`)
FirebirdREST only does it when some object in the database could read them. At each schema
load it searches the source of every view, trigger, procedure, function and package, and every
computed, default and check expression, for the text `request.`. Routines whose source is not
stored also count as possible readers. The `schema cache loaded` log line shows the outcome as
`request_context=true` or `false`.

Use `db-request-context = always` if you read the variables from code FirebirdREST can't see
(for example a UDR written in C++, or dynamic SQL that builds the names), or `never` to skip
them entirely.

## Pre-request procedure

`db-pre-request` names an executable procedure that runs at the start of every request, after
the context variables are set. Raise an exception to reject the request:

```sql
CREATE EXCEPTION banned 'This account is suspended';

SET TERM ^;
CREATE PROCEDURE check_request AS
BEGIN
  IF (EXISTS (SELECT 1 FROM banned_users
        WHERE email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub'))) THEN
    EXCEPTION banned;
END^
SET TERM ;^

GRANT EXECUTE ON PROCEDURE check_request TO ROLE web_anon;
GRANT EXECUTE ON PROCEDURE check_request TO ROLE web_user;
GRANT SELECT ON banned_users TO PROCEDURE check_request;
GRANT USAGE ON EXCEPTION banned TO PROCEDURE check_request;
```

```ini
db-pre-request = "check_request"
```

A banned user's requests then fail with
`400 {"message":"This account is suspended","hint":"exception BANNED",...}`.

## Transactions

Every write runs in its own transaction, committed when the request succeeds and rolled back
otherwise. Reads (`GET`, `HEAD`) run in read-only `READ COMMITTED` transactions that are
reused: each connection of a role keeps one open and serves request after request with it.
A statement in such a transaction sees the data committed when the statement starts, exactly
as it would in a transaction of its own, so responses are the same; what's saved is the
transaction start and commit round trips of every read. Firebird treats read-only
read-committed transactions as pre-committed, so the long-lived ones don't hold back garbage
collection. They are recycled after 1000 requests or 5 minutes, and request context variables
set by one request are cleared by the next. `db-read-tx-reuse = false` gives every read its
own transaction. Query plan requests always use a fresh transaction.

## Events

`GET /events/<name>` streams Firebird event notifications, which carry only a name and a count.
Only the names in `db-events` are exposed, to the roles in `db-events-roles` (any authenticated
role when unset). A listener learns that something happened, and reads what through the API with
its own grants. See [realtime events](../api/events.md).

## Other safeguards

The URL grammar is fuzzed in CI: `FuzzReadSQL` turns arbitrary query strings into the SQL of every
statement a read would run and checks that values only ever reach Firebird as parameters (no
string literal, comment or statement separator outside quoted identifiers), that every
identifier comes from the schema, and that bad input is refused with a 4xx error, never a panic
or a 500. It runs for a minute on every push and ten minutes in the weekly scheduled run.


- `PATCH` and `DELETE` without filters are refused (`db-safe-updates`).
- `GET` and `HEAD` run in read-only transactions.
- `db-max-rows` caps the rows a single read can return.
- Identifiers from the URL are checked against the schema cache and always quoted; values are
  always sent as parameters, never inlined into SQL.
- Error responses never carry the database login, host or internal error text: Firebird's
  "Effective user is ..." line is removed, and unexpected errors return a generic message while
  the cause goes to the log.
- The OpenAPI document at `/` lists only what the caller's role may use (`openapi-mode`).

### Request limits

A request is bounded in time and size, so one client can't tie up the server:

| | Limit | Setting |
|---|---|---|
| Time to send and handle a request | 60 s | `server-request-timeout` (`503 FBRST002` when exceeded) |
| Request body | 16 MB | `server-max-body-size` (`413 FBRST134`) |
| Request line and headers | 64 KB | fixed |
| `and=`/`or=` nesting | 16 levels | fixed (`400`) |
| Values in one `in.()` filter | 1000 | fixed (`400`) |
| Objects in `Prefer: params=multiple-objects` for a selectable procedure | 200 | fixed (`400 FBRST128`) |

The body is read in full before the transaction starts, so a slow client never holds a database
connection while sending.

### CORS

`server-cors-allowed-origins = "*"` (the default) answers any origin with
`Access-Control-Allow-Origin: *` and no credentials, which is enough for bearer tokens set by
JavaScript. To let browsers send credentials (cookies, HTTP auth from a proxy in front), list
the origins explicitly: each is reflected with `Access-Control-Allow-Credentials: true` and
`Vary: Origin`. An empty value turns the CORS headers off.

### Responses as data

Every response carries `X-Content-Type-Options: nosniff` and
`Content-Security-Policy: default-src 'none'; sandbox`, so a browser never treats a value stored
in the database as a page of the API's origin. A [single value](../api/formats-errors.md#single-values-and-files)
requested as `text/html`, SVG, XML, JavaScript or PDF is sent with `Content-Disposition:
attachment`: it downloads instead of rendering.

### Roles

Role names from tokens are folded to upper case, as Firebird folds unquoted identifiers, so
`web_user` and `WEB_USER` share one connection pool. Only roles the authenticator is granted get
a pool; the check happens when the pool is opened.
