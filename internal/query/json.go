package query

import (
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// AppendJSON appends the JSON encoding of v to dst. It handles the value types
// produced by Output (and nested objects/arrays) without reflection and gives
// the same output as encoding/json with HTML escaping turned off.
func AppendJSON(dst []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if x {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int64:
		return strconv.AppendInt(dst, x, 10)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case int32:
		return strconv.AppendInt(dst, int64(x), 10)
	case int16:
		return strconv.AppendInt(dst, int64(x), 10)
	case float64:
		return appendFloat(dst, x)
	case json.Number:
		if x == "" {
			return append(dst, '0')
		}
		return append(dst, x...)
	case string:
		return appendString(dst, x)
	case *Obj:
		if x == nil {
			return append(dst, "null"...)
		}
		dst = append(dst, '{')
		for i, k := range x.Keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendString(dst, k)
			dst = append(dst, ':')
			dst = AppendJSON(dst, x.Vals[i])
		}
		return append(dst, '}')
	case []*Obj:
		dst = append(dst, '[')
		for i, o := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendJSON(dst, o)
		}
		return append(dst, ']')
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendJSON(dst, e)
		}
		return append(dst, ']')
	}
	b, err := json.Marshal(v)
	if err != nil {
		return appendString(dst, err.Error())
	}
	return append(dst, b...)
}

// appendFloat follows encoding/json's float formatting.
func appendFloat(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return appendString(dst, strconv.FormatFloat(f, 'g', -1, 64))
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		n := len(dst) - start
		if n >= 4 && dst[len(dst)-4] == 'e' && dst[len(dst)-3] == '-' && dst[len(dst)-2] == '0' {
			dst[len(dst)-2] = dst[len(dst)-1]
			dst = dst[:len(dst)-1]
		}
	}
	return dst
}

const hexDigits = "0123456789abcdef"

// appendString writes a JSON string like encoding/json without HTML escaping.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i += size
			start = i
			continue
		}
		if r == 0x2028 || r == 0x2029 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
