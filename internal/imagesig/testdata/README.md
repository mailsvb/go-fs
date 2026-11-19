# imagesig test fixtures

Fetched on 2026-11-19. The real ones were made by the tools named below and
are kept byte for byte; tests change copies in memory for negative cases.
Signatures that need no outside tool (ed25519/RSA/P-384 cosign keys, cosign
`.att` DSSE layers, key-based bundles, a TSA-only keyless bundle with a test
Fulcio and TSA, Notation JWS with PS256/ES256/ES384) are made in the tests
themselves (`pki_test.go`, `synthetic_test.go`). `cosign`, `notation` and
`crane` were not installed and nothing was installed for this.

| File | Made by | Where from |
|---|---|---|
| `cosign-ublue-sig-manifest.json` | cosign (key-based, legacy `.sig`) | `ghcr.io/ublue-os/base-main:sha256-0da9aa01d22a97fbc474d1eec650144bb0f3015ed07b057c2303f37164a4e08b.sig`, the signature of `ghcr.io/ublue-os/base-main:latest` = `sha256:0da9aa01…e08b` |
| `cosign-ublue-payload.json` | cosign | its simplesigning layer, blob `sha256:d931478e…500d` |
| `cosign-ublue.pub` | Universal Blue | https://github.com/ublue-os/main/blob/main/cosign.pub |
| `github-uv-bundle.json` | GitHub `actions/attest` v4.2.2 (keyless, public-good Fulcio + Rekor v1 with SET, inclusion proof and checkpoint; SLSA provenance) | `gh api repos/astral-sh/uv/attestations/sha256:d3b601f0c7b118bc1753febf7c41cc80981df678445e0254284eecf84db79a7d --jq '.attestations[0].bundle'`; the subject is the `ghcr.io/astral-sh/uv:latest` index. uv does not push attestations to GHCR, so the tests wrap the bundle in an OCI 1.1 referrer manifest the way `push-to-registry` does (`bundleManifest` in `real_test.go`) |
| `cosign-release-keyless.sigstore.json` | cosign v3.1.4 release (keyless message signature, Rekor v1 entry, RFC 3161 timestamp from timestamp.sigstore.dev) | `gh release download v3.1.4 -R sigstore/cosign -p cosign-darwin-arm64.sigstore.json`; signs the binary `sha256:9f9b8cd5…d3a4` |
| `cosign-release-kms.sigstore.json` | cosign v3.1.4 release (KMS key, public-key hint) | `gh release download v3.1.4 -R sigstore/cosign -p cosign-darwin-arm64-kms.sigstore.json` |
| `cosign-release.pub` | Sigstore | https://github.com/sigstore/cosign/blob/main/release/release-cosign.pub |
| `trusted_root-public-good.json` | Sigstore TUF | first line of `gh attestation trusted-root` (gh 2.x, public-good root) |
| `trusted_root-github.json` | GitHub TUF | second line of `gh attestation trusted-root` (GitHub's Fulcio and TSA, for private-repo attestations) |
| `notation-mcr-cose-manifest.json` | Microsoft's Notation signing (COSE envelope) | referrer `sha256:9b5b2ab8…5dfc` of `mcr.microsoft.com/azurelinux/base/core:3.0` (`sha256:3d16d879…6880`), via `GET /v2/azurelinux/base/core/referrers/<digest>` |

Not covered by real fixtures: a GitHub private-repository (TSA-only)
attestation, as none is public; the TSA-only path is tested with the real
cosign bundle with its tlog entry removed, and with a synthetic bundle. A
real Notation JWS signature, as the registries checked sign with COSE. A
Rekor v2 (log2025-1, `hashedrekord`/`dsse` 0.0.2) entry.

Nothing here goes stale: every keyless check runs at the signing time the
bundle proves, not today. Refreshing `trusted_root-*.json` from a newer
`gh attestation trusted-root` is fine, but `TestRealGitHubAttestationNegative`
counts the certificate authorities in both (8 CAs, 7 TSAs, 2 logs) and needs
the new numbers.
