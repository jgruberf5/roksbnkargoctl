package cli

import (
	"context"
	"fmt"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
)

// putApplication creates or replaces the Application on the hub with app.
//
// Argo CD's upsert replaces the whole spec, and metadata.finalizers with it.
// Two things on the hub are therefore read first:
//
//   - The controller adds its pre-delete/post-delete finalizers itself once it
//     sees the hooks; sending only ours on a re-install would strip them, and a
//     delete before the next reconcile would then skip the uninstall checks.
//   - A chart version changed on the hub (the FLO chart edited in the Argo CD
//     UI, say on F5's advice) is overwritten with the render's, which is the
//     version the BNK manifest lists. That is the intended source of truth, but
//     silently undoing an operator's edit is not: each chart whose version
//     changes is named, as the change an upgrade makes, or as a warning when
//     install puts the manifest's version back.
func putApplication(ctx context.Context, p printer, ac *argocd.Client, app *argocd.Application, upgrade bool) error {
	cur, err := ac.GetApplication(ctx, app.Metadata.Name, "")
	switch {
	case err == nil:
		app.Metadata.Finalizers = mergeFinalizers(cur.Metadata.Finalizers, app.Metadata.Finalizers)
		for _, ch := range chartRevisionChanges(cur.Spec.Sources, app.Spec.Sources) {
			if upgrade {
				p.ok("Application: chart %s %s -> %s", ch.chart, ch.from, ch.to)
			} else {
				p.warn("the Application's %s chart is %s on the hub, not the %s this release's manifest lists: changed outside roksbnkargoctl? install sets it back to %s",
					ch.chart, ch.from, ch.to, ch.to)
			}
		}
	case !argocd.IsNotFound(err):
		return fmt.Errorf("reading the Application: %w", err)
	}
	if _, err := ac.UpsertApplication(ctx, app); err != nil {
		return fmt.Errorf("creating the Application: %w", err)
	}
	return nil
}

type chartChange struct{ chart, from, to string }

// chartRevisionChanges lists the Helm sources (by chart name) whose
// targetRevision differs between the hub's Application and the render. A chart
// only one side has is not a change of version, and is not listed.
func chartRevisionChanges(hub, render []map[string]any) []chartChange {
	was := map[string]string{}
	for _, s := range hub {
		if name, _ := s["chart"].(string); name != "" {
			was[name], _ = s["targetRevision"].(string)
		}
	}
	var out []chartChange
	for _, s := range render {
		name, _ := s["chart"].(string)
		to, _ := s["targetRevision"].(string)
		if from, ok := was[name]; name != "" && ok && from != to {
			out = append(out, chartChange{name, from, to})
		}
	}
	return out
}
