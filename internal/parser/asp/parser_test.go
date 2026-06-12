package asp

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestDirectivesCodeBehind(t *testing.T) {
	src := `<%@ Page Language="C#" CodeBehind="Users.aspx.cs" Inherits="MyApp.UsersPage" %>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Users.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "Users.aspx.cs")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "MyApp.UsersPage")
}

func TestDirectivesImportNamespace(t *testing.T) {
	src := `<%@ Import Namespace="System.Data" %>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Page.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "System.Data")
}

func TestDirectivesRegister(t *testing.T) {
	src := `<%@ Register Assembly="Telerik.Web.UI" Namespace="Telerik.Web.UI" TagPrefix="telerik" %>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Page.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "Telerik.Web.UI")
}

func TestDirectivesControlCodeBehind(t *testing.T) {
	src := `<%@ Control Language="C#" CodeBehind="UserControl.ascx.cs" Inherits="MyApp.UserControl" %>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserControl.ascx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "UserControl.ascx.cs")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "MyApp.UserControl")
}

func TestDirectivesMixedWithVBScript(t *testing.T) {
	src := `<%@ Page Language="C#" CodeBehind="Users.aspx.cs" Inherits="MyApp.UsersPage" %>
<%@ Import Namespace="System.Data" %>
<html>
<% Dim x = 1 %>
</html>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Users.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "Users.aspx.cs")
	assertRefTarget(t, imports, "System.Data")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "MyApp.UsersPage")
}

func TestVBScriptFunction(t *testing.T) {
	src := `<%
Function GetUserName(userId)
  GetUserName = "test"
End Function
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "utils.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "GetUserName", "function")
}

func TestVBScriptSub(t *testing.T) {
	src := `<%
Sub ProcessData()
  ' do something
End Sub
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "process.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "ProcessData", "procedure")
}

func TestIncludeDirective(t *testing.T) {
	src := `<!-- #include file="header.asp" -->`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "header.asp")
}

func TestSQLSelfAppendConcat(t *testing.T) {
	// sql = sql & "..." is the dominant SQL-building pattern in classic ASP
	src := `<%
Dim sql
sql = "SELECT u.Name, u.Email "
sql = sql & "FROM Users u "
sql = sql & "INNER JOIN Roles r ON r.RoleID = u.RoleID "
sql = sql & "WHERE u.Active = 1"
Set rs = conn.Execute(sql)
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "Users")
	assertRefTarget(t, reads, "Roles")
}

func TestSQLArbitraryVariableName(t *testing.T) {
	src := `<%
strQuery = "SELECT * FROM Orders WHERE Total > 100"
rs.Open strQuery, conn
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "Orders")
}

func TestSQLLineContinuation(t *testing.T) {
	src := `<%
sql = "SELECT OrderID, Total " & _
      "FROM Orders " & _
      "WHERE Status = 'Open'"
rs.Open sql, conn
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "Orders")
}

func TestStoredProcByCommandText(t *testing.T) {
	src := `<%
Set cmd = Server.CreateObject("ADODB.Command")
cmd.CommandType = adCmdStoredProc
cmd.CommandText = "GetUsersByStatus"
cmd.Execute
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	calls := filterRefs(result.References, "calls")
	assertRefTarget(t, calls, "GetUsersByStatus")
}

func TestScriptRunatServerBlock(t *testing.T) {
	src := `<%@ Page Language="VB" %>
<script runat="server">
Sub Page_Load(sender As Object, e As EventArgs)
    Dim sql
    sql = "SELECT * FROM Customers"
    ExecuteQuery(sql)
End Sub
</script>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Page_Load", "procedure")

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "Customers")

	// SQL ref should be attributed to the enclosing Sub
	for _, r := range reads {
		if r.ToName == "Customers" && r.FromSymbol != "Page_Load" {
			t.Errorf("expected SQL ref from Page_Load, got %q", r.FromSymbol)
		}
	}
}

func TestSqlDataSourceCommands(t *testing.T) {
	src := `<%@ Page Language="C#" %>
<asp:SqlDataSource ID="dsUsers" runat="server"
    ConnectionString="<%$ ConnectionStrings:Main %>"
    SelectCommand="SELECT UserID, Name FROM Users WHERE Active = 1"
    UpdateCommand="UPDATE Users SET Name = @Name WHERE UserID = @UserID">
</asp:SqlDataSource>
<asp:SqlDataSource ID="dsOrders" runat="server"
    SelectCommand="GetOpenOrders" SelectCommandType="StoredProcedure">
</asp:SqlDataSource>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.aspx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "Users")

	writes := filterRefs(result.References, "writes_to")
	assertRefTarget(t, writes, "Users")

	calls := filterRefs(result.References, "calls")
	assertRefTarget(t, calls, "GetOpenOrders")
}

func TestWebHandlerCSharpBody(t *testing.T) {
	src := `<%@ WebHandler Language="C#" Class="ExportHandler" %>
using System;
using System.Data.SqlClient;

public class ExportHandler : IHttpHandler {
    public void ProcessRequest(HttpContext context) {
        using (var cmd = new SqlCommand("SELECT * FROM ExportQueue", conn)) {
            cmd.ExecuteReader();
        }
    }
}`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Export.ashx", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "ExportHandler", "class")

	reads := filterRefs(result.References, "reads_from")
	assertRefTarget(t, reads, "ExportQueue")

	for _, r := range reads {
		if r.ToName == "ExportQueue" && r.FromSymbol != "ExportHandler" {
			t.Errorf("expected SQL ref from ExportHandler, got %q", r.FromSymbol)
		}
	}
}

func TestSQLRefsNotEmittedForUIStrings(t *testing.T) {
	// Strings that don't start with a SQL verb must not produce table refs
	src := `<%
msg = "Please update from the form below"
title = "User Selection"
%>`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "page.asp", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range result.References {
		if r.ReferenceType == "reads_from" || r.ReferenceType == "writes_to" {
			t.Errorf("unexpected SQL ref to %q from UI string", r.ToName)
		}
	}
}

func TestLanguages(t *testing.T) {
	p := New()
	langs := p.Languages()
	if len(langs) != 2 {
		t.Errorf("expected 2 languages, got %d: %v", len(langs), langs)
	}
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
