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
        .set_language(&tree_sitter_javascript::language())
        .expect("load javascript grammar");
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
        "export_statement" => {
            let mut cur = node.walk();
            for child in node.children(&mut cur) {
                collect(&child, src, out);
            }
            return;
        }
        "function_declaration" | "function" => {
            if let Some(sym) = make_sym(node, src, "function") {
                out.push(sym);
            }
            return;
        }
        "class_declaration" => {
            if let Some(sym) = make_sym(node, src, "class") {
                out.push(sym);
            }
            // descend into class body to pick up methods
        }
        "method_definition" => {
            if let Some(sym) = make_sym(node, src, "method") {
                out.push(sym);
            }
            return;
        }
        "lexical_declaration" | "variable_declaration" => {
            if let Some(sym) = arrow_sym(node, src) {
                out.push(sym);
                return;
            }
        }
        _ => {}
    }
    let mut cur = node.walk();
    for child in node.children(&mut cur) {
        collect(&child, src, out);
    }
}

fn make_sym(node: &Node, src: &str, kind: &str) -> Option<Symbol> {
    let name_node = node.child_by_field_name("name")?;
    let name = src[name_node.byte_range()].to_string();
    let line = node.start_position().row + 1;
    let content = first_line(src, node);
    let calls = if matches!(kind, "function" | "method") {
        collect_calls(node, src)
    } else {
        vec![]
    };
    Some(Symbol { name, kind: kind.to_string(), line, content, calls })
}

fn arrow_sym(node: &Node, src: &str) -> Option<Symbol> {
    let mut cur = node.walk();
    let declarators: Vec<Node> = node
        .children(&mut cur)
        .filter(|c| c.kind() == "variable_declarator")
        .collect();
    if declarators.len() != 1 {
        return None;
    }
    let decl = &declarators[0];
    let value = decl.child_by_field_name("value")?;
    if value.kind() != "arrow_function" {
        return None;
    }
    let name_node = decl.child_by_field_name("name")?;
    let name = src[name_node.byte_range()].to_string();
    let line = node.start_position().row + 1;
    let content = first_line(src, node);
    let calls = collect_calls(&value, src);
    Some(Symbol { name, kind: "function".to_string(), line, content, calls })
}

fn collect_calls(node: &Node, src: &str) -> Vec<String> {
    let mut calls = Vec::new();
    walk_calls(node, src, &mut calls);
    calls.sort_unstable();
    calls.dedup();
    calls
}

fn walk_calls(node: &Node, src: &str, out: &mut Vec<String>) {
    if node.kind() == "call_expression" {
        if let Some(func) = node.child_by_field_name("function") {
            let name = match func.kind() {
                "identifier" => Some(src[func.byte_range()].to_string()),
                "member_expression" => func
                    .child_by_field_name("property")
                    .map(|p| src[p.byte_range()].to_string()),
                _ => None,
            };
            if let Some(n) = name {
                out.push(n);
            }
        }
    }
    let mut cur = node.walk();
    for child in node.children(&mut cur) {
        walk_calls(&child, src, out);
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
