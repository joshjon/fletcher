package host

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// RestartSupport checks that this process is supervised by the installed unit
// and an intentional nonzero exit will restart it, without requiring privileges.
func RestartSupport(ctx context.Context) (bool, string) {
	if runtime.GOOS != "linux" || os.Getenv("INVOCATION_ID") == "" {
		return false, "remote restart requires the Fletcher systemd service"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "fletcher.service", "--property=MainPID,Restart,SuccessExitStatus,RestartPreventExitStatus").Output()
	if err != nil {
		return false, "could not verify systemd restart policy; use the local service manager"
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	if props["MainPID"] != strconv.Itoa(os.Getpid()) || (props["Restart"] != "always" && props["Restart"] != "on-failure") || props["SuccessExitStatus"] != "" || props["RestartPreventExitStatus"] != "" {
		return false, "the service must restart this process on a nonzero exit, without exit-status overrides"
	}
	return true, ""
}
