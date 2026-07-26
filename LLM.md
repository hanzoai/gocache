# gocache — LLM guide

A build cache for the go command with a shared second tier. The go command
starts it, speaks the GOCACHEPROG protocol to it, and gets compiler output that
another machine already produced.

## Scope, honestly

gocache caches **compiles**. It does not touch **linking**.

Linking is one process that loads every symbol in the program at once. The go
command does not cache link actions across machines and gocache cannot change
that. If a build's wall time is dominated by linking — which is the case for
large binaries and for `go test ./...` in a big module, where every package
gets its own test binary and every one of those is linked — a shared build
cache will not move it. That is a separate problem.

It also does not reduce link memory. Nothing here does.

## Why not zccache/sccache

They wrap rustc, MSVC and C compilers. There is no Go path in them
(`crates/zccache-compiler/src/parse_rustc.rs`, `parse_msvc.rs`). GOCACHEPROG is
the Go-native mechanism and needs no wrapper: the toolchain itself hands the
cache out.

## Verified toolchain fact

On go1.26.4, GOCACHEPROG is available with no GOEXPERIMENT gate. Checked
against this box's toolchain source: `cmd/go/internal/cacheprog/cacheprog.go`
defines the protocol, `cmd/go/internal/cache/prog.go` implements the client,
and `internal/goexperiment` has no cacheprog flag. Commands are `get`, `put`,
`close`. Earlier releases were not checked.

## Shape

One package, one concept per file, no dependencies outside the standard
library. A build-critical tool that holds credentials and feeds bytes into
every compile should have an empty supply chain.

- `prog.go` — the GOCACHEPROG conversation. Announces its commands, reads
  requests, writes replies.
- `disk.go` — `Disk`, the local tier. Content-addressed, atomic writes.
- `tier.go` — `Tier`, the composition: local first, shared on a miss.
- `remote.go` — `Remote`, the shared tier plus the policy that keeps a build
  independent of it.
- `store.go` — `Store`, the blob interface. One method to read, one to write.
- `s3.go` — `S3`, the one `Store` implementation.
- `sign.go` — AWS Signature Version 4.
- `kms.go` — reading credentials from Hanzo KMS.
- `main.go` — configuration and wiring.

## Layout

Local tier, under `GOCACHE_DIR`:

```
a/<xx>/<actionID>     one line: <outputID> <size> <unixnano>
o/<xx>/<outputID>     the bytes
```

Two namespaces, so two actions producing identical output share one file. The
output file is what the go command opens, and it must survive until the process
ends. Records are published by rename, because several go processes share one
directory.

Shared tier, one object per action under `<prefix>/<xx>/<actionID>`:

```
gocache1 <outputID> <size>\n<body>
```

One round trip per lookup. The header is bounded at 256 bytes and the body is
verified against the output ID before anything is handed to the compiler.

## Failure policy

The go command calls `base.Fatalf` if the cache program exits early, writes
malformed JSON, or answers an ID it never sent (`cmd/go/internal/cache/prog.go`
lines 102, 167, 169, 178). A crash here is a failed build in every repository
that has the cache enabled. So:

- The handshake is written before any credential or network work. The go
  command waits for it with no timeout of its own.
- Every shared operation has a deadline (`GOCACHE_TIMEOUT`, default 3s).
- Three consecutive failures disable the shared tier for the rest of the
  process. A dead store costs at most three deadlines for the whole build, not
  one per action.
- A miss is not a failure. An empty shared cache must not look like a broken
  one.
- Uploads go through a bounded queue and are dropped when it is full. A put
  never waits on the network.
- Every request handler recovers from panics and answers a miss.
- Anything that cannot be parsed, verified, or fetched is a miss. A miss means
  the action is compiled locally, which is always correct.

Proven in `TestBuildSurvivesBrokenSharedTier`, which runs real builds against a
refused port, a listener that accepts and never answers, a server that returns
500, and three malformed addresses.

## Configuration

Environment only. A laptop, a CI job and a PaaS builder all set environment
variables and none of them share a config file.

| Variable | Meaning | Default |
|---|---|---|
| `GOCACHE_DIR` | local tier directory | `~/.cache/gocache` |
| `GOCACHE_REMOTE` | `s3://bucket/prefix?endpoint=https://host&region=r` | none, local only |
| `GOCACHE_WRITE` | `1` to upload results | off |
| `GOCACHE_KEY` / `GOCACHE_SECRET` | access key pair | from KMS |
| `GOCACHE_KMS` | KMS secret path holding `access-key` and `secret-key` | `gocache` |
| `GOCACHE_TIMEOUT` | deadline per shared operation | `3s` |
| `GOCACHE_MAX` | largest object to share | `32M` |
| `GOCACHE_DRAIN` | time to finish uploads after the build is released | `30s` |
| `GOCACHE_VERBOSE` | `1` for statistics on stderr | off |
| `KMS_ADDR` `KMS_ORG` `KMS_ENV` `KMS_CLIENT_ID` `KMS_CLIENT_SECRET` | KMS identity | `https://kms.hanzo.ai` `hanzo` `prod` |

