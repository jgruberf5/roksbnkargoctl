# The COS supply chain (cos)

Every install needs two files from F5: the FAR auth tarball, which holds the service-account
key for the F5 Artifact Registry, and the subscription JWT that licenses BNK. This chapter
describes `roksbnkargoctl cos`, which keeps both in an IBM Cloud Object Storage (COS) bucket
so every workspace and every operator finds them by name, and the local-file alternative.
After reading it you will be able to find your COS instances and buckets, publish the files,
confirm they work, delete them, and point a workspace at them, with or without a workspace.

## The two files

| File | From | Used for | Checked as |
|---|---|---|---|
| FAR auth tarball (`.tgz`) | MyF5 | The FAR password: the first `*.json` entry in the tarball, used as-is with the fixed username `_json_key_base64` | A gzip tarball with a non-empty `.json` entry |
| Subscription JWT | MyF5 | `License.spec.jwt`, via Secret `bnk-license-jwt`; also the FLP's `JWT_TOKEN` | Three dot-separated parts |

Neither file is ever written to the workspace. `install` reads them at render time and
writes their contents only into ROKS Secrets (and, for `flp up`, the FLP VSI's user data).

## Commands

| Command | Does |
|---|---|
| `cos instances` | Lists the account's COS service instances |
| `cos buckets [--instance I]` | Lists an instance's buckets, with each bucket's region |
| `cos list [--instance I] [--bucket B]` | Lists the objects in a bucket |
| `cos publish [--bucket B] --far-auth F --jwt J [--create-bucket] [--region R]` | Uploads the FAR auth tarball and/or the JWT |
| `cos delete [--bucket B] [--object K]…` | Deletes the two F5 objects, or the objects named |
| `cos verify [--bucket B]` | Checks the JWT parses and the FAR key logs in to FAR |

`list`, `publish`, `delete` and `verify` also take `--instance`, `--far-auth-object` and
`--jwt-object`.

**No `cos` command needs a workspace.** With one selected, its `cos.*` settings are the
defaults and the flags override them. Without one (or with `--no-workspace`), the flags,
the `ROKSBNKARGOCTL_COS_*` variables and the defaults apply. That makes publishing the two
files a job for whoever holds them and an API key, before any workspace exists:

```sh
export IBMCLOUD_API_KEY=…
roksbnkargoctl cos instances
roksbnkargoctl cos buckets --instance bnk-supply-chain
roksbnkargoctl cos publish --bucket my-bnk-bucket --far-auth f5-far-auth-key.tgz --jwt subscription.jwt
roksbnkargoctl cos verify --bucket my-bnk-bucket
```

## Configuration

| Key | Flag | Default | Meaning |
|---|---|---|---|
| `cos.instance` | `--instance` | `bnk-supply-chain` (unless `cos.local_far_auth_file` is set) | The COS service instance: its name, GUID or CRN |
| `cos.bucket` | `--bucket` | none | The bucket that holds the two objects: its name, or a bucket CRN |
| `cos.region` | `--region` (`publish` only) | `us-south` | Where a new bucket is created. An existing bucket's region is found, not configured (below) |
| `cos.far_auth_object` | `--far-auth-object` | `f5-far-auth-key.tgz` | Object key of the FAR auth tarball |
| `cos.jwt_object` | `--jwt-object` | `subscription.jwt` | Object key of the JWT |
| `cos.local_far_auth_file` | none here (`--far-auth-file` on `flp up` and `registry`) | empty | Read the FAR tarball from this local file instead of COS |
| `cos.local_jwt_file` | none here (`--jwt-file` on `flp up`) | empty | Read the JWT from this local file instead of COS |

Each key is also overridden by its variable, for example `ROKSBNKARGOCTL_COS_BUCKET`
([Appendix A](./appendix-a-config.md#cos)). `cos.instance` and `cos.bucket` are required
unless `cos.local_far_auth_file` is set. The COS instance is authenticated with the same IBM
Cloud API key as everything else. In a workspace, `init` resolves its CRN into
`resolved.cos_instance_crn`; a `cos.instance` override, or a bucket CRN, makes the command
look the instance up again instead.

### Finding the instance

`--instance` (`cos.instance`) is matched against the account's live COS instances, in every
resource group, by CRN, then GUID, then name. A name that two instances share is an error
that lists both, so you can pass the GUID or CRN instead:

```text
instance name "bnk-supply-chain" is ambiguous: 2 instances share it; pass the GUID or CRN of one:
  bnk-supply-chain  guid …  resource group …  crn:v1:bluemix:public:cloud-object-storage:global:a/…::
  bnk-supply-chain  guid …  resource group …  crn:v1:bluemix:public:cloud-object-storage:global:a/…::
```

A **bucket CRN** in `--bucket`
(`crn:v1:bluemix:public:cloud-object-storage:global:a/<account>:<instance-guid>:bucket:<name>`)
names its instance too, so `--instance` is not needed with one. An `--instance` that names
a different instance than the bucket CRN does is an error rather than a silent choice.

### Finding the bucket's region

A bucket lives in one region, and COS serves it only from that region's endpoint
(`https://s3.<region>.cloud-object-storage.appdomain.cloud`). A request sent to another
region's endpoint does not say the region is wrong: COS answers **404 `NoSuchBucket`**
("The specified bucket does not exist"). A read through the wrong region would then look
like a missing file, and an idempotent delete would report an object as already gone.

So `roksbnkargoctl` does not assume the region. It lists the instance's buckets (any
regional endpoint answers for all of them, with each bucket's location, such as
`us-south-smart` or `eu-de-standard`), takes the region from the location, and addresses
the bucket there. You name a region only to create a bucket.

| Command | When the bucket cannot be located |
|---|---|
| `cos list`, `cos delete` | An error: `COS bucket not found: the instance has no bucket "<name>" (it has: …)` |
| `cos publish` | The same error, unless `--create-bucket` creates the bucket in `--region` |
| `cos verify`, and every read of the two files (`init`, `render`, `install`, `flp up`, `registry`) | Falls back to `cos.region`. A key allowed to read the two objects may not be allowed to list the instance's buckets, and those reads worked before discovery existed. `--verbose` prints why the bucket could not be located |

## cos instances

```sh
roksbnkargoctl cos instances
```

```text
NAME              GUID  RESOURCE GROUP  STATE   CRN
bnk-supply-chain  …     default         active  crn:v1:bluemix:public:cloud-object-storage:global:a/…::
```

Every live COS instance in the account, from the Resource Controller. If the resource
group names cannot be read, the group IDs are shown, with a warning.

## cos buckets

```sh
roksbnkargoctl cos buckets --instance bnk-supply-chain
```

```text
NAME           REGION    LOCATION        CREATED
my-bnk-bucket  us-south  us-south-smart  2026-09-01T12:00:00Z
```

The instance's buckets, sorted by name, with the region each is served from, its location
constraint and its creation time. Here `--instance` wins over a bucket CRN in the
workspace's `cos.bucket`, because the instance is the question.

## cos list

```sh
roksbnkargoctl cos list --bucket my-bnk-bucket
```

```text
  bucket my-bnk-bucket (us-south): 3 object(s)
KEY                     SIZE  MODIFIED              ROLE
f5-far-auth-key.tgz     2291  2026-09-01T12:00:00Z  far-auth
older-subscription.jwt  1839  2026-03-02T09:14:11Z
subscription.jwt        1843  2026-09-01T12:00:04Z  jwt
```

Every object with its size and modification time. The two objects an install uses,
`cos.far_auth_object` and `cos.jwt_object`, are marked `far-auth` and `jwt` in the `ROLE`
column. The first line goes to standard error and the table to standard output.

## cos publish

```sh
roksbnkargoctl cos publish --bucket my-bnk-bucket \
  --far-auth f5-far-auth-key.tgz --jwt subscription.jwt [--create-bucket] [--region eu-de]
```

| Flag | Meaning |
|---|---|
| `--far-auth <file>` | FAR auth tarball (`.tgz` from MyF5); uploaded as `cos.far_auth_object` (`--far-auth-object`) |
| `--jwt <file>` | Subscription JWT file; uploaded as `cos.jwt_object` (`--jwt-object`) |
| `--create-bucket` | Create the bucket if the instance has none of that name (Smart Tier, in `--region`) |
| `--region <region>` | Where `--create-bucket` creates it (`cos.region`, default `us-south`) |

Pass either file or both; with neither, `publish` stops with
`nothing to publish: pass --far-auth and/or --jwt`. Both files are checked before anything
is uploaded (a file that is not a FAR auth tarball, or not a JWT, is refused), and each
replaces any object of the same key.

An existing bucket is used in its own region. If you also passed a different `--region`,
you are told it is ignored:
`bucket my-bnk-bucket already exists in eu-de; --region us-south is ignored`. A missing
bucket is an error unless `--create-bucket` is given:

```text
✓ created bucket my-bnk-bucket in us-south
✓ published f5-far-auth-key.tgz → my-bnk-bucket/f5-far-auth-key.tgz (us-south)
✓ published subscription.jwt → my-bnk-bucket/subscription.jwt (us-south)
```

## cos delete

```sh
roksbnkargoctl cos delete --bucket my-bnk-bucket                    # the two F5 objects
roksbnkargoctl cos delete --bucket my-bnk-bucket --object old.jwt   # these objects (repeatable)
```

Without `--object`, `delete` removes the two F5 objects, `cos.far_auth_object` and
`cos.jwt_object`. It first checks which of them are there (an object that is not there
counts as deleted and is reported as `nothing to delete`), then asks
`delete <keys> from bucket <bucket>? [y/N]`. With no terminal and no `--yes` it refuses:
`not deleted: not confirmed (pass --yes to delete without asking)`. The bucket itself is
kept. Because the bucket's region is found first, an object in a bucket of another region
is never mistaken for an absent one.

## cos verify

```sh
roksbnkargoctl cos verify --bucket my-bnk-bucket
```

1. Reads the JWT and checks it is well-formed: `subscription JWT found and well-formed`.
2. Logs in to FAR with the key from the tarball and reads the BNK manifest chart,
   `<far_host>/release/f5-bigip-k8s-manifest:2.4.0`:
   `FAR key reads BNK 2.4.0 from repo.f5.com`.

`verify` reads from COS or from the local files, whichever is configured
(`ROKSBNKARGOCTL_COS_LOCAL_FAR_AUTH_FILE` and `ROKSBNKARGOCTL_COS_LOCAL_JWT_FILE` work
without a workspace), so in a workspace it tests exactly what `install` will use.

## GA and EA keys

A FAR key issued for an Early Access release does not grant access to the GA artifacts.
FAR answers `403` and `verify` says so:

```text
the FAR key cannot read BNK 2.4.0 from repo.f5.com (an EA key on a GA install answers 403): …
```

roksbnkargoctl installs BNK 2.4.0 GA only. Obtain a GA key from MyF5, publish it, and run
`cos verify` again.

## Local files instead of COS

If you hold the files yourself and do not want a bucket, set both:

```yaml
cos:
  local_far_auth_file: /secure/path/f5-far-auth-key.tgz
  local_jwt_file: /secure/path/subscription.jwt
```

Each local key replaces COS for its own file only. With `local_far_auth_file` set but not
`local_jwt_file`, the JWT is still read from COS, which then needs `cos.instance` and
`cos.bucket`; set both local keys to avoid COS entirely. Keep the files owner-readable only.

## See also

- [Workspaces and init](./05-workspaces-and-init.md)
- [Mirroring into a private registry](./13-registry.md), which also needs the FAR key
- [The F5 License Proxy](./12-flp.md), which needs both files
- [Appendix D. Security model](./appendix-d-security.md)
