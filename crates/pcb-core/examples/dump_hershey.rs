//! Dump ASCII Hershey glyphs as JSON for the Go port.
//! Usage: cargo run -p pcb-core --example dump_hershey > glyphs.json

fn main() {
    print!("{{");
    let mut first = true;
    for code in 0x20u32..=0x7Eu32 {
        let c = char::from_u32(code).unwrap();
        let g = pcb_core::hershey::glyph(c);
        if !first {
            print!(",");
        }
        first = false;
        print!("\"{}\":[", escape(c));
        for (si, stroke) in g.strokes.iter().enumerate() {
            if si > 0 {
                print!(",");
            }
            print!("[");
            for (pi, (x, y)) in stroke.iter().enumerate() {
                if pi > 0 {
                    print!(",");
                }
                print!("[{x},{y}]");
            }
            print!("]");
        }
        print!("]");
    }
    println!("}}");
}

fn escape(c: char) -> String {
    match c {
        '"' => "\\\"".into(),
        '\\' => "\\\\".into(),
        _ => c.to_string(),
    }
}
