//! Topological routing engine — the full rubber-band approach.
//!
//! Where the grid engine searches a raster, this engine works with the
//! board's *topology*: a constrained Delaunay triangulation (CDT) of
//! the pad sites on each layer. A route is first found as a HOMOTOPY
//! CLASS — the sequence of triangulation edges the connection crosses,
//! chosen by A* over the triangulation's dual graph with capacity
//! costs — and only then realised geometrically: waypoints are placed
//! inside the free window of every crossed edge, the polyline is
//! tightened by the same rubber-band string-pulling the organic pass
//! uses, and the result is validated against the exact clearance model
//! (other-net pads, traces, vias, outline) before any copper is
//! committed. If the realisation collides, the crossed edges are
//! penalised and the next-best homotopy class is tried — the geometric
//! validator, not the capacity bookkeeping, is the final arbiter, so
//! the engine can never emit a short.
//!
//! Layer/via model (v1): a connection is tried entirely on Top, then
//! entirely on Bottom (through-hole pads reach both layers natively),
//! then on Bottom between two freshly-planned ESCAPE VIAS next to the
//! SMD endpoints. No mid-route via splits yet — on the IoT-class
//! boards fragua targets, endpoint escapes cover the real need.
//!
//! Deterministic: sites are inserted in board order, A* ties break on
//! indices, and there is no RNG anywhere.

use std::collections::{BTreeMap, BinaryHeap, HashMap, HashSet};

use pcb_core::{Board, CopperLayer, Id, Length, Trace, Via};
use spade::{DelaunayTriangulation, HasPosition, InsertionError, Point2, Triangulation};

use crate::organic::{self, dist, to_mm, to_point, Obstacles, P2};

/// One same-layer stretch of a multilayer plan: layer index, crossed
/// CDT edges (as site pairs), visited dual faces, and the via spot at
/// the leg's end (`None` on the final leg).
type Leg = (usize, Vec<(usize, usize)>, Vec<usize>, Option<P2>);
use crate::router::{effective_net_rules, RouteOptions};

/// A CDT vertex: position plus the index of the `Site` it represents.
struct TopoVertex {
    pos: Point2<f64>,
    site: usize,
}

impl HasPosition for TopoVertex {
    type Scalar = f64;
    fn position(&self) -> Point2<f64> {
        self.pos
    }
}

/// One obstacle site on a layer: a pad (or via) centre with a
/// conservative copper radius and its net.
#[derive(Clone)]
struct Site {
    pos: P2,
    /// World-frame half extents of the copper rect (via: radius on both
    /// axes). The reach along a direction is the rect's support
    /// function — far tighter than a half-diagonal for the elongated
    /// pads modules are made of.
    half: P2,
}

impl Site {
    /// Copper reach from the centre along unit direction `d`.
    fn reach(&self, d: P2) -> f64 {
        d[0].abs() * self.half[0] + d[1].abs() * self.half[1]
    }
}

/// Per-layer triangulation context, rebuilt whenever a site is added
/// (escape vias). Rebuilds are microseconds at PCB site counts and
/// keep every handle fresh.
struct LayerCdt {
    cdt: DelaunayTriangulation<TopoVertex>,
    /// Dense dual graph derived from the CDT after every rebuild —
    /// keeps the A* free of spade handle plumbing.
    centroid: Vec<P2>,
    /// dense face -> [(dense neighbour, crossed edge as site pair)].
    adj: Vec<Vec<(usize, (usize, usize))>>,
    /// spade inner-face handle index -> dense index.
    dense_of: HashMap<usize, usize>,
    /// Extra copper already committed across an undirected site pair
    /// (sum of width + clearance of every crossing connection). Keyed
    /// by SITE indices so it survives rebuilds.
    used: HashMap<(usize, usize), f64>,
    /// Homotopy-retry penalties, reset per connection.
    penalty: HashMap<(usize, usize), f64>,
}

/// The engine's working state for one `route` call.
pub(crate) struct TopoEngine<'a> {
    board: &'a mut Board,
    opts: &'a RouteOptions,
    sites: Vec<Site>,
    layers: [LayerCdt; 2],
    /// Nets whose committed copper blocked the latest failed
    /// realisations — candidates for targeted rip-up.
    blockers: Vec<String>,
}

