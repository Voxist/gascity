package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"
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
