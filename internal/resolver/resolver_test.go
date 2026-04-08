package resolver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/parser"
)

func TestShortNameOf(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"dbo.Customers", "Customers"},
		{"schema.proc", "proc"},
		{"X", "X"},
		{"a.b.c.d", "d"},
		{"", ""},
	}
	for _, tt := range tests {
		got := shortNameOf(tt.input)
		if got != tt.want {
			t.Errorf("shortNameOf(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// helper to build a minimal SymbolTable for testing resolveTarget.
func testSymbolTable() (*SymbolTable, map[string]uuid.UUID) {
	table := newSymbolTable()
	ids := map[string]uuid.UUID{
		"dbo.Customers":    uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		"dbo.GetCustomer":  uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		"dbo.Orders":       uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		"app.OrderService": uuid.MustParse("00000000-0000-0000-0000-000000000004"),
	}

	for fqn, id := range ids {
		table.ByFQN[fqn] = id
		sn := shortNameOf(fqn)
		table.ByShortName[sn] = append(table.ByShortName[sn], id)
	}

	// Set languages for cross-language tests
	table.ByLang["dbo.Customers"] = "tsql"
	table.ByLang["dbo.GetCustomer"] = "tsql"
	table.ByLang["dbo.Orders"] = "tsql"
	table.ByLang["app.OrderService"] = "csharp"

	return table, ids
}

func TestResolveTarget_QualifiedName(t *testing.T) {
	table, ids := testSymbolTable()

	ref := parser.RawReference{ToQualified: "dbo.Customers", ToName: "Customers"}
	result := resolveTarget(ref, nil, table, nil, "")
	if !result.Resolved {
		t.Fatal("expected resolved")
	}
	if result.TargetID != ids["dbo.Customers"] {
		t.Errorf("wrong target: got %s", result.TargetID)
	}
	if result.Confidence != 1.0 {
		t.Errorf("expected confidence 1.0, got %f", result.Confidence)
	}
}

func TestResolveTarget_LocalScope(t *testing.T) {
	table, ids := testSymbolTable()
	localScope := map[string]uuid.UUID{
		"Customers": ids["dbo.Customers"],
	}

	ref := parser.RawReference{ToName: "Customers"}
	result := resolveTarget(ref, localScope, table, nil, "")
	if !result.Resolved {
		t.Fatal("expected resolved")
	}
	if result.TargetID != ids["dbo.Customers"] {
		t.Errorf("wrong target: got %s", result.TargetID)
	}
	if result.Confidence != 1.0 {
		t.Errorf("expected confidence 1.0, got %f", result.Confidence)
	}
}

func TestResolveTarget_ShortNameUnambiguous(t *testing.T) {
	table, ids := testSymbolTable()

	// "GetCustomer" has only one candidate
	ref := parser.RawReference{ToName: "GetCustomer"}
	result := resolveTarget(ref, nil, table, nil, "")
	if !result.Resolved {
		t.Fatal("expected resolved")
	}
	if result.TargetID != ids["dbo.GetCustomer"] {
		t.Errorf("wrong target: got %s", result.TargetID)
	}
}

func TestResolveTarget_CaseInsensitive(t *testing.T) {
	table, _ := testSymbolTable()

	ref := parser.RawReference{ToName: "customers"}
	result := resolveTarget(ref, nil, table, nil, "")
	if !result.Resolved {
		t.Fatal("expected resolved via case-insensitive match")
	}
	if result.Confidence != 1.0 {
		t.Errorf("expected confidence 1.0 for case-insensitive FQN match, got %f", result.Confidence)
	}
}

func TestResolveTarget_CrossLanguage(t *testing.T) {
	// Build a table where the target only resolves via cross-language bridge.
	// Use a name that won't match in steps 1-4 (FQN, local scope, short name, case-insensitive).
	table := newSymbolTable()
	targetID := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	// ORM convention: C# "Order" → T-SQL "dbo.Orders" (pluralized)
	table.ByFQN["dbo.Orders"] = targetID
	table.ByShortName["Orders"] = []uuid.UUID{targetID}
	table.ByLang["dbo.Orders"] = "tsql"

	crossLang := NewCrossLangResolver(nil)

	// C# referencing "Order" (singular) — won't match FQN, local, short, or case-insensitive
	// but will match via ORM convention (Order → Orders)
	ref := parser.RawReference{ToName: "Order", ReferenceType: "uses_table"}
	result := resolveTarget(ref, nil, table, crossLang, "csharp")
	if !result.Resolved {
		t.Fatal("expected resolved via cross-language")
	}
	if result.TargetID != targetID {
		t.Errorf("wrong target: got %s", result.TargetID)
	}
	if !result.CrossLang {
		t.Error("expected CrossLang=true")
	}
	if result.Bridge == "" {
		t.Error("expected bridge to be set")
	}
}

func TestResolveTarget_NoMatch(t *testing.T) {
	table, _ := testSymbolTable()

	ref := parser.RawReference{ToName: "NonExistentSymbol"}
	result := resolveTarget(ref, nil, table, nil, "")
	if result.Resolved {
		t.Error("expected not resolved")
	}
}
