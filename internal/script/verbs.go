package script

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/placer"
	"github.com/mentasystems/fragua/internal/router"
)

func splitRefsKV(args string) (refs []string, kv string) {
	var kvParts []string
	for _, t := range strings.Fields(args) {
		if strings.Contains(t, "=") {
			kvParts = append(kvParts, t)
			continue
		}
		refs = append(refs, t)
	}
	return refs, strings.Join(kvParts, " ")
}

func kvFloat(kv, key string) (float64, bool) {
	for _, t := range strings.Fields(kv) {
		if strings.HasPrefix(t, key+"=") {
			v, err := strconv.ParseFloat(strings.TrimPrefix(t, key+"="), 64)
			if err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func kvString(kv, key string) (string, bool) {
	for _, t := range strings.Fields(kv) {
		if strings.HasPrefix(t, key+"=") {
			return strings.TrimPrefix(t, key+"="), true
		}
	}
	return "", false
}

func cmdPour(p *core.Project, args string) (string, error) {
	// pour NET [layer=Top|Bottom] [relief=spokes4|solid] [stitch=true] [pitch=N]
	fields := strings.Fields(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("pour NET [layer=Top] [relief=spokes4|solid] [stitch=true]")
	}
	net := fields[0]
	layer := core.LayerTop
	relief := "spokes4"
	var stitch *core.StitchPolicy
	for _, t := range fields[1:] {
		switch {
		case strings.HasPrefix(t, "layer="):
			layer = parseLayerToken(strings.TrimPrefix(t, "layer="))
		case strings.HasPrefix(t, "relief="):
			relief = strings.TrimPrefix(t, "relief=")
		case strings.HasPrefix(t, "stitch="):
			v := strings.TrimPrefix(t, "stitch=")
			if v == "true" || v == "1" || v == "{}" || v == "yes" {
				stitch = &core.StitchPolicy{Enabled: true}
			}
		case strings.HasPrefix(t, "pitch="):
			var pitch float64
			fmt.Sscanf(t, "pitch=%f", &pitch)
			if stitch == nil {
				stitch = &core.StitchPolicy{Enabled: true}
			}
			stitch.PitchMM = pitch
		}
	}
	p.MutateBoard(func(b *core.Board) {
		out := b.Pours[:0]
		for _, pr := range b.Pours {
			if !(pr.Net == net && pr.Layer.Index == layer.Index) {
				out = append(out, pr)
			}
		}
		tr := core.ThermalRelief{Kind: relief}
		b.Pours = append(out, core.Pour{
			ID: core.NewID(), Net: net, Layer: layer, ThermalRelief: &tr, Stitching: stitch,
		})
	})
	return fmt.Sprintf("pour %s on %s", net, layer.LegacyName()), nil
}

func cmdAutoPour(p *core.Project, args string) (string, error) {
	// auto-pour [GND] [+3V3] …  — default GND on both layers
	nets := strings.Fields(args)
	if len(nets) == 0 {
		nets = []string{"GND"}
	}
	var msgs []string
	for _, n := range nets {
		if strings.Contains(n, "=") {
			continue
		}
		m, err := cmdPour(p, n+" layer=Top")
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
		m, err = cmdPour(p, n+" layer=Bottom")
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
	}
	return strings.Join(msgs, "; "), nil
}

func cmdClearPour(p *core.Project, args string) (string, error) {
	net := strings.TrimSpace(args)
	n := 0
	p.MutateBoard(func(b *core.Board) {
		if net == "" {
			n = len(b.Pours)
			b.Pours = nil
			return
		}
		out := b.Pours[:0]
		for _, pr := range b.Pours {
			if pr.Net == net {
				n++
				continue
			}
			out = append(out, pr)
		}
		b.Pours = out
	})
	return fmt.Sprintf("cleared %d pours", n), nil
}

func cmdStitch(p *core.Project, _ string) (string, error) {
	opts := router.DefaultOptions()
	n := 0
	p.MutateBoard(func(b *core.Board) {
		fab := core.ActiveFabRules(b)
		if fab.MinViaDrillMM > 0 {
			opts.ViaDrillMM = fab.MinViaDrillMM
		}
		if fab.MinViaDiameterMM > 0 {
			opts.ViaDiameterMM = fab.MinViaDiameterMM
		}
		n = router.StitchIsolatedPads(b, opts)
	})
	return fmt.Sprintf("stitched %d vias", n), nil
}

func cmdNC(p *core.Project, args string) (string, error) {
	// nc REF.PIN [REF.PIN...]
	fields := strings.Fields(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("nc REF.PIN [REF.PIN...]")
	}
	marked := 0
	p.MutateSchematic(func(s *core.Schematic) {
		for _, tok := range fields {
			parts := strings.SplitN(tok, ".", 2)
			if len(parts) != 2 {
				continue
			}
			ref, pin := parts[0], parts[1]
			for _, sym := range s.Symbols {
				if sym == nil || sym.Reference != ref {
					continue
				}
				for i := range sym.Kind.ICPins {
					if sym.Kind.ICPins[i].Number == pin {
						sym.Kind.ICPins[i].NC = true
						sym.Kind.ICPins[i].Role = core.PinNC
						marked++
					}
				}
			}
		}
	})
	if marked == 0 {
		return "", fmt.Errorf("nc: no matching pins (use on generic_ic pin numbers)")
	}
	return fmt.Sprintf("nc %d pin(s)", marked), nil
}

func cmdFiducial(p *core.Project, args string) (string, error) {
	// fiducial X Y [ref=FID1]
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("fiducial X Y [ref=FID1]")
	}
	x, e1 := strconv.ParseFloat(fields[0], 64)
	y, e2 := strconv.ParseFloat(fields[1], 64)
	if e1 != nil || e2 != nil {
		return "", fmt.Errorf("fiducial: bad coordinates")
	}
	ref := ""
	for _, t := range fields[2:] {
		if strings.HasPrefix(t, "ref=") {
			ref = strings.TrimPrefix(t, "ref=")
		}
	}
	p.MutateBoard(func(b *core.Board) {
		if ref == "" {
			n := 1
			for _, fp := range b.Footprints {
				if fp != nil && fp.Fiducial {
					n++
				}
			}
			ref = fmt.Sprintf("FID%d", n)
		}
		b.AddFootprint(&core.Footprint{
			ID:        core.NewID(),
			Reference: ref,
			Value:     "FIDUCIAL",
			Library:   "fiducial",
			Key:       "fiducial",
			Fiducial:  true,
			Position:  core.NewPoint(core.FromMM(x), core.FromMM(y)),
			Layer:     core.LayerTop,
			Pads: []core.Pad{{
				Number: "1",
				Size:   [2]core.Length{core.FromMM(1.0), core.FromMM(1.0)},
				Layer:  core.LayerTop,
			}},
		})
	})
	return fmt.Sprintf("fiducial %s at %.2f,%.2f", ref, x, y), nil
}

func cmdDiffPair(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("diff NETA NETB")
	}
	a, b := fields[0], fields[1]
	p.MutateSchematic(func(s *core.Schematic) {
		if s.Nets == nil {
			s.Nets = map[string]*core.Net{}
		}
		ensure := func(name, pair string) {
			n := s.Nets[name]
			if n == nil {
				n = &core.Net{Name: name}
				s.Nets[name] = n
			}
			n.DiffPair = pair
		}
		ensure(a, b)
		ensure(b, a)
	})
	return fmt.Sprintf("diff %s %s", a, b), nil
}

func cmdOutlinePoly(p *core.Project, args string) (string, error) {
	// outline-poly x1 y1 x2 y2 ...
	fields := strings.Fields(args)
	if len(fields) < 6 || len(fields)%2 != 0 {
		return "", fmt.Errorf("outline-poly x1 y1 x2 y2 ... (at least 3 points)")
	}
	var pts []core.Point
	for i := 0; i+1 < len(fields); i += 2 {
		x, e1 := strconv.ParseFloat(fields[i], 64)
		y, e2 := strconv.ParseFloat(fields[i+1], 64)
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("outline-poly: bad number at %s %s", fields[i], fields[i+1])
		}
		pts = append(pts, core.NewPoint(core.FromMM(x), core.FromMM(y)))
	}
	bb := pts[0]
	minX, minY, maxX, maxY := bb.X, bb.Y, bb.X, bb.Y
	for _, q := range pts[1:] {
		if q.X < minX {
			minX = q.X
		}
		if q.Y < minY {
			minY = q.Y
		}
		if q.X > maxX {
			maxX = q.X
		}
		if q.Y > maxY {
			maxY = q.Y
		}
	}
	r := core.RectFromCorners(core.NewPoint(minX, minY), core.NewPoint(maxX, maxY))
	p.MutateBoard(func(b *core.Board) {
		b.OutlinePoly = pts
		b.Outline = &r
	})
	return fmt.Sprintf("outline-poly %d pts  %.1fx%.1f mm", len(pts), r.Width().ToMM(), r.Height().ToMM()), nil
}

