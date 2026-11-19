# Releasing

Push a version tag; the [release workflow](../.github/workflows/release.yml) does the rest:

```bash
git tag -a v0.1.0 -m "Pulse 0.1.0"
git push origin v0.1.0
```

It tests everything, builds a universal app (Apple Silicon + Intel) with the `pulse` CLI inside,
runs the end-to-end checks, and publishes a GitHub release with:

| File | What |
|---|---|
| `Pulse-<version>.dmg` | the app; drag to Applications |
| `pulse_<version>_darwin_universal.tar.gz` | the CLI alone, for Macs |
| `pulse_<version>_linux_amd64.tar.gz`, `…_arm64.tar.gz` | the CLI for Linux machines that report to a Mac |
| `SHA256SUMS` | checksums of the above |

The version comes from the tag (`v1.2.3` → `1.2.3` in the app's Info.plist); the build number is
the commit count. Running the workflow by hand (Actions → release → Run workflow) builds the same
files as an artifact without publishing.

## Signing and notarization

Without signing secrets the app is ad-hoc signed: it runs, but Gatekeeper asks people to confirm
the first launch (right-click → Open), and the release notes say so. With a paid Apple Developer
account, add these repository secrets and every release is signed with the hardened runtime,
notarized and stapled:

| Secret | Value |
|---|---|
| `APPLE_CERT_P12` | `base64 -i DeveloperID.p12`: a *Developer ID Application* certificate with its private key |
| `APPLE_CERT_PASSWORD` | the .p12's password |
| `APPLE_ID` | the Apple ID email used for notarization |
| `APPLE_TEAM_ID` | the 10-character team ID |
| `APPLE_APP_PASSWORD` | an app-specific password for that Apple ID (appleid.apple.com) |

Locally, `SIGN_IDENTITY="Developer ID Application: …" ./scripts/build-app.sh` and the same for
`./scripts/make-dmg.sh` produce signed builds.

## The icon

`assets/icon/pulse.svg` is the source. After editing it, run `./scripts/make-icon.sh`
(needs `brew install librsvg`) and commit the regenerated `Pulse.icns` and `pulse-512.png`.