/// A connection endpoint: a position plus which layers can host
/// copper there (a TH pad or via reaches both, an SMD pad its own
/// layer, a committed trace point its trace's layer).
#[derive(Clone, Copy)]
struct EndPt {
    pos: P2,
    top: bool,
    bottom: bool,
}

/// Outcome for one net, mirroring the grid driver's shape.
pub(crate) struct NetResult {
    pub net: String,
    pub ok: bool,
    pub reason: String,
    pub length_mm: f64,
    pub lower_bound_mm: f64,
    pub trace_segments: usize,
    pub vias: usize,
}

/// Route every net topologically. Returns per-net outcomes; copper is
/// written straight onto `board` as it is committed.
pub(crate) fn route_all(board: &mut Board, opts: &RouteOptions) -> Vec<NetResult> {
    board.clear_routing();
    let Some(outline) = board.outline else {
        return Vec::new();
    };
    // Collect pads per net (same shape as the grid driver).
    let mut nets: BTreeMap<String, Vec<(P2, bool, CopperLayer)>> = BTreeMap::new();
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            let Some(net) = &pad.net else { continue };
            let c = to_mm(fp.pad_world_center(pad));
            nets.entry(net.clone())
                .or_default()
                .push((c, pad.drill.is_some(), pad.layer));
        }
    }
    nets.retain(|_, pads| pads.len() >= 2);

    // Easy nets first: fewest pads, then shortest spread — the same
    // ordering instinct as the grid driver.
    let mut order: Vec<String> = nets.keys().cloned().collect();
    order.sort_by(|a, b| {
        let (pa, pb) = (&nets[a], &nets[b]);
        pa.len()
            .cmp(&pb.len())
            .then_with(|| spread(pa).total_cmp(&spread(pb)))
            .then_with(|| a.cmp(b))
    });

    // Ordering negotiation: a greedy first pass can starve latecomers
    // (their channels are already booked). On failures, retry the whole
    // board with the starving nets FIRST — full re-routes are
    // milliseconds at this scale — and keep the best round's copper
    // (snapshotted, not replayed: rip-up decisions are path-dependent).
    let mut best: Option<(Vec<NetResult>, Vec<Trace>, Vec<Via>)> = None;
    for _round in 0..5 {
        board.clear_routing();
        let mut engine = TopoEngine::build(board, opts, outline);
        // Queue with targeted rip-up: when a net starves because some
        // committed trace sits in its way, rip that net's copper, let
        // the starving net pass, and re-route the ripped one at the
        // back of the queue (each net is ripped at most twice — enough
        // to negotiate, never a livelock).
        let mut queue: std::collections::VecDeque<String> = order.iter().cloned().collect();
        let mut ripped: HashMap<String, usize> = HashMap::new();
        let mut by_net: HashMap<String, NetResult> = HashMap::new();
        while let Some(net) = queue.pop_front() {
            let pads = &nets[&net];
            engine.blockers.clear();
            let res = engine.route_net(&net, pads);
            if !res.ok {
                let victim = engine
                    .blockers
                    .iter()
                    .find(|b| {
                        *b != &net
                            && by_net.contains_key(*b)
                            && ripped.get(*b).copied().unwrap_or(0) < 3
                    })
                    .cloned();
                if let Some(victim) = victim {
                    // Undo the failing net's partial copper too, then
                    // retry it right away with the corridor open.
                    engine.rip_net(&net);
                    engine.rip_net(&victim);
                    *ripped.entry(victim.clone()).or_default() += 1;
                    by_net.remove(&victim);
                    queue.push_front(net);
                    queue.push_back(victim);
                    continue;
                }
            }
            by_net.insert(net.clone(), res);
        }
        let mut results: Vec<NetResult> = Vec::new();
        for net in &order {
            if let Some(r) = by_net.remove(net) {
                results.push(r);
            }
        }
        let fails: Vec<String> = results
            .iter()
            .filter(|r| !r.ok)
            .map(|r| r.net.clone())
            .collect();
        let better = match &best {
            None => true,
            Some((b, _, _)) => {
                let bf = b.iter().filter(|r| !r.ok).count();
                let bl: f64 = b.iter().map(|r| r.length_mm).sum();
                let nl: f64 = results.iter().map(|r| r.length_mm).sum();
                fails.len() < bf || (fails.len() == bf && nl < bl)
            }
        };
        if better {
            best = Some((results, board.traces.clone(), board.vias.clone()));
        }
        if fails.is_empty() {
            break;
        }
        // Failed nets first next round, relative order kept otherwise.
        order.sort_by_key(|n| usize::from(!fails.contains(n)));
    }
    let (results, traces, vias) = best.unwrap();
    board.traces = traces;
    board.vias = vias;
    results
}

