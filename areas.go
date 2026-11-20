package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Region is a (possibly rotated) rectangle; Normalize maps a point into 0..1 inside it.
type Region struct {
	CX, CY, W, H float64
	Rot          float64 // radians
}

func RegionFromRect(x, y, w, h float64) Region { return Region{CX: x + w/2, CY: y + h/2, W: w, H: h} }

// RegionFromOTD converts an OTD centre-based area, optionally scaled/offset into another unit.
func RegionFromOTD(a *otdArea, sx, sy, ox, oy float64) Region {
	return Region{CX: a.X*sx + ox, CY: a.Y*sy + oy, W: a.Width * sx, H: a.Height * sy,
		Rot: a.Rotation * math.Pi / 180}
}

// ParseRegion parses "X,Y,W,H" (top-left based).
func ParseRegion(s string) (Region, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return Region{}, fmt.Errorf("region must be X,Y,W,H")
	}
	var f [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return Region{}, fmt.Errorf("region: %v", err)
		}
		f[i] = v
	}
	if f[2] <= 0 || f[3] <= 0 {
		return Region{}, fmt.Errorf("region: width and height must be > 0")
	}
	return RegionFromRect(f[0], f[1], f[2], f[3]), nil
}

func (r Region) Normalize(x, y float64) (float64, float64) {
	dx, dy := x-r.CX, y-r.CY
	if r.Rot != 0 {
		c, s := math.Cos(-r.Rot), math.Sin(-r.Rot)
		dx, dy = dx*c-dy*s, dx*s+dy*c
	}
	return dx/r.W + 0.5, dy/r.H + 0.5
}

func (r Region) String() string {
	return fmt.Sprintf("x=%g y=%g w=%g h=%g rot=%g", r.CX-r.W/2, r.CY-r.H/2, r.W, r.H, r.Rot*180/math.Pi)
}

// ---- OpenTabletDriver settings ----------------------------------------------

type otdArea struct {
	Width, Height, X, Y, Rotation float64
}

type otdSettings struct {
	Profiles []struct {
		Tablet               string
		AbsoluteModeSettings *struct {
			Display *otdArea
			Tablet  *otdArea
		}
	}
}

// profile returns the absolute-mode areas of the wanted tablet (or the first one).
func (s *otdSettings) areas(tablet string) (display, tab *otdArea) {
	for _, p := range s.Profiles {
		if p.AbsoluteModeSettings == nil || (tablet != "" && p.Tablet != tablet) {
			continue
		}
		return p.AbsoluteModeSettings.Display, p.AbsoluteModeSettings.Tablet
	}
	return nil, nil
}

func otdSettingsPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base, _ = os.UserHomeDir()
		}
		return filepath.Join(base, "OpenTabletDriver", "settings.json")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "OpenTabletDriver", "settings.json")
}

// OtdSettingsFile reads settings.json and reloads it when OTD saves new settings.
type OtdSettingsFile struct {
	Path, Tablet string
	mu           sync.Mutex
	mtime        time.Time
	display, tab *otdArea
}

func NewOtdSettingsFile(tablet string) *OtdSettingsFile {
	return &OtdSettingsFile{Path: otdSettingsPath(), Tablet: tablet}
}

func (f *OtdSettingsFile) load() {
	st, err := os.Stat(f.Path)
	if err != nil {
		f.display, f.tab = nil, nil
		return
	}
	if st.ModTime().Equal(f.mtime) {
		return
	}
	f.mtime = st.ModTime()
	f.display, f.tab = nil, nil
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return
	}
	var s otdSettings
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &s) == nil {
		f.display, f.tab = s.areas(f.Tablet)
	}
}

// DisplayArea is the OTD display area in desktop pixels.
func (f *OtdSettingsFile) DisplayArea() (Region, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	if f.display == nil || f.display.Width <= 0 {
		return Region{}, false
	}
	return RegionFromOTD(f.display, 1, 1, 0, 0), true
}

// TabletAreaMM is the OTD tablet area in millimetres.
func (f *OtdSettingsFile) TabletAreaMM() *otdArea {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	if f.tab == nil || f.tab.Width <= 0 {
		return nil
	}
	return f.tab
}
