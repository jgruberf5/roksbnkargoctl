package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/cos"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// ---- cos ------------------------------------------------------------------------
//
// Every cos command runs with or without a workspace: with one, its cos.*
// settings are the defaults; flags (and ROKSBNKARGOCTL_COS_* variables)
// override them. The instance is found by name, GUID or CRN, and a bucket's
// region from the bucket's location, so the operator never names a region
// except to create a bucket.

func newCOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cos",
		Short: "Publish and discover the FAR auth tarball and subscription JWT in COS",
		Long: `The FAR auth tarball (from MyF5) and the subscription JWT are the two supply-chain
files every install needs. Keeping them in IBM Cloud Object Storage lets every
workspace (and every operator) find them by name instead of passing files around.

No cos command needs a workspace. With one selected, its cos.* settings are the
defaults and flags override them; without one (or with --no-workspace) the flags,
the ROKSBNKARGOCTL_COS_* variables and the defaults (instance bnk-supply-chain)
apply. The IBM Cloud API key comes from IBMCLOUD_API_KEY.

--instance takes a name, GUID or CRN; --bucket a name or a bucket CRN (which
also names the instance). A bucket's region is found from its location.

  roksbnkargoctl cos instances                  the account's COS instances
  roksbnkargoctl cos buckets --instance bnk-supply-chain
  roksbnkargoctl cos list --bucket my-bucket    what the bucket holds
  roksbnkargoctl cos publish --bucket my-bucket --far-auth f5-far-auth-key.tgz --jwt subscription.jwt
  roksbnkargoctl cos delete --bucket my-bucket  the two F5 objects (asks first)
  roksbnkargoctl cos verify --bucket my-bucket  the FAR key logs in, the JWT parses`,
	}
	cmd.AddCommand(newCOSInstancesCmd(), newCOSBucketsCmd(), newCOSListCmd(), newCOSPublishCmd(), newCOSDeleteCmd(), newCOSVerifyCmd())
	return cmd
}

// bindCOSTarget registers --instance and --bucket.
func bindCOSTarget(cmd *cobra.Command) {
	bindConfigFlag(cmd, "instance", "cos.instance")
	bindConfigFlag(cmd, "bucket", "cos.bucket")
}

// bindCOSObjects registers --far-auth-object and --jwt-object.
func bindCOSObjects(cmd *cobra.Command) {
	bindConfigFlag(cmd, "far-auth-object", "cos.far_auth_object")
	bindConfigFlag(cmd, "jwt-object", "cos.jwt_object")
}

func table(cmd *cobra.Command) *tabwriter.Writer {
	return tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
}

func newCOSInstancesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "instances",
		Short: "List the account's COS service instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			l, err := s.ibmLookup()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			all, err := l.ListServiceInstances(ctx, cosService)
			if err != nil {
				return err
			}
			if len(all) == 0 {
				s.p.info("no COS instances in this account")
				return nil
			}
			groups := map[string]string{}
			if rgs, err := l.ListResourceGroups(ctx); err != nil {
				s.p.warn("resource group names unavailable (%v); showing ids", err)
			} else {
				for _, g := range rgs {
					groups[g.ID] = g.Name
				}
			}
			w := table(cmd)
			fmt.Fprintln(w, "NAME\tGUID\tRESOURCE GROUP\tSTATE\tCRN")
			for _, si := range all {
				rg := groups[si.ResourceGroupID]
				if rg == "" {
					rg = si.ResourceGroupID
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", si.Name, si.GUID, rg, si.State, si.CRN)
			}
			return w.Flush()
		},
	}
}

func newCOSBucketsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buckets",
		Short: "List a COS instance's buckets with their regions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			// The instance is the question here; a bucket CRN in the
			// workspace must not answer it over --instance.
			s.ignoreBucketCRN = s.overridden("cos.instance")
			cc, err := s.COS(cmd.Context())
			if err != nil {
				return err
			}
			bs, err := cc.ListBucketsExtended(cmd.Context())
			if err != nil {
				return err
			}
			if len(bs) == 0 {
				s.p.info("the instance has no buckets")
				return nil
			}
			w := table(cmd)
			fmt.Fprintln(w, "NAME\tREGION\tLOCATION\tCREATED")
			for _, b := range bs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.Name, b.Region, b.LocationConstraint, b.Created.UTC().Format(time.RFC3339))
			}
			return w.Flush()
		},
	}
	bindConfigFlag(cmd, "instance", "cos.instance")
	return cmd
}

func newCOSListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the objects in a bucket",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			cc, bucket, err := s.cosBucket(cmd.Context(), true)
			if err != nil {
				return err
			}
			objs, err := cc.ListObjects(cmd.Context(), bucket, "")
			if err != nil {
				return err
			}
			s.p.info("bucket %s (%s): %d object(s)", bucket, cc.Region(), len(objs))
			if len(objs) == 0 {
				return nil
			}
			w := table(cmd)
			fmt.Fprintln(w, "KEY\tSIZE\tMODIFIED\tROLE")
			for _, o := range objs {
				role := ""
				switch o.Key {
				case s.cfg.COS.FARAuthObject:
					role = "far-auth"
				case s.cfg.COS.JWTObject:
					role = "jwt"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", o.Key, o.Size, o.Modified.UTC().Format(time.RFC3339), role)
			}
			return w.Flush()
		},
	}
	bindCOSTarget(cmd)
	bindCOSObjects(cmd)
	return cmd
}

