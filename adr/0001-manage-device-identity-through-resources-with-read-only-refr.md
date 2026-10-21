# 1. Manage device identity through resources with read-only refresh

Date: 2026-10-21

## Status

Accepted.

## Context and Problem Statement

Device tags and names are API operations absent from the upstream provider.
A rename is asynchronous, and a custom DeviceInformation response does not update native inventory.
Operators need repeatable apply behavior without mutations during planning.

## Considered Options

1. Dedicated resources with bounded reconciliation and explicit ownership
2. HTTP data sources posting changes
3. An external command without provider lifecycle

## Decision Outcome

Use one resource per direct machine-tag association and one per exact enrollment's device name. Rename apply checks the Settings item and independent readback; refresh reads existing observations only. Prefer a newer native inventory observation over an older custom verification result. Destroying a name resource relinquishes management without restoring an invented prior name. Keep secret-bearing profile uploads outside provider state.

## Consequences

### Good

- Plans do not send commands or mutate tags.
- Unrelated tags are preserved, and names require actual device acknowledgement.
- Existing API clients, authentication and provider schema conventions are reused.

### Bad

- Cached inventory cannot detect a name change until Zentral receives a fresh observation.
- Offline devices can time out with a command still queued; an interrupted creation can be tainted and needs operator inspection before replacement.
- Name-based tag mutation is not atomic against concurrent external tag renames.
- Unattended custom commands require server-side APNs notification after commit.

### Rejected because

- HTTP data sources may run during plan or refresh and would store request data in state.
- An external command alone cannot supply resource import, ownership and drift semantics.
