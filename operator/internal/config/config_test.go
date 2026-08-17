package config

import (
	"strings"
	"testing"
	"time"
)

func emptyEnv(string) string { return "" }

func envFrom(m map[string]string) func(string) string {
	return func(k string) string {
		return m[k]
	}
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(nil, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := Config{
		SlotCapacity:            4,
		ClusterID:               "default",
		WatchNamespace:          "",
		DefaultSandboxClass:     "default",
		ClassSecretNamespace:    "ai-sandbox-operator-system",
		MetricsAddr:             ":8080",
		HealthProbeAddr:         ":8081",
		EnableLeaderElection:    true,
		LeaderElectionID:        "sandbox-operator.sandbox.psenna.dev",
		LeaderElectionNamespace: "",
		SchedulerInterval:       5 * time.Second,
	}
	if c != want {
		t.Fatalf("Load defaults = %+v, want %+v", c, want)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() on defaults: %v", err)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_SLOT_CAPACITY":         "8",
		"SANDBOX_OPERATOR_CLUSTER_ID":            "cluster-a",
		"SANDBOX_OPERATOR_WATCH_NAMESPACE":       "team-a",
		"SANDBOX_OPERATOR_DEFAULT_SANDBOX_CLASS": "large",
		"SANDBOX_OPERATOR_LEADER_ELECT":          "false",
	})

	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SlotCapacity != 8 {
		t.Errorf("SlotCapacity = %d, want 8", c.SlotCapacity)
	}
	if c.ClusterID != "cluster-a" {
		t.Errorf("ClusterID = %q, want cluster-a", c.ClusterID)
	}
	if c.WatchNamespace != "team-a" {
		t.Errorf("WatchNamespace = %q, want team-a", c.WatchNamespace)
	}
	if c.DefaultSandboxClass != "large" {
		t.Errorf("DefaultSandboxClass = %q, want large", c.DefaultSandboxClass)
	}
	if c.EnableLeaderElection {
		t.Errorf("EnableLeaderElection = true, want false")
	}
}

func TestLoad_FlagBeatsEnv(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_SLOT_CAPACITY": "8",
		"SANDBOX_OPERATOR_CLUSTER_ID":    "from-env",
	})

	c, err := Load([]string{"--slot-capacity=16", "--cluster-id=from-flag"}, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SlotCapacity != 16 {
		t.Errorf("SlotCapacity = %d, want 16 (flag should beat env)", c.SlotCapacity)
	}
	if c.ClusterID != "from-flag" {
		t.Errorf("ClusterID = %q, want from-flag (flag should beat env)", c.ClusterID)
	}
}

func TestLoad_UnknownFlagErrors(t *testing.T) {
	_, err := Load([]string{"--does-not-exist=1"}, emptyEnv)
	if err == nil {
		t.Fatal("Load with unknown flag: expected error, got nil")
	}
}

func TestValidate_SlotCapacityZeroErrors(t *testing.T) {
	c, err := Load([]string{"--slot-capacity=0"}, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with slot-capacity=0: expected error, got nil")
	} else if !strings.Contains(err.Error(), "slot-capacity") {
		t.Errorf("Validate() error = %v, want it to mention slot-capacity", err)
	}
}

func TestValidate_InvalidClusterID(t *testing.T) {
	cases := []string{"Uppercase", "has_underscore", "-leading-dash", "trailing-dash-", ""}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			var args []string
			if id != "" {
				args = []string{"--cluster-id=" + id}
			} else {
				args = []string{"--cluster-id="}
			}
			c, err := Load(args, emptyEnv)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate() with cluster-id=%q: expected error, got nil", id)
			}
		})
	}
}

func TestValidate_ValidClusterID(t *testing.T) {
	cases := []string{"default", "cluster-a", "a", "a1-b2"}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			c, err := Load([]string{"--cluster-id=" + id}, emptyEnv)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() with cluster-id=%q: unexpected error: %v", id, err)
			}
		})
	}
}

func TestValidate_EmptyDefaultSandboxClassErrors(t *testing.T) {
	c, err := Load([]string{"--default-sandbox-class="}, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with empty default-sandbox-class: expected error, got nil")
	}
}

