package main

import (
	"strconv"
)

// splitStatm splits the whitespace-separated fields of /proc/self/statm
// without pulling in the reflect-based fmt package on the hot RSS path.
func splitStatm(data []byte) [][]byte {
	fields := make([][]byte, 0, 8)
	start := 0
	for index, b := range data {
		if b == ' ' || b == '\n' || b == '\t' {
			if index > start {
				fields = append(fields, data[start:index])
			}
			start = index + 1
		}
	}
	if start < len(data) {
		fields = append(fields, data[start:])
	}
	return fields
}

func parseUint64(data []byte) uint64 {
	value, _ := strconv.ParseUint(string(data), 10, 64)
	return value
}