func cmdCutout(p *core.Project, args string) (string, error) {
	// cutout x1 y1 x2 y2 ... [label=NAME]
	fields := strings.Fields(args)
	var coords []string
	label := ""
	for _, t := range fields {
		if strings.HasPrefix(t, "label=") {
			label = strings.TrimPrefix(t, "label=")
			continue
		}
		coords = append(coords, t)
	}
	if len(coords) < 6 || len(coords)%2 != 0 {
		return "", fmt.Errorf("cutout x1 y1 x2 y2 ... [label=NAME]")
	}
	var pts []core.Point
	for i := 0; i+1 < len(coords); i += 2 {
		x, e1 := strconv.ParseFloat(coords[i], 64)
		y, e2 := strconv.ParseFloat(coords[i+1], 64)
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("cutout: bad number")
		}
		pts = append(pts, core.NewPoint(core.FromMM(x), core.FromMM(y)))
	}
	p.MutateBoard(func(b *core.Board) {
		b.Cutouts = append(b.Cutouts, core.Cutout{ID: core.NewID(), Polygon: pts})
	})
	if label != "" {
		return fmt.Sprintf("cutout %s (%d pts)", label, len(pts)), nil
	}
	return fmt.Sprintf("cutout (%d pts)", len(pts)), nil
}

