package indexer

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestExtractPythonSymbols(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []Symbol
	}{
		{
			name: "basic classes and functions",
			content: `
class MyClass:
    def method(self):
        pass

def my_func(a, b):
    return a + b

async def async_func():
    pass
`,
			expected: []Symbol{
				{Name: "MyClass", Type: "class", Line: 2, Content: "class MyClass:"},
				{Name: "method", Type: "function", Line: 3, Content: "def method(self):"},
				{Name: "my_func", Type: "function", Line: 6, Content: "def my_func(a, b):"},
				{Name: "async_func", Type: "function", Line: 9, Content: "async def async_func():"},
			},
		},
		{
			name: "decorators",
			content: `
@app.route("/")
@login_required
def index():
    pass

@dataclass
class User:
    id: int
`,
			expected: []Symbol{
				{Name: "index", Type: "function", Line: 4, Content: "@app.route(\"/\")\n@login_required\ndef index():"},
				{Name: "User", Type: "class", Line: 8, Content: "@dataclass\nclass User:"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPythonSymbols(tt.content)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("extractPythonSymbols() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExtractTSSymbols(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []Symbol
	}{
		{
			name: "ts symbols",
			content: `
export interface User { id: number }
export class Service extends Base {}
export type ID = string | number;
export const calculate = (a: number) => a * 2;
function internal() {}
`,
			expected: []Symbol{
				{Name: "User", Type: "interface", Line: 2, Content: "export interface User { id: number }"},
				{Name: "Service", Type: "class", Line: 3, Content: "export class Service extends Base {}"},
				{Name: "ID", Type: "type", Line: 4, Content: "export type ID = string | number;"},
				{Name: "calculate", Type: "function", Line: 5, Content: "export const calculate = (a: number) => a * 2;"},
				{Name: "internal", Type: "function", Line: 6, Content: "function internal() {}"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTSSymbols(tt.content)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("extractTSSymbols() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExtractRustSymbols(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []Symbol
	}{
		{
			name: "rust symbols",
			content: `
pub struct Config {}
pub(crate) enum Event { Click }
pub trait Handler { fn handle(&self); }
impl Config { fn new() -> Self { Config{} } }
async fn run() {}
`,
			expected: []Symbol{
				{Name: "Config", Type: "struct", Line: 2, Content: "pub struct Config {}"},
				{Name: "Event", Type: "enum", Line: 3, Content: "pub(crate) enum Event { Click }"},
				{Name: "Handler", Type: "trait", Line: 4, Content: "pub trait Handler { fn handle(&self); }"},
				{Name: "Config", Type: "impl", Line: 5, Content: "impl Config { fn new() -> Self { Config{} } }"},
				{Name: "run", Type: "function", Line: 6, Content: "async fn run() {}"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRustSymbols(tt.content)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("extractRustSymbols() = %v, want %v", got, tt.expected)
			}
		})
	}
}
func TestExtractTSSymbols_ReactComponent(t *testing.T) {
	src := `
const Button = ({ onClick }: Props) => <button onClick={onClick} />

let handler = async (req: Request) => {}
`
	syms := extractTSSymbols(src)
	byName := map[string]Symbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}

	if _, ok := byName["Button"]; !ok {
		t.Error("Button not found")
	}
	if _, ok := byName["handler"]; !ok {
		t.Error("handler not found")
	}
}

// TestExtractSymbolsHeuristic_Dispatch verifies routing by file extension.
func TestExtractSymbolsHeuristic_Dispatch(t *testing.T) {
	cases := []struct {
		ext      string
		src      string
		wantName string // empty = expect zero results
	}{
		{".py", "def hello(): pass", "hello"},
		{".ts", "export function greet() {}", "greet"},
		{".tsx", "const Btn = () => <button />", "Btn"},
		{".js", "function init() {}", "init"},
		{".jsx", "const App = () => null", "App"},
		{".rs", "pub fn run() {}", "run"},
		{".rb", "def foo; end", ""}, // unsupported — expect no results
	}
	for _, tc := range cases {
		syms := extractSymbolsHeuristic(tc.ext, tc.src)
		if tc.wantName == "" {
			if len(syms) != 0 {
				t.Errorf("ext %q: expected no symbols, got %v", tc.ext, symbolNames(syms))
			}
			continue
		}
		found := false
		for _, s := range syms {
			if s.Name == tc.wantName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ext %q: symbol %q not found in %v", tc.ext, tc.wantName, symbolNames(syms))
		}
	}
}

// TestIndexProject_PythonSymbols verifies Python symbols are indexed via heuristic fallback.
func TestIndexProject_PythonSymbols(t *testing.T) {
	tmpDir, s := setupTest(t)

	src := `
from fastapi import APIRouter

router = APIRouter()

class UserModel:
    id: int
    name: str

@router.get("/users")
async def get_users():
    return []
`
	writeFile(t, tmpDir, "users.py", src)

	if _, err := IndexProject(context.Background(), s, nil, "", tmpDir); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ id, wantTyp string }{
		{"users.py:UserModel", "class"},
		{"users.py:get_users", "function"},
	}
	for _, tc := range cases {
		n, err := s.GetNode(tc.id)
		if err != nil {
			t.Errorf("node %q not found: %v", tc.id, err)
			continue
		}
		if n.Type != tc.wantTyp {
			t.Errorf("node %q: want type %q, got %q", tc.id, tc.wantTyp, n.Type)
		}
	}
}

// TestIndexProject_TSSymbols verifies TypeScript symbols are indexed via heuristic fallback.
func TestIndexProject_TSSymbols(t *testing.T) {
	tmpDir, s := setupTest(t)

	src := `
import { useState } from "react"

export interface LoginForm {
  email: string
  password: string
}

export const LoginPage: React.FC = () => {
  const [email, setEmail] = useState("")
  return null
}

export async function login(form: LoginForm): Promise<void> {}
`
	writeFile(t, tmpDir, "login.ts", src)

	if _, err := IndexProject(context.Background(), s, nil, "", tmpDir); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ id, wantTyp string }{
		{"login.ts:LoginForm", "interface"},
		{"login.ts:LoginPage", "function"},
		{"login.ts:login", "function"},
	}
	for _, tc := range cases {
		n, err := s.GetNode(tc.id)
		if err != nil {
			t.Errorf("node %q not found: %v", tc.id, err)
			continue
		}
		if n.Type != tc.wantTyp {
			t.Errorf("node %q: want type %q, got %q", tc.id, tc.wantTyp, n.Type)
		}
	}
}

// helpers

func symbolNames(syms []Symbol) []string {
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Name
	}
	return names
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
