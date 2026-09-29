// Package registry replicates everything a BNK 2.4 install pulls into a private
// registry, so a cluster without internet egress can install from it.
//
// Layout rule, shared with the renderer: an artifact's mirror path is its source
// path with the host removed, under <mirror-host>/<prefix>/:
//
//	repo.f5.com/images/f5-tmm           → <mirror>/images/f5-tmm
//	quay.io/jetstack/cert-manager-controller → <mirror>/jetstack/cert-manager-controller
//	ghcr.io/jgruberf5/roksbnkargoctl-check → <mirror>/jgruberf5/roksbnkargoctl-check
package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// Artifact is one thing to copy.
type Artifact struct {
	Source string // host/path:tag
	Kind   string // chart | image
}

// Path is the source reference without its host.
func (a Artifact) Path() string {
	s := a.Source
	if i := strings.Index(s, "/"); i > 0 {
		return s[i+1:]
	}
	return s
}

// Dest is where the artifact lives in the mirror.
func (a Artifact) Dest(mirror string) string {
	return strings.TrimSuffix(mirror, "/") + "/" + a.Path()
}

// BOM lists every artifact an install can pull: the BNK manifest's charts and
// images (FAR), the manifest chart itself, cert-manager's chart and component
// images, and the check image.
func BOM(m *far.Manifest, farHost, certManagerVersion, checkImage string, includeCertManager bool) []Artifact {
	var out []Artifact
	out = append(out, Artifact{Source: far.ManifestChartRef(farHost, m.Version), Kind: "chart"})
	for _, c := range m.Charts {
		out = append(out, Artifact{Source: fmt.Sprintf("%s/%s:%s", farHost, c.Name, ociTag(c.Version)), Kind: "chart"})
	}
	for _, i := range m.Images {
		out = append(out, Artifact{Source: fmt.Sprintf("%s/%s:%s", farHost, i.Name, ociTag(i.Version)), Kind: "image"})
	}
	if includeCertManager {
		out = append(out, Artifact{Source: "quay.io/jetstack/charts/cert-manager:" + certManagerVersion, Kind: "chart"})
		for _, comp := range []string{"controller", "webhook", "cainjector", "acmesolver"} {
			out = append(out, Artifact{Source: "quay.io/jetstack/cert-manager-" + comp + ":" + certManagerVersion, Kind: "image"})
		}
	}
	if checkImage != "" {
		out = append(out, Artifact{Source: checkImage, Kind: "image"})
	}
	return out
}

// ociTag maps a version to the OCI tag it is stored under. "+" is illegal in an
// OCI tag, so Helm stores a chart version like 14.91.12+0.4.7 as 14.91.12_0.4.7
// (the manifest lists utils/log-doc-f5ingress that way; found replicating 2.4 GA).
func ociTag(v string) string { return strings.ReplaceAll(v, "+", "_") }

// Result is one artifact's outcome.
type Result struct {
	Artifact Artifact
	Digest   string
	Skipped  bool // already present with the same digest
	Err      error
}

// Replicate copies the BOM into the mirror with bounded concurrency. Images are
// narrowed to linux/amd64 (ROKS workers); charts and single-arch artifacts copy
// as they are. An artifact already present with the source digest is skipped.
func Replicate(ctx context.Context, p *far.Puller, mirror string, bom []Artifact, concurrency int, progress func(Result)) []Result {
	if concurrency < 1 {
		concurrency = 2
	}
	results := make([]Result, len(bom))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	amd64 := &v1.Platform{OS: "linux", Architecture: "amd64"}
	for i, a := range bom {
		wg.Add(1)
		go func(i int, a Artifact) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := Result{Artifact: a}
			opts := p.Options(ctx)
			if a.Kind == "image" {
				opts = append(opts, crane.WithPlatform(amd64))
			}
			var src string
			for attempt := 0; attempt < 4; attempt++ {
				src, r.Err = crane.Digest(a.Source, opts...)
				if r.Err == nil || !transient(r.Err) {
					break
				}
			}
			if r.Err == nil {
				if dst, err := crane.Digest(a.Dest(mirror), opts...); err == nil && dst == src {
					r.Digest, r.Skipped = src, true
				} else {
					for attempt := 0; attempt < 4; attempt++ {
						r.Err = crane.Copy(a.Source, a.Dest(mirror), opts...)
						if r.Err == nil || !transient(r.Err) {
							break
						}
					}
					r.Digest = src
				}
			}
			results[i] = r
			if progress != nil {
				progress(r)
			}
		}(i, a)
	}
	wg.Wait()
	return results
}

// Verify reports artifacts missing from the mirror.
func Verify(ctx context.Context, p *far.Puller, mirror string, bom []Artifact) (missing []Artifact, err error) {
	var errs []error
	for _, a := range bom {
		if _, e := crane.Digest(a.Dest(mirror), p.Options(ctx)...); e != nil {
			if strings.Contains(e.Error(), "MANIFEST_UNKNOWN") || strings.Contains(e.Error(), "NAME_UNKNOWN") || strings.Contains(e.Error(), "404") {
				missing = append(missing, a)
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", a.Dest(mirror), e))
		}
	}
	return missing, errors.Join(errs...)
}

func transient(err error) bool {
	s := err.Error()
	for _, t := range []string{"connection reset", "i/o timeout", "TLS handshake timeout", "EOF", "502", "503", "504", "TOOMANYREQUESTS"} {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
