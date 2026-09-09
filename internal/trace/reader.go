package trace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
)

// Segments lists the finished segment files in dir, oldest first. The names carry a
// nanosecond timestamp, so lexical order is chronological order.
func Segments(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*"+Extension))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// ReadFile decodes every batch in one segment.
//
// This exists for tests and for the Go side of any offline scoring. The trainer reads
// the same framing in Python; both sides are short because the framing is deliberately
// boring.
func ReadFile(path string) ([]*beladyv1.AccessBatch, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied trace directory
	if err != nil {
		return nil, err
	}

	var batches []*beladyv1.AccessBatch
	for len(data) > 0 {
		size, n := protowire.ConsumeVarint(data)
		if n < 0 {
			return batches, fmt.Errorf("%s: corrupt length prefix at %d bytes from the end: %w",
				path, len(data), protowire.ParseError(n))
		}
		data = data[n:]
		if size > uint64(len(data)) {
			// A truncated tail means the writer died mid-frame. Everything before it is
			// still valid, so return it along with the error and let the caller decide.
			return batches, fmt.Errorf("%s: frame claims %d bytes, %d remain: %w",
				path, size, len(data), io.ErrUnexpectedEOF)
		}
		var batch beladyv1.AccessBatch
		if err := proto.Unmarshal(data[:size], &batch); err != nil {
			return batches, fmt.Errorf("%s: %w", path, err)
		}
		batches = append(batches, &batch)
		data = data[size:]
	}
	return batches, nil
}

// ReadDir decodes every finished segment in dir in order, skipping nothing silently:
// a corrupt segment stops the read and is reported.
func ReadDir(dir string) ([]*beladyv1.AccessBatch, error) {
	paths, err := Segments(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no %s segments in %s", Extension, dir)
	}

	var all []*beladyv1.AccessBatch
	for _, p := range paths {
		batches, err := ReadFile(p)
		all = append(all, batches...)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return all, err
		}
	}
	return all, nil
}
