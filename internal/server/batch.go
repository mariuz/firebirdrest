package server

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

// Multi-row inserts run in blocks: the statements of a chunk of rows go into
// one EXECUTE BLOCK, each followed by SUSPEND, so the block returns one row of
// RETURNING values per inserted row. A body of n rows then costs a few round
// trips instead of n. Parameters are declared TYPE OF COLUMN, so they take
// the column's type, including BLOBs; the blocks work on Firebird 3 and later.

// rowStmt is one row's INSERT/UPDATE OR INSERT/MERGE, with :pN parameters
// numbered from first, and the column each parameter belongs to.
type rowStmt struct {
	sql  string
	args []any
	cols []*schema.Column
}

// Block limits. Firebird's request messages are limited to 64 KB each way,
// so chunks are sized by the declared parameter widths; the row count keeps
// the statement text and compile time reasonable.
const (
	maxBlockRows  = 100
	maxBlockBytes = 48 << 10
)

// paramBytes estimates the bytes a parameter of column c takes in the
// message: text columns may hold 4 bytes per character under UTF8.
func paramBytes(c *schema.Column) int {
	switch c.FieldType {
	case schema.TVarchar, schema.TChar:
		n := c.CharLength
		if n <= 0 {
			n = c.Length
		}
		return n*4 + 4
	case schema.TBlob:
		return 8 + 2
	}
	return 16 + 2 // numbers, dates, booleans with their null indicator
}

// paramCols lists the columns of a row's parameters, in the order
// writeRowStatement binds them.
func paramCols(d rowData, resolution string, conflict []*schema.Column) []*schema.Column {
	if resolution == "ignore-duplicates" {
		return append(append([]*schema.Column{}, conflict...), d.cols...)
	}
	return d.cols
}

