// Package kubefake is an in-memory Kubernetes API server for tests of the check
// binary. It speaks just enough of the REST protocol for kube.Client: typed
// paths, list with label/field selectors, create, merge/apply patch, delete with
// finalizer semantics, and discovery. It records every request so tests can
// assert on ORDER, which is what most of the check logic is about.
//
// It is imported only from _test files, so it never reaches the binary.
package kubefake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// Request is one recorded call.
type Request struct {
	Method      string
	Path        string
	Query       url.Values
	ContentType string
	Body        []byte
}

// String is "METHOD path".
func (r Request) String() string { return r.Method + " " + r.Path }

// Reply is what a Hook returns to take over a request.
type Reply struct {
	Code int
	Body any // marshalled as JSON; a *Status for errors
}

// Status builds an API Status error body.
func Status(code int, reason, msg string) Reply {
	return Reply{Code: code, Body: map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "code": code, "reason": reason, "message": msg}}
}

// Server is the fake API server.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	objs      map[string]kube.Object // key: gv|ns|resource|name
	resources map[string]kube.APIResource
	gvs       map[string]string // group -> version
	rv        int
	reqs      []Request

	// Hook, when set, sees every request first (with the lock NOT held) and may
	// answer it. Returning nil lets the default handler run.
	Hook func(s *Server, r Request) *Reply
	// AfterDelete runs after a successful default DELETE (lock not held).
	AfterDelete func(s *Server, r Request)
}

// New starts a server with the built-in resources the checks read registered.
func New() *Server {
	s := &Server{objs: map[string]kube.Object{}, resources: map[string]kube.APIResource{}, gvs: map[string]string{}}
	for _, r := range []struct {
		g, v, res, kind string
		ns              bool
	}{
		{"", "v1", "namespaces", "Namespace", false},
		{"", "v1", "nodes", "Node", false},
		{"", "v1", "pods", "Pod", true},
		{"", "v1", "secrets", "Secret", true},
		{"", "v1", "configmaps", "ConfigMap", true},
		{"apps", "v1", "deployments", "Deployment", true},
		{"apps", "v1", "daemonsets", "DaemonSet", true},
		{"storage.k8s.io", "v1", "storageclasses", "StorageClass", false},
		{"apiextensions.k8s.io", "v1", "customresourcedefinitions", "CustomResourceDefinition", false},
		{"admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", "ValidatingWebhookConfiguration", false},
		{"admissionregistration.k8s.io", "v1", "validatingadmissionpolicies", "ValidatingAdmissionPolicy", false},
		{"admissionregistration.k8s.io", "v1", "validatingadmissionpolicybindings", "ValidatingAdmissionPolicyBinding", false},
		{"config.openshift.io", "v1", "clusterversions", "ClusterVersion", false},
		{"cert-manager.io", "v1", "clusterissuers", "ClusterIssuer", false},
	} {
		s.AddResource(r.g, r.v, r.res, r.kind, r.ns)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Client returns a kube.Client pointed at this server.
func (s *Server) Client() *kube.Client { return kube.New(s.URL, s.Server.Client(), "test-token") }

func gvKey(g, v string) string { return g + "/" + v }

// AddResource registers a served resource (a CRD becoming established).
func (s *Server) AddResource(group, version, resource, kind string, namespaced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[gvKey(group, version)+"|"+resource] = kube.APIResource{
		Name: resource, Namespaced: namespaced, Kind: kind,
		Verbs: []string{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"},
	}
	if group != "" {
		s.gvs[group] = version
	}
}

// RemoveResource stops serving a resource (a CRD deleted).
func (s *Server) RemoveResource(group, version, resource string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.resources, gvKey(group, version)+"|"+resource)
	still := false
	for k := range s.resources {
		if strings.HasPrefix(k, gvKey(group, version)+"|") {
			still = true
		}
	}
	if !still {
		delete(s.gvs, group)
	}
}

func key(gv, ns, resource, name string) string { return gv + "|" + ns + "|" + resource + "|" + name }

// Put stores obj (overwriting) under resource of group/version.
func (s *Server) Put(group, version, resource string, obj kube.Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(gvKey(group, version), resource, deepCopy(obj))
}

func (s *Server) putLocked(gv, resource string, obj kube.Object) {
	s.rv++
	md, _ := obj["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		obj["metadata"] = md
	}
	md["resourceVersion"] = fmt.Sprint(s.rv)
	if _, ok := md["uid"]; !ok {
		md["uid"] = fmt.Sprintf("uid-%d", s.rv)
	}
	ns, _ := md["namespace"].(string)
	name, _ := md["name"].(string)
	s.objs[key(gv, ns, resource, name)] = obj
}

// Get returns a COPY of a stored object (nil if absent); Put it back to change it.
func (s *Server) Get(group, version, resource, ns, name string) kube.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key(gvKey(group, version), ns, resource, name)]
	if !ok {
		return nil
	}
	return deepCopy(o)
}

