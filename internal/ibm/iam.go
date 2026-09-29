package ibm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// IAMToken returns an IAM bearer access token for the client's API key. The
// shared authenticator caches it and refreshes it near expiry, so repeated calls
// are cheap. The token lives about an hour.
func (c *Client) IAMToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.token()
}

// AccountID returns the IBM Cloud account the API key belongs to, from IAM's
// API-key details endpoint. Cached after the first successful call (copies made
// with WithRegion share the cache).
func (c *Client) AccountID(ctx context.Context) (string, error) {
	if c.acct == nil {
		c.acct = &accountCache{}
	}
	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()
	if c.acct.id != "" {
		return c.acct.id, nil
	}
	var out struct {
		AccountID string `json:"account_id"`
		IAMID     string `json:"iam_id"`
	}
	err := c.do(ctx, http.MethodGet, c.iamURL+"/v1/apikeys/details",
		map[string]string{"IAM-ApiKey": c.apiKey}, nil, &out)
	if err != nil {
		return "", fmt.Errorf("reading API key details: %w", friendlyAuthErr(err))
	}
	if out.AccountID == "" {
		return "", errors.New("IAM returned no account_id for this API key")
	}
	c.acct.id = out.AccountID
	return out.AccountID, nil
}

// ErrIAMPermDenied marks an IAM call refused for lack of permission (HTTP 403).
var ErrIAMPermDenied = errors.New("IAM permission denied")

// friendlyAuthErr rewrites IAM's opaque bad-key error into something an
// operator can act on, and tags 403s with ErrIAMPermDenied.
func friendlyAuthErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "BXNIM0415E") || strings.Contains(msg, "Provided API key could not be found") {
		return fmt.Errorf("API key is invalid or revoked; check IBMCLOUD_API_KEY: %w", err)
	}
	if statusOf(err) == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrIAMPermDenied, err)
	}
	return err
}