// chunkRows splits rows into blocks that fit the message limits.
func chunkRows(rows []rowData, resolution string, conflict, retCols []*schema.Column) [][]rowData {
	outRow := 0
	for _, c := range retCols {
		outRow += paramBytes(c)
	}
	var chunks [][]rowData
	var cur []rowData
	in, out := 0, 0
	for _, d := range rows {
		rowIn := 0
		for _, c := range paramCols(d, resolution, conflict) {
			rowIn += paramBytes(c)
		}
		if len(cur) > 0 && (len(cur) >= maxBlockRows || in+rowIn > maxBlockBytes || out+outRow > maxBlockBytes) {
			chunks = append(chunks, cur)
			cur, in, out = nil, 0, 0
		}
		cur = append(cur, d)
		in += rowIn
		out += outRow
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// blockSQL wraps a chunk of row statements in an EXECUTE BLOCK that returns
// retCols for each row. The statements must have been built with Named
// parameters numbered consecutively from 0 across the chunk.
func blockSQL(rel *schema.Relation, chunk []rowStmt, retCols []*schema.Column) (string, []any) {
	var sb strings.Builder
	var args []any
	sb.WriteString("EXECUTE BLOCK")
	n := 0
	for _, st := range chunk {
		for _, c := range st.cols {
			if n == 0 {
				sb.WriteString(" (")
			} else {
				sb.WriteString(", ")
			}
			sb.WriteString("p" + strconv.Itoa(n) + " TYPE OF COLUMN " + rel.SQLName() + "." + schema.Quote(c.DBName) + " = ?")
			n++
		}
		args = append(args, st.args...)
	}
	if n > 0 {
		sb.WriteString(")")
	}
	sb.WriteString(" RETURNS (")
	into := make([]string, len(retCols))
	for i, c := range retCols {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("r" + strconv.Itoa(i) + " TYPE OF COLUMN " + rel.SQLName() + "." + schema.Quote(c.DBName))
		into[i] = ":r" + strconv.Itoa(i)
	}
	sb.WriteString(") AS BEGIN\n")
	for _, st := range chunk {
		sb.WriteString("  " + st.sql + " INTO " + strings.Join(into, ", ") + ";\n  SUSPEND;\n")
	}
	sb.WriteString("END")
	return sb.String(), args
}

// namedBuilder returns a Builder whose parameters continue the numbering of
// the chunk being built.
func namedBuilder(first int) *query.Builder {
	return &query.Builder{Named: true, First: first}
}

// batchPreparer is what the driver's connection offers on Firebird 4+ (wire
// protocol 16): the server-side batch API.
type batchPreparer interface {
	PrepareBatch(ctx context.Context, sql string, opts firebirdsql.BatchOptions) (*firebirdsql.PreparedBatch, error)
}

// batchable reports whether rows can go through the batch API: Firebird 4+,
// a plain INSERT, no RETURNING values wanted, and every value encodable by
// the batch exactly (see batchValue). Anything else takes the EXECUTE BLOCK
// path, which also reports the proper error for a value that doesn't fit.
func batchable(rc *reqCtx, rel *schema.Relation, resolution string, rows []rowData) bool {
	if resolution != "" || rel.IsView || len(rows) < 2 || rc.prefer.ret == "representation" ||
		schema.MajorVersion(rc.sc.Version) < 4 || rc.tx.conn == nil || rc.plan != nil {
		return false
	}
	for _, d := range rows {
		if len(d.cols) == 0 {
			return false // INSERT ... DEFAULT VALUES has no parameters
		}
		for i, c := range d.cols {
			if _, ok := batchValue(c, d.vals[i]); !ok {
				return false
			}
		}
	}
	return true
}

// batchValue converts a coerced value into what the driver's batch encoder
// writes correctly for column c, or reports that the batch can't carry it:
// BLOBs, time zone types, INT128 and DECFLOAT, text longer than the column
// (the encoder would truncate it), and numerics with more decimals than the
// column's scale.
func batchValue(c *schema.Column, v any) (driver.Value, bool) {
	if v == nil {
		return nil, true
	}
	switch c.FieldType {
	case schema.TVarchar, schema.TChar:
		s, ok := v.(string)
		if !ok || c.IsBinary() {
			return nil, false
		}
		if c.CharLength > 0 && utf8.RuneCountInString(s) > c.CharLength {
			return nil, false
		}
		return s, true
	case schema.TBoolean:
		b, ok := v.(bool)
		return b, ok
	case schema.TDouble, schema.TFloat:
		switch x := v.(type) {
		case float64:
			return x, true
		case int64:
			return float64(x), true
		case string:
			f, err := strconv.ParseFloat(x, 64)
			return f, err == nil
		}
		return nil, false
	case schema.TSmallint, schema.TInteger, schema.TBigint:
		return scaledInt(v, c.Scale)
	case schema.TDate, schema.TTime, schema.TTimestamp:
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		for _, layout := range []string{"2006-01-02", "15:04:05.0000", "15:04:05", "2006-01-02 15:04:05.0000", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
		return nil, false
	}
	return nil, false
}

// scaledInt returns the integer the engine stores for a NUMERIC/DECIMAL
// value with the column's scale: 9.99 with scale -2 is 999. A value with
// more decimals than the scale is refused rather than rounded.
func scaledInt(v any, scale int) (driver.Value, bool) {
	var s string
	switch x := v.(type) {
	case int64:
		if scale == 0 {
			return x, true
		}
		s = strconv.FormatInt(x, 10)
	case float64:
		if scale == 0 && x == float64(int64(x)) {
			return int64(x), true
		}
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		s = strings.TrimSpace(x)
	default:
		return nil, false
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	decimals := -scale
	if len(frac) > decimals || strings.ContainsAny(s, "eE") {
		return nil, false
	}
	digits := whole + frac + strings.Repeat("0", decimals-len(frac))
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return nil, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// shapeKey groups rows that share a statement: the same columns in order.
func shapeKey(d rowData) string {
	names := make([]string, len(d.cols))
	for i, c := range d.cols {
		names[i] = c.DBName
	}
	return strings.Join(names, "\x00")
}

// insertBatch inserts rows with the batch API, one batch per column shape,
// and returns how many rows were inserted.
func insertBatch(ctx context.Context, rc *reqCtx, rel *schema.Relation, rows []rowData) (int64, error) {
	// keep the body's order within a shape; shapes in order of first appearance
	var order []string
	groups := map[string][]rowData{}
	for _, d := range rows {
		k := shapeKey(d)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], d)
	}
	var total int64
	for _, k := range order {
		group := groups[k]
		b := &query.Builder{}
		b.W("INSERT INTO ", rel.SQLName(), " (", quoteCols(group[0].cols), ") VALUES (", placeholders(len(group[0].cols)), ")")
		rc.tx.note(b.String())
		err := rc.tx.conn.Raw(func(dc any) error {
			bp, ok := dc.(batchPreparer)
			if !ok {
				return errNoBatch
			}
			batch, err := bp.PrepareBatch(ctx, b.String(), firebirdsql.BatchOptions{})
			if err != nil {
				return err
			}
			defer batch.Close()
			for _, d := range group {
				args := make([]driver.Value, len(d.vals))
				for i, v := range d.vals {
					args[i], _ = batchValue(d.cols[i], v) // checked by batchable
				}
				if err := batch.Add(args...); err != nil {
					return err
				}
			}
			res, err := batch.Exec(ctx)
			if err != nil {
				return err
			}
			total += res.Affected
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

var errNoBatch = errors.New("the driver connection has no batch API")
