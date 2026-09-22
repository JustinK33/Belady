package main

import (
	"math/rand/v2"
	"strconv"

	"github.com/JustinK33/Belady/internal/cache"
	"github.com/JustinK33/Belady/internal/config"
)

// generateTrace draws keys from a Zipf distribution. Web popularity is
// consistently Zipf-like with an exponent near 1; a uniform workload would make
// every policy look identical, because with no skew there is nothing to predict.
func generateTrace(opts options) []string {
	// A fixed seed by default: comparing two policies means replaying the same
	// trace, not two draws from the same distribution.
	rng := rand.New(rand.NewPCG(uint64(config.Int("SEED", 1)), 0x9e3779b97f4a7c15))
	zipf := rand.NewZipf(rng, opts.zipfS, 1, uint64(opts.keyspace-1))

	trace := make([]string, opts.requests)
	for i := range trace {
		trace[i] = "key-" + strconv.FormatUint(zipf.Uint64(), 10)
	}
	return trace
}

// hashTrace converts keys to the same 64-bit identity the cache uses, so the
// optimum is scored on exactly the objects the server saw.
func hashTrace(trace []string) []uint64 {
	out := make([]uint64, len(trace))
	for i, k := range trace {
		out[i] = cache.Hash(k)
	}
	return out
}