func deepCopy(o kube.Object) kube.Object {
	b, _ := json.Marshal(o)
	var out kube.Object
	_ = json.Unmarshal(b, &out)
	return out
}

// Remove deletes a stored object outright, ignoring finalizers.
func (s *Server) Remove(group, version, resource, ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key(gvKey(group, version), ns, resource, name))
}

// Requests returns a copy of the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Obj is a convenience constructor.
func Obj(apiVersion, kind, ns, name string) kube.Object {
	md := map[string]any{"name": name}
	if ns != "" {
		md["namespace"] = ns
	}
	return kube.Object{"apiVersion": apiVersion, "kind": kind, "metadata": md}
}

type parsed struct {
	gv, ns, resource, name string
	discovery              bool
}

func parse(p string) (parsed, bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	var out parsed
	var rest []string
	switch {
	case len(segs) >= 2 && segs[0] == "api":
		out.gv = gvKey("", segs[1])
		rest = segs[2:]
	case len(segs) >= 3 && segs[0] == "apis":
		out.gv = gvKey(segs[1], segs[2])
		rest = segs[3:]
	case len(segs) == 1 && segs[0] == "apis":
		return parsed{discovery: true}, true
	default:
		return out, false
	}
	if len(rest) == 0 {
		out.discovery = true
		return out, true
	}
	if rest[0] == "namespaces" && len(rest) >= 3 {
		out.ns = rest[1]
		rest = rest[2:]
	}
	out.resource = rest[0]
	if len(rest) > 1 {
		out.name = rest[1]
	}
	if len(rest) > 2 {
		return out, false // subresources are not modelled
	}
	return out, true
}

func (s *Server) serve(w http.ResponseWriter, hr *http.Request) {
	body, _ := io.ReadAll(hr.Body)
	req := Request{Method: hr.Method, Path: hr.URL.Path, Query: hr.URL.Query(), ContentType: hr.Header.Get("Content-Type"), Body: body}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	if s.Hook != nil {
		if rep := s.Hook(s, req); rep != nil {
			write(w, *rep)
			return
		}
	}
	rep, deleted := s.handle(req)
	write(w, rep)
	if deleted && s.AfterDelete != nil {
		s.AfterDelete(s, req)
	}
}

func write(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Code)
	if r.Body != nil {
		_ = json.NewEncoder(w).Encode(r.Body)
	}
}

