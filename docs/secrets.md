# Secrets & Key Management

**Secrets never live in files tracked by git, and never in plaintext on a shared or
synced disk.** They live in a keystore; code reads them at runtime. This applies to API
keys, service tokens, connector URLs, database credentials — anything you would not want
in a public diff.

## The pattern

1. **Real values live in a keystore.** This project uses **1Password**, and the examples
   below are for it — but the pattern is keystore-agnostic. HashiCorp Vault, AWS/GCP
   Secrets Manager, Doppler, Infisical, or your OS credential store all work the same
   way: values in the store, references in the repo, resolved at runtime. Use whichever
   your team standardizes on; keep the *reference* format consistent within a repo.
2. **Access is via a token in your OS credential store, never a file.** With 1Password:
   a personal login (`op signin`, biometric-unlocked) for humans, or a scoped
   *service-account token* for automation/agents.
3. **The repo holds only references** — e.g. `.env.op` files with `op://Vault/Item/field`
   pointers, no values. (Other stores have their own reference syntax; the rule is the
   same — pointers, never secrets.)
4. **Resolved at runtime**, never written to disk:
   `op run --env-file=.env.op -- <your command>` injects the resolved values as env
   vars into the child process only.

## Where the token lives, per platform

The keystore token authorizes reading a vault; it must live in your OS credential store,
not a dotfile.

- **macOS** — Keychain.
  Store: `security add-generic-password -U -a "$USER" -s op-service-account-<name> -w '<token>'`
  Read:  `security find-generic-password -s op-service-account-<name> -w`
- **Linux** — a Secret Service keyring via `secret-tool` (GNOME Keyring / KWallet), or
  `pass`. On a **secure single-user machine**, a `chmod 600` file kept out of any synced
  or backed-up directory is an acceptable local equivalent.
- **Windows** — Windows Credential Manager, as a *generic credential*, or the 1Password
  CLI's own biometric-unlocked storage after `op signin`. Reach the store through the
  Credential Management API — `CredWriteW` to store, `CredReadW` to read — which
  PowerShell calls directly through `Add-Type`: nothing to install, and no secret on a
  command line. It is the store `ark login` writes the sync token to, as `ark:<host>`;
  see [windows.md](windows.md).

  **Not `cmdkey`.** It cannot read a password back at all — `/list` prints target and
  user names, and [its reference](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/cmdkey)
  says "Passwords are not displayed after they're stored" — so nothing can retrieve
  what it stored, which is the entire point of putting a token there. Its
  `/pass:<password>` form also puts the secret in the command line, and so in the
  process table, where any user on the machine can read it.

Humans can skip service-account tokens entirely: `op signin` with your personal 1Password
account grants whatever vaults you're a member of, and `op run` then works the same way.

## Adding a secret

1. Add the value to the right 1Password vault (see *This project* below) as an item/field.
2. Add a reference line to `.env.op`: `MY_VAR=op://Vault/Item/field`.
3. Consume `${MY_VAR}` where needed (e.g. `.mcp.json`, app config).
4. Never paste the value into a tracked file.

## Rotating

Update the item in 1Password — references and tokens are unchanged. If a *token* is
exposed, rotate the service account in 1Password and re-store it in your credential store
(the same `-U` command overwrites in place).

## This project

- **Vaults:** `Elk` (project secrets) + `Agent` (shared cross-project secrets).
- **Reference files:** none yet — this project references no secrets. When it needs one,
  add a `.env.op` with `op://Elk/...` or `op://Agent/...` references and launch via
  `op run --env-file=.env.op -- <command>`.