fn spread(pads: &[(P2, bool, CopperLayer)]) -> f64 {
    let (mut minx, mut miny, mut maxx, mut maxy) = (
        f64::INFINITY,
        f64::INFINITY,
        f64::NEG_INFINITY,
        f64::NEG_INFINITY,
    );
    for (p, _, _) in pads {
        minx = minx.min(p[0]);
        miny = miny.min(p[1]);
        maxx = maxx.max(p[0]);
        maxy = maxy.max(p[1]);
    }
    (maxx - minx) + (maxy - miny)
}

impl<'a> TopoEngine<'a> {
    fn build(board: &'a mut Board, opts: &'a RouteOptions, outline: pcb_core::Rect) -> Self {
        let mut sites = Vec::new();
        for fp in board.footprints_in_order() {
            for pad in &fp.pads {
                let c = to_mm(fp.pad_world_center(pad));
                let (w, h) = fp.pad_world_size(pad);
                sites.push(Site {
                    pos: c,
                    half: [w.to_mm() / 2.0, h.to_mm() / 2.0],
                });
            }
        }
        let mut engine = Self {
            board,
            opts,
            sites,
            layers: [LayerCdt::empty(), LayerCdt::empty()],
            blockers: Vec::new(),
        };
        let _ = outline;
        engine.rebuild_cdts();
        engine
    }

    fn rebuild_cdts(&mut self) {
        // v1 keeps one CDT shared by both layers (sites are pads, and a
        // pad is an obstacle *somewhere*; the exact per-layer blocking
        // is the geometric validator's job). The `used` bookkeeping IS
        // per layer.
        for l in &mut self.layers {
            l.cdt = DelaunayTriangulation::new();
            for (i, s) in self.sites.iter().enumerate() {
                let v = TopoVertex {
                    pos: Point2::new(s.pos[0], s.pos[1]),
                    site: i,
                };
                let _ignored: Result<_, InsertionError> = l.cdt.insert(v);
            }
            l.derive_dual();
        }
    }

    /// Route one net Prim-style — but the "connected set" is every
    /// point of copper committed so far (trunk sharing), not just the
    /// pads: a 7-pad power net taps its own tree instead of dragging
    /// yet another full-length run through the same channel.
    fn route_net(&mut self, net: &str, pads: &[(P2, bool, CopperLayer)]) -> NetResult {
        let (width, clearance) = effective_net_rules(self.opts, net);
        let w_mm = width.to_mm();
        let clr = clearance.to_mm();

        let ep = |p: &(P2, bool, CopperLayer)| EndPt {
            pos: p.0,
            top: p.1 || p.2.is_top(),
            bottom: p.1 || !p.2.is_top(),
        };
        let mut attach: Vec<EndPt> = vec![ep(&pads[0])];
        let mut remaining: Vec<EndPt> = pads[1..].iter().map(ep).collect();
        let mut total_len = 0.0;
        let mut lower_bound = 0.0;
        let mut segments = 0usize;
        let mut vias = 0usize;

        while !remaining.is_empty() {
            // Nearest (attachment, remaining) pair.
            let (mut bi, mut bj, mut bd) = (0usize, 0usize, f64::INFINITY);
            for (i, c) in attach.iter().enumerate() {
                for (j, r) in remaining.iter().enumerate() {
                    let d = dist(c.pos, r.pos);
                    if d < bd {
                        (bi, bj, bd) = (i, j, d);
                    }
                }
            }
            let from = attach[bi];
            let to = remaining.remove(bj);
            lower_bound += bd;

            match self.route_connection(net, from, to, w_mm, clr) {
                Ok(done) => {
                    total_len += done.length_mm;
                    segments += done.segments;
                    vias += done.vias;
                    attach.push(to);
                    // Every vertex of the committed legs becomes an
                    // attachment point for the rest of the net.
                    for (layer, pts) in &done.legs {
                        for p in pts {
                            attach.push(EndPt {
                                pos: *p,
                                top: layer.is_top(),
                                bottom: !layer.is_top(),
                            });
                        }
                    }
                    for v in &done.via_points {
                        attach.push(EndPt {
                            pos: *v,
                            top: true,
                            bottom: true,
                        });
                    }
                }
                Err(reason) => {
                    return NetResult {
                        net: net.to_string(),
                        ok: false,
                        reason: format!(
                            "no path from ({:.2}, {:.2}) to ({:.2}, {:.2}) mm: {reason}",
                            from.pos[0], from.pos[1], to.pos[0], to.pos[1]
                        ),
                        length_mm: total_len,
                        lower_bound_mm: lower_bound,
                        trace_segments: segments,
                        vias,
                    };
                }
            }
        }
        NetResult {
            net: net.to_string(),
            ok: true,
            reason: String::new(),
            length_mm: total_len,
            lower_bound_mm: lower_bound,
            trace_segments: segments,
            vias,
        }
    }

