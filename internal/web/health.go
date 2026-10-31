package web

import (
	"fmt"
	"strings"
)

// Health values understood by the UI.
const (
	healthOK    = "ok"
	healthWarn  = "warn"
	healthError = "error"
	healthDone  = "done"
)

// podHealth classifies a kubectl-style pod status, so the UI does not have to
// know every Kubernetes waiting reason.
func podHealth(status, ready string) string {
	switch status {
	case "Completed", "Succeeded":
		return healthDone
	case "Running":
		if allReady(ready) {
			return healthOK
		}
		return healthWarn
	case "Pending", "ContainerCreating", "PodInitializing", "Terminating":
		return healthWarn
	}
	// Init:0/2 is progress; Init:Error and Init:CrashLoopBackOff are not.
	if strings.HasPrefix(status, "Init:") && !strings.ContainsAny(status[len("Init:"):], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return healthWarn
	}
	return healthError
}

// allReady reports whether a READY column such as "2/2" is complete.
func allReady(ready string) bool {
	var have, want int
	if _, err := fmt.Sscanf(ready, "%d/%d", &have, &want); err != nil {
		return false
	}
	return want > 0 && have == want
}

func deploymentHealth(desired, ready int32) string {
	switch {
	case desired == 0:
		return healthDone
	case ready >= desired:
		return healthOK
	case ready == 0:
		return healthError
	default:
		return healthWarn
	}
}

// diagnose turns the usual connection failures into advice the setup wizard
// can show. It returns "" when it has nothing useful to add.
func diagnose(err error) string {
	message := strings.ToLower(err.Error())
	rules := []struct{ contains, hint string }{
		{"executable file not found", "The kubeconfig uses a credential plugin (for example aws, gcloud or kubelogin) that is not installed or not on your PATH."},
		{"connection refused", "The API server refused the connection. Make sure the cluster is running (Docker Desktop, minikube or kind for local clusters) and that the server address in the kubeconfig is right."},
		{"no such host", "The API server's host name does not resolve. Check your network, VPN or DNS."},
		{"i/o timeout", "The cluster did not answer in time. Check your VPN or firewall rules."},
		{"deadline exceeded", "The cluster did not answer in time. Check your VPN or firewall rules."},
		{"x509", "The TLS certificate was not trusted. Check certificate-authority-data in the kubeconfig."},
		{"unauthorized", "The credentials were rejected. They may have expired: refresh them with your cloud CLI."},
		{"you must be logged in", "The credentials were rejected. They may have expired: refresh them with your cloud CLI."},
		{"forbidden", "The credentials are valid, but this user is not allowed to read the cluster. Ask for read access or use another user."},
	}
	for _, rule := range rules {
		if strings.Contains(message, rule.contains) {
			return rule.hint
		}
	}
	return ""
}
