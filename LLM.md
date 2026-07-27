# gocache — LLM guide

A build cache for the go command with a shared second tier. The go command
starts it, speaks the GOCACHEPROG protocol to it, and gets compiler output that
another machine already produced.

One program, one package, no dependencies outside the standard library.

## Scope, honestly

gocache caches **compiles**. It does not touch **linking**.

Linking is one process that loads every symbol in the program at once. The go
command does not cache link actions at all and gocache cannot change that. If a
build's wall time is dominated by linking, a shared build cache will not move
it. Measured on this fleet: one `./cmd/cloud` link is 6.23 GB and 297 s, and
`go test ./...` links 153 separate test binaries — roughly 2.6 hours of pure
linking. **None of that is helped by this cache.** It also does not reduce link
memory. Nothing here does.

Where the cache does win: those same 153 test binaries recompile the same
~2,200-package dependency base over and over, on every machine and every CI run.
That compile work is identical across all of them, and deduplicating it is
exactly what this does.

So the honest summary is: this removes repeated *compilation*, which is a large
share of a cold build and nearly all of the work a second machine duplicates. It
leaves *linking* exactly where it was.

## Why not zccache or sccache

They wrap rustc, MSVC and C compilers. There is no Go path in either
(`crates/zccache-compiler/src/parse_rustc.rs`, `parse_msvc.rs`). GOCACHEPROG is
the Go-native mechanism and needs no wrapper: the toolchain hands the cache out
itself.

## The mechanism

`GOCACHEPROG` names a program the go command starts as a subprocess and talks to
in JSON over stdin and stdout. Verified against this box's toolchain, go1.26.4:

- `cmd/go/internal/cacheprog/cacheprog.go` defines the protocol. Commands are
  `get`, `put`, `close`. There is no GOEXPERIMENT gate on go1.26.4.
- `cmd/go/internal/cache/prog.go` is the client. It calls `base.Fatalf` if this
  program exits early, writes malformed JSON, or answers an ID it never sent.
- A `put` with `BodySize > 0` is followed by the body as a base64 JSON string on
  its own line. Replies may be out of order; each carries its request's ID.
- `Response.DiskPath` must be an **absolute** path to a file holding the body,
  and that file must survive until `close`.
- `Cache.Close` is documented as where a cache implementation does its cleanup,
  with the rule that trimming in one process must not delete what another is
  using, and a rule of thumb of not trimming anything used in the last day.

Verified separately: closing stdout is what releases the go command. Its `Close`
sends the close request, cancels the context, and then waits only for **its own
read loop to see EOF** on our stdout. It does not wait for this process to exit.
That is why uploads and trimming, which happen after stdout is closed, cost a
build nothing.

One consequence worth knowing: a tool that captures a build's output through a
pipe *does* wait, because a pipe reaches EOF only when every writer closes it,
and this program is still holding one. A developer at a terminal is unaffected;
a CI step that captures output waits for `GOCACHE_DRAIN`. The test harness works
around this by giving builds a real file, which is also what makes its timings
honest (`e2e_test.go runBuild`).

## Shape

One concept per file.

- `prog.go` — the GOCACHEPROG conversation. Announces its commands, reads
  requests, writes replies.
- `disk.go` — `Disk`, the local tier. Content-addressed, atomic writes.
- `trim.go` — the local tier's ceiling. Marking on use, scanning on a schedule.
- `tier.go` — `Tier`, the composition: local first, shared on a miss.
- `object.go` — the shared object's format and its two integrity checks.
- `remote.go` — `Remote`, the shared tier plus the policy that keeps a build
  independent of it.
- `store.go` — `Store`, the blob interface. One method to read, one to write.
- `s3.go` — `S3`, the one `Store` implementation.
- `sign.go` — AWS Signature Version 4.
- `kms.go` — reading secrets from Hanzo KMS.
- `main.go` — configuration and wiring.

## The local tier

Under `GOCACHE_DIR`:

```
a/<xx>/<actionID>     one line: <outputID> <size> <unixnano>
o/<xx>/<outputID>     the bytes
trim                  unix seconds of the last scan
```

Two namespaces, so two actions producing identical output share one file. The
output file is what the go command opens, and it must survive until the process
ends. Records are published by rename, because several go processes share one
directory.

### It is bounded

