// +build !js

package websocket

import (
	"bufio"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"nhooyr.io/websocket/internal/errd"
)

// Writer returns a writer bounded by the context that will write
// a WebSocket message of type dataType to the connection.
//
// You must close the writer once you have written the entire message.
//
// Only one writer can be open at a time, multiple calls will block until the previous writer
// is closed.
//
// Never close the returned writer twice.
func (c *Conn) Writer(ctx context.Context, typ MessageType) (io.WriteCloser, error) {
	w, err := c.writer(ctx, typ)
	if err != nil {
		return nil, fmt.Errorf("failed to get writer: %w", err)
	}
	return w, nil
}

// Write writes a message to the connection.
//
// See the Writer method if you want to stream a message.
//
// If compression is disabled, then it is guaranteed to write the message
// in a single frame.
func (c *Conn) Write(ctx context.Context, typ MessageType, p []byte) error {
	_, err := c.write(ctx, typ, p)
	if err != nil {
		return fmt.Errorf("failed to write msg: %w", err)
	}
	return nil
}

func newMsgWriter(c *Conn) *msgWriter {
	mw := &msgWriter{
		c:  c,
		mu: newMu(c),
	}
	mw.trimWriter = &trimLastFourBytesWriter{
		w: writerFunc(mw.write),
	}
	if c.deflate() && mw.deflateContextTakeover() {
		mw.ensureFlateWriter()
	}

	return mw
}

func (mw *msgWriter) ensureFlateWriter() {
	mw.flateWriter = getFlateWriter(mw.trimWriter)
}

func (mw *msgWriter) deflateContextTakeover() bool {
	if mw.c.client {
		return mw.c.copts.clientNoContextTakeover
	}
	return mw.c.copts.serverNoContextTakeover
}

func (c *Conn) writer(ctx context.Context, typ MessageType) (io.WriteCloser, error) {
	err := c.msgWriter.reset(ctx, typ)
	if err != nil {
		return nil, err
	}
	return c.msgWriter, nil
}

func (c *Conn) write(ctx context.Context, typ MessageType, p []byte) (int, error) {
	mw, err := c.writer(ctx, typ)
	if err != nil {
		return 0, err
	}

	if !c.deflate() {
		// Fast single frame path.
		defer c.msgWriter.mu.Unlock()
		return c.writeFrame(ctx, true, c.msgWriter.opcode, p)
	}

	n, err := mw.Write(p)
	if err != nil {
		return n, err
	}

	err = mw.Close()
	return n, err
}

type msgWriter struct {
	c *Conn

	mu *mu

	deflate bool
	ctx     context.Context
	opcode  opcode
	closed  bool

	trimWriter  *trimLastFourBytesWriter
	flateWriter *flate.Writer
}

func (mw *msgWriter) reset(ctx context.Context, typ MessageType) error {
	err := mw.mu.Lock(ctx)
	if err != nil {
		return err
	}

	mw.closed = false
	mw.ctx = ctx
	mw.opcode = opcode(typ)
	mw.deflate = false
	return nil
}

// Write writes the given bytes to the WebSocket connection.
func (mw *msgWriter) Write(p []byte) (_ int, err error) {
	defer errd.Wrap(&err, "failed to write")

	if mw.closed {
		return 0, errors.New("cannot use closed writer")
	}

	if mw.c.deflate() {
		if !mw.deflate {
			if !mw.deflateContextTakeover() {
				mw.ensureFlateWriter()
			}
			mw.trimWriter.reset()
			mw.deflate = true
		}

		return mw.flateWriter.Write(p)
	}

	return mw.write(p)
}

func (mw *msgWriter) write(p []byte) (int, error) {
	n, err := mw.c.writeFrame(mw.ctx, false, mw.opcode, p)
	if err != nil {
		return n, fmt.Errorf("failed to write data frame: %w", err)
	}
	mw.opcode = opContinuation
	return n, nil
}

