---
name: qooq-deploy
description: Deploy the project the user just finished writing to their local machine with qooq, and hand back a URL that works — one project, or several wired together. Use when the user says "deploy this", "get this running", "can we see this in a browser", "deploy these together", 「幫我部署這個專案」, 「把這個跑起來」, 「把這幾個一起跑起來」, or when a runnable project has just been written and they want to see it live. Detects the project type, writes qooq.yaml (and qooq.stack.yaml for a group), starts qooqd/watcher, deploys, and verifies the endpoint for real. When the user is signed in to qooq Cloud (or asks to deploy there), the same skill ships the project to qooq Cloud instead, by uploading the directory directly (no git needed) or from a GitHub repo.
---

# qooq-deploy: get a local project running in one pass

The user just finished a project and wants to see it work. This skill: detect the type →
write the deployment config → make sure qooq is up → deploy → **verify by actually calling
it** → report a working URL.

Report to the user in whatever language they are writing in.

## Two modes

| Mode | When | What it does |
|---|---|---|
| **GitOps** (default) | Normally | Creates a local bare repo; the watcher polls it and redeploys. From then on the user commits and pushes as usual: a pre-push hook also updates the `qooq` remote, so `git push` (to origin) redeploys too |
| **One-shot** | The user says "just testing", "don't touch git", or the project should not be in version control | `qooq deploy .` uploads the directory — no git; run it again to redeploy |

When unsure, use GitOps. Automatic redeployment is the main thing qooq is for.

> [!TIP]
> **If you have `qooq_*` tools (the qooq MCP server), use them instead of shelling out**:
> `qooq_status` / `qooq_up` for step 4, `qooq_deploy` (absolute project path) for one-shot
> mode, `qooq_build_log` when a build fails, `qooq_logs` when the app runs but misbehaves,
> `qooq_secret_import` for `.env` keys. Everything else in this skill still applies — above
> all, open the returned URL yourself before reporting success.

A third target exists: **qooq Cloud** (details in `~/.claude/skills/qooq-deploy/reference/cloud.md`).
Pick it before anything else with:

```bash
~/.claude/skills/qooq-deploy/scripts/cloud-deploy.sh detect
```

| `detect` prints | Do |
|---|---|
| `CLOUD=ready` | The user is signed in to qooq Cloud. **Deploy to qooq Cloud**, unless they said "locally" / "on my machine" |
| `CLOUD=not_signed_in` | Local paths as usual. **Only if the user asked for qooq Cloud / "put it online"**: tell them to run `qooq auth login` (it opens a browser, they must finish it), then re-run `detect` |
| `CLOUD=no_project_command`, `no_cli`, `unreachable` | Local paths as usual. Say why only if the user asked for the cloud |

If `detect` says `ready` but the request is plainly local ("just testing on my laptop"),
ask once which they want — a cloud deployment is visible to others.

## First settle the scope: one project, or several wired together

qooq holds two kinds of deployment side by side, told apart by **namespace**:

| | namespace | when |
|---|---|---|
| **Standalone** | `default` | the ordinary case: one app, deployed on its own |
| **A stack** | the stack's name | several projects that must reach each other, deployed in dependency order from one `qooq.stack.yaml` |

Look at what the machine already has before choosing:

```bash
~/.claude/skills/qooq-deploy/scripts/survey.sh
```

Then:

- **One project** → standalone; go to step 1. If it needs to reach something already
  deployed, see `~/.claude/skills/qooq-deploy/reference/multi-project.md` — that does
  **not** require a stack.
- **Several projects named at once**, or a monorepo with several deployable parts →
  `~/.claude/skills/qooq-deploy/reference/multi-project.md`.
- **Two projects already deployed separately that now need to work as one system** →
  the same section, but read the warning below first.

> [!IMPORTANT]
> **Which apps form one system is the user's knowledge, not something to infer from the
> code.** Two repos that import nothing from each other can still be one product, and two
> that share a library can be unrelated. Guessing is punished quietly: every container
> runs, every health check stays green, and only the browser breaks.

