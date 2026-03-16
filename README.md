# operator-signature-verifier

A tool that verifies the cryptographic signatures of every container image referenced by Red Hat OpenShift operator catalogs, giving you a single CSV report that answers: *"Are all the images in my operator catalog actually signed by Red Hat?"*

## Why this exists

OpenShift operator catalogs — published as [File-Based Catalogs](https://olm.operatorframework.io/docs/reference/file-based-catalogs/) (FBC) — can reference thousands of container images across hundreds of operator bundles. Each bundle declares its own image plus a set of `relatedImages` (operands, sidecars, init containers, etc.) that the operator needs at runtime. In a supply-chain-security-conscious deployment, every one of those images should carry a valid [Sigstore](https://www.sigstore.dev/) signature from the publisher.

Manually verifying this across 10 OCP release catalogs, each 100–300 MB of newline-delimited JSON, is not practical. This tool automates the entire pipeline:

1. **Parse** all catalogs, extract every image reference, and deduplicate across OCP versions.
2. **Verify** each unique image against a signing key using the [`containers/image`](https://github.com/containers/image) library — the same library that powers Podman, Skopeo, and CRI-O.
3. **Report** results as a filterable CSV with one row per architecture for multi-arch images.

## How it works

### Phase 1 — Catalog parsing

The tool shells out to `jq` to stream-parse FBC JSON files (which can be hundreds of megabytes each). It extracts every `olm.bundle` entry along with its package name, version, bundle image, and related images. Image references are deduplicated by digest across all catalogs; the OCP versions in which each image appears are tracked for the report.

### Phase 2 — Parallel verification

A configurable pool of worker goroutines pulls image references from a shared queue. For each image, the tool:

- **Fetches the manifest** from the registry via the Docker v2 API.
- **Checks the Sigstore signature** attached as an OCI tag (`sha256-<digest>.sig`) using the supplied PEM public key.
- For **multi-arch manifest lists**, verifies the top-level signature first. If the list itself is signed, all architecture-specific child manifests inherit that status. Otherwise, each child manifest with a `platform.architecture` field is verified individually.
- **Retries transient failures** with exponential backoff and jitter (up to 5 attempts).

Image references that carry both a tag and a digest (e.g., `registry.io/img:v1@sha256:abc…`) are normalized to digest-only form, since the digest is authoritative and dual-form references are rejected by some tooling.

### Phase 3 — CSV report

Results are written as a CSV file. Multi-arch images are exploded into one row per child manifest so you can filter by architecture. The columns are:

| Column | Description |
|---|---|
| `package` | Operator package name |
| `bundle_name` | OLM bundle identifier |
| `bundle_version` | Semantic version of the bundle |
| `image_name` | Role of the image within the bundle (e.g., the operand name) |
| `image_reference` | Full pull spec with digest |
| `ocp_versions` | Semicolon-separated list of OCP releases containing this image |
| `available` | Whether the manifest could be fetched from the registry |
| `signed` | Whether a valid Sigstore signature was found |
| `arch` | CPU architecture (multi-arch only) |
| `os` | Operating system (multi-arch only) |
| `child_digest` | Digest of the architecture-specific manifest (multi-arch only) |
| `error` | Verification error detail, if any |

## Prerequisites

- **Go 1.23+** (to build)
- **jq** (used at runtime to stream-parse large FBC files)
- **Registry credentials** — an `auth.json` file if your catalogs reference images in authenticated registries (same format as `podman login` / `skopeo login`)
- **Signing key** — the publisher's PEM-encoded public key (PKIX/X509 format)

## Building

```bash
go build -o osv .
```

## Usage

```
./osv [flags]
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--catalogs` | `redhat-operator-index-v4.*.json` | Glob pattern matching the FBC catalog files to scan |
| `--key` | `red-hat-signing-key-v3.txt` | Path to the PEM-encoded public signing key |
| `--output` | `verification-results.csv` | Path for the output CSV report |
| `--authfile` | *(none)* | Path to a registry `auth.json` file (as used by Podman/Skopeo) |
| `--workers` | `5` | Number of parallel verification goroutines |

### Examples

Verify all Red Hat operator catalog images for OCP 4.17 through 4.21:

```bash
./osv \
  --catalogs 'redhat-operator-index-v4.{17..21}.json' \
  --key red-hat-signing-key-v3.txt \
  --authfile ~/.config/containers/auth.json \
  --workers 10 \
  --output results.csv
```

Verify a single catalog:

```bash
./osv \
  --catalogs redhat-operator-index-v4.21.json \
  --key red-hat-signing-key-v3.txt \
  --authfile auth.json
```

## Architecture notes

- **No shelling out for verification.** Earlier iterations called `skopeo` and `cosign` as subprocesses. The current version uses `containers/image` directly for manifest fetching, signature retrieval, and policy evaluation. This eliminates per-image process overhead and gives the tool access to HTTP connection pooling across requests.
- **Sigstore OCI-tag attachments.** The tool programmatically creates a temporary `registries.d` configuration that enables `use-sigstore-attachments: true` for all registries. This tells `containers/image` to look for cosign-style signature tags (`sha256-<digest>.sig`) without requiring system-wide configuration.
- **Thread-safe policy evaluation.** The `containers/image` `PolicyContext` is stateful and not safe for concurrent use. Each worker goroutine creates its own `PolicyContext` instance from the shared key material.
- **Identity matching.** Signature identity is verified at the repository level (`prmMatchRepository`), which checks that the signature's embedded Docker reference matches the repository of the image being verified. This is consistent with how `cosign verify --key` operates for key-based verification — it validates the cryptographic signature and digest binding without imposing constraints on the tag or digest format within the signed identity.

## License

This project is licensed under the [Apache License 2.0](LICENSE).
