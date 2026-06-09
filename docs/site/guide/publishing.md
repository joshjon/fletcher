# Publishing ports

A [session](/guide/sessions) can serve something, such as a dev server, a web
app, or an API. Fletcher exposes that port two ways, both brokered by the daemon
so the VM itself never gets a network route (the same trust boundary as SSH).

## To your paired devices, over the tunnel

No setup, nothing public:

```sh
fletcher session exec dev "nohup python3 -m http.server 8000 >/tmp/log 2>&1 &"
fletcher session publish dev 8000        # prints a tunnel address like 10.99.0.1:41xxx
fletcher session ports dev               # list what's published
```

Any device on your WireGuard tunnel can reach that address. Stop it with:

```sh
fletcher session unpublish dev 8000
```

## To the public internet, over HTTPS

Fletcher can serve a published port publicly over HTTPS on a domain you own,
terminating TLS at the daemon and reverse-proxying into the VM. This is
**opt-in**, because it opens ports 80/443 on your box, and it needs a real public
IP.

Because it changes what your box exposes to the internet, it has its own setup.
See **[Public web over HTTPS](/advanced/public-web)** for the full walkthrough:
enabling `public_web`, the DNS record, certificates, and the trust model.

The short version, once `public_web` is enabled:

```sh
fletcher session publish dev 8000 --public --host app.example.com
```

Fletcher prints the exact DNS record to create and obtains a Let's Encrypt
certificate on the first request.

## Just want to ship an app?

If your goal is to run a containerised app and publish it rather than manage a
session by hand, use [`fletcher deploy`](/guide/deploy), the one-command path
from a Docker image to a published app.
