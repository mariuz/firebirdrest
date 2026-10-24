package server

import (
	"container/list"
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"
)

// stmtCache keeps prepared statements per connection pool, keyed by SQL text.
// Firebird compiles every statement on prepare, which costs about as much as
// running a simple query, so reusing them roughly halves the per-query cost.
// database/sql prepares a cached statement lazily on each connection and
// runs it inside any transaction of that connection via Tx.StmtContext.
type stmtCache struct {
	db   *sql.DB
	max  int
	idle time.Duration // statements unused for this long are closed
	stop chan struct{}

	mu      sync.Mutex
	ll      *list.List // front = most recently used
	items   map[string]*list.Element
	pending map[string]bool // being prepared in the background

	hits, misses atomic.Uint64
}

type cacheEntry struct {
	sql      string
	stmt     *sql.Stmt
	lastUsed time.Time
}

// newStmtCache creates a cache. Prepared statements hold Firebird metadata
// locks, so DROP of a table used by a cached statement waits until it is
// closed: idle statements are therefore closed after idle.
func newStmtCache(db *sql.DB, max int, idle time.Duration) *stmtCache {
	c := &stmtCache{db: db, max: max, idle: idle, stop: make(chan struct{}),
		ll: list.New(), items: map[string]*list.Element{}, pending: map[string]bool{}}
	if idle > 0 {
		go c.janitor()
	}
	return c
}

func (c *stmtCache) janitor() {
	every := c.idle / 3
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.mu.Lock()
			// the list is ordered by use, so expired entries are at the back
			for el := c.ll.Back(); el != nil; el = c.ll.Back() {
				e := el.Value.(*cacheEntry)
				if now.Sub(e.lastUsed) < c.idle {
					break
				}
				c.ll.Remove(el)
				delete(c.items, e.sql)
				e.stmt.Close()
			}
			c.mu.Unlock()
		}
	}
}

func (c *stmtCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// close stops the janitor and closes every statement.
func (c *stmtCache) close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	c.clear()
}

// get returns the cached statement for query, or nil on a miss. A miss
// schedules a background prepare: preparing through the pool needs a free
// connection, and waiting for one while the caller holds a connection in its
// transaction could deadlock a busy pool.
func (c *stmtCache) get(query string) *sql.Stmt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[query]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*cacheEntry)
		e.lastUsed = time.Now()
		c.hits.Add(1)
		return e.stmt
	}
	c.misses.Add(1)
	if !c.pending[query] {
		c.pending[query] = true
		go c.prepare(query)
	}
	return nil
}

func (c *stmtCache) prepare(query string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.db.PrepareContext(ctx, query)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, query)
	if err != nil {
		return // the caller ran it directly and saw any error
	}
	select {
	case <-c.stop:
		st.Close()
		return
	default:
	}
	if _, ok := c.items[query]; ok {
		st.Close()
		return
	}
	c.items[query] = c.ll.PushFront(&cacheEntry{sql: query, stmt: st, lastUsed: time.Now()})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		e := old.Value.(*cacheEntry)
		delete(c.items, e.sql)
		e.stmt.Close() // in-flight uses keep working: database/sql closes it after them
	}
}

// clear closes every cached statement (after DDL / schema reload).
func (c *stmtCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, el := range c.items {
		el.Value.(*cacheEntry).stmt.Close()
	}
	c.items = map[string]*list.Element{}
	c.ll.Init()
}

// cachedTx runs queries of one transaction through the statement cache.
// It satisfies query.Querier.
type cachedTx struct {
	tx    *sql.Tx
	conn  *sql.Conn  // the connection tx runs on
	cache *stmtCache // nil disables caching
	// record, when set, collects the SQL of every statement (for query plans)
	record *[]string
}

func (t *cachedTx) note(query string) {
	if t.record != nil {
		*t.record = append(*t.record, query)
	}
}

// stmt returns a transaction-bound cached statement, or nil.
func (t *cachedTx) stmt(ctx context.Context, query string) *sql.Stmt {
	if t.cache == nil {
		return nil
	}
	if st := t.cache.get(query); st != nil {
		return t.tx.StmtContext(ctx, st)
	}
	return nil
}

// isClosed reports the race where a statement was evicted between get and use.
func isClosed(err error) bool {
	return err != nil && err.Error() == "sql: statement is closed"
}

func (t *cachedTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	t.note(query)
	if st := t.stmt(ctx, query); st != nil {
		rows, err := st.QueryContext(ctx, args...)
		if !isClosed(err) {
			return rows, err
		}
	}
	return t.tx.QueryContext(ctx, query, args...)
}

func (t *cachedTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	t.note(query)
	if st := t.stmt(ctx, query); st != nil {
		res, err := st.ExecContext(ctx, args...)
		if !isClosed(err) {
			return res, err
		}
	}
	return t.tx.ExecContext(ctx, query, args...)
}

// PrepareContext returns a statement for repeated use within the transaction.
func (t *cachedTx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	t.note(query)
	if st := t.stmt(ctx, query); st != nil {
		return st, nil
	}
	return t.tx.PrepareContext(ctx, query)
}
