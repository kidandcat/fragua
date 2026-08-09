//! One-shot: load a .fragua and write schematic PNG.
use std::env;
use std::path::PathBuf;

fn main() {
    let mut args = env::args().skip(1);
    let project = PathBuf::from(
        args.next()
            .expect("usage: render_schematic <file.fragua> [out.png] [width]"),
    );
    let out = PathBuf::from(args.next().unwrap_or_else(|| "schematic.png".into()));
    let width: u32 = args.next().and_then(|s| s.parse().ok()).unwrap_or(2400);

    let project = pcb_core::Project::load_from_path(&project).expect("load project");
    let snap = project.read();
    let png = pcb_render::render_schematic_png(snap.schematic(), width).expect("render");
    std::fs::write(&out, png).expect("write png");
    eprintln!(
        "wrote {} ({} bytes)",
        out.display(),
        std::fs::metadata(&out).map(|m| m.len()).unwrap_or(0)
    );
}
