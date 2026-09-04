package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mentasystems/fragua/internal/bench"
)

// valueFlags are the bench flags whose value can be a separate argument.
var valueFlags = map[string]bool{"seed": true, "budget": true, "json": true, "md": true}

// runBench is `fragua bench [dir] [--seed N] [--budget S] [--json f] [--md f]`.
func runBench(args []string) error {
	// The directory is positional and may come before or after the flags, so
	// pull it out first: flag.Parse stops at the first non-flag argument.
	dir, flags := "bench/boards", []string(nil)
	found, wantValue := false, false
	for _, a := range args {
		switch {
		case wantValue:
			wantValue = false
		case strings.HasPrefix(a, "-"):
			name := strings.TrimLeft(a, "-")
			wantValue = !strings.Contains(a, "=") && valueFlags[name]
		case !found:
			dir, found = a, true
			continue
		}
		flags = append(flags, a)
	}
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	seed := fs.Uint64("seed", bench.DefaultOptions().Seed, "placer seed")
	budget := fs.Float64("budget", bench.DefaultOptions().BudgetSeconds, "per-board router wall budget (seconds)")
	jsonOut := fs.String("json", "", "write the run as JSON to this path")
	mdOut := fs.String("md", "", "write the markdown table to this path")
	strict := fs.Bool("strict", false, "exit non-zero when a board does not fully route or has DRC errors")
	if err := fs.Parse(flags); err != nil {
		return err
	}
	run, err := bench.RunDir(dir, bench.Options{Seed: *seed, BudgetSeconds: *budget})
	if err != nil {
		return err
	}
	md := run.Markdown()
	fmt.Print(md)
	if *mdOut != "" {
		if err := os.WriteFile(*mdOut, []byte(md), 0o644); err != nil {
			return err
		}
	}
	if *jsonOut != "" {
		raw, err := run.JSON()
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	if n := run.Failures(); n > 0 {
		return fmt.Errorf("bench: %d board(s) failed to run", n)
	}
	if *strict {
		if n := run.Unclean(); n > 0 {
			return fmt.Errorf("bench: %d board(s) not fully routed or with DRC errors", n)
		}
	}
	return nil
}