The go command trims its own cache. A program that replaces it inherits that
job. Without trimming there is no ceiling at all — for reference, the go
command's own cache on this box is **53 GB** *with* trimming on.

The policy is the go command's own, taken from `cmd/go/internal/cache/cache.go`,
because it was chosen from measurements of real Go development
(golang.org/issue/22990) and because two caches on one machine with different
retention would be one more thing to reason about:

- An entry's modification time is its time of last use, refreshed at most once
  an hour so that reading the cache does not rewrite every inode.
- A scan happens at most once a day, across every process sharing the directory.
- A scan removes what has not been used in five days.

Both files of a hit are marked, so a scan cannot take the bytes out from under a
record that is still live. Marking costs no extra system call: the times come
from reads `Disk.Get` already makes. Abandoned `.tmp-*` files, the wreckage of a
process killed mid-write, are swept on the same schedule.

Trimming runs after stdout is closed, so it is never on a build's critical path.

Note: `go clean -cache` does **not** know about GOCACHEPROG and will not clear
this. To empty it, remove `GOCACHE_DIR`.

## The shared tier

One object per action under `<prefix>/<xx>/<actionID>`:

```
gocache2 <outputID> <size> <mac>\n<body>
```

Two checks stand between shared storage and the compiler, and a build is correct
whichever one rejects, because anything unreadable, unverified or unauthentic is
a miss and a miss is compiled locally.

**The digest check** — `sha256(body)` equals the output ID the object claims —
catches corruption at rest and truncation in transit. It is *not* an integrity
boundary on its own: every field it compares came from the same object, so
whoever wrote the object chose both the body and the digest. A self-consistent
forgery is trivial to construct.

**The authenticity check is the boundary.** `mac` is HMAC-SHA256 over the format
version, the action ID, the output ID, the size and the body, under a key held
only by machines that participate in this cache. It closes two attacks the
digest cannot:

1. **Forgery.** A build cache is a supply-chain surface: an object accepted for
   an action ID is linked into binaries on every machine that asks for that
   action. This matters concretely here — the S3 gateway this runs against has
   a **single identity** (`access-key: hanzo`, actions `Admin,Read,Write,List,
   Tagging`) shared by roughly fifteen services through the `s3-credentials`
   Secret. "Can write to the bucket" is a far weaker statement on this fleet
   than "is a build machine". With the seal key, bucket write access alone buys
   an attacker nothing.
2. **Substitution.** The action ID is inside the MAC, not merely the key the
   object was found under. Without that, an authentic object for one action
   could be copied onto another action's key and would verify — handing the
   compiler a genuine object for the wrong compilation.

The MAC proves an object came from something holding the seal key. It does not
say which machine, and it does not survive the key leaking. Write-scoped
credentials and a key only build machines hold are what keep it meaningful.

The header is bounded at 256 bytes; the body is bounded by `GOCACHE_MAX` and is
verified before anything reaches the compiler. One round trip per lookup.

## Failure policy

A crash here is a failed build in every repository that has the cache enabled.
So:

- The handshake is written before any credential or network work. The go command
  waits for it with no timeout of its own.
- Every shared operation has a deadline (`GOCACHE_TIMEOUT`, default 3s),
  including the wait for credentials to arrive from KMS.
- Three consecutive failures disable the shared tier for the rest of the
  process. A dead store costs at most three deadlines for the whole build, not
  one per action.
- A miss is not a failure. An empty shared cache must not look like a broken one.
- Uploads go through a bounded queue and are dropped when it is full. A put never
  waits on the network.
- Every request handler recovers from panics and answers a miss.
- Anything that cannot be parsed, verified, or authenticated is a miss.

## Silence

Absent `GOCACHE_VERBOSE`, this program writes **nothing**. The go command's
stderr belongs to the build. A shared tier that is unreachable is meant to cost
a developer nothing, and a line of explanation on every build for a week is not
nothing — it is how a tool teaches people to distrust it.

The consequence to plan for: a cache that quietly stops helping is the failure
this program is most likely to have in production. So **CI should always set
`GOCACHE_VERBOSE=1`**, where the statistics land in a log nobody has to read but
anybody can grep. The reason the shared tier is off is printed there, not just
the fact:

