package resolver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/parser"
)

func TestOrmNameVariants(t *testing.T) {
	tests := []struct {
		input    string
		contains []string
	}{
		{"Customer", []string{"Customer", "Customers"}},
		{"Category", []string{"Category", "Categories"}},
		{"Orders", []string{"Orders", "Order"}},
		{"Status", []string{"Status", "Statuses"}},
		{"Box", []string{"Box", "Boxes"}},
	}
	for _, tt := range tests {
		variants := ormNameVariants(tt.input)
		for _, want := range tt.contains {
			found := false
			for _, v := range variants {
				if v == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ormNameVariants(%q) missing %q, got %v", tt.input, want, variants)
			}
		}
	}
}

// crossLangTable builds a SymbolTable with T-SQL targets for cross-language tests.
func crossLangTable() (*SymbolTable, map[string]uuid.UUID) {
	table := newSymbolTable()
	ids := map[string]uuid.UUID{
		"dbo.Customers":   uuid.MustParse("00000000-0000-0000-0000-000000000010"),
		"dbo.GetCustomer": uuid.MustParse("00000000-0000-0000-0000-000000000011"),
		"dbo.Orders":      uuid.MustParse("00000000-0000-0000-0000-000000000012"),
	}
	for fqn, id := range ids {
		table.ByFQN[fqn] = id
		sn := shortNameOf(fqn)
		table.ByShortName[sn] = append(table.ByShortName[sn], id)
		table.ByLang[fqn] = "tsql"
	}
	return table, ids
}

func TestResolve_CaseInsensitive(t *testing.T) {
	// Use a table without dbo. prefix so schema_qualified doesn't fire first
	table := newSymbolTable()
	id := uuid.MustParse("00000000-0000-0000-0000-000000000020")
	table.ByFQN["Inventory"] = id
	table.ByShortName["Inventory"] = []uuid.UUID{id}
	table.ByLang["Inventory"] = "tsql"

	clr := NewCrossLangResolver(nil)

	ref := parser.RawReference{ToName: "inventory", ReferenceType: "uses_table"}
	match, ok := clr.Resolve(ref, "asp", table)
	if !ok {
		t.Fatal("expected match")
	}
	if match.TargetID != id {
		t.Errorf("wrong target: got %s", match.TargetID)
	}
	if match.Confidence != 0.85 {
		t.Errorf("expected confidence 0.85, got %f", match.Confidence)
	}
	if match.Strategy != "case_insensitive" {
		t.Errorf("expected strategy case_insensitive, got %s", match.Strategy)
	}
}

func TestResolve_SchemaQualified(t *testing.T) {
	table, ids := crossLangTable()
	clr := NewCrossLangResolver(nil)

	ref := parser.RawReference{ToName: "Customers", ToQualified: "dbo.Customers", ReferenceType: "uses_table"}
	match, ok := clr.Resolve(ref, "csharp", table)
	if !ok {
		t.Fatal("expected match")
	}
	if match.TargetID != ids["dbo.Customers"] {
		t.Errorf("wrong target: got %s", match.TargetID)
	}
	if match.Confidence != 0.95 {
		t.Errorf("expected confidence 0.95, got %f", match.Confidence)
	}
	if match.Strategy != "schema_qualified" {
		t.Errorf("expected strategy schema_qualified, got %s", match.Strategy)
	}
}

func TestResolve_StripPrefix(t *testing.T) {
	table, ids := crossLangTable()
	clr := NewCrossLangResolver(nil)

	ref := parser.RawReference{ToName: "TCustomers", ReferenceType: "uses_table"}
	match, ok := clr.Resolve(ref, "delphi", table)
	if !ok {
		t.Fatal("expected match via strip_prefix")
	}
	if match.TargetID != ids["dbo.Customers"] {
		t.Errorf("wrong target: got %s", match.TargetID)
	}
	if match.Confidence != 0.75 {
		t.Errorf("expected confidence 0.75, got %f", match.Confidence)
	}
	if match.Strategy != "strip_prefix" {
		t.Errorf("expected strategy strip_prefix, got %s", match.Strategy)
	}
}

func TestResolve_OrmConvention(t *testing.T) {
	table, ids := crossLangTable()
	clr := NewCrossLangResolver(nil)

	// "Order" (singular) should match "Orders" (plural) table via ORM convention
	ref := parser.RawReference{ToName: "Order", ReferenceType: "uses_table"}
	match, ok := clr.Resolve(ref, "csharp", table)
	if !ok {
		t.Fatal("expected match via orm_convention")
	}
	if match.TargetID != ids["dbo.Orders"] {
		t.Errorf("wrong target: got %s", match.TargetID)
	}
	if match.Confidence != 0.7 {
		t.Errorf("expected confidence 0.7, got %f", match.Confidence)
	}
	if match.Strategy != "orm_convention" {
		t.Errorf("expected strategy orm_convention, got %s", match.Strategy)
	}
}

