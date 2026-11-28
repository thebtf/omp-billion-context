package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const (
	// One frame carries a whole projected request body.
	kernelMaxFrame   = 64 << 20
	kernelWriteDepth = 64
	kernelMaxPending = 256
	kernelReadBuffer = 64 << 10
)

var (
	errKernelExited   = errors.New("compression kernel exited")
	errKernelProtocol = errors.New("compression kernel broke the framing protocol")
)

// kernelRefusal is a framed answer the worker chose to give: the stream stays
// usable and the caller turns the text into feedback for the model.
type kernelRefusal struct{ message string }

func (e *kernelRefusal) Error() string { return e.message }

type kernelReply struct {
	result json.RawMessage
	err    error
}

type kernelFrame struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// kernelProcess is one private Node interpreter speaking newline-delimited JSON
// on its stdio. A single writer serializes frames and replies are matched by
// request id, so concurrent callers never interleave and never see each other's
// answers. stdout carries only IPC; stderr is drained and discarded so worker
// diagnostics cannot leak request text to a client.
type kernelProcess struct {
	stdin io.WriteCloser
	out   chan []byte
	done  chan struct{}
	next  atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan kernelReply
	closed  bool
	failure error
}

func newKernelProcess(stdin io.WriteCloser, stdout, stderr io.Reader) *kernelProcess {
	k := &kernelProcess{
		stdin:   stdin,
		out:     make(chan []byte, kernelWriteDepth),
		done:    make(chan struct{}),
		pending: make(map[uint64]chan kernelReply),
	}
	go k.write()
	go k.read(stdout)
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return k
}

func (k *kernelProcess) call(ctx context.Context, op string, fields map[string]any, into any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if op == "" {
		return errors.New("kernel operation is required")
	}
	envelope := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		if key == "id" || key == "op" {
			return errors.New("kernel field name is reserved")
		}
		envelope[key] = value
	}
	id := k.next.Add(1)
	envelope["id"], envelope["op"] = id, op
	frame, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(frame)+1 > kernelMaxFrame {
		return errors.New("kernel request is too large")
	}
	frame = append(frame, '\n')

	reply := make(chan kernelReply, 1)
	k.mu.Lock()
	if k.closed {
		failure := k.failure
		k.mu.Unlock()
		return failure
	}
	if len(k.pending) >= kernelMaxPending {
		k.mu.Unlock()
		return errors.New("too many kernel requests in flight")
	}
	k.pending[id] = reply
	k.mu.Unlock()
	defer k.forget(id)

	select {
	case k.out <- frame:
	case <-ctx.Done():
		return ctx.Err()
	case <-k.done:
		return k.cause()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case answered := <-reply:
		if answered.err != nil {
			return answered.err
		}
		if into == nil || len(answered.result) == 0 || string(answered.result) == "null" {
			return nil
		}
		return json.Unmarshal(answered.result, into)
	}
}

func (k *kernelProcess) forget(id uint64) {
	k.mu.Lock()
	delete(k.pending, id)
	k.mu.Unlock()
}

func (k *kernelProcess) write() {
	for {
		select {
		case <-k.done:
			return
		case frame := <-k.out:
			if _, err := k.stdin.Write(frame); err != nil {
				k.fail(errKernelExited)
				return
			}
		}
	}
}

func (k *kernelProcess) read(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, kernelReadBuffer)
	for {
		line, err := kernelLine(reader)
		if err != nil {
			k.fail(err)
			return
		}
		if len(line) == 0 {
			continue
		}
		var frame kernelFrame
		if json.Unmarshal(line, &frame) != nil || frame.ID == 0 {
			k.fail(errKernelProtocol)
			return
		}
		k.deliver(frame)
	}
}

func (k *kernelProcess) deliver(frame kernelFrame) {
	k.mu.Lock()
	waiting, known := k.pending[frame.ID]
	if known {
		delete(k.pending, frame.ID)
	}
	k.mu.Unlock()
	if !known {
		// A caller gave up before the answer arrived. The frame is dropped and
		// the stream keeps its alignment.
		return
	}
	answered := kernelReply{result: frame.Result}
	switch {
	case frame.OK:
	case frame.Error != "":
		answered.err = &kernelRefusal{message: frame.Error}
	default:
		answered.err = &kernelRefusal{message: "the kernel refused the request"}
	}
	waiting <- answered
}

func (k *kernelProcess) fail(cause error) {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.closed = true
	k.failure = errKernelExited
	if errors.Is(cause, errKernelProtocol) {
		k.failure = errKernelProtocol
	}
	pending := k.pending
	k.pending = make(map[uint64]chan kernelReply)
	failure := k.failure
	k.mu.Unlock()
	close(k.done)
	for _, waiting := range pending {
		waiting <- kernelReply{err: failure}
	}
}

// cause reports why an already-open process stopped answering.
func (k *kernelProcess) cause() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return k.failure
	}
	return errKernelExited
}

func (k *kernelProcess) exited() <-chan struct{} { return k.done }

// close stops the writer and closes stdin so the worker can finish its own
// flush and exit. The process itself is reaped by its owner.
func (k *kernelProcess) close() {
	k.mu.Lock()
	closed := k.closed
	k.mu.Unlock()
	if !closed {
		_ = k.stdin.Close()
	}
}

// kernelLine reads one frame, refusing anything past the frame ceiling instead
// of growing an unbounded buffer on a worker that never sends a newline.
func kernelLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > kernelMaxFrame {
			return nil, errKernelProtocol
		}
		line = append(line, fragment...)
		if err == nil {
			line = line[:len(line)-1]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, errKernelExited
		}
	}
}
