package sandboxctl

import (
	"fmt"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// validationError builds a *ValidationError (defined in probe.go) for the
// service-set validation path. The handlers surface it verbatim via
// writeValidationError (the same path the wait/done handlers use); its
// optional Allowed field is left nil here.
func validationError(code, msg, field string) *ValidationError {
	return &ValidationError{Code: code, Message: msg, Field: field}
}

// declaration is the services.yaml wire shape: services + runtimes only. The
// per-entry types ARE the Plan 1 API types (carrying json tags), so
// sigs.k8s.io/yaml parses the camelCase YAML fields with zero drift from the
// CRD schema. EnvironmentName is intentionally absent here -- the server sets
// it from its own identity before upserting the ServiceSet CR.
type declaration struct {
	Services []v1alpha1.ServiceSpec `json:"services,omitempty"`
	Runtimes []v1alpha1.RuntimeSpec `json:"runtimes,omitempty"`
}

// ParseServicesYAML unmarshals a services.yaml document into a ServiceSetSpec
// with EnvironmentName left empty. Callers (the CLI and the server) set
// EnvironmentName and then call ValidateServiceSet.
func ParseServicesYAML(data []byte) (v1alpha1.ServiceSetSpec, error) {
	var decl declaration
	if err := sigsyaml.Unmarshal(data, &decl); err != nil {
		return v1alpha1.ServiceSetSpec{}, fmt.Errorf("parsing services.yaml: %w", err)
	}
	return v1alpha1.ServiceSetSpec{Services: decl.Services, Runtimes: decl.Runtimes}, nil
}

// ValidateServiceSet returns the first validation failure as a *ValidationError,
// or nil if spec is well-formed. It is the single source of truth shared by the
// apply client (pre-check) and the server handler (defense-in-depth: the agent
// could POST with raw curl, bypassing the CLI). Checks:
//   - EnvironmentName non-empty (the server always sets it; a client-built
//     spec with it empty is a programming error, caught here).
//   - every entry has a non-empty Name and Image.
//   - no name is repeated, whether within one list or across services+runtimes
//     (the #2 defect: the CRD's +listMapKey=name pins uniqueness WITHIN each
//     list only; a service and runtime sharing a name is the storm the
//     reconciler would hit -- rejected here, and guarded in the controller).
//   - every dependsOn reference resolves to an existing entry name.
//   - storage, when set, has a non-empty Size and MountPath.
//   - a healthcheck, when it declares a probe, declares exactly one of
//     exec/http/tcp.
func ValidateServiceSet(spec v1alpha1.ServiceSetSpec) error {
	if spec.EnvironmentName == "" {
		return validationError(CodeMissingParam, "environmentName must not be empty", "environmentName")
	}
	names := map[string]string{} // name -> kind, to detect cross-list collisions
	all := map[string]struct{}{}

	checkEntry := func(name, image, kind string) *ValidationError {
		if name == "" {
			return validationError(CodeMissingParam, kind+" entry is missing a name", "name")
		}
		if image == "" {
			return validationError(CodeMissingParam, kind+" entry "+name+" is missing an image", "image")
		}
		if prev, ok := names[name]; ok {
			if prev == kind {
				return validationError(CodeDuplicateEntryName,
					fmt.Sprintf("name %q is duplicated in the %s list", name, kind), "name")
			}
			return validationError(CodeDuplicateEntryName,
				fmt.Sprintf("name %q appears in both %s and %s; a service and runtime cannot share a name", name, prev, kind), "name")
		}
		names[name] = kind
		all[name] = struct{}{}
		return nil
	}

	for i := range spec.Services {
		s := &spec.Services[i]
		if ve := checkEntry(s.Name, s.Image, "service"); ve != nil {
			return ve
		}
		if s.Storage != nil {
			if s.Storage.Size == "" || s.Storage.MountPath == "" {
				return validationError(CodeInvalidDeclaration, "service "+s.Name+" storage requires both size and mountPath", "storage")
			}
		}
		if ve := validateHealthcheck(s.Healthcheck, "service "+s.Name); ve != nil {
			return ve
		}
	}
	for i := range spec.Runtimes {
		rt := &spec.Runtimes[i]
		if ve := checkEntry(rt.Name, rt.Image, "runtime"); ve != nil {
			return ve
		}
		if ve := validateHealthcheck(rt.Healthcheck, "runtime "+rt.Name); ve != nil {
			return ve
		}
	}
	for _, s := range spec.Services {
		for _, dep := range s.DependsOn {
			if _, ok := all[dep]; !ok {
				return validationError(CodeDanglingDependsOn, "service "+s.Name+" dependsOn unknown entry "+dep, "dependsOn")
			}
		}
	}
	for _, rt := range spec.Runtimes {
		for _, dep := range rt.DependsOn {
			if _, ok := all[dep]; !ok {
				return validationError(CodeDanglingDependsOn, "runtime "+rt.Name+" dependsOn unknown entry "+dep, "dependsOn")
			}
		}
	}
	return nil
}

func validateHealthcheck(hc v1alpha1.HealthcheckSpec, who string) *ValidationError {
	n := 0
	if len(hc.Exec) > 0 {
		n++
	}
	if hc.HTTP != nil {
		n++
	}
	if hc.TCP != nil {
		n++
	}
	if n > 1 {
		return validationError(CodeInvalidDeclaration, who+" healthcheck must set at most one of exec/http/tcp", "healthcheck")
	}
	return nil
}
