# FirebirdREST

**Serve a RESTful API from any [Firebird](https://firebirdsql.org) database.**

FirebirdREST is a standalone web server that turns a Firebird database into a REST API, in the
spirit of [PostgREST](https://postgrest.org). Your tables, views, stored procedures and functions
become endpoints, and the grants and roles you already have in Firebird decide what each caller
may do. Because the API is derived from the schema, there is no API layer to write or maintain.

```console
$ curl 'http://localhost:3000/books?select=title,price,authors(name)&price=lt.10&order=price.desc'
[
  {"title":"Guards! Guards!","price":9.99,"authors":{"name":"Terry Pratchett"}},
  {"title":"Small Gods","price":8.50,"authors":{"name":"Terry Pratchett"}},
  {"title":"A Wizard of Earthsea","price":7.25,"authors":{"name":"Ursula K. Le Guin"}}
]
```

## Features

- **One static binary** for Linux, macOS, Windows and FreeBSD. It talks the Firebird wire protocol
  directly, so no client library is needed.
- **PostgREST's URL grammar**: [`select`, filters, logic trees, ordering and paging](api/reading.md).
- **[Resource embedding](api/embedding.md)** through foreign keys: many-to-one, one-to-many,
  many-to-many, nested, `!inner` and spread (`...rel(cols)`).
- **[Aggregates](api/aggregates.md)**: `count()`, `sum()`, `avg()`, `min()`, `max()`.
- **[Computed fields and relationships](api/computed.md)** from functions and procedures.
- **[Writes](api/writing.md)**: bulk inserts, upserts, `PATCH`, `PUT` and `DELETE`, returning the
  written rows on request.
- **[Stored procedures and functions](api/rpc.md)**, including routines in packages.
- **[Security through Firebird roles](references/security.md)**: a JWT's `role` claim selects the
  Firebird `ROLE`, and claims can be read in PSQL for row-level rules.
- **OpenAPI 3** description at `/`, CSV output, health endpoints, schema reload on `POST_EVENT`.
- **[Realtime events](api/events.md)**: Firebird `POST_EVENT`s streamed to clients as Server-Sent
  Events, something PostgREST does not have.
- Firebird **3, 4 and 5**, all tested in CI, plus early testing against the
  [Firebird 6 snapshot](references/deployment.md#firebird-6).
- **[SQL schemas](references/schemas.md)** on Firebird 6 (experimental), chosen per request with `Accept-Profile`
  and `Content-Profile`.

## Where to start

<div class="grid cards" markdown>

- [**Getting started**](tutorials/getting-started.md): run the example database with Docker and
  make your first requests in five minutes.
- [**Authentication**](tutorials/authentication.md): add a JWT and see grants decide what each
  role can see.
- [**API reference**](api/reading.md): everything a URL can say.
- [**Configuration**](references/configuration.md): every setting.

</div>

## Install

=== "Binary"

    Download an archive for your platform from the
    [releases page](https://github.com/mariuz/firebirdrest/releases), for example:

    ```sh
    curl -L https://github.com/mariuz/firebirdrest/releases/download/v0.7.0/firebirdrest_0.7.0_linux_amd64.tar.gz | tar xz
    ./firebirdrest --version
    ```

=== "Docker"

    ```sh
    docker run -p 3000:3000 \
      -e FBREST_DB_URI='authenticator:secret@db.example.com:3050/data/app.fdb' \
      -e FBREST_DB_ANON_ROLE=web_anon \
      ghcr.io/mariuz/firebirdrest:latest
    ```

=== "Go"

    ```sh
    go install github.com/mariuz/firebirdrest/cmd/firebirdrest@latest
    ```
