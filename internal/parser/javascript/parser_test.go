package javascript

import (
	"strings"
	"testing"

	"github.com/maraichr/lattice/internal/parser"
)

// --- JavaScript tests ---

func TestJSFunctionDeclaration(t *testing.T) {
	src := `function fetchUsers(page) { return []; }`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "utils.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "fetchUsers", "function")
	if result.Symbols[0].Signature != "(page)" {
		t.Errorf("expected signature (page), got %s", result.Symbols[0].Signature)
	}
}

func TestJSArrowFunction(t *testing.T) {
	src := `const greet = (name) => { return "hi " + name; };`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "greet.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "greet", "function")
}

func TestJSClassWithMethods(t *testing.T) {
	src := `
class UserService {
  constructor(config) {
    this.config = config;
  }
  async getById(id) {
    return this.fetch('/users/' + id);
  }
  get name() { return this._name; }
  set name(val) { this._name = val; }
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "service.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "UserService", "class")
	assertHasSymbol(t, result.Symbols, "UserService.constructor", "method")
	assertHasSymbol(t, result.Symbols, "UserService.getById", "method")
	assertHasSymbol(t, result.Symbols, "UserService.name", "property") // getter/setter
}

func TestJSImportFrom(t *testing.T) {
	src := `import { useState, useEffect } from 'react';`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "app.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "react")
}

func TestJSRequire(t *testing.T) {
	src := `const fs = require('fs');`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "server.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "fs")
}

func TestJSReexport(t *testing.T) {
	src := `export { bar } from './bar';`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "index.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "./bar")
}

func TestJSClassExtends(t *testing.T) {
	src := `class Foo extends Bar {}`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "foo.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Foo", "class")
	inherits := filterRefs(result.References, "inherits")
	if len(inherits) != 1 {
		t.Fatalf("expected 1 inherits ref, got %d", len(inherits))
	}
	if inherits[0].ToName != "Bar" {
		t.Errorf("expected inherits Bar, got %s", inherits[0].ToName)
	}
}

func TestJSExportedFunction(t *testing.T) {
	src := `export function processData(items) { return items; }`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "process.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "processData", "function")
}

func TestJSExportedClass(t *testing.T) {
	src := `
export class UserService extends BaseService {
  constructor(config) { super(config); }
  async getById(id) { return this.fetch('/users/' + id); }
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "service.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "UserService", "class")
	assertHasSymbol(t, result.Symbols, "UserService.constructor", "method")
	assertHasSymbol(t, result.Symbols, "UserService.getById", "method")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "BaseService")
}

func TestJSExportedArrowFunction(t *testing.T) {
	src := `export const handler = (req, res) => { res.send('ok'); };`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "handler.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "handler", "function")
}

