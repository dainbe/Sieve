// Package env provides helpers for reading SIEVE_ environment variables.
// All helpers return the default when the variable is absent or unparseable.
package env

import (
	"os"
	"strconv"
)

// Int reads an integer from key, returning def when absent or invalid.
// Negative values are treated as invalid and fall back to def.
func Int(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// IntPos reads a strictly-positive integer from key, returning def otherwise.
func IntPos(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Int64 reads an int64 from key, returning def when absent or invalid.
// Non-positive values fall back to def.
func Int64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Float reads a float64 from key, returning def when absent or invalid.
// Negative values fall back to def.
func Float(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}

// Bool returns true when key is set to "1".
func Bool(key string) bool {
	return os.Getenv(key) == "1"
}