func newCOSPublishCmd() *cobra.Command {
	var farFile, jwtFile string
	var createBucket bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Upload the FAR auth tarball and/or JWT to a bucket",
		Long: `publish uploads the FAR auth tarball as cos.far_auth_object (--far-auth-object,
default f5-far-auth-key.tgz) and the subscription JWT as cos.jwt_object
(--jwt-object, default subscription.jwt), replacing what is there. Both files are
checked before anything is uploaded.

An existing bucket is used in its own region. A missing one is an error unless
--create-bucket is given, which creates it (Smart Tier) in --region (cos.region,
default us-south).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			if farFile == "" && jwtFile == "" {
				return errors.New("nothing to publish: pass --far-auth and/or --jwt")
			}
			type upload struct {
				file, key string
				data      []byte
			}
			var ups []upload
			if farFile != "" {
				b, err := os.ReadFile(farFile)
				if err != nil {
					return err
				}
				if _, err := far.ServiceAccountFromTarball(b); err != nil {
					return fmt.Errorf("%s is not a FAR auth tarball: %w", farFile, err)
				}
				ups = append(ups, upload{farFile, s.cfg.COS.FARAuthObject, b})
			}
			if jwtFile != "" {
				b, err := os.ReadFile(jwtFile)
				if err != nil {
					return err
				}
				if strings.Count(strings.TrimSpace(string(b)), ".") != 2 {
					return fmt.Errorf("%s is not a JWT (expected three dot-separated parts)", jwtFile)
				}
				ups = append(ups, upload{jwtFile, s.cfg.COS.JWTObject, b})
			}

			ctx := cmd.Context()
			bucket := s.cosBucketName()
			if bucket == "" {
				return errors.New("cos.bucket is not set: pass --bucket")
			}
			cc, err := s.COS(ctx)
			if err != nil {
				return err
			}
			b, err := cc.FindBucket(ctx, bucket)
			switch {
			case err == nil:
				if cmd.Flags().Changed("region") && b.Region != cc.Region() {
					s.p.warn("bucket %s already exists in %s; --region %s is ignored", bucket, b.Region, cc.Region())
				}
				if cc, err = cc.InRegion(b.Region); err != nil {
					return err
				}
			case errors.Is(err, cos.ErrBucketNotFound) && createBucket:
				created, err := cc.EnsureBucket(ctx, bucket)
				if err != nil {
					return err
				}
				if created {
					s.p.ok("created bucket %s in %s", bucket, cc.Region())
				}
			case errors.Is(err, cos.ErrBucketNotFound):
				return fmt.Errorf("%w; pass --create-bucket (and --region) to create it", err)
			default:
				return err
			}
			for _, u := range ups {
				if err := cc.PutObject(ctx, bucket, u.key, u.data); err != nil {
					return err
				}
				s.p.ok("published %s → %s/%s (%s)", u.file, bucket, u.key, cc.Region())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&farFile, "far-auth", "", "FAR auth tarball (.tgz from MyF5)")
	cmd.Flags().StringVar(&jwtFile, "jwt", "", "subscription JWT file")
	cmd.Flags().BoolVar(&createBucket, "create-bucket", false, "create the bucket (in --region) if the instance has no bucket of that name")
	bindCOSTarget(cmd)
	bindCOSObjects(cmd)
	bindConfigFlag(cmd, "region", "cos.region")
	return cmd
}

func newCOSDeleteCmd() *cobra.Command {
	var objects []string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete objects from a bucket (default: the FAR auth tarball and the JWT)",
		Long: `delete removes --object keys (repeatable) from the bucket; without --object it
removes the two F5 objects, cos.far_auth_object and cos.jwt_object. It asks first
unless --yes is given. An object that is not there counts as deleted. The bucket
itself is kept.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			keys := objects
			if len(keys) == 0 {
				keys = []string{s.cfg.COS.FARAuthObject, s.cfg.COS.JWTObject}
			}
			keys = uniqueNonEmpty(keys)
			if len(keys) == 0 {
				return errors.New("no object keys to delete")
			}
			ctx := cmd.Context()
			cc, bucket, err := s.cosBucket(ctx, true)
			if err != nil {
				return err
			}
			var present []string
			for _, k := range keys {
				ok, err := cc.Exists(ctx, bucket, k)
				if err != nil {
					return err
				}
				if ok {
					present = append(present, k)
				} else {
					s.p.info("%s/%s is not there; nothing to delete", bucket, k)
				}
			}
			if len(present) == 0 {
				s.p.ok("nothing to delete in %s", bucket)
				return nil
			}
			if !confirm(cmd, fmt.Sprintf("delete %s from bucket %s?", strings.Join(present, ", "), bucket)) {
				return errors.New("not deleted: not confirmed (pass --yes to delete without asking)")
			}
			for _, k := range present {
				if err := cc.DeleteObject(ctx, bucket, k); err != nil {
					return err
				}
				s.p.ok("deleted %s/%s", bucket, k)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&objects, "object", nil, "object key to delete (repeatable; default the FAR auth and JWT objects)")
	bindCOSTarget(cmd)
	bindCOSObjects(cmd)
	return cmd
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func newCOSVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check the FAR key logs in to FAR and the JWT parses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if _, err := s.JWT(ctx); err != nil {
				return err
			}
			s.p.ok("subscription JWT found and well-formed")
			pl, err := s.Puller(ctx, true)
			if err != nil {
				return err
			}
			if _, err := pl.Digest(ctx, far.ManifestChartRef(s.cfg.Registry.FARHost, s.cfg.BNK.Version)); err != nil {
				return fmt.Errorf("the FAR key cannot read BNK %s from %s (an EA key on a GA install answers 403): %w", s.cfg.BNK.Version, s.cfg.Registry.FARHost, err)
			}
			s.p.ok("FAR key reads BNK %s from %s", s.cfg.BNK.Version, s.cfg.Registry.FARHost)
			return nil
		},
	}
	bindCOSTarget(cmd)
	bindCOSObjects(cmd)
	return cmd
}
