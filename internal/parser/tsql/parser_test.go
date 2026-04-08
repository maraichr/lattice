package tsql

import (
	"strings"
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestParseCreateTable(t *testing.T) {
	input := `
CREATE TABLE dbo.Users (
    UserID INT IDENTITY(1,1) PRIMARY KEY,
    Username NVARCHAR(50) NOT NULL,
    Email NVARCHAR(255) NOT NULL,
    CreatedAt DATETIME2 DEFAULT GETDATE()
);
GO
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
	if table.QualifiedName != "dbo.Users" {
		t.Errorf("expected dbo.Users, got %s", table.QualifiedName)
	}
	if len(table.Children) < 3 {
		t.Errorf("expected at least 3 columns, got %d", len(table.Children))
	}
}

func TestParseCreateProcedure(t *testing.T) {
	input := `
CREATE PROCEDURE dbo.GetUserOrders
    @UserID INT,
    @Status VARCHAR(20) = NULL
AS
BEGIN
    SET NOCOUNT ON;

    SELECT o.OrderID, o.Total
    FROM dbo.Orders o
    WHERE o.UserID = @UserID;

    INSERT INTO dbo.AuditLog (Action, UserID)
    VALUES ('GetOrders', @UserID);

    EXEC dbo.UpdateLastAccess @UserID;
END
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	// Should have the procedure symbol
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
	if proc.QualifiedName != "dbo.GetUserOrders" {
		t.Errorf("expected dbo.GetUserOrders, got %s", proc.QualifiedName)
	}

	// Should have references
	refTypes := map[string]bool{}
	for _, ref := range result.References {
		refTypes[ref.ReferenceType+":"+ref.ToQualified] = true
	}

	if !refTypes["reads_from:dbo.Orders"] {
		t.Error("expected reads_from reference to dbo.Orders")
	}
	if !refTypes["writes_to:dbo.AuditLog"] {
		t.Error("expected writes_to reference to dbo.AuditLog")
	}
	if !refTypes["calls:dbo.UpdateLastAccess"] {
		t.Error("expected calls reference to dbo.UpdateLastAccess")
	}
}

func TestParseCreateView(t *testing.T) {
	input := `
CREATE VIEW dbo.ActiveUsers AS
SELECT u.UserID, u.Username, u.Email
FROM dbo.Users u
WHERE u.IsActive = 1;
GO
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
	if view.QualifiedName != "dbo.ActiveUsers" {
		t.Errorf("expected dbo.ActiveUsers, got %s", view.QualifiedName)
	}

	// View should reference Users table
	found := false
	for _, ref := range result.References {
		if ref.ToQualified == "dbo.Users" && ref.ReferenceType == "reads_from" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected reads_from reference to dbo.Users")
	}
}

func TestParseCreateTrigger(t *testing.T) {
	input := `
CREATE TRIGGER dbo.trg_OrderInsert
ON dbo.Orders
AFTER INSERT
AS
BEGIN
    INSERT INTO dbo.OrderHistory (OrderID, Action)
    SELECT i.OrderID, 'INSERT'
    FROM inserted i;
END
GO
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
	if trigger.QualifiedName != "dbo.trg_OrderInsert" {
		t.Errorf("expected dbo.trg_OrderInsert, got %s", trigger.QualifiedName)
	}

	// Should reference Orders (ON table) and OrderHistory (INSERT INTO)
	refTypes := map[string]bool{}
	for _, ref := range result.References {
		refTypes[ref.ReferenceType+":"+ref.ToQualified] = true
	}
	if !refTypes["uses_table:dbo.Orders"] {
		t.Error("expected uses_table reference to dbo.Orders")
	}
	if !refTypes["writes_to:dbo.OrderHistory"] {
		t.Error("expected writes_to reference to dbo.OrderHistory")
	}
}

func TestColumnLineageQualification(t *testing.T) {
	input := `
CREATE PROCEDURE dbo.ArchiveOrders
AS
BEGIN
    INSERT INTO dbo.OrderHistory (OrderID, CustomerID, Amount)
    SELECT o.OrderID, o.CustomerID, o.Amount
    FROM dbo.Orders o
    WHERE o.OrderDate < DATEADD(year, -1, GETDATE())
END
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ColumnReferences) == 0 {
		t.Fatal("expected column references")
	}

	// Source columns should be qualified with the FROM table
	expectedRefs := map[string]string{
		"dbo.Orders.OrderID":    "dbo.OrderHistory.OrderID",
		"dbo.Orders.CustomerID": "dbo.OrderHistory.CustomerID",
		"dbo.Orders.Amount":     "dbo.OrderHistory.Amount",
	}

	for _, ref := range result.ColumnReferences {
		expected, ok := expectedRefs[ref.SourceColumn]
		if !ok {
			t.Errorf("unexpected source column: %s", ref.SourceColumn)
			continue
		}
		if ref.TargetColumn != expected {
			t.Errorf("for source %s: expected target %s, got %s", ref.SourceColumn, expected, ref.TargetColumn)
		}
		delete(expectedRefs, ref.SourceColumn)
	}

	for src, tgt := range expectedRefs {
		t.Errorf("missing column reference: %s → %s", src, tgt)
	}
}

func TestColumnLineageViewQualification(t *testing.T) {
	input := `
CREATE VIEW dbo.ActiveUsers AS
SELECT u.UserID, u.Username, r.RoleName
FROM dbo.Users u
JOIN dbo.Roles r ON u.RoleID = r.RoleID
WHERE u.IsActive = 1;
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ColumnReferences) == 0 {
		t.Fatal("expected column references from view")
	}

	// Source columns should be qualified with their FROM/JOIN tables
	sourceMap := make(map[string]bool)
	for _, ref := range result.ColumnReferences {
		sourceMap[ref.SourceColumn] = true
	}

	expected := []string{"dbo.Users.UserID", "dbo.Users.Username", "dbo.Roles.RoleName"}
	for _, exp := range expected {
		if !sourceMap[exp] {
			t.Errorf("expected qualified source column %s, got sources: %v", exp, sourceMap)
		}
	}
}

