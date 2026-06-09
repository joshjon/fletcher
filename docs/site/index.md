---
layout: home

hero:
  name: Fletcher
  text: Your own private cloud.
  tagline: >-
    Fletcher makes it simple to spin up isolated virtual machines on hardware
    you own. Run agents, apps, and jobs on them, with nothing leaving your
    network.
  actions:
    - theme: brand
      text: Get started
      link: /guide/introduction
    - theme: alt
      text: Install
      link: /guide/installation
    - theme: alt
      text: GitHub
      link: https://github.com/joshjon/fletcher

features:
  - title: Self-hosted and private
    details: >-
      Everything runs on hardware you own. No cloud account, no metering, and no
      code or data leaving your network.
  - title: Isolated virtual machines
    details: >-
      Spin up hardware-isolated VMs in seconds, each a fast copy-on-write fork
      of a base image. Use one and throw it away, or keep it as a durable
      workspace you SSH into.
  - title: Sandboxed by default
    details: >-
      A new VM has no route to the internet and reaches models only through the
      daemon. Open up egress when a workload needs it, and keep it shut when it
      does not.
  - title: Agents and apps, first class
    details: >-
      Run an agent like Claude Code inside a VM with your keys kept on the host.
      Or deploy a Docker app and serve it over HTTPS, all from one command.
---
