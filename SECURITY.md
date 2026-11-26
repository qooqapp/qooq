# Security policy

English | [繁體中文](SECURITY.zh-TW.md)

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately instead:

- **Email:** qooqapp1010@gmail.com
- or GitHub's **Report a vulnerability** button under this repository's *Security* tab

Include what you found, how to reproduce it, and the version (`qooq --version`, `qooqd -version`).
You will get a reply acknowledging the report; once a fix is released, the report can be made public.

## Scope

This covers the code in [qooqapp/qooq](https://github.com/qooqapp/qooq) (qooqd, qooq-watcher, the
deployment skill, `install.sh`) and [qooqapp/qooq-cli](https://github.com/qooqapp/qooq-cli).

Deploying a project runs its code on your machine, so deploying an untrusted project is not by
itself a vulnerability — see the security model in [ARCHITECTURE](docs/ARCHITECTURE.md#security-model).

## Supported versions

Fixes go into the latest release only.
