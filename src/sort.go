package gozip

import "sort"

// SortFilesOptimized returns a sorted slice of files according to the strategy.
// Returns copy of the original slice.
func SortFilesOptimized(files []*File, strategy FileSortStrategy) []*File {
	if len(files) <= 1 {
		result := make([]*File, len(files))
		copy(result, files)
		return result
	}

	switch strategy {
	case SortLargeFilesLast:
		return partitionSortLargeFilesLast(files)

	case SortLargeFilesFirst:
		return partitionSortLargeFilesFirst(files)

	case SortZIP64Optimized:
		if len(files) > 1000 {
			return optimizedSortZIP64OptimizedWithBuckets(files)
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

// partitionSortLargeFilesLast - O(n) time for specific strategy
func partitionSortLargeFilesLast(files []*File) []*File {
	if len(files) <= 1 {
		return files
	}

	// Single pass partitioning - much faster than full sort for this case
	result := make([]*File, len(files))
	smallIdx, largeIdx := 0, len(files)-1

	for _, f := range files {
		if f.uncompressedSize < 1<<32 { // < 4GB
			result[smallIdx] = f
			smallIdx++
		} else { // >= 4GB
			result[largeIdx] = f
			largeIdx--
		}
	}

	return result
}

// optimizedSortZIP64OptimizedWithBuckets - bucket sort approach
func optimizedSortZIP64OptimizedWithBuckets(files []*File) []*File {
	var smallCount, mediumCount, largeCount int
	for _, f := range files {
		switch getSizePriority(f.uncompressedSize) {
		case 0:
			smallCount++
		case 1:
			mediumCount++
		case 2:
			largeCount++
		}
	}
	small := make([]*File, 0, smallCount)
	medium := make([]*File, 0, mediumCount)
	large := make([]*File, 0, largeCount)

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

	if len(small) > 1 {
		sort.Slice(small, func(i, j int) bool {
			return small[i].uncompressedSize < small[j].uncompressedSize
		})
	}
	if len(medium) > 1 {
		sort.Slice(medium, func(i, j int) bool {
			return medium[i].uncompressedSize < medium[j].uncompressedSize
		})
	}

	result := make([]*File, len(files))
	pos := 0
	pos += copy(result[pos:], small)
	pos += copy(result[pos:], medium)
	pos += copy(result[pos:], large)
	return result
}

func partitionSortLargeFilesFirst(files []*File) []*File {
	result := make([]*File, len(files))
	smallIdx, largeIdx := len(files)-1, 0

	for _, f := range files {
		if f.uncompressedSize < 1<<32 {
			result[smallIdx] = f
			smallIdx--
		} else {
			result[largeIdx] = f
			largeIdx++
		}
	}
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
	if size < 0 {
		return 2
	}
	if size < 10485760 {
		return 0
	}
	if size < 1<<32 {
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
