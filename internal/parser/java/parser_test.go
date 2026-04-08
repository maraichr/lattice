package java

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestBasicClass(t *testing.T) {
	src := `
package com.example;

import java.util.List;

public class User {
    private String name;
    public String getName() { return name; }
    public User() {}
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "com.example.User", "class")
	assertHasSymbol(t, result.Symbols, "com.example.User.getName", "method")
	assertHasRef(t, result.References, "java.util.List", "imports")
}

func TestEntityAnnotation(t *testing.T) {
	src := `
package com.example;

@Entity
@Table(name = "users")
public class User {
    @Id
    private Long id;
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}

func TestQueryAnnotation(t *testing.T) {
	src := `
package com.example;

public interface UserRepository {
    @Query("SELECT u FROM Users u WHERE u.active = true")
    List<User> findActiveUsers();
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepository.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
}

func TestJDBCPrepareStatement(t *testing.T) {
	src := `
package com.example;

public class UserDao {
    public User getById(int id) {
        PreparedStatement ps = conn.prepareStatement("SELECT * FROM users WHERE id = ?");
        return mapResult(ps.executeQuery());
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserDao.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}

func TestJDBCPrepareCall(t *testing.T) {
	src := `
package com.example;

public class UserDao {
    public void callProc() {
        CallableStatement cs = conn.prepareCall("EXEC dbo.GetUser ?");
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserDao.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "dbo.GetUser")
}

func TestSpringDataRepository(t *testing.T) {
	src := `
package com.example;

public interface UserRepository extends JpaRepository<User, Long> {
    List<User> findByEmailAndStatus(String email, String status);
    long countByStatus(String status);
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepository.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	// Should have uses_table for User (from JpaRepository<User, Long>)
	assertRefTarget(t, tableRefs, "User")

	// Derived query methods should also reference User
	count := 0
	for _, r := range tableRefs {
		if r.ToName == "User" {
			count++
		}
	}
	// 1 from JpaRepository<User, Long> + 2 from derived query methods
	if count < 3 {
		t.Errorf("expected at least 3 uses_table refs for User, got %d", count)
	}
}

func TestNamedQuery(t *testing.T) {
	src := `
package com.example;

@NamedQuery(name = "User.findAll", query = "SELECT u FROM Users u")
public class User {
    private String name;
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "Users")
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

func assertHasRef(t *testing.T, refs []parser.RawReference, toName, refType string) {
	t.Helper()
	for _, r := range refs {
		if (r.ToName == toName || r.ToQualified == toName) && r.ReferenceType == refType {
			return
		}
	}
	t.Errorf("missing ref %s (%s)", toName, refType)
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

// ---------------------------------------------------------------------------
// Spring MVC endpoint extraction tests
// ---------------------------------------------------------------------------

func TestJavaSpringEndpointBasic(t *testing.T) {
	src := `
package com.example;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/api/orders")
public class OrderController {
    @GetMapping
    public List<Order> getAll() { return null; }

    @GetMapping("/{id}")
    public Order getById(@PathVariable Long id) { return null; }

    @PostMapping
    public Order create(@RequestBody Order order) { return null; }

    @DeleteMapping("/{id}")
    public void delete(@PathVariable Long id) {}
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrderController.java", Content: []byte(src)})
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
		"DELETE /api/orders/{id}",
	}
	for _, want := range wantRoutes {
		if _, ok := endpointMap[want]; !ok {
			t.Errorf("missing endpoint %q; have: %v", want, endpointKeys(endpointMap))
		}
	}
}

func TestJavaSpringEndpointNoController(t *testing.T) {
	src := `
package com.example;

public class UserService {
    public User getUser(Long id) { return null; }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserService.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			t.Errorf("unexpected endpoint symbol %q on non-controller class", s.QualifiedName)
		}
	}
}

func endpointKeys(m map[string]parser.Symbol) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ---------------------------------------------------------------------------
// Nested class support tests
// ---------------------------------------------------------------------------

func TestNestedInnerClass(t *testing.T) {
	src := `
package com.example;

public class Outer {
    public void outerMethod() {}

    public static class Inner {
        public void innerMethod() {}
    }

    public interface Callback {
    }

    public enum Status {
        ACTIVE, INACTIVE
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Outer.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "com.example.Outer", "class")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.outerMethod", "method")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Inner", "class")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Inner.innerMethod", "method")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Callback", "interface")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Status", "enum")
}

func TestDeeplyNestedClass(t *testing.T) {
	src := `
package com.example;

public class Outer {
    public static class Middle {
        public static class Inner {
            public void deepMethod() {}
        }
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Outer.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "com.example.Outer", "class")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Middle", "class")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Middle.Inner", "class")
	assertHasSymbol(t, result.Symbols, "com.example.Outer.Middle.Inner.deepMethod", "method")
}

// ---------------------------------------------------------------------------
// Method call extraction tests
// ---------------------------------------------------------------------------

func TestMethodCallExtraction(t *testing.T) {
	src := `
package com.example;

public class OrderService {
    public void processOrder(Order order) {
        validate(order);
        orderRepository.save(order);
        notificationService.sendConfirmation(order);
        System.out.println("done");
    }

    private void validate(Order order) {
        order.toString();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrderService.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")

	// Should include non-trivial calls
	assertRefTarget(t, callRefs, "validate")
	assertRefTarget(t, callRefs, "save")
	assertRefTarget(t, callRefs, "sendConfirmation")

	// Should NOT include common methods
	for _, r := range callRefs {
		if r.ToName == "println" || r.ToName == "toString" {
			t.Errorf("common method %q should be filtered from call refs", r.ToName)
		}
	}

	// Verify FromSymbol is the enclosing method
	for _, r := range callRefs {
		if r.ToName == "validate" && r.FromSymbol != "com.example.OrderService.processOrder" {
			t.Errorf("expected FromSymbol com.example.OrderService.processOrder, got %s", r.FromSymbol)
		}
	}
}

func TestMethodCallSkipsJDBC(t *testing.T) {
	src := `
package com.example;

public class UserDao {
    public void getUser() {
        conn.prepareStatement("SELECT * FROM users");
        helper.doSomething();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserDao.java", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(result.References, "calls")
	assertRefTarget(t, callRefs, "doSomething")

	// prepareStatement should NOT appear in calls (filtered + handled by JDBC)
	for _, r := range callRefs {
		if r.ToName == "prepareStatement" {
			t.Error("prepareStatement should not appear in calls refs")
		}
	}

	// JDBC detection should still produce uses_table
	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}