func (s *Server) handle(req Request) (Reply, bool) {
	p, ok := parse(req.Path)
	if !ok {
		return Status(404, "NotFound", "unmodelled path "+req.Path), false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.discovery {
		return s.discoveryLocked(p), false
	}
	res, served := s.resources[p.gv+"|"+p.resource]
	if !served {
		return Status(404, "NotFound", "the server could not find the requested resource"), false
	}
	k := key(p.gv, p.ns, p.resource, p.name)
	switch req.Method {
	case http.MethodGet:
		if p.name != "" {
			o, ok := s.objs[k]
			if !ok {
				return Status(404, "NotFound", fmt.Sprintf("%s %q not found", p.resource, p.name)), false
			}
			return Reply{Code: 200, Body: deepCopy(o)}, false
		}
		return Reply{Code: 200, Body: map[string]any{"kind": res.Kind + "List", "metadata": map[string]any{}, "items": s.listLocked(p, req.Query)}}, false
	case http.MethodPost:
		var o kube.Object
		if err := json.Unmarshal(req.Body, &o); err != nil {
			return Status(400, "BadRequest", err.Error()), false
		}
		name := o.Name()
		if _, exists := s.objs[key(p.gv, p.ns, p.resource, name)]; exists {
			return Status(409, "AlreadyExists", name+" already exists"), false
		}
		s.putLocked(p.gv, p.resource, o)
		return Reply{Code: 201, Body: deepCopy(o)}, false
	case http.MethodPatch:
		var patch map[string]any
		if err := json.Unmarshal(req.Body, &patch); err != nil {
			return Status(400, "BadRequest", err.Error()), false
		}
		o, exists := s.objs[k]
		if !exists {
			if !strings.HasPrefix(req.ContentType, kube.ApplyPatch) {
				return Status(404, "NotFound", p.name+" not found"), false
			}
			o = kube.Object{}
		}
		pmd, _ := patch["metadata"].(map[string]any)
		if rv, _ := pmd["resourceVersion"].(string); rv != "" && exists && rv != o.ResourceVersion() {
			return Status(409, "Conflict", "the object has been modified"), false
		}
		merged := mergePatch(map[string]any(o), patch).(map[string]any)
		s.putLocked(p.gv, p.resource, kube.Object(merged))
		code := 200
		if !exists {
			code = 201
		}
		return Reply{Code: code, Body: deepCopy(kube.Object(merged))}, false
	case http.MethodDelete:
		o, exists := s.objs[k]
		if !exists {
			return Status(404, "NotFound", p.name+" not found"), false
		}
		if len(o.Finalizers()) > 0 {
			md := o["metadata"].(map[string]any)
			if _, set := md["deletionTimestamp"]; !set {
				md["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			}
			return Reply{Code: 200, Body: deepCopy(o)}, true
		}
		delete(s.objs, k)
		return Reply{Code: 200, Body: map[string]any{"kind": "Status", "status": "Success"}}, true
	}
	return Status(405, "MethodNotAllowed", req.Method), false
}

func (s *Server) discoveryLocked(p parsed) Reply {
	if p.gv == "" {
		var groups []any
		names := make([]string, 0, len(s.gvs))
		for g := range s.gvs {
			names = append(names, g)
		}
		sort.Strings(names)
		for _, g := range names {
			v := s.gvs[g]
			groups = append(groups, map[string]any{
				"name":             g,
				"versions":         []any{map[string]any{"groupVersion": g + "/" + v, "version": v}},
				"preferredVersion": map[string]any{"groupVersion": g + "/" + v, "version": v},
			})
		}
		return Reply{Code: 200, Body: map[string]any{"kind": "APIGroupList", "groups": groups}}
	}
	var rs []kube.APIResource
	for k, r := range s.resources {
		if strings.HasPrefix(k, p.gv+"|") {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return Status(404, "NotFound", "group version not served")
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	// A status subresource, which discovery clients must skip.
	rs = append(rs, kube.APIResource{Name: rs[0].Name + "/status", Namespaced: rs[0].Namespaced, Verbs: []string{"get", "patch"}})
	return Reply{Code: 200, Body: map[string]any{"kind": "APIResourceList", "groupVersion": strings.TrimPrefix(p.gv, "/"), "resources": rs}}
}

func (s *Server) listLocked(p parsed, q url.Values) []kube.Object {
	var keys []string
	prefix := p.gv + "|"
	for k := range s.objs {
		parts := strings.SplitN(k, "|", 4)
		if !strings.HasPrefix(k, prefix) || parts[2] != p.resource {
			continue
		}
		if p.ns != "" && parts[1] != p.ns {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := []kube.Object{}
	for _, k := range keys {
		o := s.objs[k]
		if !matchLabels(o.Labels(), q.Get("labelSelector")) || !matchFields(o, q.Get("fieldSelector")) {
			continue
		}
		items = append(items, deepCopy(o))
	}
	return items
}

func matchLabels(l map[string]string, sel string) bool {
	if sel == "" {
		return true
	}
	for _, term := range strings.Split(sel, ",") {
		term = strings.TrimSpace(term)
		switch {
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			if l[k] == v {
				return false
			}
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(strings.Replace(term, "==", "=", 1), "=")
			if got, ok := l[k]; !ok || got != v {
				return false
			}
		default:
			if _, ok := l[term]; !ok {
				return false
			}
		}
	}
	return true
}

func matchFields(o kube.Object, sel string) bool {
	if sel == "" {
		return true
	}
	for _, term := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(term, "=")
		if o.String(strings.Split(k, ".")...) != v {
			return false
		}
	}
	return true
}

// mergePatch applies an RFC 7386 JSON merge patch.
func mergePatch(target any, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok || tm == nil {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}
