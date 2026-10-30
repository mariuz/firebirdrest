# Formats, OpenAPI and errors

## Response formats

Choose with the `Accept` header:

| `Accept` | Response |
|---|---|
| `application/json` (default, also `*/*`) | JSON array of objects |
| `text/csv` | header row plus one line per row; embedded values are written as JSON |
| `application/vnd.pgrst.object+json` | a single JSON object, or `406` unless exactly one row matches |
| any other concrete type, e.g. `text/plain`, `application/octet-stream`, `image/png` | a single value as is: see below |

```sh
curl 'localhost:3000/books?select=id,title' -H 'Accept: text/csv'
curl 'localhost:3000/books?id=eq.1' -H 'Accept: application/vnd.pgrst.object+json'
```

### Single values and files

Ask for any concrete media type that isn't in the table above, and the response body is the
value itself instead of JSON, with the `Content-Type` you asked for. The request must select
one column and match one row:

```sh
# the bytes of a BLOB SUB_TYPE BINARY column, served as an image
curl 'localhost:3000/covers?select=image&book_id=eq.1' -H 'Accept: image/png' > cover.png
# a text column, as text
curl 'localhost:3000/books?select=summary&id=eq.1' -H 'Accept: text/plain'
# a function result, as text
curl 'localhost:3000/rpc/add_vat?amount=100' -H 'Accept: text/plain'
```

- Binary columns (`BLOB SUB_TYPE BINARY`, `CHARACTER SET OCTETS`) are sent as their bytes; text
  columns as their text; numbers, booleans and dates as their JSON text (`9.99`, `true`).
  `text/*`, `+json` and `+xml` types get `; charset=utf-8`.
- `NULL` gives an empty `204`.
- Types a browser would run or render as a document (`text/html`, SVG, XML, JavaScript, PDF) are
  sent as downloads (`Content-Disposition: attachment`), so a stored value can never run as a
  page of the API.
- More than one row gives `406` (`FBRST116`), more than one column `406` (`FBRST107`).
- `application/octet-stream` works for any column. A column's own type (`image/png`,
  `application/pdf`) is handy to serve files straight from the database; keep the type in another
  column if it varies per row, and read it first.
- This also covers formats FirebirdREST doesn't produce itself: a function that builds GeoJSON
  text can be requested with `Accept: application/geo+json`.
- It works for `/rpc` functions, procedures with one output parameter, selectable procedures, and
  writes with `Prefer: return=representation` (for example `POST /covers?select=book_id`).
- A built-in type anywhere in `Accept`, or a wildcard, takes precedence: a browser's
  `text/html, */*` still gets JSON.

### Binary data and memory

In JSON, `BLOB SUB_TYPE BINARY` and `CHARACTER SET OCTETS` values are base64 strings, and are
sent the same way on input. Firebird returns a BLOB as a whole, so each BLOB in a response is
held in memory while the request runs: a raw single-value response holds it once, a JSON response
also holds its base64 form. Keep that in mind for large files:

- Select only the columns you need: `select=*` includes BLOB columns.
- Serve files with a raw single-value request (above) rather than through JSON.
- To show sizes without reading the data, expose `OCTET_LENGTH(image)` through a
  [computed field](computed.md).
- `db-max-rows` bounds the number of rows, not their size.

## Query plans

To see how Firebird runs a request, ask for its plan instead of its data. This is off by default,
because plans show table and index names: set `db-plan-enabled = true` (for example in
development).

```sh
curl 'localhost:3000/books?select=title,authors(name)&price=lt.10' -H 'Accept: application/vnd.pgrst.plan'
```

```text
-- statement 1
SELECT t0."TITLE", t0."AUTHOR_ID" FROM "BOOKS" t0 WHERE ((t0."PRICE" < ?))

Select Expression
    -> Filter
        -> Table "BOOKS" as "T0" Full Scan

-- statement 2
SELECT t0."NAME", t0."ID" FROM "AUTHORS" t0 WHERE (t0."ID" IN (?, ?))

Select Expression
    -> Filter
        -> Table "AUTHORS" as "T0" Access By ID
            -> Bitmap
                -> Index "RDB$PRIMARY1" List Scan (full match)
```

Here `price` has no index, so the books are read with a full scan.

- The response lists every statement the request runs: one per embed level, and the count with
  `Prefer: count=exact`. The plan is Firebird's explained plan (`MON$EXPLAINED_PLAN`).
