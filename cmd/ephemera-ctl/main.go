// Command ephemera-ctl is a thin operator CLI over the Ephemera control-plane
// REST API (v0.4.1). It wraps spawn/list/delete of VMs, flocks, and snapshots,
// plus the access audit log and Prometheus metrics. Dependency-free (stdlib).
//
// Configuration (env):
//
//	EPHEMERA_CTL_URL    control-plane base URL (default http://127.0.0.1:3000).
//	                    Not derived from EPHEMERA_API_ADDR — that is a bind addr
//	                    and 0.0.0.0 is not dialable.
//	EPHEMERA_CTL_TOKEN  bearer token; falls back to EPHEMERA_API_TOKEN.
//	                    A --token flag on any command overrides both.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
)

const defaultURL = "http://127.0.0.1:3000"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "vm":
		vmCmd(os.Args[2:])
	case "flock":
		flockCmd(os.Args[2:])
	case "snapshot":
		snapshotCmd(os.Args[2:])
	case "audit":
		auditCmd(os.Args[2:])
	case "metrics":
		metricsCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ephemera-ctl: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ephemera-ctl — Ephemera control-plane operator CLI

Usage: ephemera-ctl <noun> <verb> [args] [--json] [--token T]

  vm spawn [--profile NAME]        flock create --task T --roles a,b,c
  vm ls [--stats]                  flock ls
  vm rm <id>                       flock get <id>
  vm health <id>                   flock rm <id>
  vm stop <id>                     flock post <id> --agent A --body B
  vm task <id> <prompt>            flock wall <id> [--history]
  vm stats <id>                    flock restart <id> <agent_id>
  vm snapshot <id> [--stop-after] [--type full|diff]
                                   flock add-agent <id> <role>
                                   flock rm-agent <id> <agent_id>
                                   flock set-role <id> <agent_id> <role>

  snapshot ls | restore <id> | rm <id>
  audit [--limit N] [--client C] [--status S] [--method M]
  metrics

Env: EPHEMERA_CTL_URL (default `+defaultURL+`), EPHEMERA_CTL_TOKEN / EPHEMERA_API_TOKEN.
`)
}

// resolveURL returns the control-plane base URL. Only EPHEMERA_CTL_URL is
// consulted (never EPHEMERA_API_ADDR, which is a bind address — 0.0.0.0 is not
// dialable).
func resolveURL() string {
	if v := os.Getenv("EPHEMERA_CTL_URL"); v != "" {
		return v
	}
	return defaultURL
}

// resolveToken applies precedence: --token override > EPHEMERA_CTL_TOKEN >
// EPHEMERA_API_TOKEN. Empty means unauthenticated (for a no-auth daemon).
func resolveToken(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("EPHEMERA_CTL_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("EPHEMERA_API_TOKEN")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ephemera-ctl: "+err.Error())
	os.Exit(1)
}

// printJSON pretty-prints a raw JSON response to stdout (the --json path).
func printJSON(data []byte) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		os.Stdout.Write(data) // not JSON (e.g. /metrics text) — pass through
		fmt.Println()
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func newTab() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
}
