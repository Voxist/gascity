// Command delivery-warden runs the delivery-warden sweep once and exits.
// It is registered as an exec-type order (interval=2m, idempotent=true).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "delivery-warden:", err)
		os.Exit(1)
	}
}

// repoFlag accumulates repeated --repo=owner/repo flags.
type repoFlag [][2]string

func (r *repoFlag) String() string {
	parts := make([]string, len(*r))
	for i, pair := range *r {
		parts[i] = pair[0] + "/" + pair[1]
	}
	return strings.Join(parts, ",")
}

func (r *repoFlag) Set(v string) error {
	owner, repo, ok := strings.Cut(v, "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("repo must be owner/repo, got %q", v)
	}
	*r = append(*r, [2]string{owner, repo})
	return nil
}

// recoveryTargetFlag accumulates repeated --recovery-target=phase=rig/pool flags.
type recoveryTargetFlag map[string]string

func (f *recoveryTargetFlag) String() string {
	parts := make([]string, 0, len(*f))
	for phase, target := range *f {
		parts = append(parts, phase+"="+target)
	}
	return strings.Join(parts, ",")
}

func (f *recoveryTargetFlag) Set(v string) error {
	phase, target, ok := strings.Cut(v, "=")
	if !ok || phase == "" || target == "" {
		return fmt.Errorf("recovery-target must be phase=rig/pool, got %q", v)
	}
	if *f == nil {
		*f = make(map[string]string)
	}
	(*f)[phase] = target
	return nil
}

func run() error {
	var repos repoFlag
	var recoveryTargets recoveryTargetFlag
	heartbeatFile := flag.String("heartbeat-file", "", "heartbeat file path (default: /tmp/gc-delivery-warden.heartbeat)")
	flag.Var(&repos, "repo", "owner/repo pair to scan (repeatable)")
	flag.Var(&recoveryTargets, "recovery-target", "phase=rig/pool override for recovery nudges (repeatable); falls back to voxist-platform/* defaults")
	flag.Parse()

	// Supplement --repo flags with GC_WARDEN_REPOS env var (comma-separated).
	if envRepos := os.Getenv("GC_WARDEN_REPOS"); envRepos != "" {
		for _, item := range strings.Split(envRepos, ",") {
			_ = repos.Set(strings.TrimSpace(item))
		}
	}

	// Supplement --recovery-target flags with GC_WARDEN_RECOVERY_TARGETS env var
	// (comma-separated phase=rig/pool items, e.g. "review-pending=voxist-web/voxist.reviewer").
	if envTargets := os.Getenv("GC_WARDEN_RECOVERY_TARGETS"); envTargets != "" {
		for _, item := range strings.Split(envTargets, ",") {
			_ = recoveryTargets.Set(strings.TrimSpace(item))
		}
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}

	beadsDir := os.Getenv("BEADS_DIR")
	if beadsDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("BEADS_DIR not set and cannot determine cwd: %w", err)
		}
		beadsDir = cwd
	}

	store := beads.NewBdStore(beadsDir, beads.ExecCommandRunner())
	gh := newGitHubClient(token)
	mail := &gcMailSender{}
	w := NewWarden(store, gh, mail)
	if len(recoveryTargets) > 0 {
		w.SetRecoveryTargets(map[string]string(recoveryTargets))
	}
	return w.Sweep([][2]string(repos), *heartbeatFile)
}

// --- production GitHub REST client ---

type gitHubRESTClient struct {
	httpClient *http.Client
	token      string
}

func newGitHubClient(token string) *gitHubRESTClient {
	return &gitHubRESTClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		token:      token,
	}
}

type ghPRResponse struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	MergedAt *string `json:"merged_at"`
}

func (c *gitHubRESTClient) get(url string, out interface{}) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *gitHubRESTClient) ListOpenPRs(owner, repo string) ([]PullRequest, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=100", owner, repo)
	var raw []ghPRResponse
	if err := c.get(url, &raw); err != nil {
		return nil, err
	}
	prs := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PullRequest{
			Owner:   owner,
			Repo:    repo,
			Number:  r.Number,
			URL:     r.HTMLURL,
			HeadRef: r.Head.Ref,
			State:   "OPEN",
		})
	}
	return prs, nil
}

func (c *gitHubRESTClient) GetPR(prURL string) (PullRequest, error) {
	// Parse https://github.com/owner/repo/pull/123 → API path.
	rest := strings.TrimPrefix(prURL, "https://github.com/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 || parts[2] != "pull" {
		return PullRequest{}, fmt.Errorf("unrecognized PR URL: %s", prURL)
	}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%s", parts[0], parts[1], parts[3])
	var r ghPRResponse
	if err := c.get(apiURL, &r); err != nil {
		return PullRequest{}, err
	}
	pr := PullRequest{
		Owner:   parts[0],
		Repo:    parts[1],
		Number:  r.Number,
		URL:     r.HTMLURL,
		HeadRef: r.Head.Ref,
	}
	switch {
	case r.MergedAt != nil:
		pr.State = "MERGED"
		if t, err := time.Parse(time.RFC3339, *r.MergedAt); err == nil {
			pr.MergedAt = &t
		}
	case strings.EqualFold(r.State, "closed"):
		pr.State = "CLOSED"
	default:
		pr.State = "OPEN"
	}
	return pr, nil
}

// --- production mail sender ---

type gcMailSender struct{}

func (s *gcMailSender) Send(_, to, subject, body string) error {
	cmd := exec.Command("gc", "mail", "send", "--to", to, "-s", subject, "-m", body)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