    /// Try one 2-point connection with the MULTILAYER homotopy search:
    /// A* over (face, layer) nodes where a via is just another move.
    /// Falls back to the endpoint escape plan for anything the planner
    /// misses.
    fn route_connection(
        &mut self,
        net: &str,
        from: EndPt,
        to: EndPt,
        w_mm: f64,
        clr: f64,
    ) -> Result<Committed, String> {
        if let Some(plan) = self.find_and_realise_ml(net, from, to, w_mm, clr) {
            return Ok(plan);
        }
        Err("no clear homotopy on either layer (vias included)".into())
    }

    /// Multilayer homotopy + realisation with penalised retries.
    fn find_and_realise_ml(
        &mut self,
        net: &str,
        from: EndPt,
        to: EndPt,
        w_mm: f64,
        clr: f64,
    ) -> Option<Committed> {
        let via_r = self.opts.via_diameter.to_mm() / 2.0;
        let obs = [
            self.validation_obstacles(net, CopperLayer::Top, w_mm, clr),
            self.validation_obstacles(net, CopperLayer::Bottom, w_mm, clr),
        ];
        for l in &mut self.layers {
            l.penalty.clear();
        }
        for attempt in 0..6 {
            let plan = self.ml_astar(from, to, w_mm, clr, via_r, &obs)?;
            let strategy = attempt % 3;
            // Realise leg by leg; a leg is the run between layer flips.
            let mut legs: Vec<(CopperLayer, Vec<P2>)> = Vec::new();
            let mut vias: Vec<P2> = Vec::new();
            let mut all_clear = true;
            let mut cursor = from.pos;
            for (li, crossed, faces, via_spot) in &plan {
                let layer = if *li == 0 {
                    CopperLayer::Top
                } else {
                    CopperLayer::Bottom
                };
                let target = via_spot.unwrap_or(to.pos);
                let waypoints = self.layers[*li].waypoints(
                    &self.sites,
                    crossed,
                    faces,
                    cursor,
                    target,
                    w_mm,
                    clr,
                    strategy,
                );
                let repaired = repair_chain(&waypoints, &obs[*li], w_mm / 2.0, clr)
                    .unwrap_or_else(|| waypoints.clone());
                let pulled = organic::string_pull(&repaired, &obs[*li], w_mm / 2.0, clr);
                if !obs[*li].polyline_clear(&pulled, w_mm / 2.0, clr) {
                    if let Some(b) = obs[*li].first_blocking_net(&pulled, w_mm / 2.0, clr) {
                        if b != net && !self.blockers.contains(&b) {
                            self.blockers.push(b);
                        }
                    }
                    if std::env::var("TOPO_DEBUG").is_ok() {
                        eprintln!(
                            "TOPO ml-realise-fail {net} L{li} attempt {attempt}: {}",
                            obs[*li]
                                .first_violation(&pulled, w_mm / 2.0, clr)
                                .unwrap_or_default()
                        );
                    }
                    for e in crossed {
                        *self.layers[*li].penalty.entry(*e).or_default() += 25.0;
                    }
                    all_clear = false;
                    break;
                }
                if let Some(v) = via_spot {
                    vias.push(*v);
                    cursor = *v;
                }
                legs.push((layer, pulled));
            }
            if all_clear {
                // Book the crossings.
                for (li, crossed, _, _) in &plan {
                    for e in crossed {
                        *self.layers[*li].used.entry(*e).or_default() += w_mm + clr;
                    }
                }
                return Some(self.commit(net, &legs, &vias, w_mm));
            }
        }
        None
    }

