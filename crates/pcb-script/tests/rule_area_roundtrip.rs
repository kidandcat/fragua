//! Rule areas through the agent surface: declare, list, remove, and
//! survive a save/load round-trip.
//!
//! Two things are pinned here. First, the verbs answer in PLAIN TEXT
//! with the effective values — the HTTP surface hands agents nothing
//! else, so an area that only shows up in structured data is invisible.
//! Second, an area is design intent: it has to come back byte-identical
//! from the file the server autosaves, and a project written before the
//! feature existed must still load.

use std::future::Future;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, OnceLock};
use std::task::{Context, Poll, Wake, Waker};

use pcb_core::Project;
use serde_json::{json, Value};

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

/// The text an agent over HTTP actually sees: the summary line plus each
/// line's own text, exactly the way the server flattens a script reply
/// (`format_script_result`). Anything not in here is invisible to agents.
fn reply_text(reply: &Value) -> String {
    let mut out = reply["content"][0]["text"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    if let Some(results) = reply
        .pointer("/structuredContent/results")
        .and_then(Value::as_array)
    {
        for r in results {
            let body = if r["ok"].as_bool().unwrap_or(false) {
                r.pointer("/result/content/0/text")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string()
            } else {
                r["error"].as_str().unwrap_or_default().to_string()
            };
            out.push('\n');
            out.push_str(&body);
        }
    }
    out
}

fn sandbox_home(tag: &str) {
    let tmp = std::env::temp_dir().join(format!("pcb-test-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp).expect("mkdir tmp HOME");
    std::env::set_var("HOME", &tmp);
}

fn demo_project(name: &str) -> Project {
    let project = Project::new(name);
    run_script(&project, "outline 40 30");
    run_script(
        &project,
        "lib ra_two_pad\n  pad 1 -2 0 1 1\n  pad 2  2 0 1 1\n",
    );
    project
        .confirm_pending_library_entry("ra_two_pad")
        .expect("confirm library entry");
    run_script(
        &project,
        "sym U1 ic key=ra_two_pad\n  pin 1 L\n  pin 2 R\n\
         sym U2 ic key=ra_two_pad\n  pin 1 L\n  pin 2 R\n\
         net GND U1.1 U2.1\n\
         net VCC U1.2 U2.2\n",
    );
    run_script(
        &project,
        "palette U1 ra_two_pad\npalette U2 ra_two_pad\nplace U1 10 15\nplace U2 25 15\n",
    );
    project
}

fn snapshot(project: &Project, tag: &str) -> String {
    let path = std::env::temp_dir().join(format!("pcb-ra-{}-{}.json", std::process::id(), tag));
    project.save_to_path(&path).expect("serialise project");
    let bytes = std::fs::read_to_string(&path).expect("read back snapshot");
    let _ = std::fs::remove_file(&path);
    bytes
}

#[test]
fn rule_area_verbs_answer_in_text_and_edit_in_place() {
    let _guard = test_lock();
    sandbox_home("rule-area-text");
    let project = demo_project("rule-area-text");

    let reply = run_script(
        &project,
        "rule-area fine 5 5 20 20 clearance=0.13 via_dia=0.45 via_drill=0.2\n\
         rule-area-around U1 u1zone margin=1.5 clearance=0.1 layers=top priority=5\n\
         list-rule-areas\n",
    );
    let text = reply_text(&reply);
    assert!(text.contains("clearance=0.130"), "{text}");
    assert!(text.contains("via_dia=0.450"), "{text}");
    // The bbox helper reports the rect it derived, not just "ok".
    assert!(text.contains("u1zone"), "{text}");
    assert!(text.contains("layers=top"), "{text}");
    assert!(text.contains("priority=5"), "{text}");
    assert!(text.contains("2 rule area(s)"), "{text}");

    // U1 sits at (10,15) with pads at ±2 mm and 1 mm tall → bbox
    // 7.5..12.5 × 14.5..15.5, plus 1.5 mm margin.
    {
        let snap = project.read();
        let a = snap
            .board()
            .rule_areas
            .iter()
            .find(|a| a.name == "u1zone")
            .expect("area stored");
        assert!((a.rect.min.x.to_mm() - 6.0).abs() < 1e-6, "{:?}", a.rect);
        assert!((a.rect.max.y.to_mm() - 17.0).abs() < 1e-6, "{:?}", a.rect);
    }

    // Re-declaring a name edits it; an area that overrides nothing is a
    // no-op and must be refused rather than silently stored.
    let reply = run_script(&project, "rule-area fine 5 5 20 20 clearance=0.2\n");
    assert!(reply_text(&reply).contains("clearance=0.200"));
    assert_eq!(project.read().board().rule_areas.len(), 2);

    let reply = run_script(&project, "rule-area nothing 0 0 1 1\n");
    assert!(
        reply_text(&reply).contains("override something"),
        "{}",
        reply_text(&reply)
    );

    let reply = run_script(&project, "rule-area-remove fine\nlist-rule-areas\n");
    let text = reply_text(&reply);
    assert!(text.contains("removed"), "{text}");
    assert!(text.contains("1 rule area(s)"), "{text}");
}

#[test]
fn fab_rules_warn_when_an_area_asks_for_less_than_the_fab_can_build() {
    let _guard = test_lock();
    sandbox_home("rule-area-fab");
    let project = demo_project("rule-area-fab");

    let reply = run_script(
        &project,
        "fab-rules jlcpcb-2l\nrule-area tight 5 5 20 20 clearance=0.1\ndrc\n",
    );
    let text = reply_text(&reply);
    assert!(text.contains("jlcpcb-2l"), "{text}");
    // Declaration time: the agent is told immediately.
    assert!(text.contains("RuleBelowFabLimit"), "{text}");
    // And the DRC verb names the rules in force.
    assert!(text.contains("rule area: tight"), "{text}");

    let reply = run_script(&project, "fab-rules nope\n");
    assert!(reply_text(&reply).contains("unknown preset"));
}

#[test]
fn rule_areas_survive_save_and_load() {
    let _guard = test_lock();
    sandbox_home("rule-area-roundtrip");
    let project = demo_project("rule-area-roundtrip");
    run_script(
        &project,
        "fab-rules jlcpcb-2l\n\
         rule-area fine 5 5 20 20 clearance=0.13 width=0.127 via_dia=0.45 via_drill=0.2 priority=2\n\
         rule-area-around U2 u2zone margin=1.0 clearance=0.15 layers=bottom\n",
    );
    // Compare the rule state itself: the surrounding JSON has
    // hash-ordered maps whose order is not stable across a reload.
    let rules_of = |p: &Project| -> Value {
        let json: Value = serde_json::from_str(&snapshot(p, "cmp")).expect("valid json");
        json!({
            "rule_areas": json["board"]["rule_areas"].clone(),
            "fab_rules": json["board"]["fab_rules"].clone(),
        })
    };
    let before = rules_of(&project);

    let path = std::env::temp_dir().join(format!("pcb-ra-file-{}.fragua", std::process::id()));
    project.save_to_path(&path).expect("save");
    let reloaded = Project::load_from_path(Path::new(&path)).expect("load");
    let after = rules_of(&reloaded);
    let _ = std::fs::remove_file(&path);

    assert_eq!(before, after, "rule areas did not survive save/load");
    assert!(before["rule_areas"]
        .as_array()
        .is_some_and(|a| a.len() == 2));
    let snap = reloaded.read();
    assert_eq!(snap.board().rule_areas.len(), 2);
    assert_eq!(
        snap.board().fab_rules.as_ref().map(|f| f.preset.as_str()),
        Some("jlcpcb-2l")
    );
}

#[test]
fn projects_written_before_rule_areas_still_load() {
    let _guard = test_lock();
    sandbox_home("rule-area-legacy");
    let path = std::env::temp_dir().join(format!("pcb-ra-legacy-{}.fragua", std::process::id()));
    // Shape of a pre-feature file: no `rule_areas`, no `fab_rules`.
    std::fs::write(
        &path,
        r#"{"name":"legacy","board":{"outline":null,"footprints":{},"footprint_order":[],
            "traces":[],"vias":[]},"schematic":{"symbols":{},"symbol_order":[],"nets":{},
            "net_classes":{},"net_to_class":{},"sub_sheets":[],"ports":[]}}"#,
    )
    .expect("write legacy file");
    let project = Project::load_from_path(Path::new(&path)).expect("legacy project must load");
    let _ = std::fs::remove_file(&path);
    assert!(project.read().board().rule_areas.is_empty());
    assert!(project.read().board().fab_rules.is_none());
}