func TestClassSecretNamespace_DefaultFallback(t *testing.T) {
	c, err := Load(nil, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.ClassSecretNamespace != "ai-sandbox-operator-system" {
		t.Errorf("ClassSecretNamespace = %q, want ai-sandbox-operator-system", c.ClassSecretNamespace)
	}
}

func TestClassSecretNamespace_EnvOverride(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_CLASS_SECRET_NAMESPACE": "custom-ns",
	})
	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.ClassSecretNamespace != "custom-ns" {
		t.Errorf("ClassSecretNamespace = %q, want custom-ns", c.ClassSecretNamespace)
	}
}

func TestClassSecretNamespace_PodNamespaceFallback(t *testing.T) {
	env := envFrom(map[string]string{
		"POD_NAMESPACE": "pod-ns",
	})
	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.ClassSecretNamespace != "pod-ns" {
		t.Errorf("ClassSecretNamespace = %q, want pod-ns", c.ClassSecretNamespace)
	}
}

func TestClassSecretNamespace_PrefixedEnvBeatsPodNamespace(t *testing.T) {
	env := envFrom(map[string]string{
		"POD_NAMESPACE": "pod-ns",
		"SANDBOX_OPERATOR_CLASS_SECRET_NAMESPACE": "prefixed-ns",
	})
	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.ClassSecretNamespace != "prefixed-ns" {
		t.Errorf("ClassSecretNamespace = %q, want prefixed-ns", c.ClassSecretNamespace)
	}
}

func TestClassSecretNamespace_FlagBeatsEnv(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_CLASS_SECRET_NAMESPACE": "env-ns",
	})
	c, err := Load([]string{"--class-secret-namespace=flag-ns"}, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.ClassSecretNamespace != "flag-ns" {
		t.Errorf("ClassSecretNamespace = %q, want flag-ns", c.ClassSecretNamespace)
	}
}

func TestValidate_InvalidClassSecretNamespaceErrors(t *testing.T) {
	c, err := Load([]string{"--class-secret-namespace=Invalid_NS"}, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with invalid class-secret-namespace: expected error, got nil")
	} else if !strings.Contains(err.Error(), "class-secret-namespace") {
		t.Errorf("Validate() error = %v, want it to mention class-secret-namespace", err)
	}
}

func TestSchedulerInterval_Default(t *testing.T) {
	c, err := Load(nil, emptyEnv)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SchedulerInterval != 5*time.Second {
		t.Errorf("SchedulerInterval = %s, want 5s", c.SchedulerInterval)
	}
}

func TestSchedulerInterval_EnvOverride(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_SCHEDULER_INTERVAL": "250ms",
	})
	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SchedulerInterval != 250*time.Millisecond {
		t.Errorf("SchedulerInterval = %s, want 250ms", c.SchedulerInterval)
	}
}

func TestSchedulerInterval_UnparseableEnvFallsBackToDefault(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_SCHEDULER_INTERVAL": "not-a-duration",
	})
	c, err := Load(nil, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SchedulerInterval != 5*time.Second {
		t.Errorf("SchedulerInterval = %s, want 5s (default) on unparseable env value", c.SchedulerInterval)
	}
}

func TestSchedulerInterval_FlagBeatsEnv(t *testing.T) {
	env := envFrom(map[string]string{
		"SANDBOX_OPERATOR_SCHEDULER_INTERVAL": "250ms",
	})
	c, err := Load([]string{"--scheduler-interval=1s"}, env)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if c.SchedulerInterval != time.Second {
		t.Errorf("SchedulerInterval = %s, want 1s (flag should beat env)", c.SchedulerInterval)
	}
}

func TestValidate_SchedulerIntervalBounds(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		wantErr bool
	}{
		{"too low", "--scheduler-interval=50ms", true},
		{"too high", "--scheduler-interval=10m", true},
		{"lower boundary accepted", "--scheduler-interval=100ms", false},
		{"upper boundary accepted", "--scheduler-interval=5m", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load([]string{tc.flag}, emptyEnv)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			err = c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() with %s: expected error, got nil", tc.flag)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() with %s: unexpected error: %v", tc.flag, err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "scheduler-interval") {
				t.Errorf("Validate() error = %v, want it to mention scheduler-interval", err)
			}
		})
	}
}
