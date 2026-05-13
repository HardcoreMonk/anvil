//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	input := flag.String("input", "", "path to snapshot metadata.json")
	flag.Parse()

	if *input == "" {
		fmt.Fprintln(os.Stderr, "missing required -input path")
		os.Exit(2)
	}

	data, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *input, err)
		os.Exit(1)
	}

	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", *input, err)
		os.Exit(1)
	}

	delete(metadata, "agent_token")

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(metadata); err != nil {
		fmt.Fprintf(os.Stderr, "encode scrubbed metadata: %v\n", err)
		os.Exit(1)
	}
}
