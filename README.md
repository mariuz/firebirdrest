# FirebirdREST

**Serve a RESTful API from any [Firebird](https://firebirdsql.org) database.**

FirebirdREST is a standalone web server that turns your Firebird database into a REST API,
in the spirit of [PostgREST](https://postgrest.org). Your tables, views, stored procedures and
functions become endpoints. Grants and roles you already defined in Firebird decide what each
caller can do. The API is described by the database schema, so you don't write or maintain an
API layer.

```console
$ curl 'http://localhost:3000/books?select=title,price,authors(name)&price=lt.10&order=price.desc'
[
  {"title":"Guards! Guards!","price":9.99,"authors":{"name":"Terry Pratchett"}},
  {"title":"Small Gods","price":8.50,"authors":{"name":"Terry Pratchett"}},
  {"title":"A Wizard of Earthsea","price":7.25,"authors":{"name":"Ursula K. Le Guin"}}
]
```

- **Single static binary** (Go, pure-Go wire protocol driver, no Firebird client library needed)
- **PostgREST-compatible URL grammar**: `select`, filters, `or`/`and` trees, `order`, `limit`/`offset`, `Range`
- **Resource embedding** through foreign keys: many-to-one, one-to-many, many-to-many, nested, `!inner`
- **Writes**: insert (single/bulk), upsert (`UPDATE OR INSERT` / `MERGE`), `PATCH`, `PUT`, `DELETE`, with `Prefer: return=representation`
- **Stored procedures and PSQL functions** over `/rpc/…`
- **Security through Firebird roles**: JWT `role` claim → Firebird `ROLE`; claims readable from PSQL with `RDB$GET_CONTEXT`
- **OpenAPI 3** document at `/`
- **Schema cache reload** on `POST_EVENT 'fbrest_reload'`, `SIGUSR1` or `POST /reload`
- Works with Firebird **3, 4 and 5** (the integration suite runs against all three)

## Quick start

With Docker:

```sh
git clone https://github.com/mariuz/firebirdrest && cd firebirdrest
docker compose up --build
```

This starts Firebird 5 with the [example bookstore database](examples/bookstore.sql), FirebirdREST on
<http://localhost:3000> and Swagger UI on <http://localhost:8080>.

From source (Go 1.24+):

```sh
go install github.com/mariuz/firebirdrest/cmd/firebirdrest@latest

export FBREST_DB_URI='authenticator:secret@localhost:3050/var/lib/firebird/data/app.fdb'
export FBREST_DB_ANON_ROLE=web_anon
firebirdrest            # or: firebirdrest examples/firebirdrest.conf
```

## Security model

FirebirdREST follows PostgREST's model and leaves authorization to the database:

1. FirebirdREST logs in as an **authenticator** user with *no privileges of its own*.
2. Each request runs in its own transaction under a **Firebird role**: the `role` claim of the
   JWT, or `db-anon-role` when there is no token.
3. Your `GRANT`s on tables, views, procedures and functions decide what that role may do.

```sql
CREATE USER authenticator PASSWORD 'secret';
CREATE ROLE web_anon;
CREATE ROLE web_user;

GRANT SELECT ON books TO ROLE web_anon;
GRANT ALL    ON books TO ROLE web_user;

GRANT web_anon TO USER authenticator;
GRANT web_user TO USER authenticator;
```

> **Note:** do not connect as `SYSDBA` or as the **database owner**: in Firebird they bypass every
> grant. If the authenticator user isn't granted a role, Firebird quietly connects with no role.
> FirebirdREST checks `CURRENT_ROLE` and refuses the request with 403 instead.

Tokens are verified with `jwt-secret` (HS256/384/512), or with a PEM public key (RS*/ES*).
`exp`, `nbf` and (if configured) `aud` are checked.

```sh
curl localhost:3000/customers -H "Authorization: Bearer $TOKEN"
```

### Request context in PSQL

Every request sets transaction-scoped context variables that triggers, views, procedures and
check constraints can read:

| Variable (namespace `USER_TRANSACTION`) | Value |
|---|---|
| `request.role` | Firebird role of the request |
| `request.method`, `request.path` | HTTP method and path |
| `request.jwt.claim.<name>` | each scalar JWT claim, e.g. `request.jwt.claim.sub` |
| `request.jwt.claims` | the whole claims object as JSON (if it fits in 255 bytes) |
| `request.header.origin`, `request.header.user-agent`, `request.header.x-forwarded-for` | selected headers |

```sql
-- row-level security with a view
CREATE VIEW my_orders AS
  SELECT * FROM orders
  WHERE customer_email = RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.sub');
```

`db-pre-request` names an executable procedure that runs before each request. It can raise an
`EXCEPTION` to reject the request.

## API reference

Identifiers stored in upper case (the normal, unquoted Firebird case) are shown in lower case:
table `BOOKS` is `/books` and column `AUTHOR_ID` is `author_id`. Quoted identifiers such as
`"Events Log"` keep their exact spelling (`/Events%20Log`).

### Reading

| Request | Meaning |
|---|---|
| `GET /books` | all rows and columns |
| `GET /books?select=id,title` | chosen columns |
| `GET /books?select=name:title,price::text` | rename (`alias:col`) and cast (`col::type`) |
| `GET /books?id=eq.1` | filter |
| `GET /books?order=price.desc.nullslast,title` | ordering |
| `GET /books?limit=10&offset=20` | paging (also `Range: 20-29` header) |
| `GET /books?select=id` + `Prefer: count=exact` | total in `Content-Range: 0-4/5` |

Casts: `text`, `varchar`, `integer`, `smallint`, `bigint`, `int128`, `numeric`, `double`, `decfloat`, `boolean`, `date`, `time`, `timestamp`.

#### Filter operators

| Operator | SQL | Example |
|---|---|---|
| `eq`, `neq` | `=`, `<>` | `?status=eq.new` |
| `gt`, `gte`, `lt`, `lte` | `>`, `>=`, `<`, `<=` | `?price=lt.10` |
| `like` | `LIKE` (`*` is the wildcard) | `?title=like.The*` |
| `ilike` | case-insensitive `LIKE` | `?title=ilike.*god*` |
| `similar` | `SIMILAR TO` | `?code=similar.[A-Z]{3}` |
| `containing` | `CONTAINING` (case-insensitive substring) | `?summary=containing.dragon` |
| `starting` | `STARTING WITH` | `?title=starting.The` |
| `in` | `IN (...)` | `?id=in.(1,2,3)` , `?name=in.("a,b",c)` |
| `is` | `IS NULL / TRUE / FALSE / UNKNOWN`, `not_null` | `?born=is.null` |
| `isdistinct` | `IS DISTINCT FROM` | `?a=isdistinct.5` |
| `not.<op>` | negation | `?id=not.in.(1,2)` |

Logic trees:

```
?or=(price.lt.8,and(published.gte.1990,in_stock.is.true))
?not.and=(id.gt.2,id.lt.5)
?authors.or=(country.eq.UK,country.eq.Poland)      # inside an embedded resource
```

#### Resource embedding

Foreign keys connect resources. Embed one by naming it in `select`:

```
GET /books?select=title,authors(name)                    # many-to-one  -> object
GET /authors?select=name,books(title,price)              # one-to-many  -> array
GET /books?select=title,tags(name)                       # many-to-many through book_tags -> array
GET /customers?select=email,orders(id,order_items(qty,books(title)))   # nested
GET /books?select=title,writer:authors(name)             # alias
GET /books?select=title,author_id(name)                  # embed by FK column name
GET /orders?select=*,customers!fk_orders_customer(name)  # pick a FK when there are several (300 otherwise)
```

Embedded resources take their own filters, order and limits:

```
GET /authors?select=name,books(title)&books.price=gt.9&books.order=price.desc&books.limit=1
```

By default the parent rows are always returned (a left join). `!inner` keeps only parents with
matching children, and `<embed>=is.null` keeps only parents without:

```
GET /books?select=title,authors!inner(name)&authors.country=eq.Poland
GET /books?select=title,tags(name)&tags=is.null           # books with no tags
GET /books?select=title,tags(name)&tags=not.is.null
```

### Writing

```sh
# insert (single object or array); 201 + Location header
curl -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '{"name":"classic"}'

# return the written rows, shaped by select (embeds work here too)
curl -X POST 'localhost:3000/books?select=id,title,authors(name)' \
     -H 'Prefer: return=representation' -d '[{"title":"Mort","author_id":1}]'

# upsert on the primary key, or on any unique columns with on_conflict
curl -X POST 'localhost:3000/tags?on_conflict=name' \
     -H 'Prefer: resolution=merge-duplicates' -d '[{"name":"classic"}]'     # UPDATE OR INSERT ... MATCHING
curl -X POST 'localhost:3000/tags?on_conflict=name' \
     -H 'Prefer: resolution=ignore-duplicates' -d '[{"name":"classic"}]'    # MERGE ... WHEN NOT MATCHED

# update / replace / delete (filters required unless db-safe-updates = false)
curl -X PATCH  'localhost:3000/books?id=eq.1' -d '{"price":11.50}'
curl -X PUT    'localhost:3000/tags?id=eq.7'  -d '{"id":7,"name":"noir"}'
curl -X DELETE 'localhost:3000/tags?id=eq.7'  -H 'Prefer: return=representation'
```

| `Prefer` | Effect |
|---|---|
| `return=minimal` (default) / `return=representation` | empty body, or the affected rows |
| `count=exact` | `Content-Range` with the number of matched/affected rows |
| `resolution=merge-duplicates` / `ignore-duplicates` | upsert behaviour of `POST` |
| `missing=default` | with `?columns=`, keys missing from a row use the column default instead of NULL |
| `tx=rollback` | run everything, then roll back (handy for tests) |

`?columns=a,b` limits which keys of the body are inserted. Bodies are JSON; form-urlencoded is
also accepted for a single row. Binary BLOBs are read and written as base64.

### Stored procedures and functions

| Routine | Called as | Result |
|---|---|---|
| Selectable procedure (with `SUSPEND`) | `SELECT * FROM proc(...)` | array of rows. `select`, filters, `order`, paging and counts all work |
| Executable procedure | `EXECUTE PROCEDURE proc(...)` | object of output parameters (204 if none) |
| PSQL function (Firebird 3+) | `SELECT fn(...) FROM RDB$DATABASE` | scalar JSON value |

```sh
curl 'localhost:3000/rpc/books_by_country?country=UK&order=price'      # GET: args in the query string
curl -X POST localhost:3000/rpc/place_order -d '{"customer_id":1,"book_id":2}'
curl 'localhost:3000/rpc/add_vat?amount=100'                            # -> 119.00
```

Arguments are matched by name. Missing trailing arguments that have defaults get their default
values. `GET` requests (including GET RPC) run in a **read-only transaction**.

### Response formats

Choose with the `Accept` header:

- `application/json` (default)
- `text/csv`
- `application/vnd.pgrst.object+json` returns one object instead of an array, and 406 unless exactly one row matches

### Errors

Errors use PostgREST's shape. `code` is Firebird's SQLSTATE, or an `FBRST…` code for API errors:

```json
{"code":"23000","message":"violation of FOREIGN KEY constraint INTEG_20 on table ORDERS",
 "details":"Foreign key reference target does not exist\nProblematic key value is (\"CUSTOMER_ID\" = 99)","hint":null}
```

| Status | When |
|---|---|
| 400 | bad query syntax, unknown column, constraint/validation errors, PSQL `EXCEPTION` |
| 401 | missing/invalid/expired JWT, or permission denied for an anonymous request |
| 403 | permission denied for an authenticated role |
| 404 | unknown table or routine |
| 406 | requested media type unavailable / singular object with ≠ 1 row |
| 409 | unique or foreign key violation, update conflicts |
| 300 | ambiguous embedding (several foreign keys); add a `!hint` |

## Configuration

Settings come from an optional config file (`firebirdrest app.conf`; see
[examples/firebirdrest.conf](examples/firebirdrest.conf)) and from environment variables. Environment variables win.

| File key | Environment | Default | |
|---|---|---|---|
| `db-uri` | `FBREST_DB_URI` | (required) | `user:password@host:port/path/db.fdb[?wire_crypt=false&charset=UTF8…]` |
| `db-anon-role` | `FBREST_DB_ANON_ROLE` | none | role for requests without a JWT. Empty means a token is required |
| `db-pool` | `FBREST_DB_POOL` | `10` | connections per role |
| `db-max-rows` | `FBREST_DB_MAX_ROWS` | `0` | cap on rows per read (0 = no cap) |
| `db-pre-request` | `FBREST_DB_PRE_REQUEST` | none | executable procedure run before each request |
| `db-safe-updates` | `FBREST_DB_SAFE_UPDATES` | `true` | refuse `PATCH`/`DELETE` without filters |
| `db-reload-event` | `FBREST_DB_RELOAD_EVENT` | `fbrest_reload` | `POST_EVENT` that reloads the schema cache |
| `jwt-secret` | `FBREST_JWT_SECRET` | none | HMAC secret, or PEM public key |
| `jwt-aud` | `FBREST_JWT_AUD` | none | required `aud` claim |
| `jwt-role-claim-key` | `FBREST_JWT_ROLE_CLAIM_KEY` | `role` | dotted path to the role, e.g. `app_metadata.role` |
| `server-host` / `server-port` | `FBREST_SERVER_HOST` / `FBREST_SERVER_PORT` | `0.0.0.0` / `3000` | |
| `admin-server-port` | `FBREST_ADMIN_SERVER_PORT` | `0` (off) | `GET /live`, `GET /ready`, `POST /reload` |
| `server-cors-allowed-origins` | `FBREST_SERVER_CORS_ALLOWED_ORIGINS` | `*` | comma-separated list |
| `openapi-server-proxy-uri` | `FBREST_OPENAPI_SERVER_PROXY_URI` | request host | public URL shown in the OpenAPI document |
| `log-level` | `FBREST_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

### Reloading the schema cache

Run DDL, then do any one of these:

```sql
EXECUTE BLOCK AS BEGIN POST_EVENT 'fbrest_reload'; END
```

```sh
kill -USR1 $(pidof firebirdrest)
curl -X POST localhost:3001/reload        # admin server
```

To reload automatically after every DDL change, use a DDL trigger:

```sql
SET TERM ^;
CREATE TRIGGER fbrest_ddl_reload AFTER ANY DDL STATEMENT AS
BEGIN
  POST_EVENT 'fbrest_reload';
END^
SET TERM ;^
```

## Differences from PostgREST

FirebirdREST borrows PostgREST's interface, but Firebird is not PostgreSQL:

- Firebird has no schemas, so all tables/views/routines are in one namespace (`/table`, `/rpc/name`).
  Packaged routines are not exposed yet.
- No JSON operators (`->`, `->>`), array or range operators, or full-text `fts`. Firebird-specific `containing`, `starting` and `similar` are added instead.
- Aggregates (`select=count()`), spread embeds (`...rel(...)`) and computed relationships are not implemented yet.
- Embedded children are fetched with one batched `IN (...)` query per embed level instead of a single
  SQL statement. Per-parent `limit`/`offset` on embeds is applied after fetching.
- A request cannot change roles mid-transaction, so each role gets its own connection pool.

## Development

```sh
go test ./...                       # unit tests

# integration tests against a real server
docker run -d --name fb -p 3050:3050 -e FIREBIRD_ROOT_PASSWORD=masterkey -e FIREBIRD_DATABASE=test.fdb firebirdsql/firebird:5
docker cp examples/bookstore.sql fb:/tmp/ && docker exec fb /opt/firebird/bin/isql \
  -user SYSDBA -password masterkey localhost:/var/lib/firebird/data/test.fdb -i /tmp/bookstore.sql
FBREST_TEST_DB_URI=authenticator:authenticator@localhost:3050/var/lib/firebird/data/test.fdb go test ./...
```

Layout:

```
cmd/firebirdrest     main: config, signals, admin server, event listener
internal/config      config file + FBREST_* environment
internal/schema      reads RDB$ system tables into the schema cache
internal/query       URL grammar parser, embedding planner, SQL builder, executor
internal/server      HTTP handlers, JWT/roles, OpenAPI, errors
examples/            bookstore database and sample config
```

## License

MIT. Built on the [nakagami/firebirdsql](https://github.com/nakagami/firebirdsql) driver. The API
design comes from [PostgREST](https://postgrest.org).
