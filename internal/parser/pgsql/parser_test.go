package pgsql

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestParseCreateTable(t *testing.T) {
	input := `
CREATE TABLE public.users (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Symbols) == 0 {
		t.Fatal("expected at least 1 symbol")
	}

	table := result.Symbols[0]
	if table.Kind != "table" {
		t.Errorf("expected table, got %s", table.Kind)
	}
	if table.QualifiedName != "public.users" {
		t.Errorf("expected public.users, got %s", table.QualifiedName)
	}
	if len(table.Children) < 3 {
		t.Errorf("expected at least 3 columns, got %d", len(table.Children))
	}
}

func TestParseCreateView(t *testing.T) {
	input := `
CREATE VIEW active_users AS
SELECT u.id, u.email, u.name
FROM users u
WHERE u.is_active = true;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	var view *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "view" {
			view = &result.Symbols[i]
			break
		}
	}
	if view == nil {
		t.Fatal("expected view symbol")
	}
	if view.QualifiedName != "active_users" {
		t.Errorf("expected active_users, got %s", view.QualifiedName)
	}

	found := false
	for _, ref := range result.References {
		if ref.ToName == "u" || ref.ToName == "users" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected reference to users table")
	}
}

func TestParseCreateFunction(t *testing.T) {
	input := `
CREATE OR REPLACE FUNCTION public.get_user_orders(p_user_id UUID)
RETURNS TABLE(order_id UUID, total NUMERIC)
LANGUAGE SQL
AS $$
    SELECT o.id, o.total FROM orders o WHERE o.user_id = p_user_id;
$$;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	var fn *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "function" {
			fn = &result.Symbols[i]
			break
		}
	}
	if fn == nil {
		t.Fatal("expected function symbol")
	}
	if fn.QualifiedName != "public.get_user_orders" {
		t.Errorf("expected public.get_user_orders, got %s", fn.QualifiedName)
	}
}

func TestSymbolLineNumbers(t *testing.T) {
	// pg_query statement locations are byte offsets; symbols must report lines.
	input := `CREATE TABLE users (id int);

CREATE TABLE orders (
    id int,
    user_id int
);`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	lines := map[string]int{}
	colLines := map[string]int{}
	for _, s := range result.Symbols {
		lines[s.Name] = s.StartLine
		for _, c := range s.Children {
			colLines[s.Name+"."+c.Name] = c.StartLine
		}
	}

	if lines["users"] != 1 {
		t.Errorf("expected users at line 1, got %d", lines["users"])
	}
	if lines["orders"] != 3 {
		t.Errorf("expected orders at line 3, got %d", lines["orders"])
	}
	if colLines["orders.user_id"] != 5 {
		t.Errorf("expected orders.user_id at line 5, got %d", colLines["orders.user_id"])
	}
}

func TestParseCreateTrigger(t *testing.T) {
	input := `
CREATE TRIGGER trg_user_update
AFTER UPDATE ON users
FOR EACH ROW
EXECUTE FUNCTION update_timestamp();
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	var trigger *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "trigger" {
			trigger = &result.Symbols[i]
			break
		}
	}
	if trigger == nil {
		t.Fatal("expected trigger symbol")
	}

	// Should reference the users table
	foundTable := false
	foundFunc := false
	for _, ref := range result.References {
		if ref.ToName == "users" && ref.ReferenceType == "uses_table" {
			foundTable = true
		}
		if ref.ToName == "update_timestamp" && ref.ReferenceType == "calls" {
			foundFunc = true
		}
	}
	if !foundTable {
		t.Error("expected uses_table reference to users")
	}
	if !foundFunc {
		t.Error("expected calls reference to update_timestamp")
	}
}
