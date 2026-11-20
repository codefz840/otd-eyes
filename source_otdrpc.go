package main

// otd-rpc: OpenTabletDriver daemon JSON-RPC (StreamJsonRpc, Content-Length framing).
// Turns on tablet debug reports and maps the raw pen position through the OTD
// tablet area. Linux: Unix socket; Windows: named pipe (see otdrpc_*.go).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

const otdPipeName = "OpenTabletDriver.Daemon"

func init() {
	registerSource(SourceInfo{
		Name:      "otd-rpc",
		Available: func(o *Options) bool { return otdEndpointExists(otdEndpoint(o)) },
		New:       func(o *Options) Source { return &otdRPCSource{opt: o} },
	})
}

func otdEndpoint(o *Options) string {
	if o.Socket != "" {
		return o.Socket
	}
	return defaultOtdEndpoint()
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type otdRPCSource struct {
	opt     *Options
	hub     *Hub
	conn    io.ReadWriteCloser
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse

	smu    sync.Mutex
	specs  map[string]otdSpecs // tablet name -> specifications
	areas  map[string]*otdArea // tablet name -> tablet area (mm)
	warned map[string]bool
}

type otdSpecs struct {
	Digitizer *struct{ Width, Height, MaxX, MaxY float64 }
	Pen       *struct{ MaxPressure float64 }
}

func (s *otdRPCSource) send(obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.conn.Write(append([]byte("Content-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"), body...))
	return err
}

func (s *otdRPCSource) call(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	ch := make(chan rpcResponse, 1)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

func (s *otdRPCSource) readLoop() error {
	r := textproto.NewReader(bufio.NewReaderSize(s.conn, 64*1024))
	for {
		hdr, err := r.ReadMIMEHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("daemon closed the connection")
			}
			return err
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil || n < 0 || n > 64<<20 {
			return fmt.Errorf("bad Content-Length %q", hdr.Get("Content-Length"))
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r.R, body); err != nil {
			return err
		}
		var msg struct {
			ID     *int
			Method string
			Params []json.RawMessage
			Result json.RawMessage
			Error  json.RawMessage
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "" && msg.ID != nil:
			s.mu.Lock()
			ch := s.pending[*msg.ID]
			s.mu.Unlock()
			if ch != nil {
				resp := rpcResponse{result: msg.Result}
				if len(msg.Error) > 0 && string(msg.Error) != "null" {
					resp.err = fmt.Errorf("rpc error: %s", msg.Error)
				}
				ch <- resp
			}
		case msg.Method == "DeviceReport" && len(msg.Params) > 0:
			s.onReport(msg.Params[len(msg.Params)-1])
		}
	}
}

func (s *otdRPCSource) Run(ctx context.Context, hub *Hub) error {
	ep := otdEndpoint(s.opt)
	conn, err := dialOtd(ctx, ep)
	if err != nil {
		return unavailable("cannot connect to %s: %v", ep, err)
	}
	s.conn = conn
	s.pending = map[int]chan rpcResponse{}
	s.specs, s.areas, s.warned = map[string]otdSpecs{}, map[string]*otdArea{}, map[string]bool{}
	s.hub = hub

	readErr := make(chan error, 1)
	go func() { readErr <- s.readLoop() }()
	defer func() {
		// notification (no reply expected): stop the daemon's debug stream
		s.send(map[string]any{"jsonrpc": "2.0", "method": "SetTabletDebug", "params": []any{false}})
		conn.Close()
	}()

	if err := s.refresh(); err != nil {
		return err
	}
	if _, err := s.call("SetTabletDebug", true); err != nil {
		return err
	}
	log.Printf("otd-rpc: connected to %s, tablet debug reports enabled", ep)
	hub.Status("otd-rpc", true)

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case <-tick.C:
			// Closing the OTD GUI tablet debugger turns debug reports off again,
			// and settings may change: re-assert both every few seconds.
			if _, err := s.call("SetTabletDebug", true); err != nil {
				return err
			}
			if err := s.refresh(); err != nil {
				return err
			}
		}
	}
}

func (s *otdRPCSource) refresh() error {
	raw, err := s.call("GetTablets")
	if err != nil {
		return err
	}
	var tablets []struct {
		Properties struct {
			Name           string
			Specifications otdSpecs
		}
	}
	json.Unmarshal(raw, &tablets)
	if raw, err = s.call("GetSettings"); err != nil {
		return err
	}
	var settings otdSettings
	json.Unmarshal(raw, &settings)

	s.smu.Lock()
	defer s.smu.Unlock()
	for _, t := range tablets {
		if t.Properties.Name != "" && t.Properties.Specifications.Digitizer != nil {
			s.specs[t.Properties.Name] = t.Properties.Specifications
		}
	}
	for _, p := range settings.Profiles {
		if p.Tablet != "" && p.AbsoluteModeSettings != nil && p.AbsoluteModeSettings.Tablet != nil {
			s.areas[p.Tablet] = p.AbsoluteModeSettings.Tablet
		}
	}
	return nil
}

type otdReport struct {
	Tablet struct{ Properties struct{ Name string } }
	Path   string
	Data   struct {
		Position *struct{ X, Y float64 }
		Pressure *float64
	}
}

func (s *otdRPCSource) onReport(raw json.RawMessage) {
	var rep otdReport
	if json.Unmarshal(raw, &rep) != nil {
		return
	}
	if strings.Contains(rep.Path, "OutOfRange") {
		s.hub.PenOut()
		return
	}
	if rep.Data.Position == nil {
		s.smu.Lock()
		if !s.warned[rep.Path] { // aux buttons etc.; log once
			s.warned[rep.Path] = true
			log.Printf("otd-rpc: ignoring report type %s", rep.Path)
		}
		s.smu.Unlock()
		return
	}

	s.smu.Lock()
	spec, ok := s.specs[rep.Tablet.Properties.Name]
	if !ok && len(s.specs) == 1 {
		for _, v := range s.specs {
			spec = v
		}
	}
	area := s.areas[rep.Tablet.Properties.Name]
	s.smu.Unlock()

	d := spec.Digitizer
	if d == nil || d.MaxX <= 0 || d.MaxY <= 0 {
		return
	}
	if area == nil {
		area = &otdArea{Width: d.Width, Height: d.Height, X: d.Width / 2, Y: d.Height / 2}
	}
	// raw units -> mm, then into the tablet area
	u, v := RegionFromOTD(area, 1, 1, 0, 0).Normalize(
		rep.Data.Position.X/d.MaxX*d.Width, rep.Data.Position.Y/d.MaxY*d.Height)
	p := -1.0
	if rep.Data.Pressure != nil && spec.Pen != nil && spec.Pen.MaxPressure > 0 {
		p = *rep.Data.Pressure / spec.Pen.MaxPressure
	}
	s.hub.Pen(u, v, p)
}
