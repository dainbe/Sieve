use serde::Serialize;
use std::alloc::{alloc, dealloc, Layout};
use tree_sitter::{Node, Parser};

#[derive(Serialize)]
struct Symbol {
    #[serde(rename = "Name")]
    name: String,
    #[serde(rename = "Type")]
    kind: String,
    #[serde(rename = "Line")]
    line: usize,
    #[serde(rename = "Content")]
    content: String,
    #[serde(rename = "Calls")]
    calls: Vec<String>,
}

#[no_mangle]
pub extern "C" fn malloc(size: u32) -> *mut u8 {
    if size == 0 {
        return std::ptr::null_mut();
    }
    let layout = Layout::from_size_align(size as usize, 1).unwrap();
    unsafe { alloc(layout) }
}

#[no_mangle]
pub extern "C" fn free(ptr: *mut u8) {
    if ptr.is_null() {
        return;
    }
    // size-1 layout; allocator tracks real size internally.
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

    let symbols = extract(source);
    encode_json(symbols)
}

fn extract(source: &str) -> Vec<Symbol> {
    let mut parser = Parser::new();
    parser
        .set_language(&tree_sitter_python::language())
        .expect("load python grammar");

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
        "function_definition" | "async_function_definition" => {
            if let Some(sym) = make_symbol(node, src, "function") {
                out.push(sym);
            }
            return; // don't descend into nested functions
        }
        "class_definition" => {
            if let Some(sym) = make_symbol(node, src, "class") {
                out.push(sym);
            }
            // descend to capture methods
        }
        _ => {}
    }

    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        collect(&child, src, out);
    }
}

fn make_symbol(node: &Node, src: &str, kind: &str) -> Option<Symbol> {
    let name_node = node.child_by_field_name("name")?;
    let name = &src[name_node.byte_range()];
    let line = node.start_position().row + 1;

    // content = first non-empty line of the node (signature)
    let node_text = &src[node.byte_range()];
    let content = node_text
        .lines()
        .find(|l| !l.trim().is_empty())
        .unwrap_or("")
        .trim_end()
        .to_string();

    let calls = if kind == "function" {
        extract_calls(node, src)
    } else {
        vec![]
    };

    Some(Symbol {
        name: name.to_string(),
        kind: kind.to_string(),
        line,
        content,
        calls,
    })
}

fn extract_calls(node: &Node, src: &str) -> Vec<String> {
    let mut calls = Vec::new();
    collect_calls(node, src, &mut calls);
    calls.sort_unstable();
    calls.dedup();
    calls
}

fn collect_calls(node: &Node, src: &str, out: &mut Vec<String>) {
    if node.kind() == "call" {
        if let Some(func) = node.child_by_field_name("function") {
            // bare call: foo() → func kind = "identifier"
            // method call: self.foo() → func kind = "attribute", last child = "identifier"
            let name = match func.kind() {
                "identifier" => Some(&src[func.byte_range()]),
                "attribute" => func
                    .child_by_field_name("attribute")
                    .map(|n| &src[n.byte_range()]),
                _ => None,
            };
            if let Some(n) = name {
                out.push(n.to_string());
            }
        }
    }
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        collect_calls(&child, src, out);
    }
}

fn encode_json(symbols: Vec<Symbol>) -> *mut u8 {
    let json = match serde_json::to_string(&symbols) {
        Ok(j) => j,
        Err(_) => return std::ptr::null_mut(),
    };
    let mut bytes = json.into_bytes();
    bytes.push(0); // null terminator
    let layout = Layout::from_size_align(bytes.len(), 1).unwrap();
    unsafe {
        let out = alloc(layout);
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), out, bytes.len());
        out
    }
}
