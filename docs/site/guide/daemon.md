# Managing the daemon

You don't need `systemctl`. `fletcher daemon` is a thin wrapper over the service:

```sh
fletcher daemon status
fletcher daemon enable          # start now and on every boot
fletcher daemon restart
fletcher daemon logs            # recent logs; -f to follow
fletcher daemon stop
```

systemd is still the supervisor underneath, handling boot persistence,
crash-restart, and the unit sandbox. These are just friendlier verbs.

## When to restart

Settings apply on the next start, so the common loop is:

```sh
fletcher settings set <key> <value>
fletcher daemon restart
```

## Starting over

To wipe all state and start fresh:

```sh
fletcher daemon stop
sudo rm -rf /var/lib/fletcher
fletcher daemon start
```

This regenerates the age identity, the server WireGuard key, and an empty peer
registry. **All previously-paired devices will need to be re-paired.**
