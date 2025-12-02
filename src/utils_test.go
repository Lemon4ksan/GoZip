// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"testing"
	"time"
)

func TestByteCounterWriter(t *testing.T) {
	buf := new(bytes.Buffer)
	counter := &byteCountWriter{dest: buf}

	testData := []byte("Hello, World!")
	n, err := counter.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != len(testData) {
		t.Errorf("written bytes mismatch: got %d, expected %d", n, len(testData))
	}

	if counter.bytesWritten != int64(len(testData)) {
		t.Errorf("counter mismatch: got %d, expected %d", counter.bytesWritten, len(testData))
	}

	if buf.String() != string(testData) {
		t.Error("data not written to underlying writer")
	}
}

func TestTimeToMsDos(t *testing.T) {
	tests := []struct {
		name         string
		time         time.Time
		expectedDate uint16
		expectedTime uint16
	}{
		{
			name:         "Epoch time",
			time:         time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
			expectedDate: 0x0021, // (1980-1980=0)<<9 | 1<<5 | 1 = 0|32|1 = 33 = 0x0021
			expectedTime: 0x0000, // 0<<11 | 0<<5 | 0/2 = 0
		},
		{
			name:         "Specific date",
			time:         time.Date(2023, 12, 15, 14, 30, 15, 0, time.UTC),
			expectedDate: 0x578F, // (2023-1980=43)<<9 | 12<<5 | 15 = 22016|384|15 = 22415 = 0x578F
			expectedTime: 0x73C7, // 14<<11 | 30<<5 | 15/2=7 = 28672|960|7 = 29639 = 0x73C7
		},
		{
			name:         "Before 1980",
			time:         time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			expectedDate: 0x0021, // year clamped to 0 = 1980-01-01
			expectedTime: 0x0000,
		},
		{
			name:         "After 2107",
			time:         time.Date(2108, 1, 1, 0, 0, 0, 0, time.UTC),
			expectedDate: 0xFE21, // year clamped to 127 = 2107-12-31? Actually 127<<9 | 1<<5 | 1
			expectedTime: 0x0000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, timeVal := timeToMsDos(tt.time)
			if date != tt.expectedDate {
				t.Errorf("date mismatch: got %04x, expected %04x", date, tt.expectedDate)
			}
			if timeVal != tt.expectedTime {
				t.Errorf("time mismatch: got %04x, expected %04x", timeVal, tt.expectedTime)
			}
		})
	}
}

func TestMsDosToTime(t *testing.T) {
	tests := []struct {
		name     string
		date     uint16
		timeVal  uint16
		expected time.Time
	}{
		{
			name:     "Epoch",
			date:     0x0021, // 1980-01-01
			timeVal:  0x0000, // 00:00:00
			expected: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "Specific date",
			date:     0x578F, // 2023-12-15
			timeVal:  0x73C7, // 14:30:14 (15 seconds becomes 14 due to 2-second resolution)
			expected: time.Date(2023, 12, 15, 14, 30, 14, 0, time.UTC),
		},
		{
			name:     "Max time values",
			date:     0x0021, // 1980-01-01
			timeVal:  0xBF7D, // 23:59:58 (max valid time)
			expected: time.Date(1980, 1, 1, 23, 59, 58, 0, time.UTC),
		},
		{
			name:     "Invalid month clamped",
			date:     0x0001, // month=0, day=1 - should clamp month to 1
			timeVal:  0x0000,
			expected: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "Invalid day clamped",
			date:     0x0020, // month=1, day=0 - should clamp day to 1
			timeVal:  0x0000,
			expected: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := msDosToTime(tt.date, tt.timeVal)
			if !result.Equal(tt.expected) {
				t.Errorf("time mismatch: got %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestTimeToMsDos_EdgeCases(t *testing.T) {
	// Test year before 1980 (should clamp to 1980)
	earlyTime := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	date, _ := timeToMsDos(earlyTime)
	expectedYear := 0 // 1980-1980
	actualYear := (date >> 9) & 0x7F
	if actualYear != uint16(expectedYear) {
		t.Errorf("year clamping failed: got %d, expected %d", actualYear, expectedYear)
	}

	// Test year after 2107 (should clamp to 2107-1980=127)
	lateTime := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	date, _ = timeToMsDos(lateTime)
	expectedYear = 127
	actualYear = (date >> 9) & 0x7F
	if actualYear != uint16(expectedYear) {
		t.Errorf("year clamping failed: got %d, expected %d", actualYear, expectedYear)
	}
}

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		time time.Time
	}{
		{"Epoch", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"Recent date", time.Date(2023, 12, 15, 14, 30, 15, 0, time.UTC)},
		{"Max DOS date", time.Date(2107, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"Min DOS date", time.Date(1980, 1, 1, 0, 0, 1, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, timeVal := timeToMsDos(tt.time)
			result := msDosToTime(date, timeVal)

			// Allow for 2-second precision loss in MS-DOS format
			diff := result.Sub(tt.time)
			if diff < -2*time.Second || diff > 2*time.Second {
				t.Errorf("round trip mismatch: original %v, got %v", tt.time, result)
			}
		})
	}
}
