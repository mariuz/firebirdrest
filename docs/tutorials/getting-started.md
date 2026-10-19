# Getting started

This tutorial runs Firebird 5 with a small bookstore database, puts FirebirdREST in front of it
and walks through the main kinds of requests. You need Docker with Compose and `curl`.

## 1. Start everything

```sh
git clone https://github.com/mariuz/firebirdrest
cd firebirdrest
docker compose up --build
```

Compose starts three containers:

| Service | Address | What it is |
|---|---|---|
| `firebird` | `localhost:3050` | Firebird 5 with [`examples/bookstore.sql`](https://github.com/mariuz/firebirdrest/blob/main/examples/bookstore.sql) and [`bookstore-fb4.sql`](https://github.com/mariuz/firebirdrest/blob/main/examples/bookstore-fb4.sql) loaded |
| `firebirdrest` | <http://localhost:3000> | the API |
| `swagger` | <http://localhost:8080> | Swagger UI reading the API's OpenAPI document |

The example has `authors`, `books`, `tags` (linked to books through `book_tags`), `customers`,
`orders` and `order_items`, plus a view, a few procedures and functions, and two roles:
`web_anon` can read the catalogue and `web_user` can do everything.

## 2. Read a table

```sh
curl 'localhost:3000/books'
```

Every table and view is an endpoint. Unquoted Firebird names are upper case in the database
and lower case in the API, so `BOOKS` is `/books` and `AUTHOR_ID` is `author_id`.

Pick columns, filter and sort:

```sh
curl 'localhost:3000/books?select=title,price&price=lt.10&order=price.desc'
```

```json
[{"title":"Guards! Guards!","price":9.99},{"title":"Small Gods","price":8.50},{"title":"A Wizard of Earthsea","price":7.25}]
```

Filters take the form `column=operator.value`. A few more:

```sh
curl 'localhost:3000/books?select=title&title=ilike.*god*'          # case-insensitive LIKE
curl 'localhost:3000/books?select=title&published=gte.1970&in_stock=is.true'
curl 'localhost:3000/books?select=title&or=(price.lt.8,published.lt.1965)'
```

See [Reading tables](../api/reading.md) for every operator.

## 3. Follow relationships

Foreign keys let you pull related rows into the same response:

```sh
curl 'localhost:3000/books?select=title,authors(name,country)&id=eq.1'
```

```json
[{"title":"Guards! Guards!","authors":{"name":"Terry Pratchett","country":"UK"}}]
```

It works in every direction, and through junction tables:

```sh
curl 'localhost:3000/authors?select=name,books(title)&id=eq.2'      # one-to-many
curl 'localhost:3000/books?select=title,tags(name)&id=eq.1'          # many-to-many via book_tags
```

Add `!inner` to keep only rows that have a match, for example books by Polish authors:

```sh
curl 'localhost:3000/books?select=title,authors!inner(name)&authors.country=eq.Poland'
```

## 4. Count and summarise

```sh
curl 'localhost:3000/books?select=author_id,count(),total:price.sum()&order=author_id'
curl 'localhost:3000/authors?select=name,books(count)'
```

## 5. Call a procedure

Stored procedures and functions live under `/rpc`:

```sh
curl 'localhost:3000/rpc/books_by_country?country=UK'               # selectable procedure
curl 'localhost:3000/rpc/add_vat?amount=100'                        # function: 119.00
```

## 6. Try a write

Anonymous requests run as `web_anon`, which may only read:

```sh
curl -i -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '{"name":"classic"}'
```

```http
HTTP/1.1 401 Unauthorized
```

```json
{"code":"28000","message":"no permission for INSERT access to TABLE TAGS","details":"Effective user is AUTHENTICATOR","hint":null}
```

That is Firebird refusing the `INSERT`, not FirebirdREST. Continue with the
[authentication tutorial](authentication.md) to send requests as `web_user`.

## 7. Explore the API

Open <http://localhost:8080> for Swagger UI, or fetch the OpenAPI document directly:

```sh
curl localhost:3000/
```

## Using your own database

Point FirebirdREST at any Firebird 3, 4 or 5 database:

```sh
export FBREST_DB_URI='authenticator:secret@localhost:3050/var/lib/firebird/data/app.fdb'
export FBREST_DB_ANON_ROLE=web_anon
firebirdrest
```

Connect as a dedicated user with no privileges of its own, and grant it the roles requests should
use. [Security and roles](../references/security.md) explains why and how.
