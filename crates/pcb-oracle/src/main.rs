//! Headless oracle for Fragua algorithmic parity.
//!
//! Emits a stable JSON document describing DRC, ERC, board geometry, and
//! optional route metrics so a Go port can be compared field-by-field.
//!
//! Usage:
//!   cargo run -p pcb-oracle --release -- <project.fragua> [--route] [--out path.json]

use std::collections::BTreeMap;
use std::fs;
use std::path::PathBuf;

use pcb_core::Project;
use pcb_drc::DrcOptions;
use pcb_erc::ErcOptions;
use pcb_router::{route, Outcome, RouteOptions};
use serde::Serialize;
use sha2::{Digest, Sha256};

#[derive(Serialize)]
struct OracleDump {
    engine: &'static str,
    path: String,
    geometry: Geometry,
    drc: CheckReport,
    erc: CheckReport,
    #[serde(skip_serializing_if = "Option::is_none")]
    route: Option<RouteDump>,
    #[serde(skip_serializing_if = "Option::is_none")]
    place: Option<PlaceDump>,
    #[serde(skip_serializing_if = "Option::is_none")]
    copper_hash: Option<String>,
}

#[derive(Serialize)]
struct Geometry {
    footprints: usize,
    traces: usize,
    vias: usize,
    pours: usize,
    nets: usize,
    outline_mm: Option<[f64; 2]>,
}

#[derive(Serialize)]
struct CheckReport {
    errors: usize,
    warnings: usize,
    /// kind → count (sorted by BTreeMap serialisation)
    by_kind: BTreeMap<String, usize>,
    /// Normalised findings for deep compare (sorted).
    findings: Vec<Finding>,
}

#[derive(Serialize, Clone)]
struct Finding {
    kind: String,
    severity: String,
    net: String,
    /// Rounded to 0.01 mm for stable compare.
    x_mm: f64,
    y_mm: f64,
    /// Involved refs sorted.
    involved: Vec<String>,
}

#[derive(Serialize)]
struct RouteDump {
    failed: usize,
    ok: usize,
    skipped: usize,
    traces: usize,
    vias: usize,
    total_length_mm: f64,
    iterations: u32,
    per_net: BTreeMap<String, String>,
    drc_errors: usize,
    drc_warnings: usize,
    drc_by_kind: BTreeMap<String, usize>,
}

#[derive(Serialize)]
struct PlaceDump {
    initial_hpwl_mm: f64,
    final_hpwl_mm: f64,
    positions: BTreeMap<String, [f64; 3]>, // x_mm, y_mm, rot
}