    /// A* over (face, layer). Returns the plan as legs: per leg the
    /// layer index, crossed edges, visited faces, and the via spot at
    /// the leg's END (None for the final leg).
    #[allow(clippy::items_after_statements)]
    fn ml_astar(
        &self,
        from: EndPt,
        to: EndPt,
        w_mm: f64,
        clr: f64,
        via_r: f64,
        obs: &[Obstacles; 2],
    ) -> Option<Vec<Leg>> {
        // Node = face * 2 + layer. Both layers share one triangulation,
        // so face indices line up.
        let nf = self.layers[0].centroid.len();
        if nf == 0 || self.layers[1].centroid.len() != nf {
            return None;
        }
        let f_from = self.layers[0].face_containing(from.pos)?;
        let f_to = self.layers[0].face_containing(to.pos)?;
        let start_layers: Vec<usize> = [(0, from.top), (1, from.bottom)]
            .iter()
            .filter(|(_, ok)| *ok)
            .map(|(l, _)| *l)
            .collect();
        let goal_ok = [to.top, to.bottom];

        // Via cost in mm-equivalent — the same spirit as the grid
        // engine's via_cost knob (which multiplies a cell step).
        let via_cost_mm = 0.25 * f64::from(self.opts.via_cost);

        #[derive(PartialEq)]
        struct Open {
            f: f64,
            node: usize,
        }
        impl Eq for Open {}
        impl Ord for Open {
            fn cmp(&self, other: &Self) -> std::cmp::Ordering {
                other
                    .f
                    .total_cmp(&self.f)
                    .then_with(|| other.node.cmp(&self.node))
            }
        }
        impl PartialOrd for Open {
            fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
                Some(self.cmp(other))
            }
        }

        #[derive(Clone, Copy)]
        enum Step {
            Cross((usize, usize)),
            Flip(P2),
        }

        let n = nf * 2;
        let mut g = vec![f64::INFINITY; n];
        let mut came: Vec<Option<(usize, Step)>> = vec![None; n];
        let mut heap = BinaryHeap::new();
        for &l in &start_layers {
            let node = f_from * 2 + l;
            g[node] = 0.0;
            heap.push(Open { f: 0.0, node });
        }
        let mut closed: HashSet<usize> = HashSet::new();
        let mut goal_node = None;

        while let Some(Open { node, .. }) = heap.pop() {
            let (face, layer) = (node / 2, node % 2);
            if face == f_to && goal_ok[layer] {
                goal_node = Some(node);
                break;
            }
            if !closed.insert(node) {
                continue;
            }
            let lc = &self.layers[layer];
            // Cross an edge on this layer.
            for &(nb, key) in &lc.adj[face] {
                let (sa, sb) = key;
                let pa = self.sites[sa].pos;
                let pb = self.sites[sb].pos;
                let len = dist(pa, pb);
                if len < 1e-9 {
                    continue;
                }
                let d = [(pb[0] - pa[0]) / len, (pb[1] - pa[1]) / len];
                let used = lc.used.get(&key).copied().unwrap_or(0.0);
                let free = len - self.sites[sa].reach(d) - self.sites[sb].reach(d) - used;
                let need = w_mm + 2.0 * clr;
                if free < 0.5 * need {
                    continue;
                }
                let squeeze = if free < need {
                    8.0 * (need - free)
                } else {
                    0.0
                };
                let penalty = lc.penalty.get(&key).copied().unwrap_or(0.0);
                let step =
                    dist(lc.centroid[face], lc.centroid[nb]) + 0.3 * used + squeeze + penalty;
                let nnode = nb * 2 + layer;
                let cand = g[node] + step;
                if cand < g[nnode] {
                    g[nnode] = cand;
                    came[nnode] = Some((node, Step::Cross(key)));
                    heap.push(Open {
                        f: cand + dist(lc.centroid[nb], to.pos),
                        node: nnode,
                    });
                }
            }
            // Flip layers here if a via barrel fits near the centroid.
            let other = 1 - layer;
            let nnode = face * 2 + other;
            if g[node] + via_cost_mm < g[nnode] {
                if let Some(spot) = find_via_spot(lc.centroid[face], via_r, clr, &obs[0], &obs[1]) {
                    let cand = g[node] + via_cost_mm;
                    if cand < g[nnode] {
                        g[nnode] = cand;
                        came[nnode] = Some((node, Step::Flip(spot)));
                        heap.push(Open {
                            f: cand + dist(spot, to.pos),
                            node: nnode,
                        });
                    }
                }
            }
        }

