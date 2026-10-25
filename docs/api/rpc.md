# Procedures and functions

Stored procedures and PSQL functions are called at `/rpc/<name>`.

| Routine | Called as | Response |
|---|---|---|
| Selectable procedure (with `SUSPEND`) | `SELECT * FROM proc(...)` | array of rows |
| Executable procedure | `EXECUTE PROCEDURE proc(...)` | object of output parameters, or `204` if it has none |
| PSQL function (Firebird 3+) | `SELECT fn(...) FROM RDB$DATABASE` | a single JSON value |

## Arguments

With `GET`, arguments come from the query string. With `POST`, they come from a JSON object:

```sh
curl 'localhost:3000/rpc/books_by_country?country=UK'
curl -X POST localhost:3000/rpc/place_order -H 'Content-Type: application/json' \
     -d '{"customer_id":1,"book_id":2}'
curl 'localhost:3000/rpc/add_vat?amount=100'                 # 119.00
curl 'localhost:3000/rpc/add_vat?amount=100&rate=10'         # 110.00
```

Arguments are matched by name. A missing argument is `NULL`, unless it and every argument after
it have defaults, in which case the defaults apply (`place_order`'s `qty = 1`). Unknown argument
names in a `POST` body are rejected.

`GET` runs in a **read-only transaction**. Use `POST` for routines that change data.

## Calling a routine many times

With `Prefer: params=multiple-objects`, a `POST` body that is an array of argument objects calls
the routine once per object, in one transaction:

```sh
curl -X POST 'localhost:3000/rpc/add_vat' -H 'Prefer: params=multiple-objects' \
     -H 'Content-Type: application/json' -d '[{"amount":100},{"amount":100,"rate":10}]'
# [119.00,110.00]
```

| Routine | Response |
|---|---|
| Selectable procedure | the rows of every call together, as one result |
| Executable procedure | an array with the output parameters of each call, or `204` if it has none |
| PSQL function | an array with the value of each call |

- For a selectable procedure the calls are combined into one query, so `select`, filters,
  `order`, `limit` and `Prefer: count` apply to the combined rows:
  `POST /rpc/books_by_country?order=price&limit=5` with `[{"country":"UK"},{"country":"USA"}]`
  returns the 5 cheapest books of both countries. The query calls the procedure once per row of
  a counting CTE, with each call's arguments as parameters, so it takes up to 1024 objects, as
  long as their arguments fit in 48 KB in all (`FBRST128`); split larger arrays.
- The calls run in the request's transaction: if one fails, none of them is saved.
- A single object instead of an array is one call, and the response is still an array.

## Passing the whole body

With `Prefer: params=single-object`, the raw JSON body is passed to a routine that has exactly one
text parameter (`VARCHAR`/`CHAR` or `BLOB SUB_TYPE TEXT`). Firebird has no JSON type, so the
routine receives the JSON document as text, for example to store it:

```sql
CREATE PROCEDURE log_request (payload BLOB SUB_TYPE TEXT) RETURNS (id INTEGER) AS
BEGIN
  INSERT INTO api_log (payload) VALUES (:payload) RETURNING id INTO :id;
END
```

```sh
curl -X POST 'localhost:3000/rpc/log_request' -H 'Prefer: params=single-object' \
     -H 'Content-Type: application/json' -d '{"event":"signup","tags":["a","b"]}'
# {"id":1}
```

The body must be valid JSON, and fit the parameter when it is a `VARCHAR`. A routine with another
signature is refused with `FBRST129`.

## Raw bodies

A `POST` whose `Content-Type` is neither JSON nor a form puts the body, as it is, into the
routine's only parameter: `text/*` bodies as text, anything else as bytes. That's the way to
upload a file into a `BLOB`:

```sh
curl -X POST localhost:3000/rpc/blob_size -H 'Content-Type: image/png' --data-binary @cover.png \
  -H 'Accept: text/plain'
# 173
curl -X POST localhost:3000/rpc/log_request -H 'Content-Type: text/plain' -d 'plain text, not JSON'
```

A routine with several parameters gets `400` (`FBRST133`): send a JSON object instead. The body
is limited to 64 MB.

## Shaping selectable procedures

The output of a selectable procedure can be selected, filtered, ordered, paged and counted like
a table. With `GET`, query parameters that aren't argument names are treated as filters:

```
GET /rpc/books_by_country?country=UK&select=title&price=lt.9&order=price&limit=1
```

With `POST`, the arguments are in the body and the whole query string shapes the result.

## Packages

Routines declared in a package header are available as `/rpc/<package>.<routine>`:

```sql
CREATE PACKAGE catalog AS
BEGIN
  PROCEDURE search (term VARCHAR(100)) RETURNS (id INTEGER, title VARCHAR(200));
  FUNCTION book_count (author_id INTEGER) RETURNS INTEGER;
END
```

```sh
curl 'localhost:3000/rpc/catalog.search?term=god'
curl 'localhost:3000/rpc/catalog.book_count?author_id=1'
```

Routines that exist only in the package body are private and not exposed.

## Permissions

```sql
GRANT EXECUTE ON PROCEDURE books_by_country TO ROLE web_anon;
GRANT EXECUTE ON FUNCTION add_vat TO ROLE web_anon;
GRANT EXECUTE ON PACKAGE catalog TO ROLE web_anon;

-- a routine needs its own rights on the tables it uses
GRANT INSERT ON orders TO PROCEDURE place_order;
```

## Errors

A PSQL `EXCEPTION` raised by the routine (or by a trigger it fires) becomes a `400` response. The
exception's message leads, its name is in `hint`, and where it was raised is in `details`:

```sql
CREATE EXCEPTION out_of_stock 'This book is out of stock';

-- in the procedure
IF (NOT EXISTS (SELECT 1 FROM books b WHERE b.id = :book_id AND b.in_stock)) THEN
  EXCEPTION out_of_stock;

-- Firebird 3+: a routine needs USAGE on the exceptions it raises
GRANT USAGE ON EXCEPTION out_of_stock TO PROCEDURE place_order;
```

```json
{"code":"HY000","message":"This book is out of stock","details":"At procedure 'PLACE_ORDER' line: 6, col: 5","hint":"exception OUT_OF_STOCK"}
```

The same applies to exceptions raised by triggers on tables you write to.
