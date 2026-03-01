# Security Policy

## Supported Versions

Currently, only the version 1.x series is being supported. If and when a 2.x
series is released, the support policy will be re-evaluated based on known use
and relative impact of the project.

## Reporting a Vulnerability

Vulnerabilities in Fortsa will be taken seriously. To report vulnerabilities,
please send email to the current maintainer, Steve Scaffidi, at
stephen+fortsa-security@scaffidi.net. Make sure to include as much detail as
possible, including, if available, a means of reproducing the vulnerability.
Contact via email is preferred because we would like the opportunity to resolve
vulnerabilities before disclosure to the public. If you submit a vulnerability
report and don't hear back from the maintainer within one week, it is recommended
to raise an issue on the project. If within another week the vulnerability is not
addressed, it is acceptable to disclose to the public. We do not want to hide
vulnerabilities, we just want a chance to fix them before somebody can exploit them.

## What counts as a Vulnerability?

A vulnerability is classified as having the means to make Fortsa behave in unintended
ways that includes making unintended remote calls, spamming other services with requests,
executing arbitrary or injected code, breaking out of the container sandbox, or other
kinds of undesirable behavior that impact the security and reliability of Fortsa and the
systems Fortsa runs concurrently with, like Istio and Kubernetes and applications running
on a Kubernetes cluster running Fortsa.

For the purposes of this process, a vulnerability does not include things like use
of libraries that are out-of-date. Please raise a PR for those sorts of issues.