// Close flushes the frame to the connection.
func (mw *msgWriter) Close() (err error) {
	defer errd.Wrap(&err, "failed to close writer")

	if mw.closed {
		return errors.New("cannot use closed writer")
	}
	mw.closed = true

	if mw.c.deflate() {
		err = mw.flateWriter.Flush()
		if err != nil {
			return fmt.Errorf("failed to flush flate writer: %w", err)
		}
	}

	_, err = mw.c.writeFrame(mw.ctx, true, mw.opcode, nil)
	if err != nil {
		return fmt.Errorf("failed to write fin frame: %w", err)
	}

	if mw.deflate && !mw.deflateContextTakeover() {
		putFlateWriter(mw.flateWriter)
		mw.deflate = false
	}

	mw.mu.Unlock()
	return nil
}

func (mw *msgWriter) close() {
	if mw.c.deflate() && mw.deflateContextTakeover() {
		mw.mu.Lock(context.Background())
		putFlateWriter(mw.flateWriter)
	}
}

func (c *Conn) writeControl(ctx context.Context, opcode opcode, p []byte) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	_, err := c.writeFrame(ctx, true, opcode, p)
	if err != nil {
		return fmt.Errorf("failed to write control frame %v: %w", opcode, err)
	}
	return nil
}

// frame handles all writes to the connection.
func (c *Conn) writeFrame(ctx context.Context, fin bool, opcode opcode, p []byte) (int, error) {
	err := c.writeFrameMu.Lock(ctx)
	if err != nil {
		return 0, err
	}
	defer c.writeFrameMu.Unlock()

	select {
	case <-c.closed:
		return 0, c.closeErr
	case c.writeTimeout <- ctx:
	}

	c.writeHeader.fin = fin
	c.writeHeader.opcode = opcode
	c.writeHeader.payloadLength = int64(len(p))

	if c.client {
		c.writeHeader.masked = true
		err = binary.Read(rand.Reader, binary.LittleEndian, &c.writeHeader.maskKey)
		if err != nil {
			return 0, fmt.Errorf("failed to generate masking key: %w", err)
		}
	}

	c.writeHeader.rsv1 = false
	if c.msgWriter.deflate && (opcode == opText || opcode == opBinary) {
		c.writeHeader.rsv1 = true
	}

	err = writeFrameHeader(c.writeHeader, c.bw)
	if err != nil {
		return 0, err
	}

	n, err := c.writeFramePayload(p)
	if err != nil {
		return n, err
	}

	if c.writeHeader.fin {
		err = c.bw.Flush()
		if err != nil {
			return n, fmt.Errorf("failed to flush: %w", err)
		}
	}

	select {
	case <-c.closed:
		return n, c.closeErr
	case c.writeTimeout <- context.Background():
	}

	return n, nil
}

func (c *Conn) writeFramePayload(p []byte) (_ int, err error) {
	defer errd.Wrap(&err, "failed to write frame payload")

	if !c.writeHeader.masked {
		return c.bw.Write(p)
	}

	var n int
	maskKey := c.writeHeader.maskKey
	for len(p) > 0 {
		// If the buffer is full, we need to flush.
		if c.bw.Available() == 0 {
			err = c.bw.Flush()
			if err != nil {
				return n, err
			}
		}

		// Start of next write in the buffer.
		i := c.bw.Buffered()

		j := len(p)
		if j > c.bw.Available() {
			j = c.bw.Available()
		}

		_, err := c.bw.Write(p[:j])
		if err != nil {
			return n, err
		}

		maskKey = mask(maskKey, c.writeBuf[i:c.bw.Buffered()])

		p = p[j:]
		n += j
	}

	return n, nil
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}

// extractBufioWriterBuf grabs the []byte backing a *bufio.Writer
// and returns it.
func extractBufioWriterBuf(bw *bufio.Writer, w io.Writer) []byte {
	var writeBuf []byte
	bw.Reset(writerFunc(func(p2 []byte) (int, error) {
		writeBuf = p2[:cap(p2)]
		return len(p2), nil
	}))

	bw.WriteByte(0)
	bw.Flush()

	bw.Reset(w)

	return writeBuf
}