func TestColumnLineageBareColumnSingleTable(t *testing.T) {
	input := `
CREATE PROCEDURE dbo.CopyUsers
AS
BEGIN
    INSERT INTO dbo.UserBackup (UserID, Name)
    SELECT UserID, Name
    FROM dbo.Users
END
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ColumnReferences) < 2 {
		t.Fatalf("expected at least 2 column references, got %d", len(result.ColumnReferences))
	}

	// Bare column names should be qualified with the single FROM table
	for _, ref := range result.ColumnReferences {
		if !strings.HasPrefix(ref.SourceColumn, "dbo.Users.") {
			t.Errorf("expected source qualified as dbo.Users.*, got %s", ref.SourceColumn)
		}
	}
}

func TestDNNTemplateTokens(t *testing.T) {
	input := `
CREATE TABLE {databaseOwner}[{objectQualifier}JsonWebTokens] (
    TokenId VARCHAR(36) NOT NULL,
    UserId INT NOT NULL,
    RenewalToken VARCHAR(64) NOT NULL
)
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "01.00.00.SqlDataProvider", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Symbols) == 0 {
		t.Fatal("expected at least one symbol")
	}

	table := result.Symbols[0]
	if table.Kind != "table" {
		t.Errorf("expected table, got %s", table.Kind)
	}
	if table.Name != "JsonWebTokens" {
		t.Errorf("expected table name JsonWebTokens, got %s", table.Name)
	}

	if len(table.Children) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(table.Children))
	}

	expectedCols := []string{"TokenId", "UserId", "RenewalToken"}
	for i, col := range table.Children {
		if col.Name != expectedCols[i] {
			t.Errorf("column %d: expected %s, got %s", i, expectedCols[i], col.Name)
		}
		if col.Kind != "column" {
			t.Errorf("column %d: expected kind column, got %s", i, col.Kind)
		}
	}
}

func TestDNNTemplateTokensComplex(t *testing.T) {
	input := `
CREATE TABLE {databaseOwner}[{objectQualifier}Portals] (
    PortalID INT IDENTITY(1,1) NOT NULL,
    PortalName NVARCHAR(128) NOT NULL,
    ExpiryDate DATETIME NULL,
    UserRegistration INT NOT NULL
)
GO

CREATE PROCEDURE {databaseOwner}[{objectQualifier}GetPortal]
    @PortalID INT
AS
BEGIN
    SELECT PortalID, PortalName, ExpiryDate
    FROM {databaseOwner}[{objectQualifier}Portals]
    WHERE PortalID = @PortalID
END
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "01.00.00.SqlDataProvider", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	// Should have table + procedure
	var table, proc *parser.Symbol
	for i := range result.Symbols {
		switch result.Symbols[i].Kind {
		case "table":
			table = &result.Symbols[i]
		case "procedure":
			proc = &result.Symbols[i]
		}
	}

	if table == nil {
		t.Fatal("expected a table symbol")
	}
	if table.Name != "Portals" {
		t.Errorf("expected table Portals, got %s", table.Name)
	}
	if len(table.Children) != 4 {
		t.Errorf("expected 4 columns, got %d", len(table.Children))
	}

	if proc == nil {
		t.Fatal("expected a procedure symbol")
	}
	if proc.Name != "GetPortal" {
		t.Errorf("expected proc GetPortal, got %s", proc.Name)
	}

	// Should have reads_from reference from proc to Portals
	foundRef := false
	for _, ref := range result.References {
		if ref.ReferenceType == "reads_from" && ref.ToName == "Portals" {
			foundRef = true
			break
		}
	}
	if !foundRef {
		t.Error("expected reads_from reference to Portals")
	}
}

func TestViewColumnChildren(t *testing.T) {
	input := `
CREATE VIEW dbo.ActiveUsers AS
SELECT u.UserID, u.Username, u.Email
FROM dbo.Users u
WHERE u.IsActive = 1;
GO
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

	// View should have column children derived from SELECT output
	if len(view.Children) != 3 {
		t.Fatalf("expected 3 view column children, got %d", len(view.Children))
	}
	expectedCols := []string{"UserID", "Username", "Email"}
	for i, col := range view.Children {
		if col.Name != expectedCols[i] {
			t.Errorf("view column %d: expected %s, got %s", i, expectedCols[i], col.Name)
		}
		if col.Kind != "column" {
			t.Errorf("view column %d: expected kind column, got %s", i, col.Kind)
		}
		expectedQN := "dbo.ActiveUsers." + expectedCols[i]
		if col.QualifiedName != expectedQN {
			t.Errorf("view column %d: expected qn %s, got %s", i, expectedQN, col.QualifiedName)
		}
	}

	// Column lineage should link source table columns to view columns
	if len(result.ColumnReferences) < 3 {
		t.Fatalf("expected at least 3 column references, got %d", len(result.ColumnReferences))
	}

	expectedLineage := map[string]string{
		"dbo.Users.UserID":   "dbo.ActiveUsers.UserID",
		"dbo.Users.Username": "dbo.ActiveUsers.Username",
		"dbo.Users.Email":    "dbo.ActiveUsers.Email",
	}
	for _, ref := range result.ColumnReferences {
		expected, ok := expectedLineage[ref.SourceColumn]
		if !ok {
			continue
		}
		if ref.TargetColumn != expected {
			t.Errorf("for source %s: expected target %s, got %s", ref.SourceColumn, expected, ref.TargetColumn)
		}
		delete(expectedLineage, ref.SourceColumn)
	}
	for src, tgt := range expectedLineage {
		t.Errorf("missing column reference: %s → %s", src, tgt)
	}
}