func cmdClearCutouts(p *core.Project, _ string) (string, error) {
	n := 0
	p.MutateBoard(func(b *core.Board) {
		n = len(b.Cutouts)
		b.Cutouts = nil
	})
	return fmt.Sprintf("cleared %d cutouts", n), nil
}

func cmdHole(p *core.Project, args string) (string, error) {
	// hole x y d [label=]
	fields := strings.Fields(args)
	if len(fields) < 3 {
		return "", fmt.Errorf("hole X Y D [label=NAME]")
	}
	x, e1 := strconv.ParseFloat(fields[0], 64)
	y, e2 := strconv.ParseFloat(fields[1], 64)
	d, e3 := strconv.ParseFloat(fields[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return "", fmt.Errorf("hole: bad numbers")
	}
	label := ""
	if v, ok := kvString(strings.Join(fields[3:], " "), "label"); ok {
		label = v
	}
	p.MutateBoard(func(b *core.Board) {
		b.MountHoles = append(b.MountHoles, core.MountHole{
			ID: core.NewID(), Center: core.NewPoint(core.FromMM(x), core.FromMM(y)),
			Diameter: core.FromMM(d), Label: label,
		})
	})
	return fmt.Sprintf("hole at %.2f,%.2f d=%.2f", x, y, d), nil
}

func cmdClearHoles(p *core.Project, _ string) (string, error) {
	n := 0
	p.MutateBoard(func(b *core.Board) {
		n = len(b.MountHoles) + len(b.Holes)
		b.MountHoles = nil
		b.Holes = nil
	})
	return fmt.Sprintf("cleared %d holes", n), nil
}

func cmdKeepout(p *core.Project, args string) (string, error) {
	// keepout x1 y1 x2 y2 [no_copper=true] [no_place=true]
	fields := strings.Fields(args)
	if len(fields) < 4 {
		return "", fmt.Errorf("keepout X1 Y1 X2 Y2 [no_copper=true] [no_place=true]")
	}
	x1, e1 := strconv.ParseFloat(fields[0], 64)
	y1, e2 := strconv.ParseFloat(fields[1], 64)
	x2, e3 := strconv.ParseFloat(fields[2], 64)
	y2, e4 := strconv.ParseFloat(fields[3], 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return "", fmt.Errorf("keepout: bad coordinates")
	}
	k := core.Keepout{
		ID: core.NewID(),
		Rect: func() *core.Rect {
			r := core.RectFromCorners(
				core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
				core.NewPoint(core.FromMM(x2), core.FromMM(y2)),
			)
			return &r
		}(),
		NoCopper: true,
		NoPlace:  true,
	}
	kv := strings.Join(fields[4:], " ")
	if v, ok := kvString(kv, "no_copper"); ok {
		k.NoCopper = v != "false"
	}
	if v, ok := kvString(kv, "no_place"); ok {
		k.NoPlace = v != "false"
	}
	p.MutateBoard(func(b *core.Board) { b.Keepouts = append(b.Keepouts, k) })
	return fmt.Sprintf("keepout %.1f,%.1f–%.1f,%.1f", x1, y1, x2, y2), nil
}

func cmdSilkLine(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 4 {
		return "", fmt.Errorf("silk-line X1 Y1 X2 Y2 [width=0.15]")
	}
	x1, e1 := strconv.ParseFloat(fields[0], 64)
	y1, e2 := strconv.ParseFloat(fields[1], 64)
	x2, e3 := strconv.ParseFloat(fields[2], 64)
	y2, e4 := strconv.ParseFloat(fields[3], 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return "", fmt.Errorf("silk-line: bad coordinates")
	}
	w := 0.15
	if v, ok := kvFloat(strings.Join(fields[4:], " "), "width"); ok {
		w = v
	}
	p.MutateBoard(func(b *core.Board) {
		b.SilkLines = append(b.SilkLines, core.SilkLine{
			Layer: core.SilkTop,
			Start: core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
			End:   core.NewPoint(core.FromMM(x2), core.FromMM(y2)),
			Width: core.FromMM(w),
		})
	})
	return "silk-line added", nil
}

func cmdSilkText(p *core.Project, args string) (string, error) {
	// silk-text X Y TEXT [size=1]
	fields := tokenize(args)
	if len(fields) < 3 {
		return "", fmt.Errorf("silk-text X Y TEXT [size=1]")
	}
	x, e1 := strconv.ParseFloat(fields[0], 64)
	y, e2 := strconv.ParseFloat(fields[1], 64)
	if e1 != nil || e2 != nil {
		return "", fmt.Errorf("silk-text: bad coordinates")
	}
	text := fields[2]
	size := 1.0
	if v, ok := kvFloat(strings.Join(fields[3:], " "), "size"); ok {
		size = v
	}
	p.MutateBoard(func(b *core.Board) {
		b.SilkTexts = append(b.SilkTexts, core.SilkText{
			Layer: core.SilkTop, Position: core.NewPoint(core.FromMM(x), core.FromMM(y)),
			Text: text, Size: core.FromMM(size), Width: core.FromMM(size / 8),
		})
	})
	return fmt.Sprintf("silk-text %q", text), nil
}

func cmdMove(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 3 {
		return "", fmt.Errorf("move REF X Y")
	}
	x, e1 := strconv.ParseFloat(fields[1], 64)
	y, e2 := strconv.ParseFloat(fields[2], 64)
	if e1 != nil || e2 != nil {
		return "", fmt.Errorf("move: bad coordinates")
	}
	var ok bool
	p.MutateBoard(func(b *core.Board) {
		fp := b.FootprintByRef(fields[0])
		if fp == nil {
			return
		}
		fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
		ok = true
	})
	if !ok {
		return "", fmt.Errorf("move: unknown %q", fields[0])
	}
	return fmt.Sprintf("moved %s to %.2f,%.2f", fields[0], x, y), nil
}

func cmdRotate(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("rotate REF DEG")
	}
	deg, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return "", fmt.Errorf("rotate: bad degrees")
	}
	var ok bool
	p.MutateBoard(func(b *core.Board) {
		fp := b.FootprintByRef(fields[0])
		if fp == nil {
			return
		}
		fp.Rotation = deg
		ok = true
	})
	if !ok {
		return "", fmt.Errorf("rotate: unknown %q", fields[0])
	}
	return fmt.Sprintf("rotated %s to %.0f", fields[0], deg), nil
}

