package placer

// xorshift64* — same generator as crates/pcb-placer (deterministic SA).
type rng struct {
	state uint64
}

func newRNG(seed uint64) *rng {
	if seed == 0 {
		seed = 1
	}
	return &rng{state: seed}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *rng) nextF64() float64 {
	return float64(r.nextU64()>>11) * (1.0 / float64(uint64(1)<<53))
}
