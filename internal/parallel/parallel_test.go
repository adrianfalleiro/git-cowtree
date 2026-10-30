package parallel

import (
	"sync/atomic"
	"testing"
)

func TestForEach(t *testing.T) {
	for _, n := range []int{0, 1, Workers * 3} {
		items := make([]int, n)
		for i := range items {
			items[i] = i + 1
		}
		var sum atomic.Int64
		ForEach(items, func(i int) { sum.Add(int64(i)) })
		if want := int64(n * (n + 1) / 2); sum.Load() != want {
			t.Errorf("ForEach over %d items summed to %d, want %d", n, sum.Load(), want)
		}
	}
}