func TestTopLevelInsertSelectLineage(t *testing.T) {
	input := `
INSERT INTO dbo.OrderHistory (OrderID, CustomerID, Amount)
SELECT OrderID, CustomerID, Amount
FROM dbo.Orders
WHERE OrderDate < '2020-01-01';
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "test.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ColumnReferences) < 3 {
		t.Fatalf("expected at least 3 column references from top-level INSERT...SELECT, got %d", len(result.ColumnReferences))
	}

	expectedRefs := map[string]string{
		"dbo.Orders.OrderID":    "dbo.OrderHistory.OrderID",
		"dbo.Orders.CustomerID": "dbo.OrderHistory.CustomerID",
		"dbo.Orders.Amount":     "dbo.OrderHistory.Amount",
	}

	for _, ref := range result.ColumnReferences {
		expected, ok := expectedRefs[ref.SourceColumn]
		if !ok {
			t.Errorf("unexpected source column: %s", ref.SourceColumn)
			continue
		}
		if ref.TargetColumn != expected {
			t.Errorf("for source %s: expected target %s, got %s", ref.SourceColumn, expected, ref.TargetColumn)
		}
		delete(expectedRefs, ref.SourceColumn)
	}

	for src, tgt := range expectedRefs {
		t.Errorf("missing column reference: %s → %s", src, tgt)
	}
}

func TestDialectDetection(t *testing.T) {
	tsql := `
DECLARE @UserID INT = 1;
SELECT TOP 10 * FROM dbo.Users WITH (NOLOCK);
GO
`
	if d := parser.DetectDialect([]byte(tsql), ".sql"); d != "tsql" {
		t.Errorf("expected tsql, got %s", d)
	}
}

func TestTopLevelExecCreatesRefs(t *testing.T) {
	input := `
EXEC dbo.AddUser @Name = 'Test'
GO
EXEC dbo.UpdatePermissions
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "migration/001_seed.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	// Should have a synthetic file symbol
	var scriptSym *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "script" {
			scriptSym = &result.Symbols[i]
			break
		}
	}
	if scriptSym == nil {
		t.Fatal("expected a script symbol for top-level statements")
	}
	if !strings.HasPrefix(scriptSym.QualifiedName, "__file__:") {
		t.Errorf("expected __file__: prefix, got %s", scriptSym.QualifiedName)
	}

	// Should have calls references
	var callRefs []parser.RawReference
	for _, r := range result.References {
		if r.ReferenceType == "calls" {
			callRefs = append(callRefs, r)
		}
	}
	if len(callRefs) != 2 {
		t.Fatalf("expected 2 calls refs from top-level EXEC, got %d", len(callRefs))
	}

	names := map[string]bool{}
	for _, r := range callRefs {
		names[r.ToName] = true
	}
	if !names["AddUser"] {
		t.Error("missing calls ref to AddUser")
	}
	if !names["UpdatePermissions"] {
		t.Error("missing calls ref to UpdatePermissions")
	}
}

func TestTopLevelExecNoRefsWhenEmpty(t *testing.T) {
	// When there are no top-level EXEC statements, no script symbol should be emitted
	input := `
CREATE TABLE dbo.Users (
    ID INT PRIMARY KEY
);
GO
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "schema.sql", Content: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range result.Symbols {
		if s.Kind == "script" {
			t.Error("unexpected script symbol when no top-level EXEC")
		}
	}
}