```
gocache: 2874 gets, 12 local, 2103 shared, 759 miss
gocache: shared 2103 hit, 759 miss, 0 fail, 0 up, 0 dropped, 41213K down, 0K up
gocache: shared tier off: 3 consecutive failures, last: s3: 403 Forbidden: ...
```

## Configuration

Environment only. A laptop, a CI job and a PaaS builder all set environment
variables and none of them share a config file.

| Variable | Meaning | Default |
|---|---|---|
| `GOCACHE_DIR` | local tier directory | `~/.cache/gocache` |
| `GOCACHE_REMOTE` | `s3://bucket/prefix?endpoint=https://host&region=r` | none, local only |
| `GOCACHE_WRITE` | `1` to upload results | off |
| `GOCACHE_KEY` / `GOCACHE_SECRET` | S3 access key pair | from KMS |
| `GOCACHE_SEAL` | seal key, at least 16 bytes | from KMS |
| `GOCACHE_KMS` | KMS secret path holding `access-key`, `secret-key`, `seal-key` | `gocache` |
| `GOCACHE_TIMEOUT` | deadline per shared operation | `3s` |
| `GOCACHE_MAX` | largest object to share | `32M` |
| `GOCACHE_DRAIN` | time to finish uploads after the build is released | `30s` |
| `GOCACHE_VERBOSE` | `1` for statistics on stderr | off |
| `KMS_ADDR` `KMS_ORG` `KMS_ENV` `KMS_CLIENT_ID` `KMS_CLIENT_SECRET` | KMS identity | `https://kms.hanzo.ai` `hanzo` `prod` |

With no `GOCACHE_REMOTE` this is a plain local cache, which is what the go
command already has. Do not set `GOCACHEPROG` in that case.

`GOCACHE_DIR` must not be the go command's own cache. Two layouts and two
retention policies in one directory is a way to damage the build cache a machine
is already using, so it is refused at startup — before the handshake, where the
go command reports it plainly.

Raise `GOCACHE_TIMEOUT` on machines known to be loaded. The default of 3s is
generous for a round trip but not for a goroutine waiting to be scheduled on a
box running dozens of compilers; see the measurement section, where the default
tripped the breaker before a single object was uploaded.

The three secrets are required **together**. A cache that can reach the store
but cannot authenticate its objects would have to either accept unsigned ones or
reject every one, and both are worse than being off. Missing any of them leaves
the shared tier off, with the reason in the statistics.

## Credentials

Never in a repository, never in a file, never in this program. Two paths, both
rooted in KMS:

1. `GOCACHE_KEY`, `GOCACHE_SECRET` and `GOCACHE_SEAL` are already in the
   environment, because a `KMSSecret` CR synced them into a Kubernetes Secret or
   a CI step read them from KMS. This is the in-cluster and CI path and costs no
   round trip.
2. Otherwise `KMS_CLIENT_ID` and `KMS_CLIENT_SECRET` identify a machine, and
   gocache reads `access-key`, `secret-key` and `seal-key` from `GOCACHE_KMS`
   itself, using the same two calls a workflow uses:
   `POST /v1/kms/auth/login` with `{"clientId","clientSecret"}` returning
   `{"accessToken"}`, then
   `GET /v1/kms/orgs/{org}/secrets/{path}/{key}?env={env}`.

   The live KMS is embedded in the cloud binary and answers the flat shape
   `{"name","env","value"}`; the standalone implementations answer
   `{"secret":{"value"}}`. `kms.go` accepts both, which is what the existing
   Go SDK does.

With neither, the shared tier is off and the build runs local-only. It never
fails.

## Required before this is turned on

**None of this is provisioned yet.** The program is finished; the infrastructure
it needs is not. This is the honest blocker list.

The shared S3 gateway (`s3.hanzo.svc:9000` in cluster, `https://s3.hanzo.ai`
public, SeaweedFS, path-style, `us-east-1`) has exactly one identity today,
`access-key: hanzo`, with `Admin` on every bucket, and about fifteen services
hold it through the `s3-credentials` Secret. Handing that to a laptop would hand
out org-wide object storage, and there is an active credential-exposure incident
on this fleet. So, in order:

1. **A bucket.** `go-build-cache`. Buckets here are created explicitly, never on
   first write: either by a Job running `s3 mb --ignore-existing`
   (`universe/infra/k8s/console/console-sqlite.yaml:140` is the pattern) or
   through the cloud API, `POST /v1/s3/buckets`
   (`cloud/clients/storage/s3.go:138`).

2. **Two scoped identities** in the gateway's `s3.json`
   (`universe/infra/k8s/storage/s3.yaml`), so that reading and writing are
   different privileges:

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

3. **A seal key**, 32 random bytes, base64 or hex. The same value for every
   participant; rotating it invalidates every object, which is a cheap and
   correct way to flush the shared tier.

4. **Two KMS secrets**, `hanzo/prod`: `/gocache` holding `access-key`,
   `secret-key` (the read pair) and `seal-key`; `/gocache-write` holding the
   write pair and the same `seal-key`. Laptops get the read path. CI gets the
   write path.

Until step 4 exists, every machine configured for the shared tier runs
local-only and says so under `GOCACHE_VERBOSE=1`. Nothing breaks.

## Installing it

The repository is private. **Verified working** on this box:

```sh
go install github.com/hanzoai/gocache@feat/gocache
```

That resolved to `v0.0.0-20260726215552-50f76eb99c95` and produced a working
binary. What makes it work:

- `GOPRIVATE` already includes `github.com/hanzoai/*` in this fleet's `go env`,
  so the module proxy and checksum database are bypassed and the fetch goes
  direct.
- Git authenticates over SSH because of a global rewrite already configured
  here: `url.git@github.com:.insteadOf https://github.com/`.
- gocache has no dependencies, so this is exactly one module fetch.

**Pin an explicit ref.** `@latest` is a trap on this repository right now: there
is no `main` branch and no tags, so GitHub reports `feat/gocache` as the default
branch and `@latest` resolves to whatever that branch happened to be. Use a
branch or commit until this is merged and tagged, then use the tag.

In CI, where there is no SSH key, the same fetch works through a token:

```sh
git config --global url."https://x-access-token:${GH_PAT}@github.com/".insteadOf "https://github.com/"
```

Or skip the module system entirely — there are no dependencies, so a checkout
builds offline:

```sh
git clone git@github.com:hanzoai/gocache && cd gocache && go build -o ~/bin/gocache .
```

### The one footgun

`GOCACHEPROG` pointing at a file that does not exist breaks **every** go command
on the machine. Verified:

```
$ GOCACHEPROG=/nonexistent/gocache go build .
error starting GOCACHEPROG program "/nonexistent/gocache": fork/exec /nonexistent/gocache: no such file or directory
```

This is the reason to prefer `go env -w` below over a shell profile: it is one
command to set and one to undo, and it cannot be forgotten in a dotfile on a
machine you are not sitting at.

## Wiring

### Locally

`go env -w` persists into `~/.config/go/env` and applies to every go command.
Verified that it both writes and reads back:

```sh
go install github.com/hanzoai/gocache@feat/gocache      # before setting GOCACHEPROG
go env -w GOCACHEPROG="$(go env GOPATH)/bin/gocache"
go env -w GOCACHE_REMOTE='s3://go-build-cache/v1?endpoint=https://s3.hanzo.ai&region=us-east-1'
```

Then supply a machine identity, from `hanzo auth`, in your shell:

```sh
export KMS_CLIENT_ID=... KMS_CLIENT_SECRET=...
```

Read-only by default: a laptop consumes what CI produced and pushes nothing.
`GOCACHE_VERBOSE=1` on a single build shows hit counts. To turn it all off:

```sh
go env -u GOCACHEPROG
```

Order matters: install the binary *before* setting `GOCACHEPROG`, or the go
command that would install it cannot start.

### Hanzo CI, in `.hanzo/workflows`

Verified against the fleet:

- The forge reads **`.hanzo/workflows` and nothing else**.
  `universe/infra/k8s/operator/crs/git.yaml:320` sets
  `GIT__actions__WORKFLOW_DIRS: ".hanzo/workflows"`. `.github/workflows` and
  `.gitea/workflows` were deliberately removed from that list, so a workflow
  placed there is not run by the forge.
- `runs-on: hanzo-build-linux-amd64` is the label to use. It resolves to
  `docker://catthehacker/ubuntu:act-24.04`
  (`universe/infra/k8s/git-runner/statefulset.yaml:135`) and is what 84 of the
  ~100 `runs-on` lines across the fleet's `.hanzo/workflows` already say.
