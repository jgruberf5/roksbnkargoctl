package argocd

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// Finalizer names Argo CD understands on an Application.
const (
	ResourcesFinalizer  = "resources-finalizer.argocd.argoproj.io"
	ForegroundFinalizer = "resources-finalizer.argocd.argoproj.io/foreground"
	BackgroundFinalizer = "resources-finalizer.argocd.argoproj.io/background"
)

// Application is a minimal argoproj.io/v1alpha1 Application. Unknown fields in the
// server's answer are ignored; Operation is kept raw so its presence can be tested.
type Application struct {
	APIVersion string             `json:"apiVersion,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Metadata   ObjectMeta         `json:"metadata"`
	Spec       ApplicationSpec    `json:"spec"`
	Operation  json.RawMessage    `json:"operation,omitempty"`
	Status     *ApplicationStatus `json:"status,omitempty"`
}

// ObjectMeta is the subset of Kubernetes object metadata used here.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}

// ApplicationSpec is the subset of the Application spec used here.
type ApplicationSpec struct {
	Project     string                 `json:"project"`
	Source      *ApplicationSource     `json:"source,omitempty"`
	Destination ApplicationDestination `json:"destination"`
	SyncPolicy  *SyncPolicy            `json:"syncPolicy,omitempty"`
}

// ApplicationSource is a plain-directory Git source.
type ApplicationSource struct {
	RepoURL        string           `json:"repoURL"`
	Path           string           `json:"path,omitempty"`
	TargetRevision string           `json:"targetRevision,omitempty"`
	Directory      *SourceDirectory `json:"directory,omitempty"`
}

// SourceDirectory holds directory-source options.
type SourceDirectory struct {
	Recurse bool   `json:"recurse,omitempty"`
	Include string `json:"include,omitempty"`
	Exclude string `json:"exclude,omitempty"`
}

// ApplicationDestination is where resources are deployed.
type ApplicationDestination struct {
	Server    string `json:"server,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// SyncPolicy controls sync behaviour. Automated is left nil: the sync is manual.
type SyncPolicy struct {
	SyncOptions []string       `json:"syncOptions,omitempty"`
	Retry       *RetryStrategy `json:"retry,omitempty"`
}

// RetryStrategy is the sync retry policy.
type RetryStrategy struct {
	Limit   int64    `json:"limit,omitempty"`
	Backoff *Backoff `json:"backoff,omitempty"`
}

// Backoff is the retry backoff; durations are strings like "5s", "3m".
type Backoff struct {
	Duration    string `json:"duration,omitempty"`
	Factor      int64  `json:"factor,omitempty"`
	MaxDuration string `json:"maxDuration,omitempty"`
}

// ApplicationStatus is the subset of status used for reporting.
type ApplicationStatus struct {
	Sync           SyncStatus       `json:"sync"`
	Health         HealthStatus     `json:"health"`
	OperationState *OperationState  `json:"operationState,omitempty"`
	Resources      []ResourceStatus `json:"resources,omitempty"`
	Conditions     []AppCondition   `json:"conditions,omitempty"`
}

// SyncStatus is Synced / OutOfSync / Unknown plus the revision compared.
type SyncStatus struct {
	Status   string `json:"status,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// HealthStatus is Healthy / Progressing / Degraded / Suspended / Missing / Unknown.
type HealthStatus struct {
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// AppCondition is an Application condition (ComparisonError, SyncError, ...).
type AppCondition struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ResourceStatus is one entry of status.resources.
type ResourceStatus struct {
	Group     string        `json:"group,omitempty"`
	Version   string        `json:"version,omitempty"`
	Kind      string        `json:"kind,omitempty"`
	Namespace string        `json:"namespace,omitempty"`
	Name      string        `json:"name,omitempty"`
	Status    string        `json:"status,omitempty"`
	Health    *HealthStatus `json:"health,omitempty"`
	Hook      bool          `json:"hook,omitempty"`
	SyncWave  int64         `json:"syncWave,omitempty"`
}

// OperationState is status.operationState.
type OperationState struct {
	Phase      string               `json:"phase"`
	Message    string               `json:"message,omitempty"`
	StartedAt  string               `json:"startedAt,omitempty"`
	FinishedAt string               `json:"finishedAt,omitempty"`
	RetryCount int64                `json:"retryCount,omitempty"`
	SyncResult *SyncOperationResult `json:"syncResult,omitempty"`
}

// SyncOperationResult is operationState.syncResult.
type SyncOperationResult struct {
	Revision  string           `json:"revision,omitempty"`
	Resources []ResourceResult `json:"resources,omitempty"`
}

// ResourceResult is the per-resource outcome of a sync, including hooks.
type ResourceResult struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`    // Synced, SyncFailed, Pruned, PruneSkipped
	Message   string `json:"message,omitempty"`   //
	HookType  string `json:"hookType,omitempty"`  // PreSync, Sync, PostSync, SyncFail, PreDelete, PostDelete, Skip
	HookPhase string `json:"hookPhase,omitempty"` // Pending, Running, Succeeded, Failed, Error, Terminating
	SyncPhase string `json:"syncPhase,omitempty"` // PreSync, Sync, PostSync, SyncFail
}

// Operation phases.
const (
	PhaseRunning     = "Running"
	PhaseTerminating = "Terminating"
	PhaseFailed      = "Failed"
	PhaseError       = "Error"
	PhaseSucceeded   = "Succeeded"
)

// PhaseCompleted mirrors OperationPhase.Completed() upstream.
func PhaseCompleted(p string) bool {
	return p == PhaseFailed || p == PhaseError || p == PhaseSucceeded
}

func (c *Client) appQuery(appNamespace string) url.Values {
	q := url.Values{}
	if appNamespace != "" {
		q.Set("appNamespace", appNamespace)
	}
	if c.Project != "" {
		q.Set("project", c.Project)
	}
	return q
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// UpsertApplication creates or updates app:
// POST /api/v1/applications?upsert=true&validate=true.
func (c *Client) UpsertApplication(ctx context.Context, app *Application) (*Application, error) {
	body := *app
	if body.APIVersion == "" {
		body.APIVersion = "argoproj.io/v1alpha1"
	}
	if body.Kind == "" {
		body.Kind = "Application"
	}
	body.Status = nil
	body.Operation = nil
	var out Application
	if err := c.do(ctx, "create application "+app.Metadata.Name, "POST",
		"/api/v1/applications?upsert=true&validate=true", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetApplication reads one Application (GET /api/v1/applications/{name}). A missing
// app yields IsNotFound when Client.Project is set (see Client.Project).
func (c *Client) GetApplication(ctx context.Context, name, appNamespace string) (*Application, error) {
	var out Application
	if err := c.do(ctx, "read application "+name, "GET",
		withQuery("/api/v1/applications/"+url.PathEscape(name), c.appQuery(appNamespace)), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SyncOptions are the knobs of one manual sync.
type SyncOptions struct {
	Prune        bool
	Revision     string
	SyncOptions  []string // e.g. ServerSideApply=true; empty uses the app's own
	AppNamespace string
}

type syncBody struct {
	Name         string        `json:"name"`
	AppNamespace string        `json:"appNamespace,omitempty"`
	Project      string        `json:"project,omitempty"`
	Prune        bool          `json:"prune"`
	Revision     string        `json:"revision,omitempty"`
	SyncOptions  *syncOptItems `json:"syncOptions,omitempty"`
}

type syncOptItems struct {
	Items []string `json:"items"`
}

// Sync starts a sync: POST /api/v1/applications/{name}/sync. It returns once the
// operation is requested; use WaitOperation for the outcome.
func (c *Client) Sync(ctx context.Context, name string, o SyncOptions) (*Application, error) {
	body := syncBody{Name: name, AppNamespace: o.AppNamespace, Project: c.Project, Prune: o.Prune, Revision: o.Revision}
	if len(o.SyncOptions) > 0 {
		body.SyncOptions = &syncOptItems{Items: o.SyncOptions}
	}
	var out Application
	if err := c.do(ctx, "sync application "+name, "POST",
		"/api/v1/applications/"+url.PathEscape(name)+"/sync", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete deletes an Application: DELETE /api/v1/applications/{name}?cascade=..&propagationPolicy=..
// With cascade, Argo CD adds the propagation-policy finalizer, runs PreDelete hooks, prunes
// the resources and runs PostDelete hooks before the Application disappears. Argo CD rejects
// a propagationPolicy with cascade=false, so it is only sent when cascade is true.
func (c *Client) Delete(ctx context.Context, name string, cascade bool, propagationPolicy string) error {
	q := c.appQuery("")
	q.Set("cascade", strconv.FormatBool(cascade))
	if cascade && propagationPolicy != "" {
		q.Set("propagationPolicy", propagationPolicy)
	}
	return c.do(ctx, "delete application "+name, "DELETE",
		withQuery("/api/v1/applications/"+url.PathEscape(name), q), nil, nil)
}

// ResourceNode is one node of the application resource tree.
type ResourceNode struct {
	Group           string        `json:"group,omitempty"`
	Version         string        `json:"version,omitempty"`
	Kind            string        `json:"kind,omitempty"`
	Namespace       string        `json:"namespace,omitempty"`
	Name            string        `json:"name,omitempty"`
	UID             string        `json:"uid,omitempty"`
	Health          *HealthStatus `json:"health,omitempty"`
	CreatedAt       string        `json:"createdAt,omitempty"`
	ResourceVersion string        `json:"resourceVersion,omitempty"`
	Info            []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"info,omitempty"`
}

// ApplicationTree is the live resource tree.
type ApplicationTree struct {
	Nodes         []ResourceNode `json:"nodes,omitempty"`
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
}

// ResourceTree reads GET /api/v1/applications/{name}/resource-tree.
func (c *Client) ResourceTree(ctx context.Context, name, appNamespace string) (*ApplicationTree, error) {
	var out ApplicationTree
	if err := c.do(ctx, "read resource tree of "+name, "GET",
		withQuery("/api/v1/applications/"+url.PathEscape(name)+"/resource-tree", c.appQuery(appNamespace)), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ManagedResource is one item of the managed-resources answer (states are JSON strings).
type ManagedResource struct {
	Group       string `json:"group,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
	Hook        bool   `json:"hook,omitempty"`
	Modified    bool   `json:"modified,omitempty"`
	LiveState   string `json:"liveState,omitempty"`
	TargetState string `json:"targetState,omitempty"`
}

// ManagedResources reads GET /api/v1/applications/{name}/managed-resources.
func (c *Client) ManagedResources(ctx context.Context, name, appNamespace string) ([]ManagedResource, error) {
	var out struct {
		Items []ManagedResource `json:"items"`
	}
	if err := c.do(ctx, "read managed resources of "+name, "GET",
		withQuery("/api/v1/applications/"+url.PathEscape(name)+"/managed-resources", c.appQuery(appNamespace)), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// JobStatus summarises one Job, for status output.
type JobStatus struct {
	Namespace string
	Name      string
	Health    string // Healthy (complete), Progressing, Degraded (failed), Missing
	Message   string
	HookType  string // from the last operation's syncResult, when the Job was a hook
	HookPhase string
}

// Jobs lists the Jobs in tree, merged with hook phases/messages from the last operation
// (hook Jobs deleted by a hook-delete-policy survive only in the operation result).
func Jobs(tree *ApplicationTree, op *OperationState) []JobStatus {
	var out []JobStatus
	idx := map[string]int{}
	key := func(ns, n string) string { return ns + "/" + n }
	if tree != nil {
		for _, n := range tree.Nodes {
			if n.Kind != "Job" || n.Group != "batch" {
				continue
			}
			j := JobStatus{Namespace: n.Namespace, Name: n.Name}
			if n.Health != nil {
				j.Health, j.Message = n.Health.Status, n.Health.Message
			}
			idx[key(n.Namespace, n.Name)] = len(out)
			out = append(out, j)
		}
	}
	if op != nil && op.SyncResult != nil {
		for _, r := range op.SyncResult.Resources {
			if r.Kind != "Job" || r.HookType == "" {
				continue
			}
			i, ok := idx[key(r.Namespace, r.Name)]
			if !ok {
				idx[key(r.Namespace, r.Name)] = len(out)
				out = append(out, JobStatus{Namespace: r.Namespace, Name: r.Name, Health: "Missing"})
				i = len(out) - 1
			}
			out[i].HookType, out[i].HookPhase = r.HookType, r.HookPhase
			if r.Message != "" {
				out[i].Message = r.Message
			}
		}
	}
	return out
}