func cmdDelete(p *core.Project, args string) (string, error) {
	refs := strings.Fields(args)
	if len(refs) == 0 {
		return "", fmt.Errorf("delete REF [REF ...]")
	}
	n := 0
	p.MutateBoard(func(b *core.Board) {
		for _, r := range refs {
			if b.RemoveFootprintByRef(r) != nil {
				n++
			}
		}
	})
	return fmt.Sprintf("deleted %d footprints", n), nil
}

func cmdUnplace(p *core.Project, args string) (string, error) {
	refs := strings.Fields(args)
	if len(refs) == 0 {
		return "", fmt.Errorf("unplace REF [REF ...]")
	}
	var taken []*core.Footprint
	p.MutateBoard(func(b *core.Board) {
		for _, r := range refs {
			if fp := b.RemoveFootprintByRef(r); fp != nil {
				cp := *fp
				taken = append(taken, &cp)
			}
		}
	})
	for _, fp := range taken {
		p.PaletteAdd(*fp)
	}
	return fmt.Sprintf("unplaced %d to palette", len(taken)), nil
}

func cmdClearBoard(p *core.Project, _ string) (string, error) {
	n := 0
	p.MutateBoard(func(b *core.Board) {
		n = len(b.Footprints)
		b.Footprints = map[string]*core.Footprint{}
		b.FootprintOrder = nil
		b.Traces = nil
		b.Vias = nil
	})
	return fmt.Sprintf("cleared %d footprints + copper", n), nil
}

