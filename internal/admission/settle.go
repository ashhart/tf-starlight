package admission

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashhart/tf-starlight/internal/ids"
)

// Usage is what a request consumed; Observed is false when the final usage never arrived.
type Usage struct {
	Prompt     int64
	Completion int64
	Cached     int64
	Micros     int64
	Observed   bool
}

// Record is the usage row written when a lease settles.
type Record struct {
	RequestID      string
	PrincipalType  string
	PrincipalID    string
	UserID         string
	ProjectID      string
	APIKeyID       string
	TaskID         string
	ModelName      string
	Protocol       string
	Status         int
	Outcome        string
	Queue          time.Duration
	TTFT           time.Duration
	Duration       time.Duration
	Classification string
	Client         string
}

// Settle releases the lease, replaces reservations with actual use and records the request, in one statement.
func (a *Admitter) Settle(ctx context.Context, l *Lease, u Usage, rec Record) error {
	_, err := a.settle(ctx, l, u, rec, false)
	return err
}

// SettleResult returns settled, late, duplicate, complete, released, unknown, expired, unobserved or unverified.
func (a *Admitter) SettleResult(ctx context.Context, l *Lease, u Usage, rec Record) (string, error) {
	return a.settle(ctx, l, u, rec, true)
}

func (a *Admitter) settle(ctx context.Context, l *Lease, u Usage, rec Record, result bool) (string, error) {
	defer drop(l.ID)
	observed := u.Observed
	if !u.Observed {
		u = Usage{Prompt: l.req.Tokens, Micros: l.req.Micros}
	}
	args := map[string]any{"lease": l.ID, "tokens": u.Prompt + u.Completion, "micros": u.Micros, "usage": ids.New(), "request": rec.RequestID,
		"observed": observed, "lease_request": l.req.RequestID, "lease_gateway": a.GatewayID,
		"lease_principal_type": l.req.Who.PrincipalType, "lease_principal_id": l.req.Who.PrincipalID,
		"lease_project_id": l.req.Who.ProjectID,
		"principal_type":   rec.PrincipalType, "principal_id": rec.PrincipalID, "user_id": rec.UserID, "project_id": rec.ProjectID,
		"api_key_id": rec.APIKeyID, "task_id": rec.TaskID, "model": l.req.ModelID, "model_name": rec.ModelName, "route_kind": "local",
		"protocol": rec.Protocol, "status": rec.Status, "outcome": rec.Outcome, "prompt": u.Prompt, "completion": u.Completion,
		"cached": u.Cached, "cost": u.Micros, "queue_ms": rec.Queue.Milliseconds(), "ttft_ms": rec.TTFT.Milliseconds(),
		"duration_ms": rec.Duration.Milliseconds(), "classification": rec.Classification, "priority": l.req.Priority.String(), "client": rec.Client}
	if l.Target.Local() {
		args["route_id"], args["node_id"], args["replica"] = l.Target.ReplicaID, l.Target.NodeID, l.Target.ReplicaID
	} else {
		args["route_kind"], args["route_id"] = "provider", l.Target.Route.ID
	}
	var state string
	err := a.retry(ctx, func(ctx context.Context) error {
		if result {
			return a.Pool.QueryRow(ctx, "SELECT starlight_settle_result($1)", args).Scan(&state)
		}
		_, err := a.Pool.Exec(ctx, "SELECT starlight_settle($1)", args)
		return err
	})
	return state, err
}

// release gives back a lease whose caller left before using it, refunding its reservations.
func (a *Admitter) release(id string) {
	_ = a.retry(context.Background(), func(ctx context.Context) error {
		_, err := a.Pool.Exec(ctx, "SELECT starlight_release($1, 0, 0)", id)
		return err
	})
}

// retry runs f, bounded and outliving the caller, up to three times while a failure proves nothing was committed.
func (a *Admitter) retry(ctx context.Context, f func(context.Context) error) error {
	var err error
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * 50 * time.Millisecond)
		}
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout())
		err = f(tctx)
		cancel()
		if err == nil || !uncommitted(err) {
			return err
		}
	}
	return err
}

// uncommitted reports a failure that proves nothing committed: Postgres refused the statement, or it was never sent.
func uncommitted(err error) bool {
	var pg *pgconn.PgError
	var ce *pgconn.ConnectError
	return errors.As(err, &pg) || errors.As(err, &ce) || pgconn.SafeToRetry(err)
}