        let goal = goal_node?;
        // Walk back, splitting into legs at flips.
        let mut rev: Vec<(usize, Step)> = Vec::new();
        let mut cur = goal;
        while let Some((prev, step)) = came[cur] {
            rev.push((cur, step));
            cur = prev;
        }
        rev.reverse();
        let mut plan: Vec<Leg> = Vec::new();
        let mut leg_layer = cur % 2;
        let mut edges: Vec<(usize, usize)> = Vec::new();
        let mut faces: Vec<usize> = vec![cur / 2];
        for (node, step) in rev {
            match step {
                Step::Cross(e) => {
                    edges.push(e);
                    faces.push(node / 2);
                }
                Step::Flip(spot) => {
                    plan.push((
                        leg_layer,
                        std::mem::take(&mut edges),
                        std::mem::take(&mut faces),
                        Some(spot),
                    ));
                    leg_layer = node % 2;
                    faces = vec![node / 2];
                }
            }
        }
        plan.push((leg_layer, edges, faces, None));
        Some(plan)
    }

    /// Exact clearance model for `net` on `layer` — everything already
    /// on the board (committed topo copper included, since it IS board
    /// copper by now).
    fn validation_obstacles(
        &self,
        net: &str,
        layer: CopperLayer,
        _w_mm: f64,
        clr: f64,
    ) -> Obstacles {
        let outline = self.board.outline.expect("engine built with outline");
        let rules = |o: &RouteOptions, n: &str| effective_net_rules(o, n);
        // Same resolver the grid engine and DRC use, so the topological
        // validator honours rule areas too.
        let schematic = self.opts.schematic.clone();
        let resolver = pcb_core::RuleResolver::new(
            &self.board.rule_areas,
            pcb_core::RuleDefaults {
                clearance: self.opts.clearance,
                trace_width: self.opts.trace_width,
                via_diameter: self.opts.via_diameter,
                via_drill: self.opts.via_drill,
            },
        )
        .with_schematic(schematic.as_deref());
        // The half-width is folded in at check time by the
        // `polyline_clear` callers — the obstacle set is width-agnostic.
        organic::collect_obstacles(
            self.board, net, layer, self.opts, &rules, clr, outline, &resolver,
        )
    }

    /// Targeted rip-up: remove a net's committed copper so a starving
    /// net can pass; the ripped net goes back in the queue. Capacity
    /// bookkeeping is reset (the validator stays exact regardless).
    fn rip_net(&mut self, net: &str) {
        self.board.traces.retain(|t| t.net != net);
        self.board.vias.retain(|v| v.net != net);
        for l in &mut self.layers {
            l.used.clear();
            l.penalty.clear();
        }
    }

    /// Write a realised connection onto the board.
    fn commit(
        &mut self,
        net: &str,
        legs: &[(CopperLayer, Vec<P2>)],
        vias: &[P2],
        w_mm: f64,
    ) -> Committed {
        let mut out = Committed {
            legs: legs.to_vec(),
            via_points: vias.to_vec(),
            ..Committed::default()
        };
        for (layer, pts) in legs {
            for w in pts.windows(2) {
                if dist(w[0], w[1]) < 1e-6 {
                    continue;
                }
                self.board.traces.push(Trace {
                    id: Id::new(),
                    layer: *layer,
                    start: to_point(w[0]),
                    end: to_point(w[1]),
                    width: Length::from_mm(w_mm),
                    net: net.to_string(),
                });
                out.segments += 1;
                out.length_mm += dist(w[0], w[1]);
            }
        }
        for v in vias {
            self.board.vias.push(Via {
                id: Id::new(),
                position: to_point(*v),
                diameter: self.opts.via_diameter,
                drill: self.opts.via_drill,
                net: net.to_string(),
            });
            out.vias += 1;
        }
        out
    }
}

