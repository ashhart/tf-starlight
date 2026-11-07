# Changelog

## Unreleased

- [`engine.json`](engine.json) pins TensorFold 1.2.4 (from 1.2.3), with its three archives' SHA-256 digests. CI checks the engine
  contract against it on Linux and macOS, as before.

## 0.1.0-rc.2

The second pre-release, and the first with a GitHub release: it holds everything listed under 0.1.0-rc.1. Like that one,
it is not qualified for production: [docs/qualification.md](docs/qualification.md#open-gates) lists the gates still open.

- The release workflow logs in to GHCR with Docker's login before it records the chart's build provenance. 0.1.0-rc.1
  published its images and chart, but that step found no credentials, so it had no GitHub release.

## 0.1.0-rc.1

The first pre-release. It is not qualified for production: [docs/qualification.md](docs/qualification.md#open-gates) lists
the gates that still need real hardware, identity tenants or an independent review.

- Starlight moves here from [ashhart/TensorFold](https://github.com/ashhart/TensorFold), where it landed in
  ashhart/TensorFold#1922, with its history. The Go module is now `github.com/ashhart/tf-starlight`.
- [`engine.json`](engine.json) pins TensorFold 1.2.3, the engine release Starlight is tested with. CI checks the engine
  contract against it on Linux and macOS, and against TensorFold's newest release every day. The node container image
  installs the pinned release instead of taking a version and SHA-256 at build time.
- `.gitignore` keeps the Compose quick start's generated `secrets/` and `.env` out of commits.
- Security: `github.com/russellhaering/goxmldsig` moves from 1.4.0 to 1.6.1, with `etree` 1.7.0 and `clockwork` 0.5.0, to fix
  GO-2026-4753 (CVE-2026-33487): an XML signature listing more than one reference was checked against the wrong one, so a
  SAML response could be accepted without a valid signature over its contents. Anyone running an earlier commit with a
  SAML connection should update.
- A deployment can set `ssd_experts_gib` and `ple_on_ssd`, which let Flash Next run on a Mac with less memory than its
  checkpoint needs, and `learn_conversations` with `learn_gib` and `learn_daily_gib`, which keep conversations on disk
  so they resume after a restart. [docs/fleet.md](docs/fleet.md#deployments) has the backends and bounds of each. A
  replica's saved conversations stay in its directory on the node and leave with it.
- An agent that starts after a crash removes the directories of replicas it is no longer assigned, and stopping the
  agent waits for each replica's directory to be cleared.
- `starlight-node enroll` works as Starlight prints it. With no `--advertise` it sent an empty address, which Starlight
  refused, so every enrolment needed the flag. It now reports the host name and port 7443 until `run` reports its own.
- A release workflow builds every binary, image and chart from a `v*` tag, with checksums and build provenance, and a manual
  run does all of it and publishes nothing. Images and the chart live under `ghcr.io/ashhart`, so Compose's local image and
  the chart's default image now use that name. The node image is built for arm64 too, cross-compiles its agent, and carries
  its version.
- A standby instance checks the controller lease every second, not every 5, so a new leader takes over within about 15
  seconds of losing the old one, where it could take 20.
- CI kills and pauses a real controller leader and restores a backup into a new database, as the high availability and
  recovery gate asks. That gate is closed.
- Windows nodes: a `path` artifact can name a Windows path. The state directory's ACL admits only SYSTEM and Administrators,
  where an administrator running any `starlight-node` command used to add their own account. The service logs to
  `starlight-node.log` in the state directory, where its log used to go nowhere.
- CI runs on Windows: the node agent and Connect suites, fleet tests, `install-windows.ps1` and the service it installs, the
  state directory's ACL, Credential Manager, the shipped Group Policy values and the tray.
- CI checks Connect's stores on Linux: GNOME Keyring through the Secret Service, and the private file where there is none.
- CI runs the README's Compose quick start as written and checks what it starts, including a node that enrols with the
  printed command.
- Security: post-login redirects refuse anything starting with two slashes, and the cookies that set and clear a console
  notice carry `Secure` whenever the session cookie does, including behind a load balancer that terminates TLS. CodeQL's
  first run found both; `http.Redirect` already kept the first from leaving the site.
- CI runs `govulncheck` on every pull request, on `main` and daily, for each platform it builds, and CodeQL for Go on every
  pull request, on `main` and weekly.
- Dependabot proposes weekly updates to Go modules, Actions and container images, grouped so they stay few.
