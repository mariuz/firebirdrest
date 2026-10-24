package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// Read requests reuse transactions. A GET or HEAD runs in a read-only READ
// COMMITTED transaction, where every statement sees the data committed when
// it starts, so one such transaction serves request after request with the
// same results as a fresh one each time, and saves the transaction start and
// commit round trips of every read. Firebird treats read-only read-committed
// transactions as pre-committed, so a long-lived one doesn't hold back
// garbage collection. Each readTx owns its pooled connection; a role's
// pool keeps the idle ones.
type readTx struct {
	conn    *sql.Conn
	tx      *sql.Tx
	uses    int
	started time.Time
	// vars are the request context variables the last request set, to be
	// cleared by the next one (they live for the transaction)
	vars map[string]bool
}

// Recycling limits: a transaction is committed and its connection returned
// to the pool after this many requests or this long, so resources it has
// accumulated are released.
var (
	maxReadTxUses = 1000
	maxReadTxAge  = 5 * time.Minute
)

// readTxPool holds the idle read transactions of one role.
type readTxPool struct {
	mu   sync.Mutex
	idle []*readTx
}

// acquire returns an idle read transaction, or starts one.
func (p *rolePool) acquireRead(ctx context.Context) (*readTx, error) {
	p.reads.mu.Lock()
	if n := len(p.reads.idle); n > 0 {
		rt := p.reads.idle[n-1]
		p.reads.idle = p.reads.idle[:n-1]
		p.reads.mu.Unlock()
		return rt, nil
	}
	p.reads.mu.Unlock()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	// begun with a context that outlives the request: database/sql rolls a
	// transaction back when the context it was begun with ends
	tx, err := conn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &readTx{conn: conn, tx: tx, started: time.Now()}, nil
}

// releaseRead returns a read transaction for reuse, or ends it when it is
// worn out, its connection is in doubt, or keeping it would leave writes
// without a connection: idle read transactions hold pooled connections, so
// at most db-pool - 1 are kept.
func (p *rolePool) releaseRead(rt *readTx, broken bool) {
	rt.uses++
	if broken || rt.uses >= maxReadTxUses || time.Since(rt.started) > maxReadTxAge {
		rt.end()
		return
	}
	p.reads.mu.Lock()
	if len(p.reads.idle) >= p.db.Stats().MaxOpenConnections-1 {
		p.reads.mu.Unlock()
		rt.end()
		return
	}
	p.reads.idle = append(p.reads.idle, rt)
	p.reads.mu.Unlock()
}

// freeForWrite ends an idle read transaction when the pool is exhausted, so
// a write about to ask for a connection gets one instead of waiting.
func (p *rolePool) freeForWrite() {
	st := p.db.Stats()
	if st.OpenConnections < st.MaxOpenConnections || st.Idle > 0 {
		return
	}
	p.reads.mu.Lock()
	var rt *readTx
	if n := len(p.reads.idle); n > 0 {
		rt = p.reads.idle[n-1]
		p.reads.idle = p.reads.idle[:n-1]
	}
	p.reads.mu.Unlock()
	if rt != nil {
		rt.end()
	}
}

// end commits the transaction (nothing was written) and returns the
// connection to the pool.
func (rt *readTx) end() {
	rt.tx.Commit()
	rt.conn.Close()
}

// closeReads ends every idle read transaction.
func (p *rolePool) closeReads() {
	p.reads.mu.Lock()
	idle := p.reads.idle
	p.reads.idle = nil
	p.reads.mu.Unlock()
	for _, rt := range idle {
		rt.end()
	}
}

// connectionLost reports errors after which a connection shouldn't be
// reused: the network failed, or a cancellation may have left a reply
// unread.
func connectionLost(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
