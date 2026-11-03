package drc

import "github.com/mentasystems/fragua/internal/core"

// ConnectStats is how much of a board's copper actually joins its pads.
// The router uses it to decide whether a re-route is allowed to replace
// the copper it started with.
type ConnectStats struct {
	Nets          int
	CompleteNets  int
	Pads          int
	ConnectedPads int
	Vias          int
}

// Connectivity measures pad-to-pad continuity the same way DRC does:
// a pad counts as connected when it touches same-net copper, and a net
// is complete when every one of its pads sits in that single island.
// The returned set is those complete nets.
func Connectivity(board *core.Board) (ConnectStats, map[string]bool) {
	var st ConnectStats
	if board != nil {
		st.Vias = len(board.Vias)
	}
	done := completeNets(board, &st)
	st.CompleteNets = len(done)
	return st, done
}

// CompleteNetSet is the set of nets whose pads are one copper island.
func CompleteNetSet(board *core.Board) map[string]bool {
	_, done := Connectivity(board)
	return done
}

func completeNets(board *core.Board, st *ConnectStats) map[string]bool {
	out := map[string]bool{}
	if board == nil {
		return out
	}
	pads := collectPads(board)
	elems := buildCopperElems(board, pads)
	n := len(elems)
	if n == 0 {
		return out
	}
	byNet := map[string][]int{}
	for i, e := range elems {
		if e.net == "" {
			continue
		}
		byNet[e.net] = append(byNet[e.net], i)
	}
	if st != nil {
		st.Nets = 0
	}
	for net, idxs := range byNet {
		var padIdxs []int
		for _, i := range idxs {
			if elems[i].isPad {
				padIdxs = append(padIdxs, i)
			}
		}
		if len(padIdxs) < 2 {
			continue
		}
		if st != nil {
			st.Nets++
			st.Pads += len(padIdxs)
		}
		uf := newUF(n)
		touched := make([]bool, n)
		for ai := 0; ai < len(idxs); ai++ {
			for bi := ai + 1; bi < len(idxs); bi++ {
				ia, ib := idxs[ai], idxs[bi]
				if elemsTouch(board, &elems[ia], &elems[ib]) {
					uf.union(ia, ib)
					touched[ia] = true
					touched[ib] = true
				}
			}
		}
		root := -1
		all := true
		for _, pi := range padIdxs {
			if !touched[pi] {
				all = false
				continue
			}
			if st != nil {
				st.ConnectedPads++
			}
			r := uf.find(pi)
			if root < 0 {
				root = r
			} else if r != root {
				all = false
			}
		}
		if all {
			out[net] = true
		}
	}
	return out
}