With no `GOCACHE_REMOTE` this is a plain local cache, which is what the go
command already has. Do not set `GOCACHEPROG` in that case.

## Credentials

Never in a repository, never in a file, never in this program. Two paths, both
rooted in KMS:

1. `GOCACHE_KEY` and `GOCACHE_SECRET` are already in the environment, because a
   `KMSSecret` CR synced them into a Kubernetes Secret, or a CI step read them
   from KMS. This is the in-cluster and CI path and costs no round trip.
2. Otherwise `KMS_CLIENT_ID` and `KMS_CLIENT_SECRET` identify a machine, and
   gocache reads `access-key` and `secret-key` from `GOCACHE_KMS` itself. Same
   two endpoints a workflow uses:
   `POST /v1/kms/auth/login`, then
   `GET /v1/kms/orgs/{org}/secrets/{path}/{key}?env={env}`.

With neither, the shared tier is off and the build runs local-only. It never
fails.

## Required before turning writes on

The shared S3 gateway has one admin identity today
(`hanzoai/cloud clients/storage/s3.go`), and handing that to a laptop would
hand it org-wide object storage. Before `GOCACHE_WRITE=1` is set anywhere,
give the cache its own bucket-scoped identities in the gateway's `s3.json`:

```json
{"identities": [
  {"name": "gocache-read",
   "credentials": [{"accessKey": "...", "secretKey": "..."}],
   "actions": ["Read:go-build-cache", "List:go-build-cache"]},
  {"name": "gocache-write",
   "credentials": [{"accessKey": "...", "secretKey": "..."}],
   "actions": ["Read:go-build-cache", "Write:go-build-cache", "List:go-build-cache"]}
]}
```

Seal both pairs in KMS at `hanzo/prod:/gocache` (read pair) and
`hanzo/prod:/gocache-write` (write pair). Laptops get the read pair. CI gets
the write pair. This is the control that matters: a shared build cache is a
supply-chain surface, and whoever can write to it can hand compiled objects to
everyone. The body digest check catches corruption, not a malicious writer.

## Enabling it

### Installing it

The repository is private, so `go install github.com/hanzoai/gocache@latest`
needs `GOPRIVATE=github.com/hanzoai/*` and a credential for github.com — the
same setup every other private hanzoai module needs. Without that, build it
from a checkout:

```sh
git clone git@github.com:hanzoai/gocache && cd gocache && go build -o ~/bin/gocache .
```

There are no dependencies, so neither path touches the module proxy for
anything but the module itself.

### Locally

```sh
export GOCACHE_REMOTE='s3://go-build-cache/v1?endpoint=https://s3.hanzo.ai&region=us-east-1'
export GOCACHE_KMS=gocache
export KMS_CLIENT_ID=...      # machine identity, from `hanzo auth`
export KMS_CLIENT_SECRET=...
export GOCACHEPROG="$(go env GOPATH)/bin/gocache"
```

Read-only by default: a laptop consumes what CI produced and pushes nothing.
Add `GOCACHE_VERBOSE=1` to see hit counts. To turn it off, unset `GOCACHEPROG`.

### Hanzo CI, in `.hanzo/workflows`

The forge scans `.hanzo/workflows` first
(`GIT__actions__WORKFLOW_DIRS=".hanzo/workflows,.github/workflows,.gitea/workflows"`,
set in `universe/infra/k8s/operator/crs/git.yaml`) and the syntax is
GitHub-Actions-compatible. Runner label `hanzo-build-linux-amd64` resolves to
`catthehacker/ubuntu:act-24.04`, which has `sudo`.

