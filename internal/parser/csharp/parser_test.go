package csharp

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestBasicClassWithMembers(t *testing.T) {
	src := `
namespace MyApp.Models {
    public class User {
        private string _name;
        public string Name { get; set; }
        public int GetAge() { return 0; }
        public User() {}
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	symbolMap := make(map[string]parser.Symbol)
	for _, s := range result.Symbols {
		symbolMap[s.QualifiedName] = s
	}

	assertSymbol(t, symbolMap, "MyApp.Models.User", "class")
	assertSymbol(t, symbolMap, "MyApp.Models.User._name", "field")
	assertSymbol(t, symbolMap, "MyApp.Models.User.Name", "property")
	assertSymbol(t, symbolMap, "MyApp.Models.User.GetAge", "method")
	assertSymbol(t, symbolMap, "MyApp.Models.User.User", "method") // constructor
}

func TestInterface(t *testing.T) {
	src := `
namespace MyApp {
    public interface IRepository {
        void Save();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "IRepository.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyApp.IRepository", "interface")
}

func TestStruct(t *testing.T) {
	src := `
namespace MyApp {
    public struct Point {
        public int X;
        public int Y;
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Point.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyApp.Point", "class") // structs stored as class
	assertHasSymbol(t, result.Symbols, "MyApp.Point.X", "field")
	assertHasSymbol(t, result.Symbols, "MyApp.Point.Y", "field")
}

func TestEnum(t *testing.T) {
	src := `
namespace MyApp {
    public enum Status { Active, Inactive }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Status.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyApp.Status", "enum")
}

func TestUsingDirectives(t *testing.T) {
	src := `
using System;
using System.Data;
namespace MyApp {
    public class Foo {}
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Foo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	if len(imports) < 2 {
		t.Fatalf("expected at least 2 import refs, got %d", len(imports))
	}
	assertRefTarget(t, imports, "System")
	assertRefTarget(t, imports, "System.Data")
}

func TestClassInheritance(t *testing.T) {
	src := `
namespace MyApp {
    public class User : BaseEntity {
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	inherits := filterRefs(result.References, "inherits")
	if len(inherits) != 1 {
		t.Fatalf("expected 1 inherits ref, got %d", len(inherits))
	}
	if inherits[0].ToName != "BaseEntity" {
		t.Errorf("expected inherits BaseEntity, got %s", inherits[0].ToName)
	}
}

func TestInterfaceImplementation(t *testing.T) {
	src := `
namespace MyApp {
    public class UserService : IUserService {
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserService.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	impls := filterRefs(result.References, "implements")
	if len(impls) != 1 {
		t.Fatalf("expected 1 implements ref, got %d", len(impls))
	}
	if impls[0].ToName != "IUserService" {
		t.Errorf("expected implements IUserService, got %s", impls[0].ToName)
	}
}

func TestMixedBaseTypes(t *testing.T) {
	src := `
namespace MyApp {
    public class User : BaseEntity, IIdentifiable, IEquatable {
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	inherits := filterRefs(result.References, "inherits")
	impls := filterRefs(result.References, "implements")

	if len(inherits) != 1 {
		t.Fatalf("expected 1 inherits ref, got %d", len(inherits))
	}
	if inherits[0].ToName != "BaseEntity" {
		t.Errorf("expected inherits BaseEntity, got %s", inherits[0].ToName)
	}
	if len(impls) != 2 {
		t.Fatalf("expected 2 implements refs, got %d", len(impls))
	}
}

func TestTableAttribute(t *testing.T) {
	src := `
using System.ComponentModel.DataAnnotations.Schema;
namespace MyApp.Models {
    [Table("Users")]
    public class User {
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	if len(tableRefs) < 1 {
		t.Fatalf("expected at least 1 uses_table ref, got %d", len(tableRefs))
	}
	assertRefTarget(t, tableRefs, "Users")
}

func TestDbSetProperty(t *testing.T) {
	src := `
namespace MyApp.Data {
    public class AppDbContext {
        public DbSet<User> Users { get; set; }
        public DbSet<Order> Orders { get; set; }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "AppDbContext.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	if len(tableRefs) < 2 {
		t.Fatalf("expected at least 2 uses_table refs, got %d", len(tableRefs))
	}
	assertRefTarget(t, tableRefs, "User")
	assertRefTarget(t, tableRefs, "Order")
}

func TestFromSqlRaw(t *testing.T) {
	src := `
namespace MyApp {
    public class UserRepo {
        public void GetUsers() {
            var users = db.Users.FromSqlRaw("SELECT * FROM Users WHERE Active = 1");
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
}

func TestDapperQuery(t *testing.T) {
	src := `
namespace MyApp {
    public class OrderRepo {
        public void GetOrders() {
            var orders = conn.Query("SELECT * FROM Orders WHERE Status = 1");
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrderRepo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Orders")
}

func TestFileScopedNamespace(t *testing.T) {
	src := `
namespace MyApp.Models;

public class Product {
    public string Name { get; set; }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Product.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyApp.Models.Product", "class")
	assertHasSymbol(t, result.Symbols, "MyApp.Models.Product.Name", "property")
}

func TestNestedClass(t *testing.T) {
	src := `
namespace MyApp {
    public class Outer {
        public class Inner {
            public void DoWork() {}
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Outer.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "MyApp.Outer", "class")
	assertHasSymbol(t, result.Symbols, "MyApp.Inner", "class")
}

func TestLanguages(t *testing.T) {
	p := New()
	langs := p.Languages()
	if len(langs) != 1 || langs[0] != "csharp" {
		t.Errorf("expected [csharp], got %v", langs)
	}
}

func TestFullSample(t *testing.T) {
	src := `
using System.Data;
namespace MyApp.Models {
    [Table("Users")]
    public class User : BaseEntity, IIdentifiable {
        public DbSet<Order> Orders { get; set; }
        public string GetName() { return Name; }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	// Class symbol
	assertHasSymbol(t, result.Symbols, "MyApp.Models.User", "class")

	// Import
	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "System.Data")

	// Inheritance
	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "BaseEntity")

	// Interface implementation
	impls := filterRefs(result.References, "implements")
	assertRefTarget(t, impls, "IIdentifiable")

	// Table refs (attribute + DbSet)
	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
	assertRefTarget(t, tableRefs, "Order")
}

func TestExecuteNonQueryProcName(t *testing.T) {
	src := `
namespace DotNetNuke.Data {
    public class DataProvider {
        public void AddUser(string name) {
            provider.ExecuteNonQuery("AddUser", name);
        }
        public void GetUser(int id) {
            provider.IDataReader("GetUser", id);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataProvider.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 2 {
		t.Fatalf("expected 2 calls refs, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
	assertRefTarget(t, callRefs, "AddUser")
	assertRefTarget(t, callRefs, "GetUser")
	// Verify dbo. qualification
	for _, r := range callRefs {
		if r.ToQualified != "dbo."+r.ToName {
			t.Errorf("expected ToQualified dbo.%s, got %s", r.ToName, r.ToQualified)
		}
	}
}

func TestExecuteWithInlineSQL(t *testing.T) {
	// When Execute is called with actual SQL, it should extract table refs not proc names
	src := `
namespace MyApp {
    public class Repo {
        public void Run() {
            conn.Execute("INSERT INTO Users (Name) VALUES (@name)");
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
	// Should NOT have calls refs since it's inline SQL
	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 0 {
		t.Errorf("expected 0 calls refs for inline SQL, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
}

func TestSqlCommandConstructor(t *testing.T) {
	src := `
namespace MyApp {
    public class UserRepo {
        public void Delete(int id) {
            var cmd = new SqlCommand("DeleteUser", conn);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 1 {
		t.Fatalf("expected 1 calls ref, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
	assertRefTarget(t, callRefs, "DeleteUser")
}

func TestSqlCommandWithSQL(t *testing.T) {
	src := `
namespace MyApp {
    public class UserRepo {
        public void Get() {
            var cmd = new SqlCommand("SELECT * FROM Users", conn);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 0 {
		t.Errorf("expected 0 calls refs for SQL in SqlCommand, got %d", len(callRefs))
	}
}

func TestCommandTextAssignment(t *testing.T) {
	src := `
namespace MyApp {
    public class UserRepo {
        public void Run() {
            cmd.CommandText = "GetAllUsers";
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 1 {
		t.Fatalf("expected 1 calls ref, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
	assertRefTarget(t, callRefs, "GetAllUsers")
}

func TestLegacyAdoCommandTypes(t *testing.T) {
	// .NET Framework-era apps commonly use OleDb/Odbc providers and DataAdapters
	src := `
using System.Data.OleDb;
namespace Legacy {
    public class Repo {
        public void Load() {
            var cmd = new OleDbCommand("GetOrders", conn);
            var da = new SqlDataAdapter("SELECT * FROM Products", conn);
            var qcmd = new System.Data.SqlClient.SqlCommand("ArchiveOrders", conn);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "GetOrders")
	assertRefTarget(t, callRefs, "ArchiveOrders")

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Products")
}

func TestSQLInLocalVariable(t *testing.T) {
	// SQL built in a string variable and assigned to CommandText later
	src := `
namespace Legacy {
    public class Repo {
        public void Load(int id) {
            string sql = "SELECT * FROM Customers WHERE Id = " + id;
            sql += " ORDER BY Name";
            cmd.CommandText = sql;
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Customers")

	// The bare identifier assignment to CommandText must not produce a calls ref
	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 0 {
		t.Errorf("expected 0 calls refs, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
}

func TestSQLVariableConcatenation(t *testing.T) {
	src := `
namespace Legacy {
    public class Repo {
        public void Load() {
            string sql = "SELECT o.OrderID, o.Total " +
                "FROM Orders o " +
                "INNER JOIN Customers c ON c.CustomerID = o.CustomerID";
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Orders")
	assertRefTarget(t, tableRefs, "Customers")
}

func TestNonSQLStringVariableIgnored(t *testing.T) {
	src := `
namespace Legacy {
    public class Repo {
        public void Load() {
            string msg = "Loaded all records from the cache";
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	if len(tableRefs) != 0 {
		t.Errorf("expected 0 table refs for UI string, got %v", refsToNames(tableRefs))
	}
}

func TestExecInInlineSQL(t *testing.T) {
	src := `
namespace MyApp {
    public class Repo {
        public void Run() {
            conn.Query("EXEC GetActiveUsers @Status = 1");
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "GetActiveUsers")
}

func TestMultipleProcNameMethods(t *testing.T) {
	src := `
namespace DotNetNuke.Data {
    public class DataProvider {
        public void DoStuff() {
            provider.ExecuteReader("GetRoles");
            provider.ExecuteScalar("CountUsers");
            provider.GetDataReader("GetPermissions");
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "DataProvider.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 3 {
		t.Fatalf("expected 3 calls refs, got %d: %v", len(callRefs), refsToNames(callRefs))
	}
	assertRefTarget(t, callRefs, "GetRoles")
	assertRefTarget(t, callRefs, "CountUsers")
	assertRefTarget(t, callRefs, "GetPermissions")
}

func TestProcNameWithDboPrefix(t *testing.T) {
	src := `
namespace MyApp {
    public class Repo {
        public void Run() {
            provider.ExecuteNonQuery("dbo.AddUser", name);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repo.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	if len(callRefs) != 1 {
		t.Fatalf("expected 1 calls ref, got %d", len(callRefs))
	}
	if callRefs[0].ToName != "AddUser" {
		t.Errorf("expected ToName AddUser (dbo. stripped), got %s", callRefs[0].ToName)
	}
	if callRefs[0].ToQualified != "dbo.AddUser" {
		t.Errorf("expected ToQualified dbo.AddUser, got %s", callRefs[0].ToQualified)
	}
}

func TestEFNavigationCollection(t *testing.T) {
	src := `
namespace MyApp.Models {
    public class Customer {
        public int Id { get; set; }
        public virtual ICollection<Order> Orders { get; set; }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Customer.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "Order")
}

func TestEFNavigationSingle(t *testing.T) {
	src := `
namespace MyApp.Models {
    public class Order {
        public int Id { get; set; }
        public virtual Customer Customer { get; set; }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Order.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "Customer")
}

func TestIncludeStringArg(t *testing.T) {
	src := `
namespace MyApp.Data {
    public class UserRepository {
        public User GetWithOrders(int id) {
            return context.Users.Include("Orders").FirstOrDefault(u => u.Id == id);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepository.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "Orders")
}

// --- helpers ---

func assertSymbol(t *testing.T, symbolMap map[string]parser.Symbol, qname, kind string) {
	t.Helper()
	sym, ok := symbolMap[qname]
	if !ok {
		t.Errorf("missing symbol %s", qname)
		return
	}
	if sym.Kind != kind {
		t.Errorf("symbol %s: expected kind %s, got %s", qname, kind, sym.Kind)
	}
}

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

func refsToNames(refs []parser.RawReference) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.ToName
	}
	return names
}

// ---------------------------------------------------------------------------
// Method invocation and type reference extraction tests
// ---------------------------------------------------------------------------

func TestMethodInvocationExtraction(t *testing.T) {
	src := `
namespace MyApp.Services {
    public class OrderService {
        private readonly IUserService _userService;

        public void ProcessOrder(int orderId) {
            var user = _userService.GetUser(orderId);
            var validator = new OrderValidator();
            validator.Validate(user);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrderService.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "GetUser")
	assertRefTarget(t, callRefs, "Validate")

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "OrderValidator")
}

func TestTypeReferenceExtraction(t *testing.T) {
	src := `
namespace MyApp.Services {
    public class UserService {
        private UserRepository _repo;
        public UserDto GetUser(UserQuery query) {
            return null;
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserService.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "UserRepository")
	assertRefTarget(t, refRefs, "UserDto")
	assertRefTarget(t, refRefs, "UserQuery")
}

func TestObjectCreationExtraction(t *testing.T) {
	src := `
namespace MyApp {
    public class Factory {
        public void Create() {
            var svc = new UserService();
            var repo = new OrderRepository();
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Factory.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	refRefs := filterRefs(result.References, "references")
	assertRefTarget(t, refRefs, "UserService")
	assertRefTarget(t, refRefs, "OrderRepository")
}

// ---------------------------------------------------------------------------
// DNN controller endpoint extraction tests
// ---------------------------------------------------------------------------

func TestDNNControllerEndpoints(t *testing.T) {
	src := `
namespace DotNetNuke.Modules.MyModule {
    public class MyModuleController : DnnApiController {
        public HttpResponseMessage GetItems() {
            return Request.CreateResponse(HttpStatusCode.OK);
        }
        public HttpResponseMessage DeleteItem(int id) {
            return Request.CreateResponse(HttpStatusCode.OK);
        }
        private void HelperMethod() {}
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "MyModuleController.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	endpointMap := make(map[string]parser.Symbol)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpointMap[s.Signature] = s
		}
	}

	if len(endpointMap) < 2 {
		t.Fatalf("expected at least 2 DNN endpoints, got %d: %v", len(endpointMap), endpointMapKeys(endpointMap))
	}

	// Convention-based routes: GET /api/{controller}/{action}
	wantRoutes := []string{
		"GET /api/mymodule/getitems",
		"GET /api/mymodule/deleteitem",
	}
	for _, want := range wantRoutes {
		if _, ok := endpointMap[want]; !ok {
			t.Errorf("missing DNN endpoint %q; have: %v", want, endpointMapKeys(endpointMap))
		}
	}
}

func TestDNNServicesApiController(t *testing.T) {
	src := `
namespace DotNetNuke.Modules {
    public class ItemController : ServicesApiController {
        public HttpResponseMessage GetItem(int id) {
            return Request.CreateResponse(HttpStatusCode.OK);
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "ItemController.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	endpointMap := make(map[string]parser.Symbol)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpointMap[s.Signature] = s
		}
	}

	if _, ok := endpointMap["GET /api/item/getitem"]; !ok {
		t.Errorf("missing DNN endpoint; have: %v", endpointMapKeys(endpointMap))
	}
}

// ---------------------------------------------------------------------------
// ASP.NET Core endpoint extraction tests
// ---------------------------------------------------------------------------

func TestCSharpEndpointExtractionBasicRoute(t *testing.T) {
	src := `
using Microsoft.AspNetCore.Mvc;

namespace MyApp.Controllers {
    [ApiController]
    [Route("api/[controller]")]
    public class OrdersController : ControllerBase {
        [HttpGet]
        public IActionResult GetAll() => Ok();

        [HttpGet("{id}")]
        public IActionResult GetById(int id) => Ok();

        [HttpPost]
        public IActionResult Create([FromBody] object body) => Ok();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrdersController.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	endpointMap := make(map[string]parser.Symbol)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpointMap[s.Signature] = s
		}
	}

	if len(endpointMap) == 0 {
		t.Fatal("expected endpoint symbols, got none")
	}

	wantRoutes := []string{
		"GET /api/orders",
		"GET /api/orders/{id}",
		"POST /api/orders",
	}
	for _, want := range wantRoutes {
		if _, ok := endpointMap[want]; !ok {
			t.Errorf("missing endpoint %q; have: %v", want, endpointMapKeys(endpointMap))
		}
	}
}

func TestCSharpEndpointExtractionVerbAttribute(t *testing.T) {
	src := `
using Microsoft.AspNetCore.Mvc;

namespace MyApp.Controllers {
    [Route("api/products")]
    public class ProductsController : ControllerBase {
        [HttpGet("{id:int}")]
        public IActionResult Get(int id) => Ok();

        [HttpPut("{id:int}")]
        public IActionResult Update(int id) => Ok();

        [HttpDelete("{id:int}")]
        public IActionResult Delete(int id) => Ok();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "ProductsController.cs", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	endpointMap := make(map[string]parser.Symbol)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpointMap[s.Signature] = s
		}
	}

	wantRoutes := []string{
		"GET /api/products/{id}",
		"PUT /api/products/{id}",
		"DELETE /api/products/{id}",
	}
	for _, want := range wantRoutes {
		if _, ok := endpointMap[want]; !ok {
			t.Errorf("missing endpoint %q; have: %v", want, endpointMapKeys(endpointMap))
		}
	}
}

func endpointMapKeys(m map[string]parser.Symbol) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
