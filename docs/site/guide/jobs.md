# Jobs & cron

A **job** is the core primitive: an environment, a command, and a trigger. Run
it once, or give it a schedule and it runs on a cron. Either way the command
runs inside an isolated microVM that's torn down when it finishes.

## Run a one-shot job

```sh
fletcher job create --name build --image fletcher-base --command "make build"
fletcher job get <job-id>
```

`--image` defaults to the `default_image` setting and `--name` defaults to the
command's program name, so for the common case only `--command` is required:

```sh
fletcher job create --command "claude -p 'summarise CHANGELOG.md'"
```

The command runs in a fresh fork of the base image, reaching models only through
the daemon gateway and with no other egress. `job get` shows its status, exit
code, and captured output.

::: tip Output of a failed job
A job's captured output lands in its error field, which `job get` shows. A quick
way to prove a job really ran inside a microVM:

```sh
fletcher job create --name vm-check --image fletcher-base \
  --command "echo KERNEL=\$(uname -r); cat /proc/1/comm; exit 3"
```

The `exit 3` fails the job so its output surfaces. You'll see the guest kernel
version and `fletcher-init` as PID 1.
:::

## Run on a schedule

Give a job `--trigger cron` and a schedule and it runs repeatedly:

```sh
fletcher job create --trigger cron --schedule "*/30 * * * *" \
  --name hourly-scrape --image fletcher-base --command "scrape.sh"
```

A cron job is a **definition**. It shows up with status `scheduled` and a
`next_run_at`. Each time the schedule fires, Fletcher creates a normal **run** of
it. That run is a child job you'll see in `job list`, linked back to its parent,
with its own status, output, and exit code.

The schedule is a standard 5-field expression (`min hour day-of-month month
day-of-week`) or a macro (`@hourly`, `@daily`, `@weekly`, ...).

Behaviour worth knowing:

- A run still going when the next window arrives is **not** double-started.
- A window missed while the daemon was down fires **once** on the next start
  (no backfill).
- Stop a cron job with `fletcher job cancel <id>`.

## The agent-authored-then-automated pattern

You rarely want an agent in the loop on every cron run, because that is slow and
non-deterministic. Instead, have an agent write the script once (an interactive
[session](/guide/sessions) or a one-off job), then schedule the plain program to
run it on a cron. Use an agent-in-the-loop run only when each run genuinely needs
judgement.
