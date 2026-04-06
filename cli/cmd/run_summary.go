package cmd

import (
	"fmt"
	"time"
)

// RunSummary accumulates metrics across all loop iterations and is printed
// on clean exit. Fields are incremented by individual loop steps.
type RunSummary struct {
	startTime     time.Time
	iterations    int
	issuesCreated int
	fixesApplied  int
	fixesReverted int
	finalVerdict  string
}

func newRunSummary() *RunSummary {
	return &RunSummary{startTime: time.Now()}
}

func (s *RunSummary) print(logPath string) {
	elapsed := time.Since(s.startTime).Round(time.Second)
	fmt.Println("\n─── Run Summary ───────────────────────────────")
	fmt.Printf("  Iterations     : %d\n", s.iterations)
	fmt.Printf("  Issues created : %d\n", s.issuesCreated)
	fmt.Printf("  Fixes applied  : %d\n", s.fixesApplied)
	if s.fixesReverted > 0 {
		fmt.Printf("  Fixes reverted : %d\n", s.fixesReverted)
	}
	if s.finalVerdict != "" {
		fmt.Printf("  Final verdict  : %s\n", s.finalVerdict)
	}
	fmt.Printf("  Elapsed        : %s\n", elapsed)
	if logPath != "" {
		fmt.Printf("  Log            : %s\n", logPath)
	}
	fmt.Println("────────────────────────────────────────────────")
}
