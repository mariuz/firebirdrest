package server

import (
	"strconv"
	"strings"

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
