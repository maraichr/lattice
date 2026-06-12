package sqlutil

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestExtractTableRefs_FromJoin(t *testing.T) {
	sql := "SELECT u.name FROM Users u JOIN Orders o ON u.id = o.user_id"
	refs := ExtractTableRefs(sql, 10, "MyClass.GetData", "dbo")

	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %v", len(refs), refNames(refs))
	}
	assertHasRef(t, refs, "Users", "uses_table", "dbo.Users")
	assertHasRef(t, refs, "Orders", "uses_table", "dbo.Orders")
	for _, r := range refs {
		if r.FromSymbol != "MyClass.GetData" {
			t.Errorf("expected FromSymbol MyClass.GetData, got %s", r.FromSymbol)
		}
	}
}

func TestExtractTableRefs_InsertUpdate(t *testing.T) {
	refs := ExtractTableRefs("INSERT INTO Customers VALUES (1)", 5, "", "dbo")
	assertHasRef(t, refs, "Customers", "writes_to", "dbo.Customers")

	refs = ExtractTableRefs("UPDATE Products SET price = 10", 6, "", "dbo")
	assertHasRef(t, refs, "Products", "writes_to", "dbo.Products")
}

func TestExtractTableRefs_Delete(t *testing.T) {
	refs := ExtractTableRefs("DELETE FROM Users WHERE id = 1", 1, "", "dbo")
	// DELETE keyword itself doesn't have a table next to it (FROM does), and
	// FROM picks up Users
	assertHasRef(t, refs, "Users", "uses_table", "dbo.Users")
}

func TestExtractTableRefs_Exec(t *testing.T) {
	refs := ExtractTableRefs("EXEC dbo.GetUserById @id = 1", 1, "caller", "dbo")
	assertHasRef(t, refs, "dbo.GetUserById", "calls", "dbo.GetUserById")

	refs = ExtractTableRefs("EXECUTE sp_GetOrders", 2, "caller", "dbo")
	assertHasRef(t, refs, "sp_GetOrders", "calls", "dbo.sp_GetOrders")
}

func TestExtractTableRefs_SchemaQualified(t *testing.T) {
	refs := ExtractTableRefs("SELECT * FROM dbo.Users u JOIN audit.Log l ON l.UserID = u.ID", 1, "", "dbo")
	assertHasRef(t, refs, "dbo.Users", "uses_table", "dbo.Users")
	assertHasRef(t, refs, "audit.Log", "uses_table", "audit.Log")
}

func TestExtractTableRefs_NoSchema(t *testing.T) {
	refs := ExtractTableRefs("SELECT * FROM users", 1, "", "")
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].ToQualified != "" {
		t.Errorf("expected empty ToQualified, got %s", refs[0].ToQualified)
	}
}

func TestExtractTableRefs_FilterKeywords(t *testing.T) {
	refs := ExtractTableRefs("SELECT * FROM WHERE", 1, "", "dbo")
	if len(refs) != 0 {
		t.Errorf("expected 0 refs (WHERE is keyword), got %d: %v", len(refs), refNames(refs))
	}
}

func TestLooksLikeSQL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"SELECT * FROM users", true},
		{"INSERT INTO orders VALUES (1)", true},
		{"UPDATE products SET name = 'x'", true},
		{"DELETE FROM users WHERE id = 1", true},
		{"EXEC dbo.GetUser", true},
		{"CreateUser", false},
		{"DeleteUser", false},
		{"SELECT", true},
		{"hello world", false},
		{"", false},
	}

	for _, tt := range tests {
		got := LooksLikeSQL(tt.input)
		if got != tt.want {
			t.Errorf("LooksLikeSQL(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsSQLKeyword(t *testing.T) {
	if !IsSQLKeyword("SELECT") {
		t.Error("SELECT should be a keyword")
	}
	if !IsSQLKeyword("from") {
		t.Error("from should be a keyword (case-insensitive)")
	}
	if IsSQLKeyword("Users") {
		t.Error("Users should not be a keyword")
	}
}

// --- helpers ---

func assertHasRef(t *testing.T, refs []parser.RawReference, toName, refType, toQualified string) {
	t.Helper()
	for _, r := range refs {
		if r.ToName == toName && r.ReferenceType == refType {
			if toQualified != "" && r.ToQualified != toQualified {
				t.Errorf("ref %s has ToQualified=%s, want %s", toName, r.ToQualified, toQualified)
			}
			return
		}
	}
	t.Errorf("missing ref: ToName=%s, type=%s; have: %v", toName, refType, refNames(refs))
}

func refNames(refs []parser.RawReference) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.ToName + " (" + r.ReferenceType + ")"
	}
	return names
}
