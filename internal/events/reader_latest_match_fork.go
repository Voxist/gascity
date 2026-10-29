package events

import (
	"fmt"
	"path/filepath"
)

// latestMatchInHistory returns the newest event matching filter in the
// rotated history of dir: the canonical .gz archives and the in-flight
// rotating-* files, the segments a rotation has renamed away from the active
// log but not yet gzipped (ga-s69mk.12). Both are walked as one sequence,
// newest seq window first, and the walk stops at the first segment holding a
// match. A rotating file is usually the newest segment, but one whose gzip
// failed can trail a later archive, so neither kind is consulted wholesale
// before the other.
//
// A rotating file and its already-promoted archive share a seq window and
// content, so whichever is read first answers the same. A file promoted
// between listing and open is read from that archive (openSegmentReader).
// Unreadable segments are skipped with a warning and the walk continues into
// older ones, so a corrupt segment can only make the answer older. A history
// that cannot be listed (usually: the directory does not exist) holds no
// match.
func latestMatchInHistory(dir string, filter Filter) (Event, bool, []string) {
	archives, err := archiveFilesIn(dir)
	if err != nil {
		archives = nil
	}
	var warnings []string
	var rotating []backfillSource
	srcs, err := listBackfillSources(dir, filter.AfterSeq)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("listing in-flight rotating event files: %v", err))
	}
	for _, src := range srcs {
		if src.kind != sourceRotating {
			continue
		}
		if filter.BeforeSeq > 0 && src.firstSeq >= filter.BeforeSeq {
			continue
		}
		rotating = append(rotating, src)
	}

	a, r := len(archives)-1, len(rotating)-1
	for a >= 0 || r >= 0 {
		// Newest window next; a tie is a rotating file and its archive twin.
		if r >= 0 && (a < 0 || rotating[r].firstSeq >= archives[a].FirstSeq) {
			src := rotating[r]
			r--
			last, found, err := lastMatchInSegment(src, filter)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("skipped unreadable rotating file %q: %v", filepath.Base(src.path), err))
				continue
			}
			if found {
				return last, true, warnings
			}
			continue
		}
		info := archives[a]
		a--
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		last, found, err := lastMatchInArchive(filepath.Join(dir, info.Basename), filter)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped unreadable archive %q: %v", info.Basename, err))
			continue
		}
		if found {
			return last, true, warnings
		}
	}
	return Event{}, false, warnings
}

// lastMatchInArchive streams one archive and returns its last match. Events
// in an archive are oldest-first, so the whole archive is streamed: O(1
// archive) for the typical rare-event query, independent of history size.
func lastMatchInArchive(path string, filter Filter) (Event, bool, error) {
	var last Event
	var found bool
	err := streamArchive(path, filter, func(e Event) bool {
		if matchesFilter(e, filter) {
			last = e
			found = true
		}
		return true
	})
	if err != nil {
		return Event{}, false, err
	}
	return last, found, nil
}

// lastMatchInSegment streams one rotating segment in bounded batches and
// returns its newest match.
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
