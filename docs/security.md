---
layout: default
title: Security
---

# Security

[Home](./index) | [How It Works](./how-it-works) | [Azure Setup](./azure)

---

Claude Code transcripts can contain source code, command output and anything a tool printed, secrets included. Stradl is built so that your storage provider never sees any of it in the clear.

## Encryption

- Every file is gzip-compressed and then encrypted on your Mac with [age](https://age-encryption.org) (X25519 + ChaCha20-Poly1305) before upload. Storage holds only ciphertext.
- The key is derived from your passphrase with Argon2id, so every Mac that knows the passphrase derives the same key, and no key file ever has to be copied between Macs. The salt is fixed by design, which is what makes the same passphrase produce the same key everywhere; choose a long passphrase.
- The passphrase is never stored or uploaded. The derived identity sits in `~/.stradl/age-key.txt` (mode `0600`).
- Lose the passphrase and the synced copies can't be decrypted. Your local sessions are unaffected.

## What storage can see

Object names are visible to anyone who can list the container: they are the file paths under `~/.claude` with your home folder replaced by `${HOME}`, so they reveal project folder names and session IDs, but not your username or any content. Sizes and modification times are visible too. Everything inside the objects is encrypted, including the git remote hints attached to sidebar entries.

## The Azure token

- The installer stores a **container SAS token**: it can read, write, list and delete blobs in one container and nothing else in your Azure account.
- It's bound to a **stored access policy** named `stradl`. Deleting the policy in the Azure portal revokes the token on every Mac at once; extending the policy's expiry extends the token without touching any Mac (`scripts/azure-setup.sh --rotate`).
- Anyone holding the token can delete or overwrite your synced data, but not read it. Treat it like a password anyway: it lives only in your password manager and in `~/.stradl/config.yaml` (mode `0600`).
- Stradl redacts SAS signatures from every error message it prints or logs.
- Multi-factor authentication protects your Azure portal sign-in, not the token. That's why the token is scoped to one container and revocable.

## Undo

The setup script turns on blob versioning and 30-day soft delete for blobs and containers. Every overwritten or deleted object can be restored from the Azure portal for at least 30 days. A lifecycle rule deletes previous versions once their content is 30 days old (soft delete keeps each one for 30 more days), so the history doesn't grow without bound; see [Azure Setup](./azure#what-it-costs).

## Installing and updating

- Release archives are built by GitHub Actions from tagged commits and published with a `checksums.txt`. The installer and `stradl update` download over HTTPS and refuse any archive whose SHA-256 doesn't match.
- The checksums come from the same release as the archives, so they protect against corrupted or tampered downloads in transit, not against a compromised GitHub account. Protect the account that owns the repository with two-factor authentication.
- The installer needs no administrator rights and writes only to `~/.local/bin`, `~/.stradl`, `~/Library/LaunchAgents/com.breakpointdata.stradl.plist` and one `PATH` line in your shell profile. If bash is your login shell and you have no `~/.bash_profile`, it creates one that first sources the `~/.profile` (or `~/.bash_login`) bash was reading.
- Binaries are ad-hoc signed, not notarized. The curl installer doesn't attach the quarantine flag, so Gatekeeper doesn't block them.

## One person per storage container

Every Mac that shares a container and passphrase sees every session synced into it. Use one container per person, and don't install Stradl in a macOS login that holds work you mustn't mix in, such as an employer's code.