func TestJSMixedModules(t *testing.T) {
	src := `
import { useState } from 'react';
const api = require('./api');
export class UserService extends BaseService {
  constructor(config) { super(config); }
  async getById(id) { return this.fetch('/users/' + id); }
}
export default function App() { return null; }
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "app.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "UserService", "class")
	assertHasSymbol(t, result.Symbols, "UserService.constructor", "method")
	assertHasSymbol(t, result.Symbols, "UserService.getById", "method")
	assertHasSymbol(t, result.Symbols, "App", "function")

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "react")
	assertRefTarget(t, imports, "./api")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "BaseService")
}

func TestJSLanguages(t *testing.T) {
	p := NewJS()
	langs := p.Languages()
	if len(langs) != 1 || langs[0] != "javascript" {
		t.Errorf("expected [javascript], got %v", langs)
	}
}

// --- TypeScript tests ---

func TestTSInterfaceDeclaration(t *testing.T) {
	src := `interface IUserService { getById(id: string): Promise<User>; }`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "service.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "IUserService", "interface")
}

func TestTSEnumDeclaration(t *testing.T) {
	src := `enum Role { Admin, User, Guest }`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "role.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Role", "enum")
}

func TestTSTypeAlias(t *testing.T) {
	src := `type UserID = string | number;`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "types.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "UserID", "type")
}

func TestTSClassImplements(t *testing.T) {
	src := `
class UserService implements IUserService {
  getById(id: string): Promise<User> { return null as any; }
}
`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "service.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "UserService", "class")
	assertHasSymbol(t, result.Symbols, "UserService.getById", "method")

	impls := filterRefs(result.References, "implements")
	if len(impls) != 1 {
		t.Fatalf("expected 1 implements ref, got %d", len(impls))
	}
	assertRefTarget(t, impls, "IUserService")
}

func TestTSExportedInterface(t *testing.T) {
	src := `export interface Config { host: string; port: number; }`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "config.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Config", "interface")
}

func TestTSExportedEnum(t *testing.T) {
	src := `export enum Status { Active = 'active', Inactive = 'inactive' }`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "status.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Status", "enum")
}

func TestTSExportedTypeAlias(t *testing.T) {
	src := `export type Handler = (req: Request, res: Response) => void;`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "handler.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Handler", "type")
}

func TestTSImportStatement(t *testing.T) {
	src := `import { Injectable } from '@angular/core';`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "service.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "@angular/core")
}

func TestTSFullSample(t *testing.T) {
	src := `
import { Injectable } from '@angular/core';

interface IUserService {
  getById(id: string): Promise<User>;
}

enum Role { Admin, User, Guest }

type UserID = string | number;

export class UserService implements IUserService {
  getById(id: string): Promise<User> { return null as any; }
}
`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "service.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "IUserService", "interface")
	assertHasSymbol(t, result.Symbols, "Role", "enum")
	assertHasSymbol(t, result.Symbols, "UserID", "type")
	assertHasSymbol(t, result.Symbols, "UserService", "class")
	assertHasSymbol(t, result.Symbols, "UserService.getById", "method")

	imports := filterRefs(result.References, "imports")
	assertRefTarget(t, imports, "@angular/core")

	impls := filterRefs(result.References, "implements")
	assertRefTarget(t, impls, "IUserService")
}

func TestTSLanguages(t *testing.T) {
	p := NewTS()
	langs := p.Languages()
	if len(langs) != 1 || langs[0] != "typescript" {
		t.Errorf("expected [typescript], got %v", langs)
	}
}

func TestTSClassExtendsAndImplements(t *testing.T) {
	src := `
class AdminService extends BaseService implements IAdminService {
  doAdmin(): void {}
}
`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "admin.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "AdminService", "class")

	inherits := filterRefs(result.References, "inherits")
	assertRefTarget(t, inherits, "BaseService")

	impls := filterRefs(result.References, "implements")
	assertRefTarget(t, impls, "IAdminService")
}

func TestJSDefaultExportFunction(t *testing.T) {
	src := `export default function App() { return null; }`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "app.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "App", "function")
}

func TestJSNestedClassMethods(t *testing.T) {
	src := `
class Outer {
  method1() {}
  method2(a, b) {}
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "outer.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	assertHasSymbol(t, result.Symbols, "Outer", "class")
	assertHasSymbol(t, result.Symbols, "Outer.method1", "method")
	assertHasSymbol(t, result.Symbols, "Outer.method2", "method")
}

// --- Database/ORM detection tests ---

func TestTSEntityDecorator(t *testing.T) {
	src := `
import { Entity, Column } from 'typeorm';

@Entity("users")
class User {
  @Column()
  name: string;
}
`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "user.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}

func TestJSSequelizeDefine(t *testing.T) {
	src := `
const User = sequelize.define('users', {
  name: DataTypes.STRING,
});
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "models.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}

func TestJSPoolQuery(t *testing.T) {
	src := `
async function getUsers() {
  const result = await pool.query("SELECT * FROM users WHERE active = true");
  return result.rows;
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "db.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "users")
}

func TestJSKnexQueryBuilder(t *testing.T) {
	src := `
const users = knex('customers').select('*').where('active', true);
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "query.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "customers")
}

func TestJSKnexRaw(t *testing.T) {
	src := `
const result = await knex.raw("SELECT * FROM payments WHERE amount > 100");
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "raw.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "payments")
}

func TestTSPrismaModelAccess(t *testing.T) {
	src := `
async function getUser(id: string) {
  return prisma.user.findUnique({ where: { id } });
}
`
	p := NewTS()
	result, err := p.Parse(parser.FileInput{Path: "service.ts", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	assertRefTarget(t, tableRefs, "user")
}

func TestJSConnectionExecute(t *testing.T) {
	src := `
async function insertOrder(order) {
  await connection.execute("INSERT INTO orders VALUES ($1, $2)", [order.id, order.name]);
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "orders.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "writes_to")
	assertRefTarget(t, tableRefs, "orders")
}

func TestJSNoFalsePositiveOnNonSQL(t *testing.T) {
	src := `
const result = await fetch("/api/users");
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "api.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}

	tableRefs := filterRefs(result.References, "uses_table")
	if len(tableRefs) != 0 {
		t.Errorf("expected no table refs from fetch call, got %d", len(tableRefs))
	}
}

func TestJSAPICallFetch(t *testing.T) {
	src := `
async function loadUsers() {
  const res = await fetch("/api/users");
  return res.json();
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "client.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	apiRefs := filterRefs(result.References, "calls_api")
	if len(apiRefs) == 0 {
		t.Fatal("expected at least one calls_api ref from fetch")
	}
	assertRefTarget(t, apiRefs, "/api/users")
}

func TestJSAPICallAxiosGet(t *testing.T) {
	src := `
async function getOrder(id) {
  return axios.get("/api/orders/" + id);
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "client.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	apiRefs := filterRefs(result.References, "calls_api")
	if len(apiRefs) == 0 {
		t.Fatal("expected at least one calls_api ref from axios.get")
	}
	found := false
	for _, r := range apiRefs {
		if strings.Contains(r.ToName, "/api/orders") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected calls_api ref containing /api/orders, got %v", apiRefs)
	}
}

func TestJSAPICallTemplateString(t *testing.T) {
	src := "async function fetchUser(id) { return fetch(`/api/users/${id}`); }"
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "client.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	apiRefs := filterRefs(result.References, "calls_api")
	if len(apiRefs) == 0 {
		t.Fatal("expected calls_api ref from template string fetch")
	}
	// Template ${id} should be normalised to {*}
	assertRefTarget(t, apiRefs, "/api/users/{*}")
}

func TestJSAPICallAxiosPost(t *testing.T) {
	src := `
function createOrder(data) {
  return axios.post("/api/orders", data);
}
`
	p := NewJS()
	result, err := p.Parse(parser.FileInput{Path: "client.js", Content: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	apiRefs := filterRefs(result.References, "calls_api")
	found := false
	for _, r := range apiRefs {
		if strings.Contains(r.ToName, "POST") && strings.Contains(r.ToName, "/api/orders") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected POST /api/orders ref, got %v", apiRefs)
	}
}

func TestJSAPICallJQueryAjax(t *testing.T) {
	src := `
function loadUsers() {
	$.ajax({
		url: "/api/users",
		method: "GET",
		success: function() {}
	});
}
`
	p := NewJS()
	res, _ := p.Parse(parser.FileInput{Path: "test.js", Content: []byte(src)})
	refs := filterRefs(res.References, "calls_api")
	if len(refs) != 1 {
		t.Fatalf("expected 1 calls_api, got %d", len(refs))
	}
	if refs[0].ToName != "GET /api/users" {
		t.Errorf("expected GET /api/users, got %s", refs[0].ToName)
	}
}

func TestJSAPICallDNNServiceFramework(t *testing.T) {
	src := `
function loadUsers() {
	var sf = $.ServicesFramework(123);
	$.ajax({
		url: sf.getServiceRoot('module') + 'users/list',
		method: "GET",
		success: function() {}
	});
}
`
	p := NewJS()
	res, _ := p.Parse(parser.FileInput{Path: "test.js", Content: []byte(src)})
	refs := filterRefs(res.References, "calls_api")
	if len(refs) != 1 {
		t.Fatalf("expected 1 calls_api, got %d", len(refs))
	}
	if refs[0].ToName != "GET users/list{*}" {
		t.Errorf("expected GET users/list{*}, got %s", refs[0].ToName)
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
