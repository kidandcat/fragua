//! Bundle crossings — the routability term pure HPWL cannot see.
//!
//! HPWL is blind to *order*. A 1×6 header sitting 2 mm from the IC bank
//! it wires to scores the same whether its pin 1 faces the IC's pin 1 or
//! its pin 6 does — the bounding boxes are identical either way. On a
//! 2-layer board the second layout is unroutable in practice: six wires
//! that have to swap places need six via pairs and a bottom-layer
//! crossbar, right where the fine-pitch escape ring already has no room.
//!
//! So we score the crossings directly. For every pair of footprints
//! joined by a *bundle* (≥ 2 point-to-point wires), project each wire's
//! two pads onto the axis PERPENDICULAR to the line joining the two
//! footprint centres and count the pairs of wires whose perpendicular
//! order disagrees between the two ends. That count is exactly the
//! number of times the ribbon has to cross itself, is invariant under
//! translating/rotating the whole pair (the axis rotates with it), and
//! costs `O(k²)` per pair for a `k`-wire bundle.
//!
//! Deterministic by construction: the result is an integer count and the
//! nets are walked through a `BTreeMap`, so no float-summation or
//! hash-order effect can leak in.

use std::collections::BTreeMap;

use pcb_core::{Board, Footprint, Id};

/// A bundle wire is a POINT-TO-POINT net: exactly two pads on the whole
/// board, one on each of the two footprints.
///
/// Fat nets are deliberately excluded. `GND`/`+3V3` are routed as trees
/// (usually swallowed by a pour), so "where does this net sit in the
/// ribbon order" is not a meaningful question for them — counting them
/// would let a GND pin's position veto a perfectly ordered signal
/// bundle. Two pads is both the cheap rule and the correct one.
const BUNDLE_NET_PADS: usize = 2;

/// Perpendicular coordinates closer than this (mm) count as "same
/// position", i.e. no crossing. Guards against float noise on pads that
/// sit exactly abreast (a two-row header wired straight across).
const ORDER_EPS_MM: f64 = 1e-6;

/// Total bundle crossings on `board`.
///
/// Public so callers (and tests) can measure the same number the SA
/// scores — the placement report carries it before/after a run.
#[must_use]
pub fn bundle_crossings(board: &Board) -> usize {
    crossings(board, None)
}

/// Crossings of every bundle that has `fp` at one end — the only part of
/// the total that moving a single footprint can change, so the SA scores
/// its per-move delta with this instead of re-counting the whole board.
pub(crate) fn footprint_bundle_crossings(board: &Board, fp: Id) -> usize {
    crossings(board, Some(std::slice::from_ref(&fp)))
}

/// Crossings of every bundle with at least one end in `ids`, counted
/// once per pair (a bundle between two listed footprints is not double
/// counted). Used by the edge planner, which moves several parts at once.
pub(crate) fn crossings_involving(board: &Board, ids: &[Id]) -> usize {
    crossings(board, Some(ids))
}

/// One wire of a bundle: the pad position (mm) at the lower-indexed
/// footprint and at the higher-indexed one, in that order.
type Wire = ([f64; 2], [f64; 2]);

