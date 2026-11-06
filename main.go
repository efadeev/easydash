package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
)

//go:embed index.html
var indexHTML []byte

func main() {
	port := flag.Int("port", 8000, "local dashboard port")
	interval := flag.Int("interval", 2, "sampling interval in seconds")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: easydash [flags] <host>\n\nhost is an ssh destination: user@host or an alias from ~/.ssh/config\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *interval < 1 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetPrefix("easydash: ")
	log.SetFlags(0)

	c := NewCollector(flag.Arg(0), *interval)
	go c.Run()

	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	http.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(c.Snapshot())
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/"
	log.Printf("%s -> %s", flag.Arg(0), url)
	openBrowser(url)
	log.Fatal(http.Serve(ln, nil))
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	}
}