- Secrets come from a KMS machine identity. `KMS_CLIENT_ID` and
  `KMS_CLIENT_SECRET` are the only two `secrets.*` a job should need; everything
  else is fetched. The pattern below is the one already in
  `extension/.hanzo/workflows/publish.yml:36`.

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
          # Private module, no dependencies: one fetch, no proxy.
          git config --global url."https://x-access-token:${GH_PAT}@github.com/".insteadOf "https://github.com/"
          GOPRIVATE='github.com/hanzoai/*' GOFLAGS= go install github.com/hanzoai/gocache@feat/gocache
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: Shared build cache
        env:
          KMS_CLIENT_ID: ${{ secrets.KMS_CLIENT_ID }}
          KMS_CLIENT_SECRET: ${{ secrets.KMS_CLIENT_SECRET }}
        run: |
          KMS=https://kms.hanzo.ai; ORG=hanzo; ENVN=prod; P=gocache-write
          TOKEN=$(curl -sf "$KMS/v1/kms/auth/login" -H 'Content-Type: application/json' \
            -d "{\"clientId\":\"$KMS_CLIENT_ID\",\"clientSecret\":\"$KMS_CLIENT_SECRET\"}" | jq -r .accessToken)
          [ -n "$TOKEN" ] && [ "$TOKEN" != null ] || { echo "::error::KMS login failed"; exit 1; }
          for K in access-key secret-key seal-key; do
            # Two response shapes are live on this fleet: the cloud-embedded KMS
            # answers {"value"} and the standalone ones answer {"secret":{"value"}}.
            V=$(curl -sf "$KMS/v1/kms/orgs/$ORG/secrets/$P/$K?env=$ENVN" \
              -H "Authorization: Bearer $TOKEN" | jq -r '.secret.value // .value // empty')
            echo "::add-mask::$V"
            case $K in
              access-key) echo "GOCACHE_KEY=$V"    >> "$GITHUB_ENV" ;;
              secret-key) echo "GOCACHE_SECRET=$V" >> "$GITHUB_ENV" ;;
              seal-key)   echo "GOCACHE_SEAL=$V"   >> "$GITHUB_ENV" ;;
            esac
          done
          echo "GOCACHEPROG=$(go env GOPATH)/bin/gocache"                           >> "$GITHUB_ENV"
          echo "GOCACHE_REMOTE=s3://go-build-cache/v1?endpoint=http://s3.hanzo.svc:9000" >> "$GITHUB_ENV"
          echo "GOCACHE_WRITE=1"                                                    >> "$GITHUB_ENV"
          echo "GOCACHE_DRAIN=120s"                                                 >> "$GITHUB_ENV"
          echo "GOCACHE_VERBOSE=1"                                                  >> "$GITHUB_ENV"

      - run: go build ./...
```

Three details that are easy to get wrong:

- **`jq -r '.secret.value // empty'` is not enough.** The workflows on this
  fleet use exactly that, but the live KMS is embedded in the cloud binary and
  answers the flat shape `{"name","env","value"}`
  (`cloud/clients/kms/mount.go:309`), where `.secret.value` is null. Adding
  `// .value` covers both. gocache's own `kms.go` already accepts either.
- **In-cluster, use `http://s3.hanzo.svc:9000`**, not the public host. Same
  bucket, no egress, no TLS handshake per connection.
- **`GOCACHE_DRAIN` is charged to the CI step, not to the build.** A step's
  output is captured through a pipe, and a pipe reaches EOF only when every
  writer closes it, so the runner waits for the uploads even though the go
  command has already exited. 120s is an upper bound, not a cost you pay every
  time.

For a repository that only calls the shared reusable workflow
(`uses: hanzoai/ci/.github/workflows/build.yml@v1`), the lever is `hanzo.yml`'s
`test[].run`, which `ci/.github/workflows/build.yml:508` executes with `bash -c`
directly on the runner:

```yaml
test:
  - name: build
    run: |
      command -v gocache >/dev/null 2>&1 && export GOCACHEPROG=gocache || true
      go build ./...
```

