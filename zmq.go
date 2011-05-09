// Package zmq implements the ØMQ socket types and wire protocol.
// For more information, see http://zeromq.org.
package zmq

import (
	"bufio"
	"encoding/binary"
	"io"
	"io/ioutil"
	"net"
	"os"
	"sync"
)

const (
	flagMore = 1
)

const (
	SOCK_PAIR = iota
	SOCK_PUB
	SOCK_SUB
	SOCK_REQ
	SOCK_REP
	//SOCK_XREQ
	//SOCK_XREP
	SOCK_PULL
	SOCK_PUSH
)

type nilWAdder struct {
	net.Conn
}

func (b nilWAdder) addConn(wc io.WriteCloser) {}

type nilRAdder struct{}

func (b nilRAdder) addConn(fr *frameReader) {}

type reader interface {
	addConn(fr *frameReader)
	RecvMsg() (*Msg, os.Error)
	Close() os.Error
}

type bindWriter interface {
	io.WriteCloser
	addConn(wc io.WriteCloser)
}

// A Context is a container for sockets and manages in-process communications.
// Contexts are thread safe and may be shared between goroutines without
// synchronization.
type Context struct {
	endpoints map[string]net.Conn
	epLock sync.Mutex
}

// NewContext returns a new context.
func NewContext() (*Context, os.Error) {
	return &Context{endpoints: map[string]net.Conn{}}, nil
}

func (c *Context) registerEndpoint(name string) (net.Conn, os.Error) {
	c1, c2 := net.Pipe()
	c.epLock.Lock()
	defer c.epLock.Unlock()
	if _, ok := c.endpoints[name]; ok {
		return nil, os.NewError("endpoint already exists")
	}
	c.endpoints[name] = c2
	return c1, nil
}

func (c *Context) findEndpoint(name string) (net.Conn, os.Error) {
	c.epLock.Lock()
	defer c.epLock.Unlock()
	if conn, ok := c.endpoints[name]; ok {
		return conn, nil
	}
	return nil, os.NewError("endpoint does not exist")
}

// Similar to io.MultiWriter, but we have access to its internals and it has a Close method.
type multiWriter []io.WriteCloser

func newMultiWriter() *multiWriter {
	mw := make(multiWriter, 0, 5)
	return &mw
}

func (mw *multiWriter) Write(p []byte) (n int, err os.Error) {
	n = len(p)
	for _, w := range *mw {
		n2, err2 := w.Write(p)
		if err2 != nil {
			n = n2
			err = err2
		}
	}
	return
}

func (mw *multiWriter) Close() (err os.Error) {
	for _, w := range *mw {
		err2 := w.Close()
		if err2 != nil {
			err = err2
		}
	}
	return
}

func (mw *multiWriter) addConn(wc io.WriteCloser) {
	*mw = append(*mw, wc)
}

// a load-balanced WriteCloser
type lbWriter struct {
	w []io.WriteCloser
	c chan []byte
}

func newLbWriter() *lbWriter {
	c := make(chan []byte, 10)
	return &lbWriter{nil, c}
}

func (w *lbWriter) addConn(wc io.WriteCloser) {
	go writeListen(wc, w.c)
	// TODO: figure out a better way to keep track of writers
	w.w = append(w.w, wc)
}

func writeListen(w io.WriteCloser, c chan []byte) {
	for {
		b, ok := <-c
		if !ok {
			w.Close()
			break
		}
		if _, err := w.Write(b); err != nil {
			// pass it on to a different writer
			c <- b
			break
		}
	}
}

func (w *lbWriter) Write(b []byte) (int, os.Error) {
	w.c <- b
	// TODO: can we do better?
	return len(b), nil
}

func (w *lbWriter) Close() os.Error {
	close(w.c)
	return nil
}

type queuedReader struct {
	fr []*frameReader
	c  chan *Msg
}

func newQueuedReader() *queuedReader {
	c := make(chan *Msg, 10)
	return &queuedReader{nil, c}
}

func (r *queuedReader) addConn(fr *frameReader) {
	go readListen(fr, r.c)
	// TODO: figure out a better way to keep track of readers
	r.fr = append(r.fr, fr)
}

func readListen(fr *frameReader, c chan *Msg) {
	for {
		mr, err := fr.RecvMsg()
		if err != nil {
			break
		}
		c <- mr
	}
}

func (r *queuedReader) RecvMsg() (*Msg, os.Error) {
	mr := <-r.c
	return mr, nil
}

