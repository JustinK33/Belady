// Package config reads configuration from the process environment and nowhere
// else. No config files, so a container image never has to carry a secret.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// MustString exits if the key is missing. Use for values with no safe default,
// such as an upstream address.
func MustString(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		fmt.Fprintf(os.Stderr, "config: required environment variable %s is not set\n", key)
		os.Exit(2)
	}
	return v
}

func Int(key string, def int) int {
	return parse(key, def, strconv.Atoi)
}

func Bool(key string, def bool) bool {
	return parse(key, def, strconv.ParseBool)
}

func Duration(key string, def time.Duration) time.Duration {
	return parse(key, def, time.ParseDuration)
}

func Float(key string, def float64) float64 {
	return parse(key, def, func(s string) (float64, error) { return strconv.ParseFloat(s, 64) })
}

// Strings splits a comma-separated list, trimming whitespace and dropping empties.
func Strings(key string, def []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// Bytes parses a human-readable size such as "512mb" or "4GiB" into bytes.
// Cache capacities are the main reason this exists; nobody should have to write
// 1073741824 in a compose file.
func Bytes(key string, def int64) int64 {
	return parse(key, def, ParseBytes)
}

var byteUnits = []struct {
	suffix string
	scale  int64
}{
	// Longest suffixes first so "kib" is not matched as "b".
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	{"b", 1},
}

func ParseBytes(s string) (int64, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	if norm == "" {
		return 0, fmt.Errorf("empty size")
	}
	for _, u := range byteUnits {
		if !strings.HasSuffix(norm, u.suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(norm, u.suffix))
		val, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("bad size %q: %w", s, err)
		}
		if val < 0 {
			return 0, fmt.Errorf("bad size %q: negative", s)
		}
		return int64(val * float64(u.scale)), nil
	}
	val, err := strconv.ParseInt(norm, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q: %w", s, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("bad size %q: negative", s)
	}
	return val, nil
}

// parse falls back to the default and warns rather than exiting: a typo in an
// optional tuning knob should not take a cache node out of rotation.
func parse[T any](key string, def T, fn func(string) (T, error)) T {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := fn(raw)
	if err != nil {
		slog.Warn("config: ignoring malformed value", "key", key, "value", raw, "default", def)
		return def
	}
	return v
}
