// Package schema reads the Firebird system tables (RDB$...) and builds an
// in-memory cache of the tables, views, keys and routines that the API exposes.
package schema

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Column describes one column of a table or view, or one parameter of a routine.
type Column struct {
	Name       string // name used in the API (lower case for unquoted identifiers)
	DBName     string // name as stored in the database
	Position   int
	FieldType  int
	SubType    int
	Scale      int
	Length     int
	CharLength int
	Precision  int
	Nullable   bool
	HasDefault bool
	Identity   bool
	Computed   bool
	// for routine parameters declared TYPE OF COLUMN rel.col
	SourceRel string
	SourceCol string
}

// Relation is a table or a view.
type Relation struct {
	Name        string
	DBName      string
	IsView      bool
	Description string
	Columns     []*Column
	PrimaryKey  []string // DB column names
	Uniques     [][]string
	cols        map[string]*Column
	// Computed lists routines whose inputs are all TYPE OF COLUMN this relation:
	// functions are computed fields, procedures are computed relationships.
	Computed []*Routine
}

// ComputedField finds a function usable as a computed column of r.
func (r *Relation) ComputedField(name string) *Routine {
	for _, rt := range r.Computed {
		if rt.Kind == Function && (rt.Name == name || rt.DBName == name) {
			return rt
		}
	}
	return nil
}

// ForeignKey links columns of From to the referenced columns of To.
type ForeignKey struct {
	Name     string // constraint name (API form)
	DBName   string
	From     *Relation
	FromCols []string // DB names
	To       *Relation
	ToCols   []string // DB names
}

// Routine kinds.
const (
	Selectable = "selectable" // stored procedure with SUSPEND, used in FROM
	Executable = "executable" // stored procedure run with EXECUTE PROCEDURE
	Function   = "function"   // PSQL function (Firebird 3+)
)

// Routine is a stored procedure or a PSQL function.
type Routine struct {
	Name        string // "proc", or "pkg.proc" for packaged routines
	DBName      string
	Package     string // DB name of the package, "" when standalone
	Kind        string
	Description string
	In          []*Column
	Out         []*Column // output parameters (procedures) or the return value (functions)
	// Parent is set when every input parameter is TYPE OF COLUMN of one relation;
	// ParentCols are those columns in parameter order.
	Parent     *Relation
	ParentCols []string
}

// Schema is the cached view of the database.
type Schema struct {
	Relations   map[string]*Relation // keyed by API name
	ForeignKeys []*ForeignKey
	Routines    map[string]*Routine // keyed by API name
	Version     string
	// UsesRequestContext is true when some view, trigger, routine, package or
	// computed/default expression mentions "request." (or its source is not
	// readable), so the request.* context variables must be set.
	UsesRequestContext bool
}

var plainIdent = regexp.MustCompile(`^[A-Z][A-Z0-9_$]*$`)

// APIName converts a database identifier to the name shown in the API:
// unquoted (upper case) identifiers become lower case, quoted ones stay as-is.
func APIName(dbName string) string {
	if plainIdent.MatchString(dbName) {
		return strings.ToLower(dbName)
	}
	return dbName
}

// Quote returns a safely quoted Firebird identifier.
func Quote(dbName string) string {
	return `"` + strings.ReplaceAll(dbName, `"`, `""`) + `"`
}

// Column finds a column by API or database name.
func (r *Relation) Column(name string) *Column {
	if c, ok := r.cols[name]; ok {
		return c
	}
	if c, ok := r.cols[strings.ToLower(name)]; ok && c.DBName == strings.ToUpper(name) {
		return c
	}
	for _, c := range r.Columns {
		if c.DBName == name {
			return c
		}
	}
	return nil
}

// Index builds the column lookup map; call after Columns is filled.
func (r *Relation) Index() {
	r.cols = make(map[string]*Column, len(r.Columns))
	for _, c := range r.Columns {
		r.cols[c.Name] = c
	}
}

// Relation finds a table or view by API or database name.
func (s *Schema) Relation(name string) *Relation {
	if r, ok := s.Relations[name]; ok {
		return r
	}
	for _, r := range s.Relations {
		if r.DBName == name || r.DBName == strings.ToUpper(name) && r.Name == strings.ToLower(name) {
			return r
		}
	}
	return nil
}

