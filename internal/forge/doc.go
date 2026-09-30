// Package forge registers a ROKS cluster with BNK Forge v3 over its REST API.
// It is a port of roksbnkctl's internal/forge, and this comment carries the
// behaviour that roksbnkctl's book chapter on BNK Forge registration specifies.
//
// # Registration
//
// Forge needs four calls, in this order:
//
//  1. POST /api/auth/login → {"token": …}; every later call sends it as a
//     bearer token.
//  2. GET/POST/PUT /api/credential-templates: an IBM credential template that
//     holds the IBM Cloud API key, resource group, region and (optionally) COS
//     instance, marked default. Its provider must be lowercase "ibm": Forge
//     compares provider == "ibm" case-sensitively in several places, so "IBM" is
//     stored and then matches nothing. An existing template of the same name is
//     updated, which also repairs one written as "IBM".
//  3. GET/POST /api/projects, then PUT /api/projects/{id} to set the ROKS/IBM
//     platform (without it Forge shows the platform as Unknown).
//  4. POST /api/projects/{id}/k8s/clusters with the cluster id, region, the
//     template id and a base64 kubeconfig, or PUT /api/k8s/clusters/{id} to
//     update an existing registration in place.
//
// Forge connects to the cluster as soon as it is registered, so the kubeconfig
// is required. It must be self-contained and certificate-based (see
// CertKubeconfig): ROKS is OpenShift, whose API server accepts client
// certificates or OpenShift OAuth tokens and rejects IBM IAM bearer tokens.
//
// # Registration is non-destructive (roksbnkctl issue #54)
//
//   - A cluster of that name held by THIS project is updated in place, and its
//     Forge cluster id is kept.
//   - One held by ANOTHER project is refused with ErrClusterOwnedElsewhere,
//     naming the owner, unless forced. A forced takeover is a real move: the
//     cluster is deleted there and created here, so its id changes.
//   - Held by nobody, it is created.
//
// An older Forge with no PUT for clusters falls back to delete-and-create, but
// only on 404 or 405; any other failure is reported, not turned into a
// destructive retry. A project this account cannot read produces a warning, not
// a refusal: the cross-project scan is a second opinion on the direct query of
// the target project. A response body that cannot be parsed is an error, never
// "nobody owns it".
//
// # Unregistering
//
// Unregistering is the inverse and never creates anything. Absence is success:
// no project, no cluster of that name, or a cluster already deleted (404) all
// report and succeed, so a teardown can run it twice or late.
//
// # TLS
//
// Verification uses a pinned CA if one is given, else the system roots, else,
// only with Insecure, nothing. The session token is sent on every request, so an
// insecure client warns once, on its first request, that the connection is
// encrypted but not authenticated, and says so more loudly for a public IP.
package forge
