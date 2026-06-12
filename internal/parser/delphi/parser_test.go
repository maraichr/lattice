package delphi

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestPascalUnit(t *testing.T) {
	src := `unit MyUnit;

interface

uses SysUtils, Classes;

type
  TMyClass = class(TBaseClass)
  private
    FName: string;
  public
    procedure DoWork;
    function GetName: string;
  end;

implementation

procedure TMyClass.DoWork;
begin
  // ...
end;

function TMyClass.GetName: string;
begin
  Result := FName;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "MyUnit.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyUnit", "module")
	assertHasSymbol(t, result.Symbols, "MyUnit.TMyClass", "class")
	assertHasRef(t, result.References, "TBaseClass", "inherits")
	assertHasRef(t, result.References, "SysUtils", "imports")
}

func TestPascalSQLTextAssignment(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.LoadCustomers;
begin
  MyQuery.SQL.Text := 'SELECT * FROM Customers WHERE Active = 1';
  MyQuery.Open;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Customers")
}

func TestPascalSQLAdd(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.LoadOrders;
begin
  MyQuery.SQL.Add('SELECT * FROM Orders');
  MyQuery.SQL.Add('WHERE CustomerID = :ID');
  MyQuery.Open;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Orders")
}

func TestPascalCommandText(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.ExecProc;
begin
  MyCmd.CommandText := 'EXEC dbo.GetUser @ID = 1';
  MyCmd.Execute;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "dbo.GetUser")
}

func TestPascalMultiLineSQLText(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.LoadData;
begin
  MyQuery.SQL.Text := 'SELECT * ' +
    'FROM Customers c ' +
    'JOIN Orders o ON c.ID = o.CustomerID';
  MyQuery.Open;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Customers")
	assertRefTarget(t, tableRefs, "Orders")
}

func TestDFMSQLStrings(t *testing.T) {
	content := `object Form1: TForm1
  object qryCustomers: TADOQuery
    SQL.Strings = (
      'SELECT * FROM Customers'
      'WHERE Active = 1'
    )
  end
end`

	symbols, refs := ParseDFM(content, 0)
	if len(symbols) == 0 {
		t.Fatal("expected symbols from DFM")
	}

	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "Customers")
}

func TestDFMCommandText(t *testing.T) {
	content := `object Form1: TForm1
  object cmdGetUser: TADOCommand
    CommandText = 'EXEC dbo.GetUserById @ID = :ID'
  end
end`

	_, refs := ParseDFM(content, 0)
	callRefs := filterRefs(refs, "calls")
	assertRefTarget(t, callRefs, "dbo.GetUserById")
}

func TestDFMSelectSQLStrings(t *testing.T) {
	content := `object Form1: TForm1
  object qryOrders: TIBQuery
    SelectSQL.Strings = (
      'SELECT * FROM Orders'
      'WHERE Status = 1'
    )
  end
end`

	_, refs := ParseDFM(content, 0)
	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "Orders")
}

func TestPascalSQLAddSplitClauses(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.LoadInvoices;
begin
  MyQuery.SQL.Clear;
  MyQuery.SQL.Add('SELECT i.ID, i.Total');
  MyQuery.SQL.Add('FROM');
  MyQuery.SQL.Add('Invoices i');
  MyQuery.SQL.Add('JOIN Customers c ON c.ID = i.CustomerID');
  MyQuery.Open;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Invoices")
	assertRefTarget(t, tableRefs, "Customers")
}

func TestPascalStoredProcName(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.GetBalance;
begin
  spBalance.StoredProcName := 'GetCustomerBalance';
  spBalance.ExecProc;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "GetCustomerBalance")
	if len(callRefs) != 1 || callRefs[0].ToQualified != "dbo.GetCustomerBalance" {
		t.Errorf("expected ToQualified dbo.GetCustomerBalance, got %+v", callRefs)
	}
	if callRefs[0].FromSymbol != "DataModule.TDataModule.GetBalance" {
		t.Errorf("expected FromSymbol DataModule.TDataModule.GetBalance, got %s", callRefs[0].FromSymbol)
	}
}

func TestPascalProcedureNameInWithBlock(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.RunOrders;
begin
  with ADOStoredProc1 do
  begin
    ProcedureName := 'usp_GetOrders;1';
    Open;
  end;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "usp_GetOrders")
}

func TestPascalCommandTextProcName(t *testing.T) {
	src := `unit DataModule;

implementation

procedure TDataModule.Cleanup;
begin
  MyCmd.CommandText := 'PurgeExpiredSessions';
  MyCmd.Execute;
end;

end.`

	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataModule.pas", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "PurgeExpiredSessions")
}

func TestDFMSQLStringsParenOnLastLine(t *testing.T) {
	// The Delphi DFM writer puts the closing paren on the last string's line.
	content := `object Form1: TForm1
  object qryUsers: TADOQuery
    SQL.Strings = (
      'SELECT * '
      'FROM dbo.Users')
    Left = 24
  end
end`

	_, refs := ParseDFM(content, 0)
	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "dbo.Users")
	if len(tableRefs) != 1 || tableRefs[0].ToQualified != "dbo.Users" {
		t.Errorf("expected ToQualified dbo.Users, got %+v", tableRefs)
	}
}

func TestDFMSQLStringsContinuation(t *testing.T) {
	// Long list items are split mid-token across lines with a trailing '+'.
	content := `object Form1: TForm1
  object qryOrders: TADOQuery
    SQL.Strings = (
      'SELECT * FROM Ord' +
      'ers o'
      'WHERE o.Status = 1')
  end
end`

	_, refs := ParseDFM(content, 0)
	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "Orders")
}

func TestDFMStoredProcName(t *testing.T) {
	content := `object DataModule1: TDataModule
  object spBalance: TStoredProc
    DatabaseName = 'Main'
    StoredProcName = 'GetCustomerBalance'
  end
  object spOrders: TADOStoredProc
    ProcedureName = 'usp_GetOrders;1'
  end
end`

	symbols, refs := ParseDFM(content, 0)
	assertHasSymbol(t, symbols, "TStoredProc.spBalance", "variable")

	callRefs := filterRefs(refs, "calls")
	assertRefTarget(t, callRefs, "GetCustomerBalance")
	assertRefTarget(t, callRefs, "usp_GetOrders")
	for _, r := range callRefs {
		if r.ToName == "usp_GetOrders" && r.FromSymbol != "spOrders" {
			t.Errorf("expected FromSymbol spOrders, got %s", r.FromSymbol)
		}
	}
}

func TestDFMCommandTextProcName(t *testing.T) {
	content := `object Form1: TForm1
  object cmdPurge: TADOCommand
    CommandText = 'PurgeExpiredSessions'
  end
end`

	_, refs := ParseDFM(content, 0)
	callRefs := filterRefs(refs, "calls")
	assertRefTarget(t, callRefs, "PurgeExpiredSessions")
}

func TestDFMCommandTextTableType(t *testing.T) {
	// With CommandType = cmdTable, CommandText holds a table name, not a proc.
	content := `object Form1: TForm1
  object tblUsers: TADODataSet
    CommandText = 'Users'
    CommandType = cmdTable
  end
end`

	_, refs := ParseDFM(content, 0)
	if calls := filterRefs(refs, "calls"); len(calls) != 0 {
		t.Errorf("expected no calls refs for cmdTable, got %+v", calls)
	}
	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
}

func TestDFMCommandTextMultiLine(t *testing.T) {
	content := `object Form1: TForm1
  object qryReport: TADOQuery
    CommandText = 'SELECT r.ID, r.Name ' +
      'FROM Reports r ' +
      'JOIN Runs x ON x.ReportID = r.ID'
  end
end`

	_, refs := ParseDFM(content, 0)
	tableRefs := filterRefs(refs, "uses_table")
	assertRefTarget(t, tableRefs, "Reports")
	assertRefTarget(t, tableRefs, "Runs")
}

// --- helpers ---

func assertHasSymbol(t *testing.T, symbols []parser.Symbol, qname, kind string) {
	t.Helper()
	for _, s := range symbols {
		if s.QualifiedName == qname && s.Kind == kind {
			return
		}
	}
	names := make([]string, len(symbols))
	for i, s := range symbols {
		names[i] = s.QualifiedName + " (" + s.Kind + ")"
	}
	t.Errorf("missing symbol %s (%s); have: %v", qname, kind, names)
}

func assertHasRef(t *testing.T, refs []parser.RawReference, toName, refType string) {
	t.Helper()
	for _, r := range refs {
		if r.ToName == toName && r.ReferenceType == refType {
			return
		}
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.ToName + " (" + r.ReferenceType + ")"
	}
	t.Errorf("missing ref %s (%s); have: %v", toName, refType, names)
}

func filterRefs(refs []parser.RawReference, refType string) []parser.RawReference {
	var out []parser.RawReference
	for _, r := range refs {
		if r.ReferenceType == refType {
			out = append(out, r)
		}
	}
	return out
}

func assertRefTarget(t *testing.T, refs []parser.RawReference, target string) {
	t.Helper()
	for _, r := range refs {
		if r.ToName == target || r.ToQualified == target {
			return
		}
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.ToName
	}
	t.Errorf("missing ref target %s; have: %v", target, names)
}
