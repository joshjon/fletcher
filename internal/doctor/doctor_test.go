package doctor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectPlanDedupesByID(t *testing.T) {
	r1 := Result{Plan: &PlanStep{
		ID:       "configure-endpoint",
		Priority: PriorityBlocker,
		Title:    "Get a public endpoint working",
		Options:  []PlanOption{{Label: "A", Steps: []string{"do A"}}},
	}}
	r2 := Result{Plan: &PlanStep{
		ID:       "configure-endpoint",
		Priority: PriorityBlocker,
		Title:    "Get a public endpoint working",
		Options:  []PlanOption{{Label: "B", Steps: []string{"do B"}}},
	}}
	plan := CollectPlan([]Result{r1, r2})
	require.Len(t, plan, 1, "results with the same plan ID must merge into one step")
	require.Equal(t, "configure-endpoint", plan[0].ID)
	require.Len(t, plan[0].Options, 2, "options from both results must be present")
	require.Equal(t, "A", plan[0].Options[0].Label)
	require.Equal(t, "B", plan[0].Options[1].Label)
}

func TestCollectPlanOrdersBlockersFirst(t *testing.T) {
	results := []Result{
		{Plan: &PlanStep{ID: "followup1", Priority: PriorityFollowup, Title: "later"}},
		{Plan: &PlanStep{ID: "blocker1", Priority: PriorityBlocker, Title: "now"}},
		{Plan: &PlanStep{ID: "followup2", Priority: PriorityFollowup, Title: "even later"}},
	}
	plan := CollectPlan(results)
	require.Equal(t, []string{"blocker1", "followup1", "followup2"},
		[]string{plan[0].ID, plan[1].ID, plan[2].ID})
}

func TestCollectPlanIgnoresNilPlans(t *testing.T) {
	plan := CollectPlan([]Result{
		{Status: StatusOK},
		{Status: StatusWarn},
		{Plan: &PlanStep{ID: "x", Title: "fix x"}},
	})
	require.Len(t, plan, 1)
	require.Equal(t, "x", plan[0].ID)
}

func TestSummariseCountsByStatus(t *testing.T) {
	results := []Result{
		{Status: StatusOK},
		{Status: StatusOK},
		{Status: StatusWarn},
		{Status: StatusFail},
		{Status: StatusSkip},
	}
	s := Summarise(results)
	require.Equal(t, 2, s.OK)
	require.Equal(t, 1, s.Warn)
	require.Equal(t, 1, s.Fail)
	require.Equal(t, 1, s.Skip)
}
