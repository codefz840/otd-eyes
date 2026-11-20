package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Options are the command line settings sources may use.
type Options struct {
	Region      string        // desktop-pixel X,Y,W,H for evdev-otd / cursor
	Tablet      string        // OTD profile (tablet name)
	Device      string        // evdev-tablet: device name regex
	Socket      string        // otd-rpc endpoint
	Rate        float64       // max messages per second
	IdleTimeout time.Duration // evdev-otd: no movement this long = pen out
}

// ErrUnavailable marks "this source cannot start here" (missing device/daemon,
// no permission, wrong OS…); the runner then tries the next one.
var ErrUnavailable = errors.New("unavailable")

func unavailable(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, a...))
}

// Source is one way of getting the pen position. Run streams positions into the
// hub until the source is lost or ctx is cancelled (used to switch sources).
type Source interface {
	Run(ctx context.Context, hub *Hub) error
}

// SourceInfo registers a source. Adding a new way to get the pen position = one
// Source implementation + one registerSource call; nothing else changes.
type SourceInfo struct {
	Name      string
	Available func(*Options) bool // cheap check whether Run has a chance right now
	New       func(*Options) Source
}

var (
	registry  = map[string]SourceInfo{}
	autoOrder []string // set per OS, highest priority first
)

func registerSource(s SourceInfo) { registry[s.Name] = s }

// runSources runs the first available source; falls back down the list, and
// switches back up as soon as a higher-priority source becomes available again.
func runSources(ctx context.Context, names []string, hub *Hub, opt *Options) {
	backoff := map[string]time.Time{}
	reasons := map[string]string{}
	usable := func(n string) bool {
		return time.Now().After(backoff[n]) && registry[n].Available(opt)
	}
	waiting := false

	for ctx.Err() == nil {
		name := ""
		for _, n := range names {
			if usable(n) {
				name = n
				break
			}
		}
		if name == "" {
			if !waiting {
				log.Printf("no input source available yet (%v), waiting…", names)
				hub.Status("none", false)
				waiting = true
			}
			sleep(ctx, 2*time.Second)
			continue
		}
		waiting = false

		sctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- registry[name].New(opt).Run(sctx, hub) }()

		var err error
		finished := false
		tick := time.NewTicker(3 * time.Second)
	watch:
		for {
			select {
			case err = <-done:
				finished = true
				break watch
			case <-ctx.Done():
				break watch
			case <-tick.C:
				for _, n := range names {
					if n == name {
						break
					}
					if usable(n) {
						log.Printf("%s is available, switching from %s", n, name)
						break watch
					}
				}
			}
		}
		tick.Stop()
		cancel()
		if !finished {
			<-done
		} else if errors.Is(err, ErrUnavailable) {
			if reasons[name] != err.Error() {
				reasons[name] = err.Error()
				log.Printf("%s: %v", name, err)
			}
			backoff[name] = time.Now().Add(15 * time.Second)
		} else {
			delete(reasons, name)
			if err == nil {
				log.Printf("%s: stopped", name)
			} else {
				log.Printf("%s: stopped: %v", name, err)
			}
			backoff[name] = time.Now().Add(time.Second)
		}
		hub.Status(name, false)
		sleep(ctx, 500*time.Millisecond)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
