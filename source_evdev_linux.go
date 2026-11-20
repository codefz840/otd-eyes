package main

// Linux evdev sources (read /dev/input/eventN directly; needs the `input` group).
//
//   evdev-otd    "OpenTabletDriver Virtual Tablet": the cursor position OTD outputs
//                (desktop pixels, after OTD filters), mapped through the OTD display
//                area so u/v match the tablet area like otd-rpc.
//   evdev-tablet the kernel's own pen device (no OTD needed). Silent while OTD runs,
//                because OTD takes the tablet over.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"syscall"
	"time"
	"unsafe"
)

const (
	evSyn, evKey, evAbs        = 0x00, 0x01, 0x03
	synReport                  = 0
	absX, absY, absPressure    = 0x00, 0x01, 0x18
	btnToolPen, btnToolRubber  = 0x140, 0x141
	inputEventSize             = 24 // struct input_event on 64-bit
	otdVirtualTabletDeviceName = "OpenTabletDriver Virtual Tablet"
)

func init() {
	autoOrder = []string{"evdev-otd", "otd-rpc", "evdev-tablet"}
	registerSource(SourceInfo{
		Name:      "evdev-otd",
		Available: func(o *Options) bool { _, ok := findOtdVirtualTablet(); return ok },
		New:       func(o *Options) Source { return &evdevSource{opt: o, otdDevice: true} },
	})
	registerSource(SourceInfo{
		Name:      "evdev-tablet",
		Available: func(o *Options) bool { _, ok := findKernelTablet(o); return ok },
		New:       func(o *Options) Source { return &evdevSource{opt: o} },
	})
}

type inputDevice struct{ Name, Path string }

var (
	reDevName    = regexp.MustCompile(`(?m)^N: Name="(.*)"$`)
	reDevHandler = regexp.MustCompile(`(?m)^H: Handlers=.*?\b(event\d+)`)
	reBlock      = regexp.MustCompile(`\n\s*\n`)
)

// listDevices reads /proc: event numbers change across boots, names don't.
func listDevices() []inputDevice {
	data, err := os.ReadFile("/proc/bus/input/devices")
	if err != nil {
		return nil
	}
	var out []inputDevice
	for _, block := range reBlock.Split(string(data), -1) {
		n, h := reDevName.FindStringSubmatch(block), reDevHandler.FindStringSubmatch(block)
		if n != nil && h != nil {
			out = append(out, inputDevice{n[1], "/dev/input/" + h[1]})
		}
	}
	return out
}

func findOtdVirtualTablet() (inputDevice, bool) {
	for _, d := range listDevices() {
		if d.Name == otdVirtualTabletDeviceName {
			return d, true
		}
	}
	return inputDevice{}, false
}

func findKernelTablet(o *Options) (inputDevice, bool) {
	var pat *regexp.Regexp
	if o.Device != "" {
		pat, _ = regexp.Compile("(?i)" + o.Device)
	}
	for _, d := range listDevices() {
		if d.Name == otdVirtualTabletDeviceName || (pat != nil && !pat.MatchString(d.Name)) {
			continue
		}
		fd, err := syscall.Open(d.Path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		pen, _ := hasKey(uintptr(fd), btnToolPen)
		ax, err := absInfo(uintptr(fd), absX)
		syscall.Close(fd)
		if pen && err == nil && ax.Max > 0 {
			return d, true
		}
	}
	return inputDevice{}, false
}

// ---- ioctls -------------------------------------------------------------------

type absinfo struct{ Value, Min, Max, Fuzz, Flat, Resolution int32 }

func iocRead(nr, size uintptr) uintptr { return 2<<30 | size<<16 | uintptr('E')<<8 | nr }

func ioctl(fd, req uintptr, p unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p)); e != 0 {
		return e
	}
	return nil
}

func absInfo(fd uintptr, code uintptr) (absinfo, error) {
	var a absinfo
	err := ioctl(fd, iocRead(0x40+code, unsafe.Sizeof(a)), unsafe.Pointer(&a))
	return a, err
}

func hasKey(fd uintptr, code int) (bool, error) {
	var bits [96]byte
	if err := ioctl(fd, iocRead(0x20+evKey, uintptr(len(bits))), unsafe.Pointer(&bits[0])); err != nil {
		return false, err
	}
	return bits[code/8]&(1<<(code%8)) != 0, nil
}

// ---- source -------------------------------------------------------------------

type evdevSource struct {
	opt       *Options
	otdDevice bool
	settings  *OtdSettingsFile
	ax, ay    absinfo
}

func (s *evdevSource) name() string {
	if s.otdDevice {
		return "evdev-otd"
	}
	return "evdev-tablet"
}

