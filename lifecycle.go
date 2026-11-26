package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const nativeDrainTimeout = 2 * time.Second

type lifecycleState uint8

const (
	lifecycleActive lifecycleState = iota
	lifecycleQuiescing
	lifecycleQuiesced
	lifecycleClosed
)

type nativeRegistry struct {
	initialization sync.Mutex
	current        atomic.Pointer[nativeLifecycle]
}

func (r *nativeRegistry) initialize(call hostCall) bool {
	r.initialization.Lock()
	defer r.initialization.Unlock()
	if previous := r.current.Load(); previous != nil {
		previous.transition.Lock()
		defer previous.transition.Unlock()
		previous.mu.Lock()
		if previous.state != lifecycleQuiesced && previous.state != lifecycleClosed {
			previous.mu.Unlock()
			return false
		}
		previous.state = lifecycleClosed
		generation := previous.generation
		previous.mu.Unlock()
		generation.shutdown()
	}
	r.current.Store(newNativeLifecycle(call))
	return true
}

type nativeLifecycle struct {
	transition sync.Mutex
	mu         sync.Mutex
	state      lifecycleState
	generation *plugin
	call       hostCall
	admitted   int
	idle       chan struct{}
}

func newNativeLifecycle(call hostCall) *nativeLifecycle {
	idle := make(chan struct{})
	close(idle)
	return &nativeLifecycle{generation: newPlugin(call), call: call, idle: idle}
}

func (l *nativeLifecycle) handle(method string, raw []byte) (any, *rpcError) {
	switch method {
	case "plugin.quiesce":
		return struct{}{}, l.quiesce()
	case "plugin.reconfigure":
		l.transition.Lock()
		defer l.transition.Unlock()
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.state == lifecycleClosed {
			return nil, lifecycleFailure(l.state)
		}
		if l.state == lifecycleQuiesced {
			l.generation = newPlugin(l.call)
			l.state = lifecycleActive
		}
		return registration(), nil
	case "plugin.shutdown":
		l.shutdown()
		return struct{}{}, nil
	}

	l.mu.Lock()
	if l.state != lifecycleActive {
		failure := lifecycleFailure(l.state)
		l.mu.Unlock()
		return nil, failure
	}
	if l.admitted == 0 {
		l.idle = make(chan struct{})
	}
	l.admitted++
	generation := l.generation
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.admitted--
		if l.admitted == 0 {
			close(l.idle)
		}
		l.mu.Unlock()
	}()
	return generation.handle(method, raw)
}

func lifecycleFailure(state lifecycleState) *rpcError {
	if state == lifecycleClosed {
		return &rpcError{Code: "plugin_closed", Message: "plugin is shut down", HTTPStatus: http.StatusServiceUnavailable}
	}
	return &rpcError{Code: "plugin_quiesced", Message: "plugin is quiesced", HTTPStatus: http.StatusServiceUnavailable}
}

func (l *nativeLifecycle) quiesce() *rpcError {
	l.transition.Lock()
	defer l.transition.Unlock()
	l.mu.Lock()
	if l.state != lifecycleActive {
		state := l.state
		l.mu.Unlock()
		if state == lifecycleQuiesced {
			return nil
		}
		return lifecycleFailure(state)
	}
	l.state = lifecycleQuiescing
	generation, idle := l.generation, l.idle
	l.mu.Unlock()

	timer := time.NewTimer(nativeDrainTimeout)
	defer timer.Stop()
	wait := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		case <-timer.C:
			return false
		}
	}
	drained := wait(idle)
	if drained {
		generation.streamMu.Lock()
		streamIdle := generation.streamIdle
		generation.streamMu.Unlock()
		drained = wait(streamIdle)
	}
	if drained {
		generation.engineMu.Lock()
		attempt := generation.attempt
		generation.engineMu.Unlock()
		if attempt != nil {
			drained = wait(attempt.done)
			if drained && attempt.relay != nil {
				attempt.relay.mu.Lock()
				relayIdle := attempt.relay.idle
				attempt.relay.mu.Unlock()
				drained = wait(relayIdle)
			}
		}
	}
	if !drained {
		l.mu.Lock()
		l.state = lifecycleActive
		l.mu.Unlock()
		return &rpcError{Code: "plugin_busy", Message: "plugin still has active requests or callbacks", HTTPStatus: http.StatusServiceUnavailable}
	}

	generation.shutdown()
	l.mu.Lock()
	l.state = lifecycleQuiesced
	l.mu.Unlock()
	return nil
}

func (l *nativeLifecycle) shutdown() {
	l.transition.Lock()
	defer l.transition.Unlock()
	l.mu.Lock()
	l.state = lifecycleClosed
	generation := l.generation
	l.mu.Unlock()
	generation.shutdown()
}
