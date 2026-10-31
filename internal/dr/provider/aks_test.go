package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestAKSCommands(t *testing.T) {
	e := newTestEnv()
	params := map[string]string{"resourceGroup": "rg-prod", "name": "aks1"}

	down := e.steps(t, plan(KindAKS, params), dr.DirDown)
	up := e.steps(t, plan(KindAKS, params), dr.DirUp)
	if got := equivalents(down)[0]; got != "az aks stop --name aks1 --resource-group rg-prod" {
		t.Errorf("down = %s", got)
	}
	if got := equivalents(up)[0]; got != "az aks start --name aks1 --resource-group rg-prod" {
		t.Errorf("up = %s", got)
	}

	withSub := map[string]string{"resourceGroup": "rg-prod", "name": "aks1", "subscription": "00000000-0000-0000-0000-000000000000"}
	if got := equivalents(e.steps(t, plan(KindAKS, withSub), dr.DirDown))[0]; got != "az aks stop --name aks1 --resource-group rg-prod --subscription 00000000-0000-0000-0000-000000000000" {
		t.Errorf("with subscription = %s", got)
	}
	if ids(down)[0] != "power/aks/rg-prod/aks1:stop" {
		t.Errorf("id = %v", ids(down))
	}

	if err := down[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.exec.lines(); got[len(got)-1] != "az aks stop --name aks1 --resource-group rg-prod" {
		t.Errorf("Apply ran %v", got)
	}
}

func TestAKSDone(t *testing.T) {
	for _, tc := range []struct {
		dir  dr.Direction
		out  string
		err  error
		done bool
		fail string
	}{
		{dr.DirDown, "Stopped\n", nil, true, ""},
		{dr.DirDown, "Running\n", nil, false, ""},
		{dr.DirUp, "Running\n", nil, true, ""},
		{dr.DirUp, "Stopped\n", nil, false, ""},
		{dr.DirUp, "", errors.New("Please run 'az login'"), false, "az login"},
		{dr.DirDown, "Stopping\n", nil, false, "power state"},
	} {
		e := newTestEnv()
		e.exec.handler = func(string, []string) (string, error) { return tc.out, tc.err }
		step := e.steps(t, plan(KindAKS, map[string]string{"resourceGroup": "rg", "name": "aks1"}), tc.dir)[0]
		done, err := step.Action.Done(context.Background())
		if tc.fail != "" {
			if err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Errorf("%s %q: err = %v", tc.dir, tc.out, err)
			}
			continue
		}
		if err != nil || done != tc.done {
			t.Errorf("%s %q: done=%v err=%v", tc.dir, tc.out, done, err)
		}
		want := "az aks show --name aks1 --resource-group rg --query powerState.code -o tsv"
		if got := e.exec.lines()[e.exec.count()-1]; got != want {
			t.Errorf("show command = %s", got)
		}
	}
}