Note what is *not* available there: `hanzo.yml` has no `env:` key and no
`build:` hook. The only declarative channels are `images[].args` (static
literals, and used by no repository today) and `images[].build_secrets` (names
fetched from KMS into the job environment). So a `test[].run` that exports the
variables itself is the whole mechanism.

### PaaS — not wired, and this is the honest state

**There is no working path today to get `GOCACHE_*` into a platform-native
BuildKit build.** Documented here as a blocker rather than as instructions,
because the instructions would not run.

- `platform/pkg/platform/src/services/ci/buildkit-job.ts:323` sets
  `containers[0].env` to exactly three hardcoded entries, all git credentials.
  `BuildJobLaunchInput` has no `env` field at all.
- `buildkitArgs` at line 218 passes exactly three `--secret` flags, all git
  credentials. There is no caller extension point.
- `BuildJobLaunchInput.buildArgs` is declared at line 130 and consumed at line
  207, but the only caller — `dispatchBuild` in `build-scheduler.ts:309` —
  never sets it, so `--opt=build-arg:` is never emitted.
- `hanzo.yml`'s `images[].args` is read by `hanzoai/ci` and **dropped** by
  `platform/pkg/platform/src/services/ci/platform-config.ts:339 parseImageEntry`,
  along with `build_secrets` and `platforms`.

Closing this needs a change in three files of `hanzoai/platform`
(`platform-config.ts` to carry the keys, `build-scheduler.ts` to pass them,
`buildkit-job.ts` to emit them as `--secret`, not `--build-arg`, so the values
do not land in an image layer). That is a separate, reviewable change in
another repository and is not attempted here.

The legacy app-deploy path is different and *does* work, with one caveat:
`prepareEnvironmentVariables` (`platform/pkg/platform/src/utils/docker/utils.ts:399`)
emits **service-level** variables as `--env` for nixpacks, railpack, paketo and
heroku builds. A plain **project**-level variable does not reach a build; it is
only an interpolation source for `${{project.KEY}}` referenced from a service
variable. For dockerfile builds the split is `application.buildArgs` →
`--build-arg` and `application.buildSecrets` → `--secret type=env`; the key pair
belongs in `buildSecrets`, never `buildArgs`, so it stays out of image history.

## What this does not protect against

- **cgo.** The go command's action IDs do not account for system C libraries
  (`go help cache` says so). Sharing a cache between machines with different C
  libraries can serve a stale object for a cgo package. Keep cgo-heavy repos off
  the shared tier, or key the prefix by base image.
- **A leaked seal key.** The MAC proves an object came from something holding
  the key. Everyone who reads the cache holds it, so it is a control against
  whoever holds only the *bucket* credential — which on this fleet is most of
  the platform — not against a compromised build machine.
- **Attribution.** Nothing here says which machine wrote an object.
- **Link time and link memory.** Not cached, not reduced, not addressed here.

## Measured

`github.com/hanzoai/kms`: 648 packages in the dependency closure, 5 packages of
its own, 4 binaries. Small enough to measure honestly, real enough to count.
Two workloads, because they answer different questions.

`compile` builds the 648-package closure with nothing linked — the work a shared
cache can eliminate. `build` is `go build ./...` as a person runs it, four links
included. `-p 4` throughout. Every run starts with an empty local tier; the
shared runs read a tier a previous run filled. Wall time on this box is noise,
so CPU (user+sys, covering the go command and every compiler it reaps) is the
number to trust.

Shared tier on loopback, no injected latency:

| run | wall | CPU | actions from the shared tier |
|---|---|---|---|
| compile, cold, no shared tier | 29.00 s | 181.4 s | — |
| compile, cold local, warm shared | **0.36 s** | **0.52 s** | 1618 of 1618 |
| compile, cold, no shared tier (repeat) | 28.77 s | 180.6 s | — |
| compile, cold local, warm shared (repeat) | **0.38 s** | **0.55 s** | 1618 of 1618 |
| `go build ./...`, cold, no shared tier | 31.07 s | 187.9 s | — |
| `go build ./...`, cold local, warm shared | **2.57 s** | **6.2 s** | 1618 of 1633 |

Same thing with 5 ms injected per request, which is the shape of an in-cluster
round trip rather than a loopback one:

