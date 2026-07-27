//! Stackup round-trip invariance (stress campaign issue **O9**).
//!
//! `layer add In1.Cu` + `layer add In2.Cu` + `layer remove In2.Cu` +
//! `layer remove In1.Cu` on a 2-layer board must be a **true no-op**:
//! every byte of persisted project state (board, stackup, pours,
//! keepouts, footprint order, schematic classes) has to come back
//! identical, because the router keys off all of it. If any of it
//! drifted, two routes of "the same" board would silently disagree.
//!
//! The tests below pin that property both on a synthetic board (fast,
//! hermetic) and on the real RP2040 stress board (the one that
//! surfaced O9), by serialising the project through the very same
//! `save_to_path` path the server autosaves with and diffing the bytes.

use std::future::Future;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, OnceLock};
use std::task::{Context, Poll, Wake, Waker};

use pcb_core::Project;
use serde_json::{json, Value};

/// Tests here mutate the process-global `HOME` to sandbox the on-disk
/// library; serialise them like the other integration tests do.
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
        Poll::Pending => panic!("script future stalled — this codepath is sync"),
    }
}

fn run_script(project: &Project, script: &str) -> Value {
    block_on(pcb_script::tools::dispatch(
        project,
        "script",
        &json!({"script": script}),
    ))
    .unwrap_or_else(|e| panic!("script failed: {}\n--script--\n{script}", e.message))
}

/// Every line of the reply must be `ok` — a silently refused
/// `layer remove` would make the round-trip vacuous.
fn assert_all_lines_ok(reply: &Value, label: &str) {
    let lines = reply["structuredContent"]["results"]
        .as_array()
        .unwrap_or_else(|| panic!("{label}: no per-line results in reply: {reply}"));
    for line in lines {
        assert_eq!(
            line["ok"].as_bool(),
            Some(true),
            "{label}: line {} ({}) failed: {}",
            line["line"],
            line["tool"],
            line["result"]
        );
    }
}

const ROUND_TRIP: &str = "layer add In1.Cu signal\n\
                          layer add In2.Cu signal\n\
                          clear-route\n\
                          layer remove In2.Cu\n\
                          layer remove In1.Cu\n";

/// Serialise the project exactly the way the server persists it.
fn snapshot(project: &Project, tag: &str) -> String {
    let path = std::env::temp_dir().join(format!("pcb-o9-{}-{}.json", std::process::id(), tag));
    project.save_to_path(&path).expect("serialise project");
    let bytes = std::fs::read_to_string(&path).expect("read back snapshot");
    let _ = std::fs::remove_file(&path);
    bytes
}

#[test]
fn layer_round_trip_leaves_project_state_byte_identical() {
    let _guard = test_lock();
    let tmp = std::env::temp_dir().join(format!("pcb-test-o9-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp).expect("mkdir tmp HOME");
    std::env::set_var("HOME", &tmp);

    let project = Project::new("o9-round-trip");
    run_script(&project, "outline 40 30 radius=1.5");
    run_script(
        &project,
        "lib o9_two_pad\n  pad 1 -2 0 1 1\n  pad 2  2 0 1 1\n",
    );
    project
        .confirm_pending_library_entry("o9_two_pad")
        .expect("confirm library entry");
    // A ground class with pours on both outer layers plus a power class:
    // pours and class overrides are the state the router reads back.
    run_script(
        &project,
        "class ground pour=both\n\
         class power width=0.35 clearance=0.2\n\
         sym U1 ic key=o9_two_pad\n  pin 1 L\n  pin 2 R\n\
         sym U2 ic key=o9_two_pad\n  pin 1 L\n  pin 2 R\n\
         net GND U1.1 U2.1 class=ground\n\
         net VCC U1.2 U2.2 class=power\n",
    );
    run_script(
        &project,
        "palette U1 o9_two_pad\npalette U2 o9_two_pad\nplace U1 10 15\nplace U2 25 15\n",
    );
    // Pours + a keepout: both carry explicit layer references, which is
    // exactly what a mis-indexed stackup edit would corrupt.
    run_script(
        &project,
        "pour GND top\npour GND bottom\nkeepout add 30,5 38,5 38,12 30,12 label=antenna\n",
    );

    let before = snapshot(&project, "before");
    let reply = run_script(&project, ROUND_TRIP);
    assert_all_lines_ok(&reply, "round-trip");
    let after = snapshot(&project, "after");

    assert_eq!(
        before, after,
        "layer add/remove round-trip mutated persisted project state (O9)"
    );
    // Belt and braces: the in-memory stackup is back to the 2-layer default.
    let snap = project.read();
    assert_eq!(snap.board().stackup, pcb_core::LayerStackup::default());
}

/// A pour (or keepout) living on an inner layer must block that
/// layer's removal, exactly like a trace does. Otherwise the pour
/// survives the round-trip pointing at a layer index that no longer
/// exists — persisted state the router silently reads back on later
/// runs. Regression for the O9 investigation.
#[test]
fn layer_remove_refuses_while_a_pour_sits_on_the_layer() {
    let _guard = test_lock();
    let tmp = std::env::temp_dir().join(format!("pcb-test-o9-pour-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp).expect("mkdir tmp HOME");
    std::env::set_var("HOME", &tmp);

    let project = Project::new("o9-inner-pour");
    run_script(&project, "outline 20 20");
    run_script(
        &project,
        "layer add In1.Cu signal\nlayer add In2.Cu signal\n",
    );
    // `in1` == the third copper layer == the In1.Cu just added.
    run_script(&project, "pour GND in1\n");

    let reply = run_script(&project, "layer remove In1.Cu\n");
    let line = &reply["structuredContent"]["results"][0];
    assert_eq!(
        line["ok"].as_bool(),
        Some(false),
        "removing a layer under a pour must fail, got {reply}"
    );
    assert_eq!(
        project.read().board().stackup.layer_count(),
        4,
        "the refused removal must leave the stackup untouched"
    );

    // Drop the pour and the removal goes through, restoring the 2-layer
    // stackup byte-for-byte.
    run_script(&project, "clear-pour GND in1\n");
    let reply = run_script(&project, "layer remove In2.Cu\nlayer remove In1.Cu\n");
    assert_all_lines_ok(&reply, "removal after clearing the pour");
    assert_eq!(
        project.read().board().stackup,
        pcb_core::LayerStackup::default()
    );
}

/// Same property on the board that actually surfaced O9. Skipped (not
/// failed) if the stress project is absent from the checkout.
#[test]
fn stress_board_layer_round_trip_is_a_noop() {
    let _guard = test_lock();
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../stress/rp2040-minimal.fragua");
    let Ok(path) = path.canonicalize() else {
        eprintln!("stress board not present — skipping");
        return;
    };
    let Some(project) = Project::load_from_path(&path) else {
        eprintln!("stress board unreadable — skipping");
        return;
    };
    // Re-bind autosave away from the checked-in stress file: the
    // snapshots below must never write over it.
    project.set_save_path(None);

    run_script(&project, "clear-route");
    let before = snapshot(&project, "stress-before");
    let reply = run_script(&project, ROUND_TRIP);
    assert_all_lines_ok(&reply, "stress round-trip");
    let after = snapshot(&project, "stress-after");

    assert_eq!(
        before, after,
        "layer round-trip mutated the RP2040 stress board state (O9)"
    );
}
