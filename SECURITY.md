# Security policy

## Reporting a vulnerability

Do not open a public issue for a security problem.

Report it privately through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability**. Include the version, a description of the problem and, if you can,
the steps to reproduce it.

You can expect an acknowledgement within five working days. Once a fix is released, the
advisory is published and the reporter is credited unless they ask not to be.

## Supported versions

Until 1.0.0, only the latest release receives security fixes.

## Scope

In scope: the client serving wrong file contents.

Problems in the server (reading files outside a shared directory, authentication, tokens,
crafted requests) are reported the same way in the
[server repository](https://github.com/SpliceFS/server).

Out of scope: the replication tool that keeps nodes in step, and the reverse proxy in front
of a node.
