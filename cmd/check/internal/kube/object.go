package kube

import (
	"fmt"
	"strings"
)

// Object is an untyped Kubernetes object, exactly as the API server serialises it.
type Object map[string]any

// Nested walks maps by key and returns what is there (nil if any step is missing).
func (o Object) Nested(path ...string) any {
	var cur any = map[string]any(o)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// String returns the string at path, or "".
func (o Object) String(path ...string) string {
	s, _ := o.Nested(path...).(string)
	return s
}

// Int returns the number at path, or 0. JSON numbers decode as float64.
func (o Object) Int(path ...string) int64 {
	switch v := o.Nested(path...).(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// Bool returns the bool at path, or false.
func (o Object) Bool(path ...string) bool {
	b, _ := o.Nested(path...).(bool)
	return b
}

// Slice returns the list at path.
func (o Object) Slice(path ...string) []any {
	s, _ := o.Nested(path...).([]any)
	return s
}

// Map returns the map at path as an Object (nil if absent).
func (o Object) Map(path ...string) Object {
	m, _ := o.Nested(path...).(map[string]any)
	return Object(m)
}

// StringMap returns a map[string]string at path (labels, annotations).
func (o Object) StringMap(path ...string) map[string]string {
	out := map[string]string{}
	for k, v := range o.Map(path...) {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Name is metadata.name.
func (o Object) Name() string { return o.String("metadata", "name") }

// Namespace is metadata.namespace.
func (o Object) Namespace() string { return o.String("metadata", "namespace") }

// UID is metadata.uid.
func (o Object) UID() string { return o.String("metadata", "uid") }

// ResourceVersion is metadata.resourceVersion.
func (o Object) ResourceVersion() string { return o.String("metadata", "resourceVersion") }

// Kind is kind.
func (o Object) Kind() string { return o.String("kind") }

// Labels is metadata.labels.
func (o Object) Labels() map[string]string { return o.StringMap("metadata", "labels") }

// Annotations is metadata.annotations.
func (o Object) Annotations() map[string]string { return o.StringMap("metadata", "annotations") }

// Deleting reports whether metadata.deletionTimestamp is set.
func (o Object) Deleting() bool { return o.String("metadata", "deletionTimestamp") != "" }

// Finalizers is metadata.finalizers.
func (o Object) Finalizers() []string {
	var out []string
	for _, f := range o.Slice("metadata", "finalizers") {
		if s, ok := f.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// OwnerReferences returns "Kind/name" for each owner.
func (o Object) OwnerReferences() []string {
	var out []string
	for _, r := range o.Slice("metadata", "ownerReferences") {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%v/%v", m["kind"], m["name"]))
	}
	return out
}

// Condition finds status.conditions[type==t].
func (o Object) Condition(t string) (status, reason, message string, found bool) {
	for _, c := range o.Slice("status", "conditions") {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m["type"].(string); s == t {
			st, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			return st, r, msg, true
		}
	}
	return "", "", "", false
}

// ConditionTrue reports whether condition t is "True".
func (o Object) ConditionTrue(t string) bool {
	s, _, _, ok := o.Condition(t)
	return ok && strings.EqualFold(s, "True")
}

// Ref is "namespace/name" or "name".
func (o Object) Ref() string {
	if ns := o.Namespace(); ns != "" {
		return ns + "/" + o.Name()
	}
	return o.Name()
}
