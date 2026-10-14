package session_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/session"
)

func TestStoppedPolicyChangeInvalidatesSnapshot(t *testing.T) {
	for _, p := range []session.UpdateParams{{Gateway: "off"}, {EgressPolicy: "none"}} {
		t.Run("policy", func(t *testing.T) {
			rt := &fakeRuntime{}
			mgr := newManager(t, rt, newFakeSnapshot())
			_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base", EgressPolicy: "allowlist", Gateway: "on"})
			require.NoError(t, err)
			_, err = mgr.Stop(t.Context(), "dev")
			require.NoError(t, err)
			_, restart, err := mgr.Update(t.Context(), "dev", p)
			require.NoError(t, err)
			require.False(t, restart)
			require.Equal(t, 1, rt.discarded)
		})
	}
}

func TestInteractiveRestartColdBootsAfterEnvChange(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	_, restart, err := mgr.Update(t.Context(), "dev", session.UpdateParams{UpdateEnv: true, EnvVars: []session.EnvVar{{Name: "EXAMPLE", Value: "new"}}})
	require.NoError(t, err)
	require.True(t, restart)
	require.Zero(t, rt.discarded, "a running vm must not have its directory removed")
	_, err = mgr.Restart(t.Context(), "dev")
	require.NoError(t, err)
	require.Equal(t, 1, rt.discarded)
	require.Contains(t, rt.envs[1], "EXAMPLE=new")
}

func TestUpdateValidatesWholeRequestBeforeWriting(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	_, _, err = mgr.Update(t.Context(), "dev", session.UpdateParams{Gateway: "invalid", UpdateEnv: true, EnvVars: []session.EnvVar{{Name: "EXAMPLE", Value: "new"}}})
	require.Error(t, err)
	got, err := mgr.Get(t.Context(), "dev")
	require.NoError(t, err)
	require.Empty(t, got.EnvVars)
}

func TestUnchangedStoppedConfigurationKeepsSnapshot(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base", Gateway: "on"})
	require.NoError(t, err)
	_, err = mgr.Stop(t.Context(), "dev")
	require.NoError(t, err)
	_, restart, err := mgr.Update(t.Context(), "dev", session.UpdateParams{Gateway: "on"})
	require.NoError(t, err)
	require.False(t, restart)
	require.Zero(t, rt.discarded)
}
