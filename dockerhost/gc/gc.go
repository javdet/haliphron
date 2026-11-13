// Package gc removes what accumulates on a single host and that Kubernetes
// would have collected by owner reference or by kubelet image GC: containers
// and volumes no row accounts for, and agent images nobody has used lately.
//
// It touches only what it can prove is the controller's: objects labelled
// haliphron.io/managed-by, and images that appear in the store's images table
// because an agent container ran them. An image some other workload on the
// host uses is never a candidate.
package gc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/automagicops/haliphron/controller/clusterapi"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Collector runs the collection periodically.
type Collector struct {
	Engine docker.Engine
	Store  *state.Store
	// Keep is how many most recently used agent images survive regardless.
	Keep int
	// Grace protects objects younger than this: a container created a second
	// ago may belong to a row being written right now.
	Grace    time.Duration
	Interval time.Duration
	Log      *slog.Logger
}

// Start collects every interval until the context ends.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		if !clusterapi.Wait(ctx, interval) {
			return nil
		}
		if err := c.Once(ctx); err != nil && ctx.Err() == nil {
			c.log().Warn("garbage collection", "error", err)
		}
	}
}

func (c *Collector) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Once runs one collection.
func (c *Collector) Once(ctx context.Context) error {
	rows, err := c.Store.List(ctx)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, r := range rows {
		held[strings.ToLower(string(r.RunID))] = true
	}
	grace := c.Grace
	if grace <= 0 {
		grace = 10 * time.Minute
	}
	sel := map[string]string{launcher.LabelManagedBy: launcher.ManagedBy}

	ctrs, err := c.Engine.ContainerList(ctx, sel)
	if err != nil {
		return err
	}
	inUse := map[string]bool{}
	for _, ctr := range ctrs {
		inUse[ctr.Image] = true
		run := ctr.Labels[launcher.LabelRunID]
		orphanAgent := ctr.Labels[launcher.LabelRole] == launcher.RoleAgent && run != "" && !held[run]
		// A probe that outlived its check: the controller died mid-check.
		staleProbe := ctr.Labels[launcher.LabelRole] == launcher.RoleProbe
		if (orphanAgent || staleProbe) && time.Since(time.Unix(ctr.Created, 0)) > grace {
			c.log().Info("removing a container no run accounts for", "container", ctr.ID, "runID", run)
			if err := c.Engine.ContainerRemove(ctx, ctr.ID, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
				c.log().Warn("removing an orphaned container", "container", ctr.ID, "error", err)
			}
		}
	}

	vols, err := c.Engine.VolumeList(ctx, sel)
	if err != nil {
		return err
	}
	for _, v := range vols {
		run := v.Labels[launcher.LabelRunID]
		if run == "" || held[run] {
			continue
		}
		if created, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil && time.Since(created) < grace {
			continue
		}
		c.log().Info("removing a volume no run accounts for", "volume", v.Name, "runID", run)
		// Not forced: a volume still mounted by a container is not an orphan
		// yet, and the daemon's 409 says so.
		if err := c.Engine.VolumeRemove(ctx, v.Name, false); err != nil &&
			!errors.Is(err, docker.ErrNotFound) && !errors.Is(err, docker.ErrConflict) {
			c.log().Warn("removing an orphaned volume", "volume", v.Name, "error", err)
		}
	}

	return c.images(ctx, rows, inUse)
}

// images removes agent images that no live container uses, that no held run
// names, and that are not among the Keep most recently used.
func (c *Collector) images(ctx context.Context, rows []state.Run, inUse map[string]bool) error {
	used, err := c.Store.Images(ctx)
	if err != nil {
		return err
	}
	keep := c.Keep
	if keep < 0 {
		keep = 0
	}
	wanted := map[string]bool{}
	for _, r := range rows {
		wanted[r.Spec.Image] = true
	}
	for i, img := range used {
		if i < keep || inUse[img.Ref] || wanted[img.Ref] {
			continue
		}
		err := c.Engine.ImageRemove(ctx, img.Ref)
		switch {
		case err == nil, errors.Is(err, docker.ErrNotFound):
			c.log().Info("removed an agent image nobody has used lately", "image", img.Ref, "lastUsed", img.LastUsedAt)
			if err := c.Store.ForgetImage(ctx, img.Ref); err != nil {
				return err
			}
		case errors.Is(err, docker.ErrConflict):
			// Used by a container this collector does not know about.
		default:
			c.log().Warn("removing an agent image", "image", img.Ref, "error", err)
		}
	}
	return nil
}
