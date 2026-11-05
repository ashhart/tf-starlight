# Changelog

## Unreleased

- Starlight moves here from [ashhart/TensorFold](https://github.com/ashhart/TensorFold), where it landed in
  ashhart/TensorFold#1922, with its history. The Go module is now `github.com/ashhart/tf-starlight`.
- [`engine.json`](engine.json) pins TensorFold 1.2.1, the engine release Starlight is tested with. CI checks the engine
  contract against it on Linux and macOS, and against TensorFold's newest release every day. The node container image
  installs the pinned release instead of taking a version and SHA-256 at build time.
- `.gitignore` keeps the Compose quick start's generated `secrets/` and `.env` out of commits.
- Security: `github.com/russellhaering/goxmldsig` moves from 1.4.0 to 1.6.1, with `etree` 1.7.0 and `clockwork` 0.5.0, to fix
  GO-2026-4753 (CVE-2026-33487): an XML signature listing more than one reference was checked against the wrong one, so a
  SAML response could be accepted without a valid signature over its contents. Anyone running an earlier commit with a
  SAML connection should update.
- `starlight-node enroll` works as Starlight prints it. With no `--advertise` it sent an empty address, which Starlight
  refused, so every enrolment needed the flag. It now reports the host name and port 7443 until `run` reports its own.
- A release workflow builds every binary, image and chart from a `v*` tag, with checksums and build provenance, and a manual
  run does all of it and publishes nothing. No release has been published yet. Images and the chart will live under
  `ghcr.io/ashhart`, so Compose's local image and the chart's default image now use that name. The node image is built for
  arm64 too, cross-compiles its agent, and carries its version.
- CI runs the README's Compose quick start as written and checks what it starts, including a node that enrols with the
  printed command.
- CI runs `govulncheck` on every pull request, on `main` and daily, for each platform it builds.
