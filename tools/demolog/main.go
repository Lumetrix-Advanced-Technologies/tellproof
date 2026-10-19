// Command demolog writes a synthetic local evidence log (signed and chained
// like a real one) for developing and screenshotting the dashboard:
//
//	go run ./tools/demolog -out /tmp/demo -days 7
//	go run ./cmd/agent dashboard --data-dir /tmp/demo
//
// tellproof-agent demo does the same and serves the dashboard.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/demolog"
)

func main() {
	out := flag.String("out", "", "data directory to create")
	days := flag.Int("days", 7, "days of hourly windows, ending now")
	seed := flag.Uint64("seed", 7, "random seed")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: demolog -out <dir> [-days 7]")
		os.Exit(2)
	}
	r, err := demolog.Write(*out, *days, *seed, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d hourly batches to %s as %s\n", r.Batches, *out, r.AgentID)
}