**When the survey shows something already deployed and the user has not said how the new
project relates to it, ask before writing any config** — as a multiple-choice question, not
as prose. The host renders the options as a list the user picks from, and the difference
matters here: "do you want to wire these together?" can only be answered by someone who
already knows what wiring means, while options carrying their consequence can be answered
by someone who does not.

Ask it once, in the user's language. **`newsapi` and `newsweb` in both tables below are
placeholders — substitute the names you actually found, never copy these.** The reader is a
vibe coder: say what they will see, not what qooq will do.

**The two tables say the same thing in two languages. Changing one without the other is a
bug** — they are adjacent so that a diff shows both.

> **`newsweb` 要怎麼跑起來？**（三種都跑在你這台電腦上，網址長得一樣、只差最後的號碼。
> 真正的差別是 `newsweb` 找不找得到 `newsapi`）
>
> | | |
> |---|---|
> | **分開跑，但能互通** | 多數情況選這個。兩個各自更新、互不打擾，`newsweb` 照樣抓得到 `newsapi` 的資料 |
> | **綁成一組** | 跟上面一樣能互通，另外多兩件事：`newsapi` 好了才會換 `newsweb`，而且一個指令管兩個。東西少的時候用不太到 |
> | **各跑各的，不互通** | 只有兩個東西真的無關時才選。**你這份程式碼選了會壞**——畫面會顯示連不上 |

> **How should `newsweb` run?** (All three run on this machine, at the same address with a
> different number on the end. What actually differs is whether `newsweb` can find `newsapi`.)
>
> | | |
> |---|---|
> | **Separate, but talking to each other** | Pick this one unless you know otherwise. You can update either one without disturbing the other, and `newsweb` still gets its data from `newsapi` |
> | **Tied together as one group** | They talk to each other same as above, plus two things: `newsapi` has to be up before `newsweb` is swapped in, and one command handles both. Not worth it when there are only a couple of them |
> | **Fully separate, not talking** | Only if the two really have nothing to do with each other. **Your code will break if you pick this** — the page will say it cannot connect |

Map the answer straight onto the three paths: a stack, a standalone deployment with
`${component....}` references, or a plain standalone deployment. **Only one project on the
machine? Do not ask** — there is nothing to wire to, and a question with one real answer
just costs the user a turn.

