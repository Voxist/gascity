package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/providergov"
	"github.com/spf13/cobra"
)

// newProviderCmd builds the `gc provider` command group: provider
// governor utilities (city-scale plan, "Provider Governor" P1).
func newProviderCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Provider governor utilities",
		Long: `Provider governor utilities.

The provider governor reads each Claude account's subscription usage
(quota) and resolves which provider should serve each agent tier.
Accounts are configured per provider in city.toml:

    [providers.claude]
    quota_monitor      = true
    monitor_config_dir = "~/.gc/monitor-claude"

The monitoring credential is a full OAuth login (scope user:profile)
created once per account with:

    CLAUDE_CONFIG_DIR=~/.gc/monitor-claude claude login`,
	}
	cmd.AddCommand(newProviderQuotaCmd(stdout, stderr))
	return cmd
}

// newProviderQuotaCmd builds `gc provider quota`: poll each
// monitor-enabled account once, print the measured account states and
// the SelectProvider decision for each tier. With --poll it keeps
// polling on an interval, recording typed provider.* events to the
// city event log instead of printing.
func newProviderQuotaCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		jsonOutput    bool
		baseURL       string
		timeout       time.Duration
		active        string
		flipThreshold float64
		overflow      []string
		poll          bool
		interval      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show Claude account quota states and per-tier provider decisions",
		RunE: func(_ *cobra.Command, _ []string) error {
			rctx, err := resolveContext()
			if err != nil {
				fmt.Fprintf(stderr, "gc provider quota: %v\n", err) //nolint:errcheck
				return errExit
			}
			cfg, err := loadCityConfig(rctx.CityPath, io.Discard)
			if err != nil {
				fmt.Fprintf(stderr, "gc provider quota: loading city config: %v\n", err) //nolint:errcheck
				return errExit
			}
			accounts, err := providergov.AccountsFromConfig(cfg)
			if err != nil {
				fmt.Fprintf(stderr, "gc provider quota: %v\n", err) //nolint:errcheck
				return errExit
			}

			policy := providergov.DefaultPolicy()
			policy.FlipThreshold = flipThreshold
			policy.OverflowProviders = overflow

			if poll {
				if err := runProviderQuotaPoller(rctx.CityPath, accounts, interval, baseURL, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "gc provider quota: %v\n", err) //nolint:errcheck
					return errExit
				}
				return nil
			}

			rows := collectAccountQuota(context.Background(), http.DefaultClient, baseURL, accounts, timeout)
			report := buildProviderQuotaReport(rows, active, policy)
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc provider quota", report)
			}
			renderProviderQuotaText(stdout, report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	cmd.Flags().StringVar(&baseURL, "base-url", providergov.DefaultBaseURL, "API origin serving /api/oauth/usage")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "per-account usage request timeout")
	cmd.Flags().StringVar(&active, "active", "", "provider name of the currently-active Claude account (alternation stay/flip input)")
	cmd.Flags().Float64Var(&flipThreshold, "flip-threshold", providergov.DefaultPolicy().FlipThreshold,
		"five-hour utilization percent above which the active account flips to its sibling")
	cmd.Flags().StringSliceVar(&overflow, "overflow", nil, "overflow vendor pool in priority order (e.g. zai,openrouter)")
	cmd.Flags().BoolVar(&poll, "poll", false, "keep polling on an interval, recording provider.* events to the city event log")
	cmd.Flags().DurationVar(&interval, "interval", providergov.DefaultPollInterval, "poll interval used with --poll")
	return cmd
}

// runProviderQuotaPoller runs the quota poller in the foreground until
// interrupted, recording typed events to the city event log.
func runProviderQuotaPoller(cityPath string, accounts []providergov.Account, interval time.Duration, baseURL string, stdout, stderr io.Writer) error {
	recorder, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), stderr)
	if err != nil {
		return fmt.Errorf("opening city event log: %w", err)
	}
	defer recorder.Close() //nolint:errcheck // best-effort close on shutdown
	poller, err := providergov.NewPoller(providergov.PollerOptions{
		Accounts: accounts,
		Recorder: recorder,
		Interval: interval,
		BaseURL:  baseURL,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "polling %d account(s) every %s; events → .gc/events.jsonl (Ctrl-C to stop)\n", //nolint:errcheck
		len(accounts), interval)
	poller.Run(ctx)
	return nil
}

// accountQuotaRow is one account's poll outcome: a flattened observed
// payload on success, the classified error on failure.
type accountQuotaRow struct {
	Account providergov.Account
	Payload providergov.QuotaObservedPayload
	Err     error
}

// collectAccountQuota polls every account once with a per-request timeout.
func collectAccountQuota(ctx context.Context, client *http.Client, baseURL string, accounts []providergov.Account, timeout time.Duration) []accountQuotaRow {
	rows := make([]accountQuotaRow, 0, len(accounts))
	for _, acc := range accounts {
		row := accountQuotaRow{Account: acc}
		snap, err := pollAccountWithTimeout(ctx, client, baseURL, acc, timeout)
		if err != nil {
			row.Err = err
		} else {
			row.Payload = providergov.ObservedPayloadFromSnapshot(acc.Name, snap)
		}
		rows = append(rows, row)
	}
	return rows
}

