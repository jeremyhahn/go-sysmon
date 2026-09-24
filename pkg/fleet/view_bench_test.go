//go:build unix

package fleet

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// What GET /api/fleet costs at fleet scale (#15), per request: building the
// view and encoding it, which is the whole of the handler's work. A dashboard
// pays it on every poll, so it is measured per request rather than per cycle.
//
// Three shapes of request against the same cache:
//
//	whole    no parameters: every node, the shape before #15 and still what
//	         an unpaged API client gets
//	page     limit=25, the dashboard's default page
//	search   q matching a tenth of the fleet, limit=25
//
// bytes/response is what goes on the wire; ms_cpu/op is process CPU, for the
// reason fleet_cpu_bench_test.go gives.
//
//	make bench-fleet
func BenchmarkPoller_View(b *testing.B) {
	shapes := []struct {
		name string
		q    Query
	}{
		{"whole", Query{}},
		{"page", Query{Limit: 25}},
		{"search", Query{Search: "node-000", Limit: 25}},
	}
	for _, n := range []int{250, 1000, 10000} {
		p := largeFleet(b, n)
		for _, shape := range shapes {
			b.Run(fmt.Sprintf("nodes=%d/%s", n, shape.name), func(b *testing.B) {
				var size, ops int
				start := processCPU(b)
				b.ResetTimer()
				for b.Loop() {
					body, err := json.Marshal(p.View(shape.q))
					if err != nil {
						b.Fatalf("encode: %v", err)
					}
					size = len(body)
					ops++
				}
				b.StopTimer()
				used := processCPU(b) - start
				b.ReportMetric(float64(size), "bytes/response")
				b.ReportMetric(float64(used)/float64(ops)/float64(time.Millisecond), "ms_cpu/op")
			})
		}
	}
}