- `application/vnd.pgrst.plan+json` returns `[{"query": "...", "plan": "...", "tree": [...]}]`:
  the plan as text and parsed into nodes, each `{"node": "Filter", "children": [...]}`, one tree
  per expression of the statement (a sub-query or a block's statements add trees).
  `application/vnd.pgrst.plan` and `+text` return text.
- Plans are available for every method. To know the statements of embedded resources the
  request is run with its result discarded; a `POST`, `PATCH`, `PUT` or `DELETE` runs in a
  transaction that is always rolled back, so nothing is written. Firebird has no plan for a
  plain `INSERT` (an empty one is shown); multi-row inserts show their `EXECUTE BLOCK`, and
  `UPDATE` and `DELETE` show how the rows are found.
- With plans disabled, the plan media types get `406`.

## OpenAPI

`GET /` returns an [OpenAPI 3](https://spec.openapis.org/oas/v3.0.3) document describing the
tables, views, procedures and functions the caller's role may use, with their columns, types,
keys and computed fields, and only the methods the role is granted (`SELECT` → `get`, `INSERT` →
`post`, ...). Send the request with the token whose view you want; without one, the anonymous
role's. `openapi-mode = "ignore-privileges"` describes everything instead, and `"disabled"`
turns `/` off. Point Swagger UI, Postman or a code generator at it:

```sh
curl localhost:3000/ > openapi.json
```

Behind a proxy, set `openapi-server-proxy-uri` to the public URL so the document's `servers`
entry is right.

### Data for client generators

Three `x-firebirdrest` extensions carry what the JSON schemas can't, for tools such as
[`firebirdrest-js gen types`](https://github.com/mariuz/firebirdrest-js#typed-queries):

| Where | Fields |
|---|---|
| `info` | `schema` (the one described, `""` before Firebird 6), `schemas` (all exposed, each read with `Accept-Profile`), `firebird` (server version) |
| each table or view in `components.schemas` | `kind` (`table` or `view`), `primaryKey`, `foreignKeys` (`name`, `columns`, `referencedRelation`, `referencedColumns`, `isOneToOne`, and `referencedSchema` for a key into another schema), `computedFields` (functions usable as columns; `select=*` leaves them out) |
| each `/rpc/...` path | `kind` (`function`, `selectable`, `executable`), `args` (`name`, `hasDefault`) |

Names are API names, as in URLs. The extensions follow the caller's privileges like the rest of
the document.

## Errors

Errors are JSON objects in PostgREST's shape:

```json
{
  "code": "23000",
  "message": "violation of FOREIGN KEY constraint INTEG_20 on table ORDERS",
  "details": "Foreign key reference target does not exist\nProblematic key value is (\"CUSTOMER_ID\" = 99)",
  "hint": null
}
```

`code` is Firebird's SQLSTATE for database errors, or an `FBRST` code for API errors.
`message` is the first line of Firebird's message and `details` the rest.

| Status | When |
|---|---|
| `300` | ambiguous embedding: several foreign keys connect the tables; add a `!hint` |
| `400` | bad query syntax, unknown column, invalid value, constraint or validation error, PSQL `EXCEPTION` |
| `401` | missing, invalid or expired JWT; or permission denied for an anonymous request |
| `403` | permission denied for an authenticated role, or the role isn't granted to the database user |
| `404` | unknown table, view, procedure or function |
| `405` | method not allowed (e.g. `PUT` without primary key filters) |
| `406` | requested media type unavailable, or a single object requested and the row count isn't 1 |
| `409` | unique or foreign key violation, update conflict, deadlock |
| `416` | invalid `Range`, or (with `Prefer: count=exact`) an offset beyond the last row |
| `503` | the database can't be reached |

### API error codes

| Code | Meaning |
|---|---|
| `FBRST000` | unexpected server error (see the log) |
| `FBRST001` | the database can't be reached (`503`) |
| `FBRST002` | the request took longer than `server-request-timeout` (`503`) |
| `FBRST100` | filter syntax or unknown operator |
| `FBRST101` | `order` syntax |
| `FBRST102` | invalid body or parameter value |
| `FBRST103` | `select` syntax, or invalid `Range` |
| `FBRST104` | unsupported cast |
| `FBRST105` | `PUT` needs `eq` filters on the whole primary key |
| `FBRST106` | `Accept-Profile`/`Content-Profile` names a schema that isn't exposed (or the server has no schemas) |
| `FBRST107` | no acceptable media type |
| `FBRST108` | a filter or order references an embed that isn't in `select` |
| `FBRST109` | `PATCH`/`DELETE` without filters or `limit` (`db-safe-updates`) |
| `FBRST114`, `FBRST115` | `PUT` body doesn't match the URL |
| `FBRST116` | single object requested, row count isn't 1 |
| `FBRST117` | method not allowed |
| `FBRST120` | operation not available on this embed (e.g. `!inner` on an executable procedure) |
| `FBRST121` | two output keys would have the same name |
| `FBRST123` | aggregates are disabled |
| `FBRST124` | upsert without primary key or `on_conflict` |
| `FBRST125` | invalid path |
| `FBRST126` | `order=embed(column)` on a to-many embed, or together with aggregates |
| `FBRST127` | `PATCH`/`DELETE` with `limit` but no `order`, or `offset` without `limit` |
| `FBRST128` | more than 1024 objects for a selectable procedure with `params=multiple-objects`, or arguments over 48 KB in all |
| `FBRST129` | `params=single-object` on a routine without exactly one text parameter |
| `FBRST130` | an unknown or invalid preference with `Prefer: handling=strict` |
| `FBRST131` | a `PATCH`/`DELETE` would affect more rows than `Prefer: max-affected` |
| `FBRST134` | the request body is larger than `server-max-body-size` (`413`) |
| `FBRST135` | `GET /events/...` while `db-events` is empty (`404`) |
| `FBRST136` | an event name that isn't in `db-events` (`404`) |
| `FBRST137` | the role is not in `db-events-roles` (`403`, `401` when anonymous) |
| `FBRST133` | a raw request body (`text/plain`, `application/octet-stream`...) sent to a routine that doesn't have exactly one parameter |
| `FBRST132` | a query plan was asked for a method without statements (`OPTIONS`) |
| `FBRST200` | no relationship between the tables |
| `FBRST201` | several relationships, `!hint` needed |
| `FBRST202` | unknown procedure or function, or unknown argument |
| `FBRST204` | unknown column |
| `FBRST205` | unknown table or view |
| `FBRST300` | role not granted to the authenticator user, or no JWT secret configured |
| `FBRST301` | invalid JWT |
| `FBRST302` | anonymous access is disabled |
