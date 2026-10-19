# Formats, OpenAPI and errors

## Response formats

Choose with the `Accept` header:

| `Accept` | Response |
|---|---|
| `application/json` (default, also `*/*`) | JSON array of objects |
| `text/csv` | header row plus one line per row; embedded values are written as JSON |
| `application/vnd.pgrst.object+json` | a single JSON object, or `406` unless exactly one row matches |
| `text/plain` | for `/rpc` functions only: the bare value |

```sh
curl 'localhost:3000/books?select=id,title' -H 'Accept: text/csv'
curl 'localhost:3000/books?id=eq.1' -H 'Accept: application/vnd.pgrst.object+json'
```

## OpenAPI

`GET /` returns an [OpenAPI 3](https://spec.openapis.org/oas/v3.0.3) document describing every
table, view, procedure and function, with their columns, types, keys and computed fields. Point
Swagger UI, Postman or a code generator at it:

```sh
curl localhost:3000/ > openapi.json
```

Behind a proxy, set `openapi-server-proxy-uri` to the public URL so the document's `servers`
entry is right.

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
| `FBRST100` | filter syntax or unknown operator |
| `FBRST101` | `order` syntax |
| `FBRST102` | invalid body or parameter value |
| `FBRST103` | `select` syntax, or invalid `Range` |
| `FBRST104` | unsupported cast |
| `FBRST105` | `PUT` needs `eq` filters on the whole primary key |
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
| `FBRST200` | no relationship between the tables |
| `FBRST201` | several relationships, `!hint` needed |
| `FBRST202` | unknown procedure or function, or unknown argument |
| `FBRST204` | unknown column |
| `FBRST205` | unknown table or view |
| `FBRST300` | role not granted to the authenticator user, or no JWT secret configured |
| `FBRST301` | invalid JWT |
| `FBRST302` | anonymous access is disabled |
