package php

import (
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

func TestParseClassWithNamespace(t *testing.T) {
	input := `<?php

namespace App\Controllers;

use App\Models\User;
use App\Services\AuthService;

class UserController extends BaseController implements ControllerInterface
{
    public $name;
    const STATUS_ACTIVE = 1;

    public function index(): void
    {
        $users = User::all();
    }

    public function show(int $id): User
    {
        return User::find($id);
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserController.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	// Should have class, methods, property, constant
	var class *parser.Symbol
	var methods []parser.Symbol
	for i, s := range result.Symbols {
		switch s.Kind {
		case "class":
			class = &result.Symbols[i]
		case "method":
			methods = append(methods, s)
		}
	}

	if class == nil {
		t.Fatal("expected class symbol")
	}
	if class.Name != "UserController" {
		t.Errorf("expected UserController, got %s", class.Name)
	}
	if class.QualifiedName != `App\Controllers\UserController` {
		t.Errorf("expected App\\Controllers\\UserController, got %s", class.QualifiedName)
	}

	if len(methods) < 2 {
		t.Errorf("expected at least 2 methods, got %d", len(methods))
	}

	// Check for import refs
	importCount := 0
	for _, ref := range result.References {
		if ref.ReferenceType == "imports" {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected at least 2 import refs, got %d", importCount)
	}

	// Check inheritance refs
	hasInherits := false
	hasImplements := false
	for _, ref := range result.References {
		if ref.ReferenceType == "inherits" && ref.ToName == "BaseController" {
			hasInherits = true
		}
		if ref.ReferenceType == "implements" && ref.ToName == "ControllerInterface" {
			hasImplements = true
		}
	}
	if !hasInherits {
		t.Error("expected inherits BaseController ref")
	}
	if !hasImplements {
		t.Error("expected implements ControllerInterface ref")
	}
}

func TestParseTrait(t *testing.T) {
	input := `<?php

namespace App\Traits;

trait HasRoles
{
    public function hasRole(string $role): bool
    {
        return in_array($role, $this->roles);
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "HasRoles.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	var trait *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "class" && s.Name == "HasRoles" {
			trait = &result.Symbols[i]
			break
		}
	}
	if trait == nil {
		t.Fatal("expected trait symbol (stored as class kind)")
	}
	if trait.QualifiedName != `App\Traits\HasRoles` {
		t.Errorf("expected App\\Traits\\HasRoles, got %s", trait.QualifiedName)
	}
}

func TestParseInterface(t *testing.T) {
	input := `<?php

namespace App\Contracts;

interface Repository
{
    public function find(int $id);
    public function all(): array;
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "Repository.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	var iface *parser.Symbol
	for i, s := range result.Symbols {
		if s.Kind == "interface" {
			iface = &result.Symbols[i]
			break
		}
	}
	if iface == nil {
		t.Fatal("expected interface symbol")
	}
	if iface.QualifiedName != `App\Contracts\Repository` {
		t.Errorf("expected App\\Contracts\\Repository, got %s", iface.QualifiedName)
	}
}

func TestParseFunction(t *testing.T) {
	input := `<?php

namespace App\Helpers;

function formatDate(string $date): string
{
    return date('Y-m-d', strtotime($date));
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "helpers.php", Content: []byte(input), Language: "php"})
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
	if fn.Name != "formatDate" {
		t.Errorf("expected formatDate, got %s", fn.Name)
	}
}

func TestParseDatabaseRefs(t *testing.T) {
	input := `<?php

class UserRepository
{
    public function getActive($pdo)
    {
        $stmt = $pdo->query("SELECT * FROM users WHERE active = 1");
        return $stmt->fetchAll();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "UserRepository.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, ref := range result.References {
		if ref.ReferenceType == "uses_table" && ref.ToName == "users" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected uses_table reference to users, got refs: %+v", result.References)
	}
}

func TestParseLaravelTable(t *testing.T) {
	input := `<?php

class OrderService
{
    public function getOrders()
    {
        return DB::table('orders')->where('active', 1)->get();
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "OrderService.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, ref := range result.References {
		if ref.ReferenceType == "uses_table" && ref.ToName == "orders" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected uses_table reference to orders, got refs: %+v", result.References)
	}
}

func TestParseLaravelRoutes(t *testing.T) {
	input := `<?php

use Illuminate\Support\Facades\Route;

Route::get('/api/users', [UserController::class, 'index']);
Route::post('/api/users', [UserController::class, 'store']);
Route::resource('/api/orders', OrderController::class);
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "routes.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	endpoints := make(map[string]bool)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpoints[s.Signature] = true
		}
	}

	expected := []string{"GET /api/users", "POST /api/users", "RESOURCE /api/orders"}
	for _, e := range expected {
		if !endpoints[e] {
			t.Errorf("expected endpoint %q, got endpoints: %v", e, endpoints)
		}
	}
}

func TestParseWordPressAjax(t *testing.T) {
	input := `<?php

add_action('wp_ajax_my_search', 'handle_search');
add_action('wp_ajax_nopriv_my_search', 'handle_search');
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "ajax.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	endpoints := make(map[string]bool)
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" {
			endpoints[s.Name] = true
		}
	}

	if !endpoints["wp_ajax_my_search"] {
		t.Errorf("expected wp_ajax_my_search endpoint, got %v", endpoints)
	}
	if !endpoints["wp_ajax_nopriv_my_search"] {
		t.Errorf("expected wp_ajax_nopriv_my_search endpoint, got %v", endpoints)
	}
}

func TestParseWordPressRestRoute(t *testing.T) {
	input := `<?php

register_rest_route('wp/v2', '/posts', array(
    'methods' => 'GET',
    'callback' => 'get_posts',
));
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "rest.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, s := range result.Symbols {
		if s.Kind == "endpoint" && s.Signature == "GET /wp/v2/posts" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected endpoint GET /wp/v2/posts, got symbols: %+v", result.Symbols)
	}
}

func TestParseTraitUse(t *testing.T) {
	input := `<?php

namespace App\Models;

class User
{
    use HasRoles;
    use Notifiable;

    public function getName(): string
    {
        return $this->name;
    }
}
`
	p := New()
	result, err := p.Parse(parser.FileInput{Path: "User.php", Content: []byte(input), Language: "php"})
	if err != nil {
		t.Fatal(err)
	}

	traitUses := make(map[string]bool)
	for _, ref := range result.References {
		if ref.ReferenceType == "imports" {
			traitUses[ref.ToName] = true
		}
	}

	if !traitUses["HasRoles"] {
		t.Errorf("expected imports HasRoles, got %v", traitUses)
	}
	if !traitUses["Notifiable"] {
		t.Errorf("expected imports Notifiable, got %v", traitUses)
	}
}
