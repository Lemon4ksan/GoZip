// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import "sort"

// Sequential Saving (Write()):
//   ┌──────────────────┬────────────────────────┬───────────────────────┬──────────────────┐
//   │ Strategy         │ ZIP64 Overhead         │ Speed                 │ Recommendation   │
//   ├──────────────────┼────────────────────────┼───────────────────────┼──────────────────┤
//   │ Default          │ Depends on file order  │ No sorting            │ Small files only │
//   │ LargeFilesLast   │ Minimal (optimal)      │ Very Fast             │ Large files ≥4GB │
//   │ LargeFilesFirst  │ High (avoid)           │ Very Fast             │ Parallel only    │
//   │ ZIP64Optimized   │ Low (good balance)     │ Fast                  │ General purpose  │
//   │ SizeAscending    │ Medium                 │ Fast                  │ Specific needs   │
//   │ SizeDescending   │ Medium                 │ Fast                  │ Specific needs   │
//   │ Alphabetical     │ Depends on file order  │ Slow                  │ Avoid            │
//   └──────────────────┴────────────────────────┴───────────────────────┴──────────────────┘
//
// Parallel Saving (WriteParallel()):
//   ┌──────────────────┬────────────────────────┬───────────────────────┬──────────────────┐
//   │ Strategy         │ Parallel Efficiency    │ ZIP64 Overhead        │ Speed            │
//   ├──────────────────┼────────────────────────┼───────────────────────┼──────────────────┤
//   │ Default          │ Good                   │ Depends on file order │ No sorting       │
//   │ LargeFilesFirst  │ High (optimal)         │ Medium if Stored      │ Very Fast        │
//   │                  │                        │ Minimal/Low otherwise │                  │
//   │ LargeFilesLast   │ Low (suboptimal)       │ Minimal               │ Very Fast        │
//   │ ZIP64Optimized   │ Medium (good balance)  │ Low                   │ Fast             │
//   │ SizeAscending    │ Medium                 │ Medium                │ Fast             │
//   │ SizeDescending   │ Medium                 │ Medium                │ Fast             │
//   │ Alphabetical     │ No Effect              │ Depends on file order │ Slow             │
//   └──────────────────┴────────────────────────┴───────────────────────┴──────────────────┘
type FileSortStrategy int

const (
	SortDefault         FileSortStrategy = iota
	SortLargeFilesLast                   // Large files (>=4GB) at end
	SortLargeFilesFirst                  // Large filles (>=4GB) at start
	SortSizeAscending                    // Smallest first (slower)
	SortSizeDescending                   // Largest first (slower)
	SortZIP64Optimized                   // Smart ZIP64 optimization
	SortAlphabetical                     // A-Z by filename (folders first naturally)
)

// SortFilesOptimized returns a sorted slice of files according to the strategy.
// Returns a new slice, original slice is not modified.
func SortFilesOptimized(files []*File, strategy FileSortStrategy) []*File {
	if len(files) <= 1 {
		result := make([]*File, len(files))
		copy(result, files)
		return result
	}

	switch strategy {
	case SortLargeFilesLast:
		return partitionStable(files, func(f *File) bool {
			return f.uncompressedSize < 1<<32 // Condition for "First" group
		})

	case SortLargeFilesFirst:
		return partitionStable(files, func(f *File) bool {
			return f.uncompressedSize >= 1<<32 // Condition for "First" group
		})

	case SortZIP64Optimized:
		// Bucket sort is faster for large datasets, but standard sort is fine for smaller ones
		if len(files) > 1000 {
			return optimizedSortZIP64Buckets(files)
		}
		return sortZip64Optimized(files)

	case SortSizeAscending:
		return sortSizeAscending(files)

	case SortSizeDescending:
		return sortSizeDescending(files)

	case SortAlphabetical:
		return sortAlphabetical(files)

	default:
		result := make([]*File, len(files))
		copy(result, files)
		return result
	}
}

// partitionStable splits files into two groups based on the keepFirst condition.
// It preserves the relative order of elements (Stable).
// O(N) time, O(N) space, 2 passes (Count then Fill).
func partitionStable(files []*File, keepFirst func(*File) bool) []*File {
	countFirst := 0
	for _, f := range files {
		if keepFirst(f) {
			countFirst++
		}
	}

	result := make([]*File, len(files))

	// Pointers for where to write the next element
	idxFirst := 0
	idxSecond := countFirst

	for _, f := range files {
		if keepFirst(f) {
			result[idxFirst] = f
			idxFirst++
		} else {
			result[idxSecond] = f
			idxSecond++
		}
	}

	return result
}

func sortSizeAscending(files []*File) []*File {
	sorted := make([]*File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].uncompressedSize < sorted[j].uncompressedSize
	})
	return sorted
}

func sortSizeDescending(files []*File) []*File {
	sorted := make([]*File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].uncompressedSize > sorted[j].uncompressedSize
	})
	return sorted
}

// optimizedSortZIP64Buckets implements a bucket sort strategy.
// Priority: Small (<10MB) -> Medium (<4GB) -> Large (>=4GB).
// Inside buckets: Sorted by size ASC.
func optimizedSortZIP64Buckets(files []*File) []*File {
	var small, medium, large []*File

	// Pre-allocate to avoid resizing if possible, assuming roughly equal distribution
	// or just let append handle it. For >1000 files, append overhead is negligible compared to sort.
	capEst := len(files) / 3
	small = make([]*File, 0, capEst)
	medium = make([]*File, 0, capEst)
	large = make([]*File, 0, capEst)

	for _, f := range files {
		switch getSizePriority(f.uncompressedSize) {
		case 0:
			small = append(small, f)
		case 1:
			medium = append(medium, f)
		case 2:
			large = append(large, f)
		}
	}

	// Helper to sort a bucket
	sortBucket := func(bucket []*File) {
		if len(bucket) > 1 {
			sort.Slice(bucket, func(i, j int) bool {
				return bucket[i].uncompressedSize < bucket[j].uncompressedSize
			})
		}
	}

	sortBucket(small)
	sortBucket(medium)
	sortBucket(large)

	// Merge results
	result := make([]*File, 0, len(files))
	result = append(result, small...)
	result = append(result, medium...)
	result = append(result, large...)

	return result
}

func sortZip64Optimized(files []*File) []*File {
	sorted := make([]*File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		iSize := sorted[i].uncompressedSize
		jSize := sorted[j].uncompressedSize

		iPriority := getSizePriority(iSize)
		jPriority := getSizePriority(jSize)

		if iPriority != jPriority {
			return iPriority < jPriority
		}
		return iSize < jSize
	})
	return sorted
}

func getSizePriority(size int64) int {
	// Priority 0: Small files (good for headers packing)
	// Priority 1: Standard files
	// Priority 2: Zip64 files or Unknown size (Stream)

	if size < 0 {
		return 2 // Unknown size -> treat as Large/Complex
	}
	if size < 10*1024*1024 { // 10 MB
		return 0
	}
	if size < 1<<32 { // 4 GB
		return 1
	}
	return 2
}

// sortAlphabetical sorts files by name A-Z
func sortAlphabetical(files []*File) []*File {
	sorted := make([]*File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].name < sorted[j].name
	})
	return sorted
}
