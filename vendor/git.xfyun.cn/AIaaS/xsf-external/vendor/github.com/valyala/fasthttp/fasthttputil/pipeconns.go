package fasthttputil

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// NewPipeConns returns new bi-directional connection pipe.
func NewPipeConns() *PipeConns {
	ch1 := make(chan *byteBuffer, 4)
	ch2 := make(chan *byteBuffer, 4)

	pc := &PipeConns{
		stopCh: make(chan struct{}),
	}
	pc.c1.rCh = ch1
	pc.c1.wCh = ch2
	pc.c2.rCh = ch2
	pc.c2.wCh = ch1
	pc.c1.pc = pc
	pc.c2.pc = pc
	return pc
}

// PipeConns provides bi-directional connection pipe,
// which use in-process memory as a transport.
//
// PipeConns must be created by calling NewPipeConns.
//
// PipeConns has the following additional features comparing to connections
// returned from net.Pipe():
//
//   * It is faster.
//   * It buffers Write calls, so there is no need to have concurrent goroutine
//     calling Read in order to unblock each Write call.
//   * It supports read and write deadlines.
//
type PipeConns struct {
	c1         pipeConn
	c2         pipeConn
	stopCh     chan struct{}
	stopChLock sync.Mutex
}

// Conn1 returns the first end of bi-directional pipe.
//
// Data written to Conn1 may be read from Conn2.
// Data written to Conn2 may be read from Conn1.
func (pc *PipeConns) Conn1() net.Conn {
	return &pc.c1
}

// Conn2 returns the second end of bi-directional pipe.
//
// Data written to Conn2 may be read from Conn1.
// Data written to Conn1 may be read from Conn2.
func (pc *PipeConns) Conn2() net.Conn {
	return &pc.c2
}

// Close closes pipe connections.
func (pc *PipeConns) Close() error {
	pc.stopChLock.Lock()
	select {
	case <-pc.stopCh:
	default:
		close(pc.stopCh)
	}
	pc.stopChLock.Unlock()

	return nil
}

type pipeConn struct {
	b  *byteBuffer
	bb []byte

	rCh chan *byteBuffer
	wCh chan *byteBuffer
	pc  *PipeConns

	readDeadlineTimer  *time.Timer
	writeDeadlineTimer *time.Timer

	readDeadlineCh  <-chan time.Time
	writeDeadlineCh <-chan time.Time
}

func (c *pipeConn) Write(p []byte) (int, error) {
	b := acquireByteBuffer()
	b.b = append(b.b[:0], p...)

	select {
	case <-c.pc.stopCh:
		releaseByteBuffer(b)
		return 0, errConnectionClosed
	default:
	}

	select {
	case c.wCh <- b:
	default:
		select {
		case c.wCh <- b:
		case <-c.writeDeadlineCh:
			c.writeDeadlineCh = closedDeadlineCh
			return 0, ErrTimeout
		case <-c.pc.stopCh:
			releaseByteBuffer(b)
			return 0, errConnectionClosed
		}
	}

	return len(p), nil
}

func (c *pipeConn) Read(p []byte) (int, error) {
	mayBlock := true
	nn := 0
	for len(p) > 0 {
		n, err := c.read(p, mayBlock)
		nn += n
		if err != nil {
			if !mayBlock && err == errWouldBlock {
				err = nil
			}
			return nn, err
		}
		p = p[n:]
		mayBlock = false
	}

	return nn, nil
}

func (c *pipeConn) read(p []byte, mayBlock bool) (int, error) {
	if len(c.bb) == 0 {
		if err := c.readNextByteBuffer(mayBlock); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.bb)
	c.bb = c.bb[n:]

	return n, nil
}

func (c *pipeConn) readNextByteBuffer(mayBlock bool) error {
	releaseByteBuffer(c.b)
	c.b = nil

	select {
	case c.b = <-c.rCh:
	default:
		if !mayBlock {
			return errWouldBlock
		}
		select {
		case c.b = <-c.rCh:
		case <-c.readDeadlineCh:
			c.readDeadlineCh = closedDeadlineCh
			// rCh may contain data when deadline is reached.
			// Read the data before returning ErrTimeout.
			select {
			case c.b = <-c.rCh:
			default:
				return ErrTimeout
			}
		case <-c.pc.stopCh:
			// rCh may contain data when stopCh is closed.
			// Read the data before returning EOF.
			select {
			case c.b = <-c.rCh:
			default:
				return io.EOF
			}
		}
	}

	c.bb = c.b.b
	return nil
}

var (
	errWouldBlock       = errors.New("would block")
	errConnectionClosed = errors.New("connection closed")

	// ErrTimeout is returned from Read() or Write() on timeout.
	ErrTimeout = errors.New("timeout")
)

func (c *pipeConn) Close() error {
	return c.pc.Close()
}

func (c *pipeConn) LocalAddr() net.Addr {
	return pipeAddr(0)
}

func (c *pipeConn) RemoteAddr() net.Addr {
	return pipeAddr(0)
}

func (c *pipeConn) SetDeadline(deadline time.Time) error {
	c.SetReadDeadline(deadline)
	c.SetWriteDeadline(deadline)
	return nil
}

func (c *pipeConn) SetReadDeadline(deadline time.Time) error {
	if c.readDeadlineTimer == nil {
		c.readDeadlineTimer = time.NewTimer(time.Hour)
	}
	c.readDeadlineCh = updateTimer(c.readDeadlineTimer, deadline)
	return nil
}

func (c *pipeConn) SetWriteDeadline(deadline time.Time) error {
	if c.writeDeadlineTimer == nil {
		c.writeDeadlineTimer = time.NewTimer(time.Hour)
	}
	c.writeDeadlineCh = updateTimer(c.writeDeadlineTimer, deadline)
	return nil
}

func updateTimer(t *time.Timer, deadline time.Time) <-chan time.Time {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if deadline.IsZero() {
		return nil
	}
	d := -time.Since(deadline)
	if d <= 0 {
		return closedDeadlineCh
	}
	t.Reset(d)
	return t.C
}

var closedDeadlineCh = func() <-chan time.Time {
	ch := make(chan time.Time)
	close(ch)
	return ch
}()

type pipeAddr int

func (pipeAddr) Network() string {
	return "pipe"
}

func (pipeAddr) String() string {
	return "pipe"
}

type byteBuffer struct {
	b []byte
}

func acquireByteBuffer() *byteBuffer {
	return byteBufferPool.Get().(*byteBuffer)
}

func releaseByteBuffer(b *byteBuffer) {
	if b != nil {
		byteBufferPool.Put(b)
	}
}

var byteBufferPool = &sync.Pool{
	New: func() interface{} {
		return &byteBuffer{
			b: make([]byte, 1024),
		}
	},
}
