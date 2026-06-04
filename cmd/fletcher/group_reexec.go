package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// reexecSentinelEnv marks a process that has already been re-executed under
// the daemon's group via sg(1). It guards against an infinite re-exec loop if
// the child still cannot reach the socket (e.g. an old daemon binary that
// chmods its socket 0600, or sg failing to change the group).
const reexecSentinelEnv = "FLETCHER_GROUP_REEXEC"

// maybeReexecUnderDaemonGroup transparently re-runs the CLI under the daemon
// socket's owning group when the operator has been added to that group but
// their current login session predates the change.
//
// A process's supplementary groups are fixed when its login session is
// created. `usermod -aG fletcher $USER` updates /etc/group, but an already
// running shell keeps its old credential set until the next login. The classic
// symptom is `fletcher <cmd>` failing with a permission error on the socket
// right after `make install`, with the fix ("log out and back in") easy to
// miss. We cannot inject a group into a live shell, but we can re-exec
// ourselves through sg(1), which is setgid-root and consults /etc/group
// directly - so the operator never has to type `newgrp` or log out.
//
// This is best-effort: any detection miss or error returns silently and lets
// the normal command flow (and `fletcher doctor`) guide the operator. It only
// fires when ALL of the following hold, which together mean "added to the
// group but not yet active in this session":
//
//   - we are not already a re-exec child (sentinel env unset);
//   - the platform is Linux and sg(1) is available;
//   - a daemon socket exists, owned by a group we belong to on disk but which
//     is absent from this process's active group set.
func maybeReexecUnderDaemonGroup() {
	if runtime.GOOS != "linux" {
		return
	}
	if os.Getenv(reexecSentinelEnv) != "" {
		return
	}
	// `fletcher serve` creates the socket; never re-exec the daemon itself.
	if hasServeCommand(os.Args[1:]) {
		return
	}
	gid, ok := socketOwningGID()
	if !ok {
		return
	}
	if inActiveGroups(gid) {
		return // already able to reach the socket; nothing to do.
	}
	if !memberOnDisk(gid) {
		return // genuinely not in the group; doctor will say so.
	}
	grp, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return
	}
	sg, err := exec.LookPath("sg")
	if err != nil {
		return // no sg(1); fall through to the normal permission error.
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}
	// sg GROUP -c COMMAND runs COMMAND under GROUP via a shell. Re-exec the
	// real binary so stdio and the exit code pass straight through, and set
	// the sentinel so the child never loops back here.
	argv := append([]string{self}, os.Args[1:]...)
	command := reexecSentinelEnv + "=1 exec " + shellJoin(argv)
	// syscall.Exec replaces this process; on success it does not return. If it
	// does return, it failed - fall through to the normal command flow.
	//nolint:gosec // re-exec of our own binary under sg(1); argv is shell-quoted above
	_ = syscall.Exec(sg, []string{"sg", grp.Name, "-c", command}, os.Environ())
}

// socketOwningGID returns the group that owns the daemon socket. It prefers
// the socket's own group, falling back to its parent directory: when the
// operator is not yet in the group, the 0750 runtime directory denies them
// stat() on the socket inode while the directory itself stays statable. Both
// are created with the same fletcher:fletcher ownership under systemd.
func socketOwningGID() (int, bool) {
	path := os.Getenv("FLETCHER_SOCKET")
	if path == "" {
		path = defaultSocketPath()
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		fi, err := os.Stat(p) //nolint:gosec // socket path comes from the daemon's own config, not untrusted input
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(st.Gid), true
		}
	}
	return 0, false
}

// inActiveGroups reports whether gid is in this process's active credential
// set (its real GID or one of its supplementary groups).
func inActiveGroups(gid int) bool {
	if syscall.Getgid() == gid {
		return true
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// memberOnDisk reports whether the current user is a member of gid according
// to /etc/group (the CGO-free os/user implementation parses it directly),
// independent of this process's active group set.
func memberOnDisk(gid int) bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false
	}
	target := strconv.Itoa(gid)
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// hasServeCommand reports whether the daemon subcommand is being invoked, so
// the re-exec guard can leave `fletcher serve` alone.
func hasServeCommand(args []string) bool {
	for _, a := range args {
		if a == "serve" {
			return true
		}
	}
	return false
}

// shellJoin renders argv as a single POSIX-shell-safe command string for
// `sg GROUP -c`.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote single-quotes s when it contains anything outside a conservative
// safe set, escaping any embedded single quote the POSIX way: close the quote,
// emit a backslash-escaped quote, reopen.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_/.:=,@+", r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
