package gozip

import (
	"math/rand"
	"testing"
)

// Benchmark data generators
func generateSmallFiles(count int) []*file {
	files := make([]*file, count)
	for i := range count {
		files[i] = &file{
			name:             "small_file.txt",
			uncompressedSize: int64(rand.Intn(10 * 1024 * 1024)), // 0-10MB
		}
	}
	return files
}

func generateMixedFiles(count int) []*file {
	files := make([]*file, count)
	for i := range count {
		var size int64
		switch rand.Intn(100) {
		case 0: // 1% large files (>4GB)
			size = 1<<32 + int64(rand.Intn(10*1024*1024*1024))
		case 1, 2, 3, 4: // 4% medium files (100MB-4GB)
			size = 100*1024*1024 + int64(rand.Intn(4*1024*1024*1024-100*1024*1024))
		default: // 95% small files (<100MB)
			size = int64(rand.Intn(100 * 1024 * 1024))
		}
		files[i] = &file{
			name:             "mixed_file.txt",
			uncompressedSize: size,
		}
	}
	return files
}

func generateLargeFiles(count int) []*file {
	files := make([]*file, count)
	for i := range count {
		files[i] = &file{
			name:             "large_file.bin",
			uncompressedSize: 1<<32 + int64(rand.Intn(10*1024*1024*1024)), // 4GB+
		}
	}
	return files
}

func BenchmarkSortSmallFiles_LargeFilesLast(b *testing.B) {
	files := generateSmallFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortSmallFiles_ZIP64Optimized(b *testing.B) {
	files := generateSmallFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

func BenchmarkSortMixedFiles_LargeFilesLast(b *testing.B) {
	files := generateMixedFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortMixedFiles_ZIP64Optimized(b *testing.B) {
	files := generateMixedFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

func BenchmarkSortLargeFiles_LargeFilesLast(b *testing.B) {
	files := generateLargeFiles(1000)
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortSizeAscending(b *testing.B) {
	files := generateMixedFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortSizeAscending)
	}
}

func BenchmarkSortSizeDescending(b *testing.B) {
	files := generateMixedFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortSizeDescending)
	}
}

// Scalability benchmarks
func BenchmarkSortScalability_1000(b *testing.B) {
	files := generateMixedFiles(1000)
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

func BenchmarkSortScalability_10000(b *testing.B) {
	files := generateMixedFiles(10000)
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

func BenchmarkSortScalability_100000(b *testing.B) {
	files := generateMixedFiles(100000)
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

// Memory allocation benchmarks
func BenchmarkSortMemory_LargeFilesLast(b *testing.B) {
	files := generateMixedFiles(10000)
	b.ReportAllocs()
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortMemory_ZIP64Optimized(b *testing.B) {
	files := generateMixedFiles(10000)
	b.ReportAllocs()
	for b.Loop() {
		SortFilesOptimized(files, SortZIP64Optimized)
	}
}

// Edge case benchmarks
func BenchmarkSortEmpty(b *testing.B) {
	files := []*file{}
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortSingleFile(b *testing.B) {
	files := []*file{{name: "single.txt", uncompressedSize: 1024}}
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortAllLargeFiles(b *testing.B) {
	files := generateLargeFiles(1000)
	for b.Loop() {
		SortFilesOptimized(files, SortLargeFilesLast)
	}
}

func BenchmarkSortComparative(b *testing.B) {
	testCases := []struct {
		name     string
		files    []*file
		strategy FileSortStrategy
	}{
		{"SmallFiles_LargeLast", generateSmallFiles(10000), SortLargeFilesLast},
		{"MixedFiles_LargeLast", generateMixedFiles(10000), SortLargeFilesLast},
		{"MixedFiles_ZIP64Opt", generateMixedFiles(10000), SortZIP64Optimized},
		{"LargeFiles_LargeLast", generateLargeFiles(1000), SortLargeFilesLast},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				SortFilesOptimized(tc.files, tc.strategy)
			}
		})
	}
}