package schema

import (
	"context"
	"database/sql"
	"strings"
)

// ResponseSetters records which database objects may set the response.status
// and response.headers context variables: those whose source mentions
// "response.", and, through RDB$DEPENDENCIES, every object that uses one of
// them (a procedure calling a setter, a view or trigger using it). Reading
// the variables back costs a round trip, so only requests that reach such
// an object pay it.
type ResponseSetters struct {
	// Always is set when every request must read them: a database trigger
	// or a column expression mentions them, or some source is not readable.
	Always bool
	names  map[string]bool // DB names of setters and their dependents
}

// Has reports whether the object with this DB name may set the variables.
func (r *ResponseSetters) Has(dbName string) bool {
	return r != nil && (r.Always || r.names[dbName])
}

// responseSetters finds the setters. On any error it answers Always, which
// is slower but never misses a status or header.
func responseSetters(ctx context.Context, db *sql.DB) *ResponseSetters {
	always := &ResponseSetters{Always: true}
	r := &ResponseSetters{names: map[string]bool{}}
	rows, err := db.QueryContext(ctx, `
SELECT x.typ, x.name, x.rel, IIF(x.src IS NULL, 1, 0) FROM (
  SELECT 'P' AS typ, CAST(TRIM(RDB$PROCEDURE_NAME) AS VARCHAR(63)) AS name, CAST(NULL AS VARCHAR(63)) AS rel,
         RDB$PROCEDURE_SOURCE AS src FROM RDB$PROCEDURES
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 AND RDB$PACKAGE_NAME IS NULL
  UNION ALL
  SELECT 'F', TRIM(RDB$FUNCTION_NAME), NULL, RDB$FUNCTION_SOURCE FROM RDB$FUNCTIONS
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 AND RDB$PACKAGE_NAME IS NULL AND RDB$MODULE_NAME IS NULL
  UNION ALL
  SELECT 'T', TRIM(RDB$TRIGGER_NAME), TRIM(RDB$RELATION_NAME), RDB$TRIGGER_SOURCE FROM RDB$TRIGGERS
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  UNION ALL
  SELECT 'V', TRIM(RDB$RELATION_NAME), NULL, RDB$VIEW_SOURCE FROM RDB$RELATIONS
   WHERE RDB$VIEW_BLR IS NOT NULL AND COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  UNION ALL
  SELECT 'K', TRIM(RDB$PACKAGE_NAME), NULL, RDB$PACKAGE_BODY_SOURCE FROM RDB$PACKAGES
   WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
) x
WHERE x.src IS NULL OR x.src CONTAINING 'response.'`)
	if err != nil {
		return always
	}
	var direct []string
	for rows.Next() {
		var typ, name string
		var rel sql.NullString
		var noSource int
		if err := rows.Scan(&typ, &name, &rel, &noSource); err != nil {
			rows.Close()
			return always
		}
		if noSource == 1 || typ == "T" && !rel.Valid {
			r.Always = true // stripped source, or a database trigger
		}
		direct = append(direct, strings.TrimSpace(name))
	}
	rows.Close()
	if rows.Err() != nil {
		return always
	}
	// column expressions: computed, default and check sources
	var n int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM RDB$FIELDS
WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
  AND (RDB$COMPUTED_SOURCE CONTAINING 'response.' OR RDB$DEFAULT_SOURCE CONTAINING 'response.'
       OR RDB$VALIDATION_SOURCE CONTAINING 'response.')`).Scan(&n); err != nil || n > 0 {
		return always
	}
	if r.Always {
		return r
	}
	// who uses whom
	users := map[string][]string{}
	if rows, err = db.QueryContext(ctx, `
SELECT DISTINCT TRIM(RDB$DEPENDENT_NAME), TRIM(RDB$DEPENDED_ON_NAME) FROM RDB$DEPENDENCIES
WHERE RDB$DEPENDENT_NAME NOT STARTING WITH 'RDB$'`); err != nil {
		return always
	}
	for rows.Next() {
		var user, used string
		if err := rows.Scan(&user, &used); err != nil {
			rows.Close()
			return always
		}
		users[used] = append(users[used], user)
	}
	rows.Close()
	if rows.Err() != nil {
		return always
	}
	queue := direct
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if r.names[name] {
			continue
		}
		r.names[name] = true
		queue = append(queue, users[name]...)
	}
	// a trigger that may set them makes its table a setter; a database
	// trigger makes every request one
	if rows, err = db.QueryContext(ctx, `
SELECT TRIM(RDB$TRIGGER_NAME), TRIM(RDB$RELATION_NAME) FROM RDB$TRIGGERS
WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0`); err != nil {
		return always
	}
	for rows.Next() {
		var trig string
		var rel sql.NullString
		if err := rows.Scan(&trig, &rel); err != nil {
			rows.Close()
			return always
		}
		if r.names[trig] {
			if !rel.Valid {
				r.Always = true
			} else {
				r.names[strings.TrimSpace(rel.String)] = true
			}
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return always
	}
	return r
}

// markResponse sets SetsResponse on the relations and routines a request
// may reach a setter through: a relation that is one (a view using one, a
// table with such a trigger), has a computed field or relationship that is
// one, or is directly related to one (it may be embedded); a routine that is
// one, or belongs to a package that is.
func (c *Catalog) markResponse(r *ResponseSetters) {
	for _, s := range c.Schemas {
		s.Response = r
		if r == nil {
			continue
		}
		for _, rt := range s.Routines {
			rt.SetsResponse = r.Has(rt.DBName) || rt.Package != "" && r.Has(rt.Package)
		}
		for _, rel := range s.Relations {
			set := r.Has(rel.DBName)
			for _, rt := range rel.Computed {
				set = set || rt.SetsResponse
			}
			rel.SetsResponse = set
		}
		for _, fk := range s.ForeignKeys {
			if fk.From != nil && fk.To != nil && (r.Has(fk.From.DBName) || r.Has(fk.To.DBName)) {
				fk.From.SetsResponse = true
				fk.To.SetsResponse = true
			}
		}
	}
}
