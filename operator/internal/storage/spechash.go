package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// specHashInput is the deterministic, JSON-marshaled shape SpecHash hashes.
// Deliberately excludes anything status- or resourceVersion-derived: an
// unrelated status write (a condition flip, an observedGeneration bump)
// must never change the hash and therefore must never invalidate a
// snapshot's compatibility with its environment.
type specHashInput struct {
	Class v1alpha1.SandboxClassSpec       `json:"class"`
	Env   v1alpha1.SandboxEnvironmentSpec `json:"env"`
}

// SpecHash deterministically hashes the two specs that together determine
// whether a snapshot is safe to restore into an environment: the
// SandboxClass's spec (image, engine, storage backend, ...) and the
// SandboxEnvironment's own spec. Pure: no I/O, no cluster access. Returns
// "sha256:<hex>". Returns an ErrInvalid-kinded error if either pointer is
// nil.
func SpecHash(class *v1alpha1.SandboxClassSpec, env *v1alpha1.SandboxEnvironmentSpec) (string, error) {
	if class == nil {
		return "", fmt.Errorf("%w: class spec is required", ErrInvalid)
	}
	if env == nil {
		return "", fmt.Errorf("%w: env spec is required", ErrInvalid)
	}
	b, err := json.Marshal(specHashInput{Class: *class, Env: *env})
	if err != nil {
		return "", fmt.Errorf("storage: marshaling spec hash input: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
