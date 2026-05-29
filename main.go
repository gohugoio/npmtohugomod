package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gohugoio/npmtohugomod/internal/lib"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg lib.Config

	flag.StringVar(&cfg.BaseOutputDir, "base-output-dir", "", "base output directory (defaults to the current directory)")
	flag.StringVar(&cfg.ModuleBase, "module-base", "", "Go module path prefix for generated packages (e.g. github.com/owner/repo); when empty, derived from the base output dir's \"origin\" git remote")
	flag.Parse()

	if cfg.BaseOutputDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg.BaseOutputDir = wd
	}
	return lib.Run(cfg)
}
