// The e2e demo app: what an indie hacker's service looks like to the
// orchestrator. Reports its version (from version.txt in the release)
// and pid; /health goes 500 while a "broken" marker file exists in the
// release dir (shipped in the artifact for the broken-deploy scenario,
// or created live via /break for the watchdog scenario); every other
// path answers 500 while an "errors" marker is in it (the release a
// post-deploy comparison must find worse than the one before). /boom exits
// the process — the crash the watchdog must recover from. /argv
// reports the arguments it was started with, Go-quoted, so the suite
// can see that a `command` word reaches the app as written.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	version := "unknown"
	if b, err := os.ReadFile("version.txt"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Per request, not once at boot: the watchdog scenario breaks
		// and heals a running instance.
		if _, err := os.Stat("broken"); err == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(50 * time.Millisecond) // let the response flush
			os.Exit(1)
		}()
	})
	http.HandleFunc("/break", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile("broken", []byte("1"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/heal", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.Remove("broken"); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/argv", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "%q", os.Args[1:]) //nolint:gosec // the test app's own argv, Go-quoted, as plain text: no markup reaches a browser
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat("errors"); err == nil {
			http.Error(w, "the errors release fails every request", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "hello %s pid %d", version, os.Getpid())
	})
	ln, err := net.Listen("unix", os.Getenv("SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
