# Changelog

## Unreleased

- Starlight moves here from [ashhart/TensorFold](https://github.com/ashhart/TensorFold), where it landed in
  ashhart/TensorFold#1922, with its history. The Go module is now `github.com/ashhart/tf-starlight`.
- [`engine.json`](engine.json) pins TensorFold 1.2.1, the engine release Starlight is tested with. CI checks the engine
  contract against it on Linux and macOS, and against TensorFold's newest release every day. The node container image
  installs the pinned release instead of taking a version and SHA-256 at build time.
- `.gitignore` keeps the Compose quick start's generated `secrets/` and `.env` out of commits.
