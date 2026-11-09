//go:build !windows

package e2e_test

import (
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
)

// holdFailoverReservation leaves an acknowledged lease open across promotion, without an engine request.
func holdFailoverReservation(t *testing.T, f *fleet) (*admission.Lease, admission.Record) {
	t.Helper()
	subject := admission.Subject{UserID: f.user.ID, ProjectID: f.project, ModelID: f.model}
	limits, err := admission.LoadLimits(f.ctx, f.pool, subject)
	if err != nil {
		t.Fatal(err)
	}
	scopes, rates, budgets := admission.Build(limits, subject, time.Now(), 20, 0)
	request := admission.Request{RequestID: ids.New(), ModelID: f.model, Priority: levels.Standard, Class: levels.Internal,
		Local: true, Tokens: 20, Scopes: scopes, Rates: rates, Budgets: budgets, QueueUntil: time.Now().Add(5 * time.Second),
		Who: admission.Who{PrincipalType: "user", PrincipalID: f.user.ID, ProjectID: f.project}}
	lease, err := f.starlight.Admitter.Admit(f.ctx, request)
	if err != nil {
		t.Fatalf("holding a reservation across database promotion: %v", err)
	}
	return lease, admission.Record{RequestID: request.RequestID, PrincipalType: "user", PrincipalID: f.user.ID,
		UserID: f.user.ID, ProjectID: f.project, ModelName: "qwen3.8-27b", Protocol: "openai.chat", Status: 200,
		Outcome: "ok", Classification: "internal"}
}