func (s *evdevSource) region() Region {
	full := RegionFromRect(float64(s.ax.Min), float64(s.ay.Min),
		float64(s.ax.Max-s.ax.Min), float64(s.ay.Max-s.ay.Min))
	if s.otdDevice {
		if s.opt.Region != "" {
			if r, err := ParseRegion(s.opt.Region); err == nil {
				return r
			}
		}
		// The virtual tablet reports desktop pixels; OTD's display area is in the same space.
		if r, ok := s.settings.DisplayArea(); ok {
			return r
		}
		return full
	}
	// resolution = units per mm, so the OTD tablet area (mm) maps onto device units
	if a := s.settings.TabletAreaMM(); a != nil && s.ax.Resolution > 0 && s.ay.Resolution > 0 {
		return RegionFromOTD(a, float64(s.ax.Resolution), float64(s.ay.Resolution),
			float64(s.ax.Min), float64(s.ay.Min))
	}
	return full
}

func (s *evdevSource) Run(ctx context.Context, hub *Hub) error {
	var dev inputDevice
	var ok bool
	if s.otdDevice {
		dev, ok = findOtdVirtualTablet()
	} else {
		dev, ok = findKernelTablet(s.opt)
	}
	if !ok {
		return unavailable("device not found")
	}
	// O_NONBLOCK makes Go put the fd on its poller, so Close() unblocks Read().
	f, err := os.OpenFile(dev.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrPermission) {
		return unavailable("no permission for %s (add yourself to the 'input' group)", dev.Path)
	} else if err != nil {
		return unavailable("%v", err)
	}
	defer f.Close()

	var hasProx bool
	var pMax int32
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	rc.Control(func(fd uintptr) {
		if s.ax, ioErr = absInfo(fd, absX); ioErr != nil {
			return
		}
		if s.ay, ioErr = absInfo(fd, absY); ioErr != nil {
			return
		}
		if ap, err := absInfo(fd, absPressure); err == nil && ap.Max > ap.Min {
			pMax = ap.Max
		}
		hasProx, _ = hasKey(fd, btnToolPen)
	})
	if ioErr != nil {
		return unavailable("%s: %v", dev.Path, ioErr)
	}

	s.settings = NewOtdSettingsFile(s.opt.Tablet)
	region := s.region()
	idle := ""
	if !hasProx && s.opt.IdleTimeout > 0 {
		idle = fmt.Sprintf(", no proximity: idle after %v", s.opt.IdleTimeout)
	}
	log.Printf("%s: reading %q (%s), area %v%s", s.name(), dev.Name, dev.Path, region, idle)
	hub.Status(s.name(), true)

	type sample struct {
		x, y, p int32
		out     bool
	}
	samples := make(chan sample, 64)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, inputEventSize*64)
		var x, y, p int32 = -1, -1, -1
		tool := -1 // -1 = device never reported proximity
		dirty := false
		for {
			n, err := f.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			for i := 0; i+inputEventSize <= n; i += inputEventSize {
				ev := buf[i : i+inputEventSize]
				typ, code := binary.LittleEndian.Uint16(ev[16:]), binary.LittleEndian.Uint16(ev[18:])
				val := int32(binary.LittleEndian.Uint32(ev[20:]))
				switch {
				case typ == evAbs && code == absX:
					x, dirty = val, true
				case typ == evAbs && code == absY:
					y, dirty = val, true
				case typ == evAbs && code == absPressure:
					p = val
				case typ == evKey && (code == btnToolPen || code == btnToolRubber):
					tool, dirty = int(val), true
				case typ == evSyn && code == synReport && dirty:
					dirty = false
					sm := sample{x, y, p, tool == 0}
					if !sm.out && (x < 0 || y < 0) {
						continue
					}
					select {
					case samples <- sm:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastMove, lastRegion := time.Now(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if errors.Is(err, syscall.ENODEV) {
				return errors.New("device removed")
			}
			return err
		case sm := <-samples:
			if sm.out {
				hub.PenOut()
				continue
			}
			lastMove = time.Now()
			if lastMove.Sub(lastRegion) > 2*time.Second { // pick up OTD area changes
				region, lastRegion = s.region(), lastMove
			}
			u, v := region.Normalize(float64(sm.x), float64(sm.y))
			pr := -1.0
			if pMax > 0 && sm.p >= 0 {
				pr = float64(sm.p) / float64(pMax)
			}
			hub.Pen(u, v, pr)
		case <-tick.C:
			// Devices without proximity (OTD's virtual tablet) never say "pen left";
			// treat no movement for the idle timeout as out of range instead.
			if !hasProx && s.opt.IdleTimeout > 0 && hub.InRange() && time.Since(lastMove) > s.opt.IdleTimeout {
				hub.PenOut()
			}
		}
	}
}
