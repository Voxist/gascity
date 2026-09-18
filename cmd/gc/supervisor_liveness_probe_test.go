package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// TestSupervisorPingReply_OnlyAParseablePIDEstablishesALiveSupervisor is the
// guard for the state a bare pid cannot carry.
//
// supervisorAliveAtPathUntil returns 0 for every one of the rows below, and
// its callers read 0 as "no supervisor is running" — which for `gc doctor`'s
// binary-divergence check is a green verdict. A supervisor that is up,
// executing a stale image, with a wedged control socket is therefore
// indistinguishable from one that was never started, and that is the exact
// state an operator runs `gc doctor` to find.
//
// A socket that accepted the connection has already proved something is
// listening. Whatever happens after that can only fail to answer the
// question, never settle it in the negative.
func TestSupervisorPingReply_OnlyAParseablePIDEstablishesALiveSupervisor(t *testing.T) {
	for name, tc := range map[string]struct {
		reply        string
		n            int
		err          error
		wantPID      int
		wantLiveness supervisorLiveness
	}{
		// The wedged supervisor: accepted, then never wrote before the
		// read deadline.
		"read timed out":     {"", 0, os.ErrDeadlineExceeded, 0, supervisorLivenessUnknown},
		"connection reset":   {"", 0, syscall.ECONNRESET, 0, supervisorLivenessUnknown},
		"short read":         {"", 0, nil, 0, supervisorLivenessUnknown},
		"unparseable reply":  {"busy\n", 5, nil, 0, supervisorLivenessUnknown},
		"zero pid":           {"0\n", 2, nil, 0, supervisorLivenessUnknown},
		"negative pid":       {"-5\n", 3, nil, 0, supervisorLivenessUnknown},
		"pid with a newline": {"4242\n", 5, nil, 4242, supervisorLivenessAlive},
		"pid unterminated":   {"4242", 4, nil, 4242, supervisorLivenessAlive},
	} {
		t.Run(name, func(t *testing.T) {
			pid, liveness := supervisorPingReply([]byte(tc.reply), tc.n, tc.err)

			if liveness != tc.wantLiveness {
				t.Fatalf("liveness = %v, want %v", liveness, tc.wantLiveness)
			}
			if pid != tc.wantPID {
				t.Errorf("pid = %d, want %d", pid, tc.wantPID)
			}
		})
	}
}

