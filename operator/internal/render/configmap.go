package render

import (
	"encoding/json"
	"fmt"
	"sort"

	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// runConfig is the fixed, deterministic shape of the ConfigMap's
// sandbox.json entry point configuration.
type runConfig struct {
	APIVersion    string        `json:"apiVersion"`
	Kind          string        `json:"kind"`
	ClusterID     string        `json:"clusterID"`
	Environment   runConfigEnv  `json:"environment"`
	Class         string        `json:"class"`
	Engine        string        `json:"engine"`
	Repo          string        `json:"repo"`
	WorkspacePath string        `json:"workspacePath"`
	AgentHome     string        `json:"agentHome"`
	ConfigPath    string        `json:"configPath"`
	TaskFile      string        `json:"taskFile"`
	SidecarURL    string        `json:"sidecarURL"`
	Task          runConfigTask `json:"task"`
}

type runConfigEnv struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
}

type runConfigTask struct {
	HasPrompt bool                `json:"hasPrompt"`
	Issue     *runConfigTaskIssue `json:"issue,omitempty"`
}

type runConfigTaskIssue struct {
	Repo   string `json:"repo"`
	Number int32  `json:"number"`
}

// renderConfigMap renders the entrypoint ConfigMap: sandbox.json (the
// resolved run configuration), task.md (the agent's task instructions), and
// one skill-<name>.md entry per in.Skills (sorted by Name for determinism;
// Skills is always empty today -- #27 populates it).
func renderConfigMap(in Inputs) (*acorev1.ConfigMapApplyConfiguration, error) {
	names := ChildNames(in.Env.Name)

	rc := runConfig{
		APIVersion: "sandbox.psenna.dev/v1alpha1",
		Kind:       "SandboxEnvironment",
		ClusterID:  in.ClusterID,
		Environment: runConfigEnv{
			Name:      in.Env.Name,
			Namespace: in.Env.Namespace,
			UID:       string(in.Env.UID),
		},
		Class:         in.Env.Spec.ClassRef.Name,
		Engine:        string(in.Class.Spec.Engine.Type),
		Repo:          in.Env.Spec.Repo,
		WorkspacePath: WorkspaceMountPath,
		AgentHome:     AgentHomePath,
		ConfigPath:    ConfigMountPath,
		TaskFile:      TaskFileName,
		SidecarURL:    SidecarBaseURL,
		Task:          renderRunConfigTask(in),
	}

	sandboxJSON, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling sandbox.json: %w", err)
	}
	sandboxJSON = append(sandboxJSON, '\n')

	data := map[string]string{
		RunConfigFileName: string(sandboxJSON),
		TaskFileName:      renderTaskMD(in),
	}

	skills := make([]SkillFile, len(in.Skills))
	copy(skills, in.Skills)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	for _, s := range skills {
		data["skill-"+s.Name+".md"] = s.Content
	}

	return acorev1.ConfigMap(names.ConfigMap, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithData(data), nil
}

// renderRunConfigTask builds sandbox.json's task block.
func renderRunConfigTask(in Inputs) runConfigTask {
	t := in.Env.Spec.Task
	if t.Prompt != "" {
		return runConfigTask{HasPrompt: true}
	}
	if t.IssueRef != nil {
		return runConfigTask{
			HasPrompt: false,
			Issue:     &runConfigTaskIssue{Repo: t.IssueRef.Repo, Number: t.IssueRef.Number},
		}
	}
	return runConfigTask{HasPrompt: false}
}

// renderTaskMD renders task.md's content: the prompt verbatim if set, else a
// deterministic one-line instruction derived from the issue reference.
func renderTaskMD(in Inputs) string {
	t := in.Env.Spec.Task
	if t.Prompt != "" {
		return t.Prompt
	}
	if t.IssueRef != nil {
		return fmt.Sprintf("Implement issue #%d in %s.", t.IssueRef.Number, t.IssueRef.Repo)
	}
	return ""
}
