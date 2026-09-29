package checks

import (
	"context"
	"fmt"
	"time"
)

// OpenShift's admission policy that refuses writes to Gateway API CRDs from
// anything but its ingress operator. FLO's crd-installer installs Gateway API
// CRDs, so without removing this the install stalls. The ingress operator
// re-creates it, hence the loop.
const GatewayAPIVAPName = "openshift-ingress-operator-gatewayapi-crd-admission"

// The CRDs whose existence means the crd-installer is past the policy.
var GatewaySweepCRDs = []string{
	"gateways.gateway.networking.k8s.io",
	"gatewaysettings.gateway.k8s.f5.com",
}

// GatewaySweepConfig configures gateway-api-sweep.
type GatewaySweepConfig struct {
	Interval time.Duration // 5s
	Timeout  time.Duration // 20m
	// Idle sleeps forever after success. It runs as a Deployment, not a hook:
	// the CRDs it waits for are installed by the NEXT sync wave, so a blocking
	// hook would deadlock the sync; and an exiting container in a Deployment
	// CrashLoops.
	Idle bool
}

// GatewayAPISweep deletes the VAP and its binding every Interval until both
// CRDs exist.
func GatewayAPISweep(ctx context.Context, env *Env, cfg GatewaySweepConfig, res *Result) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Minute
	}
	deadline := time.Now().Add(cfg.Timeout)
	deletions := 0
	var lastErr error
	for {
		present, missing, err := crdsExist(ctx, env, GatewaySweepCRDs)
		if err != nil {
			lastErr = err
			env.logf("checking CRDs: %v", err)
		} else if len(missing) == 0 {
			res.Pass("gateway-api-crds", "%v exist; removed the admission policy/binding %d time(s)", present, deletions)
			res.Set("deletions", deletions)
			return nil
		}
		for _, p := range []string{GVRVAPBinding.Path("", GatewayAPIVAPName), GVRVAP.Path("", GatewayAPIVAPName)} {
			// Binding first: without it the policy is inert, so the window in which
			// a half-removed pair still blocks is as short as possible.
			did, err := env.Kube.DeleteIfExists(ctx, p, "")
			if err != nil {
				lastErr = err
				env.logf("deleting %s: %v", p, err)
				continue
			}
			if did {
				deletions++
				env.logf("deleted %s (waiting for %v)", p, missing)
			}
		}
		if !time.Now().Before(deadline) {
			res.Fail("gateway-api-crds", "still missing %v after %s (last error: %v); is FLO installed and its crd-installer running?", missing, cfg.Timeout, lastErr)
			return nil
		}
		if err := sleep(ctx, cfg.Interval); err != nil {
			return err
		}
	}
}

func crdsExist(ctx context.Context, env *Env, names []string) (present, missing []string, err error) {
	for _, n := range names {
		ok, e := env.Kube.Exists(ctx, GVRCRD.Path("", n))
		if e != nil {
			return nil, nil, fmt.Errorf("reading CRD %s: %w", n, e)
		}
		if ok {
			present = append(present, n)
		} else {
			missing = append(missing, n)
		}
	}
	return present, missing, nil
}
