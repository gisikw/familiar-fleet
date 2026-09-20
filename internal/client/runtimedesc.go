package client

import (
	"errors"
	"fmt"
	"regexp"
)

// RuntimeDescriptorSchema is the only descriptor schema this client accepts.
// A descriptor is an authority statement from Familiar, so an unknown schema
// is a hard failure rather than something to interpret optimistically.
const RuntimeDescriptorSchema = 1

// RuntimeInstallableRE pins the enrolled runtime to one exact, immutable
// Familiar commit. Branch or tag references (`main`, `refs/heads/...`) are
// deliberately unrepresentable: the node must converge on the precise output
// Familiar chose, and a moving reference would silently change what a node
// runs between two identical true-ups.
var RuntimeInstallableRE = regexp.MustCompile(`^github:gisikw/familiar/[0-9a-f]{40}#familiar-worker-runtime$`)

// ErrNoRuntimeDescriptor reports enrollment state written before the runtime
// descriptor became part of the enrollment contract.
var ErrNoRuntimeDescriptor = errors.New("enrollment predates the runtime descriptor contract")

// RuntimeDescriptor is the Familiar-owned statement of which runtime this node
// must run. It is the authority that `familiar-fleet` trues up to; the local
// `runtime apply` seam is only an admin override, reconciled on the next
// normal Herdr start.
type RuntimeDescriptor struct {
	Schema      int    `json:"schema"`
	Installable string `json:"installable"`
}

// IsZero reports a descriptor that was absent entirely, which is how
// pre-descriptor state is distinguished from a malformed descriptor.
func (d RuntimeDescriptor) IsZero() bool { return d.Schema == 0 && d.Installable == "" }

// Validate enforces the enrollment contract strictly: schema exactly 1 and an
// exact-commit Familiar installable.
func (d RuntimeDescriptor) Validate() error {
	if d.IsZero() {
		return ErrNoRuntimeDescriptor
	}
	if d.Schema != RuntimeDescriptorSchema {
		return fmt.Errorf("unsupported runtime descriptor schema %d (this client understands schema %d only)", d.Schema, RuntimeDescriptorSchema)
	}
	if d.Installable == "" {
		return errors.New("runtime descriptor omits installable")
	}
	if !RuntimeInstallableRE.MatchString(d.Installable) {
		return fmt.Errorf("runtime installable %q must be github:gisikw/familiar/<40-hex-commit>#familiar-worker-runtime; branch or tag references are not accepted", d.Installable)
	}
	return nil
}
