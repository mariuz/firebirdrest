# Computed fields and relationships

A function or procedure whose input parameters are **all** declared
`TYPE OF COLUMN <table>.<column>` of the same table is attached to that table. FirebirdREST
passes the row's columns as arguments. You don't need to configure anything else.

| Routine | Becomes | Example |
|---|---|---|
| PSQL function | a **computed field** | `books?select=title,price_with_vat` |
| Selectable procedure (with `SUSPEND`) | a **to-many** relationship (array) | `books?select=title,similar_books(title)` |
| Executable procedure | a **to-one** relationship (object of its output parameters) | `authors?select=name,author_stats(*)` |

PostgREST does this with functions that take a table row as their argument. Firebird routines
can't, so the `TYPE OF COLUMN` declaration marks which table a routine belongs to.

## Computed fields

```sql
CREATE FUNCTION price_with_vat (price TYPE OF COLUMN books.price)
RETURNS NUMERIC(12,2)
AS
BEGIN
  RETURN price * 1.19;
END

CREATE FUNCTION label (title TYPE OF COLUMN books.title, published TYPE OF COLUMN books.published)
RETURNS VARCHAR(220)
AS
BEGIN
  RETURN title || ' (' || published || ')';
END
```

A computed field works like a column in `select`, filters, `order` and aggregates:

```
GET /books?select=id,label,price_with_vat&price_with_vat=gt.12&order=price_with_vat.desc
[{"id":3,"label":"The Dispossessed (1974)","price_with_vat":14.28},{"id":5,"label":"Solaris (1961)","price_with_vat":12.38}]

GET /books?select=price_with_vat.sum()
```

Computed fields are not included in `select=*`; name them.

## To-many relationships

```sql
CREATE PROCEDURE similar_books (book_id TYPE OF COLUMN books.id, author_id TYPE OF COLUMN books.author_id)
RETURNS (id INTEGER, title VARCHAR(200))
AS
BEGIN
  FOR SELECT b.id, b.title FROM books b
      WHERE b.author_id = :author_id AND b.id <> :book_id
      INTO :id, :title
  DO SUSPEND;
END
```

```
GET /books?select=title,similar_books(title)
GET /books?select=title,similar_books(title)&similar_books.title=starting.S&similar_books.limit=3
GET /books?select=title,similar_books!inner(title)       # only books that have similar ones
GET /books?select=title,similar_books(count)
GET /books?select=title,...similar_books(similar:title)  # spread: "similar":["Small Gods"]
```

The procedure runs once per distinct parent key, all in one batched query:

```sql
SELECT ... FROM (SELECT DISTINCT k.ID, k.AUTHOR_ID FROM BOOKS k WHERE k.ID IN (...)) k
CROSS JOIN SIMILAR_BOOKS(k.ID, k.AUTHOR_ID) t0
```

## To-one relationships

```sql
CREATE PROCEDURE author_stats (author_id TYPE OF COLUMN authors.id)
RETURNS (book_count INTEGER, avg_price NUMERIC(10,2))
AS
BEGIN
  SELECT COUNT(*), AVG(b.price) FROM books b WHERE b.author_id = :author_id
    INTO :book_count, :avg_price;
END
```

```
GET /authors?select=name,author_stats(*)
[{"name":"Terry Pratchett","author_stats":{"book_count":2,"avg_price":9.24}}, ...]

GET /authors?select=name,stats:author_stats(n:book_count)
GET /authors?select=name,...author_stats(book_count)
```

An executable procedure runs once per distinct parent key with `EXECUTE PROCEDURE`. Firebird
can't select from it, so filters, `order`, paging, aggregates and `!inner` are not available on
to-one computed relationships. Use a selectable procedure if you need them.

## Permissions

The request's role needs `EXECUTE` on the routine, and the routine needs access to the tables
it reads:

```sql
GRANT EXECUTE ON FUNCTION price_with_vat TO ROLE web_anon;
GRANT EXECUTE ON PROCEDURE similar_books TO ROLE web_anon;
GRANT SELECT ON books TO PROCEDURE similar_books;
```
