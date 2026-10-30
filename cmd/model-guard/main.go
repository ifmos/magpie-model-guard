package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mos/magpie-model-guard/internal/check"
)

func main() {
	var (
		dataDir string
		gate    string
		addr    string
		owned   bool
	)
	flag.StringVar(&dataDir, "data", "", "data directory (state db, logs)")
	flag.StringVar(&gate, "gate", "", "gate middleware js file to publish")
	flag.StringVar(&addr, "addr", "127.0.0.1:0", "management api listen address")
	flag.BoolVar(&owned, "owned", false, "started by the magpie plugin host; print startup info as one json line")
	flag.Parse()

	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "-data is required")
		os.Exit(2)
	}
	if gate == "" {
		fmt.Fprintln(os.Stderr, "-gate is required")
		os.Exit(2)
	}
	if err := check.Run(check.Options{
		DataDir:     dataDir,
		GatePath:    gate,
		Addr:        addr,
		PluginOwned: owned,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// Keep the binary self-locating for future use without filepath walking.
	_ = filepath.Separator
}