// pollAccountWithTimeout bounds one account poll with its own deadline.
func pollAccountWithTimeout(ctx context.Context, client *http.Client, baseURL string, acc providergov.Account, timeout time.Duration) (providergov.UsageSnapshot, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return providergov.PollAccount(ctx, client, baseURL, acc)
}

// providerQuotaAccountReport is the per-account slice of the report.
type providerQuotaAccountReport struct {
	Provider    string                            `json:"provider"`
	OK          bool                              `json:"ok"`
	Quota       *providergov.QuotaObservedPayload `json:"quota,omitempty"`
	ReasonClass string                            `json:"reason_class,omitempty"`
	Error       string                            `json:"error,omitempty"`
}

// providerQuotaDecisionReport is one tier's decision in the report.
type providerQuotaDecisionReport struct {
	Tier         string `json:"tier"`
	Provider     string `json:"provider,omitempty"`
	PreferSonnet bool   `json:"prefer_sonnet,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

// providerQuotaReport is the full `gc provider quota` report. The JSON
// contract (schemas/provider/quota/result.schema.json) adds the
// top-level ok:true discriminator at write time.
type providerQuotaReport struct {
	SchemaVersion string                        `json:"schema_version"`
	Accounts      []providerQuotaAccountReport  `json:"accounts"`
	Decisions     []providerQuotaDecisionReport `json:"decisions"`
}

// buildProviderQuotaReport assembles account rows and per-tier
// decisions. Only successfully-polled accounts feed SelectProvider —
// an unreachable account is unknown, not zero-utilization.
func buildProviderQuotaReport(rows []accountQuotaRow, active string, policy providergov.Policy) providerQuotaReport {
	report := providerQuotaReport{
		SchemaVersion: "1",
		Accounts:      []providerQuotaAccountReport{},
	}
	var states []providergov.AccountState
	for _, row := range rows {
		if row.Err != nil {
			report.Accounts = append(report.Accounts, providerQuotaAccountReport{
				Provider:    row.Account.Name,
				ReasonClass: providergov.ReasonClassOf(row.Err),
				Error:       row.Err.Error(),
			})
			continue
		}
		payload := row.Payload
		report.Accounts = append(report.Accounts, providerQuotaAccountReport{
			Provider: row.Account.Name,
			OK:       true,
			Quota:    &payload,
		})
		states = append(states, providergov.StateFromPayload(payload, row.Account.Name == active))
	}
	sort.Slice(report.Accounts, func(i, j int) bool {
		return report.Accounts[i].Provider < report.Accounts[j].Provider
	})

	for _, tier := range []string{config.AgentTierClaudeRequired, config.AgentTierOverflowOK} {
		entry := providerQuotaDecisionReport{Tier: tier}
		decision, err := providergov.SelectProvider(tier, states, policy)
		if err != nil {
			entry.Error = err.Error()
		} else {
			entry.Provider = decision.Provider
			entry.PreferSonnet = decision.PreferSonnet
			entry.Reason = decision.Reason
		}
		report.Decisions = append(report.Decisions, entry)
	}
	return report
}

// renderProviderQuotaText prints the report as aligned text.
func renderProviderQuotaText(w io.Writer, report providerQuotaReport) {
	if len(report.Accounts) == 0 {
		fmt.Fprintln(w, "No monitor-enabled accounts. Set quota_monitor = true and monitor_config_dir on a [providers.<name>] block.") //nolint:errcheck
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ACCOUNT\t5H%\t5H RESETS\t7D%\t7D RESETS\tOPUS%\tSONNET%\tSTATUS") //nolint:errcheck
		for _, acc := range report.Accounts {
			if !acc.OK {
				fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\t%s: %s\n", acc.Provider, acc.ReasonClass, acc.Error) //nolint:errcheck
				continue
			}
			q := acc.Quota
			fmt.Fprintf(tw, "%s\t%.1f\t%s\t%.1f\t%s\t%s\t%s\tok\n", //nolint:errcheck
				acc.Provider,
				q.FiveHourUtil, orDash(q.FiveHourResetsAt),
				q.SevenDayUtil, orDash(q.SevenDayResetsAt),
				utilOrDash(q.OpusUtil), utilOrDash(q.SonnetUtil))
		}
		tw.Flush() //nolint:errcheck
	}

	fmt.Fprintln(w) //nolint:errcheck
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tPROVIDER\tREASON") //nolint:errcheck
	for _, d := range report.Decisions {
		if d.Error != "" {
			fmt.Fprintf(tw, "%s\tERROR\t%s\n", d.Tier, d.Error) //nolint:errcheck
			continue
		}
		provider := d.Provider
		if d.PreferSonnet {
			provider += " [prefer sonnet]"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Tier, provider, d.Reason) //nolint:errcheck
	}
	tw.Flush() //nolint:errcheck
}

// orDash substitutes "-" for an empty string in text output.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// utilOrDash renders an optional utilization percent or "-".
func utilOrDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}