func cmdClearNet(p *core.Project, args string) (string, error) {
	net := strings.TrimSpace(args)
	if net == "" {
		return "", fmt.Errorf("clear-net NET")
	}
	nt, nv := 0, 0
	p.MutateBoard(func(b *core.Board) {
		outT := b.Traces[:0]
		for _, t := range b.Traces {
			if t.Net == net {
				nt++
				continue
			}
			outT = append(outT, t)
		}
		b.Traces = outT
		outV := b.Vias[:0]
		for _, v := range b.Vias {
			if v.Net == net {
				nv++
				continue
			}
			outV = append(outV, v)
		}
		b.Vias = outV
	})
	return fmt.Sprintf("cleared net %s (%d traces, %d vias)", net, nt, nv), nil
}

func cmdDeleteVia(p *core.Project, args string) (string, error) {
	id := strings.TrimSpace(args)
	if id == "" {
		return "", fmt.Errorf("delete-via ID")
	}
	n := 0
	p.MutateBoard(func(b *core.Board) {
		out := b.Vias[:0]
		for _, v := range b.Vias {
			if v.ID.String() == id {
				n++
				continue
			}
			out = append(out, v)
		}
		b.Vias = out
	})
	return fmt.Sprintf("deleted %d vias", n), nil
}

func cmdEdgePlace(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("edge-place REF left|right|top|bottom [along=N]")
	}
	ref, side := fields[0], fields[1]
	var along *float64
	if v, ok := kvFloat(strings.Join(fields[2:], " "), "along"); ok {
		along = &v
	}
	fromPal, palOK := p.PaletteTake(ref)
	var msg string
	err := error(nil)
	p.MutateBoard(func(b *core.Board) {
		if b.Outline == nil {
			err = fmt.Errorf("edge-place: no outline")
			return
		}
		fp := b.FootprintByRef(ref)
		if fp == nil {
			if !palOK {
				err = fmt.Errorf("edge-place: unknown %q", ref)
				return
			}
			fp = fromPal
			fp.EdgeMounted = true
			b.AddFootprint(fp)
			fromPal = nil
		}
		fp.EdgeMounted = true
		placer.EdgePlace(fp, *b.Outline, side, along)
		msg = fmt.Sprintf("edge-placed %s on %s at (%.2f, %.2f) rot=%.0f",
			ref, side, fp.Position.X.ToMM(), fp.Position.Y.ToMM(), fp.Rotation)
	})
	if err != nil && fromPal != nil {
		p.PaletteAdd(*fromPal)
	}
	return msg, err
}

func cmdEdgePlan(p *core.Project, args string) (string, error) {
	refs, _ := splitRefsKV(args)
	if len(refs) == 0 {
		return "", fmt.Errorf("edge-plan REF [REF...]")
	}
	n := 0
	p.MutateBoard(func(b *core.Board) {
		n = placer.PlanEdges(b, refs)
	})
	return fmt.Sprintf("edge-plan moved %d connectors", n), nil
}

