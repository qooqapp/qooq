# Manual Walkthrough

English | [繁體中文](WALKTHROUGH.zh-TW.md)

*From zero to "push and it deploys itself", with what you should see at every step. About 10 minutes.*

> Want the automated version? `make smoke` (about 1 minute; it runs stages 1 to 3).
> The point of walking through it by hand is to see the intermediate states: build going from `never` to `success`, and how the endpoints change.
>
> For field reference see [DEPLOY-CONFIG.md](DEPLOY-CONFIG.md); for the full operations guide see [USAGE.md](USAGE.md).

## Preparation

```bash
# If running from source, build first (the two repos must be checked out side by side)
make build && make -C ../qooq-cli build
alias qooq=$(cd ../qooq-cli/bin && pwd)/qooq

# If you installed a release, qooq is already on your PATH; skip the above
```

Docker must be running (Docker Desktop / OrbStack open).

---

## Stage 1: Is the daemon alive? (10 seconds)

```bash
qooq up
qooq status
```

You should see:

```
qooqd          :7777  up
qooq-watcher   :7778  up
```

`qooq up` is idempotent: if the daemons are already running, it does not restart them. All state (databases, logs, pids)
lives in `~/.qooq`; no files are left in your current directory.

**If they fail to start**, it prints the tail of the daemon log, and the cause is usually in those few lines.

## Stage 2: Skip git and deploy an image directly (30 seconds)

```bash
qooq component deploy --name hello --image nginx:alpine --port 8082:80
qooq component get hello
```

**Look at the `endpoints:` line.** It is `http://<your LAN IP>:8082`, not `localhost`.
This is deliberate: **a phone on the same network can open that address too**, so try it from your phone.

```bash
curl http://<that address>   # nginx welcome page
qooq component logs hello    # view logs
qooq component rm hello      # clean up
```

## Stage 3: The full GitOps chain (the core)

This stage is what qooq is really for: **you just push, and the deployment happens by itself**.

First make a small Git repo to watch, from the [`static-web`](../examples/static-web/) example (it
already has a `qooq.yaml`). The watcher polls a bare repo, so the working copy pushes to one next to it:

```bash
git clone --depth 1 https://github.com/qooqapp/qooq ~/qooq-src   # skip if you already have the source
cp -R ~/qooq-src/examples/static-web ~/qooq-demo
cd ~/qooq-demo
git init -q -b main && git add -A && git commit -qm "first version"
git clone -q --bare . ~/qooq-demo.git
git remote add origin ~/qooq-demo.git && git push -q -u origin main
```

Then point qooq at it:

```bash
qooq watch add --name demo \
  --repo file://$HOME/qooq-demo.git \
  --component demo-web --poll 10
```

Watch it build. It starts within 10 seconds:

```bash
qooq watch get demo
```

`last_build_status` goes from `never` → `building` → `success`. **If it fails, `last_build_error`
is in the same output**; there is no need to dig through logs.

Once it succeeds:

```bash
qooq component get demo-web
curl http://<address shown in endpoints>
```

### Next comes the step most worth seeing

Change one line of code and **run no deploy command at all**:

```bash
cd ~/qooq-demo
echo '<h1>I changed this</h1>' > index.html
git add -A && git commit -m "test" && git push

# wait 10-30 seconds
curl http://<same address>      # the content has changed
```

The watcher detects the new commit → clones → builds the image → replaces the container. Nobody runs a command along the way.

Clean up:

```bash
qooq watch rm demo            # also removes the demo-web service
rm -rf ~/qooq-demo ~/qooq-demo.git
```

## Stage 4: Use URLs instead of ports

```bash
qooq down
qooq up --ingress            # binds :80
qooq component deploy --name hello --image nginx:alpine --port 8082:80
qooq component get hello
```

`endpoints` now has two more entries:

```
http://hello.localhost, http://hello.192.168.1.50.nip.io, http://192.168.1.50:8082
```

- **`hello.localhost`**: open it directly in a browser with **zero configuration**; no hosts file edits, no DNS setup.
- **`hello.<LAN IP>.nip.io`**: phones and coworkers' computers on the same network can open it, again with no configuration.

If you want https locally (for service workers, the clipboard API, and other things that need a secure context):

```bash
qooq down && qooq up --ingress --ingress-tls --ingress-tls-issuer internal
curl -k https://hello.localhost      # -k because the signer is Caddy's built-in CA
```

**Do not leave out `--ingress-tls-issuer internal`**: the default `acme` goes through Let's Encrypt,
and a public CA cannot issue certificates for `.localhost`, so omitting it means nothing gets set up locally.
The cost of the built-in CA is that every device that connects must trust its root certificate; see
[the TLS section of USAGE.md](USAGE.md#tls) for details.

## Stage 5: Missing values prompt you instead of blowing up

Add this to the `qooq.yaml` of any project:

```yaml
inputs:
  API_KEY:
    description: Third-party API key, get it from the xxx dashboard
    required: true
    secret: true
```

After you push:

```bash
qooq watch get <name>
```

The status is **`needs_input`, not `failed`**, and **no build was run at all**. It lists what is missing,
what it is for, and how to supply it:

```bash
qooq watch set-input <name> API_KEY=abc123      # supply it and it redeploys automatically
qooq watch set-input <name> API_KEY=@key.txt    # read from a file, keeping it out of shell history
```

Secret values are encrypted in the database, and the API always returns `***`.

## Wrapping up

```bash
qooq component ls            # see what is still running
qooq component rm <name>     # remove them one by one
qooq down                    # stop the daemons (deployed components are unaffected)
```

`qooq down` does **not** stop the components you deployed; they are docker containers, not child processes of the daemon.
The next `qooq up` picks them back up automatically.

---

## When you are stuck

| Symptom | Usually |
|---|---|
| `qooq up` says it cannot find qooqd | You did not run `make build`, or the two repos are not checked out side by side (`QOOQ_BIN_DIR` can point to the binaries) |
| `qooq up` says docker is not responding | Docker Desktop / OrbStack is not open |
| `watch get` shows `failed` | Look at `last_build_error` in the same output |
| `watch get` shows `needs_input` | Nothing is broken; a value is missing. See stage 5 |
| The address in endpoints is unreachable | NIC detection guessed wrong (VPN, multiple NICs); add `-advertise-host` when starting the daemon |
| An ingress URL returns 502 | The component is not Running yet, or not yet attached to the ingress network (the next sweep fixes this automatically) |
| Build succeeds but the container dies right after starting, with only a line or two of log | The path the app writes to is not covered by a volume, or `USER <non-root>` lacks permission. See [Files and Volumes in DEPLOY-CONFIG](DEPLOY-CONFIG.md#files-and-volumes) |

For more, see [the FAQ in USAGE.md](USAGE.md#faq).