// Routine finds a routine by API or database name.
func (s *Schema) Routine(name string) *Routine {
	if r, ok := s.Routines[name]; ok {
		return r
	}
	for _, r := range s.Routines {
		full := r.DBName
		if r.Package != "" {
			full = r.Package + "." + r.DBName
		}
		if full == name {
			return r
		}
	}
	return nil
}

// RelationNames returns the API names sorted alphabetically.
func (s *Schema) RelationNames() []string {
	out := make([]string, 0, len(s.Relations))
	for n := range s.Relations {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RoutineNames returns the routine API names sorted alphabetically.
func (s *Schema) RoutineNames() []string {
	out := make([]string, 0, len(s.Routines))
	for n := range s.Routines {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SQLName is the quoted name used to call the routine.
func (r *Routine) SQLName() string {
	if r.Package != "" {
		return Quote(r.Package) + "." + Quote(r.DBName)
	}
	return Quote(r.DBName)
}

func routineKey(pkg, name string) string { return pkg + "\x00" + name }

func routineName(pkg, name string) string {
	if pkg == "" {
		return APIName(name)
	}
	return APIName(pkg) + "." + APIName(name)
}

// AsRelation exposes the outputs of a selectable procedure as a relation so
// the read machinery (select, filters, order, paging) can be reused.
func (r *Routine) AsRelation() *Relation {
	rel := &Relation{Name: r.Name, DBName: r.DBName, IsView: true, Columns: r.Out}
	rel.Index()
	return rel
}

// Arg finds an input parameter by API or DB name.
func (r *Routine) Arg(name string) *Column {
	for _, p := range r.In {
		if p.Name == name || p.DBName == name {
			return p
		}
	}
	return nil
}

func trim(s sql.NullString) string { return strings.TrimSpace(s.String) }

func blobText(v any) string {
	switch b := v.(type) {
	case []byte:
		return string(b)
	case string:
		return b
	}
	return ""
}

// Load reads the schema from the database.
func Load(ctx context.Context, db *sql.DB) (*Schema, error) {
	s := &Schema{Relations: map[string]*Relation{}, Routines: map[string]*Routine{}}

	_ = db.QueryRowContext(ctx,
		`SELECT RDB$GET_CONTEXT('SYSTEM','ENGINE_VERSION') FROM RDB$DATABASE`).Scan(&s.Version)

	byDB := map[string]*Relation{}

	rows, err := db.QueryContext(ctx, `
SELECT r.RDB$RELATION_NAME, r.RDB$VIEW_BLR IS NOT NULL, r.RDB$DESCRIPTION
FROM RDB$RELATIONS r
WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
  AND COALESCE(r.RDB$RELATION_TYPE, 0) IN (0, 1, 4, 5)`)
	if err != nil {
		return nil, fmt.Errorf("reading relations: %w", err)
	}
	for rows.Next() {
		var name sql.NullString
		var isView bool
		var desc any
		if err := rows.Scan(&name, &isView, &desc); err != nil {
			rows.Close()
			return nil, err
		}
		rel := &Relation{DBName: trim(name), IsView: isView, Description: blobText(desc)}
		rel.Name = APIName(rel.DBName)
		byDB[rel.DBName] = rel
		s.Relations[rel.Name] = rel
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
SELECT rf.RDB$RELATION_NAME, rf.RDB$FIELD_NAME, rf.RDB$FIELD_POSITION,
       f.RDB$FIELD_TYPE, COALESCE(f.RDB$FIELD_SUB_TYPE, 0), COALESCE(f.RDB$FIELD_SCALE, 0),
       COALESCE(f.RDB$FIELD_LENGTH, 0), COALESCE(f.RDB$CHARACTER_LENGTH, 0),
       COALESCE(f.RDB$FIELD_PRECISION, 0),
       COALESCE(rf.RDB$NULL_FLAG, f.RDB$NULL_FLAG, 0),
       rf.RDB$DEFAULT_SOURCE IS NOT NULL OR f.RDB$DEFAULT_SOURCE IS NOT NULL,
       rf.RDB$IDENTITY_TYPE IS NOT NULL,
       f.RDB$COMPUTED_BLR IS NOT NULL
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = rf.RDB$RELATION_NAME
WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY rf.RDB$RELATION_NAME, rf.RDB$FIELD_POSITION`)
	if err != nil {
		return nil, fmt.Errorf("reading columns: %w", err)
	}
	for rows.Next() {
		var rel, col sql.NullString
		var pos sql.NullInt64
		var notNull int
		c := &Column{}
		if err := rows.Scan(&rel, &col, &pos, &c.FieldType, &c.SubType, &c.Scale, &c.Length,
			&c.CharLength, &c.Precision, &notNull, &c.HasDefault, &c.Identity, &c.Computed); err != nil {
			rows.Close()
			return nil, err
		}
		r := byDB[trim(rel)]
		if r == nil {
			continue
		}
		c.DBName = trim(col)
		c.Name = APIName(c.DBName)
		c.Position = int(pos.Int64)
		c.Nullable = notNull == 0
		r.Columns = append(r.Columns, c)
	}
	rows.Close()
	for _, r := range s.Relations {
		r.Index()
	}

	if err := loadConstraints(ctx, db, s, byDB); err != nil {
		return nil, err
	}
	if err := loadProcedures(ctx, db, s); err != nil {
		return nil, err
	}
	if err := loadFunctions(ctx, db, s); err != nil {
		return nil, err
	}
	linkComputed(s, byDB)
	s.UsesRequestContext = usesRequestContext(ctx, db)
	return s, nil
}

func loadConstraints(ctx context.Context, db *sql.DB, s *Schema, byDB map[string]*Relation) error {
	rows, err := db.QueryContext(ctx, `
SELECT rc.RDB$RELATION_NAME, rc.RDB$CONSTRAINT_NAME, rc.RDB$CONSTRAINT_TYPE,
       seg.RDB$FIELD_NAME, refc.RDB$CONST_NAME_UQ
FROM RDB$RELATION_CONSTRAINTS rc
JOIN RDB$INDEX_SEGMENTS seg ON seg.RDB$INDEX_NAME = rc.RDB$INDEX_NAME
LEFT JOIN RDB$REF_CONSTRAINTS refc ON refc.RDB$CONSTRAINT_NAME = rc.RDB$CONSTRAINT_NAME
WHERE rc.RDB$CONSTRAINT_TYPE IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
ORDER BY rc.RDB$CONSTRAINT_NAME, seg.RDB$FIELD_POSITION`)
	if err != nil {
		return fmt.Errorf("reading constraints: %w", err)
	}
	type cons struct {
		rel, name, typ, refName string
		cols                    []string
	}
	var order []string
	all := map[string]*cons{}
	for rows.Next() {
		var rel, name, typ, col, ref sql.NullString
		if err := rows.Scan(&rel, &name, &typ, &col, &ref); err != nil {
			rows.Close()
			return err
		}
		n := trim(name)
		c := all[n]
		if c == nil {
			c = &cons{rel: trim(rel), name: n, typ: trim(typ), refName: trim(ref)}
			all[n] = c
			order = append(order, n)
		}
		c.cols = append(c.cols, trim(col))
	}
	rows.Close()

	for _, n := range order {
		c := all[n]
		r := byDB[c.rel]
		if r == nil {
			continue
		}
		switch c.typ {
		case "PRIMARY KEY":
			r.PrimaryKey = c.cols
		case "UNIQUE":
			r.Uniques = append(r.Uniques, c.cols)
		case "FOREIGN KEY":
			ref := all[c.refName]
			if ref == nil || byDB[ref.rel] == nil {
				continue
			}
			s.ForeignKeys = append(s.ForeignKeys, &ForeignKey{
				Name: APIName(c.name), DBName: c.name,
				From: r, FromCols: c.cols, To: byDB[ref.rel], ToCols: ref.cols,
			})
		}
	}
	return nil
}

func loadProcedures(ctx context.Context, db *sql.DB, s *Schema) error {
	rows, err := db.QueryContext(ctx, `
SELECT p.RDB$PACKAGE_NAME, p.RDB$PROCEDURE_NAME, COALESCE(p.RDB$PROCEDURE_TYPE, 2), p.RDB$DESCRIPTION
FROM RDB$PROCEDURES p
WHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0 AND COALESCE(p.RDB$PRIVATE_FLAG, 0) = 0`)
	if err != nil {
		return fmt.Errorf("reading procedures: %w", err)
	}
	byDB := map[string]*Routine{}
	for rows.Next() {
		var pkg, name sql.NullString
		var typ int
		var desc any
		if err := rows.Scan(&pkg, &name, &typ, &desc); err != nil {
			rows.Close()
			return err
		}
		r := &Routine{DBName: trim(name), Package: trim(pkg), Kind: Executable, Description: blobText(desc)}
		if typ == 1 {
			r.Kind = Selectable
		}
		r.Name = routineName(r.Package, r.DBName)
		byDB[routineKey(r.Package, r.DBName)] = r
		s.Routines[r.Name] = r
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
SELECT pp.RDB$PACKAGE_NAME, pp.RDB$PROCEDURE_NAME, pp.RDB$PARAMETER_NAME, pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER,
       f.RDB$FIELD_TYPE, COALESCE(f.RDB$FIELD_SUB_TYPE, 0), COALESCE(f.RDB$FIELD_SCALE, 0),
       COALESCE(f.RDB$FIELD_LENGTH, 0), COALESCE(f.RDB$CHARACTER_LENGTH, 0),
       COALESCE(f.RDB$FIELD_PRECISION, 0),
       pp.RDB$DEFAULT_SOURCE IS NOT NULL OR f.RDB$DEFAULT_SOURCE IS NOT NULL,
       pp.RDB$RELATION_NAME, pp.RDB$FIELD_NAME
FROM RDB$PROCEDURE_PARAMETERS pp
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = pp.RDB$FIELD_SOURCE
ORDER BY pp.RDB$PACKAGE_NAME, pp.RDB$PROCEDURE_NAME, pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER`)
	if err != nil {
		return fmt.Errorf("reading procedure parameters: %w", err)
	}
	for rows.Next() {
		var pkg, proc, name, srcRel, srcCol sql.NullString
		var ptype, num int
		c := &Column{Nullable: true}
		if err := rows.Scan(&pkg, &proc, &name, &ptype, &num, &c.FieldType, &c.SubType, &c.Scale,
			&c.Length, &c.CharLength, &c.Precision, &c.HasDefault, &srcRel, &srcCol); err != nil {
			rows.Close()
			return err
		}
		r := byDB[routineKey(trim(pkg), trim(proc))]
		if r == nil {
			continue
		}
		c.DBName = trim(name)
		c.Name = APIName(c.DBName)
		c.Position = num
		c.SourceRel, c.SourceCol = trim(srcRel), trim(srcCol)
		if ptype == 0 {
			r.In = append(r.In, c)
		} else {
			r.Out = append(r.Out, c)
		}
	}
	rows.Close()
	return nil
}

func loadFunctions(ctx context.Context, db *sql.DB, s *Schema) error {
	rows, err := db.QueryContext(ctx, `
SELECT fn.RDB$PACKAGE_NAME, fn.RDB$FUNCTION_NAME, COALESCE(fn.RDB$RETURN_ARGUMENT, 0), fn.RDB$DESCRIPTION
FROM RDB$FUNCTIONS fn
WHERE COALESCE(fn.RDB$SYSTEM_FLAG, 0) = 0 AND COALESCE(fn.RDB$PRIVATE_FLAG, 0) = 0
  AND fn.RDB$MODULE_NAME IS NULL`)
	if err != nil {
		// Firebird 2.5 has no PSQL functions; ignore.
		return nil
	}
	byDB := map[string]*Routine{}
	retPos := map[string]int{}
	for rows.Next() {
		var pkg, name sql.NullString
		var ret int
		var desc any
		if err := rows.Scan(&pkg, &name, &ret, &desc); err != nil {
			rows.Close()
			return err
		}
		r := &Routine{DBName: trim(name), Package: trim(pkg), Kind: Function, Description: blobText(desc)}
		r.Name = routineName(r.Package, r.DBName)
		if _, clash := s.Routines[r.Name]; clash {
			continue
		}
		k := routineKey(r.Package, r.DBName)
		byDB[k] = r
		retPos[k] = ret
		s.Routines[r.Name] = r
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
SELECT a.RDB$PACKAGE_NAME, a.RDB$FUNCTION_NAME, a.RDB$ARGUMENT_NAME, a.RDB$ARGUMENT_POSITION,
       COALESCE(f.RDB$FIELD_TYPE, a.RDB$FIELD_TYPE), COALESCE(f.RDB$FIELD_SUB_TYPE, a.RDB$FIELD_SUB_TYPE, 0),
       COALESCE(f.RDB$FIELD_SCALE, a.RDB$FIELD_SCALE, 0),
       COALESCE(f.RDB$FIELD_LENGTH, a.RDB$FIELD_LENGTH, 0),
       COALESCE(f.RDB$CHARACTER_LENGTH, a.RDB$CHARACTER_LENGTH, 0),
       COALESCE(f.RDB$FIELD_PRECISION, a.RDB$FIELD_PRECISION, 0),
       a.RDB$DEFAULT_SOURCE IS NOT NULL,
       a.RDB$RELATION_NAME, a.RDB$FIELD_NAME
FROM RDB$FUNCTION_ARGUMENTS a
LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = a.RDB$FIELD_SOURCE
ORDER BY a.RDB$PACKAGE_NAME, a.RDB$FUNCTION_NAME, a.RDB$ARGUMENT_POSITION`)
	if err != nil {
		return fmt.Errorf("reading function arguments: %w", err)
	}
	for rows.Next() {
		var pkg, fn, name, srcRel, srcCol sql.NullString
		var pos int
		var ft sql.NullInt64
		c := &Column{Nullable: true}
		if err := rows.Scan(&pkg, &fn, &name, &pos, &ft, &c.SubType, &c.Scale, &c.Length,
			&c.CharLength, &c.Precision, &c.HasDefault, &srcRel, &srcCol); err != nil {
			rows.Close()
			return err
		}
		k := routineKey(trim(pkg), trim(fn))
		r := byDB[k]
		if r == nil {
			continue
		}
		c.FieldType = int(ft.Int64)
		c.DBName = trim(name)
		c.Position = pos
		c.SourceRel, c.SourceCol = trim(srcRel), trim(srcCol)
		if pos == retPos[k] {
			c.Name, c.DBName = APIName(r.DBName), "RESULT"
			r.Out = []*Column{c}
		} else {
			c.Name = APIName(c.DBName)
			r.In = append(r.In, c)
		}
	}
	rows.Close()
	return nil
}

// linkComputed attaches routines whose input parameters are all declared
// TYPE OF COLUMN of the same relation to that relation.
func linkComputed(s *Schema, byDB map[string]*Relation) {
	for _, name := range s.RoutineNames() {
		rt := s.Routines[name]
		if len(rt.In) == 0 || len(rt.Out) == 0 {
			continue
		}
		parent := byDB[rt.In[0].SourceRel]
		if parent == nil {
			continue
		}
		var cols []string
		for _, p := range rt.In {
			if p.SourceRel != parent.DBName || parent.Column(p.SourceCol) == nil {
				cols = nil
				break
			}
			cols = append(cols, p.SourceCol)
		}
		if cols == nil {
			continue
		}
		rt.Parent, rt.ParentCols = parent, cols
		parent.Computed = append(parent.Computed, rt)
	}
}

// usesRequestContext looks for "request." in every piece of PSQL and every
// expression stored in the database. Missing source (stripped metadata) counts
// as a use, so the variables are only skipped when that is provably safe.
func usesRequestContext(ctx context.Context, db *sql.DB) bool {
	var n int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
  SELECT RDB$PROCEDURE_SOURCE AS src FROM RDB$PROCEDURES
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 AND RDB$PACKAGE_NAME IS NULL
  UNION ALL
  SELECT RDB$FUNCTION_SOURCE FROM RDB$FUNCTIONS
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 AND RDB$PACKAGE_NAME IS NULL AND RDB$MODULE_NAME IS NULL
  UNION ALL
  SELECT RDB$TRIGGER_SOURCE FROM RDB$TRIGGERS WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  UNION ALL
  SELECT RDB$VIEW_SOURCE FROM RDB$RELATIONS
   WHERE RDB$VIEW_BLR IS NOT NULL AND COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  UNION ALL
  SELECT RDB$PACKAGE_BODY_SOURCE FROM RDB$PACKAGES WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
) x
WHERE x.src IS NULL OR x.src CONTAINING 'request.'`).Scan(&n)
	if err != nil || n > 0 {
		return true
	}
	err = db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM RDB$FIELDS
WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  AND (RDB$COMPUTED_SOURCE CONTAINING 'request.' OR RDB$DEFAULT_SOURCE CONTAINING 'request.'
       OR RDB$VALIDATION_SOURCE CONTAINING 'request.')`).Scan(&n)
	if err != nil || n > 0 {
		return true
	}
	err = db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM RDB$RELATION_FIELDS
WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 AND RDB$DEFAULT_SOURCE CONTAINING 'request.'`).Scan(&n)
	return err != nil || n > 0
}
