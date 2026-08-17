package render

import "github.com/psenna/ai-sandbox/operator/api/v1alpha1"

// noneEngine is a true no-op: no sidecar, no extra containers, no volumes,
// no env, no security relaxation. The agent container runs by itself.
type noneEngine struct{}

func (noneEngine) Type() v1alpha1.EngineType { return v1alpha1.EngineTypeNone }

func (noneEngine) Contribute(Inputs) (Contribution, error) { return Contribution{}, nil }
