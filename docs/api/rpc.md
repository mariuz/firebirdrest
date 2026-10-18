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