#[derive(Default)]
struct Committed {
    length_mm: f64,
    segments: usize,
    vias: usize,
    /// The realised legs, for trunk-sharing attachment points.
    legs: Vec<(CopperLayer, Vec<P2>)>,
    via_points: Vec<P2>,
}

impl LayerCdt {
    fn empty() -> Self {
        Self {
            cdt: DelaunayTriangulation::new(),
            centroid: Vec::new(),
            adj: Vec::new(),
            dense_of: HashMap::new(),
            used: HashMap::new(),
            penalty: HashMap::new(),
        }
    }

    /// Rebuild the dense dual graph from the fresh CDT.
    fn derive_dual(&mut self) {
        self.centroid.clear();
        self.adj.clear();
        self.dense_of.clear();
        for f in self.cdt.inner_faces() {
            let dense = self.centroid.len();
            self.dense_of.insert(f.fix().index(), dense);
            let vs = f.vertices();
            let mut c = [0.0, 0.0];
            for v in &vs {
                c[0] += v.position().x;
                c[1] += v.position().y;
            }
            self.centroid.push([c[0] / 3.0, c[1] / 3.0]);
            self.adj.push(Vec::new());
        }
        for f in self.cdt.inner_faces() {
            let dense = self.dense_of[&f.fix().index()];
            for edge in f.adjacent_edges() {
                let Some(nb) = edge.rev().face().as_inner() else {
                    continue; // hull edge: crossing would leave the board
                };
                let nb_dense = self.dense_of[&nb.fix().index()];
                let (sa, sb) = (edge.from().data().site, edge.to().data().site);
                self.adj[dense].push((nb_dense, (sa.min(sb), sa.max(sb))));
            }
        }
    }

    /// Dense index of the inner face containing (or nearest to) `p`.
    fn face_containing(&self, p: P2) -> Option<usize> {
        use spade::PositionInTriangulation as Pos;
        let spade_ix = match self.cdt.locate(Point2::new(p[0], p[1])) {
            Pos::OnFace(f) => Some(f.index()),
            Pos::OnVertex(v) => {
                let vh = self.cdt.vertex(v);
                vh.out_edges()
                    .find_map(|e| e.face().as_inner().map(|f| f.fix().index()))
            }
            Pos::OnEdge(e) => {
                let eh = self.cdt.directed_edge(e);
                eh.face()
                    .as_inner()
                    .map(|f| f.fix().index())
                    .or_else(|| eh.rev().face().as_inner().map(|f| f.fix().index()))
            }
            _ => None,
        }?;
        self.dense_of.get(&spade_ix).copied()
    }

