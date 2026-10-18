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

Send `Authorization: Bearer <token>`. The token is verified with `jwt-secret`:

| `jwt-secret` | Algorithms |
|---|---|
| a string (32+ characters recommended) | `HS256`, `HS384`, `HS512` |
| a PEM public key (`-----BEGIN PUBLIC KEY-----...`) | `RS256/384/512` (RSA) or `ES256/384/512` (ECDSA) |

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
| token sent but no `jwt-secret` configured | `500 FBRST300` |

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

## Other safeguards

- `PATCH` and `DELETE` without filters are refused (`db-safe-updates`).
- `GET` and `HEAD` run in read-only transactions.
- `db-max-rows` caps the rows a single read can return.
- CORS is controlled with `server-cors-allowed-origins`.
- Identifiers from the URL are checked against the schema cache and always quoted; values are
  always sent as parameters, never inlined into SQL.