> [!WARNING]
> **Moving an already-deployed project into a stack is a move, not a setting.** A stack
> owns its projects' namespace, so `default/api` becomes `<stack>/api` — a **different
> component**, with a different address. The old standalone target has to go first:
>
> 1. `$QOOQ_CLI watch rm <target>` — the old standalone target, its container, **and its
>    data**
> 2. then create the stack
>
> **If the old project keeps data — `services:` (a database) or `run.volumes:` — ask the
> user before step 1**: the stack starts empty. Step 1 deletes the old data, and none of it
> moves to the new namespace. Say so plainly and let them decide (export the data first,
> or accept starting fresh).
>
> Skip step 1 and the stack refuses the project, with `last_error` naming the target that
> already holds it. That is the intended outcome, not a bug to work around: two targets on
> one repository build and deploy independently, and both report success. **Do not reach
> for `--allow-duplicate` to get past it** — that flag exists for deliberately running the
> same app twice (staging beside production), and using it here produces exactly the mess
> the check is there to prevent.
>
> Tell the user before starting: **the app is redeployed and its address changes.** Anything
> pointing at the old address (another app's config, a bookmark, a CORS allowlist) breaks,
> and it breaks far from here.

## Steps

### 1. Settle the project location and name

The component name defaults to the project directory name (lowercased, non-alphanumerics
replaced with `-`). If a component with that name is already running, reuse it — that is a
redeployment of the same app, not a second one.

**The component name must be a lowercase DNS label**: only `a-z`, `0-9`, `-`, not starting
or ending with `-`, at most 63 characters. Directory names like `_tmp` or `2024.專案`
produce something illegal (`-tmp`, `2024--`), so trim the leading and trailing `-` yourself
— an illegal name is rejected at deploy time, not before. The name becomes the container's
DNS alias on the network, which is how other components address it.

### 2. Detect the project type

Read `reference/detect.md` and follow the table, then confirm by **actually reading the
entry point source**:

- which container port the app listens on
- whether there is a usable health endpoint (if you cannot find one, do not set one)
- whether it needs environment variables to start at all (especially projects that read `PORT`)

- whether the code reads an environment variable whose value is nowhere in the repo
  (`process.env.X`, `os.Getenv("X")` with no `.env`/`.env.example` providing it) — that is
  an `inputs:` declaration (see step 5-A), not a value for you to invent

> [!NOTE]
> If the project needs a database or cache, **declare it under `services:`** (step 5-A):
> `postgres`, `mysql`, `mariadb`, `redis` and `mongo` are built in — qooq starts it, keeps
> its data across redeploys, waits until it is healthy, and hands the app `DATABASE_URL`
> and friends. Do not ask the user for a database they would have to run themselves.
> A third-party service (Stripe, OpenAI, an external API) is not a reason to stop either:
> keys already in the project's `.env` are imported as secrets; keys nobody has given you yet
> are declared under `inputs:`, which is how the user gets asked for them (both in step 5-A).
> **Stop and tell the user** only when the app needs a database or server qooq cannot
> start — a type not in that list.

### 3. Let the daemon assign the host port

Write `auto` on the host side and do not pick a number yourself:

```yaml
run:
  ports:
    - "auto:8080"      # 8080 is the port the app listens on inside the container
```

One-shot mode reads the same `qooq.yaml`, so this applies there too.

qooqd allocates from 19000-19999 while holding the whole-table view, so there is no window
between choosing and deploying for someone else to take it. **A component keeps the same
host port across redeploys** — the match is on (container port, protocol), so reordering or
adding ports in the manifest does not shuffle the addresses already handed out.

You will not know the number until the deployment finishes; step 6 prints it. Do not write
the host port into `run.env` or a config file — reference the component by name instead
(see `~/.claude/skills/qooq-deploy/reference/multi-project.md`).

### 4. Prepare the environment

```bash
~/.claude/skills/qooq-deploy/scripts/qooq-env.sh    # both modes
```

Checks docker, finds the qooq CLI — installed on PATH, or a source checkout, or by
installing it — and brings up whichever daemon is down (via `qooq up`, so the state
lands in `~/.qooq` like it would if the user ran it themselves).

`BLOCKED` means something it cannot fix by itself: docker not running, or no way to
obtain qooq. Relay the message it printed and stop; do not push on. The message names
the fix in both cases.

### 5-A. GitOps mode

**First check for anything that should not go into version control** (compiled binaries,
`node_modules/`, `.env`, large data files). If there is no `.gitignore`, or it does not
cover them, add it and tell the user what you added.

> [!CAUTION]
> Never commit the user's secrets (`.env`, key files). If you find one, deal with it
> before going any further.

**The app's keys are in `.env`?** `.env` is never committed or uploaded, so the deployed app
would start without them. Store them once — `$QOOQ_CLI secret import <project-dir>/.env` —
and reference each one in `qooq.yaml` as `run.env: {STRIPE_KEY: "${secret.STRIPE_KEY}"}`.
Use `inputs:` only for values that exist nowhere yet and the user has to supply. Both end up
in `run.env`; the only question is where the value comes from — already in `.env` →
`${secret.X}`, not there yet → `inputs:` and `${input.X}`.

Write `qooq.yaml` in the project root (field reference: `$QOOQ_TREE/docs/DEPLOY-CONFIG.md`):

```yaml
version: 1
app: <component-name>
description: <one line>

# network: <shared network name>   # only when it must reach another deployed component;
                                  # see ~/.claude/skills/qooq-deploy/reference/multi-project.md

requests:            # omitting limits means limits are 2x requests
  cpu: 0.25
  memory: 256Mi

build:
  template: go       # per detection; if the project has its own Dockerfile, use
                     # build.dockerfile: docker/Dockerfile instead
                     # (**look in docker/ before deciding**)

# services:          # only if the app genuinely needs a database; one line gets defaults
#   db:
#     type: postgres

# inputs:            # only for values that just the person deploying can know
#   STRIPE_KEY:
#     description: Stripe secret key
#     required: true
#     secret: true

run:
  env:
    PORT: "8080"     # only if the app reads PORT
  ports:
    - "auto:8080"    # host side is always auto; 8080 is the container port
  files:             # only if the app reads a config file from its cwd
    - "./config.toml:/app/config.toml"
  volumes:           # only if the app writes files, otherwise data resets on redeploy
    - "appdata:/app/data"

health:              # only when you are sure the endpoint exists
  http: /health
  port: 8080         # the **container** port, the same number as in run.ports
```

**`health.port` must be one of the ports in `run.ports`.** With `auto:` that means the
container port — qooq then probes whichever host port it assigned. Naming a port the
component does not declare is rejected at deploy time, which is deliberate: a probe that
lands on a port some other component happens to hold reports a broken app as healthy.

`qooq init` writes a first draft of this file from the same detection the watcher uses, and
`qooq manifest validate qooq.yaml` checks it without needing a daemon — worth running before
committing, because otherwise a typo only surfaces after commit → push → poll → clone.

With `services:` declared, the app automatically gets `DB_HOST`/`DB_PORT`/`DB_USER`/
`DB_PASSWORD`/`DB_DATABASE`/`DATABASE_URL`; a framework that wants different variable names
can take them from `${service.db.host}` and friends (full list in
`$QOOQ_TREE/docs/DEPLOY-CONFIG.md`).

**When a value is not in the repo and should not be: declare it as an `inputs:` entry.**
Do not guess it and do not leave it blank. Typically API keys, third-party credentials, and
public-facing URLs.

```yaml
inputs:
  STRIPE_KEY:
    description: Stripe secret key   # the user reads this to know where to get it — required
    required: true                   # blocks the deploy until it is supplied
    secret: true                     # stored encrypted, API returns ***
  PUBLIC_URL:
    description: URL a browser uses to reach this app
    default: http://<component-name>.localhost   # the ingress address; a default means the user is never asked
run:
  env:
    STRIPE_SECRET_KEY: "${input.STRIPE_KEY}"
    PUBLIC_URL: "${input.PUBLIC_URL}"
```

The test for "should this be an input": **would this value differ on another machine?**
If yes (keys, public addresses, domains) it is an input; if no (ports, paths, modes)
hard-code it.

Values you already know can be supplied when creating the target, so nothing stops to ask:

```bash
$QOOQ_CLI watch add ... --input PUBLIC_URL=http://shop.localhost:8080
```

`${host}` is the outward-facing address qooqd detected, so it saves asking the user for
their IP. `${env.NAME}` still works, but it reads the watcher process's environment and is
an operator escape hatch — **values the app needs always go through `inputs:`**, because
that is the only path that tells the user what is missing and what it is.

Wire up local GitOps and register the target:

```bash
~/.claude/skills/qooq-deploy/scripts/gitops-setup.sh <project-dir> <component-name>
# output includes BARE=<bare repo path>

$QOOQ_CLI watch add --name <component-name> \
  --repo file://<BARE> --component <component-name> --poll 30 \
  --watcher-addr http://127.0.0.1:7778
```

If a target of that name already exists, **do not remove it.** `watch rm` deletes the
deployment *and its data* — a `services:` database is wiped with it. An existing target
with this name is almost always the same project being deployed again, and that needs no
new target: run `gitops-setup.sh` again (it commits and pushes the current files), then
go straight to step 6 with `--trigger`. Check `$QOOQ_CLI watch get <component-name>`
first: only if its repo is a *different* project, stop and ask the user — either pick
another component name, or have them confirm removing the old one together with its data.

### 5-B. One-shot mode

```bash
cd <project-dir> && $QOOQ_CLI deploy .     # prints `deployed: <urls>` when it is running
```

It packs the directory (honouring `.gitignore`, never `.env`), uploads it, and the watcher
builds it exactly as in GitOps mode — `qooq.yaml` is used if present, detected otherwise —
then waits for the build. Run it again after a change to redeploy; unchanged content is not
rebuilt. `--name` sets the service name (default: `app:` in qooq.yaml, else the directory).
On failure the output ends with the build's last lines; `$QOOQ_CLI watch build-log <name>`
has all of it.

### 6. Wait and verify (**not optional**)

```bash
# GitOps
~/.claude/skills/qooq-deploy/scripts/wait-and-verify.sh <component-name> --target <component-name> --trigger --path /health
# One-shot
~/.claude/skills/qooq-deploy/scripts/wait-and-verify.sh <component-name> --path /health
```

`--path` is a page the app really serves: its health endpoint if it has one, otherwise `/`.

> [!IMPORTANT]
> **In GitOps mode, pass `--trigger` and let the script start the build.** Do not run
> `watch trigger` yourself and then call this without it. A target that has built before
> still reports `last_build_status: success` and its old revision for the first seconds
> after a trigger, and the old container is still the one running — so a script with no
> baseline reads all of that as "finished" and reports the *previous* deployment. The
> failure that produces is the confusing kind: it prints the old image and the old
> settings, which looks exactly like the new manifest never took effect.
> `--trigger` reads the build generation first, so it knows which build it is waiting for.
> Add `--force` to rebuild when the remote has not changed. Without it, an unchanged remote
> prints `NOTHING_TO_BUILD` within seconds and the script verifies the version already
> running (exit 0) — so if you just changed something, check it was committed and pushed.

Any line starting with `FAIL:` means it did not work, and the exit code is 1. When the
app itself is the problem the script ends with `--- last output of <name> ---`: read it
and fix the code before anything else — it is usually the whole answer (a stack trace).

**A new version that does not start is rolled back automatically.** If it exits, crash-
restarts, or never passes its health check, the watcher puts the previous image back, marks
the deploy `failed` with the reason and the crash output, and does not redeploy that commit
on its own. Tell the user their last good version is serving again (after a gap of a few
seconds while the broken one was tried — the two cannot share the host port, so there is no
zero-downtime swap yet), show the error, fix it, commit and push — a new commit deploys normally.
`qooq watch get <name>` shows the failed revision and its reason (`last_build_revision`, `last_build_error`).

Outputs `ENDPOINT_LAN` / `ENDPOINT_LOCAL` / `BODY`. **Do not claim success when it fails** —
the script prints the build failure or `last_error`; read it, fix the cause, retry.

`ENDPOINT_LAN` is computed by qooqd itself (it is the one that knows where the container is
bound and what this machine's outward address is), and the script probes that address rather
than localhost — what has to be verified is that the URL you report actually works.

> [!IMPORTANT]
> **Exit code 2 means `BUILD: needs_input`. That is not a failure, it is a question.**
> **If it lists no `MISSING_INPUT` at all, there is nothing to ask for.** That is this
> project waiting for another component to finish deploying, which resolves itself on the
> next sweep — `watch get <target>` names who it is waiting for. Wait, or
> `watch trigger <target>`, and re-run. Asking the user for a value here produces a
> question with no answer. Two projects that reference each other's address also resolve
> on their own: each one's address is reserved before its env is resolved, so the second
> one deploys and the first follows on the next sweep. Only if `last_build_error` starts
> with `circular reference` is it stuck (usually a qooqd older than the watcher): tell the
> user which projects form the loop, and for the first deploy give one of them a fixed
> value instead of the `${component...}` reference, then put it back and redeploy.
>
> Otherwise the script prints `MISSING_INPUT=<name> | <description> | e.g. <example>`. Then:
> 1. Ask the user for each one **using the description it printed** (do not invent a value,
>    do not skip it)
> 2. `$QOOQ_CLI watch set-input <target> NAME=value` (for secrets use `NAME=@file`, which
>    keeps it out of shell history)
> 3. Re-run the script
>
> If the user cannot answer (they have not obtained the key yet), report where it is stuck
> and where to get the value. Do not deploy with a placeholder — the app will start and
> then break somewhere else, which is far harder to trace than being stopped here.

Common failures:

| Symptom | Cause | What to do |
|---|---|---|
| build failed, message mentions the Dockerfile | template variables do not fit | read the template, adjust `template_vars` (for example `start_cmd`) |
| Running but curl does not connect | the app's port ≠ the container port | read the source for the real listen port, fix `run.env.PORT` or the template's `port` |
| restarting repeatedly / restart_count climbing | wrong health path, or a missing dependency | drop the `health:` block to confirm the app itself runs, then add the correct path |
| 409 port conflict | a host port was written by hand instead of `auto:` | change `run.ports` to `auto:<container port>` |

### 7. Report

Always include **the URL you actually tested** and **the response you saw**, so the user can
confirm it at a glance:

```
✅ <component-name> deployed

  LAN:    http://192.168.1.51:19000    ← other devices on the network reach this too
  Local:  http://127.0.0.1:19000
  Tested /health → {"status":"ok"}

After changing code:
  git add -A && git commit -m "..." && git push
  redeploys within 30s (qooq watch get <component-name> shows progress)
  a version that fails to start is rolled back; the previous one keeps serving
```

In one-shot mode, the last part becomes "run this skill again after changing code".
If `gitops-setup.sh` printed `SKIPPED: pre-push hook`, a plain `git push` does **not**
redeploy (the project has its own hooks): write `git push qooq` in the report instead, and say why.

Also mention the things they may not have noticed: which files were added (`qooq.yaml`,
`.gitignore`), that a git remote named `qooq` was added
(**origin was not touched**), and that the daemons were started just now.

## When to read the reference files

These are kept out of this file so it stays short; read one only when its case applies.

| Read | When |
|---|---|
| `~/.claude/skills/qooq-deploy/reference/cloud.md` | `cloud-deploy.sh detect` printed `CLOUD=ready` and the deploy goes to qooq Cloud |
| `~/.claude/skills/qooq-deploy/reference/multi-project.md` | several projects deployed together (`qooq.stack.yaml`), or this project must reach one that is already deployed (`${component.x.endpoint}`, shared networks) |
| `~/.claude/skills/qooq-deploy/reference/detect.md` | detection is unclear: health checks, cgo, port and template details |

## Boundaries and known limits

- **Local paths are local only** (the qooq Cloud path above is the exception: its URL is public). The address handed back is a LAN address: other devices on the same
  network reach it, the public internet does not. When the daemon runs with `-ingress`,
  `endpoints` will be `http://<name>.localhost` and `http://<name>.<LAN IP>.nip.io`
  (still LAN only) — **just report what the API returns**, you do not have to work out
  whether ingress is on. Going public additionally needs a domain and TLS; see
  `$QOOQ_TREE/docs/USAGE.md`, "用網址取代 port".
- **The user's origin remote is never touched.** Local GitOps always uses a remote named `qooq`.
- **Requires docker** (unless the project uses the process runtime, which this skill does not cover).
- Frontend-only projects have to be built first; see `reference/detect.md`.

## Path variables

The scripts read these, with defaults when unset:

| Variable | Default | What it is |
|---|---|---|
| `QOOQ_CLI` | *(found)* | the qooq binary; normally discovered, set it to override |
| `QOOQ_TREE` | `$CLAUDE_PLUGIN_ROOT` (Claude Code plugin installs only), else `$QOOQ_SRC` | qooq's own source tree, for reading its docs and templates. **May not exist**: someone who installed the binaries has no copy of the repository, and no agent other than Claude Code sets a plugin root. When it is missing, read the references at <https://github.com/qooqapp/qooq/tree/main/docs> instead of guessing — do not go looking for a checkout |
| `QOOQ_SRC` / `QOOQ_CLI_SRC` | `~/projects/qooqapp/qooq`, `-cli` | source checkouts, only used on a machine developing qooq |
| `QOOQ_INSTALL_PREFIX` | `~/.local/bin` | where qooq gets installed when the machine has none |
| `QOOQ_HOME` | `~/.qooq` | db, logs and bare repos all live here |

`qooq-env.sh` finds the CLI in this order: `QOOQ_CLI` → **`qooq` on PATH** → a built
source checkout → install it with the repository's `install.sh`. Most machines stop at
the second; only the machine qooq is developed on reaches the third.

> `QOOQ_SRC` is deliberately not called `QOOQ_REPO`: `install.sh` already uses that name
> for the GitHub repository to download from, and one name meaning two things is how a
> filesystem path ends up in an error message about a missing release.
