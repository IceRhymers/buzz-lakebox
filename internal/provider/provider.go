// Package provider implements the provider-mode wire protocol described in
// docs/CONTRACT.md: read one JSON object from stdin, dispatch on "op", and
// always emit exactly one JSON object on stdout — never a non-zero exit for
// a handled case (CONTRACT.md §2).
package provider

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

const (
	// Name is this provider's id-qualified binary name, echoed in info
	// responses (docs/CONTRACT.md §4).
	Name = "buzz-backend-databricks"

	// Description is the human-readable info-response description
	// (docs/CONTRACT.md §4).
	Description = "Deploys Buzz agents into Databricks Lakebox sandboxes"

	// Protocol is the frozen provider protocol version.
	Protocol = "v1"

	opInfo   = "info"
	opDeploy = "deploy"
)

// supportedOps is the frozen list advertised in the info response and in
// the unknown-op error message. Order matters for the error string
// (docs/CONTRACT.md §4 "Unknown op").
var supportedOps = []string{opInfo, opDeploy}

// envelope is the minimal shape needed to route any request; deploy's
// remaining fields are parsed separately by internal/payload once we know
// op == "deploy".
type envelope struct {
	Op string `json:"op"`
}

// DeployFunc performs an actual deploy given a validated request and
// returns the sandbox id to report as agent_id. A nil DeployFunc means
// deploy is not implemented yet: every deploy op returns the M0 stub error.
type DeployFunc func(req *payload.DeployRequest) (agentID string, err error)

type infoResponse struct {
	Ok          bool     `json:"ok"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Protocol    string   `json:"protocol"`
	Ops         []string `json:"ops"`
}

type errorResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
}

type deploySuccessResponse struct {
	Ok      bool   `json:"ok"`
	AgentID string `json:"agent_id"`
}

func newErrorResponse(msg string) errorResponse {
	return errorResponse{Ok: false, Error: msg}
}

// Run reads one JSON request from r, dispatches it, and writes exactly one
// JSON response line to w. It returns a non-nil error only for
// unhandleable I/O failures (reading stdin or writing stdout) — per
// CONTRACT.md §2, every request that can be parsed enough to route is a
// "handled case" and always yields a written response with a nil error.
func Run(r io.Reader, w io.Writer, deploy DeployFunc) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	resp := route(data, deploy)

	out, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

func route(data []byte, deploy DeployFunc) any {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return newErrorResponse(fmt.Sprintf("malformed request: could not parse JSON: %v", err))
	}

	switch env.Op {
	case opInfo:
		return infoResponse{
			Ok:          true,
			Name:        Name,
			Version:     version.Version,
			Description: Description,
			Protocol:    Protocol,
			Ops:         supportedOps,
		}
	case opDeploy:
		return handleDeploy(data, deploy)
	default:
		return newErrorResponse(fmt.Sprintf("unknown op %q; supported: %s", env.Op, joinOps()))
	}
}

func handleDeploy(data []byte, deploy DeployFunc) any {
	if deploy == nil {
		return newErrorResponse("deploy not implemented yet (M1)")
	}

	req, err := payload.ParseDeployRequest(data)
	if err != nil {
		// A body that fails even to unmarshal cannot be walked for
		// per-field secrets, but scrub any bare nsec1 token that made it
		// into the error text (e.g. from a JSON syntax error snippet).
		return newErrorResponse(redact.Redact(fmt.Sprintf("malformed deploy request: %v", err), nil))
	}

	if err := req.Validate(); err != nil {
		secrets := redact.SecretsFromPayload(req.Agent)
		return newErrorResponse(redact.Redact(err.Error(), secrets))
	}

	agentID, err := deploy(req)
	if err != nil {
		secrets := redact.SecretsFromPayload(req.Agent)
		return newErrorResponse(redact.Redact(err.Error(), secrets))
	}

	return deploySuccessResponse{Ok: true, AgentID: agentID}
}

func joinOps() string {
	out := ""
	for i, op := range supportedOps {
		if i > 0 {
			out += ", "
		}
		out += op
	}
	return out
}
