package router

import "sync/atomic"

// Processor classifies, rewrites and drops messages. It is implemented by the
// pipeline package, which imports router, so the pumps only know this interface.
type Processor interface {
	// Global runs once per message, before it is fanned out to the routes.
	// It may modify m, which is not shared yet. dropped means no route gets it.
	Global(m *Message) (dropped bool)
	// Target runs per route. It must not modify m, which is shared between
	// routes: when the route has rules it returns a modified copy, else m.
	Target(routeName string, m *Message) (out *Message, dropped bool)
}

type processorHolder struct{ p Processor }

var processor atomic.Pointer[processorHolder]

// SetProcessor sets the active processor, or removes it with nil. It is safe
// to call while messages flow, so the processor can be swapped on reload.
// Without a processor the pumps send messages unchanged.
func SetProcessor(p Processor) {
	if p == nil {
		processor.Store(nil)
		return
	}
	processor.Store(&processorHolder{p})
}

// CurrentProcessor returns the active processor, or nil.
func CurrentProcessor() Processor {
	if h := processor.Load(); h != nil {
		return h.p
	}
	return nil
}

// Tap sees every raw message in the pumps, before the processor. The web
// interface uses it for its samples and the live view. p is the active
// processor at that moment, nil when none. Observe must not block, must not
// modify m and must not keep it: m is shared with the rest of the pump.
type Tap interface {
	Observe(m *Message, p Processor)
}

type tapHolder struct{ t Tap }

var tap atomic.Pointer[tapHolder]

// SetTap sets the tap, or removes it with nil. Without a tap the pumps do nothing extra.
func SetTap(t Tap) {
	if t == nil {
		tap.Store(nil)
		return
	}
	tap.Store(&tapHolder{t})
}

// CurrentTap returns the active tap, or nil.
func CurrentTap() Tap {
	if h := tap.Load(); h != nil {
		return h.t
	}
	return nil
}
