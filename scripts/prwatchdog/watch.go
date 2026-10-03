package prwatchdog

import (
	"context"
	"time"
)

// Fetcher retrieves the current set of check runs for a head commit.
type Fetcher interface {
	FetchCheckRuns(ctx context.Context, headSHA string) ([]CheckRun, error)
}

// Clock reports the current time, abstracted for testability.
type Clock interface {
	Now() time.Time
}

// Sleeper pauses for a duration, abstracted for testability.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration)
}

// PollOptions configures a Watch invocation.
type PollOptions struct {
	HeadSHA                  string
	NeedsMacLabel            bool
	NeedsReviewFormulasLabel bool
	// BaseSHA, when non-empty, is fetched alongside HeadSHA on every poll
	// and handed to Evaluate as BaseCheckRuns for the base-red comparison.
	// A failed base fetch is reported through Input.BaseFetchError and never
	// interrupts the observation loop on its own.
	BaseSHA  string
	Deadline time.Duration
	Interval time.Duration
}

// Watch polls fetcher for check runs on opts.HeadSHA, evaluating after each
// poll, until the evaluation is terminal or opts.Deadline elapses.
func Watch(ctx context.Context, fetcher Fetcher, clock Clock, sleeper Sleeper, opts PollOptions) Evaluation {
	start := clock.Now()
	for {
		elapsed := clock.Now().Sub(start)
		runs, err := fetcher.FetchCheckRuns(ctx, opts.HeadSHA)

		var baseRuns []CheckRun
		var baseErr error
		if opts.BaseSHA != "" {
			baseRuns, baseErr = fetcher.FetchCheckRuns(ctx, opts.BaseSHA)
		}

		eval := Evaluate(Input{
			HeadSHA:                  opts.HeadSHA,
			CheckRuns:                runs,
			Elapsed:                  elapsed,
			Deadline:                 opts.Deadline,
			NeedsMacLabel:            opts.NeedsMacLabel,
			NeedsReviewFormulasLabel: opts.NeedsReviewFormulasLabel,
			FetchError:               err,
			BaseSHA:                  opts.BaseSHA,
			BaseCheckRuns:            baseRuns,
			BaseFetchError:           baseErr,
		})
		if eval.Terminal {
			return eval
		}
		sleeper.Sleep(ctx, opts.Interval)
	}
}
