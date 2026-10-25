# Stability promise

What you can build on, what may still change, and how changes are announced. FirebirdREST
follows [semantic versioning](https://semver.org): until 1.0 a minor version (0.x → 0.y) may
change behaviour, and every such change is listed under *behaviour changes* at the top of the
version's [changelog](../changelog.md) entry. From 1.0 on, the interfaces marked **stable**
below change only in a major version.

## Stable

These are the interfaces applications depend on. They are covered by the test suite on
Firebird 3, 4 and 5, and by the [firebirdrest-js](https://github.com/mariuz/firebirdrest-js)
compatibility suite.

| Interface | What is promised |
|---|---|
| **URL grammar** | `select`, filters and operators, `order`, `limit`/`offset`, logic trees, embedding (`!inner`, `!hint`, spread), aggregates, casts, `/rpc/<name>` and `/rpc/<package>.<name>`, as documented in the [API](../api/reading.md). New operators and parameters may be added; existing ones keep their meaning. |
| **Request headers** | `Prefer` values, `Range`, `Accept` media types, `Accept-Profile`/`Content-Profile`, `Authorization: Bearer`. |
| **Response headers** | `Content-Range`, `Range-Unit`, `Preference-Applied`, `Content-Profile`, `Location`, `Content-Type`. The `nosniff`/CSP security headers stay. |
| **Status codes** | As listed in [errors](../api/formats-errors.md#errors). |
| **Error body** | `{code, message, details, hint}`. `FBRST` codes are never reused for another meaning; a code may be retired, never redefined. Firebird errors keep Firebird's SQLSTATE as `code`. `message` text may be reworded. |
| **JSON encoding of types** | numbers as JSON numbers (exact numerics as numbers, not strings), `BOOLEAN` as booleans, dates and times as ISO 8601 strings, `WITH TIME ZONE` values with their offset, binary as base64, `BLOB SUB_TYPE TEXT` as strings. |
| **Request context variables** | the `request.*` names available to PSQL through `RDB$GET_CONTEXT`. |
| **Configuration** | every key in the [configuration reference](configuration.md), its `FBREST_*` environment variable, and its default. A default changes only in a minor version before 1.0, in a major one after. |
| **Admin endpoints** | `/live`, `/ready`, `/reload`, `/metrics`. |
| **Metric names** | `firebirdrest_*` metrics and their labels. Metrics may be added. |
| **Command line** | `firebirdrest [config-file]`, `-version`, `-check`. |
| **Docker image** | `ghcr.io/mariuz/firebirdrest` with tags `X.Y.Z`, `X.Y` and `latest`, running as a non-root user on port 3000, configured through `FBREST_*`. |
| **Firebird versions** | 3.0, 4.0 and 5.0. A Firebird version is dropped only in a major release, after its vendor end of life. |

## Experimental

Available and tested, but may change in a minor version while the ground under it moves.
Each is marked as such in its documentation.

| Feature | Why |
|---|---|
| [SQL schemas](schemas.md) (`db-schemas`, `Accept-Profile` on Firebird 6) | Firebird 6 is not released; its schema catalog may still change. |
| Firebird 6 support in general | Tested against the nightly snapshot in CI, with known driver issues listed under [Deployment](deployment.md#firebird-6). |
| Query plan output (`Accept: application/vnd.pgrst.plan`) | The text is Firebird's explained plan, which differs between Firebird versions; the JSON shape `[{query, plan, tree}]` is stable, the plan text and the node texts of the tree are not. |
| `Server-Timing` metric names | Follow PostgREST's; may gain entries. |

## Not covered

- The exact text of `message`, `details` and `hint` in errors, and of Firebird's own messages.
- The format of log lines.
- The OpenAPI document's descriptions and the order of its entries (its structure follows the
  OpenAPI 3.0 specification).
- The Go packages under `internal/`: FirebirdREST is a program, not a library.
- Statement text shown in query plans.
- Undocumented behaviour. If you rely on something that isn't documented, open an issue so it
  can be documented or changed deliberately.

## How changes are made

- **Additions** (a new operator, header, setting, metric or error code) can arrive in any
  release and are listed under *Added*.
- **Behaviour changes** are listed first in the version's changelog entry, under *behaviour
  changes to check before upgrading*, with the setting that restores the old behaviour when
  there is one.
- **Deprecations**: a setting or feature to be removed keeps working for at least one minor
  version while the server logs a warning at startup naming its replacement. Renamed settings
  accept the old name during that time.
- **Removals** happen in a major version (or, before 1.0, in a minor version after the
  deprecation period), and are listed under *Removed*.
- **Security fixes** may change behaviour in a patch release when the old behaviour is the
  vulnerability; the changelog says so.

Before deploying a new version, run the binary with `-check` against your configuration: it
loads the configuration, reports unknown or invalid settings and deprecation warnings, and
exits without connecting to the database. See the [upgrade guide](../upgrading.md).
