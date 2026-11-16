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
	fmt.Println("fakeapp", version, "listening on", *port)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %d\n", version, *port, os.Getpid())
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
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
