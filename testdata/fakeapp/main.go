// fakeapp is the app the tests supervise: a tiny HTTP server that says which build and process answered.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var version = "v1"

func main() {
	port := flag.String("port", os.Getenv("PORT"), "port")
	flag.Parse()
	if os.Getenv("FAKEAPP_FAIL") != "" {
		fmt.Println("fakeapp: FAKEAPP_FAIL is set, exiting")
		os.Exit(1)
	}
	fmt.Println("fakeapp", version, "listening on", *port)
	// /file returns a file relative to the working directory, and /cwd the directory itself: a folder app
	// is started in the folder of its version.
	http.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(r.URL.Query().Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Write(data)
	})
	http.HandleFunc("/cwd", func(w http.ResponseWriter, r *http.Request) {
		wd, _ := os.Getwd()
		fmt.Fprintln(w, wd)
	})
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, os.Getenv(r.URL.Query().Get("name")))
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %d\n", version, *port, os.Getpid())
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	// /ready fails while the file named by FAKEAPP_NOT_READY exists: a dependency that is down.
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if f := os.Getenv("FAKEAPP_NOT_READY"); f != "" {
			if _, err := os.Stat(f); err == nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		fmt.Fprintln(w, "ready")
	})
	http.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintf(w, "%s slow\n", version)
	})
	// /ws answers a protocol upgrade, says which build it is, then echoes every byte: enough to check that
	// an upgraded connection is carried through the load balancer.
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n%s\n", version)
		buf.Flush()
		io.Copy(conn, buf)
	})
	if err := http.ListenAndServe("127.0.0.1:"+*port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
