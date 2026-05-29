# Changelog

## [0.2.1](https://github.com/portflare/server/compare/v0.2.0...v0.2.1) (2026-05-29)


### Features

* add admin abuse report workflow ([56220e6](https://github.com/portflare/server/commit/56220e68193d715c406af60e45ea7b76d2b46a69))
* add per-app served-by override policy ([6257693](https://github.com/portflare/server/commit/625769322b1de81fa7cd6412e61c8676a3e16e55))
* add public Portflare learn-more page ([eeb95ab](https://github.com/portflare/server/commit/eeb95ab99c6bfebeb5a25bbec7169117c83974bb))


### Bug Fixes

* harden abuse report intake ([8495f3c](https://github.com/portflare/server/commit/8495f3c94aca5c20cb7bab8a471cf881b2285874))

## [0.2.0](https://github.com/portflare/server/compare/v0.1.1...v0.2.0) (2026-05-13)


### ⚠ BREAKING CHANGES

* server configuration now uses PORTFLARE_* environment variables instead of REVERSE_* and the server binary is now portflare-server.

### Features

* rename server brand and environment variables ([6bb4e6e](https://github.com/portflare/server/commit/6bb4e6ee66e6a512fde15f26ac13093a389bdaed))
* **server:** add interval traffic stats store ([5d4572b](https://github.com/portflare/server/commit/5d4572b54bd30a9ac58bac1345f99facd9db59e6))
* **server:** add public registration endpoint ([022445e](https://github.com/portflare/server/commit/022445e47f8100c4b6603e0cfe2f59081eb9453d))
* **server:** add readyz build metadata endpoint ([50173ad](https://github.com/portflare/server/commit/50173adb2ea5fc45994f55283da70af289267a85))


### Bug Fixes

* route app subdomains before user pages ([db35ee6](https://github.com/portflare/server/commit/db35ee6bc5ba82128f7ba1c14c41f14cd0307f2a))

## [0.1.1](https://github.com/portflare/server/compare/v0.1.0...v0.1.1) (2026-04-22)


### Features

* scaffold standalone server repository ([f3365eb](https://github.com/portflare/server/commit/f3365ebc59527d45266961d5a841b0b1315a8c79))

## 0.1.0

- initial split from monorepo
