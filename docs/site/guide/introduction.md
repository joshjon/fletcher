# Introduction

Fletcher turns a Linux host you control into a personal cloud, managed through
native iOS and macOS clients. Create persistent VMs, deploy container images,
and manage access, storage and lifecycle from your devices. The CLI provides
local control and automation.

Use a VM as a development environment or an isolated place to run a program.
Deploy an app from a container image and access it privately, or publish it over
HTTPS on your own domain. Coding agents and scheduled tasks are optional uses,
not the reason every VM exists. No agent or model account is needed to start.

VMs and app deployments require Linux with KVM. macOS is a client platform, not
a supported host runtime. Compute and storage remain on your host, but any
external services you use receive the requests you send to them.

## Next steps

1. [Install Fletcher](/guide/installation)
2. [Choose a networking mode and start the daemon](/guide/networking)
3. [Pair your first device](/guide/pairing)
4. [Create a persistent VM](/guide/sessions) or [deploy an app](/guide/deploy)

For optional automation, see [Jobs and cron](/guide/jobs). To run a coding agent
inside an environment, see [Your first agent](/guide/first-agent).