```yaml
jobs:
  build:
    runs-on: hanzo-build-linux-amd64
    steps:
      - uses: actions/checkout@v4

      - name: Install gocache
        env:
          GH_PAT: ${{ secrets.GH_PAT }}
        run: |
          # Private module: authenticate, then install. No dependencies, so
          # this is one module fetch.
          git config --global url."https://x-access-token:${GH_PAT}@github.com/".insteadOf "https://github.com/"
          GOPRIVATE='github.com/hanzoai/*' GOFLAGS= go install github.com/hanzoai/gocache@latest
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: Shared build cache
        env:
          KMS_CLIENT_ID: ${{ secrets.KMS_CLIENT_ID }}
          KMS_CLIENT_SECRET: ${{ secrets.KMS_CLIENT_SECRET }}
        run: |
          KMS=https://kms.hanzo.ai; ORG=hanzo; ENVN=prod; P=gocache-write
          TOKEN=$(curl -sf "$KMS/v1/kms/auth/login" -H 'Content-Type: application/json' \
            -d "{\"clientId\":\"$KMS_CLIENT_ID\",\"clientSecret\":\"$KMS_CLIENT_SECRET\"}" | jq -r .accessToken)
          for K in access-key secret-key; do
            V=$(curl -sf "$KMS/v1/kms/orgs/$ORG/secrets/$P/$K?env=$ENVN" \
              -H "Authorization: Bearer $TOKEN" | jq -r '.secret.value // empty')
            echo "::add-mask::$V"
            case $K in
              access-key) echo "GOCACHE_KEY=$V"    >> "$GITHUB_ENV" ;;
              secret-key) echo "GOCACHE_SECRET=$V" >> "$GITHUB_ENV" ;;
            esac
          done
          echo "GOCACHEPROG=$(go env GOPATH)/bin/gocache"                                >> "$GITHUB_ENV"
          echo "GOCACHE_REMOTE=s3://go-build-cache/v1?endpoint=https://s3.hanzo.ai"      >> "$GITHUB_ENV"
          echo "GOCACHE_WRITE=1"                                                          >> "$GITHUB_ENV"
          echo "GOCACHE_DRAIN=120s"                                                       >> "$GITHUB_ENV"

      - run: go build ./...
```

The KMS step is the same two-call pattern already used in
`hanzo/extension/.hanzo/workflows/publish.yml` and
`hanzo/cloud/.github/workflows/release.yml`. `GOCACHE_DRAIN` is longer in CI
because the job may end right after the last compile.

For a repo that only calls the shared reusable
(`uses: hanzoai/ci/.github/workflows/build.yml@v1`), the lever is `hanzo.yml`'s
`test[].run`, exporting inline — the same shape `cloud/hanzo.yml` already uses
for `RUSTC_WRAPPER`:

```yaml
test:
  - name: build
    run: |
      command -v gocache >/dev/null 2>&1 && export GOCACHEPROG=gocache || true
      go build ./...
```

### PaaS

Two build paths, and they take it differently.

**Platform-native CI (BuildKit)** — `platform/pkg/platform/src/services/ci/buildkit-job.ts`.
Add `GOCACHE_*` to `containers[0].env` in `buildBuildkitJob()`, reading them
through one `process.env` accessor defaulted at `launchBuildJob()`, the way
`fleetRegistryHost()` already does. Credentials go through
`buildkitArgs()` as `--secret=id=GOCACHE_SECRET,env=GOCACHE_SECRET` rather than
a build arg, so they do not land in an image layer. In-cluster the endpoint
should be `http://s3.hanzo.svc:9000` — same bucket, no egress.

**Buildpack-style app builds** (nixpacks / dockerfile / railpack) — nothing to
write. Set the `GOCACHE_*` names as application or project environment
variables and the existing `prepareEnvironmentVariables` plumbing emits them as
`--env` / `--build-arg` / `--secret`. Use `buildSecrets`, not `buildArgs`, for
the key pair.

Note: `hanzo.yml`'s `images[].build-args` is parsed by `hanzoai/ci` but dropped
by platform (`platform-config.ts parseImageEntry`), so per-repo configuration
through that key does not currently reach a PaaS build.

## What this does not protect against

- **cgo.** The go command's action IDs do not account for system C libraries
  (`go help cache` says so). Sharing a cache between machines with different C
  libraries can serve a stale object for a cgo package. Keep cgo-heavy repos
  off the shared tier, or key the prefix by base image.
- **A malicious writer.** The digest check proves the body matches the output
  ID the object claims; it does not prove who wrote the object. Write access is
  the control. See "Required before turning writes on".
- **Link time and link memory.** Not cached, not reduced, not addressed here.

## Tests

`go test ./...` — unit tests for the disk tier, the protocol, the tier
composition and the signer, plus end-to-end tests that run the real go command
with `GOCACHEPROG` set against an in-process S3 server. The signer is checked
against two published AWS signature vectors.

## Checked against the production gateway

Pointing gocache at `https://s3.hanzo.ai` with a deliberately wrong key returns:

```
Code: InvalidAccessKeyId
Resource: /go-build-cache/v1/cc/cc54a1...
BucketName: go-build-cache
Key: v1/cc/cc54a1...
```

The gateway accepted the `AWS4-HMAC-SHA256` header, parsed the credential
scope far enough to look the key up, and split the path-style URL into the
right bucket and key. A malformed request would have come back as
`AuthorizationHeaderMalformed` or `InvalidRequest` instead. The build itself
finished normally with the shared tier rejecting every request.

Not yet checked against production: a signature the gateway agrees with, since
that needs a real key. The signature arithmetic is covered by the AWS vectors.