func (r *queuedReader) Close() os.Error {
	for _, r := range r.fr {
		r.Close()
	}
	return nil
}

type frameWriter struct {
	bindWriter
	buf *bufio.Writer
}

func newFrameWriter(wc bindWriter) *frameWriter {
	w := &frameWriter{wc, bufio.NewWriter(wc)}
	return w
}

func (fw *frameWriter) sendIdentity(id string) os.Error {
	var b []byte
	if id != "" {
		b = []byte(id)
	}
	_, err := fw.write(b, 0)
	return err
}

func (fw *frameWriter) write(b []byte, flags byte) (n int, err os.Error) {
	// + 1 for flags
	l := len(b) + 1
	if l < 255 {
		n, err = fw.buf.Write([]byte{byte(l)})
	} else {
		var length [9]byte
		length[0] = 255
		binary.BigEndian.PutUint64(length[1:], uint64(l))
		n, err = fw.buf.Write(length[:])
	}
	if err != nil {
		return
	}
	err = fw.buf.WriteByte(flags)
	if err != nil {
		return
	}
	n++
	nn, err := fw.buf.Write(b)
	n += nn
	fw.buf.Flush()
	return
}

func (fw *frameWriter) ReadFrom(r io.Reader) (n int64, err os.Error) {
	// same size as io.Copy + header
	buf := make([]byte, 32*1024)
	flags := byte(flagMore)
	for {
		nn, err := r.Read(buf)
		n += int64(nn)
		if err != nil {
			if err == os.EOF {
				flags = 0
			} else {
				return n, err
			}
		}
		fw.write(buf[:nn], flags)
	}
	return
}

type frameReader struct {
	nilRAdder
	lock sync.Mutex
	rc   io.ReadCloser
	buf  *bufio.Reader
}

// A Msg represents a ØMQ message. Only one Msg from an endpoint can be
// active at a time. The caller must Close the Msg when finished with it.
type Msg struct {
	length uint64 // length of the current frame
	more   bool   // whether there are more frames after this one
	buf    *bufio.Reader
	lock   *sync.Mutex
}

func newMsg(buf *bufio.Reader, lock *sync.Mutex) (*Msg, os.Error) {
	m := &Msg{buf: buf, lock: lock}
	err := m.readHeader()
	return m, err
}

func (m *Msg) readHeader() os.Error {
	var b [8]byte
	if _, err := m.buf.Read(b[:1]); err != nil {
		return err
	}
	if b[0] == 255 {
		if _, err := m.buf.Read(b[:]); err != nil {
			return err
		}
		m.length = binary.BigEndian.Uint64(b[:])
	} else {
		m.length = uint64(b[0])
	}
	m.length--
	flags, err := m.buf.ReadByte()
	if err != nil {
		return err
	}
	m.more = flags&flagMore != 0
	return nil
}

func (m *Msg) Read(b []byte) (n int, err os.Error) {
	if m.length == 0 && !m.more {
		return 0, os.EOF
	}
	for n < len(b) {
		l := uint64(len(b) - n)
		if m.length < l {
			l = m.length
		}
		nn, err := m.buf.Read(b[n : n+int(l)])
		n += nn
		m.length -= uint64(nn)
		if err != nil {
			return n, err
		}
		if m.length == 0 {
			if m.more {
				m.readHeader()
			} else {
				return n, os.EOF
			}
		}
	}
	return
}

// discard reads the rest of the data off the wire.
func (m *Msg) discard() {
	io.Copy(ioutil.Discard, m)
}

const maxInt = int(^uint(0)/2)

// Len returns the message's length. If the length is unknown or too large for an int to
// hold, Len returns -1.
func (m *Msg) Len() int {
	if m.more || m.length > uint64(maxInt) {
		return -1
	}
	return int(m.length)
}

// Close unlocks the associated Socket so that another message can be read,
// discarding any unread data.
func (m *Msg) Close() os.Error {
	m.discard()
	m.lock.Unlock()
	return nil
}

func newFrameReader(rc io.ReadCloser) *frameReader {
	r := &frameReader{rc: rc, buf: bufio.NewReader(rc)}
	return r
}

func (fr *frameReader) RecvMsg() (*Msg, os.Error) {
	fr.lock.Lock()
	return newMsg(fr.buf, &fr.lock)
}

func (fr *frameReader) Close() os.Error {
	return fr.rc.Close()
}
