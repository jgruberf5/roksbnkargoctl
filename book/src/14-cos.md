# The COS supply chain (cos)

Every install needs two files from F5: the FAR auth tarball, which holds the service-account
key for the F5 Artifact Registry, and the subscription JWT that licenses BNK. This chapter
describes `roksbnkargoctl cos`, which keeps both in an IBM Cloud Object Storage (COS) bucket
so every workspace and every operator finds them by name, and the local-file alternative.
After reading it you will be able to publish the files, confirm they work, and point a
workspace at them.

## The two files

| File | From | Used for | Checked as |
|---|---|---|---|
| FAR auth tarball (`.tgz`) | MyF5 | The FAR password: the first `*.json` entry in the tarball, used as-is with the fixed username `_json_key_base64` | A gzip tarball with a non-empty `.json` entry |
| Subscription JWT | MyF5 | `License.spec.jwt`, via Secret `bnk-license-jwt`; also the FLP's `JWT_TOKEN` | Three dot-separated parts |

Neither file is ever written to the workspace. `install` reads them at render time and
writes their contents only into ROKS Secrets (and, for `flp up`, the FLP VSI's user data).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `cos.instance` | `bnk-supply-chain` (unless `cos.local_far_auth_file` is set) | Name of the COS service instance |
| `cos.bucket` | none | Bucket that holds the two objects |
| `cos.region` | `us-south` | Region of the bucket; the client uses `https://s3.<region>.cloud-object-storage.appdomain.cloud` |
| `cos.far_auth_object` | `f5-far-auth-key.tgz` | Object key of the FAR auth tarball |
| `cos.jwt_object` | `subscription.jwt` | Object key of the JWT |
| `cos.local_far_auth_file` | empty | Read the FAR tarball from this local file instead of COS |
| `cos.local_jwt_file` | empty | Read the JWT from this local file instead of COS |

`cos.instance` and `cos.bucket` are required unless `cos.local_far_auth_file` is set. The
COS instance is authenticated with the same IBM Cloud API key as everything else;
`init` resolves its CRN into `resolved.cos_instance_crn`.

## Commands

### cos publish

```sh
roksbnkargoctl cos publish --far-auth f5-far-auth-key.tgz --jwt subscription.jwt [--create-bucket]
```

| Flag | Meaning |
|---|---|
| `--far-auth <file>` | FAR auth tarball (`.tgz` from MyF5); uploaded as `cos.far_auth_object` |
| `--jwt <file>` | Subscription JWT file; uploaded as `cos.jwt_object` |
| `--create-bucket` | Create `cos.bucket` if it does not exist (Smart Tier, in `cos.region`) |

Pass either flag or both; with neither, `publish` stops with
`nothing to publish: pass --far-auth and/or --jwt`. Each file is validated before upload (a
file that is not a FAR auth tarball, or not a JWT, is refused) and replaces any existing
object of the same key.

### cos list

```sh
roksbnkargoctl cos list
```

Lists every object in the bucket with its size and modification time. The two objects the
workspace uses are marked with `*`:

```text
* f5-far-auth-key.tgz                          2291  2026-09-01T12:00:00Z
* subscription.jwt                             1843  2026-09-01T12:00:04Z
  older-subscription.jwt                       1839  2026-03-02T09:14:11Z
```

### cos verify

```sh
roksbnkargoctl cos verify
```

1. Reads the JWT and checks it is well-formed: `subscription JWT found and well-formed`.
2. Logs in to FAR with the key from the tarball and reads the BNK manifest chart,
   `<far_host>/release/f5-bigip-k8s-manifest:2.4.0`:
   `FAR key reads BNK 2.4.0 from repo.f5.com`.

`verify` reads from COS or from the local files, whichever the workspace is configured to
use, so it tests exactly what `install` will use.

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
