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

In scope: reading files outside a shared directory, bypassing authentication, forging or
replaying tokens, crashing the server with crafted requests, and the client serving wrong
file contents.

Out of scope: the replication tool that keeps nodes in step, and the reverse proxy in front
of a node.
