# Realtime events

Firebird can tell its clients that something happened: a trigger or procedure runs
`POST_EVENT 'order_placed'`, and when its transaction commits every attachment listening for that
name is notified. FirebirdREST forwards these notifications to HTTP clients as
[Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html), so a page or
a service can react to a change without polling. PostgREST has no equivalent; Supabase provides it
through a separate service.

## Exposing events

Events carry no data and Firebird checks no privilege before delivering them, so FirebirdREST
only streams the names you list:

```ini
db-events = "order_placed, stock_changed"
db-events-roles = "web_user"       # optional: only these roles may listen
```

Without `db-events` the endpoint is off (`404`, `FBRST135`). Without `db-events-roles` any
authenticated role may listen; the anonymous role may listen only when it is listed. As for any
request, the role must be granted to the authenticator user.

In the database, post the event where the change happens:

```sql
CREATE TRIGGER orders_posted AFTER INSERT ON orders
AS
BEGIN
  POST_EVENT 'order_placed';
END
```

The [example database](https://github.com/mariuz/firebirdrest/blob/main/examples/bookstore.sql)
posts `order_placed` from the `place_order` procedure and has a `notify(name)` procedure that posts
any event, for trying things out.

## Listening

```sh
curl -N 'localhost:3000/events/order_placed,stock_changed' -H "Authorization: Bearer $JWT"
```

```text
retry: 3000
: listening order_placed,stock_changed

: keepalive

id: 1
event: order_placed
data: {"name":"order_placed","count":1}
```

- The path names one event or several, comma separated. Names are matched as Firebird stores
  them: `stock_changed` and `STOCK_CHANGED` are the same event.
- Every notification is one SSE message: `event` is the name, `data` repeats it with `count`,
  the number of posts Firebird merged into this notification (one transaction posting the same
  event ten times is delivered once, with `count: 10`), and `id` counts the messages of the
  stream.
- A comment line (`: keepalive`) is sent every 15 seconds so proxies and clients keep the
  connection open. `retry: 3000` tells `EventSource` clients to reconnect after 3 seconds.
- The stream is not subject to `server-request-timeout`; it ends when the client closes it or
  the server shuts down (then with `event: error` and `data: {"message":"server shutting down"}`).
- An event that isn't exposed is `404` (`FBRST136`); a role that may not listen gets `403`
  (`FBRST137`, `401` when anonymous).

A client hears *that* something changed, not what: it then reads the data it needs through the
API, under its own role and grants. That keeps events safe to expose to many roles: an event name
is the only thing they learn.

Browsers' `EventSource` can't send an `Authorization` header. Use `fetch` with a streaming body
instead, as [firebirdrest-js](https://github.com/mariuz/firebirdrest-js) does, or put the token in
a cookie that a proxy turns into the header.

## How it works

One Firebird attachment listens for all exposed events and fans the notifications out to the
HTTP streams in process: a thousand listeners cost the database one connection. If the
connection to the database is lost the server resubscribes every 30 seconds; streams opened
meanwhile get `event: error` with `"not listening"` and should reconnect. A stream that isn't
read fast enough loses notifications rather than slowing the others (a comment in the stream
says how many).

With several FirebirdREST instances each listens by itself, so a client connected to any of them
hears every event.

### With firebirdrest-js

```ts
const channel = client.channel('order_placed', 'stock_changed')
  .on('order_placed', (e) => refreshOrders())      // e.name, e.count
  .on('*', (e) => console.log('event', e.name))
  .subscribe((status) => console.log(status))     // "open", "closed", "error"
// later
channel.unsubscribe()
```

The channel reconnects by itself with a growing delay while the server is unreachable.

## Metrics

With `db-events` set, `/metrics` reports open streams, notifications received by event,
notifications delivered, and times the database subscription was lost
([monitoring](../references/observability.md#events)).
