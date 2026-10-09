package tracker

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const FlightPassLifetime = 30 * time.Minute

// FlightCode is a 4-digit code. The same event, request, and salt always
// produce the same code, so a retried submission can find the pass it started.
func FlightCode(eventID, requestID string, salt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", eventID, requestID, salt)))
	n := binary.BigEndian.Uint16(sum[:2])
	return fmt.Sprintf("%04d", int(n)%10000)
}

func FlightPassID(eventID, code string) string {
	return "flight-pass/" + eventID + "/" + strings.ToUpper(strings.TrimSpace(code))
}

type FlightPassInput struct {
	EventID   string    `json:"eventId"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Email     string    `json:"email"`
	RequestID string    `json:"requestId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type FlightPassProfile struct {
	EventID string `json:"eventId"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Email   string `json:"email"`
}

type flightPassState struct {
	FlightPassInput
	Claimed bool
}

func FlightPassWorkflow(ctx workflow.Context, in FlightPassInput) error {
	if in.EventID == "" || in.Code == "" || in.Email == "" || in.ExpiresAt.IsZero() {
		return temporal.NewNonRetryableApplicationError("flight pass input is incomplete", "BadInput", nil)
	}
	state := flightPassState{FlightPassInput: in}
	if err := workflow.SetQueryHandler(ctx, "pass", func() (FlightPassProfile, error) {
		return state.profile(), nil
	}); err != nil {
		return err
	}
	if err := workflow.SetQueryHandler(ctx, "request", func() (string, error) {
		return state.RequestID, nil
	}); err != nil {
		return err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "claim", func(ctx workflow.Context) (FlightPassProfile, error) {
		if state.Claimed {
			return FlightPassProfile{}, temporal.NewApplicationError("this flight code was already used", "Claimed")
		}
		if !workflow.Now(ctx).Before(state.ExpiresAt) {
			return FlightPassProfile{}, temporal.NewApplicationError("this flight code has expired", "Expired")
		}
		state.Claimed = true
		return state.profile(), nil
	}, workflow.UpdateHandlerOptions{Validator: func() error {
		if state.Claimed {
			return temporal.NewApplicationError("this flight code was already used", "Claimed")
		}
		if !workflow.Now(ctx).Before(state.ExpiresAt) {
			return temporal.NewApplicationError("this flight code has expired", "Expired")
		}
		return nil
	}}); err != nil {
		return err
	}
	remaining := state.ExpiresAt.Sub(workflow.Now(ctx))
	if remaining > 0 {
		if err := workflow.Sleep(ctx, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (s flightPassState) profile() FlightPassProfile {
	return FlightPassProfile{EventID: s.EventID, Name: s.Name, Role: s.Role, Email: s.Email}
}

func NormalizeFlightCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if len(code) != 4 {
		return "", errors.New("enter the 4 digit flight code")
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return "", errors.New("enter the 4 digit flight code")
		}
	}
	return code, nil
}
