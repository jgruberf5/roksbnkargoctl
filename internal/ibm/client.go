package ibm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/IBM/go-sdk-core/v5/core"
)

// Default production endpoints. Overridden only by tests, via the unexported
// Client fields.
const (
	defaultIAMURL        = "https://iam.cloud.ibm.com"
	defaultRCURL         = "https://resource-controller.cloud.ibm.com"
	defaultContainersURL = "https://containers.cloud.ibm.com/global"
	defaultTransitURL    = "https://transit.cloud.ibm.com/v1"
)

// vpcAPIVersion is the VPC API's date version. 2025-04-08 is past the
// 2024-04-30 date from which instances get a virtual network interface by
// default, and before 2025-06-30 (images: owner_type -> remote.account) and
// 2025-12-09 (security-group protocol "all" -> "icmp_tcp_udp"), both of which
// would change request or response shapes this package relies on.
const vpcAPIVersion = "2025-04-08"

// tgwAPIVersion is the Transit Gateway API's date version — the value
// roksbnkctl has run against in production.
const tgwAPIVersion = "2024-04-30"

// tokenSource is what the REST helpers need from an authenticator.
// *core.IamAuthenticator satisfies it (and caches + refreshes the token);
// tests substitute a static token.
type tokenSource interface {
	GetToken() (string, error)
}

// Client talks to the IBM Cloud APIs with one API key. Region scopes the VPC
// calls only; IAM, Resource Controller, containers and Transit Gateway are
// global. Use WithRegion for a copy bound to another region.
//
// A Client is safe to share between goroutines for reads; the cached account
// ID is guarded.
type Client struct {
	apiKey string
	region string

	auth tokenSource
	http *http.Client

	iamURL        string
	rcURL         string
	containersURL string
	transitURL    string
	// vpcURL, when set, replaces https://<region>.iaas.cloud.ibm.com/v1 for
	// every region (tests).
	vpcURL string

	// pollInterval is the wait between polls in the Wait* helpers.
	pollInterval time.Duration
	// kubeconfig retry tuning (tests shorten the wait).
	kubeconfigAttempts  int
	kubeconfigRetryWait time.Duration

	acct *accountCache
}

type accountCache struct {
	mu sync.Mutex
	id string
}

// New constructs a Client. It does not contact IBM Cloud: the API key is first
// exercised by the first call that needs a token.
func New(apiKey, region string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("IBM Cloud API key is empty (set IBMCLOUD_API_KEY)")
	}
	auth := &core.IamAuthenticator{ApiKey: apiKey}
	if err := auth.Validate(); err != nil {
		return nil, fmt.Errorf("IBM Cloud API key: %w", err)
	}
	return &Client{
		apiKey:              apiKey,
		region:              region,
		auth:                auth,
		http:                &http.Client{Timeout: 60 * time.Second},
		iamURL:              defaultIAMURL,
		rcURL:               defaultRCURL,
		containersURL:       defaultContainersURL,
		transitURL:          defaultTransitURL,
		pollInterval:        5 * time.Second,
		kubeconfigAttempts:  12,
		kubeconfigRetryWait: 15 * time.Second,
		acct:                &accountCache{},
	}, nil
}

// WithRegion returns a copy of c bound to region. The copy shares the IAM
// authenticator and the cached account ID.
func (c *Client) WithRegion(region string) *Client {
	cp := *c
	cp.region = region
	return &cp
}

// Region is the region the VPC calls are bound to.
func (c *Client) Region() string { return c.region }

// APIKey returns the API key, for SDKs that authenticate on their own (COS).
func (c *Client) APIKey() string { return c.apiKey }

// vpcBase returns the regional VPC API base URL (ending in /v1).
func (c *Client) vpcBase() (string, error) {
	if c.vpcURL != "" {
		return c.vpcURL, nil
	}
	if c.region == "" {
		return "", errors.New("no region set for a VPC call (use WithRegion)")
	}
	return fmt.Sprintf("https://%s.iaas.cloud.ibm.com/v1", c.region), nil
}

// vpcURLf formats a VPC API URL: base + path, with version and generation
// appended. path may already carry a query string.
func (c *Client) vpcURLf(format string, a ...any) (string, error) {
	base, err := c.vpcBase()
	if err != nil {
		return "", err
	}
	p := fmt.Sprintf(format, a...)
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	return base + p + sep + "version=" + vpcAPIVersion + "&generation=2", nil
}

// tgwURLf formats a Transit Gateway API URL with the version parameter.
func (c *Client) tgwURLf(format string, a ...any) string {
	p := fmt.Sprintf(format, a...)
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	return c.transitURL + p + sep + "version=" + tgwAPIVersion
}
