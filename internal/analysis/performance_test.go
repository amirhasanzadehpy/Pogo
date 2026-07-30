package analysis

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

func BenchmarkParseUpdateMatrix(b *testing.B) {
	for _, prefixSize := range []int{1 << 10, 100 << 10} {
		b.Run(fmt.Sprintf("document-%d", prefixSize), func(b *testing.B) {
			store, err := NewStore()
			if err != nil {
				b.Fatal(err)
			}
			defer store.CloseAll()
			prefix := strings.Repeat("# padding\n", prefixSize/10)
			source := prefix + "value = 'a'\n"
			uri := "file:///parse-matrix.py"
			if err := store.Open(uri, 1, source); err != nil {
				b.Fatal(err)
			}
			position, _ := PositionAt([]byte(source), len(source)-3)
			version := int32(1)
			totals := make([]time.Duration, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				started := time.Now()
				version++
				text := "b"
				if index%2 == 1 {
					text = "a"
				}
				if err := store.Change(uri, version, []Change{{Range: &Range{Start: position, End: Position{Line: position.Line, Character: position.Character + 1}}, Text: text}}); err != nil {
					b.Fatal(err)
				}
				totals[index] = time.Since(started)
			}
			b.StopTimer()
			reportPerformancePercentiles(b, totals)
		})
	}
}

func BenchmarkDocumentSnapshots(b *testing.B) {
	for _, count := range []int{1, 100, 1_000} {
		b.Run(fmt.Sprintf("open-documents-%d", count), func(b *testing.B) {
			store, err := NewStore()
			if err != nil {
				b.Fatal(err)
			}
			defer store.CloseAll()
			for index := 0; index < count; index++ {
				if err := store.Open(fmt.Sprintf("file:///document-%04d.py", index), 1, "from app.models import Model\nModel.objects.filter(name=1)\n"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			totals := make([]time.Duration, b.N)
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				started := time.Now()
				if snapshots := store.Snapshots(); len(snapshots) != count {
					b.Fatalf("Snapshots() count = %d", len(snapshots))
				}
				totals[index] = time.Since(started)
			}
			b.StopTimer()
			reportPerformancePercentiles(b, totals)
		})
	}
}

func reportPerformancePercentiles(b *testing.B, values []time.Duration) {
	b.Helper()
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	for _, percent := range []int{50, 95, 99} {
		index := (len(values)*percent + 99) / 100
		if index > 0 {
			index--
		}
		b.ReportMetric(float64(values[index].Nanoseconds())/1000, fmt.Sprintf("p%d-us", percent))
	}
}
