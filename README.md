# stradl

**Musical chairs for your Claude sessions.**

Stradl keeps every Claude Code session on every one of your Macs, along with everything the sessions worked on. Start a conversation on one Mac, sit down at another, and it's already there: in the Claude desktop app's sidebar and in `claude --resume`, with its project folder, git history and files.

- **Every session, everywhere.** Sessions from every Mac end up on all of them. Nothing is overwritten.
- **Everything a session touched.** Project folders sync too, git repositories included, plus `~/.claude`: settings, skills, agents, commands, plugins. Logins stay on each Mac.
- **Built for the Claude desktop app.** Sessions show up in the Code tab's sidebar on every Mac, with the same groups and pins.
- **Your storage, your keys.** Everything is compressed and encrypted on your Mac with [age](https://age-encryption.org) before it leaves. Your cloud storage (Azure Blob, or R2, S3, GCS, WebDAV) only sees ciphertext.
- **Set and forget.** A small menu bar app syncs in the background, plus Stradl.app when you want to look.

This repository holds Stradl's releases.

## Install

### 1. Create your storage (once)

In [Azure Cloud Shell](https://shell.azure.com) (Bash):

```bash
curl -fsSL https://github.com/MattCampana/stradl/releases/latest/download/azure-setup.sh | bash -s -- \
  --resource-group rg-stradl --location eastus --account <yourname>stradl
```

It creates a private container with versioning and prints a SAS token scoped to that container. Keep the account name, container name and token in your password manager.

### 2. Install on each Mac

```bash
/bin/bash -c "$(curl -fsSL https://github.com/MattCampana/stradl/releases/latest/download/install.sh)"
```

No admin rights, Homebrew or Go needed. It verifies the download, installs `stradl` into `~/.local/bin`, then asks for your storage details and an encryption passphrase. Use the same storage and the same passphrase on every Mac; the passphrase never leaves your Mac and can't be recovered.

## Update

```bash
stradl update
```

Stradl.app and the menu bar app update with it.

## Requirements

macOS 13 or later (Apple Silicon or Intel). Linux builds sync from the command line.

## License

MIT. See [LICENSE](LICENSE).
