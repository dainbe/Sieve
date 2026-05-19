use serde::Serialize;
use std::alloc::{alloc, dealloc, Layout};
use tree_sitter::{Node, Parser};

#[derive(Serialize)]
struct Symbol {
    #[serde(rename = "Name")]    name:    String,
    #[serde(rename = "Type")]    kind:    String,
    #[serde(rename = "Line")]    line:    usize,
    #[serde(rename = "Content")] content: String,
    #[serde(rename = "Calls")]   calls:   Vec<String>,
}

#[no_mangle]
pub extern "C" fn sieve_malloc(size: u32) -> *mut u8 {
    if size == 0 { return std::ptr::null_mut(); }
    let layout = Layout::from_size_align(size as usize, 1).unwrap();
    unsafe { alloc(layout) }
}

#[no_mangle]
pub extern "C" fn sieve_free(ptr: *mut u8) {
    if ptr.is_null() { return; }
    let layout = Layout::from_size_align(1, 1).unwrap();
    unsafe { dealloc(ptr, layout) }
}

#[no_mangle]
pub extern "C" fn parse(ptr: *const u8, len: u32) -> *mut u8 {
    let src = unsafe { std::slice::from_raw_parts(ptr, len as usize) };
    let source = match std::str::from_utf8(src) {
        Ok(s) => s,
        Err(_) => return std::ptr::null_mut(),
    };
    encode_json(extract(source))
}

fn extract(source: &str) -> Vec<Symbol> {
    let mut parser = Parser::new();
    parser
        .set_language(&tree_sitter_rust::language())
        .expect("load rust grammar");
    let tree = match parser.parse(source, None) {
        Some(t) => t,
        None => return vec![],
    };
    let mut out = Vec::new();
    collect(&tree.root_node(), source, &mut out);
    out
}

fn collect(node: &Node, src: &str, out: &mut Vec<Symbol>) {
    match node.kind() {
        "function_item" => {
            if let Some(sym) = make_sym(node, src, "function", "name") {
                out.push(sym);
            }
            return;
        }
        "struct_item" => {
            if let Some(sym) = make_sym(node, src, "struct", "name") {
                out.push(sym);
            }
            return;
        }
        "enum_item" => {
            if let Some(sym) = make_sym(node, src, "enum", "name") {
                out.push(sym);
            }
            return;
        }
        "trait_item" => {
            if let Some(sym) = make_sym(node, src, "trait", "name") {
                out.push(sym);
            }
            // descend into trait body to pick up method signatures
        }
        "impl_item" => {
            if let Some(sym) = impl_sym(node, src) {
                out.push(sym);
            }
            // descend into impl block to pick up methods
        }
        "type_item" => {
            if let Some(sym) = make_sym(node, src, "type_alias", "name") {
                out.push(sym);
            }
            return;
        }
        _ => {}
    }
    let mut cur = node.walk();
    for child in node.children(&mut cur) {
        collect(&child, src, out);
    }
}

fn make_sym(node: &Node, src: &str, kind: &str, name_field: &str) -> Option<Symbol> {
    let name_node = node.child_by_field_name(name_field)?;
    let name = src[name_node.byte_range()].to_string();
    let line = node.start_position().row + 1;
    let content = first_line(src, node);
    let calls = if kind == "function" {
        collect_calls(node, src)
    } else {
        vec![]
    };
    Some(Symbol { name, kind: kind.to_string(), line, content, calls })
}

// impl blocks: extract the type name from the "type" field.
fn impl_sym(node: &Node, src: &str) -> Option<Symbol> {
    let type_node = node.child_by_field_name("type")?;
    let name = src[type_node.byte_range()].to_string();
    let line = node.start_position().row + 1;
    let content = first_line(src, node);
    Some(Symbol { name, kind: "impl".to_string(), line, content, calls: vec![] })
}

fn collect_calls(node: &Node, src: &str) -> Vec<String> {
    let mut calls = Vec::new();
    walk_calls(node, src, &mut calls);
    calls.sort_unstable();
    calls.dedup();
    calls
}

fn walk_calls(node: &Node, src: &str, out: &mut Vec<String>) {
    match node.kind() {
        // foo(args)
        "call_expression" => {
            if let Some(func) = node.child_by_field_name("function") {
                if let Some(name) = call_name(func, src) {
                    out.push(name);
                }
            }
        }
        // receiver.method(args)
        "method_call_expression" => {
            if let Some(method) = node.child_by_field_name("name") {
                out.push(src[method.byte_range()].to_string());
            }
        }
        _ => {}
    }
    let mut cur = node.walk();
    for child in node.children(&mut cur) {
        walk_calls(&child, src, out);
    }
}

fn call_name(func: Node, src: &str) -> Option<String> {
    match func.kind() {
        "identifier" => Some(src[func.byte_range()].to_string()),
        // path expressions: foo::bar → take last segment
        "scoped_identifier" => func
            .child_by_field_name("name")
            .map(|n| src[n.byte_range()].to_string()),
        "field_expression" => func
            .child_by_field_name("field")
            .map(|f| src[f.byte_range()].to_string()),
        _ => None,
    }
}

fn first_line(src: &str, node: &Node) -> String {
    let text = &src[node.byte_range()];
    text.lines()
        .find(|l| !l.trim().is_empty())
        .unwrap_or("")
        .trim_end()
        .to_string()
}

fn encode_json(symbols: Vec<Symbol>) -> *mut u8 {
    let json = match serde_json::to_string(&symbols) {
        Ok(j) => j,
        Err(_) => return std::ptr::null_mut(),
    };
    let mut bytes = json.into_bytes();
    bytes.push(0);
    let layout = Layout::from_size_align(bytes.len(), 1).unwrap();
    unsafe {
        let out = alloc(layout);
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), out, bytes.len());
        out
    }
}
