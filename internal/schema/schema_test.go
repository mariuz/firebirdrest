package schema

import "testing"

func TestSQLNames(t *testing.T) {
	cases := []struct{ got, want string }{
		{(&Relation{DBName: "BOOKS"}).SQLName(), `"BOOKS"`},
		{(&Relation{DBName: "BOOKS", Schema: "INVENTORY"}).SQLName(), `"INVENTORY"."BOOKS"`},
		{(&Relation{DBName: `My "T"`, Schema: "S"}).SQLName(), `"S"."My ""T"""`},
		{(&Routine{DBName: "P"}).SQLName(), `"P"`},
		{(&Routine{DBName: "F", Package: "PKG"}).SQLName(), `"PKG"."F"`},
		{(&Routine{DBName: "F", Package: "PKG", Schema: "INVENTORY"}).SQLName(), `"INVENTORY"."PKG"."F"`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
	if r := (&Routine{DBName: "P", Schema: "S"}).AsRelation(); r.Schema != "S" {
		t.Error("AsRelation drops the schema")
	}
}

func TestCatalogLookup(t *testing.T) {
	c := &Catalog{Schemas: []*Schema{{Name: "public", DBName: "PUBLIC"}, {Name: "Mixed", DBName: "Mixed"}, {Name: "inventory", DBName: "INVENTORY"}}}
	for name, want := range map[string]string{
		"public": "PUBLIC", "PUBLIC": "PUBLIC", "inventory": "INVENTORY", "Inventory": "INVENTORY",
		"Mixed": "Mixed", "mixed": "", "nope": "", "": "",
	} {
		got := ""
		if s := c.Lookup(name); s != nil {
			got = s.DBName
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", name, got, want)
		}
	}
	if c.Default().DBName != "PUBLIC" {
		t.Error("the first schema is the default")
	}
}

func TestMajorVersion(t *testing.T) {
	for v, want := range map[string]int{"6.0.0": 6, "5.0.4": 5, "3.0.14": 3, "": 0, "x": 0} {
		if got := MajorVersion(v); got != want {
			t.Errorf("MajorVersion(%q) = %d", v, got)
		}
	}
}

func TestScope(t *testing.T) {
	legacy := scope{}
	if c, a := legacy.is("r.RDB$SCHEMA_NAME"); c != "" || a != nil || legacy.same("a", "b") != "" ||
		legacy.col("x") != "CAST(NULL AS VARCHAR(63))" {
		t.Error("before Firebird 6 the scope adds nothing")
	}
	fb6 := scope{"INVENTORY"}
	if c, a := fb6.is("r.RDB$SCHEMA_NAME"); c != " AND r.RDB$SCHEMA_NAME = ?" || len(a) != 1 || a[0] != "INVENTORY" {
		t.Errorf("is: %q %v", c, a)
	}
	if fb6.same("a", "b") != " AND a = b" || fb6.col("x") != "x" {
		t.Error("same/col")
	}
}
