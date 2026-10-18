# Writing

Send JSON bodies with `Content-Type: application/json`. A form-urlencoded body is also accepted
for a single row.

## Insert

`POST` takes one object or an array of objects:

```sh
curl -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '{"name":"classic"}'
curl -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '[{"name":"noir"},{"name":"poetry"}]'
```

The response is `201 Created`. For a single row with a primary key, a `Location` header points
to it (`/tags?id=eq.7`). Columns missing from an object get their default (or the identity
value), and unknown keys are rejected with `400`.

### Getting the rows back

`Prefer: return=representation` returns the inserted rows, shaped by `select`, with embeds:

```sh
curl -X POST 'localhost:3000/books?select=id,title,authors(name)' \
     -H 'Prefer: return=representation' -H 'Content-Type: application/json' \
     -d '[{"title":"Mort","author_id":1}]'
```

```json
[{"id":6,"title":"Mort","authors":{"name":"Terry Pratchett"}}]
```

### Choosing columns

`?columns=` lists the keys to insert and ignores the rest. Keys missing from a row become `NULL`.
Add `Prefer: missing=default` to use the column default instead:

```
POST /books?columns=title,author_id
Prefer: missing=default
```

## Upsert

| `Prefer` | Firebird statement | On conflict |
|---|---|---|
| `resolution=merge-duplicates` | `UPDATE OR INSERT ... MATCHING (...)` | update the existing row |
| `resolution=ignore-duplicates` | `MERGE ... WHEN NOT MATCHED THEN INSERT` | keep the existing row |

Conflicts are detected on the primary key, or on the columns listed in `on_conflict`:

```sh
curl -X POST 'localhost:3000/tags?on_conflict=name' \
     -H 'Prefer: resolution=ignore-duplicates,return=representation' \
     -H 'Content-Type: application/json' -d '[{"name":"fantasy"},{"name":"new-tag"}]'
```

```json
[{"id":5,"name":"new-tag"}]
```

With `ignore-duplicates`, only newly inserted rows are returned.

## Update

`PATCH` updates every row matching the filters:

```sh
curl -X PATCH 'localhost:3000/books?author_id=eq.1&select=id,price' \
     -H 'Prefer: return=representation' -H 'Content-Type: application/json' -d '{"price":11.50}'
```

## Replace one row

`PUT` with `eq` filters on every primary key column inserts or replaces that single row. The body
must contain the same key values:

```sh
curl -X PUT 'localhost:3000/tags?id=eq.10' -H 'Content-Type: application/json' -d '{"id":10,"name":"noir"}'
```

## Delete

```sh
curl -X DELETE 'localhost:3000/tags?id=eq.10' -H 'Prefer: return=representation'
```

## Safety

`PATCH` and `DELETE` without any filter are refused with `400` so a missing parameter can't
change a whole table. Set `db-safe-updates = false` to allow them.

## Prefer reference

| `Prefer` | Effect |
|---|---|
| `return=minimal` (default) | empty body (`201` for POST, `204` for PATCH/PUT/DELETE) |
| `return=representation` | the affected rows, shaped by `select` |
| `count=exact` | `Content-Range` ending in `/<n>`, the number of affected rows |
| `resolution=merge-duplicates` / `ignore-duplicates` | upsert behaviour of `POST` |
| `missing=default` | with `?columns=`, missing keys use column defaults |
| `tx=rollback` | run the request, then roll back: nothing is saved |

Applied preferences are echoed in `Preference-Applied`. Combine them with commas or several
`Prefer` headers.

## Value formats

| Firebird type | JSON |
|---|---|
| `SMALLINT`, `INTEGER`, `BIGINT` | number |
| `INT128`, `NUMERIC`, `DECIMAL`, `DECFLOAT` | number, printed exactly as stored (`9.99`, not `9.9900000001`) |
| `FLOAT`, `DOUBLE PRECISION` | number |
| `BOOLEAN` | `true` / `false` |
| `DATE` | `"2024-05-06"` |
| `TIME` | `"13:14:15"` |
| `TIMESTAMP` | `"2024-05-06T13:14:15.1234"` |
| `... WITH TIME ZONE` | ISO 8601 with offset |
| `CHAR`, `VARCHAR`, `BLOB SUB_TYPE TEXT` | string |
| `BLOB SUB_TYPE BINARY`, `OCTETS` | base64 string (also accepted on input) |

On input, ISO 8601 timestamps (`2026-10-18T10:00:00+03:00`) and JSON numbers or strings for
numeric columns are accepted. JSON objects and arrays are stored as their text.
