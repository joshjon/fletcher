package runtime

import "context"

// HostAccount identifies the locally authorized Linux account for direct execution.
type HostAccount struct {
	User string
	UID  int64
	Home string
}

// HostSessionSpec binds a host session to its recorded account and initial directory.
type HostSessionSpec struct {
	SessionID string
	Account   HostAccount
	WorkDir   string
}

// HostSessionRuntime provides explicit host sessions independently of VM and snapshot drivers.
type HostSessionRuntime interface {
	HostAccount(context.Context) (HostAccount, error)
	StartHostSession(context.Context, HostSessionSpec) (SessionHandle, error)
	StopHostSession(context.Context, HostSessionSpec) error
}