/// Shared kernel. `only = None` counts every pair; `only = Some(ids)`
/// keeps the pairs with at least one end in `ids`.
fn crossings(board: &Board, only: Option<&[Id]>) -> usize {
    let fps: Vec<&Footprint> = board.footprints_in_order().collect();
    if fps.len() < 2 {
        return 0;
    }
    // Insertion-order indices keep the pair keys (and therefore the walk
    // order) independent of the footprint map's hashing. `only` is
    // resolved to those indices once, so the per-pair filter is a tiny
    // slice scan (1 element in the SA's incremental case).
    let wanted: Option<Vec<usize>> = only.map(|ids| {
        fps.iter()
            .enumerate()
            .filter(|(_, fp)| ids.contains(&fp.id))
            .map(|(i, _)| i)
            .collect()
    });
    let keep = |a: usize, b: usize| -> bool {
        match &wanted {
            None => true,
            Some(w) => w.contains(&a) || w.contains(&b),
        }
    };

    // Pass 1: collect the pads of every net, dropping any net that grows
    // past the point-to-point limit. `BTreeMap` for a deterministic walk.
    let mut net_pads: BTreeMap<&str, Vec<(usize, [f64; 2])>> = BTreeMap::new();
    for (i, fp) in fps.iter().enumerate() {
        for pad in &fp.pads {
            let Some(net) = pad.net.as_deref() else {
                continue;
            };
            let entry = net_pads.entry(net).or_default();
            // Stop at LIMIT+1 pads: that is already enough to disqualify
            // the net, and it keeps a 20-pad power rail from allocating.
            if entry.len() > BUNDLE_NET_PADS {
                continue;
            }
            let c = fp.pad_world_center(pad);
            entry.push((i, [c.x.to_mm(), c.y.to_mm()]));
        }
    }

    // Pass 2: group the surviving wires by the footprint pair they join.
    // `wires[(lo, hi)]` holds `[lo_pad, hi_pad]` positions, in the pair's
    // own index order so both ends are always read the same way round.
    let mut wires: BTreeMap<(usize, usize), Vec<Wire>> = BTreeMap::new();
    for pads in net_pads.values() {
        if pads.len() != BUNDLE_NET_PADS {
            continue;
        }
        let (ia, pa) = pads[0];
        let (ib, pb) = pads[1];
        if ia == ib {
            continue; // both pads on the same part: nothing to align
        }
        let (lo, hi, plo, phi) = if ia < ib {
            (ia, ib, pa, pb)
        } else {
            (ib, ia, pb, pa)
        };
        if !keep(lo, hi) {
            continue;
        }
        wires.entry((lo, hi)).or_default().push((plo, phi));
    }

    // Pass 3: count inversions per bundle. Footprint bbox centres are
    // memoised — a dense board hits the same part in many pairs.
    let mut centre: Vec<Option<[f64; 2]>> = vec![None; fps.len()];
    let mut total = 0usize;
    for ((lo, hi), bundle) in &wires {
        if bundle.len() < 2 {
            continue; // a single wire cannot cross anything
        }
        let Some(clo) = bbox_centre(fps[*lo], &mut centre, *lo) else {
            continue;
        };
        let Some(chi) = bbox_centre(fps[*hi], &mut centre, *hi) else {
            continue;
        };
        let dx = chi[0] - clo[0];
        let dy = chi[1] - clo[1];
        let len = (dx * dx + dy * dy).sqrt();
        if len < ORDER_EPS_MM {
            continue; // parts stacked on top of each other: no axis
        }
        // Perpendicular to the centre-to-centre axis, unit length. The
        // wires run roughly along the axis, so their ORDER across this
        // perpendicular is the ribbon order at each end.
        let px = -dy / len;
        let py = dx / len;
        // Signed perpendicular offset of each end, measured from its own
        // footprint centre so the two ends are directly comparable even
        // when the parts have wildly different sizes.
        let s: Vec<(f64, f64)> = bundle
            .iter()
            .map(|(plo, phi)| {
                (
                    (plo[0] - clo[0]) * px + (plo[1] - clo[1]) * py,
                    (phi[0] - chi[0]) * px + (phi[1] - chi[1]) * py,
                )
            })
            .collect();
        for i in 0..s.len() {
            for j in (i + 1)..s.len() {
                let da = s[i].0 - s[j].0;
                let db = s[i].1 - s[j].1;
                if da.abs() < ORDER_EPS_MM || db.abs() < ORDER_EPS_MM {
                    continue; // abreast at one end: no defined inversion
                }
                if da * db < 0.0 {
                    total += 1;
                }
            }
        }
    }
    total
}

/// Memoised pad-bbox centre of a footprint, mm.
fn bbox_centre(fp: &Footprint, cache: &mut [Option<[f64; 2]>], i: usize) -> Option<[f64; 2]> {
    if let Some(c) = cache[i] {
        return Some(c);
    }
    let b = fp.bounds()?;
    let c = [
        f64::midpoint(b.min.x.to_mm(), b.max.x.to_mm()),
        f64::midpoint(b.min.y.to_mm(), b.max.y.to_mm()),
    ];
    cache[i] = Some(c);
    Some(c)
}
