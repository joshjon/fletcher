package doctor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKVMGroupPlan(t *testing.T) {
	plan := kvmGroupPlan("kvm")

	require.Equal(t, "kvm-group", plan.ID)
	require.Equal(t, PriorityFollowup, plan.Priority)
	require.Contains(t, plan.Why, "/dev/kvm")
	require.Len(t, plan.Options, 1)
	require.Contains(t, plan.Options[0].Label, `"kvm"`)
	require.Contains(t, plan.Options[0].Steps, "sudo usermod -aG kvm fletcher")
	require.Contains(t, plan.Options[0].Steps, "fletcher daemon restart")
}

func TestKVMGroupPlanUsesTheGivenGroup(t *testing.T) {
	// The device group is not always named "kvm"; the plan must follow it.
	plan := kvmGroupPlan("vboxusers")
	require.Contains(t, plan.Options[0].Steps, "sudo usermod -aG vboxusers fletcher")
}

func TestKVMAbsentPlan(t *testing.T) {
	plan := kvmAbsentPlan()

	require.Equal(t, "enable-kvm", plan.ID)
	require.Equal(t, PriorityFollowup, plan.Priority)
	require.Contains(t, plan.Why, "runc")

	// Two ways out: enable virtualization, or accept the degraded runtime.
	require.Len(t, plan.Options, 2)
	require.Contains(t, plan.Options[0].Steps, "lsmod | grep kvm")
	require.Contains(t, plan.Options[1].Steps, "fletcher settings set runtime runc")
}
