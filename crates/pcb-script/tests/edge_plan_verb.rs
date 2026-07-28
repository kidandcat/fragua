//! Integration test for the `edge-plan` script verb: two edge-mounted
//! headers that start on the wrong outline edges must be reported (and
//! moved) onto the edge their wiring is on, with nothing else touched.
//!
//! Same hand-rolled single-step executor pattern as `placement_delete.rs`
//! (`dispatch` is async only for the `script` / `batch` entrypoints).

use std::future::Future;
use std::sync::{Arc, Mutex, MutexGuard, OnceLock};
use std::task::{Context, Poll, Wake, Waker};

use pcb_core::Project;
use serde_json::{json, Value};

/// The on-disk library is process-global via `HOME`; serialise like the
/// other script tests do.
fn test_lock() -> MutexGuard<'static, ()> {
    static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
    LOCK.get_or_init(|| Mutex::new(()))
        .lock()
        .unwrap_or_else(|p| p.into_inner())
}

struct NoopWake;
impl Wake for NoopWake {
    fn wake(self: Arc<Self>) {}
}

fn block_on<F: Future>(fut: F) -> F::Output {
    let waker = Waker::from(Arc::new(NoopWake));
    let mut cx = Context::from_waker(&waker);
    let mut fut = Box::pin(fut);
    match fut.as_mut().poll(&mut cx) {
        Poll::Ready(v) => v,
        Poll::Pending => panic!("test future stalled — handler should be sync"),
    }
}

fn run_tool(project: &Project, tool: &str, args: Value) -> Value {
    block_on(pcb_script::tools::dispatch(project, tool, &args))
        .map_err(|e| format!("{} ({})", e.message, e.code))
        .unwrap_or_else(|e| panic!("{tool} failed: {e}"))
}

fn run_script(project: &Project, script: &str) -> Value {
    block_on(pcb_script::tools::dispatch(
        project,
        "script",
        &json!({"script": script}),
    ))
    .map_err(|e| format!("{} ({})", e.message, e.code))
    .unwrap_or_else(|e| panic!("script failed: {e}\n--script--\n{script}"))
}

fn sandbox_home(tag: &str) {
    let tmp = std::env::temp_dir().join(format!("pcb-test-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp).expect("mkdir tmp HOME");
    std::env::set_var("HOME", &tmp);
}

/// 80 × 40 mm board, two fixed 3-pad banks (left at x=12, right at x=68)
/// and two 3-pad edge-mounted headers wired to them — placed on the WRONG
/// edges to start with.
fn build_project() -> Project {
    let project = Project::new("edge-plan-test");
    run_script(&project, "outline 80 40");
    run_script(
        &project,
        "lib test_bank\n  pad 1 0 -2.54 1 1\n  pad 2 0 0 1 1\n  pad 3 0 2.54 1 1\n",
    );
    run_script(
        &project,
        "lib test_hdr\n  pad 1 -2.54 0 1 1\n  pad 2 0 0 1 1\n  pad 3 2.54 0 1 1\n",
    );
    project
        .confirm_pending_library_entry("test_bank")
        .expect("confirm bank");
    project
        .confirm_pending_library_entry("test_hdr")
        .expect("confirm header");
    // The header key is edge-mounted (any side): that is what makes it
    // visible to the planner.
    run_script(&project, "edge-mount test_hdr true");

    run_script(
        &project,
        "sym U1 ic key=test_bank\n  pin 1 L\n  pin 2 L\n  pin 3 L\n\
         sym U2 ic key=test_bank\n  pin 1 R\n  pin 2 R\n  pin 3 R\n\
         sym JL ic key=test_hdr\n  pin 1 L\n  pin 2 L\n  pin 3 L\n\
         sym JR ic key=test_hdr\n  pin 1 R\n  pin 2 R\n  pin 3 R\n",
    );
    // JL is wired to the LEFT bank U1, JR to the RIGHT bank U2.
    run_script(
        &project,
        "net L0 U1.1 JL.1\nnet L1 U1.2 JL.2\nnet L2 U1.3 JL.3\n\
         net R0 U2.1 JR.1\nnet R1 U2.2 JR.2\nnet R2 U2.3 JR.3\n",
    );
    run_script(
        &project,
        "palette U1 test_bank\npalette U2 test_bank\npalette JL test_hdr\npalette JR test_hdr\n\
         place U1 12 20\nplace U2 68 20\n",
    );
    // Headers on the wrong edges: JL (left wiring) hard against the RIGHT
    // edge, JR against the LEFT one.
    run_script(&project, "edge-place JL right\nedge-place JR left\n");
    project
}

fn centre_x(project: &Project, reference: &str) -> f64 {
    let snap = project.read();
    let fp = snap
        .board()
        .footprints_in_order()
        .find(|f| f.reference == reference)
        .expect("footprint on board");
    let b = fp.bounds().expect("bounds");
    f64::midpoint(b.min.x.to_mm(), b.max.x.to_mm())
}

#[test]
fn edge_plan_moves_headers_to_the_edge_their_wiring_is_on() {
    let _guard = test_lock();
    sandbox_home("edge-plan");
    let project = build_project();
    assert!(centre_x(&project, "JL") > 40.0, "JL starts on the right");
    assert!(centre_x(&project, "JR") < 40.0, "JR starts on the left");

    let reply = run_tool(
        &project,
        "placement.edge_plan",
        json!({"refs": ["JL", "JR"]}),
    );

    // Agents only see text: it must name the side and position per ref.
    let text = reply["content"][0]["text"].as_str().unwrap_or_default();
    assert!(
        text.contains("JL → left edge"),
        "text should report JL on the left edge, got:\n{text}"
    );
    assert!(
        text.contains("JR → right edge"),
        "text should report JR on the right edge, got:\n{text}"
    );
    // And the plan is applied to the live project, not just reported.
    assert!(
        centre_x(&project, "JL") < 40.0,
        "JL should have moved left, x={:.1}",
        centre_x(&project, "JL")
    );
    assert!(
        centre_x(&project, "JR") > 40.0,
        "JR should have moved right, x={:.1}",
        centre_x(&project, "JR")
    );
    // The fixed banks must not have moved.
    assert!((centre_x(&project, "U1") - 12.0).abs() < 0.01);
    assert!((centre_x(&project, "U2") - 68.0).abs() < 0.01);
}

#[test]
fn edge_plan_reports_refs_it_cannot_plan() {
    let _guard = test_lock();
    sandbox_home("edge-plan-skip");
    let project = build_project();
    let reply = run_tool(
        &project,
        "placement.edge_plan",
        json!({"refs": ["U1", "NOPE"]}),
    );
    let text = reply["content"][0]["text"].as_str().unwrap_or_default();
    assert!(
        text.contains("U1: not edge-mounted"),
        "should explain why U1 was skipped, got:\n{text}"
    );
    assert!(
        text.contains("NOPE: not on the board"),
        "should explain why NOPE was skipped, got:\n{text}"
    );
}

#[test]
fn edge_plan_verb_parses_from_a_script_line() {
    let _guard = test_lock();
    sandbox_home("edge-plan-script");
    let project = build_project();
    // Through the DSL this time (parse → dispatch), with the optional seed.
    let reply = run_script(&project, "edge-plan JL JR seed=7\n");
    let text = serde_json::to_string(&reply).unwrap_or_default();
    assert!(
        text.contains("left edge") && text.contains("right edge"),
        "script reply should carry the plan, got: {text}"
    );
    assert!(centre_x(&project, "JL") < 40.0);
    assert!(centre_x(&project, "JR") > 40.0);
}
