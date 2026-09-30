package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// EnvPrefix starts every configuration override variable.
const EnvPrefix = "ROKSBNKARGOCTL_"

// Kind is the value type of a configuration key, as an override spells it.
type Kind string

// Value kinds. A *bool key (unset means "the default") is KindBool too.
const (
	KindString     Kind = "string"
	KindInt        Kind = "int"
	KindBool       Kind = "bool"
	KindStringList Kind = "[]string"
)

// Key is one leaf setting of config.yaml.
type Key struct {
	// Path is the dotted YAML path, e.g. "flp.vsi.allowed_cidrs".
	Path string
	// Env is the variable that overrides it: EnvPrefix + the path upper-cased
	// with "." as "_" (flp.vsi.allowed_cidrs → EnvPrefix + "FLP_VSI_ALLOWED_CIDRS").
	Env  string
	Kind Kind
	// Help is the one-line description from the field's `help` tag.
	Help string

	index []int // reflect field index path from Config
}

// Override is one value that takes precedence over config.yaml for a single
// command run. It is never written back to config.yaml.
type Override struct {
	Key Key
	// Value is a string, int, bool or []string, matching Key.Kind.
	Value any
	// Source names where the value came from, for messages: the environment
	// variable or the flag ("--cidr").
	Source string
}

var (
	keysOnce sync.Once
	keys     []Key
	keyByP   map[string]Key
)

// Keys lists every overridable setting in config.yaml, in declaration order,
// derived from the yaml tags of Config: a new field gets an environment
// variable and a bindable flag without any other change. `resolved` is machine
// state and is not a setting; a field tagged `override:"-"` is left out.
func Keys() []Key {
	keysOnce.Do(func() {
		keys = walkKeys(reflect.TypeOf(Config{}), "", nil)
		keyByP = make(map[string]Key, len(keys))
		for _, k := range keys {
			keyByP[k.Path] = k
		}
	})
	return append([]Key(nil), keys...)
}

// EnvOverrides is Keys, named for what the documentation lists: every
// ROKSBNKARGOCTL_* variable that overrides a config.yaml key.
func EnvOverrides() []Key { return Keys() }

// LookupKey returns the key at a dotted path.
func LookupKey(path string) (Key, bool) {
	Keys()
	k, ok := keyByP[path]
	return k, ok
}

// KeysUnder returns the keys at or below the dotted section ("" is every key).
func KeysUnder(section string) []Key {
	var out []Key
	for _, k := range Keys() {
		if section == "" || k.Path == section || strings.HasPrefix(k.Path, section+".") {
			out = append(out, k)
		}
	}
	return out
}

func walkKeys(t reflect.Type, prefix string, index []int) []Key {
	var out []Key
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" || name == "resolved" || f.Tag.Get("override") == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		idx := append(append([]int(nil), index...), i)
		if f.Type.Kind() == reflect.Struct {
			out = append(out, walkKeys(f.Type, path, idx)...)
			continue
		}
		k := Key{Path: path, Env: EnvName(path), Help: f.Tag.Get("help"), index: idx}
		switch {
		case f.Type.Kind() == reflect.String:
			k.Kind = KindString
		case f.Type.Kind() == reflect.Int:
			k.Kind = KindInt
		case f.Type.Kind() == reflect.Bool:
			k.Kind = KindBool
		case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Bool:
			k.Kind = KindBool
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String:
			k.Kind = KindStringList
		default:
			// A programming error, caught by the first test that lists the keys.
			panic(fmt.Sprintf("config key %s: unsupported type %s", path, f.Type))
		}
		out = append(out, k)
	}
	return out
}

// EnvName is the override variable for a dotted key path.
func EnvName(path string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// ParseValue converts the text of an override into the key's value type.
// Lists are comma-separated; entries are trimmed and empty ones dropped.
func ParseValue(k Key, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch k.Kind {
	case KindString:
		return raw, nil
	case KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", raw)
		}
		return n, nil
	case KindBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean (use true or false)", raw)
		}
		return b, nil
	case KindStringList:
		return SplitList(raw), nil
	}
	return nil, fmt.Errorf("key %s: unknown kind %s", k.Path, k.Kind)
}

