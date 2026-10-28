package events

import "fmt"

// latestMatchInRotating returns the newest event matching filter in the
// in-flight rotating-* files of dir, the segments a rotation has renamed away
// from the active log but not yet gzipped into a canonical archive. They hold
// events newer than every archive, so ReadLatestMatch consults them after the
// active file and before the archives; without them a match that exists only
// in a segment mid-rotation is invisible (ga-s69mk.12).
//
// Files are walked newest-first and each is streamed in bounded batches,
// keeping only its last match. A file promoted to its archive between listing
// and open is read from that archive (openSegmentReader). An unreadable file
// is skipped with a warning, as ReadLatestMatch does for archives.
func latestMatchInRotating(dir string, filter Filter) (Event, bool, []string) {
	srcs, err := listBackfillSources(dir, filter.AfterSeq)
	if err != nil {
		return Event{}, false, []string{fmt.Sprintf("listing in-flight rotating event files: %v", err)}
	}
	var warnings []string
	for i := len(srcs) - 1; i >= 0; i-- {
		src := srcs[i]
		if src.kind != sourceRotating {
			continue
		}
		last, found, err := lastMatchInSegment(src, filter)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped unreadable rotating file %q: %v", src.path, err))
			continue
		}
		if found {
			return last, true, warnings
		}
	}
	return Event{}, false, warnings
}

// lastMatchInSegment streams one segment and returns its newest match.
func lastMatchInSegment(src backfillSource, filter Filter) (Event, bool, error) {
	sr, err := openSegmentReader(src)
	if err != nil || sr == nil {
		return Event{}, false, err
	}
	defer sr.close()
	var (
		last   Event
		found  bool
		maxSeq = filter.AfterSeq
		batch  []Event
	)
	for {
		batch = batch[:0]
		done, err := sr.readInto(filter, &maxSeq, &batch, backfillBatch)
		if len(batch) > 0 {
			last, found = batch[len(batch)-1], true
		}
		if err != nil {
			return Event{}, false, err
		}
		if done {
			return last, found, nil
		}
	}
}
