package main

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

func TestRegionNormalize(t *testing.T) {
	// OTD display area from this machine: 2048x1152 centred at (3072, 576) = DP-3
	r := RegionFromOTD(&otdArea{Width: 2048, Height: 1152, X: 3072, Y: 576}, 1, 1, 0, 0)
	if u, v := r.Normalize(2936, 958); !near(u, 0.43359) || !near(v, 0.83160) {
		t.Fatalf("got %v %v", u, v)
	}
	// rotated 90°: the area's +x axis points down the screen
	rr := Region{CX: 0, CY: 0, W: 2, H: 2, Rot: math.Pi / 2}
	if u, v := rr.Normalize(0, 1); !near(u, 1) || !near(v, 0.5) {
		t.Fatalf("rotated: got %v %v", u, v)
	}
	if _, err := ParseRegion("1,2,0,4"); err == nil {
		t.Fatal("expected error for zero width")
	}
	if r, _ := ParseRegion("100,50,200,100"); !near(r.CX, 200) || !near(r.CY, 100) {
		t.Fatalf("parse: %+v", r)
	}
}

func TestOtdTabletMapping(t *testing.T) {
	// XP-Pen Star G640S digitizer (raw units) through the full-size OTD tablet area
	s := &otdRPCSource{hub: NewHub(0),
		specs: map[string]otdSpecs{"T": {Digitizer: &struct{ Width, Height, MaxX, MaxY float64 }{164.995, 102.995, 32999, 20599}}},
		areas: map[string]*otdArea{"T": {Width: 165, Height: 103, X: 82.5, Y: 51.5}}}
	s.onReport([]byte(`{"Tablet":{"Properties":{"Name":"T"}},"Path":"X","Data":{"Position":{"X":16500,"Y":10300}}}`))
	if string(s.hub.posFrame[2:]) != `{"t":"pos","u":0.50000,"v":0.50000}` {
		t.Fatalf("got %s", s.hub.posFrame[2:])
	}
	s.onReport([]byte(`{"Path":"OpenTabletDriver.Plugin.Tablet.OutOfRangeReport","Data":{}}`))
	if s.hub.InRange() {
		t.Fatal("out of range report should clear InRange")
	}
}

// fakeSource: available while *avail is true, fails when it turns false.
type fakeSource struct {
	name  string
	avail *bool
	mu    *sync.Mutex
}

func (f *fakeSource) Run(ctx context.Context, hub *Hub) error {
	hub.Status(f.name, true)
	for {
		f.mu.Lock()
		ok := *f.avail
		f.mu.Unlock()
		if !ok {
			return errors.New("gone")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestRunnerFallbackAndSwitchBack(t *testing.T) {
	var mu sync.Mutex
	a, b := true, true
	registerSource(SourceInfo{Name: "A",
		Available: func(*Options) bool { mu.Lock(); defer mu.Unlock(); return a },
		New:       func(*Options) Source { return &fakeSource{"A", &a, &mu} }})
	registerSource(SourceInfo{Name: "B",
		Available: func(*Options) bool { return true },
		New:       func(*Options) Source { return &fakeSource{"B", &b, &mu} }})
	defer delete(registry, "A")
	defer delete(registry, "B")

	hub := NewHub(0)
	current := func() string {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return string(hub.state[2:])
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runSources(ctx, []string{"A", "B"}, hub, &Options{}); close(done) }()

	waitFor := func(want string, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if current() == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("want %s, have %s", want, current())
	}
	waitFor(`{"t":"status","ok":true,"source":"A"}`, time.Second)
	mu.Lock()
	a = false
	mu.Unlock()
	waitFor(`{"t":"status","ok":true,"source":"B"}`, 3*time.Second)
	mu.Lock()
	a = true
	mu.Unlock()
	waitFor(`{"t":"status","ok":true,"source":"A"}`, 5*time.Second)
	cancel()
	<-done
}
