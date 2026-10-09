package tracker

import (
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestFlightCodeIsStable(t *testing.T) {
	first := FlightCode("kubecon", "request-1")
	if first != FlightCode("kubecon", "request-1") || first == FlightCode("kubecon", "request-2") {
		t.Fatalf("code = %s", first)
	}
	if len(first) != 8 {
		t.Fatalf("length = %d", len(first))
	}
}

func TestFlightPassClaimsOnce(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	start := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	env.SetStartTime(start)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("claim", "first", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { t.Error(err) },
			OnComplete: func(result any, err error) {
				if err != nil {
					t.Fatal(err)
				}
				profile, ok := result.(FlightPassProfile)
				if !ok {
					t.Fatalf("result type %T", result)
				}
				if profile.Email != "ada@example.com" || profile.Name != "Ada" {
					t.Fatalf("profile = %#v", profile)
				}
			},
		})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("claim", "second", &testsuite.TestUpdateCallback{
			OnReject: func(err error) {
				var app *temporal.ApplicationError
				if !errors.As(err, &app) || app.Type() != "Claimed" {
					t.Errorf("second claim = %v", err)
				}
			},
			OnComplete: func(_ any, err error) {
				if err == nil {
					t.Fatal("second claim succeeded")
				}
			},
		})
	}, 2*time.Second)
	env.ExecuteWorkflow(FlightPassWorkflow, FlightPassInput{
		EventID: "kubecon", Code: "ABCDEFGH", Name: "Ada", Role: "SA", Email: "ada@example.com",
		RequestID: "request-1", ExpiresAt: start.Add(FlightPassLifetime),
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}