func cmdPlaceLegal(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("place-legal REF [tries=N] [rot=DEG]")
	}
	ref := fields[0]
	tries := 80
	rot := 0.0
	rotSet := false
	kv := strings.Join(fields[1:], " ")
	if v, ok := kvFloat(kv, "tries"); ok {
		tries = int(v)
	}
	if v, ok := kvFloat(kv, "rot"); ok {
		rot, rotSet = v, true
	}
	fp, fromPal := p.PaletteTake(ref)
	var msg string
	err := error(nil)
	p.MutateBoard(func(b *core.Board) {
		if b.Outline == nil {
			err = fmt.Errorf("place-legal: no outline")
			return
		}
		if fp == nil {
			fp = b.FootprintByRef(ref)
		}
		if fp == nil {
			err = fmt.Errorf("place-legal: unknown %q", ref)
			return
		}
		if rotSet {
			fp.Rotation = rot
		}
		ow, oh := b.Outline.Width().ToMM(), b.Outline.Height().ToMM()
		rng := newLegalRNG(42 + uint64(len(ref))*17)
		ok := false
		for i := 0; i < tries; i++ {
			x := b.Outline.Min.X.ToMM() + 2 + rng.f64()*(ow-4)
			y := b.Outline.Min.Y.ToMM() + 2 + rng.f64()*(oh-4)
			fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
			if placer.LegalAt(b, fp) {
				ok = true
				break
			}
		}
		if !ok {
			err = fmt.Errorf("place-legal: no legal site for %s in %d tries", ref, tries)
			return
		}
		if fromPal {
			b.AddFootprint(fp)
		}
		msg = fmt.Sprintf("place-legal %s at %.2f,%.2f", ref, fp.Position.X.ToMM(), fp.Position.Y.ToMM())
	})
	if err != nil && fromPal && fp != nil {
		p.PaletteAdd(*fp)
	}
	return msg, err
}

func cmdReset(p *core.Project, _ string) (string, error) {
	p.Reset()
	return "project reset", nil
}

func cmdNetClass(p *core.Project, args string) (string, error) {
	// class NAME [clearance=] [width=]   or   net-class NET CLASS
	fields := strings.Fields(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("class NAME [clearance=N] [width=N]  |  net-class NET CLASS")
	}
	// net-class NET CLASS
	if !strings.Contains(args, "=") && len(fields) >= 2 {
		net, cls := fields[0], fields[1]
		p.MutateSchematic(func(s *core.Schematic) {
			if s.NetToClass == nil {
				s.NetToClass = map[string]string{}
			}
			s.NetToClass[net] = cls
			if n := s.Nets[net]; n != nil {
				n.Class = cls
			}
		})
		return fmt.Sprintf("net %s class=%s", net, cls), nil
	}
	name := fields[0]
	cls := core.NetClass{Name: name}
	for _, t := range fields[1:] {
		if strings.HasPrefix(t, "clearance=") {
			v, _ := strconv.ParseFloat(strings.TrimPrefix(t, "clearance="), 64)
			cls.ClearanceMM = v
		}
		if strings.HasPrefix(t, "width=") {
			v, _ := strconv.ParseFloat(strings.TrimPrefix(t, "width="), 64)
			cls.TraceWidthMM = v
		}
		if strings.HasPrefix(t, "impedance=") {
			v, _ := strconv.ParseFloat(strings.TrimPrefix(t, "impedance="), 64)
			cls.ImpedanceOhms = v
		}
		if strings.HasPrefix(t, "diff=") {
			cls.DiffPair = strings.TrimPrefix(t, "diff=")
		}
	}
	p.MutateSchematic(func(s *core.Schematic) {
		if s.NetClasses == nil {
			s.NetClasses = map[string]*core.NetClass{}
		}
		s.NetClasses[name] = &cls
	})
	return fmt.Sprintf("class %s", name), nil
}

// tiny xorshift for place-legal (not the placer RNG — keep packages decoupled).
type legalRNG struct{ s uint64 }

func newLegalRNG(seed uint64) *legalRNG {
	if seed == 0 {
		seed = 1
	}
	return &legalRNG{s: seed}
}

func (r *legalRNG) f64() float64 {
	x := r.s
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.s = x
	return float64((x*2685821657736338717)>>11) * (1.0 / float64(uint64(1)<<53))
}
