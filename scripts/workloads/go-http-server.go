package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"
)

func main() {
	addr := ":18080"
	if v := os.Getenv("ANVIL_GO_HTTP_ADDR"); v != "" {
		addr = v
	}

	started := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service":    "anvil-go-http",
			"status":     "ok",
			"go_version": runtime.Version(),
			"started_at": started.Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "anvil-go-http-ok")
	})

	log.Printf("anvil-go-http listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
