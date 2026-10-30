package cmd

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// now is replaced in tests to make ages deterministic.
var now = time.Now

func timeSince(t metav1.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return now().Sub(t.Time)
}
