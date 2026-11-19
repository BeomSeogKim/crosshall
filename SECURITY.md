# Security Policy

## Supported versions

v0.1.0 is the first release. The latest release is the only supported version
until stated otherwise here.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately, by either of:

- GitHub's private vulnerability reporting on this repository (the
  "Report a vulnerability" button under the Security tab), or
- email to **kbs4520@daum.net** with the subject starting `[security]`.

Include what you found, how to reproduce it, and what an attacker could do
with it. You will get an acknowledgement, and a fix or a decision is
coordinated with you before anything is published.

## Scope

The tool is designed to run locally for a single operating-system user and
treats other processes under that account as cooperative. Reports that matter
most:

- a way for message content to be executed rather than treated as data;
- a way for one agent session to send as another, or as the owner;
- a way for messages to cross a channel boundary;
- anything that writes to a terminal's input.
