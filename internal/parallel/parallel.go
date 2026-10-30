// Package parallel runs filesystem work on a bounded pool of goroutines.
package parallel

import (
	"os"
	"runtime"
	"strconv"
	"sync"
)

// Workers is how many goroutines ForEach uses. It defaults to the number of
// CPUs (at least 8, since the work is mostly waiting on the filesystem) and
// can be set with COWTREE_WORKERS.
var Workers = workers()

func workers() int {
	if n, err := strconv.Atoi(os.Getenv("COWTREE_WORKERS")); err == nil && n > 0 {
		return n
	}
	return max(8, runtime.NumCPU())
}

// ForEach calls fn for every item, on Workers goroutines, and waits for all of
// them to finish.
func ForEach[T any](items []T, fn func(T)) {
	work := make(chan T)
	var wg sync.WaitGroup
	for range min(Workers, max(len(items), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		work <- it
	}
	close(work)
	wg.Wait()
}
