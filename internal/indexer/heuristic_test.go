package indexer

import (
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
