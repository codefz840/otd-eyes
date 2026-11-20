package main

// cursor (Windows): poll the system cursor with GetCursorPos. Works with any tablet
// driver (OTD, Wacom, XP-Pen…) and with a mouse, as long as the driver moves the
// system cursor. Area: --region, else the OTD display area, else the primary monitor.

import (
	"context"
	"log"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	procGetCursorPos         = user32.NewProc("GetCursorPos")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	procSetDpiAwarenessCtx   = user32.NewProc("SetProcessDpiAwarenessContext")
	shcore                   = syscall.NewLazyDLL("shcore.dll")
	procSetProcessDpiAwarens = shcore.NewProc("SetProcessDpiAwareness")
)

func init() {
	autoOrder = []string{"otd-rpc", "cursor"}
	registerSource(SourceInfo{
		Name:      "cursor",
		Available: func(*Options) bool { return true },
		New:       func(o *Options) Source { return &cursorSource{opt: o} },
	})
}

type cursorSource struct{ opt *Options }

func (s *cursorSource) Run(ctx context.Context, hub *Hub) error {
	if err := procGetCursorPos.Find(); err != nil {
		return unavailable("GetCursorPos: %v", err)
	}
	// Per-monitor DPI awareness, so coordinates are real pixels on scaled monitors.
	if procSetDpiAwarenessCtx.Find() == nil {
		procSetDpiAwarenessCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	} else if procSetProcessDpiAwarens.Find() == nil {
		procSetProcessDpiAwarens.Call(2)
	}
	settings := NewOtdSettingsFile(s.opt.Tablet)
	region := func() Region {
		if s.opt.Region != "" {
			if r, err := ParseRegion(s.opt.Region); err == nil {
				return r
			}
		}
		if r, ok := settings.DisplayArea(); ok {
			return r
		}
		w, _, _ := procGetSystemMetrics.Call(0)
		h, _, _ := procGetSystemMetrics.Call(1)
		return RegionFromRect(0, 0, float64(int32(w)), float64(int32(h)))
	}
	area := region()
	log.Printf("cursor: polling GetCursorPos, area %v", area)
	hub.Status("cursor", true)

	rate := s.opt.Rate
	if rate <= 0 || rate > 1000 {
		rate = 500
	}
	tick := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer tick.Stop()
	var pt struct{ X, Y int32 }
	lastX, lastY := int32(-1<<31), int32(-1<<31)
	lastRegion := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ok == 0 {
				continue
			}
			if now.Sub(lastRegion) > 2*time.Second {
				area, lastRegion = region(), now
			}
			if pt.X != lastX || pt.Y != lastY {
				lastX, lastY = pt.X, pt.Y
				u, v := area.Normalize(float64(pt.X), float64(pt.Y))
				hub.Pen(u, v, -1)
			}
		}
	}
}
