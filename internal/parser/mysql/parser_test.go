package mysql

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestParseCreateTable(t *testing.T) {
	input := `
CREATE TABLE users (
    id INT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(50) NOT NULL,
    email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
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
	if table.QualifiedName != "users" {
		t.Errorf("expected users, got %s", table.QualifiedName)
	}
	if len(table.Children) < 3 {
		t.Errorf("expected at least 3 columns, got %d: %+v", len(table.Children), table.Children)
	}
}

func TestParseCreateTableBackticks(t *testing.T) {
	input := "CREATE TABLE `my_db`.`orders` (\n    `order_id` INT PRIMARY KEY,\n    `total` DECIMAL(10,2)\n);\n"
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Symbols) == 0 {
		t.Fatal("expected table symbol")
	}
	if result.Symbols[0].QualifiedName != "my_db.orders" {
		t.Errorf("expected my_db.orders, got %s", result.Symbols[0].QualifiedName)
	}
	if result.Symbols[0].Name != "orders" {
		t.Errorf("expected name=orders, got %s", result.Symbols[0].Name)
	}
}

func TestParseCreateProcedure(t *testing.T) {
	input := `
DELIMITER $$

CREATE PROCEDURE GetUserOrders(IN p_user_id INT)
BEGIN
    SELECT o.order_id, o.total
    FROM orders o
    WHERE o.user_id = p_user_id;

    INSERT INTO audit_log (action, user_id)
    VALUES ('GetOrders', p_user_id);

    CALL update_last_access(p_user_id);
END$$

DELIMITER ;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
	if err != nil {
		t.Fatal(err)
	}

	var proc *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "procedure" {
			proc = &result.Symbols[i]
			break
		}
	}
	if proc == nil {
		t.Fatal("expected procedure symbol")
	}
	if proc.QualifiedName != "GetUserOrders" {
		t.Errorf("expected GetUserOrders, got %s", proc.QualifiedName)
	}

	// Check references
	refTypes := make(map[string][]string)
	for _, ref := range result.References {
		refTypes[ref.ReferenceType] = append(refTypes[ref.ReferenceType], ref.ToName)
	}

	if !contains(refTypes["reads_from"], "orders") {
		t.Errorf("expected reads_from orders, got %v", refTypes["reads_from"])
	}
	if !contains(refTypes["writes_to"], "audit_log") {
		t.Errorf("expected writes_to audit_log, got %v", refTypes["writes_to"])
	}
	if !contains(refTypes["calls"], "update_last_access") {
		t.Errorf("expected calls update_last_access, got %v", refTypes["calls"])
	}
}

func TestParseCreateFunction(t *testing.T) {
	input := `
CREATE FUNCTION calculate_tax(price DECIMAL(10,2))
RETURNS DECIMAL(10,2)
DETERMINISTIC
BEGIN
    DECLARE tax DECIMAL(10,2);
    SELECT tax_rate INTO tax FROM tax_rates WHERE active = 1;
    RETURN price * tax;
END;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
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
	if fn.QualifiedName != "calculate_tax" {
		t.Errorf("expected calculate_tax, got %s", fn.QualifiedName)
	}

	found := false
	for _, ref := range result.References {
		if ref.ReferenceType == "reads_from" && ref.ToName == "tax_rates" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected reads_from tax_rates reference")
	}
}

func TestParseCreateView(t *testing.T) {
	input := `
CREATE VIEW active_users AS
SELECT u.id, u.username, u.email
FROM users u
JOIN user_roles ur ON u.id = ur.user_id
WHERE u.active = 1;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
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

	refTypes := make(map[string][]string)
	for _, ref := range result.References {
		refTypes[ref.ReferenceType] = append(refTypes[ref.ReferenceType], ref.ToName)
	}
	if !contains(refTypes["reads_from"], "users") {
		t.Errorf("expected reads_from users, got %v", refTypes["reads_from"])
	}
	if !contains(refTypes["joins"], "user_roles") {
		t.Errorf("expected joins user_roles, got %v", refTypes["joins"])
	}
}

func TestParseCreateTrigger(t *testing.T) {
	input := `
CREATE TRIGGER before_user_update
BEFORE UPDATE ON users
FOR EACH ROW
BEGIN
    INSERT INTO audit_log (action, user_id, old_email, new_email)
    VALUES ('email_change', OLD.id, OLD.email, NEW.email);
END;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
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
	if trigger.QualifiedName != "before_user_update" {
		t.Errorf("expected before_user_update, got %s", trigger.QualifiedName)
	}

	// Should have uses_table ref to users (ON users)
	found := false
	for _, ref := range result.References {
		if ref.ReferenceType == "uses_table" && ref.ToName == "users" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected uses_table reference to users")
	}

	// Should have writes_to ref to audit_log
	found = false
	for _, ref := range result.References {
		if ref.ReferenceType == "writes_to" && ref.ToName == "audit_log" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected writes_to reference to audit_log")
	}
}

func TestParseDefinerClause(t *testing.T) {
	input := `
CREATE DEFINER='root'@'localhost' PROCEDURE admin_cleanup()
BEGIN
    DELETE FROM temp_sessions;
END;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Symbols) == 0 {
		t.Fatal("expected procedure symbol")
	}
	if result.Symbols[0].Kind != "procedure" {
		t.Errorf("expected procedure, got %s", result.Symbols[0].Kind)
	}
	if result.Symbols[0].QualifiedName != "admin_cleanup" {
		t.Errorf("expected admin_cleanup, got %s", result.Symbols[0].QualifiedName)
	}
}

func TestParseInsertSelectLineage(t *testing.T) {
	input := `
CREATE PROCEDURE copy_active_users()
BEGIN
    INSERT INTO archived_users (name, email)
    SELECT username, email FROM users WHERE active = 0;
END;
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input), Language: "mysql"})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ColumnReferences) == 0 {
		t.Fatal("expected column references for INSERT...SELECT")
	}

	found := false
	for _, cr := range result.ColumnReferences {
		if cr.TargetColumn == "archived_users.name" && cr.SourceColumn == "username" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected column lineage username -> archived_users.name, got %+v", result.ColumnReferences)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