// SplitList splits a comma-separated list, trimming entries and dropping empty ones.
func SplitList(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// FromEnv reads every override variable that is set to a non-empty value.
// (An empty variable is treated as unset, as the tool's other variables are.)
// A value that does not parse is an error naming the variable; all such
// errors are reported together.
func FromEnv(lookup func(string) (string, bool)) ([]Override, error) {
	var out []Override
	var errs []error
	for _, k := range Keys() {
		raw, ok := lookup(k.Env)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		v, err := ParseValue(k, raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s (overrides %s): %w", k.Env, k.Path, err))
			continue
		}
		out = append(out, Override{Key: k, Value: v, Source: k.Env})
	}
	return out, errors.Join(errs...)
}

// Apply writes the overrides into c, in order (a later one wins).
func Apply(c *Config, ovs []Override) error {
	for _, o := range ovs {
		if err := c.set(o.Key, o.Value); err != nil {
			return fmt.Errorf("%s: %w", o.Source, err)
		}
	}
	return nil
}

func (c *Config) field(k Key) reflect.Value {
	if len(k.index) == 0 {
		// A Key built by hand: find it by path.
		if kk, ok := LookupKey(k.Path); ok {
			k = kk
		}
	}
	return reflect.ValueOf(c).Elem().FieldByIndex(k.index)
}

func (c *Config) set(k Key, v any) error {
	f := c.field(k)
	switch x := v.(type) {
	case string:
		if f.Kind() != reflect.String {
			return fmt.Errorf("key %s is %s, not a string", k.Path, k.Kind)
		}
		f.SetString(x)
	case int:
		if f.Kind() != reflect.Int {
			return fmt.Errorf("key %s is %s, not an int", k.Path, k.Kind)
		}
		f.SetInt(int64(x))
	case bool:
		switch {
		case f.Kind() == reflect.Bool:
			f.SetBool(x)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Bool:
			b := x
			f.Set(reflect.ValueOf(&b))
		default:
			return fmt.Errorf("key %s is %s, not a bool", k.Path, k.Kind)
		}
	case []string:
		if f.Kind() != reflect.Slice {
			return fmt.Errorf("key %s is %s, not a list", k.Path, k.Kind)
		}
		f.Set(reflect.ValueOf(append([]string(nil), x...)))
	default:
		return fmt.Errorf("key %s: unsupported value %T", k.Path, v)
	}
	return nil
}

// Get returns the value at a dotted key path, in the form Override.Value
// uses; an unset *bool is nil.
func (c *Config) Get(path string) (any, error) {
	k, ok := LookupKey(path)
	if !ok {
		return nil, fmt.Errorf("no config key %q", path)
	}
	f := c.field(k)
	if f.Kind() == reflect.Pointer {
		if f.IsNil() {
			return nil, nil
		}
		return f.Elem().Bool(), nil
	}
	if f.Kind() == reflect.Slice {
		return append([]string(nil), f.Interface().([]string)...), nil
	}
	return f.Interface(), nil
}

// DiffKeys returns the paths of the settings whose values differ between a and
// b (Resolved is not compared). A nil and an empty list are equal.
func DiffKeys(a, b *Config) []string {
	var out []string
	for _, k := range Keys() {
		va, _ := a.Get(k.Path)
		vb, _ := b.Get(k.Path)
		if !SameValue(va, vb) {
			out = append(out, k.Path)
		}
	}
	sort.Strings(out)
	return out
}

// SameValue compares two values as Get returns them; a nil and an empty list
// are the same (config.yaml cannot tell them apart).
func SameValue(a, b any) bool {
	if l, ok := a.([]string); ok && len(l) == 0 {
		a = []string(nil)
	}
	if l, ok := b.([]string); ok && len(l) == 0 {
		b = []string(nil)
	}
	return reflect.DeepEqual(a, b)
}

// Clone returns a deep copy of c (Resolved included), so an override applied
// to the copy never reaches the original's slices or pointers.
func (c *Config) Clone() *Config {
	out := &Config{}
	deepCopy(reflect.ValueOf(out).Elem(), reflect.ValueOf(c).Elem())
	return out
}

func deepCopy(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			deepCopy(dst.Field(i), src.Field(i))
		}
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		p := reflect.New(src.Type().Elem())
		deepCopy(p.Elem(), src.Elem())
		dst.Set(p)
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		s := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := 0; i < src.Len(); i++ {
			deepCopy(s.Index(i), src.Index(i))
		}
		dst.Set(s)
	case reflect.Map:
		if src.IsNil() {
			return
		}
		m := reflect.MakeMapWithSize(src.Type(), src.Len())
		for _, key := range src.MapKeys() {
			v := reflect.New(src.Type().Elem()).Elem()
			deepCopy(v, src.MapIndex(key))
			m.SetMapIndex(key, v)
		}
		dst.Set(m)
	default:
		dst.Set(src)
	}
}
