# Deploying to qooq Cloud

Read this only when `cloud-deploy.sh detect` printed `CLOUD=ready` and the user wants
qooq Cloud (see the table at the top of SKILL.md).

qooq Cloud gives the service a public address and gets the code one of **two ways**:

- **Upload**: packs the directory as it is on disk and sends it. No git, no GitHub.
  Nothing redeploys by itself; every change is another upload. (`cloud-deploy.sh upload`
  wraps the CLI's `qooq deploy --cloud`; use the script, it reports the same way as `ship`.)
- **GitHub**: qooq Cloud builds from a GitHub repo, and every `git push` redeploys.

The steps all go through `~/.claude/skills/qooq-deploy/scripts/cloud-deploy.sh`; exit codes
are `0` ok, `1` failed, `2` needs the user's input, `3` stop and ask the user (or have them
do something).

1. **Config.** Detect and write `qooq.yaml` exactly as in the local **Steps** section's
   2, 3 and 5-A above (container port, `auto:` host port, `inputs:` for values only the
   user knows; never commit a `.env`). Run `qooq manifest validate qooq.yaml`.
2. **Pick the way in.** `cloud-deploy.sh remote <project-dir>` first (read-only).
   - `REMOTE=github` → the project already lives on GitHub; use the GitHub path
     (steps 3-6) unless the user asked for an upload.
   - `REMOTE=none` / `not_github` (exit 3) → **always STOP and ask the user**
     (multiple-choice), offering both: **upload directly** (no git or GitHub needed; run it
     again after each change) or **connect a GitHub repo** (push to redeploy; a new repo is
     created for them, private or public). List upload first as the recommended option.
   - Upload chosen → **skip steps 3-5**: run
     `cloud-deploy.sh upload <project-dir> <name> [-n <namespace>]`, then report as in
     step 6. It uploads what `.gitignore`/`.qooqignore` let through and never a `.env*`
     file (secrets are referenced from qooq.yaml as `${secret.NAME}`); the CLI prints
     `packed N files, <size>` first, so tell the user if that is far larger than the
     project should be and add a `.qooqignore`. An exit 3 means the name belongs to a
     git-built service: ask for another name. Identical content is a no-op; add
     `--force` to rebuild anyway.
3. **Commit** (GitHub path). `cloud-deploy.sh commit <project-dir> <name>` — git init if needed, then the
   same large-file guard as local GitOps (it stops and names the files; relay that, do
   not bypass it with `QOOQ_ALLOW_LARGE_FILES` unless the user says those files belong).
   The component name follows the same DNS-label rule as step 1.
4. **GitHub remote.** Skip when step 2 printed `REMOTE=github`. Otherwise the user chose
   GitHub in step 2; ask **private or public?** (default to private; say that public means
   anyone can read the code). Only after the answer:
   `cloud-deploy.sh create-repo <project-dir> <repo-name> private|public`. If it prints
     `GH=not_installed` or `GH=not_authenticated`, do not improvise: relay its `NEXT=` lines
     (the user creates the empty repo or runs `gh auth login` themselves), then re-run `remote`.
5. **Ship.** `cloud-deploy.sh ship <project-dir> <name> [-n <namespace>]` (namespace
   defaults to `default`). It refuses uncommitted changes, pushes the branch, then runs
   `qooq project create <name> --repo <https URL> --branch <branch> -n <ns> --wait -o json`
   for a new service, or `qooq project deploy <name> -n <ns> --wait -o json` when
   `project get` finds it. `--wait` follows the **new** build, so the result is this
   deploy's, not the previous one's. If a service of that name exists but builds a
   **different repo**, the script stops (exit 3): ask the user whether to overwrite or
   rename; do not pick for them.
6. **Report.** The script prints `ENDPOINT=<url>` and `PROBE=<url> -> HTTP <code>` (it
   really calls the URL). Report the URL and the code you saw. `ENDPOINT=none` means
   deployed but **no public URL**: the app publishes no port; the usual cause is a missing
   `run.ports: ["auto:<port>"]` — fix the manifest, then commit and ship again (or upload again). A non-2xx probe
   right after a green deploy is worth one retry in ~10 s before calling it broken.

**Updating later**: on the GitHub path a `git push` to the deployed branch redeploys by itself; on the upload path run `upload` again.
`qooq project deploy <name> -n <ns> --wait` forces one; `qooq project logs <name> -n <ns>`
prints what the running app writes (`--tail N`, `-f` to follow), and `--build` prints the
last build log instead.

**Where to stop and ask**: (a) running `qooq auth login`, (b) upload or GitHub, and a new GitHub repo's
visibility, (c) a name collision with a different repo, (d) `needs_input` values, (e) deploying
to qooq Cloud when the request sounded local. Everything else runs unattended.

| Symptom | Cause | What to do |
|---|---|---|
| `detect` → `not_signed_in`; or a command says "not signed in" / "token is no longer accepted" | no login, or the token was revoked | the user runs `qooq auth login` (interactive browser flow) and you re-run; never ask them to paste a token into the chat |
| `REMOTE=none` | project has no GitHub remote | step 2: ask upload or GitHub; then `upload`, or `create-repo` (or give the user the two commands) |
| `ship` says push failed | git credentials or no permission on the remote | relay git's message; the user fixes access, then re-run |
| build failed with `could not read Username`, `authentication required` or `repository not found` | the repo is private and qooq Cloud cannot read it | say so plainly: grant qooq Cloud access to this repo (the script prints where), then `ship` again. Or make the repo public |
| `BUILD: failed` for another reason | Dockerfile / build error | the CLI already printed the log tail; fetch more with `qooq project logs <name> -n <ns> --build`, fix the cause, commit, `ship` again |
| exit 2, `BUILD: needs_input` | `inputs:` declared in `qooq.yaml` with no value yet | `qooq project input <name> -n <ns>` lists them; ask the user for each value by its description (never invent one), then `qooq project input <name> -n <ns> NAME=value` (secrets: `NAME=@file`, keeps it out of shell history) — that redeploys by itself. |
| deployed but `ENDPOINT=none` | no published port | see step 6 |
| the CLI refuses with a message about the account (not allowed, limit reached) | the account cannot do this right now | relay the message as is; remove a service only if the user asks (`qooq project rm`) |

Do not run `qooq-env.sh`, start qooqd/watcher, or add a `qooq` git remote on either path:
nothing runs locally; on the GitHub path the user's `origin` is the only remote involved.

