package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// configKeyAnnotation marks a flag as an override of a config.yaml key; the
// value is the key's dotted path. openSession finds the flags by it, so a
// command binds flags and calls newSession(cmd) with nothing in between.
const configKeyAnnotation = "roksbnkargoctl/config-key"

// bindConfigFlags registers one flag per config key under section (a dotted
// path such as "flp.vsi"; "" is every key) on cmd's local flags. A flag is
// named prefix + the key's path below section, with "." and "_" as "-":
// flp.vsi.allowed_cidrs under "flp.vsi" is --allowed-cidrs, and under "flp"
// with prefix "flp-" it is --flp-vsi-allowed-cidrs. Keys whose path below the
// section is listed in skip get no flag (bind those under another name with
// bindConfigFlag). It returns the flag names, in key order.
//
// The flags take precedence over the ROKSBNKARGOCTL_* variables and
// config.yaml, and only when set on the command line: an unset flag never
// replaces a configured value with its zero value.
func bindConfigFlags(cmd *cobra.Command, section, prefix string, skip ...string) []string {
	keys := config.KeysUnder(section)
	if len(keys) == 0 {
		panic(fmt.Sprintf("bindConfigFlags: no config keys under %q", section))
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	var names []string
	for _, k := range keys {
		rel := k.Path
		switch {
		case section == "":
		case k.Path == section:
			rel = k.Path[strings.LastIndex(k.Path, ".")+1:]
		default:
			rel = strings.TrimPrefix(k.Path, section+".")
		}
		if skipped[rel] {
			continue
		}
		name := prefix + strings.NewReplacer(".", "-", "_", "-").Replace(rel)
		bindConfigFlag(cmd, name, k.Path)
		names = append(names, name)
	}
	return names
}

// bindConfigFlag registers --name as the override of the config key at path.
// It panics on an unknown path, which the first test that builds the command
// catches.
func bindConfigFlag(cmd *cobra.Command, name, path string) {
	k, ok := config.LookupKey(path)
	if !ok {
		panic(fmt.Sprintf("bindConfigFlag: no config key %q", path))
	}
	help := k.Help
	if help == "" {
		help = k.Path
	}
	help += fmt.Sprintf(" (overrides %s; env %s)", k.Path, k.Env)
	fs := cmd.Flags()
	switch k.Kind {
	case config.KindString:
		fs.String(name, "", help)
	case config.KindInt:
		fs.Int(name, 0, help)
	case config.KindBool:
		fs.Bool(name, false, help)
	case config.KindStringList:
		fs.StringSlice(name, nil, help+", comma-separated")
	default:
		panic(fmt.Sprintf("bindConfigFlag: key %s has unsupported kind %s", path, k.Kind))
	}
	if err := fs.SetAnnotation(name, configKeyAnnotation, []string{path}); err != nil {
		panic(err)
	}
}

// flagOverrides returns an override for every config flag set on the command
// line. Two flags that set the same key are an error rather than a silent pick.
func flagOverrides(cmd *cobra.Command) ([]config.Override, error) {
	fs := cmd.Flags()
	var out []config.Override
	var errs []string
	seen := map[string]string{}
	fs.Visit(func(f *pflag.Flag) {
		paths := f.Annotations[configKeyAnnotation]
		if len(paths) != 1 {
			return
		}
		k, ok := config.LookupKey(paths[0])
		if !ok {
			errs = append(errs, fmt.Sprintf("--%s: no config key %q", f.Name, paths[0]))
			return
		}
		if other, dup := seen[k.Path]; dup {
			errs = append(errs, fmt.Sprintf("--%s and --%s both set %s", other, f.Name, k.Path))
			return
		}
		seen[k.Path] = f.Name
		var v any
		var err error
		switch k.Kind {
		case config.KindString:
			var s string
			s, err = fs.GetString(f.Name)
			v = strings.TrimSpace(s)
		case config.KindInt:
			v, err = fs.GetInt(f.Name)
		case config.KindBool:
			v, err = fs.GetBool(f.Name)
		case config.KindStringList:
			var l []string
			l, err = fs.GetStringSlice(f.Name)
			v = config.SplitList(strings.Join(l, ","))
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("--%s: %v", f.Name, err))
			return
		}
		out = append(out, config.Override{Key: k, Value: v, Source: "--" + f.Name})
	})
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}
