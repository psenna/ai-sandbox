package sandboxctl

import (
	"strconv"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Compose renders the docker-compose.yml equivalent of a ServiceSetSpec.
// Pure and deterministic: sigs.k8s.io/yaml marshals via yaml.v2, which sorts
// map keys, so two renders of the same spec are byte-identical.
//
// Mapping notes (compose-aligned, per the approved design §1):
//   - command -> entrypoint, args -> command (compose semantics).
//   - restart is always "always" (the API has no Restart field; long-lived).
//   - envFromSecret has no compose equivalent (env_file references a file, not
//     a k8s Secret) -> omitted.
//   - healthcheck.http/.tcp have no compose equivalent (compose only supports
//     `test`) -> omitted; only healthcheck.exec translates.
//   - service storage -> a named volume "<name>-data"; runtime mountWorkspace
//     (default true) -> the shared named volume "workspace".
func Compose(spec v1alpha1.ServiceSetSpec) ([]byte, error) {
	services := map[string]any{}
	volumes := map[string]struct{}{}

	for _, s := range spec.Services {
		svc := map[string]any{"image": s.Image, "restart": "always"}
		if len(s.Ports) > 0 {
			svc["ports"] = composePorts(s.Ports, s.Expose)
		}
		if len(s.Env) > 0 {
			svc["environment"] = s.Env
		}
		if s.Storage != nil {
			svc["volumes"] = []string{s.Name + "-data:" + s.Storage.MountPath}
			volumes[s.Name+"-data"] = struct{}{}
		}
		if hc := composeHealthcheck(s.Healthcheck); hc != nil {
			svc["healthcheck"] = hc
		}
		if len(s.DependsOn) > 0 {
			svc["depends_on"] = s.DependsOn
		}
		if len(s.Command) > 0 {
			svc["entrypoint"] = s.Command
		}
		if len(s.Args) > 0 {
			svc["command"] = s.Args
		}
		if s.RunAsUser != nil {
			svc["user"] = strconv.FormatInt(*s.RunAsUser, 10)
		}
		services[s.Name] = svc
	}

	for _, rt := range spec.Runtimes {
		svc := map[string]any{"image": rt.Image, "restart": "always"}
		mount := true
		if rt.MountWorkspace != nil {
			mount = *rt.MountWorkspace
		}
		if mount {
			svc["volumes"] = []string{"workspace:/workspace"}
			volumes["workspace"] = struct{}{}
		}
		// Runtimes default to staying alive (sleep infinity) so the agent can exec
		// into them. A runtime's command is the compose command (the process), NOT
		// an image-entrypoint override like services; an explicit command+args pair
		// maps to compose entrypoint+command (the same split services use) only
		// when both are set.
		switch {
		case len(rt.Command) == 0 && len(rt.Args) == 0:
			svc["command"] = []string{"sleep", "infinity"}
		case len(rt.Command) > 0:
			svc["entrypoint"] = rt.Command
			if len(rt.Args) > 0 {
				svc["command"] = rt.Args
			}
		default: // args without a command: args become the compose command
			svc["command"] = rt.Args
		}
		if len(rt.Env) > 0 {
			svc["environment"] = rt.Env
		}
		if hc := composeHealthcheck(rt.Healthcheck); hc != nil {
			svc["healthcheck"] = hc
		}
		if len(rt.DependsOn) > 0 {
			svc["depends_on"] = rt.DependsOn
		}
		if rt.RunAsUser != nil {
			svc["user"] = strconv.FormatInt(*rt.RunAsUser, 10)
		}
		services[rt.Name] = svc
	}

	out := map[string]any{"services": services}
	if len(volumes) > 0 {
		// Convert struct{} values to empty maps so compose reads them as
		// `name: {}` (a valid, default-driver volume declaration).
		volMap := map[string]any{}
		for k := range volumes {
			volMap[k] = map[string]any{}
		}
		out["volumes"] = volMap
	}
	return sigsyaml.Marshal(out)
}

func composePorts(ports []int32, expose *int32) []string {
	out := make([]string, 0, len(ports))
	for i, p := range ports {
		if expose != nil && i == 0 {
			out = append(out, strconv.FormatInt(int64(*expose), 10)+":"+strconv.FormatInt(int64(p), 10))
			continue
		}
		out = append(out, strconv.FormatInt(int64(p), 10)+":"+strconv.FormatInt(int64(p), 10))
	}
	return out
}

func composeHealthcheck(hc v1alpha1.HealthcheckSpec) map[string]any {
	if len(hc.Exec) == 0 {
		return nil // http/tcp have no compose equivalent; omit.
	}
	m := map[string]any{"test": append([]string{"CMD"}, hc.Exec...)}
	if hc.Interval != "" {
		m["interval"] = hc.Interval
	}
	return m
}
