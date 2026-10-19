package server

import (
	"reflect"
	"testing"
)

func TestParsePrefer(t *testing.T) {
	p := parsePrefer([]string{"return=representation, count=exact", "handling=strict,max-affected=5", "tx=sometimes", "foo"})
	if p.ret != "representation" || p.count != "exact" || p.handling != "strict" || p.maxAffected != 5 {
		t.Errorf("parsed %+v", p)
	}
	if !reflect.DeepEqual(p.applied, []string{"return=representation", "count=exact", "handling=strict", "max-affected=5"}) {
		t.Errorf("applied %q", p.applied)
	}
	if !reflect.DeepEqual(p.invalid, []string{"tx=sometimes", "foo"}) {
		t.Errorf("invalid %q", p.invalid)
	}
	if err := p.check(); err == nil {
		t.Error("strict: invalid preferences should be refused")
	}

	// lenient (the default) ignores invalid preferences and max-affected
	p = parsePrefer([]string{"max-affected=1, return=nope"})
	if err := p.check(); err != nil || p.maxAffected != 0 || p.ret != "" {
		t.Errorf("lenient: %v %+v", err, p)
	}
	if err := p.checkAffected(100); err != nil {
		t.Errorf("lenient max-affected enforced: %v", err)
	}

	p = parsePrefer([]string{"handling=strict, max-affected=2"})
	if err := p.check(); err != nil {
		t.Fatal(err)
	}
	if p.checkAffected(2) != nil || p.checkAffected(3) == nil {
		t.Error("max-affected=2 should allow 2 rows and refuse 3")
	}
	for _, bad := range []string{"max-affected=-1", "max-affected=x", "max-affected="} {
		if p := parsePrefer([]string{"handling=strict, " + bad}); p.check() == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
