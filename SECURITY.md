# Security

## Reporting a vulnerability

Report security problems privately: open this repository's Security tab and choose "Report a vulnerability"
([a new advisory](https://github.com/ashhart/tf-starlight/security/advisories/new)). Please don't open a public issue or
pull request for a problem that isn't fixed yet. You'll get a reply within three days, and a fix or a mitigation lands
as soon as it is gated, within two weeks for anything serious.

## Supported versions

Starlight has no releases yet; fixes land on `main`.

## What is in scope

- `starlight`: the gateway, the control plane, the console, single sign-on, SCIM and the metrics endpoint.
- `starlight-node`: enrolment, the agent's API and how it runs TensorFold engines.
- `tensorfold-connect`: desktop sign-in, the local endpoint and the tray app.

Problems in the engine itself fall under [TensorFold's security policy](https://github.com/ashhart/TensorFold/security/policy).
[docs/security.md](docs/security.md) has Starlight's threat model.
