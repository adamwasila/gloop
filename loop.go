package gloop

import (
	"sync"
	"time"
)

// Hz60Delay is 1/60th of a second.
const Hz60Delay time.Duration = time.Duration(int64(time.Second) / 60)

type state int

const (
	stateInit state = iota
	stateRun  state = iota
	stateStop state = iota
)

// LoopFn is a function that is called inside the game loop.
// step should be treated as if it was the amount of time that
// elapsed since the last call.
type LoopFn func(step time.Duration) error

// Loop is a game loop.
type Loop struct {
	// Render is an elastic-step function.
	Render LoopFn
	// Simulate is a fixed-step function.
	Simulate LoopFn
	// RenderRate controls how often Render will be called.
	// This is the time delay between calls.
	RenderLatency time.Duration
	// SimulationRate controls how often Simulate will be called.
	// This is the time delay between calls.
	SimulationLatency time.Duration
	mu                sync.Mutex
	runOnce           sync.Once
	doneSignal        chan interface{}
	done              chan interface{}
	err               error
	heartbeat         chan LatencySample
	curState          state
}

// NewLoop creates a new game loop.
func NewLoop(Render, Simulate LoopFn, RenderLatency, SimulationLatency time.Duration) (*Loop, error) {
	// Input validation.
	if RenderLatency <= 0 {
		return nil, wrapLoopError(nil, TokenLoop, "RenderRate can't be lte 0")
	}
	if SimulationLatency <= 0 {
		return nil, wrapLoopError(nil, TokenLoop, "SimulationRate can't be lte 0")
	}

	// Init loop.
	return &Loop{
		Render:            Render,
		Simulate:          Simulate,
		SimulationLatency: SimulationLatency,
		RenderLatency:     RenderLatency,
		runOnce:           sync.Once{},
		doneSignal:        make(chan interface{}),
		done:              make(chan interface{}),
		err:               nil,
		heartbeat:         make(chan LatencySample),
		curState:          stateInit,
	}, nil
}

// Heartbeat returns the heartbeat channel which
// can be used to monitor the health of the game loop.
// A pulse will be sent every second with current simulation
// and render latency.
func (l *Loop) Heartbeat() <-chan LatencySample {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heartbeat
}

// Done returns a chan that indicates when the loop is stopped.
// When this finishes, you should do cleanup.
func (l *Loop) Done() <-chan interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.doneSignal
}

// Stop halts the loop and sets Err().
// You probably want to make a call to this somewhere in Simulate().
func (l *Loop) Stop(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.curState {
	case stateInit:
		l.signalDone()
		close(l.done)
		l.err = err
		l.curState = stateStop
	case stateRun:
		// If we are running, make the loop goroutine close the reporting chan.
		// I want to guarantee that render or simulate will not be called once
		// Done() closes.
		close(l.done)
		l.err = err
		l.curState = stateStop
	case stateStop:
		return
	}
}

// Err returns the the reason why the loop closed if there was an error.
// Err will return nil if the loop has not yet run, is currently running,
// or closed without an error.
func (l *Loop) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Loop) signalDone() {
	l.runOnce.Do(func() { close(l.doneSignal) })
}

// Start initiates a game loop. This call block.
// To stop the loop, close the done chan.
// To get notified before Simulate or Render are called, pull items from
// the heartbeat channel.
// If either Render or Simulate throw an error, the error will be made available
// on the output error channel and the loop will stop.
func (l *Loop) Start() error {
	startInit := func() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Silently fail on re-starts.
	if l.curState != stateInit {
		return wrapLoopError(nil, TokenLoop, "Loop is already running or is done")
	}
	l.curState = stateRun
		return nil
	}
	err := startInit()
	if err != nil {
		return err
	}

		// Stats heartbeat channel set up
		heartTick := time.NewTicker(time.Second)
		sendBeat := func(ps LatencySample) {
			select {
			case l.heartbeat <- ps:
			default: // Throw it away if no one is listening.
			}
		}

		// simTick has an internal limiter, and I need to make sure the
		// delay isn't accidentally doubled.
		simChan := time.NewTimer(time.Duration(0))
		// rendTick has no internal limiter, the Ticker controls
		// the execution rate.
		rendTick := time.NewTicker(l.RenderLatency)

		defer simChan.Stop()
		defer rendTick.Stop()
		defer heartTick.Stop()
		defer close(l.heartbeat)
		defer l.Stop(nil)

		// Time tracking.
		simAccumulator := time.Duration(0)
		now := time.Now()
		simLatency := newLatencyTracker()
		previousSim := now
		rendLatency := newLatencyTracker()
		previousRend := now

		for {
			select {
			case <-l.doneSignal:
			return nil
			case <-l.done:
				l.signalDone()
			return nil
			case <-heartTick.C:
				sendBeat(LatencySample{
					RenderLatency:   rendLatency.Latency(),
					SimulateLatency: simLatency.Latency(),
				})
			case <-simChan.C:
				// How much are we behind?
				curTime := time.Now()
				frameTime := curTime.Sub(previousSim)
				previousSim = curTime
				simAccumulator += frameTime
				// Call simulate() if we built up enough lag.
				for simAccumulator >= l.SimulationLatency {
					// Run the simulation with a fixed step.

					// Actually call simulate...
					if er := l.Simulate(l.SimulationLatency); er != nil {
						wrapped := wrapLoopError(er, TokenSimulate, "Error returned by Simulate(%s)", l.SimulationLatency.String())
						wrapped.Misc["curTime"] = curTime
						l.Stop(wrapped)
					return nil
					}

					simLatency.MarkDone(l.SimulationLatency)

					// Keep track of leftover time.
					simAccumulator -= l.SimulationLatency
				}
				// Set up next call to simulate()...
				simChan.Reset(l.SimulationLatency - simAccumulator)
			case <-rendTick.C:
				// How much are we behind?
				curTime := time.Now()
				frameTime := curTime.Sub(previousRend)
				previousRend = curTime

				// Call render() if we built up enough lag.
				// Unlike simulate(), we can skip calls by varying the input time delta.
				// Actually call render...
				if er := l.Render(frameTime); er != nil {
					wrapped := wrapLoopError(er, TokenRender, "Error returned by Render(%s)", frameTime.String())
					wrapped.Misc["curTime"] = curTime
					l.Stop(wrapped)
				return nil
				}

				rendLatency.MarkDone(frameTime)
			}
		}
}