func TestResolve_NoMatch(t *testing.T) {
	table, _ := crossLangTable()
	clr := NewCrossLangResolver(nil)

	ref := parser.RawReference{ToName: "CompletelyFakeSymbol", ReferenceType: "calls"}
	_, ok := clr.Resolve(ref, "csharp", table)
	if ok {
		t.Error("expected no match")
	}
}

// ---------------------------------------------------------------------------
// API route matching tests
// ---------------------------------------------------------------------------

// apiRouteTable builds a SymbolTable with backend endpoint symbols.
func apiRouteTable() (*SymbolTable, map[string]uuid.UUID) {
	table := newSymbolTable()
	ids := map[string]uuid.UUID{
		"GET /api/orders":      uuid.MustParse("00000000-0000-0000-0000-000000000100"),
		"GET /api/orders/{id}": uuid.MustParse("00000000-0000-0000-0000-000000000101"),
		"POST /api/orders":     uuid.MustParse("00000000-0000-0000-0000-000000000102"),
		"DELETE /api/users/{id}": uuid.MustParse("00000000-0000-0000-0000-000000000103"),
	}
	for sig, id := range ids {
		table.BySignature[sig] = id
		table.ByLang[sig] = "csharp"
	}
	return table, ids
}

func TestResolve_APIRouteMatch_Exact(t *testing.T) {
	table, ids := apiRouteTable()
	clr := NewCrossLangResolver(nil)

	ref := parser.RawReference{
		ToName:        "GET /api/orders",
		ReferenceType: "calls_api",
	}
	match, ok := clr.Resolve(ref, "typescript", table)
	if !ok {
		t.Fatal("expected api_route_match")
	}
	if match.TargetID != ids["GET /api/orders"] {
		t.Errorf("wrong target: got %s, want %s", match.TargetID, ids["GET /api/orders"])
	}
	if match.Strategy != "api_route_match" {
		t.Errorf("expected strategy api_route_match, got %s", match.Strategy)
	}
	if match.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", match.Confidence)
	}
}

func TestResolve_APIRouteMatch_PathParam(t *testing.T) {
	table, ids := apiRouteTable()
	clr := NewCrossLangResolver(nil)

	// Frontend emits "/api/orders/{*}" (template literal) — should match "GET /api/orders/{id}"
	ref := parser.RawReference{
		ToName:        "GET /api/orders/{*}",
		ReferenceType: "calls_api",
	}
	match, ok := clr.Resolve(ref, "javascript", table)
	if !ok {
		t.Fatal("expected api_route_match for parameterised route")
	}
	if match.TargetID != ids["GET /api/orders/{id}"] {
		t.Errorf("wrong target: got %s, want %s", match.TargetID, ids["GET /api/orders/{id}"])
	}
}

func TestResolve_APIRouteMatch_VerbMismatch(t *testing.T) {
	table, _ := apiRouteTable()
	clr := NewCrossLangResolver(nil)

	// POST /api/orders/{id} — no such endpoint in our table (only DELETE)
	ref := parser.RawReference{
		ToName:        "POST /api/users/{*}",
		ReferenceType: "calls_api",
	}
	_, ok := clr.Resolve(ref, "javascript", table)
	if ok {
		t.Error("expected no match for wrong verb")
	}
}

func TestResolve_APIRouteMatch_NoVerbInRef(t *testing.T) {
	table, ids := apiRouteTable()
	clr := NewCrossLangResolver(nil)

	// fetch("/api/orders") without explicit verb — should still match
	ref := parser.RawReference{
		ToName:        "/api/orders",
		ReferenceType: "calls_api",
	}
	match, ok := clr.Resolve(ref, "typescript", table)
	if !ok {
		t.Fatal("expected match when ref has no verb")
	}
	// Could match GET or POST /api/orders — just verify it matched something valid
	validIDs := map[uuid.UUID]bool{
		ids["GET /api/orders"]:  true,
		ids["POST /api/orders"]: true,
	}
	if !validIDs[match.TargetID] {
		t.Errorf("matched unexpected target %s", match.TargetID)
	}
}

func TestNormalizeRouteForMatch(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"GET /api/orders/{id}", "get /api/orders/{p}"},
		{"get /api/orders/{*}", "get /api/orders/{p}"},
		{"POST /api/users/{id:int}", "post /api/users/{p}"},
		{"/api/orders", "/api/orders"},
		{"DELETE /api/items/{id}/", "delete /api/items/{p}"},
		{"/api/users/:id", "/api/users/{p}"},
	}
	for _, tt := range tests {
		got := normalizeRouteForMatch(tt.input)
		if got != tt.want {
			t.Errorf("normalizeRouteForMatch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
