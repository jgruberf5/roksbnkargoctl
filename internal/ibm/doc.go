// Package ibm is roksbnkargoctl's in-process client for the IBM Cloud APIs it
// needs. There is no Terraform and no ibmcloud CLI behind it: every call is a
// plain REST request authenticated with a bearer token minted from the
// operator's API key, so the package builds and runs natively on Windows.
//
// Services and base URLs:
//
//   - IAM (token, API-key details, trusted profiles, access policies)
//     https://iam.cloud.ibm.com
//   - Resource Controller (resource groups, service instances such as COS)
//     https://resource-controller.cloud.ibm.com
//   - Kubernetes Service (ROKS cluster metadata, admin kubeconfig)
//     https://containers.cloud.ibm.com/global
//   - VPC (regional; reads and the create/delete calls the FLP and test Argo CD
//     VSIs need) https://<region>.iaas.cloud.ibm.com/v1
//   - Transit Gateway (global) https://transit.cloud.ibm.com/v1
//
// Every base URL is an unexported field on Client so tests can point the client
// at an httptest server; production code never changes them.
//
// Deletes treat HTTP 404 as success, so teardown is idempotent. Create calls are
// not idempotent on their own; callers look an object up by name first where a
// re-run must not duplicate it (FindTrustedProfileByName, FindPublicGateway,
// GetSSHKeyByName, FindSecurityGroupByName, FindConnectionForVPC).
//
// A Client is cheap to copy with WithRegion; copies share the IAM authenticator,
// so the bearer token is exchanged once per process, not once per call.
//
// Typical use:
//
//	c, err := ibm.New(os.Getenv("IBMCLOUD_API_KEY"), "us-south")
//	cl, err := c.GetCluster(ctx, "my-roks")
//	kc, err := c.FetchAdminKubeconfig(ctx, cl.ID, true) // private endpoint
package ibm
