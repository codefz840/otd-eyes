// Tablet Eyes: pen position -> OBS Browser Source.
//
// Reads the pen position from an input source, normalizes it to 0..1 inside the
// tablet / display area, and pushes it to every browser on ws://HOST:PORT/ws.
// Serves the eyes page (built into the binary) at http://HOST:PORT/.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed eyes.html
var eyesHTML []byte

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".png": "image/png", ".js": "text/javascript",
	".css": "text/css", ".svg": "image/svg+xml",
}

func main() {
	log.SetFlags(log.Ltime)
	opt := &Options{}
	host := flag.String("host", "127.0.0.1", "listen address")
	port := flag.Int("port", 8765, "listen port")
	source := flag.String("source", "auto", "auto, or a comma separated priority list of input sources")
	web := flag.String("web", "", "serve eyes.html (and images) from this folder instead of the built-in page")
	flag.StringVar(&opt.Region, "region", "", "evdev-otd / cursor: desktop-pixel `X,Y,W,H` that maps to 0..1 (default: the OTD display area)")
	flag.StringVar(&opt.Tablet, "tablet", "", "OTD profile (tablet name) to read areas from (default: first)")
	flag.StringVar(&opt.Device, "device", "", "evdev-tablet: device name `regex`")
	flag.StringVar(&opt.Socket, "socket", "", "otd-rpc: daemon socket / pipe path")
	flag.Float64Var(&opt.Rate, "rate", 500, "max position messages per second (0 = no limit)")
	flag.DurationVar(&opt.IdleTimeout, "idle-timeout", time.Second, "evdev-otd: no movement this long counts as pen out of range (0 = never)")
	flag.Usage = func() {
		names := make([]string, 0, len(registry))
		for n := range registry {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n\nInput sources on this OS: %s\nauto order: %s\n\nOptions:\n",
			filepath.Base(os.Args[0]), strings.Join(names, ", "), strings.Join(autoOrder, " -> "))
		flag.PrintDefaults()
	}
	flag.Parse()

	if opt.Region != "" {
		if _, err := ParseRegion(opt.Region); err != nil {
			log.Fatal(err)
		}
	}
	names := autoOrder
	if *source != "auto" {
		names = nil
		for _, n := range strings.Split(*source, ",") {
			n = strings.TrimSpace(n)
			if _, ok := registry[n]; !ok {
				log.Fatalf("unknown input source %q for this OS (see -h)", n)
			}
			names = append(names, n)
		}
	}

	hub := NewHub(opt.Rate)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { servePage(w, r, *web) })

	addr := net.JoinHostPort(*host, fmt.Sprint(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	log.Printf("Eyes page: http://%s/   (add this as an OBS Browser Source)", addr)
	log.Printf("Input sources, in priority order: %s", strings.Join(names, ", "))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runSources(ctx, names, hub, opt)
	srv.Close()
	log.Print("bye")
}

func servePage(w http.ResponseWriter, r *http.Request, webDir string) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "eyes.html"
	}
	if strings.ContainsAny(name, `/\`) || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	ctype, ok := contentTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body []byte
	if webDir != "" {
		b, err := os.ReadFile(filepath.Join(webDir, name))
		if err != nil && !(name == "eyes.html" && errors.Is(err, os.ErrNotExist)) {
			http.NotFound(w, r)
			return
		}
		body = b
	}
	if body == nil {
		if name != "eyes.html" {
			http.NotFound(w, r)
			return
		}
		body = eyesHTML
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}
