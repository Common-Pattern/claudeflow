// Package sandbox runs one claudeflow run inside a fresh container.
//
// The host side (Podman) starts a container per run and answers whether it is
// still going. The container side (Inside) is claudeflow's own binary, mounted
// into the container and run as its command: it clones the run's branch, runs
// the install hook, brings the project's Compose stack up on the podman nested
// inside the container, and runs the agent.
//
// Running claudeflow itself inside means the work of preparing a run is the
// same Go code whichever side of the container boundary it happens on, and the
// project's image has to carry nothing of claudeflow's — the binary is
// mounted, so it cannot drift from the supervisor that started it.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Spec is everything the inside of a sandbox needs to prepare and start one
// run. It is written to a file the container reads; it carries no secrets,
// which reach the container as environment variables passed by name.
type Spec struct {
	// RunID names the run, for log lines.
	RunID string `json:"runID"`

	// Remote is the repository URL to clone and push.
	Remote string `json:"remote"`
	// Mirror, when set, is a path inside the container holding a git
	// directory to borrow objects from, so the clone does not fetch them all.
	Mirror string `json:"mirror,omitempty"`
	// Workdir is where the checkout goes inside the container.
	Workdir string `json:"workdir"`
	// Branch is the run's branch. Empty for a run that works on no branch —
	// planning — which is left on StartPoint, detached.
	Branch string `json:"branch,omitempty"`
	// StartPoint is the remote branch a new Branch is cut from, and what a
	// branchless run checks out.
	StartPoint string `json:"startPoint"`
	// AuthorName and AuthorEmail are the git identity commits are made under.
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`

	// Stack is false for a run that holds no slot: no install hook, no
	// Compose stack.
	Stack bool `json:"stack"`
	// Install is the project's install hook, run in the checkout before the
	// stack comes up.
	Install string `json:"install,omitempty"`
	// ComposeFile is relative to the checkout.
	ComposeFile     string            `json:"composeFile,omitempty"`
	ComposeProfiles []string          `json:"composeProfiles,omitempty"`
	ComposeEnv      map[string]string `json:"composeEnv,omitempty"`
	// Expose maps a service to the container port the agent should reach it
	// on, as compose.expose does on the host.
	Expose       map[string]int `json:"expose,omitempty"`
	ReadyTimeout time.Duration  `json:"readyTimeout"`
	// AgentEnv is the project's agent.env, expanded against the stack's
	// addresses once they are known.
	AgentEnv map[string]string `json:"agentEnv,omitempty"`

	// Command, Args and Env start the agent. Env is the run's own variables;
	// the container's environment — secrets included — is inherited too.
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Timeout time.Duration     `json:"timeout"`
	// Log is the run's transcript, mounted at the same path it has on the
	// host, so everything that reads logs there reads this one unchanged.
	Log string `json:"log"`
}

// Write stores the spec at path, readable only by the operator: it holds the
// run's prompt, which is the project's business.
func (s Spec) Write(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// ReadSpec loads a spec written by Write.
func ReadSpec(path string) (Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("parse sandbox spec %s: %w", path, err)
	}
	return s, nil
}