    /// Place a waypoint inside the free window of every crossed edge:
    /// past the `a` endpoint's copper + clearance + already-booked
    /// load, plus half my width.
    #[allow(clippy::too_many_arguments)]
    fn waypoints(
        &self,
        sites: &[Site],
        crossed: &[(usize, usize)],
        faces: &[usize],
        from: P2,
        to: P2,
        w_mm: f64,
        clr: f64,
        strategy: usize,
    ) -> Vec<P2> {
        // Interleave face centroids with edge-window points: the
        // polyline then travels through triangle INTERIORS between
        // consecutive crossings instead of chording across whatever
        // copper sits at the corner — string-pulling trims the slack
        // right back out wherever the direct line is actually clear.
        let mut pts = Vec::with_capacity(crossed.len() * 2 + 2);
        pts.push(from);
        for (k, &(sa, sb)) in crossed.iter().enumerate() {
            if let Some(&f) = faces.get(k) {
                if let Some(c) = self.centroid.get(f) {
                    pts.push(*c);
                }
            }
            let pa = sites[sa].pos;
            let pb = sites[sb].pos;
            let len = dist(pa, pb);
            if len < 1e-9 {
                continue;
            }
            let used = self
                .used
                .get(&(sa.min(sb), sa.max(sb)))
                .copied()
                .unwrap_or(0.0);
            let d = [(pb[0] - pa[0]) / len, (pb[1] - pa[1]) / len];
            // Free window along the edge after the a-side copper +
            // booked load and the b-side copper.
            let lo = sites[sa].reach(d) + clr + used + w_mm / 2.0;
            let hi = len - sites[sb].reach(d) - clr - w_mm / 2.0;
            let off = if hi <= lo {
                lo.min(len * 0.9)
            } else {
                match strategy {
                    0 => lo,
                    1 => f64::midpoint(lo, hi),
                    _ => hi,
                }
            };
            let t = (off / len).clamp(0.05, 0.95);
            pts.push([pa[0] + (pb[0] - pa[0]) * t, pa[1] + (pb[1] - pa[1]) * t]);
        }
        if let Some(&f) = faces.last() {
            if let Some(c) = self.centroid.get(f) {
                pts.push(*c);
            }
        }
        pts.push(to);
        pts
    }
}

/// Rebuild `chain` so every segment is clear, deforming dirty segments
/// around whatever blocks them (recursive midpoint push — the elastic
/// band bending). Returns `None` when some segment cannot be freed
/// within the search depth.
fn repair_chain(chain: &[P2], obs: &Obstacles, hw: f64, clr: f64) -> Option<Vec<P2>> {
    let mut out: Vec<P2> = Vec::with_capacity(chain.len());
    out.push(chain[0]);
    for w in chain.windows(2) {
        let sub = clear_subpath(w[0], w[1], obs, hw, clr, 4)?;
        out.extend_from_slice(&sub[1..]);
    }
    Some(out)
}

/// A clear polyline from `a` to `b`: the segment itself if clean,
/// otherwise recurse on both halves around a perpendicularly displaced
/// midpoint (nearest displacement first, both sides).
fn clear_subpath(a: P2, b: P2, obs: &Obstacles, hw: f64, clr: f64, depth: u8) -> Option<Vec<P2>> {
    if obs.polyline_clear(&[a, b], hw, clr) {
        return Some(vec![a, b]);
    }
    if depth == 0 {
        return None;
    }
    let len = dist(a, b);
    if len < 0.05 {
        return None; // nothing left to bend
    }
    let mid = [f64::midpoint(a[0], b[0]), f64::midpoint(a[1], b[1])];
    let n = [-(b[1] - a[1]) / len, (b[0] - a[0]) / len];
    for d in [0.4, 0.8, 1.6, 3.2] {
        for sgn in [1.0, -1.0] {
            let m = [mid[0] + n[0] * d * sgn, mid[1] + n[1] * d * sgn];
            let Some(left) = clear_subpath(a, m, obs, hw, clr, depth - 1) else {
                continue;
            };
            let Some(right) = clear_subpath(m, b, obs, hw, clr, depth - 1) else {
                continue;
            };
            let mut out = left;
            out.extend_from_slice(&right[1..]);
            return Some(out);
        }
    }
    None
}

/// A spot near `c` where a via barrel keeps clearance on BOTH layers
/// (spiral search).
fn find_via_spot(c: P2, via_r: f64, clr: f64, top: &Obstacles, bottom: &Obstacles) -> Option<P2> {
    for ring in 0..5 {
        let rad = 0.7 * f64::from(ring);
        let steps = if ring == 0 { 1 } else { 12 };
        for k in 0..steps {
            let ang = std::f64::consts::TAU * f64::from(k) / f64::from(steps);
            let v = [c[0] + rad * ang.cos(), c[1] + rad * ang.sin()];
            if top.polyline_clear(&[v, v], via_r, clr) && bottom.polyline_clear(&[v, v], via_r, clr)
            {
                return Some(v);
            }
        }
    }
    None
}
