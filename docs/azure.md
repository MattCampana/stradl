---
layout: default
title: Azure Setup
---

# Azure Setup

[Home](./index) | [Architecture](./architecture) | [How It Works](./how-it-works) | [Security](./security)

---

Stradl keeps your Claude Code sessions in a storage container that you own. One script, run once in Azure Cloud Shell, creates the container, locks it down, and prints the token each Mac needs. The whole thing takes about two minutes.

## Run the setup script

1. Open [Azure Cloud Shell](https://shell.azure.com) and choose **Bash**. It comes signed in, with the Azure CLI installed.
2. Pick a storage account name: 3 to 24 lowercase letters and digits, unique across all of Azure.
3. Run the script:

```bash
curl -fsSL https://raw.githubusercontent.com/<owner>/stradl/main/scripts/azure-setup.sh | bash -s -- \
  --resource-group rg-stradl --location eastus --account bpstradl
```

The script shows a plan and asks before it changes anything. Running it again is safe: it reuses whatever already exists and creates only what is missing.

| Flag | Default | Purpose |
|------|---------|---------|
| `--resource-group` | required | Resource group to create or reuse |
| `--account` | required | Storage account name |
| `--location` | the group's region | Azure region; required only for a new resource group |
| `--container` | `stradl` | Blob container |
| `--years` | `1` | How long the token stays valid |
| `--subscription` | current | Subscription to switch to first |
| `--rotate` | off | Extend the token's expiry and change nothing else |
| `--yes` | off | Skip the confirmation prompt |

At the end, the script prints the storage account, the container, the expiry date and the SAS token. The token appears once, so copy it straight into your password manager.

## Connect each Mac

Run the Stradl installer on each Mac and enter these values when it prompts:

| Prompt | Value |
|--------|-------|
| Storage account | the `--account` name, e.g. `bpstradl` |
| Container | `stradl`, or your `--container` |
| SAS token | paste it from your password manager |

Use the same encryption passphrase on every Mac. Azure can take up to 30 seconds to activate a new access policy, so retry a first sync that fails with a 403 error.

## What gets created

| Resource | Settings |
|----------|----------|
| Resource group | Tagged `app=stradl`, so one command removes everything |
| Storage account | StorageV2, Standard_LRS, Hot tier, HTTPS only, TLS 1.2 minimum, public blob access off, tagged `app=stradl` |
| Blob versioning | On: every overwrite keeps the previous version |
| Soft delete | 30 days for deleted blobs, 30 days for deleted containers |
| Container | Private: no anonymous access |
| Stored access policy `stradl` | Read, add, create, write, delete and list; expires `--years` from now |
| SAS token | Bound to the policy, HTTPS only |

Standard_LRS keeps three copies inside one datacenter. That is plenty here, because every Mac holds a full copy of its sessions too.

Shared key access stays on because Azure signs SAS tokens with the account key. The key itself stays in Azure: the script reads it to sign the token, never prints it, and never writes it to disk. Your Macs only ever see the token.

## What it costs

Expect well under a dollar a month. Storage is nearly free at this size; most of the bill comes from requests, since each Mac checks the container on every sync. The figures below use approximate US list prices for Hot LRS. The [Azure pricing calculator](https://azure.microsoft.com/pricing/calculator/) has exact rates for your region.

| Item | Approximate price | Typical use, three Macs | Per month |
|------|-------------------|-------------------------|-----------|
| Storage | $0.02 per GB | 0.2 to 2 GB, old versions included | $0.01 to $0.04 |
| List and write requests | $0.065 per 10,000 | one list per sync, plus uploads: 20,000 to 60,000 | $0.13 to $0.40 |
| Read requests | $0.005 per 10,000 | about the same count | under $0.03 |
| Data out | first 100 GB free | well under 1 GB | $0 |

A longer sync interval lowers the request charge in proportion.

Versioning adds a slow trickle of storage: Stradl uploads a whole compressed, encrypted transcript each time it changes, and Azure keeps the version it replaced. To cap that growth, add a lifecycle rule that deletes previous versions after 90 days. Lifecycle rules are free, and this command replaces any lifecycle policy already on the account:

```bash
az storage account management-policy create \
  --account-name bpstradl --resource-group rg-stradl \
  --policy '{"rules":[{"enabled":true,"name":"expire-old-versions","type":"Lifecycle","definition":{"actions":{"version":{"delete":{"daysAfterCreationGreaterThan":90}}},"filters":{"blobTypes":["blockBlob"]}}}]}'
```

## Security model

Two independent layers protect your sessions: the token controls access, and encryption controls what anyone can read.

**The token controls access.** It is a service SAS scoped to one container. It can read, add, create, write, delete and list blobs in that container, and nothing more: no other container, no account settings, no keys, and no way to delete the container itself. It works only over HTTPS. It is also a bearer token, which means anyone holding it gets that access, so keep it in your password manager and in Stradl, nowhere else.

**Encryption protects content.** Before anything leaves a Mac, Stradl compresses each file and encrypts it with [age](https://github.com/FiloSottile/age) (X25519 and ChaCha20-Poly1305), using a key derived from your passphrase. Azure stores only ciphertext. Without the passphrase, which never leaves your Macs, neither Microsoft nor a token holder can read a single session. Azure does see blob names, which mirror file paths under `~/.claude` (project folder names and session IDs), along with sizes and timestamps.

**Versioning and soft delete are the undo button.** The token lacks the two permissions that destroy history: deleting previous versions and permanent delete. A blob that gets overwritten or deleted, whether by a sync bug, a slip of the hand or a leaked token, leaves its previous version behind, and soft delete holds deleted blobs for 30 days. To recover a file, open the container in the Azure portal, turn on **Show deleted blobs**, and restore the blob or promote an earlier entry on its **Versions** tab.

## Extend, revoke, replace

The token carries no expiry of its own. It names the stored access policy, and the policy holds the expiry and permissions. That design makes each of these a single command.

**Extend the expiry without touching the token.** Before the date the script printed, run the script again with `--rotate`:

```bash
curl -fsSL https://raw.githubusercontent.com/<owner>/stradl/main/scripts/azure-setup.sh | bash -s -- \
  --resource-group rg-stradl --account bpstradl --rotate
```

This moves the policy's expiry to `--years` from now (one year by default) and prints the token again. Every token bound to the policy picks up the new date right away, so your Macs need no changes. If the date passes first, syncs fail with 403 errors until you rotate, and your data stays intact. A calendar reminder a few weeks ahead keeps that from happening.

**Revoke every token at once.** Delete the policy:

```bash
az storage container policy delete --account-name bpstradl --container-name stradl --name stradl --auth-mode key
```

Every token bound to it stops working within 30 seconds.

**Replace a leaked token.** Revoke as above, then renew the account key that signed the token, then run the setup script again for a fresh one:

```bash
az storage account keys renew --resource-group rg-stradl --account-name bpstradl --key primary
```

The renewal matters. A token is a signature over the policy name, made with the account key, so a new `stradl` policy under the old key would accept the old token again. After the renewal, enter the new token on each Mac.

## Remove everything

When the resource group holds nothing but Stradl's storage, one command deletes it all:

```bash
az group delete --name rg-stradl
```

This removes the storage account along with every version inside it. Soft delete protects blobs and containers, not the account itself, so treat this step as final. Your Macs keep their local sessions.