// TestDialFailureLiveness_OnlyARefusedDialProvesNoSupervisor is the other half.
// A missing socket file or a refused connection is what a supervisor that is
// not running leaves behind; a dial that timed out or was denied says nothing,
// and reporting it as "not running" is the same false negative one layer down.
func TestDialFailureLiveness_OnlyARefusedDialProvesNoSupervisor(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want supervisorLiveness
	}{
		"socket file absent":  {fs.ErrNotExist, supervisorLivenessAbsent},
		"wrapped ENOENT":      {&os.PathError{Op: "dial", Err: syscall.ENOENT}, supervisorLivenessAbsent},
		"nothing listening":   {&os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}, supervisorLivenessAbsent},
		"dial timed out":      {os.ErrDeadlineExceeded, supervisorLivenessUnknown},
		"permission denied":   {&os.PathError{Op: "dial", Err: syscall.EACCES}, supervisorLivenessUnknown},
		"listen backlog full": {syscall.EAGAIN, supervisorLivenessUnknown},
		"unclassified":        {errors.New("something else"), supervisorLivenessUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			if got := dialFailureLiveness(tc.err); got != tc.want {
				t.Errorf("dialFailureLiveness(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestProbeSupervisorAtPathUntil_NoSocketAndNoBudget walks the two states the
// probe reaches without anything listening. They are opposites and must not
// collapse into each other: nothing at the path is a settled negative, while a
// budget that was already spent means the probe never ran at all.
func TestProbeSupervisorAtPathUntil_NoSocketAndNoBudget(t *testing.T) {
	// A short path that is never created. macOS caps a unix socket address at
	// ~104 bytes and this package's TMPDIR is already past that, so a
	// t.TempDir() path would fail the dial with EINVAL — "the probe learned
	// nothing" — instead of the ENOENT this case is about. Nothing is written
	// here: the probe only dials.
	sock := fmt.Sprintf("/tmp/gc-supervisor-absent-%d.sock", os.Getpid())

	pid, liveness := probeSupervisorAtPathUntil(sock, time.Now().Add(time.Second))
	if liveness != supervisorLivenessAbsent {
		t.Errorf("liveness = %v for a path nothing is listening on, want supervisorLivenessAbsent", liveness)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0", pid)
	}

	if _, liveness := probeSupervisorAtPathUntil(sock, time.Now().Add(-time.Second)); liveness != supervisorLivenessUnknown {
		t.Errorf("liveness = %v for an exhausted budget, want supervisorLivenessUnknown: the probe never ran", liveness)
	}

	// The pid-only wrapper the existing callers use must keep behaving as it
	// did; the tri-state was added under it, not in front of it.
	if got := supervisorAliveAtPathUntil(sock, time.Now().Add(time.Second)); got != 0 {
		t.Errorf("supervisorAliveAtPathUntil on a missing socket = %d, want 0", got)
	}
}

// TestProbeRunningSupervisorAt_OneUnansweredCandidateSinksTheWholeWalk is the
// guard for the joint the per-path tests leave open.
//
// The walk visits every candidate socket and has to fold their answers into
// one. Absent is only earned when EVERY candidate refused outright: a
// candidate that accepted a connection and then went quiet may be a supervisor
// that is up, executing a stale image and wedged, and folding it in as "not
// running" hands `gc doctor` the one pid value that licenses a green verdict.
// That is F2 reintroduced whole, one function above where it was fixed.
//
// The probe is injected so the aggregation is tested without a listener — the
// resource census ratchets untagged net.Listen at zero growth — and without
// supervisorSocketPathCandidates, which panics in a test binary rather than
// resolve the host's runtime dir.
func TestProbeRunningSupervisorAt_OneUnansweredCandidateSinksTheWholeWalk(t *testing.T) {
	answers := func(m map[string]struct {
		pid      int
		liveness supervisorLiveness
	},
	) func(string) (int, supervisorLiveness) {
		return func(path string) (int, supervisorLiveness) {
			got := m[path]
			return got.pid, got.liveness
		}
	}
	type answer = struct {
		pid      int
		liveness supervisorLiveness
	}

	for name, tc := range map[string]struct {
		paths        []string
		replies      map[string]answer
		wantPath     string
		wantPID      int
		wantLiveness supervisorLiveness
	}{
		"every candidate refused": {
			paths:        []string{"a", "b"},
			replies:      map[string]answer{"a": {0, supervisorLivenessAbsent}, "b": {0, supervisorLivenessAbsent}},
			wantLiveness: supervisorLivenessAbsent,
		},
		"one refused, one went quiet": {
			paths:        []string{"a", "b"},
			replies:      map[string]answer{"a": {0, supervisorLivenessAbsent}, "b": {0, supervisorLivenessUnknown}},
			wantLiveness: supervisorLivenessUnknown,
		},
		"quiet candidate first": {
			paths:        []string{"a", "b"},
			replies:      map[string]answer{"a": {0, supervisorLivenessUnknown}, "b": {0, supervisorLivenessAbsent}},
			wantLiveness: supervisorLivenessUnknown,
		},
		"a live candidate wins outright": {
			paths:        []string{"a", "b"},
			replies:      map[string]answer{"a": {0, supervisorLivenessUnknown}, "b": {4242, supervisorLivenessAlive}},
			wantPath:     "b",
			wantPID:      4242,
			wantLiveness: supervisorLivenessAlive,
		},
		"the first live candidate is the answer": {
			paths:        []string{"a", "b"},
			replies:      map[string]answer{"a": {31337, supervisorLivenessAlive}, "b": {4242, supervisorLivenessAlive}},
			wantPath:     "a",
			wantPID:      31337,
			wantLiveness: supervisorLivenessAlive,
		},
		"no candidates at all establishes nothing": {
			paths:        nil,
			replies:      map[string]answer{},
			wantLiveness: supervisorLivenessUnknown,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path, pid, liveness := probeRunningSupervisorAt(tc.paths, answers(tc.replies))

			if liveness != tc.wantLiveness {
				t.Fatalf("liveness = %v, want %v", liveness, tc.wantLiveness)
			}
			if pid != tc.wantPID {
				t.Errorf("pid = %d, want %d", pid, tc.wantPID)
			}
			if path != tc.wantPath {
				t.Errorf("socket path = %q, want %q", path, tc.wantPath)
			}
		})
	}
}

// TestDoctorPIDForLiveness_OnlyASettledNegativeBecomesZero pins the last
// translation before the value leaves cmd/gc. 0 is the pid that makes
// binary-divergence report StatusOK "supervisor not running", so only a probe
// that actually established absence may produce it.
func TestDoctorPIDForLiveness_OnlyASettledNegativeBecomesZero(t *testing.T) {
	if got := doctorPIDForLiveness(4242, supervisorLivenessAlive); got != 4242 {
		t.Errorf("alive → %d, want the probed pid 4242", got)
	}
	if got := doctorPIDForLiveness(0, supervisorLivenessAbsent); got != 0 {
		t.Errorf("absent → %d, want 0", got)
	}
	if got := doctorPIDForLiveness(0, supervisorLivenessUnknown); got != doctor.SupervisorPIDUnknown {
		t.Errorf("unknown → %d, want doctor.SupervisorPIDUnknown (%d)", got, doctor.SupervisorPIDUnknown)
	}
	// A value outside the enum must not fall through to the green answer
	// either; the sentinel is the fail-safe direction.
	if got := doctorPIDForLiveness(0, supervisorLiveness(99)); got != doctor.SupervisorPIDUnknown {
		t.Errorf("unclassified liveness → %d, want doctor.SupervisorPIDUnknown (%d)", got, doctor.SupervisorPIDUnknown)
	}
}
