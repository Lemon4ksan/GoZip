// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import "sort"

// FileSortStrategy defines the order in which files are written to the archive.
// Choosing the right strategy can optimize writing speed (CPU parallelism),
// memory usage or archive structure (ZIP64 overhead).
type FileSortStrategy int

const (
	SortDefault         FileSortStrategy = iota
	SortLargeFilesLast                   // Large files (>=4GB) at end
	SortLargeFilesFirst                  // Large files (>=4GB) at start
	SortSizeAscending                    // Smallest first
	SortSizeDescending                   // Largest first
	SortZIP64Optimized                   // Buckets: <10MB, <4GB, >=4GB (each sorted Asc)
	SortAlphabetical                     // A-Z by filename
)

// SortFilesOptimized returns a sorted slice of files according to the strategy.
// Returns a new slice; the original slice is not modified.
func SortFilesOptimized(files []*File, strategy FileSortStrategy) []*File {
	if len(files) <= 1 {
		return files
	}

	switch strategy {
	case SortLargeFilesLast:
		// Move small files (<4GB) to front, large files to back.
		return partitionStable(files, func(f *File) bool {
			return f.uncompressedSize < 1<<32
		})

	case SortLargeFilesFirst:
		// Move large files (>=4GB) to front.
		return partitionStable(files, func(f *File) bool {
			return f.uncompressedSize >= 1<<32
		})

	case SortZIP64Optimized:
		// Bucket sort is faster for large datasets and reduces comparison overhead.
		if len(files) > 1000 {
			return optimizedSortZIP64Buckets(files)
		}
		sortZip64Optimized(files)

	case SortSizeAscending:
		sort.SliceStable(files, func(i, j int) bool {
			return files[i].uncompressedSize < files[j].uncompressedSize
		})

	case SortSizeDescending:
		sort.SliceStable(files, func(i, j int) bool {
			return files[i].uncompressedSize > files[j].uncompressedSize
		})

	case SortAlphabetical:
		sortAlphabetical(files)
	}

	return files
}

// partitionStable splits files into two groups based on the keepFirst condition.
// It preserves the relative order of elements within groups (Stable).
// Complexity: O(N) time, O(N) space.
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

// optimizedSortZIP64Buckets implements a bucket sort strategy.
// Priority: Small (<10MB) -> Medium (<4GB) -> Large (>=4GB).
// Inside buckets: Sorted by size ASC.
func optimizedSortZIP64Buckets(files []*File) []*File {
	var small, medium, large []*File

	// Heuristic allocation: assume 60% small, 30% medium, 10% large
	n := len(files)
	small = make([]*File, 0, n/2)
	medium = make([]*File, 0, n/4)
	large = make([]*File, 0, n/8)

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

	// Helper to stably sort a bucket
	sortBucket := func(bucket []*File) {
		if len(bucket) > 1 {
			sort.SliceStable(bucket, func(i, j int) bool {
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

func sortZip64Optimized(files []*File) {
	sort.SliceStable(files, func(i, j int) bool {
		iSize := files[i].uncompressedSize
		jSize := files[j].uncompressedSize

		iPriority := getSizePriority(iSize)
		jPriority := getSizePriority(jSize)

		if iPriority != jPriority {
			return iPriority < jPriority
		}
		return iSize < jSize
	})
}

func getSizePriority(size int64) int {
	// Priority 0: Small files (good for headers packing)
	// Priority 1: Standard files
	// Priority 2: Zip64 files or Unknown size (Stream)

	if size < 0 {
		// Unknown size -> treat as Large/Complex
		return 2
	}
	if size < 10*1024*1024 { // 10 MB
		return 0
	}
	if size < 1<<32 { // 4 GB
		return 1
	}
	return 2
}

// sortAlphabetical sorts files by name A-Z.
// This naturally groups files in the same directory together.
func sortAlphabetical(files []*File) {
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].name < files[j].name
	})
}
