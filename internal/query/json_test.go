package query

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func stdJSON(t *testing.T, v any) string {
	t.Helper()
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func TestAppendJSONMatchesEncodingJSON(t *testing.T) {
	values := []any{
		nil, true, false, int64(0), int64(-42), int64(math.MaxInt64), 7,
		0.0, 1.5, -2.25, 1e-7, 123456789.125, 1e21, 3e-300, math.MaxFloat64,
		json.Number("12.50"), "", "plain", `quo"te\back`, "tab\tnl\ncr\r", "\x00\x1f\x7f",
		"<html>&amp;", "üñíçødé ✓ 🐦", "bad\xffutf8", "line\u2028sep\u2029",
		[]any{int64(1), "two", nil, []any{true}},
	}
	for _, v := range values {
		if got, want := string(AppendJSON(nil, v)), stdJSON(t, v); got != want {
			t.Errorf("%#v:\n got %s\nwant %s", v, got, want)
		}
	}
	o := &Obj{Keys: []string{"b", "a", "nested"}, Vals: []any{int64(1), "x",
		[]*Obj{{Keys: []string{"k"}, Vals: []any{nil}}}}}
	if got := string(AppendJSON(nil, o)); got != `{"b":1,"a":"x","nested":[{"k":null}]}` {
		t.Errorf("object: %s", got)
	}
	if got := string(AppendJSON(nil, []*Obj{})); got != `[]` {
		t.Errorf("empty: %s", got)
	}
}

func BenchmarkAppendJSON(b *testing.B) {
	rows := make([]*Obj, 100)
	for i := range rows {
		rows[i] = &Obj{Keys: []string{"id", "title", "price", "in_stock", "authors"},
			Vals: []any{int64(i), "Some book title", json.Number("12.50"), true,
				&Obj{Keys: []string{"name"}, Vals: []any{"Author"}}}}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		AppendJSON(make([]byte, 0, 256*len(rows)), rows)
	}
}