fn main() {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    if args.is_empty() {
        eprintln!("usage: pcb-oracle <project.fragua> [--route] [--place] [--out file.json]");
        std::process::exit(2);
    }
    let mut do_route = false;
    let mut do_place = false;
    let mut out: Option<PathBuf> = None;
    let mut path = PathBuf::new();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--route" => do_route = true,
            "--place" => do_place = true,
            "--out" => {
                i += 1;
                out = args.get(i).map(PathBuf::from);
            }
            s if !s.starts_with('-') && path.as_os_str().is_empty() => {
                path = PathBuf::from(s);
            }
            other => {
                eprintln!("unknown arg: {other}");
                std::process::exit(2);
            }
        }
        i += 1;
    }
    if path.as_os_str().is_empty() {
        eprintln!("missing project path");
        std::process::exit(2);
    }

    let project = Project::load_from_path(&path).unwrap_or_else(|| {
        eprintln!("failed to load {}", path.display());
        std::process::exit(1);
    });
    let snap = project.read();
    let board = snap.board();
    let sch = snap.schematic();

    let drc = pcb_drc::run(board, &DrcOptions::default());
    let mut drc_by = BTreeMap::new();
    let mut drc_findings = Vec::new();
    for v in &drc.violations {
        *drc_by.entry(format!("{:?}", v.kind)).or_default() += 1;
        let mut involved = v.involved.clone();
        involved.sort();
        let net = involved
            .iter()
            .find(|s| !s.contains('.'))
            .cloned()
            .unwrap_or_default();
        drc_findings.push(Finding {
            kind: kind_snake(&format!("{:?}", v.kind)),
            severity: format!("{:?}", v.severity).to_ascii_lowercase(),
            net,
            x_mm: round2(v.x_mm),
            y_mm: round2(v.y_mm),
            involved,
        });
    }
    sort_findings(&mut drc_findings);

    let erc = pcb_erc::run(board, sch, &ErcOptions::default());
    let mut erc_by = BTreeMap::new();
    let mut erc_findings = Vec::new();
    for v in &erc.violations {
        *erc_by.entry(format!("{:?}", v.kind)).or_default() += 1;
        let mut involved = v.involved.clone();
        involved.sort();
        erc_findings.push(Finding {
            kind: kind_snake(&format!("{:?}", v.kind)),
            severity: format!("{:?}", v.severity).to_ascii_lowercase(),
            net: involved.first().cloned().unwrap_or_default(),
            x_mm: 0.0,
            y_mm: 0.0,
            involved,
        });
    }
    sort_findings(&mut erc_findings);

    let outline_mm = board.outline.map(|o| {
        [
            (o.max.x - o.min.x).to_mm(),
            (o.max.y - o.min.y).to_mm(),
        ]
    });

    let mut route_dump = None;
    let mut place_dump = None;
    let mut copper_hash = Some(hash_copper(board));

    if do_place {
        let mut b = board.clone();
        let refs: Vec<String> = b
            .footprints_in_order()
            .filter(|fp| !fp.edge_mounted)
            .map(|fp| fp.reference.clone())
            .collect();
        let mut opts = pcb_placer::PlaceOptions {
            seed: 42,
            global_stage: false,
            edge_plan: false,
            decouple: false,
            congestion_resolution: 0,
            congestion_penalty_factor: 0.0,
            crossing_penalty_factor: 0.0,
            ..pcb_placer::PlaceOptions::default()
        };
        opts.max_iterations = 8000;
        let report = pcb_placer::place(&mut b, &refs, &opts, &Default::default())
            .unwrap_or_else(|e| {
                eprintln!("place failed: {e}");
                std::process::exit(1);
            });
        let mut positions = BTreeMap::new();
        for fp in b.footprints_in_order() {
            positions.insert(
                fp.reference.clone(),
                [
                    fp.position.x.to_mm(),
                    fp.position.y.to_mm(),
                    f64::from(fp.rotation),
                ],
            );
        }
        place_dump = Some(PlaceDump {
            initial_hpwl_mm: report.initial_hpwl_mm,
            final_hpwl_mm: report.final_hpwl_mm,
            positions,
        });
    }

    if do_route {
        // Clear and re-route for process parity of the route verb.
        let mut b = board.clone();
        b.traces.clear();
        b.vias.clear();
        let mut opts = RouteOptions::default();
        opts.max_seconds = Some(30.0);
        let report = route(&mut b, &opts);
        let mut per = BTreeMap::new();
        let mut ok = 0usize;
        let mut failed = 0usize;
        let skipped = 0usize;
        for (name, outcome) in &report.per_net {
            let status = match outcome {
                Outcome::Ok { .. } => {
                    ok += 1;
                    "ok"
                }
                Outcome::Failed { .. } => {
                    failed += 1;
                    "failed"
                }
            };
            per.insert(name.clone(), status.to_string());
        }
        copper_hash = Some(hash_copper(&b));
        let post = pcb_drc::run(&b, &DrcOptions::default());
        let mut drc_by = BTreeMap::new();
        for v in &post.violations {
            *drc_by.entry(format!("{:?}", v.kind)).or_default() += 1;
        }
        route_dump = Some(RouteDump {
            failed,
            ok,
            skipped,
            traces: b.traces.len(),
            vias: b.vias.len(),
            total_length_mm: report.total_length_mm,
            iterations: report.iterations as u32,
            per_net: per,
            drc_errors: post.error_count,
            drc_warnings: post.warning_count,
            drc_by_kind: drc_by,
        });
    }

    let dump = OracleDump {
        engine: "rust",
        path: path.display().to_string(),
        geometry: Geometry {
            footprints: board.footprints.len(),
            traces: board.traces.len(),
            vias: board.vias.len(),
            pours: board.pours.len(),
            nets: sch.nets.len(),
            outline_mm,
        },
        drc: CheckReport {
            errors: drc.error_count,
            warnings: drc.warning_count,
            by_kind: drc_by,
            findings: drc_findings,
        },
        erc: CheckReport {
            errors: erc.error_count,
            warnings: erc.warning_count,
            by_kind: erc_by,
            findings: erc_findings,
        },
        route: route_dump,
        place: place_dump,
        copper_hash,
    };

    let json = serde_json::to_string_pretty(&dump).expect("serialize");
    if let Some(p) = out {
        fs::write(&p, &json).expect("write out");
        eprintln!("wrote {}", p.display());
    } else {
        println!("{json}");
    }
}

fn round2(v: f64) -> f64 {
    (v * 100.0).round() / 100.0
}

fn kind_snake(s: &str) -> String {
    // PascalCase → snake_case
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if c.is_uppercase() {
            if i > 0 {
                out.push('_');
            }
            out.push(c.to_ascii_lowercase());
        } else {
            out.push(c);
        }
    }
    out
}

fn sort_findings(v: &mut [Finding]) {
    v.sort_by(|a, b| {
        a.kind
            .cmp(&b.kind)
            .then(a.severity.cmp(&b.severity))
            .then(a.net.cmp(&b.net))
            .then(a.x_mm.partial_cmp(&b.x_mm).unwrap_or(std::cmp::Ordering::Equal))
            .then(a.y_mm.partial_cmp(&b.y_mm).unwrap_or(std::cmp::Ordering::Equal))
            .then_with(|| a.involved.cmp(&b.involved))
    });
}

fn hash_copper(board: &pcb_core::Board) -> String {
    let mut hasher = Sha256::new();
    let mut traces: Vec<_> = board.traces.iter().collect();
    traces.sort_by_key(|t| {
        (
            t.net.clone(),
            t.layer.index,
            t.start.x.0,
            t.start.y.0,
            t.end.x.0,
            t.end.y.0,
            t.width.0,
        )
    });
    for t in traces {
        hasher.update(t.net.as_bytes());
        hasher.update(t.layer.index.to_le_bytes());
        hasher.update(t.start.x.0.to_le_bytes());
        hasher.update(t.start.y.0.to_le_bytes());
        hasher.update(t.end.x.0.to_le_bytes());
        hasher.update(t.end.y.0.to_le_bytes());
        hasher.update(t.width.0.to_le_bytes());
    }
    let mut vias: Vec<_> = board.vias.iter().collect();
    vias.sort_by_key(|v| (v.net.clone(), v.position.x.0, v.position.y.0, v.drill.0));
    for v in vias {
        hasher.update(v.net.as_bytes());
        hasher.update(v.position.x.0.to_le_bytes());
        hasher.update(v.position.y.0.to_le_bytes());
        hasher.update(v.drill.0.to_le_bytes());
        hasher.update(v.diameter.0.to_le_bytes());
    }
    hex::encode(hasher.finalize())
}
