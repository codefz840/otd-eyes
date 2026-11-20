package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Hub fans pen state out to every connected browser. Input sources only call
// Pen / PenOut / Status; the wire format lives here.
type Hub struct {
	mu       sync.Mutex
	clients  map[*wsClient]struct{}
	minDt    time.Duration
	lastPos  time.Time
	inRange  bool
	state    []byte // last status frame
	posFrame []byte // last position frame (replayed to new clients)
}

type wsClient struct{ ch chan []byte }

func NewHub(rateHz float64) *Hub {
	h := &Hub{clients: map[*wsClient]struct{}{}}
	if rateHz > 0 {
		h.minDt = time.Duration(float64(time.Second) / rateHz)
	}
	h.state = wsFrame([]byte(`{"t":"status","ok":false,"source":null}`), 0x1)
	return h
}

// Pen reports the pen at (u, v): 0..1 inside the configured area (values outside
// are allowed). pressure < 0 means unknown.
func (h *Hub) Pen(u, v, pressure float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if h.inRange && now.Sub(h.lastPos) < h.minDt {
		return
	}
	h.lastPos, h.inRange = now, true
	var msg string
	if pressure >= 0 {
		msg = fmt.Sprintf(`{"t":"pos","u":%.5f,"v":%.5f,"p":%.4f}`, u, v, pressure)
	} else {
		msg = fmt.Sprintf(`{"t":"pos","u":%.5f,"v":%.5f}`, u, v)
	}
	h.posFrame = wsFrame([]byte(msg), 0x1)
	h.broadcast(h.posFrame)
}

// PenOut reports that the pen left the tablet.
func (h *Hub) PenOut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.penOutLocked()
}

func (h *Hub) penOutLocked() {
	if h.inRange {
		h.inRange, h.posFrame = false, nil
		h.broadcast(wsFrame([]byte(`{"t":"out"}`), 0x1))
	}
}

func (h *Hub) InRange() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inRange
}

func (h *Hub) Status(source string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = wsFrame([]byte(fmt.Sprintf(`{"t":"status","ok":%t,"source":%q}`, ok, source)), 0x1)
	if !ok {
		h.penOutLocked()
	}
	h.broadcast(h.state)
}

func (h *Hub) broadcast(frame []byte) {
	for c := range h.clients {
		select {
		case c.ch <- frame:
		default: // client not keeping up: drop instead of queueing forever
		}
	}
}

// ---- WebSocket (server side, text frames only) ------------------------------

func wsFrame(payload []byte, opcode byte) []byte {
	n := len(payload)
	var head []byte
	switch {
	case n < 126:
		head = []byte{0x80 | opcode, byte(n)}
	case n < 65536:
		head = []byte{0x80 | opcode, 126, byte(n >> 8), byte(n)}
	default:
		head = make([]byte, 10)
		head[0], head[1] = 0x80|opcode, 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	return append(head, payload...)
}

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	if rw.Flush() != nil {
		return
	}

	c := &wsClient{ch: make(chan []byte, 256)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	c.ch <- h.state
	if h.posFrame != nil {
		c.ch <- h.posFrame
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wsReadLoop(rw.Reader, c)
	}()
	for {
		select {
		case f := <-c.ch:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write(f); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

// wsReadLoop reads (and ignores) client frames; answers pings, returns on close/error.
func wsReadLoop(r *bufio.Reader, c *wsClient) {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr[:2]); err != nil {
			return
		}
		op, masked, n := hdr[0]&0x0F, hdr[1]&0x80 != 0, uint64(hdr[1]&0x7F)
		switch n {
		case 126:
			if _, err := io.ReadFull(r, hdr[:2]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(hdr[:2]))
		case 127:
			if _, err := io.ReadFull(r, hdr[:8]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(hdr[:8])
		}
		if n > 1<<20 {
			return
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(r, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch op {
		case 0x8:
			return
		case 0x9:
			select {
			case c.ch <- wsFrame(payload, 0xA):
			default:
			}
		}
	}
}
