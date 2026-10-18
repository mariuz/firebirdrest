package schema

import "fmt"

// Firebird RDB$FIELD_TYPE codes.
const (
	TSmallint    = 7
	TInteger     = 8
	TFloat       = 10
	TDate        = 12
	TTime        = 13
	TChar        = 14
	TBigint      = 16
	TBoolean     = 23
	TDecfloat16  = 24
	TDecfloat34  = 25
	TInt128      = 26
	TDouble      = 27
	TTimeTZ      = 28
	TTimestampTZ = 29
	TTimestamp   = 35
	TVarchar     = 37
	TCString     = 40
	TBlob        = 261
)

// IsExactNumeric is true for NUMERIC/DECIMAL columns stored with a scale.
func (c *Column) IsExactNumeric() bool {
	switch c.FieldType {
	case TSmallint, TInteger, TBigint, TInt128:
		return c.Scale < 0 || c.SubType == 1 || c.SubType == 2
	}
	return false
}

// IsInteger is true for integer columns without a scale.
func (c *Column) IsInteger() bool {
	switch c.FieldType {
	case TSmallint, TInteger, TBigint, TInt128:
		return !c.IsExactNumeric()
	}
	return false
}

// IsBinary is true for BLOB SUB_TYPE BINARY and CHAR/VARCHAR CHARACTER SET OCTETS.
func (c *Column) IsBinary() bool {
	return c.FieldType == TBlob && c.SubType != 1
}

// SQLType returns a readable Firebird type name.
func (c *Column) SQLType() string {
	if c.IsExactNumeric() {
		kind := "NUMERIC"
		if c.SubType == 2 {
			kind = "DECIMAL"
		}
		return fmt.Sprintf("%s(%d,%d)", kind, c.Precision, -c.Scale)
	}
	switch c.FieldType {
	case TSmallint:
		return "SMALLINT"
	case TInteger:
		return "INTEGER"
	case TBigint:
		return "BIGINT"
	case TInt128:
		return "INT128"
	case TFloat:
		return "FLOAT"
	case TDouble:
		return "DOUBLE PRECISION"
	case TDecfloat16:
		return "DECFLOAT(16)"
	case TDecfloat34:
		return "DECFLOAT(34)"
	case TDate:
		return "DATE"
	case TTime:
		return "TIME"
	case TTimeTZ:
		return "TIME WITH TIME ZONE"
	case TTimestamp:
		return "TIMESTAMP"
	case TTimestampTZ:
		return "TIMESTAMP WITH TIME ZONE"
	case TBoolean:
		return "BOOLEAN"
	case TChar:
		return fmt.Sprintf("CHAR(%d)", c.charLen())
	case TVarchar, TCString:
		return fmt.Sprintf("VARCHAR(%d)", c.charLen())
	case TBlob:
		if c.SubType == 1 {
			return "BLOB SUB_TYPE TEXT"
		}
		return "BLOB SUB_TYPE BINARY"
	}
	return fmt.Sprintf("TYPE_%d", c.FieldType)
}

func (c *Column) charLen() int {
	if c.CharLength > 0 {
		return c.CharLength
	}
	return c.Length
}

// OpenAPIType returns the OpenAPI type and format for the column.
func (c *Column) OpenAPIType() (typ, format string) {
	if c.IsExactNumeric() {
		return "number", "decimal"
	}
	switch c.FieldType {
	case TSmallint, TInteger:
		return "integer", "int32"
	case TBigint:
		return "integer", "int64"
	case TInt128:
		return "integer", ""
	case TFloat:
		return "number", "float"
	case TDouble, TDecfloat16, TDecfloat34:
		return "number", "double"
	case TDate:
		return "string", "date"
	case TTime, TTimeTZ:
		return "string", "time"
	case TTimestamp, TTimestampTZ:
		return "string", "date-time"
	case TBoolean:
		return "boolean", ""
	case TBlob:
		if c.SubType != 1 {
			return "string", "byte"
		}
	}
	return "string", ""
}