| run | wall | CPU |
|---|---|---|
| compile, cold, no shared tier | 29.52 s | 167.4 s |
| compile, cold local, warm shared | **2.21 s** | **0.52 s** |
| `go build ./...`, cold, no shared tier | 52.18 s | 197.3 s |
| `go build ./...`, cold local, warm shared | **8.02 s** | **6.7 s** |

Reading these honestly:

- **Compilation is what moves.** 181 s of CPU becomes 0.5 s. That is the whole
  claim, and it holds: the second machine did no compiling at all.
- **The residue is linking.** `go build ./...` keeps 2.57 s and 6.2 s of CPU
  that the cache cannot touch, because four binaries still have to be linked.
  On a repository whose binaries are large, that residue is the whole build --
  see the scope section. kms links four small binaries, so the residue is small
  here and would not be elsewhere.
- **Latency costs wall time, not CPU.** 5 ms per object turns 0.36 s into
  2.21 s while CPU stays at 0.5 s. 1618 objects at 5 ms is 8 s of round trips
  compressed by 32-way concurrency into about 2 s. It is still an order of
  magnitude better than compiling, and it is the reason CI should use
  `http://s3.hanzo.svc:9000` rather than the public host.
- **The populate run is not free.** Filling the tier cost 26.9 s wall and
  392 MB uploaded across 2311 objects — about the same as a cold build, because
  it *is* a cold build that also uploads. The uploads run after the go command
  is released, so they cost the build nothing and cost the CI step its drain.

Both runs were on a box shared with other work, at load averages between 9 and
48. That is why every conclusion above rests on the CPU column.

### One thing the measurement taught

The first attempt produced **nothing**: the populate run tripped the breaker
before uploading a single object, with `context deadline exceeded` against a
server that answers in 0.00 s. The cause was not the server. At load 48, with
four compilers running and a cold 648-package build underway, the cache's own
goroutines could not be scheduled inside the 3 s default deadline.

That is the breaker working exactly as designed — it protected the build by
giving up on a tier it could not reach in time — but it means **a busy CI runner
may fail to populate the cache at the default `GOCACHE_TIMEOUT=3s`**. Set it
higher where the machine is known to be loaded; the measurement above used 15 s.
The cost of a larger deadline is bounded by the breaker either way: three
deadlines for the whole build, not one per action.

## Tests

`go test ./...`. Unit tests for the disk tier, trimming, the object format, the
protocol, the tier composition and the signer, plus end-to-end tests that run
the real go command with `GOCACHEPROG` set against an in-process S3 server. The
signer is checked against published AWS signature vectors.

The load-bearing ones:

| Test | What it pins down |
|---|---|
| `TestHangingSharedTierDoesNotSlowTheBuild` | a shared tier that never answers costs a bounded amount of time, asserted, not reported |
| `TestBuildSurvivesBrokenSharedTier` | refused, hung, erroring, and three malformed addresses all still build |
| `TestForgedObjectIsRejected` | a self-consistent object without the seal key is refused |
| `TestObjectMovedToAnotherActionIsRejected` | a genuine object on the wrong key is refused |
| `TestDamagedObjectIsRejected` | corruption and truncation read as absent |
| `TestNothingIsPrintedWithoutVerbose` | silence, including when misconfigured |
| `TestUseKeepsAnEntryAlive` | trimming does not evict what builds are using |
| `TestTheGoCommandsOwnCacheIsRefused` | the local tier can never be the machine's own build cache |
| `TestTrimScansAtMostOncePerInterval` | scanning is bounded |

## Checked against the production gateway

Pointing gocache's signer at `https://s3.hanzo.ai` with a deliberately wrong key
returns:

```
STATUS: 403 Forbidden
<Error><Code>InvalidAccessKeyId</Code>
  <Resource>/go-build-cache/v1/cc/cc54a1deadbeef</Resource>
  <BucketName>go-build-cache</BucketName><Key>v1/cc/cc54a1deadbeef</Key></Error>
```

The gateway accepted the `AWS4-HMAC-SHA256` header, parsed the credential scope
far enough to look the key up, and split the path-style URL into the right
bucket and key. A malformed request would have come back as
`AuthorizationHeaderMalformed` or `InvalidRequest`.

Not checked against production: a signature the gateway *agrees* with, because
that needs a real key, which is the provisioning work above. The signature
arithmetic is covered by the AWS vectors in `sign_test.go`.
