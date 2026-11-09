package admission_test

import (
	"context"
	"testing"

	"github.com/ashhart/tf-starlight/internal/admission"
)

func TestDirectNonReaperDeleteKeepsUnknownChargeAndObservationState(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	f.exec(t, "DELETE FROM leases WHERE id=$1", lease.ID)
	var kind string
	var charge, micros *int64
	var observed *bool
	err := f.pool.QueryRow(context.Background(), `SELECT kind,charged_tokens,charged_micros,observed
		FROM terminal_leases WHERE lease_id=$1`, lease.ID).Scan(&kind, &charge, &micros, &observed)
	if err != nil || kind != "legacy_deleted" || charge != nil || micros != nil || observed != nil {
		t.Fatalf("unknown direct delete=%s/%v/%v/%v: %v", kind, charge, micros, observed, err)
	}
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
	var used, reserved, reports int64
	err = f.pool.QueryRow(context.Background(), `SELECT used,reserved,(SELECT count(*) FROM late_usage)
		FROM counters WHERE key='tokens:day:project:once'`).Scan(&used, &reserved, &reports)
	if err != nil || used != 0 || reserved != 100 || reports != 1 {
		t.Fatalf("late observation invented a charge/refund after unknown delete: used=%d reserved=%d reports=%d: %v",
			used, reserved, reports, err)
	}
}

func TestOnlyLegacyDeletesCanHaveAnUnknownOriginalCharge(t *testing.T) {
	f := newFleet(t, 1)
	for _, c := range []struct{ kind, tokens, micros, observed string }{
		{"settled", "NULL", "NULL", "false"},
		{"released", "NULL", "NULL", "false"},
		{"reaped", "NULL", "NULL", "false"},
		{"legacy_deleted", "0", "0", "NULL"},
		{"legacy_deleted", "NULL", "NULL", "false"},
	} {
		_, err := f.pool.Exec(context.Background(), `INSERT INTO terminal_leases(lease_id,kind,charged_tokens,charged_micros,
			observed,ended_at,late_until,receipt,receipt_hash) VALUES(gen_random_uuid(),$1,`+c.tokens+`,`+c.micros+`,`+
			c.observed+`,now(),now()+interval '7 days','{}',sha256(convert_to('{}','UTF8')))`, c.kind)
		if err == nil {
			t.Fatalf("invalid terminal charge/state accepted: %+v", c)
		}
	}
}
