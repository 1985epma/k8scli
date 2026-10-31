package dbops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestEnvFallbacksWorkWithoutInjectedClockOrSleep(t *testing.T) {
	bare := &dr.Env{}
	if before := time.Now(); nowOf(bare).Before(before) {
		t.Error("nowOf must fall back to the real clock")
	}
	if nowOf(nil).IsZero() {
		t.Error("nowOf(nil) must fall back to the real clock")
	}

	if err := sleepOf(context.Background(), bare, time.Millisecond); err != nil {
		t.Errorf("sleepOf with a real timer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepOf(ctx, nil, time.Hour); err == nil {
		t.Error("a cancelled context must interrupt the real sleep")
	}
	logf(nil, "no logger: must not panic %d", 1)
}

func TestFuncActionDefaultsAndGateName(t *testing.T) {
	a := &funcAction{id: "x", title: "t", describe: "d", apply: func(context.Context) error { return nil }}
	if done, err := a.Done(context.Background()); done || err != nil {
		t.Errorf("an action without a done check is never already done, got %v, %v", done, err)
	}
	if g := (&pollGate{name: "ready"}); g.Name() != "ready" {
		t.Errorf("Name = %q", g.Name())
	}
}

func TestAsInt64AcceptsEveryDecodedNumberType(t *testing.T) {
	for _, v := range []any{int64(3), int32(3), int(3), float64(3)} {
		if n, ok := asInt64(v); !ok || n != 3 {
			t.Errorf("asInt64(%T) = %d, %v", v, n, ok)
		}
	}
	if _, ok := asInt64("3"); ok {
		t.Error("a string is not a number")
	}
	if _, ok := parseTime("not a time"); ok {
		t.Error("a bad timestamp must not parse")
	}
}

func TestCNPGGetWithoutADynamicClientIsUnreachable(t *testing.T) {
	te := newTestEnv(t, nil)
	te.env.Dynamic = nil
	c := &cnpgTarget{p: NewPlanner(Options{}), env: te.env, db: cnpgDB("pg", 0, "")}
	if _, err := c.get(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("err = %v", err)
	}
}
