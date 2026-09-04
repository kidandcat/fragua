package script

import (
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// progressMinGap throttles the SSE stream: a 600 s route with hundreds of
// nets must not turn into hundreds of events a second when a repair pass
// churns. Five a second is enough to animate a bar.
const progressMinGap = 200 * time.Millisecond

// runOp brackets a long verb with op_start / op_end events and registers it
// with the project's op tracker, which is what POST /cancel reaches. The
// engines are anytime: a cancelled op keeps the work it has committed.
func runOp(p *core.Project, name string, fn func()) {
	end := p.Ops().Begin(name)
	p.Events().Publish(core.Event{Kind: core.EventOpStarted, Op: name})
	fn()
	elapsed := p.Ops().Elapsed().Milliseconds()
	cancelled := p.Ops().Cancelled()
	end()
	p.Events().Publish(core.Event{
		Kind: core.EventOpEnded, Op: name, ElapsedMS: elapsed, Cancelled: cancelled,
	})
}

// progressEmitter returns a throttled publisher for one op's progress.
func progressEmitter(p *core.Project, name string) func(detail string, done, total int) {
	var last time.Time
	return func(detail string, done, total int) {
		now := time.Now()
		if done > 0 && now.Sub(last) < progressMinGap {
			return
		}
		last = now
		p.Events().Publish(core.Event{
			Kind: core.EventOpProgress, Op: name, Detail: detail,
			Done: done, Total: total, ElapsedMS: p.Ops().Elapsed().Milliseconds(),
		})
	}
}
