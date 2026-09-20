package client

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	fleetruntime "github.com/gisikw/familiar-fleet/internal/runtime"
)

// RuntimeApplier is the single implementation of "make this installable the
// active runtime". Both the explicit `runtime apply` admin seam and the
// automatic true-up against the enrolled descriptor go through it, so there is
// exactly one build/validate/smoke/GC-root/activate path.
type RuntimeApplier struct {
	Paths Paths
	// Nix and NixStore are executable names resolved lazily, so a true-up that
	// is already satisfied never requires Nix to be installed at all.
	Nix, NixStore string
	// Lookup resolves an executable name to an absolute path. Defaults to
	// FindBinary; tests substitute it.
	Lookup func(string) (string, error)
	// Progress receives human-facing narration for slow steps (builds). It may
	// be nil.
	Progress func(string)
}

// RuntimeApplyResult describes what an apply or true-up actually did.
type RuntimeApplyResult struct {
	// Target is the realized runtime now pointed at by runtime/current.
	Target string
	// Changed reports that runtime/current moved.
	Changed bool
	// Skipped reports that the desired installable was proven to already
	// correspond to the validated active runtime, so nothing was built,
	// smoke-tested, or re-activated.
	Skipped bool
}

func (a RuntimeApplier) store() fleetruntime.Store {
	return fleetruntime.New(a.Paths.RuntimeDir)
}

func (a RuntimeApplier) lookup(name string) (string, error) {
	if a.Lookup != nil {
		return a.Lookup(name)
	}
	return FindBinary(name)
}

func (a RuntimeApplier) progress(format string, args ...any) {
	if a.Progress != nil {
		a.Progress(fmt.Sprintf(format, args...))
	}
}

// satisfiedBy reports the active runtime when the recorded provenance proves
// that installable already corresponds to the validated realized output. This
// is the cheap path: no Nix evaluation, no build, no smoke run.
func (a RuntimeApplier) satisfiedBy(installable string) (string, bool) {
	store := a.store()
	record, ok := store.Applied()
	if !ok || record.Installable != installable {
		return "", false
	}
	current, err := store.Pointer(fleetruntime.CurrentName)
	if err != nil || current != record.Target {
		return "", false
	}
	// Correctness wins over cheapness: the recorded target must still be a
	// usable runtime on disk, or we fall through to the full path.
	if err := fleetruntime.Validate(current); err != nil {
		return "", false
	}
	return current, true
}

// Apply realizes installable and makes it the active runtime. It is
// idempotent: re-applying what is already active and recorded does no work.
func (a RuntimeApplier) Apply(ctx context.Context, installable string) (RuntimeApplyResult, error) {
	if target, ok := a.satisfiedBy(installable); ok {
		return RuntimeApplyResult{Target: target, Skipped: true}, nil
	}
	store := a.store()

	nix := a.Nix
	if !filepath.IsAbs(installable) {
		var err error
		if nix, err = a.lookup(a.Nix); err != nil {
			return RuntimeApplyResult{}, err
		}
		a.progress("building %s", installable)
	}
	target, err := fleetruntime.Build(ctx, nix, installable)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := fleetruntime.Validate(target); err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := fleetruntime.Smoke(ctx, target); err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("runtime failed validation; not activated: %w", err)
	}
	// Only a Nix store output can be garbage-collected out from under us, so
	// only that case needs (and supports) a GC root. A directory activated
	// from outside the store is the operator's to keep alive.
	if fleetruntime.IsStorePath(target) {
		nixStore, err := a.lookup(a.NixStore)
		if err != nil {
			return RuntimeApplyResult{}, err
		}
		if err := store.RegisterGCRoot(ctx, nixStore, target); err != nil {
			return RuntimeApplyResult{}, fmt.Errorf("retain runtime: %w", err)
		}
	}
	changed, err := store.Activate(target)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := store.PruneGCRoots(); err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("runtime activated, but pruning old GC roots failed: %w", err)
	}
	// Provenance is recorded only after the pointer is durable, so a crash
	// mid-apply can never make a future true-up skip work it did not do.
	if err := store.RecordApplied(fleetruntime.AppliedRecord{Installable: installable, Target: target}); err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("record applied runtime: %w", err)
	}
	return RuntimeApplyResult{Target: target, Changed: changed}, nil
}

// TrueUp converges the node on the enrolled descriptor. It is the automatic
// path used before the Herdr environment is prepared, and it is what reconciles
// a manual `runtime apply` override back to Familiar's authority.
func (a RuntimeApplier) TrueUp(ctx context.Context, descriptor RuntimeDescriptor) (RuntimeApplyResult, error) {
	if err := descriptor.Validate(); err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("enrolled runtime descriptor: %w", err)
	}
	result, err := a.Apply(ctx, descriptor.Installable)
	if err != nil {
		return result, fmt.Errorf("true up to the enrolled runtime %s: %w", descriptor.Installable, err)
	}
	return result, nil
}

// LogRuntimeResult narrates a true-up in the operational log.
func LogRuntimeResult(logger *log.Logger, result RuntimeApplyResult) {
	if logger == nil {
		return
	}
	switch {
	case result.Skipped:
		logger.Printf("runtime already true to the enrolled descriptor: %s", result.Target)
	case result.Changed:
		logger.Printf("runtime activated from the enrolled descriptor: %s", result.Target)
	default:
		logger.Printf("runtime re-verified against the enrolled descriptor: %s", result.Target)
	}
}
